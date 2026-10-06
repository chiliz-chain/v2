package runtime

// COR-217: fuzz the EVM runtime with the Chiliz DeployerProxy hooks active.
//
// Upstream's FuzzVmRuntime runs arbitrary bytecode under a non-Parlia chain
// config, so deployerProxyHooksActive() is false and none of the Chiliz code in
// core/vm/chiliz.go / core/vm/evm.go (precompileOrHook, the COR-193 warm-up
// line, applyChilizInvocationEvmHook, the two applyChilizDeploymentEvmHook
// insertion points in create(), hookStateDBAdapter) is ever reached by a
// fuzzer. This file builds the same harness with a Parlia config where the
// RuntimeUpgrade fork is active and a hand-assembled stub DeployerProxy
// installed at 0x...7005, then pins the hook contract's observable behaviour.
//
// The fuzzer guarantees the properties that only hold "for arbitrary code":
// no panic, sunset => the hook never fires, every frame exits at the depth it
// entered (depth restored across hook failures), a failing DeployerProxy blocks
// the guarded call/create, and the calldata the stub receives is exactly what
// the frame stack says it should be (invocation: the callee; deployment:
// tx.Origin iff HasDeployOrigin && !DeployerFactory, else the direct creator,
// plus the created address). The deterministic properties (simple-send gas,
// COR-193 warmth, attribution over the four fork combinations, system-contract
// bypass) are pinned with fixed bytecode in chiliz_runtime_hooks_test.go.

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// Stub DeployerProxy behaviours, selected at pre-state setup.
const (
	proxyReturnTrue uint8 = iota // returns abi(true)
	proxyRevert                  // REVERT with empty data
	proxyBurnGas                 // infinite loop, consumes every unit of gas it is given
	proxyGarbage                 // returns 32 bytes of 0xff
	proxyBehaviourCount
)

// Storage slots written by the stub on every entry.
var (
	stubSlotCounter   = common.BigToHash(big.NewInt(0)) // number of times the stub was entered
	stubSlotFirstArg  = common.BigToHash(big.NewInt(1)) // calldata word 0 after the selector
	stubSlotSelector  = common.BigToHash(big.NewInt(2)) // 4-byte selector, right-aligned
	stubSlotSecondArg = common.BigToHash(big.NewInt(3)) // calldata word 1 after the selector
)

// Fixed harness addresses. None of them is a system contract, a precompile or
// an EVM hook address, and none collides with the Execute() target
// (BytesToAddress("contract")).
var (
	hookOrigin   = common.HexToAddress("0x00000000000000000000000000000000000f00d0") // EOA sender
	hookCoinbase = common.HexToAddress("0x000000000000000000000000000000000c01bba5") // block coinbase, hook caller
	hookEOA      = common.HexToAddress("0x000000000000000000000000000000000000e0a0") // code-less callee
	hookContract = common.BytesToAddress([]byte("contract"))                         // Execute() target
	hookCreator  = common.HexToAddress("0x00000000000000000000000000000000000cea7e") // CREATEs its calldata
)

// hookCreatorCode copies the frame's calldata into memory and CREATEs it:
//
//	CALLDATASIZE PUSH1 0x00 PUSH1 0x00 CALLDATACOPY  ; mem[0:size] = calldata
//	CALLDATASIZE PUSH1 0x00 PUSH1 0x00 CREATE STOP   ; create(value=0, offset=0, size)
//
// Calling it with the fuzzed code as input drives one nested deployment — and
// therefore one deployment hook attributed to a *contract* creator — on every
// exec, instead of only on the small fraction of fuzzed programs that reach a
// CREATE on their own.
var hookCreatorCode = []byte{
	byte(vm.CALLDATASIZE), byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CALLDATACOPY),
	byte(vm.CALLDATASIZE), byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CREATE),
	byte(vm.STOP),
}

// hookOriginBalance funds the origin so that value transfers succeed for half
// the uint64 range and fail (ErrInsufficientBalance) for the other half.
var hookOriginBalance = new(uint256.Int).Lsh(uint256.NewInt(1), 63)

// hookFuzzGas caps each run; the burn-gas stub spends all of it on the
// deployment path (12 gas per loop iteration), so keep it moderate.
const hookFuzzGas = 3_000_000

// stubDeployerProxyCode hand-assembles the DeployerProxy stand-in.
//
// Prologue (30 bytes), common to every behaviour:
//
//	PUSH1 0x00 SLOAD PUSH1 0x01 ADD PUSH1 0x00 SSTORE   ; slot0 = slot0 + 1            (call counter)
//	PUSH1 0x04 CALLDATALOAD PUSH1 0x01 SSTORE           ; slot1 = calldata[4:36]       (first ABI word)
//	PUSH1 0x00 CALLDATALOAD PUSH1 0xe0 SHR PUSH1 0x02 SSTORE ; slot2 = calldata[0:4]   (selector)
//	PUSH1 0x24 CALLDATALOAD PUSH1 0x03 SSTORE           ; slot3 = calldata[36:68]      (second ABI word)
//
// Tail, by behaviour:
//
//	returnTrue: PUSH1 0x01 PUSH1 0x00 MSTORE PUSH1 0x20 PUSH1 0x00 RETURN   ; return abi(true)
//	revert:     PUSH1 0x00 PUSH1 0x00 REVERT
//	burnGas:    JUMPDEST PUSH1 <pc of JUMPDEST> JUMP                        ; loop forever
//	garbage:    PUSH1 0x00 NOT PUSH1 0x00 MSTORE PUSH1 0x20 PUSH1 0x00 RETURN ; return 0xff..ff
//
// For revert and burnGas the SSTOREs are rolled back by the failed frame, so
// their entries are only visible to the tracer (hookObserver), not in state.
func stubDeployerProxyCode(behaviour uint8) []byte {
	code := []byte{
		byte(vm.PUSH1), 0x00, byte(vm.SLOAD), byte(vm.PUSH1), 0x01, byte(vm.ADD), byte(vm.PUSH1), 0x00, byte(vm.SSTORE),
		byte(vm.PUSH1), 0x04, byte(vm.CALLDATALOAD), byte(vm.PUSH1), 0x01, byte(vm.SSTORE),
		byte(vm.PUSH1), 0x00, byte(vm.CALLDATALOAD), byte(vm.PUSH1), 0xe0, byte(vm.SHR), byte(vm.PUSH1), 0x02, byte(vm.SSTORE),
		byte(vm.PUSH1), 0x24, byte(vm.CALLDATALOAD), byte(vm.PUSH1), 0x03, byte(vm.SSTORE),
	}
	switch behaviour {
	case proxyReturnTrue:
		code = append(code, byte(vm.PUSH1), 0x01, byte(vm.PUSH1), 0x00, byte(vm.MSTORE), byte(vm.PUSH1), 0x20, byte(vm.PUSH1), 0x00, byte(vm.RETURN))
	case proxyRevert:
		code = append(code, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.REVERT))
	case proxyBurnGas:
		loop := byte(len(code))
		code = append(code, byte(vm.JUMPDEST), byte(vm.PUSH1), loop, byte(vm.JUMP))
	case proxyGarbage:
		code = append(code, byte(vm.PUSH1), 0x00, byte(vm.NOT), byte(vm.PUSH1), 0x00, byte(vm.MSTORE), byte(vm.PUSH1), 0x20, byte(vm.PUSH1), 0x00, byte(vm.RETURN))
	default:
		panic(fmt.Sprintf("unknown stub behaviour %d", behaviour))
	}
	return code
}

// proxyFails reports whether the stub behaviour makes the hook call fail, i.e.
// whether the EVM must answer the guarded call/create with vm.ErrNotAllowed.
func proxyFails(behaviour uint8) bool {
	return behaviour == proxyRevert || behaviour == proxyBurnGas
}

// hookForks selects the Chiliz fork flags of a harness run.
type hookForks struct {
	sunset          bool // DeployerProxySunsetTime = 0: hooks off
	deployOrigin    bool // DeployOriginBlock = 0
	hookFix         bool // DeploymentHookFixBlock = 0: deployment hook after the collision check
	deployerFactory bool // DeployerFactoryBlock = 0
}

// chilizHookChainConfig is params.ParliaTestChainConfig (Parlia, every BSC and
// Ethereum fork up to Cancun at 0) plus the BSC forks that Chiliz mainnet has
// scheduled since (Haber, Bohr, Pascal, Prague) and the Chiliz forks:
// RuntimeUpgrade always at 0 so the hooks are live, the others per forks.
func chilizHookChainConfig(forks hookForks) *params.ChainConfig {
	zero := func() *uint64 { z := uint64(0); return &z }
	cfg := *params.ParliaTestChainConfig
	cfg.ChainID = big.NewInt(88888)
	cfg.HaberTime = zero()
	cfg.BohrTime = zero()
	cfg.PascalTime = zero()
	cfg.PragueTime = zero()
	cfg.BlobScheduleConfig = &params.BlobScheduleConfig{
		Cancun: params.DefaultCancunBlobConfig,
		Prague: params.DefaultPragueBlobConfigBSC,
	}
	cfg.RuntimeUpgradeBlock = big.NewInt(0)
	if forks.deployOrigin {
		cfg.DeployOriginBlock = big.NewInt(0)
	}
	if forks.hookFix {
		cfg.DeploymentHookFixBlock = big.NewInt(0)
	}
	if forks.deployerFactory {
		cfg.DeployerFactoryBlock = big.NewInt(0)
	}
	if forks.sunset {
		cfg.DeployerProxySunsetTime = zero()
	}
	return &cfg
}

// expectedDeployer mirrors the attribution rule in applyChilizDeploymentEvmHook.
func (forks hookForks) expectedDeployer(origin, directCaller common.Address) common.Address {
	if forks.deployOrigin && !forks.deployerFactory {
		return origin
	}
	return directCaller
}

// newHookState builds the pre-state shared by every path of one run: the stub
// at 0x...7005, STOP code at every other system contract (so a call to them
// has code to run and would trip the invocation hook if it were not bypassed),
// a funded origin, a funded RuntimeUpgrade contract account (the only caller
// the 0x...7f01 hook accepts), a code-less EOA, the fuzzed code at the
// Execute() target and the CREATE driver at hookCreator.
func newHookState(behaviour uint8, code []byte) *state.StateDB {
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	statedb.SetCode(systemcontract.DeployerProxyContractAddress, stubDeployerProxyCode(behaviour), tracing.CodeChangeUnspecified)
	for _, sys := range bypassedSystemContracts {
		if sys == systemcontract.DeployerProxyContractAddress {
			continue
		}
		statedb.SetCode(sys, []byte{byte(vm.STOP)}, tracing.CodeChangeUnspecified)
	}
	statedb.SetBalance(systemcontract.RuntimeUpgradeContractAddress, hookOriginBalance, tracing.BalanceChangeUnspecified)
	statedb.CreateAccount(hookOrigin)
	statedb.SetBalance(hookOrigin, hookOriginBalance, tracing.BalanceChangeUnspecified)
	statedb.CreateAccount(hookEOA)
	statedb.SetNonce(hookEOA, 1, tracing.NonceChangeUnspecified)
	statedb.SetCode(hookContract, code, tracing.CodeChangeUnspecified)
	statedb.SetCode(hookCreator, hookCreatorCode, tracing.CodeChangeUnspecified)
	statedb.SetNonce(hookCreator, 1, tracing.NonceChangeUnspecified)
	return statedb
}

// hookFrame is one open EVM frame as seen by the tracer.
type hookFrame struct {
	depth    int
	from, to common.Address
}

// hookEntry is one entry into the stub made by the EVM hooks (from == coinbase).
type hookEntry struct {
	input  []byte
	parent hookFrame // the frame the hook fired inside of
}

// hookObserver is a tracing.Hooks implementation that records every EVM frame
// so the test can (a) count hook invocations even when the stub reverts and its
// SSTOREs are rolled back, (b) check that every frame exits at the depth it
// entered — the observable form of "evm.depth is restored" — and (c) check the
// calldata each hook entry carries against the frame it fired in.
type hookObserver struct {
	stack     []hookFrame
	entries   []hookEntry
	rootDepth []int  // depth reported for each frame opened at stack height 0
	mismatch  string // first enter/exit depth mismatch, if any
}

func (o *hookObserver) hooks() *tracing.Hooks {
	return &tracing.Hooks{OnEnter: o.onEnter, OnExit: o.onExit}
}

func (o *hookObserver) onEnter(depth int, _ byte, from, to common.Address, input []byte, _ uint64, _ *big.Int) {
	if len(o.stack) == 0 {
		o.rootDepth = append(o.rootDepth, depth)
	}
	if from == hookCoinbase && to == systemcontract.DeployerProxyContractAddress {
		entry := hookEntry{input: bytes.Clone(input)}
		if len(o.stack) > 0 {
			entry.parent = o.stack[len(o.stack)-1]
		}
		o.entries = append(o.entries, entry)
	}
	o.stack = append(o.stack, hookFrame{depth: depth, from: from, to: to})
}

func (o *hookObserver) onExit(depth int, _ []byte, _ uint64, _ error, _ bool) {
	if len(o.stack) == 0 {
		if o.mismatch == "" {
			o.mismatch = fmt.Sprintf("OnExit(depth=%d) without an open frame", depth)
		}
		return
	}
	top := o.stack[len(o.stack)-1]
	o.stack = o.stack[:len(o.stack)-1]
	if top.depth != depth && o.mismatch == "" {
		o.mismatch = fmt.Sprintf("frame %x->%x entered at depth %d but exited at depth %d", top.from, top.to, top.depth, depth)
	}
}

// checkDepth fails the test if any frame leaked or restored the wrong depth.
func (o *hookObserver) checkDepth(t *testing.T, path string) {
	t.Helper()
	if o.mismatch != "" {
		t.Fatalf("%s: %s", path, o.mismatch)
	}
	if len(o.stack) != 0 {
		t.Fatalf("%s: %d frame(s) still open after execution", path, len(o.stack))
	}
	for i, d := range o.rootDepth {
		if d != 0 {
			t.Fatalf("%s: root frame %d entered at depth %d, want 0", path, i, d)
		}
	}
}

// checkEntries verifies every hook entry against the frame it fired in:
// checkContractActive(callee) for the invocation hook, and
// registerDeployedContract(deployer, created) for the deployment hook, where
// deployer follows the HasDeployOrigin/DeployerFactory rule.
func (o *hookObserver) checkEntries(t *testing.T, path string, forks hookForks, origin common.Address) {
	t.Helper()
	check := systemcontract.EvmHooksAbi.Methods["checkContractActive"]
	register := systemcontract.EvmHooksAbi.Methods["registerDeployedContract"]
	for i, e := range o.entries {
		if len(e.input) < 4 {
			t.Fatalf("%s: hook entry %d carries %d bytes of calldata", path, i, len(e.input))
		}
		var want []byte
		var err error
		switch {
		case bytes.Equal(e.input[:4], check.ID):
			want, err = systemcontract.EvmHooksAbi.Pack("checkContractActive", e.parent.to)
		case bytes.Equal(e.input[:4], register.ID):
			want, err = systemcontract.EvmHooksAbi.Pack("registerDeployedContract", forks.expectedDeployer(origin, e.parent.from), e.parent.to)
		default:
			t.Fatalf("%s: hook entry %d has unknown selector %x", path, i, e.input[:4])
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(e.input, want) {
			t.Fatalf("%s: hook entry %d (parent %x->%x, forks %+v): calldata %x, want %x", path, i, e.parent.from, e.parent.to, forks, e.input, want)
		}
	}
}

// hookCalleeKind selects the target of the Call path.
const (
	calleeContract         uint8 = iota // the fuzzed code at hookContract
	calleeEOA                           // code-less account
	calleePrecompile                    // one of the active precompiles
	calleeHookAddr                      // 0x...7f01 from the ordinary origin (hook rejects the caller)
	calleeHookAddrUpgrader              // 0x...7f01 from 0x...7004 (hook may succeed)
	calleeSystem                        // one of the BAS system contracts 0x...7001-7006
	calleeKinds
)

// bypassedSystemContracts is every address common/systemcontract.IsSystemContract
// answers true for — the BAS contracts 0x...7001-7006 plus the three BSC ones
// Chiliz kept enabled. All of them get STOP code in newHookState (the stub at
// 0x...7005), so a call to any of them would trip the invocation hook if the
// bypass in applyChilizInvocationEvmHook were lost.
var bypassedSystemContracts = []common.Address{
	systemcontract.StakingPoolContractAddress,
	systemcontract.GovernanceContractAddress,
	systemcontract.ChainConfigContractAddress,
	systemcontract.RuntimeUpgradeContractAddress,
	systemcontract.DeployerProxyContractAddress,
	systemcontract.TokenomicsContractAddress,
	common.HexToAddress(systemcontract.ValidatorContract),
	common.HexToAddress(systemcontract.SlashContract),
	common.HexToAddress(systemcontract.SystemRewardContract),
}

// newHookConfig returns a runtime Config for one execution path over a copy of
// base, wired to a fresh observer.
func newHookConfig(chainCfg *params.ChainConfig, base *state.StateDB, origin common.Address, value uint64) (*Config, *hookObserver) {
	obs := &hookObserver{}
	cfg := &Config{
		ChainConfig: chainCfg,
		Origin:      origin,
		Coinbase:    hookCoinbase,
		Time:        1,
		GasLimit:    hookFuzzGas,
		Value:       new(big.Int).SetUint64(value),
		State:       base.Copy(),
		EVMConfig:   vm.Config{Tracer: obs.hooks()},
	}
	return cfg, obs
}

// callWith assembles `PUSH1 0 x5, PUSH20 addr, GAS, <op>, STOP` — a call to
// addr with no value, arguments or return buffer. op must be CALL (which takes
// the extra value argument) or, with one PUSH1 0 dropped by the caller,
// DELEGATECALL/STATICCALL.
func callWith(op vm.OpCode, addr common.Address) []byte {
	code := []byte{byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00}
	if op == vm.CALL {
		code = append(code, byte(vm.PUSH1), 0x00)
	}
	code = append(code, byte(vm.PUSH20))
	code = append(code, addr.Bytes()...)
	return append(code, byte(vm.GAS), byte(op), byte(vm.STOP))
}

func FuzzVmRuntimeWithHooks(f *testing.F) {
	factory := []byte{byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CREATE), byte(vm.STOP)}
	callSystem := []byte{
		byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00,
		byte(vm.PUSH2), 0x70, 0x01, byte(vm.GAS), byte(vm.CALL), byte(vm.STOP),
	}
	// A factory whose child is itself a factory: CODECOPY the tail of this
	// program (the plain factory, appended after STOP) into memory and CREATE
	// it, so the deployment hook fires twice with two different creators.
	nestedFactoryPrologue := func(tailOffset byte) []byte {
		return []byte{
			byte(vm.PUSH1), byte(len(factory)), byte(vm.PUSH1), tailOffset, byte(vm.PUSH1), 0x00, byte(vm.CODECOPY),
			byte(vm.PUSH1), byte(len(factory)), byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00,
			byte(vm.CREATE), byte(vm.STOP),
		}
	}
	nestedFactory := append(nestedFactoryPrologue(byte(len(nestedFactoryPrologue(0)))), factory...)
	create2Factory := []byte{
		byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00,
		byte(vm.CREATE2), byte(vm.STOP),
	}
	callSelf := callWith(vm.CALL, hookContract)
	delegateToProxy := callWith(vm.DELEGATECALL, systemcontract.DeployerProxyContractAddress)
	callHookAddress := callWith(vm.CALL, systemcontract.EvmHookRuntimeUpgradeAddress)
	upgradeInput := upgradeToInput(f, common.HexToAddress("0x1234"), []byte{byte(vm.STOP)})
	f.Add([]byte{}, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeContract)
	f.Add([]byte{byte(vm.STOP)}, []byte{}, proxyReturnTrue, false, true, true, false, uint64(1), calleeEOA)
	f.Add(factory, []byte{}, proxyReturnTrue, false, true, false, false, uint64(0), calleeContract)
	f.Add(factory, []byte{}, proxyGarbage, false, true, true, true, uint64(0), calleeContract)
	f.Add(factory, []byte{}, proxyRevert, false, false, true, false, uint64(0), calleeContract)
	f.Add(factory, []byte{}, proxyBurnGas, true, false, false, false, uint64(0), calleeContract)
	f.Add(callSystem, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeSystem)
	// Seeds that open frames on their own: the fuzzer's byte-slice mutations
	// shrink aggressively, so without these the corpus is dominated by
	// one-to-five-byte programs that halt within a couple of opcodes.
	f.Add(nestedFactory, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeContract)
	f.Add(nestedFactory, []byte{}, proxyReturnTrue, false, true, false, true, uint64(0), calleeContract)
	f.Add(create2Factory, []byte{}, proxyReturnTrue, false, true, true, false, uint64(0), calleeContract)
	f.Add(callSelf, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeContract)
	f.Add(delegateToProxy, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeContract)
	f.Add(callHookAddress, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeContract)
	f.Add([]byte{byte(vm.STOP)}, upgradeInput, proxyReturnTrue, false, false, false, false, uint64(0), calleeHookAddrUpgrader)
	f.Add([]byte{byte(vm.STOP)}, upgradeInput, proxyReturnTrue, true, false, false, false, uint64(0), calleeHookAddrUpgrader)
	f.Add([]byte{byte(vm.STOP)}, []byte{}, proxyRevert, false, false, false, false, uint64(0), calleePrecompile)
	// EIP-7702 designators at the fuzzed address, one per branch of hookCodeRuns:
	// delegate with code (the stub, a BAS system contract, the designator itself)
	// and delegate without (the hook address, an EOA).
	for _, delegate := range delegationTargets {
		f.Add(types.AddressToDelegation(delegate), []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeContract)
	}
	f.Add(types.AddressToDelegation(systemcontract.DeployerProxyContractAddress), []byte{}, proxyRevert, false, false, false, false, uint64(0), calleeContract)
	f.Add(types.AddressToDelegation(systemcontract.DeployerProxyContractAddress), []byte{}, proxyReturnTrue, true, false, false, false, uint64(0), calleeContract)

	f.Fuzz(func(t *testing.T, code, input []byte, proxyBehaviour uint8, sunset, deployOrigin, hookFix, deployerFactory bool, value uint64, callee uint8) {
		behaviour := proxyBehaviour % proxyBehaviourCount
		forks := hookForks{sunset: sunset, deployOrigin: deployOrigin, hookFix: hookFix, deployerFactory: deployerFactory}
		chainCfg := chilizHookChainConfig(forks)
		base := newHookState(behaviour, code)
		canPay := hookOriginBalance.CmpUint64(value) >= 0
		codeRuns := hookCodeRuns(base, code)

		// Path 1: Execute — the fuzzed code called from the origin (upstream harness).
		{
			cfg, obs := newHookConfig(chainCfg, base, hookOrigin, value)
			_, _, err := Execute(code, input, cfg)
			obs.checkDepth(t, "Execute")
			obs.checkEntries(t, "Execute", forks, hookOrigin)
			checkHookActivation(t, "Execute", forks, behaviour, codeRuns, canPay, err, obs)
		}

		// Path 2: Call — a fuzz-selected callee.
		{
			target, origin := hookCallee(callee, chainCfg)
			cfg, obs := newHookConfig(chainCfg, base, origin, value)
			_, leftOver, err := Call(target, input, cfg)
			obs.checkDepth(t, "Call")
			obs.checkEntries(t, "Call", forks, origin)
			switch callee % calleeKinds {
			case calleeContract:
				checkHookActivation(t, "Call", forks, behaviour, codeRuns, canPay, err, obs)
			case calleeEOA:
				if len(obs.entries) != 0 {
					t.Fatalf("Call(EOA): hook fired %d time(s) for a code-less callee", len(obs.entries))
				}
				if canPay && (err != nil || leftOver != hookFuzzGas) {
					t.Fatalf("Call(EOA): err=%v leftOver=%d, want nil and %d (simple send is free)", err, leftOver, hookFuzzGas)
				}
			case calleePrecompile, calleeSystem:
				if len(obs.entries) != 0 {
					t.Fatalf("Call(%x): hook fired %d time(s) for a precompile/system-contract callee", target, len(obs.entries))
				}
			case calleeHookAddr, calleeHookAddrUpgrader:
				if len(obs.entries) != 0 {
					t.Fatalf("Call(hook 7f01): hook fired %d time(s) for an EVM hook callee", len(obs.entries))
				}
				// COR-193: dispatching to a hook address warms the DeployerProxy
				// for the calling frame, so the warmth survives iff the frame
				// did not revert and the hooks are live.
				warm := cfg.State.AddressInAccessList(systemcontract.DeployerProxyContractAddress)
				if want := !sunset && err == nil; warm != want {
					t.Fatalf("Call(hook 7f01, sunset=%v, err=%v): DeployerProxy warm=%v, want %v", sunset, err, warm, want)
				}
			}
		}

		// Path 3: Create — the fuzzed code as init code, from the origin.
		{
			cfg, obs := newHookConfig(chainCfg, base, hookOrigin, value)
			_, _, _, err := Create(code, cfg)
			obs.checkDepth(t, "Create")
			obs.checkEntries(t, "Create", forks, hookOrigin)
			switch {
			case sunset:
				if len(obs.entries) != 0 {
					t.Fatalf("Create: hook fired %d time(s) under DeployerProxySunset", len(obs.entries))
				}
			case proxyFails(behaviour):
				if err == nil {
					t.Fatalf("Create: succeeded although the DeployerProxy %s", []string{"", "reverts", "burns all gas"}[behaviour])
				}
				if !errors.Is(err, vm.ErrNotAllowed) && !(hookFix && !canPay && errors.Is(err, vm.ErrInsufficientBalance)) {
					t.Fatalf("Create: err=%v, want ErrNotAllowed (or ErrInsufficientBalance ahead of the post-collision hook)", err)
				}
			case err == nil && len(obs.entries) == 0:
				t.Fatal("Create: succeeded without the deployment hook firing")
			}
		}

		// Path 4: the fuzzed code as init code of a CREATE issued by a contract.
		// Unlike path 3 this attributes the deployment to a contract that is not
		// tx.Origin, so the HasDeployOrigin/DeployerFactory attribution rule is
		// exercised on every exec rather than only when the fuzzed program
		// happens to reach a CREATE of its own. Value is zero so the creator
		// always runs and the deployment is always reached.
		{
			cfg, obs := newHookConfig(chainCfg, base, hookOrigin, 0)
			_, _, err := Call(hookCreator, code, cfg)
			obs.checkDepth(t, "Call(creator)")
			obs.checkEntries(t, "Call(creator)", forks, hookOrigin)
			switch {
			case sunset:
				if len(obs.entries) != 0 {
					t.Fatalf("Call(creator): hook fired %d time(s) under DeployerProxySunset", len(obs.entries))
				}
			case proxyFails(behaviour):
				// The creator's own invocation hook is consulted first and fails,
				// so the CREATE is never reached.
				if !errors.Is(err, vm.ErrNotAllowed) || len(obs.entries) != 1 {
					t.Fatalf("Call(creator): err=%v entries=%d, want ErrNotAllowed and exactly 1", err, len(obs.entries))
				}
			case err == nil:
				// Invocation hook for the creator, deployment hook for the child.
				// checkEntries above has already pinned each entry's calldata
				// (and hence the attribution) against the frame it fired in.
				if len(obs.entries) < 2 {
					t.Fatalf("Call(creator): %d hook entries, want at least 2 (invocation + deployment)", len(obs.entries))
				}
			}
		}
	})
}

// delegationTargets are the delegate addresses the corpus and
// TestHookDelegationDesignator point EIP-7702 designators at; together they
// cover every branch of hookCodeRuns and both sides of the system-contract
// question (the bypass keys on the callee, never on the delegate).
var delegationTargets = []common.Address{
	systemcontract.DeployerProxyContractAddress, // stub code: runs, hook fires
	systemcontract.StakingPoolContractAddress,   // STOP code: runs, hook fires (bypass is for the callee, not the delegate)
	systemcontract.EvmHookRuntimeUpgradeAddress, // dispatched in precompileOrHook, no stored code: nothing runs
	common.BytesToAddress([]byte{1}),            // ecrecover: precompile dispatch keys on the callee, so no stored code, nothing runs
	hookEOA,                                     // no code: nothing runs
	hookContract,                                // the designator itself: 23 bytes of "code" run (0xef is invalid), hook fires
}

// hookCodeRuns reports whether EVM.Call on hookContract, whose code is code,
// reaches the interpreter — the condition under which the invocation hook is
// consulted. It mirrors core/vm/evm.go: Call takes `code := evm.resolveCode(addr)`
// and skips both the hook and the interpreter when it is empty; under Prague,
// resolveCode follows a valid EIP-7702 designator (0xef0100 || address) one
// level, returning the delegate's stored code verbatim, so a designator whose
// delegate is itself a designator yields those 23 bytes (non-empty, runs and
// faults on 0xef), and one whose delegate has no stored code — an EOA, a
// precompile, the 0x...7f01 hook address, which is dispatched by
// precompileOrHook and never holds code — yields nothing. When code does run,
// applyChilizInvocationEvmHook(evm, addr, gas) is passed the callee addr, not
// the delegate: the hook asks checkContractActive(hookContract) and the
// IsSystemContract bypass is evaluated on hookContract, so a designator to a
// system contract still fires the hook.
func hookCodeRuns(base *state.StateDB, code []byte) bool {
	if delegate, ok := types.ParseDelegation(code); ok {
		return len(base.GetCode(delegate)) > 0
	}
	return len(code) > 0
}

// checkHookActivation pins the invocation-hook contract for a call whose callee
// is the fuzzed code: under sunset the hook never fires; otherwise, if the code
// runs, the hook fires exactly once at the root and a failing DeployerProxy
// turns the call into ErrNotAllowed, while an insufficient balance is rejected
// before the hook is consulted.
func checkHookActivation(t *testing.T, path string, forks hookForks, behaviour uint8, codeRuns, canPay bool, err error, obs *hookObserver) {
	t.Helper()
	if forks.sunset {
		if len(obs.entries) != 0 {
			t.Fatalf("%s: hook fired %d time(s) under DeployerProxySunset", path, len(obs.entries))
		}
		return
	}
	if !codeRuns {
		if len(obs.entries) != 0 {
			t.Fatalf("%s: hook fired %d time(s) for a code-less callee", path, len(obs.entries))
		}
		return
	}
	if !canPay {
		if !errors.Is(err, vm.ErrInsufficientBalance) || len(obs.entries) != 0 {
			t.Fatalf("%s: err=%v entries=%d, want ErrInsufficientBalance before any hook", path, err, len(obs.entries))
		}
		return
	}
	if len(obs.entries) == 0 {
		t.Fatalf("%s: code ran (err=%v) but the invocation hook never fired", path, err)
	}
	if proxyFails(behaviour) {
		if !errors.Is(err, vm.ErrNotAllowed) {
			t.Fatalf("%s: DeployerProxy failed but err=%v, want ErrNotAllowed", path, err)
		}
		if len(obs.entries) != 1 {
			t.Fatalf("%s: %d hook entries after a failing DeployerProxy, want exactly 1", path, len(obs.entries))
		}
	}
}

// stubCounter reads the stub's call counter from state.
func stubCounter(statedb *state.StateDB) uint64 {
	return statedb.GetState(systemcontract.DeployerProxyContractAddress, stubSlotCounter).Big().Uint64()
}

// stubAddress reads one of the stub's recorded calldata words as an address.
func stubAddress(statedb *state.StateDB, slot common.Hash) common.Address {
	return common.BytesToAddress(statedb.GetState(systemcontract.DeployerProxyContractAddress, slot).Bytes())
}

// stubSelector reads the selector the stub recorded.
func stubSelector(statedb *state.StateDB) []byte {
	return statedb.GetState(systemcontract.DeployerProxyContractAddress, stubSlotSelector).Bytes()[28:]
}

// createdAddress is the address CREATE derives for creator at its current nonce.
func createdAddress(statedb *state.StateDB, creator common.Address) common.Address {
	return crypto.CreateAddress(creator, statedb.GetNonce(creator))
}

// upgradeToMethod is the RuntimeUpgrade hook's only method,
// upgradeTo(address,bytes); the production definition is unexported.
var upgradeToMethod = func() abi.Method {
	addressType, _ := abi.NewType("address", "", nil)
	bytesType, _ := abi.NewType("bytes", "", nil)
	return abi.NewMethod("upgradeTo", "upgradeTo", abi.Function, "", false, false,
		abi.Arguments{{Type: addressType}, {Type: bytesType}}, nil)
}()

// upgradeToInput packs a call to the 0x...7f01 hook replacing target's code.
func upgradeToInput(tb testing.TB, target common.Address, code []byte) []byte {
	tb.Helper()
	args, err := upgradeToMethod.Inputs.Pack(target, code)
	if err != nil {
		tb.Fatal(err)
	}
	return append(bytes.Clone(upgradeToMethod.ID), args...)
}
