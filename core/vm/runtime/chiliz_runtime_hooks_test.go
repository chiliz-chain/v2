package runtime

// COR-217: deterministic pins for the DeployerProxy hook properties that do not
// need arbitrary code. They share the harness (stub, chain config, observer)
// with FuzzVmRuntimeWithHooks in chiliz_runtime_fuzz_test.go.

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

var (
	// stopCode is the smallest runnable contract: one STOP.
	stopCode = []byte{byte(vm.STOP)}
	// factoryCode performs CREATE with empty init code and stops.
	factoryCode = []byte{byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CREATE), byte(vm.STOP)}
	// hookFactory is an ordinary contract account hosting factoryCode.
	hookFactory = common.HexToAddress("0x000000000000000000000000000000000000fac7")
)

// callTo issues a plain call to a system contract address and stops:
// PUSH1 0 (retSize) PUSH1 0 (retOffset) PUSH1 0 (argsSize) PUSH1 0 (argsOffset)
// PUSH1 0 (value) PUSH20 addr GAS CALL STOP.
func callTo(addr common.Address) []byte {
	code := []byte{byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH20)}
	code = append(code, addr.Bytes()...)
	return append(code, byte(vm.GAS), byte(vm.CALL), byte(vm.STOP))
}

// TestHookSimpleSendIsFree pins that the invocation hook costs the transaction
// nothing (a simple send stays at its 21000 intrinsic gas): the EVM Call
// returns every unit of gas it was given, whether the callee is code-less
// (the hook is not consulted at all) or runs a STOP (the hook fires once, on
// its own 1M gas budget, and charges the frame nothing).
func TestHookSimpleSendIsFree(t *testing.T) {
	for _, tc := range []struct {
		name         string
		callee       common.Address
		wantEntries  int
		wantCounter  uint64
		wantFirstArg common.Address
	}{
		{"eoa", hookEOA, 0, 0, common.Address{}},
		{"stop contract", hookContract, 1, 1, hookContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newHookState(proxyReturnTrue, stopCode)
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{}), base, hookOrigin, 7)
			_, leftOver, err := Call(tc.callee, nil, cfg)
			if err != nil {
				t.Fatal(err)
			}
			obs.checkDepth(t, "Call")
			if leftOver != hookFuzzGas {
				t.Fatalf("execution gas %d, want 0: the invocation hook must be free", hookFuzzGas-leftOver)
			}
			if got := cfg.State.GetBalance(tc.callee).Uint64(); got != 7 {
				t.Fatalf("callee balance %d, want 7", got)
			}
			if len(obs.entries) != tc.wantEntries || stubCounter(cfg.State) != tc.wantCounter {
				t.Fatalf("hook entries %d / stub counter %d, want %d / %d", len(obs.entries), stubCounter(cfg.State), tc.wantEntries, tc.wantCounter)
			}
			if tc.wantCounter > 0 {
				if got := stubSelector(cfg.State); !bytes.Equal(got, systemcontract.EvmHooksAbi.Methods["checkContractActive"].ID) {
					t.Fatalf("stub saw selector %x, want checkContractActive", got)
				}
				if got := stubAddress(cfg.State, stubSlotFirstArg); got != tc.wantFirstArg {
					t.Fatalf("stub saw contract %x, want %x", got, tc.wantFirstArg)
				}
			}
		})
	}
}

// TestHookFailureBlocksCall pins what a rejecting DeployerProxy (revert or out
// of gas) does to the guarded call: the callee never runs, the hook frame's own
// writes are rolled back, and the call answers ErrNotAllowed.
//
// It also pins two consequences of *where* EVM.Call rejects — after the value
// transfer, before the frame's RevertToSnapshot, via an early return — that are
// live consensus behaviour since the RuntimeUpgrade fork and that any change
// would have to fork-gate: the value transfer to the blocked callee persists,
// and the frame hands back every unit of gas it was given.
func TestHookFailureBlocksCall(t *testing.T) {
	for name, behaviour := range map[string]uint8{"revert": proxyRevert, "out of gas": proxyBurnGas} {
		t.Run(name, func(t *testing.T) {
			// Callee code: SSTORE(0, 1), observable iff the callee ran.
			code := []byte{byte(vm.PUSH1), 0x01, byte(vm.PUSH1), 0x00, byte(vm.SSTORE), byte(vm.STOP)}
			base := newHookState(behaviour, code)
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{}), base, hookOrigin, 5)
			_, leftOver, err := Call(hookContract, nil, cfg)
			obs.checkDepth(t, "Call")
			if !errors.Is(err, vm.ErrNotAllowed) {
				t.Fatalf("err=%v, want ErrNotAllowed", err)
			}
			if len(obs.entries) != 1 {
				t.Fatalf("hook entries %d, want 1", len(obs.entries))
			}
			if got := cfg.State.GetState(hookContract, stubSlotCounter); got != (common.Hash{}) {
				t.Fatalf("callee ran (slot0=%x) although the hook rejected it", got)
			}
			if stubCounter(cfg.State) != 0 {
				t.Fatalf("stub counter %d survived the rolled-back hook frame", stubCounter(cfg.State))
			}
			// Historical semantics of the early return (see the doc comment).
			if leftOver != hookFuzzGas {
				t.Fatalf("leftOver=%d, want %d: a hook rejection refunds the frame's gas", leftOver, hookFuzzGas)
			}
			if got := cfg.State.GetBalance(hookContract).Uint64(); got != 5 {
				t.Fatalf("callee balance %d, want 5: the value transfer precedes the hook and is not rolled back", got)
			}
		})
	}
}

// TestHookDispatchWarmsDeployerProxy pins COR-193: dispatching a Call to the
// RuntimeUpgrade hook address 0x...7f01 adds the DeployerProxy to the EIP-2929
// access list for the calling frame — but not on the sunset path and not when
// the hook frame fails (the warmth is journaled and reverted with the
// snapshot). The ordinary-contract rows show the other way the address gets
// warm: a successful invocation hook runs the DeployerProxy's code, whose
// storage accesses add the address to the list along with the slot; under
// sunset the hook does not run and the address stays cold.
func TestHookDispatchWarmsDeployerProxy(t *testing.T) {
	upgraded := common.HexToAddress("0x1234")
	newCode := []byte{byte(vm.PUSH1), 0x01, byte(vm.STOP)}
	upgrade := upgradeToInput(t, upgraded, newCode)

	for _, tc := range []struct {
		name     string
		sunset   bool
		origin   common.Address
		callee   common.Address
		input    []byte
		wantErr  bool
		wantWarm bool
		wantCode []byte // code at upgraded after the call, nil = untouched
	}{
		{"hook dispatch, hooks live", false, systemcontract.RuntimeUpgradeContractAddress, systemcontract.EvmHookRuntimeUpgradeAddress, upgrade, false, true, newCode},
		{"hook dispatch, sunset", true, systemcontract.RuntimeUpgradeContractAddress, systemcontract.EvmHookRuntimeUpgradeAddress, upgrade, false, false, newCode},
		{"hook dispatch, rejected caller", false, hookOrigin, systemcontract.EvmHookRuntimeUpgradeAddress, upgrade, true, false, nil},
		{"hook dispatch, unknown method", false, systemcontract.RuntimeUpgradeContractAddress, systemcontract.EvmHookRuntimeUpgradeAddress, []byte{1, 2, 3, 4}, true, false, nil},
		{"ordinary contract, hooks live", false, hookOrigin, hookContract, nil, false, true, nil},
		{"ordinary contract, sunset", true, hookOrigin, hookContract, nil, false, false, nil},
		{"eoa, hooks live", false, hookOrigin, hookEOA, nil, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newHookState(proxyReturnTrue, stopCode)
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{sunset: tc.sunset}), base, tc.origin, 0)
			if cfg.State.AddressInAccessList(systemcontract.DeployerProxyContractAddress) {
				t.Fatal("DeployerProxy warm before the call")
			}
			_, _, err := Call(tc.callee, tc.input, cfg)
			obs.checkDepth(t, "Call")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if warm := cfg.State.AddressInAccessList(systemcontract.DeployerProxyContractAddress); warm != tc.wantWarm {
				t.Fatalf("DeployerProxy warm=%v, want %v", warm, tc.wantWarm)
			}
			if tc.wantCode != nil && !bytes.Equal(cfg.State.GetCode(upgraded), tc.wantCode) {
				t.Fatalf("upgradeTo did not install the code: %x", cfg.State.GetCode(upgraded))
			}
			if tc.wantCode == nil && len(cfg.State.GetCode(upgraded)) != 0 {
				t.Fatalf("upgradeTo installed code on a failed/unrelated call: %x", cfg.State.GetCode(upgraded))
			}
			if tc.callee == systemcontract.EvmHookRuntimeUpgradeAddress && len(obs.entries) != 0 {
				t.Fatalf("invocation hook fired %d time(s) for the hook address", len(obs.entries))
			}
		})
	}
}

// TestHookDeploymentAttribution pins who the DeployerProxy is told deployed a
// contract, for a CREATE issued by a contract (factory) and for a top-level
// CREATE, over the four HasDeployOrigin x DeployerFactory combinations and both
// hook placements (pre/post collision check, HasDeploymentHookFix).
func TestHookDeploymentAttribution(t *testing.T) {
	register := systemcontract.EvmHooksAbi.Methods["registerDeployedContract"].ID
	for _, deployOrigin := range []bool{false, true} {
		for _, deployerFactory := range []bool{false, true} {
			for _, hookFix := range []bool{false, true} {
				forks := hookForks{deployOrigin: deployOrigin, hookFix: hookFix, deployerFactory: deployerFactory}
				name := fmt.Sprintf("origin=%v/factory=%v/fix=%v", deployOrigin, deployerFactory, hookFix)

				t.Run("nested/"+name, func(t *testing.T) {
					base := newHookState(proxyReturnTrue, stopCode)
					base.SetCode(hookFactory, factoryCode, tracing.CodeChangeUnspecified)
					base.SetNonce(hookFactory, 1, tracing.NonceChangeUnspecified)
					wantCreated := createdAddress(base, hookFactory)
					cfg, obs := newHookConfig(chilizHookChainConfig(forks), base, hookOrigin, 0)
					if _, _, err := Call(hookFactory, nil, cfg); err != nil {
						t.Fatal(err)
					}
					obs.checkDepth(t, "Call")
					obs.checkEntries(t, "Call", forks, hookOrigin)
					// One invocation hook for the factory, one deployment hook for the child.
					if len(obs.entries) != 2 || stubCounter(cfg.State) != 2 {
						t.Fatalf("hook entries %d / stub counter %d, want 2 / 2", len(obs.entries), stubCounter(cfg.State))
					}
					if got := stubSelector(cfg.State); !bytes.Equal(got, register) {
						t.Fatalf("last stub selector %x, want registerDeployedContract", got)
					}
					wantDeployer := forks.expectedDeployer(hookOrigin, hookFactory)
					if got := stubAddress(cfg.State, stubSlotFirstArg); got != wantDeployer {
						t.Fatalf("deployer attributed to %x, want %x", got, wantDeployer)
					}
					if got := stubAddress(cfg.State, stubSlotSecondArg); got != wantCreated {
						t.Fatalf("registered contract %x, want %x", got, wantCreated)
					}
					if cfg.State.GetNonce(wantCreated) != 1 {
						t.Fatalf("child contract %x was not created", wantCreated)
					}
				})

				t.Run("toplevel/"+name, func(t *testing.T) {
					base := newHookState(proxyReturnTrue, stopCode)
					wantCreated := createdAddress(base, hookOrigin)
					cfg, obs := newHookConfig(chilizHookChainConfig(forks), base, hookOrigin, 0)
					_, addr, _, err := Create(stopCode, cfg)
					if err != nil {
						t.Fatal(err)
					}
					obs.checkDepth(t, "Create")
					obs.checkEntries(t, "Create", forks, hookOrigin)
					if addr != wantCreated {
						t.Fatalf("created %x, want %x", addr, wantCreated)
					}
					if len(obs.entries) != 1 || stubCounter(cfg.State) != 1 {
						t.Fatalf("hook entries %d / stub counter %d, want 1 / 1", len(obs.entries), stubCounter(cfg.State))
					}
					// A top-level CREATE has caller == tx.Origin, so both rules agree.
					if got := stubAddress(cfg.State, stubSlotFirstArg); got != hookOrigin {
						t.Fatalf("deployer attributed to %x, want origin %x", got, hookOrigin)
					}
					if got := stubAddress(cfg.State, stubSlotSecondArg); got != wantCreated {
						t.Fatalf("registered contract %x, want %x", got, wantCreated)
					}
				})
			}
		}
	}
}

// TestHookSystemContractsBypass pins that calls to every address
// systemcontract.IsSystemContract accepts — the BAS contracts 0x...7001-7006
// and the three enabled BSC ones — and to precompiles never consult the
// DeployerProxy, whether issued from the origin or from a contract, while the
// hook is still consulted for the contract doing the calling.
func TestHookSystemContractsBypass(t *testing.T) {
	// ecrecover and identity accept empty input; the system contracts run STOP (the stub at 0x...7005).
	targets := append([]common.Address{common.BytesToAddress([]byte{1}), common.BytesToAddress([]byte{4})}, bypassedSystemContracts...)
	for _, target := range targets {
		isStub := target == systemcontract.DeployerProxyContractAddress

		t.Run("direct/"+target.Hex(), func(t *testing.T) {
			base := newHookState(proxyReturnTrue, stopCode)
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{}), base, hookOrigin, 0)
			if _, _, err := Call(target, nil, cfg); err != nil {
				t.Fatal(err)
			}
			obs.checkDepth(t, "Call")
			if len(obs.entries) != 0 {
				t.Fatalf("hook fired %d time(s) for a direct call to %x", len(obs.entries), target)
			}
			// Calling the DeployerProxy itself runs the stub once, as the callee,
			// not as a hook; every other target must leave it untouched.
			var wantCounter uint64
			if isStub {
				wantCounter = 1
			}
			if stubCounter(cfg.State) != wantCounter {
				t.Fatalf("stub counter %d, want %d", stubCounter(cfg.State), wantCounter)
			}
		})

		t.Run("via contract/"+target.Hex(), func(t *testing.T) {
			base := newHookState(proxyReturnTrue, callTo(target))
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{}), base, hookOrigin, 0)
			if _, _, err := Call(hookContract, nil, cfg); err != nil {
				t.Fatal(err)
			}
			obs.checkDepth(t, "Call")
			obs.checkEntries(t, "Call", hookForks{}, hookOrigin)
			if len(obs.entries) != 1 {
				t.Fatalf("hook entries %d, want exactly 1 (the caller's own invocation hook)", len(obs.entries))
			}
			wantCounter := uint64(1)
			if isStub {
				wantCounter = 2 // the inner CALL enters the stub directly
			}
			if stubCounter(cfg.State) != wantCounter {
				t.Fatalf("stub counter %d, want %d", stubCounter(cfg.State), wantCounter)
			}
			if !isStub {
				// The stub's last record must still be the caller's invocation hook.
				if got := stubAddress(cfg.State, stubSlotFirstArg); got != hookContract {
					t.Fatalf("stub last saw %x, want the calling contract %x", got, hookContract)
				}
			}
		})
	}
}

// TestHookDelegationDesignator pins how the invocation hook treats a callee
// whose code is an EIP-7702 delegation designator, one target per branch of
// hookCodeRuns (see its comment for the evm.go mechanics): the hook fires iff
// the delegate has stored code, it always asks about the callee (hookContract)
// rather than the delegate — so the IsSystemContract bypass does not apply to a
// designator pointing at a system contract — and the delegate's code runs in the
// callee's storage context, so the stub's own counter only ever reflects the hook.
func TestHookDelegationDesignator(t *testing.T) {
	for _, tc := range []struct {
		name        string
		delegate    common.Address
		wantEntries int
		wantErr     bool
	}{
		{"stub deployer proxy", systemcontract.DeployerProxyContractAddress, 1, false},
		{"bas system contract", systemcontract.StakingPoolContractAddress, 1, false},
		{"hook address", systemcontract.EvmHookRuntimeUpgradeAddress, 0, false},
		{"precompile", common.BytesToAddress([]byte{1}), 0, false},
		{"eoa", hookEOA, 0, false},
		{"self (designator to designator)", hookContract, 1, true},
	} {
		designator := types.AddressToDelegation(tc.delegate)

		t.Run(tc.name, func(t *testing.T) {
			base := newHookState(proxyReturnTrue, designator)
			if got := hookCodeRuns(base, designator); got != (tc.wantEntries > 0) {
				t.Fatalf("hookCodeRuns = %v, want %v", got, tc.wantEntries > 0)
			}
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{}), base, hookOrigin, 0)
			_, _, err := Call(hookContract, nil, cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			obs.checkDepth(t, "Call")
			obs.checkEntries(t, "Call", hookForks{}, hookOrigin)
			if len(obs.entries) != tc.wantEntries {
				t.Fatalf("hook entries %d, want %d", len(obs.entries), tc.wantEntries)
			}
			// Only the hook may enter the stub at its own address, and its writes
			// are rolled back with the callee frame when the callee faults.
			wantCounter := uint64(tc.wantEntries)
			if tc.wantErr {
				wantCounter = 0
			}
			if stubCounter(cfg.State) != wantCounter {
				t.Fatalf("stub counter %d, want %d", stubCounter(cfg.State), wantCounter)
			}
			if wantCounter > 0 {
				// The hook asks about the callee, never the delegate.
				if got := stubAddress(cfg.State, stubSlotFirstArg); got != hookContract {
					t.Fatalf("checkContractActive(%x), want the callee %x", got, hookContract)
				}
			}
			// The stub prologue writes slot0 of whatever account it runs in: when
			// it is the delegate it runs as hookContract, proving the code ran.
			delegateRan := cfg.State.GetState(hookContract, stubSlotCounter).Big().Uint64() == 1
			if want := tc.delegate == systemcontract.DeployerProxyContractAddress; delegateRan != want {
				t.Fatalf("delegate stub ran in the callee's context = %v, want %v", delegateRan, want)
			}
		})

		t.Run(tc.name+"/sunset", func(t *testing.T) {
			base := newHookState(proxyReturnTrue, designator)
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{sunset: true}), base, hookOrigin, 0)
			_, _, _ = Call(hookContract, nil, cfg)
			obs.checkDepth(t, "Call")
			if len(obs.entries) != 0 || stubCounter(cfg.State) != 0 {
				t.Fatalf("hook entries %d / stub counter %d under DeployerProxySunset, want 0 / 0", len(obs.entries), stubCounter(cfg.State))
			}
		})
	}
}
