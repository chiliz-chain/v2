package runtime

// COR-233: fuzz the EVM dispatch paths the COR-217 harness never reaches.
//
// CLAUDE.md section 1 states the invariant plainly: every upstream
// evm.precompile(addr) call is replaced by evm.precompileOrHook(addr, caller)
// in Call, CallCode, DelegateCall and StaticCall.
//
// COR-217 (PR #78) drives its dispatches from bytecode, so it reaches more than
// its Call/Create entry points suggest: its delegateToProxy and create2Factory
// seeds pull DelegateCall and Create2 in through the interpreter. Measured on
// this package with -coverpkg=core/vm, running FuzzVmRuntimeWithHooks alone:
//
//	Call 62.7%  CallCode 0.0%  DelegateCall 44.4%  StaticCall 0.0%
//	create 71.0%  Create 100.0%  Create2 100.0%  CreateWithAddress 0.0%
//
// So THREE of the seven entry points were genuinely unreached — CallCode,
// StaticCall and the Chiliz-only CreateWithAddress (evm.go:783), which deploys
// at a caller-chosen address and is what places the system contracts at
// genesis. The targets in this file take the same measurement to:
//
//	Call 86.4%  CallCode 92.1%  DelegateCall 91.7%  StaticCall 91.9%
//	create 75.8%  Create 100.0%  Create2 100.0%  CreateWithAddress 100.0%
//
// This file reuses COR-217's harness wholesale — the stub DeployerProxy at
// 0x...7005, the hookObserver frame recorder, the fork-combination config — and
// adds a dispatch selector on top.
//
// Two asymmetries between Call and the rest are load-bearing and pinned here,
// because both are the exact shape an upstream call-path refactor "tidies up":
//
//   - the INVOCATION hook (applyChilizInvocationEvmHook) lives only in
//     EVM.Call. CallCode, DelegateCall and StaticCall dispatch hooks and
//     precompiles but never consult the DeployerProxy. Adding it to any of
//     them would change gas on every historical block that used those opcodes.
//   - the COR-193 warm-up (warmDeployerProxyOnHookDispatch) likewise sits only
//     in Call. Widening it would re-break the replay it was written to fix.

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core/opcodeCompiler/compiler"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// The dispatch variants. The first four are the call sites CLAUDE.md section 1
// names; the last three are the three ways into EVM.create.
const (
	dispatchCall uint8 = iota
	dispatchCallCode
	dispatchDelegateCall
	dispatchStaticCall
	dispatchCreate
	dispatchCreate2
	dispatchCreateWithAddress
	dispatchKinds
)

func dispatchName(variant uint8) string {
	return [...]string{"Call", "CallCode", "DelegateCall", "StaticCall", "Create", "Create2", "CreateWithAddress"}[variant%dispatchKinds]
}

// dispatchIsCreate reports whether a variant goes through EVM.create, where the
// deployment hook lives, rather than through a call site.
func dispatchIsCreate(variant uint8) bool {
	return variant%dispatchKinds >= dispatchCreate
}

// dispatchFiresInvocationHook reports whether a variant consults the
// DeployerProxy about its callee. Only EVM.Call does — see the file comment.
func dispatchFiresInvocationHook(variant uint8) bool {
	return variant%dispatchKinds == dispatchCall
}

// hookCallee maps a fuzzed callee selector onto the target address and the
// caller to reach it from. Shared with FuzzVmRuntimeWithHooks (COR-217), whose
// Call path selects its target the same way.
func hookCallee(callee uint8, chainCfg *params.ChainConfig) (target, caller common.Address) {
	caller = hookOrigin
	switch callee % calleeKinds {
	case calleeContract:
		target = hookContract
	case calleeEOA:
		target = hookEOA
	case calleePrecompile:
		active := vm.ActivePrecompiles(chainCfg.Rules(new(big.Int), true, 1))
		target = active[int(callee>>3)%len(active)]
	case calleeHookAddr:
		target = systemcontract.EvmHookRuntimeUpgradeAddress
	case calleeHookAddrUpgrader:
		target = systemcontract.EvmHookRuntimeUpgradeAddress
		caller = systemcontract.RuntimeUpgradeContractAddress
	case calleeSystem:
		target = bypassedSystemContracts[int(callee>>3)%len(bypassedSystemContracts)]
	}
	return target, caller
}

// runDispatch executes one dispatch variant, mirroring runtime.Call's set-up
// (defaults, tx context, EIP-2929 access-list preparation) so every variant
// starts from the same state the COR-217 paths do. It returns the address the
// create variants deployed at, which is the zero address for the call variants.
//
// CreateWithAddress is reachable only from Go — no opcode dispatches to it, and
// its one production caller is the genesis generator (genesis/create-genesis.go)
// — so driving it at all means calling the method directly, which is why every
// variant here is driven that way rather than from bytecode.
func runDispatch(cfg *Config, variant uint8, target common.Address, input []byte, salt uint64) (created common.Address, leftOver uint64, err error) {
	setDefaults(cfg)
	// newHookConfig puts the caller in Origin, exactly as runtime.Call does.
	caller := cfg.Origin
	evm := NewEnv(cfg)
	rules := cfg.ChainConfig.Rules(evm.Context.BlockNumber, evm.Context.Random != nil, evm.Context.Time)
	// runtime.Call warms the destination (runtime.go:140); runtime.Create passes
	// nil (runtime.go:178), because a creation transaction has no destination.
	// Mirror that split rather than always warming target: with callee ==
	// calleeSystem the fuzzer can aim a create variant at the DeployerProxy
	// itself, and pre-warming it here would charge the hook's inner call warm
	// where a real creation transaction charges it cold. Nothing asserts gas
	// today, but this harness guards the COR-193 access-list class, and a
	// harness that warms what production does not would quietly defeat any gas
	// assertion added later.
	dest := &target
	if dispatchIsCreate(variant) {
		dest = nil
	}
	cfg.State.Prepare(rules, cfg.Origin, cfg.Coinbase, dest, vm.ActivePrecompiles(rules), nil)

	value := uint256.MustFromBig(cfg.Value)
	switch variant % dispatchKinds {
	case dispatchCall:
		_, leftOver, err = evm.Call(caller, target, input, cfg.GasLimit, value)
	case dispatchCallCode:
		_, leftOver, err = evm.CallCode(caller, target, input, cfg.GasLimit, value)
	case dispatchDelegateCall:
		// originCaller is the frame the delegation chain started in; caller is
		// the immediate one, and it is the immediate one precompileOrHook is
		// given. Passing a distinct originCaller keeps the two apart, so a
		// swap of the arguments shows up as an invalid-caller verdict.
		_, leftOver, err = evm.DelegateCall(hookEOA, caller, target, input, cfg.GasLimit, value)
	case dispatchStaticCall:
		_, leftOver, err = evm.StaticCall(caller, target, input, cfg.GasLimit)
	case dispatchCreate:
		_, created, leftOver, err = evm.Create(caller, input, cfg.GasLimit, value)
	case dispatchCreate2:
		_, created, leftOver, err = evm.Create2(caller, input, cfg.GasLimit, value, uint256.NewInt(salt))
	case dispatchCreateWithAddress:
		// CreateWithAddress discards the address create() returns (evm.go:783),
		// so the only address there is to report back is the one it was told.
		// Every "created == wantCreated" comparison is therefore a tautology for
		// this variant; the claim that it deployed anything AT ALL is carried by
		// the nonce assertions in checkDeploymentDispatch and
		// TestCreateWithAddressDeploysWhereTold.
		created = target
		_, leftOver, err = evm.CreateWithAddress(caller, input, cfg.GasLimit, cfg.Value, target)
	}
	return created, leftOver, err
}

// dispatchCreatedAddress derives the address a create variant must deploy at.
// The deployment hook must not perturb it.
//
// It calls crypto.CreateAddress/CreateAddress2 — the same functions production
// calls — so this is NOT an independent reimplementation of the derivation, and
// a bug inside those functions would be invisible here. What is independent is
// the INPUTS: the caller's nonce is read before the dispatch rather than during
// it, the salt and init code come from the fuzzer, and the hash is taken with
// crypto.Keccak256 rather than production's crypto.HashData(evm.interpreter.hasher, …).
// So the claim this pins is the one that matters for the hook: a create() that
// swapped the CREATE2 derivation for the nonce one, or let the hook's own
// activity move the caller's nonce before the address was taken, is caught.
func dispatchCreatedAddress(cfg *Config, variant uint8, caller, target common.Address, code []byte, salt uint64) common.Address {
	switch variant % dispatchKinds {
	case dispatchCreate:
		return crypto.CreateAddress(caller, cfg.State.GetNonce(caller))
	case dispatchCreate2:
		return crypto.CreateAddress2(caller, uint256.NewInt(salt).Bytes32(), crypto.Keccak256(code))
	default:
		return target // CreateWithAddress deploys where it is told
	}
}

func FuzzHookDispatchVariants(f *testing.F) {
	stop := []byte{byte(vm.STOP)}
	factory := []byte{byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.CREATE), byte(vm.STOP)}
	upgradeInput := upgradeToInput(f, common.HexToAddress("0x1234"), stop)

	// One seed per variant against an ordinary contract callee, the baseline
	// every other seed is read against.
	for variant := uint8(0); variant < dispatchKinds; variant++ {
		f.Add(stop, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeContract, variant, uint64(0), false)
	}
	// Each variant crossed with the callee kinds that decide a different branch:
	// the hook address from a non-upgrader (which every variant must refuse),
	// the hook address from 0x...7004 (which every variant must serve), a
	// system contract, a precompile and a code-less EOA.
	for variant := uint8(0); variant < dispatchKinds; variant++ {
		f.Add(stop, upgradeInput, proxyReturnTrue, false, false, false, false, uint64(0), calleeHookAddr, variant, uint64(0), false)
		f.Add(stop, upgradeInput, proxyReturnTrue, false, false, false, false, uint64(0), calleeHookAddrUpgrader, variant, uint64(0), false)
		f.Add(stop, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeSystem, variant, uint64(0), false)
		f.Add(stop, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleePrecompile, variant, uint64(0), false)
		f.Add(stop, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeEOA, variant, uint64(0), false)
	}
	// Each variant crossed with the proxy behaviours: accept, revert, burn all
	// gas, return garbage. A rejecting proxy must block the guarded call or
	// create — and must be irrelevant to the three call variants that never
	// consult it.
	for variant := uint8(0); variant < dispatchKinds; variant++ {
		f.Add(stop, []byte{}, proxyRevert, false, false, false, false, uint64(0), calleeContract, variant, uint64(0), false)
		f.Add(stop, []byte{}, proxyBurnGas, false, false, false, false, uint64(0), calleeContract, variant, uint64(0), false)
		f.Add(stop, []byte{}, proxyGarbage, false, false, false, false, uint64(0), calleeContract, variant, uint64(0), false)
	}
	// Sunset, on every variant: no DeployerProxy hook may be consulted.
	//
	// Sunset is where the two gates come apart, so BOTH hook-address callees are
	// seeded here. The DeployerProxy hooks are gated on deployerProxyHooksActive()
	// (HasRuntimeUpgrade && !DeployerProxySunset); the 0x...7f01 dispatch is not
	// — evmHookRuntimeUpgrade.Run is gated on HasRuntimeUpgrade alone
	// (systemcontract/upgrade.go:48) and precompileOrHook (evm.go:64) is ungated.
	// Folding the two into one predicate is exactly the tidy-up CLAUDE.md
	// section 1 warns against, and under sunset it turns 0x...7f01 into an
	// ordinary code-less account whose call returns (nil, gas, nil).
	//
	// calleeHookAddrUpgrader pins that the upgrade is still SERVED under sunset
	// (asserted on the state, below, not on the absence of an error — a silent
	// no-op has no error either). calleeHookAddr pins that it is still REFUSED.
	// Without the second of these the whole sunset half of property 2 was
	// unwitnessed: the only sunset seed landed in the wellFormedUpgradeTo arm,
	// which asked for err == nil, and a no-op obliges.
	for variant := uint8(0); variant < dispatchKinds; variant++ {
		f.Add(stop, upgradeInput, proxyReturnTrue, true, false, false, false, uint64(0), calleeHookAddrUpgrader, variant, uint64(0), false)
		f.Add(stop, upgradeInput, proxyReturnTrue, true, false, false, false, uint64(0), calleeHookAddr, variant, uint64(0), false)
	}
	// The create variants crossed with the attribution forks and the
	// pre-/post-collision-check hook placement.
	//
	// The init code goes in the SECOND slot. runDispatch hands `input` to
	// Create/Create2/CreateWithAddress; `code` only ever becomes the callee's
	// stored code in newHookState. Seeding the factory as `code` deployed EMPTY
	// init code instead — so the outer create had no nested CREATE, its only
	// deployment hook was attributed to an EOA, and the attribution forks these
	// seeds cross could not tell themselves apart.
	//
	// `code` is left empty so that CreateWithAddress — whose target IS
	// hookContract — has a free address to deploy at. With code there, every
	// one of these seeds died on ErrContractAddressCollision before reaching
	// the post-DeploymentHookFix hook site at all.
	for _, variant := range []uint8{dispatchCreate, dispatchCreate2, dispatchCreateWithAddress} {
		f.Add([]byte{}, factory, proxyReturnTrue, false, true, false, false, uint64(0), calleeContract, variant, uint64(0), false)
		f.Add([]byte{}, factory, proxyReturnTrue, false, true, true, false, uint64(0), calleeContract, variant, uint64(7), false)
		f.Add([]byte{}, factory, proxyReturnTrue, false, true, false, true, uint64(0), calleeContract, variant, uint64(1), false)
		f.Add([]byte{}, factory, proxyRevert, false, false, true, false, uint64(0), calleeContract, variant, uint64(0), false)
		// The post-DeploymentHookFix half of checkDeploymentDispatch only bites
		// when a create trips one of the checks the fix moved the hook BEHIND,
		// so each variant is seeded twice with hookFix on and a pre-hook error
		// waiting: a value no balance can cover, and a target that already
		// holds code. The second matters only for CreateWithAddress, whose
		// target is chosen rather than derived; for Create/Create2 it is an
		// ordinary run. Hoisting the hook back to the pre-fix site fails both.
		f.Add([]byte{}, factory, proxyReturnTrue, false, false, true, false, ^uint64(0), calleeContract, variant, uint64(0), false)
		f.Add(stop, factory, proxyReturnTrue, false, false, true, false, uint64(0), calleeContract, variant, uint64(0), false)
	}
	// CreateWithAddress aimed at a system contract: the genesis path, where the
	// deployment hook must be bypassed on the created address.
	f.Add(stop, []byte{}, proxyReturnTrue, false, false, false, false, uint64(0), calleeSystem, dispatchCreateWithAddress, uint64(0), false)
	f.Add(stop, []byte{}, proxyRevert, false, false, false, false, uint64(0), calleeSystem, dispatchCreateWithAddress, uint64(0), false)
	// Value transfers, on the variants that move value and the ones that do not.
	for _, variant := range []uint8{dispatchCall, dispatchCallCode, dispatchDelegateCall, dispatchStaticCall, dispatchCreate2} {
		f.Add(stop, []byte{}, proxyReturnTrue, false, false, false, false, uint64(1), calleeContract, variant, uint64(0), false)
	}
	// A value no balance can cover, which must be refused before any hook.
	f.Add(stop, []byte{}, proxyReturnTrue, false, false, false, false, ^uint64(0), calleeContract, dispatchCall, uint64(0), false)
	f.Add(stop, []byte{}, proxyReturnTrue, false, false, false, false, ^uint64(0), calleeContract, dispatchCallCode, uint64(0), false)
	f.Add(stop, []byte{}, proxyReturnTrue, false, false, false, false, ^uint64(0), calleeContract, dispatchCreate2, uint64(0), false)

	// Every call variant carries the dispatch twice — once for the optimized
	// interpreter and once for the base one — and the hook lookup sits above
	// both. Without this flag half of each of CallCode/DelegateCall/StaticCall
	// stays unexecuted, so a mutation confined to the optimized branch would
	// pass unseen.
	for variant := uint8(0); variant < dispatchKinds; variant++ {
		f.Add(stop, upgradeInput, proxyReturnTrue, false, false, false, false, uint64(0), calleeHookAddrUpgrader, variant, uint64(0), true)
		f.Add(stop, upgradeInput, proxyReturnTrue, false, false, false, false, uint64(0), calleeHookAddr, variant, uint64(0), true)
		// The factory in BOTH slots. As stored code it gives the four call
		// variants something to run under the optimized interpreter — EVM.Call
		// returns early on a code-less callee (evm.go), before the optimized
		// branch — and as `input` it is the real init code Create and Create2
		// deploy. CreateWithAddress still collides on this seed, which is why
		// the line below repeats it with the target left empty.
		f.Add(factory, factory, proxyReturnTrue, false, false, false, false, uint64(0), calleeContract, variant, uint64(0), true)
		f.Add([]byte{}, factory, proxyReturnTrue, false, false, false, false, uint64(0), calleeContract, variant, uint64(0), true)
		f.Add(stop, []byte{}, proxyRevert, false, false, false, false, uint64(0), calleeContract, variant, uint64(0), true)
	}

	f.Fuzz(func(t *testing.T, code, input []byte, proxyBehaviour uint8, sunset, deployOrigin, hookFix, deployerFactory bool, value uint64, callee, variant uint8, salt uint64, optimized bool) {
		behaviour := proxyBehaviour % proxyBehaviourCount
		forks := hookForks{sunset: sunset, deployOrigin: deployOrigin, hookFix: hookFix, deployerFactory: deployerFactory}
		chainCfg := chilizHookChainConfig(forks)
		base := newHookState(behaviour, code)
		target, caller := hookCallee(callee, chainCfg)
		name := dispatchName(variant)

		cfg, obs := newHookConfig(chainCfg, base, caller, value)
		cfg.EVMConfig.EnableOpcodeOptimizations = optimized
		// Save and restore the optimizer's process-global flag around the run.
		// initNewContract (evm.go) calls compiler.DisableOptimization() on entry
		// but compiler.EnableOptimization() only on its success path, so every
		// create whose init code errors leaves the global off, and it leaks into
		// every later test in this package (the same class as the graphql
		// global-config pollution already on record for this repo).
		//
		// The flag is read by the compiler's background taskProcessor
		// goroutines, which drain taskChannel asynchronously, so neither this
		// write nor production's own is ordered against them. It is an
		// atomic.Bool for that reason (opCodeProcessor.go) — as a plain bool the
		// race is reported against initNewContract itself, and the optimized
		// create seeds above reach it from this package.
		defer func(was bool) {
			if was {
				compiler.EnableOptimization()
			} else {
				compiler.DisableOptimization()
			}
		}(compiler.IsEnabled())
		wantCreated := dispatchCreatedAddress(cfg, variant, caller, target, input, salt)
		created, leftOver, err := runDispatch(cfg, variant, target, input, salt)

		// Property 4: every frame exits at the depth it entered, including when
		// the hook reverts or burns its gas.
		obs.checkDepth(t, name)
		// Property 7 (attribution): every hook entry's calldata matches the
		// frame it fired in — checkContractActive(callee) for the invocation
		// hook, registerDeployedContract(deployer, created) for the deployment
		// hook, with the deployer following the HasDeployOrigin/DeployerFactory
		// rule. For DelegateCall the frame the tracer records is caller->addr,
		// so a hook asked about the wrong party fails here.
		obs.checkEntries(t, name, forks, caller)

		canPay := hookOriginBalance.CmpUint64(value) >= 0
		// DelegateCall inherits its parent's value and StaticCall carries none,
		// so neither consults the balance: only the other variants can be
		// pre-empted by ErrInsufficientBalance before the dispatch happens.
		reachesDispatch := canPay ||
			variant%dispatchKinds == dispatchDelegateCall ||
			variant%dispatchKinds == dispatchStaticCall

		switch {
		case forks.sunset:
			// Property 3: the sunset fork turns off the DeployerProxy hooks on
			// every variant. It does NOT turn off the 0x...7f01 dispatch, which
			// is gated on HasRuntimeUpgrade alone — checked below.
			if len(obs.entries) != 0 {
				t.Fatalf("%s: hook fired %d time(s) under DeployerProxySunset", name, len(obs.entries))
			}

		case dispatchIsCreate(variant):
			checkDeploymentDispatch(t, name, behaviour, hookFix, wantCreated, created, err, obs, cfg.State)

		case !dispatchFiresInvocationHook(variant):
			// The load-bearing asymmetry: CallCode, DelegateCall and StaticCall
			// carry no invocation hook, so the DeployerProxy is never consulted
			// about THEIR callee, whatever it is and however the proxy behaves.
			// An upstream refactor that "completes" the set by adding the hook
			// to these would change gas on every historical block using those
			// opcodes — a resync failure of the COR-193 class.
			//
			// Only the root frame is the claim: the callee's own code may open
			// nested CALL and CREATE frames, and the hooks those fire are
			// ordinary and already pinned by checkEntries.
			if n := rootHookEntries(obs); n != 0 {
				t.Fatalf("%s: consulted the DeployerProxy %d time(s) about its own callee; the invocation hook exists only in EVM.Call", name, n)
			}

		default: // dispatchCall
			switch callee % calleeKinds {
			case calleeContract:
				checkHookActivation(t, name, forks, behaviour, hookCodeRuns(base, code), canPay, err, obs)
			case calleeEOA, calleePrecompile, calleeSystem:
				// Property 5: system contracts (and precompiles, and code-less
				// accounts) bypass the invocation hook.
				if n := rootHookEntries(obs); n != 0 {
					t.Fatalf("%s(%x): hook fired %d time(s) for a bypassed callee", name, target, n)
				}
			case calleeHookAddr, calleeHookAddrUpgrader:
				if n := rootHookEntries(obs); n != 0 {
					t.Fatalf("%s(hook 7f01): hook fired %d time(s) for an EVM hook callee", name, n)
				}
			}
		}

		// Property 2: dispatch to the 0x...7f01 hook address reaches the
		// runtime-upgrade hook through EVERY variant, not only Call. There is
		// no account code at that address, so without precompileOrHook the
		// frame would be an empty-account no-op and succeed; with it, a caller
		// other than 0x...7004 is refused. Reverting any one variant's
		// precompileOrHook to precompile turns this error into a silent
		// success, which is how mutation (a) is caught per variant.
		//
		// This property holds under sunset too — see the sunset seeds above.
		upgradeAddr, upgradeCode, wellFormed := decodeUpgradeTo(input)
		if !dispatchIsCreate(variant) && target == systemcontract.EvmHookRuntimeUpgradeAddress && reachesDispatch {
			switch {
			case callee%calleeKinds == calleeHookAddr:
				if err == nil {
					t.Fatalf("%s(hook 7f01) from %x succeeded; the hook must refuse every caller but the RuntimeUpgrade contract", name, caller)
				}
			case wellFormed:
				// COR-244: when the static-call violation is fixed, the
				// StaticCall iteration of this arm is one of the three places
				// that must change — a static frame will then REFUSE the write,
				// so both assertions below flip. See TestStaticCallHookWritesState.
				if err != nil {
					t.Fatalf("%s(hook 7f01) from the RuntimeUpgrade contract failed with %v; upgradeTo must be served through every dispatch variant", name, err)
				}
				// The oracle that makes this arm bite: assert the hook's EFFECT,
				// not the absence of an error. A dispatch that never reaches the
				// hook at all — precompileOrHook gated on
				// deployerProxyHooksActive(), 0x...7f01 reverting to a code-less
				// account under sunset — returns (nil, gas, nil), which err ==
				// nil accepts and an unchanged code slot does not.
				if got := cfg.State.GetCode(upgradeAddr); !bytes.Equal(got, upgradeCode) {
					t.Fatalf("%s(hook 7f01): upgradeTo(%x, %d bytes) reported success but the code at %x is %d bytes; the runtime-upgrade hook never ran",
						name, upgradeAddr, len(upgradeCode), upgradeAddr, len(got))
				}
			default:
				// Anything else — a bare selector, truncated arguments, random
				// bytes — must be an error and never a silent success. The
				// regression seed 0f700c7273df7560 is the upgradeTo selector
				// with no arguments at all, which the fuzzer found and which
				// the first version of this property wrongly demanded be served.
				if err == nil {
					t.Fatalf("%s(hook 7f01) accepted %x, which is not a well-formed upgradeTo call", name, input)
				}
			}
		}

		// COR-193 scope: the warm-up fires on hook dispatch in EVM.Call only,
		// and only while the DeployerProxy hooks are live. It is journaled with
		// the frame, so a reverted frame leaves the address cold.
		if !dispatchIsCreate(variant) && target == systemcontract.EvmHookRuntimeUpgradeAddress {
			warm := cfg.State.AddressInAccessList(systemcontract.DeployerProxyContractAddress)
			want := variant%dispatchKinds == dispatchCall && !sunset && err == nil
			if warm != want {
				t.Fatalf("%s(hook 7f01, sunset=%v, err=%v): DeployerProxy warm=%v, want %v (COR-193 warms on Call only)",
					name, sunset, err, warm, want)
			}
		}

		// A frame that never ran cannot have spent gas.
		if leftOver > cfg.GasLimit {
			t.Fatalf("%s: leftOver %d exceeds the %d it was given", name, leftOver, cfg.GasLimit)
		}
	})
}

// decodeUpgradeTo reports whether input is a complete upgradeTo(address,bytes)
// call and, when it is, returns the address and the code it asks the
// runtime-upgrade hook to write — so a test can check the hook's EFFECT rather
// than only the absence of an error.
//
// It mirrors the hook's own matchesMethod (systemcontract/upgrade.go) exactly,
// including the two type assertions that answer errFailedToUnpack: the selector
// alone is not enough, because matchesMethod unpacks the arguments and answers
// errMethodNotFound when they do not decode, so a bare selector is refused
// exactly like random bytes.
func decodeUpgradeTo(input []byte) (addr common.Address, code []byte, ok bool) {
	if len(input) < 4 || !bytes.Equal(input[:4], upgradeToMethod.ID) {
		return common.Address{}, nil, false
	}
	values, err := upgradeToMethod.Inputs.UnpackValues(input[4:])
	if err != nil || len(values) != len(upgradeToMethod.Inputs) {
		return common.Address{}, nil, false
	}
	addr, addrOK := values[0].(common.Address)
	code, codeOK := values[1].([]byte)
	if !addrOK || !codeOK {
		return common.Address{}, nil, false
	}
	return addr, code, true
}

// preHookCreateError reports whether err is one create() can raise BEFORE the
// post-DeploymentHookFix hook site: the balance, nonce and collision checks that
// the fix moved the hook behind. Seeing one of these together with a hook entry
// means the hook ran too early.
func preHookCreateError(err error) bool {
	return errors.Is(err, vm.ErrInsufficientBalance) ||
		errors.Is(err, vm.ErrNonceUintOverflow) ||
		errors.Is(err, vm.ErrContractAddressCollision)
}

// rootHookEntries counts the hook entries fired for the dispatch under test
// itself, i.e. those whose parent is the root frame. applyChilizInvocationEvmHook
// and applyChilizDeploymentEvmHook both bump evm.depth before calling the
// DeployerProxy, so a hook fired for the root frame records that frame — at
// depth 0 — as its parent, while anything the callee's own code goes on to do
// is recorded one level deeper.
func rootHookEntries(obs *hookObserver) int {
	n := 0
	for _, e := range obs.entries {
		if e.parent.depth == 0 {
			n++
		}
	}
	return n
}

// checkDeploymentDispatch pins the deployment hook for the three create
// variants: something must actually be deployed at the expected address, the
// hook must fire (unless that address is a system contract, the genesis case),
// it must be told the address dispatchCreatedAddress derived, and a rejecting
// DeployerProxy must block the deployment.
func checkDeploymentDispatch(t *testing.T, name string, behaviour uint8, hookFix bool, wantCreated, created common.Address, err error, obs *hookObserver, statedb vm.StateDB) {
	t.Helper()
	register := systemcontract.EvmHooksAbi.Methods["registerDeployedContract"]

	// A create that reports success must have left an account behind at
	// wantCreated. Address equality cannot say so on its own, and for
	// CreateWithAddress it says nothing at all: create() discards the address it
	// deployed at, so `created` can only ever be the address runDispatch was
	// told (see runDispatch), which makes `created != wantCreated` below a
	// comparison of a value with itself. A create() refactor that returned
	// success without creating the account would keep err == nil and still fire
	// the hook with the right calldata — and genesis generation would silently
	// produce empty accounts at 0x...7001-7006, the very path this entry point
	// exists for and the one where the calldata check below is skipped.
	//
	// EIP-158 sets a new contract's nonce to 1 (evm.go), which is the cheapest
	// witness that survives here. GetCode is NOT a substitute: init code that
	// returns nothing deploys empty code, indistinguishable from no account.
	if err == nil && statedb.GetNonce(wantCreated) == 0 {
		t.Fatalf("%s: reported success but left no account at %x", name, wantCreated)
	}
	deployments := 0
	for _, e := range obs.entries {
		// Root frame only: init code may itself CREATE, and those nested
		// deployments legitimately register addresses of their own. Their
		// calldata is pinned by checkEntries against the frame they fired in;
		// the claim here is about the address THIS dispatch deployed at.
		// (Regression seed d46bf51189283b39 is init code that does exactly
		// that — a nested CREATE — which the first version of this check
		// mistook for the outer one deploying at the wrong address.)
		if e.parent.depth != 0 || len(e.input) < 4 || !bytes.Equal(e.input[:4], register.ID) {
			continue
		}
		deployments++
		// Property 8/9: the address the hook is told is the one derived from
		// (caller, nonce), (caller, salt, initcode hash) or, for
		// CreateWithAddress, the one the caller chose. The hook must not move it.
		if got := common.BytesToAddress(e.input[4+32+12 : 4+64]); got != wantCreated {
			t.Fatalf("%s: deployment hook told address %x, want %x", name, got, wantCreated)
		}
	}

	// Property 5 at the create site: a deployment AT a system-contract address
	// bypasses the hook. This is the genesis path — create-genesis.go places
	// 0x...7001-7006 with CreateWithAddress — and it must stay bypassed, or
	// genesis generation would need a DeployerProxy that does not exist yet.
	if systemcontract.IsSystemContract(wantCreated) {
		if deployments != 0 {
			t.Fatalf("%s: deployment hook fired %d time(s) for system-contract address %x", name, deployments, wantCreated)
		}
		return
	}

	// Where the hook sits inside create() is the HasDeploymentHookFix rule, and
	// it decides what can pre-empt it: pre-fix the hook is the first thing
	// create() does after the depth check, so it always runs; post-fix it sits
	// behind the balance, nonce and collision checks, which can therefore
	// answer before the DeployerProxy is ever asked.
	//
	// Both halves are enforced. Without the post-fix half, moving the hook back
	// ahead of the collision check would go unnoticed: the hook fires, a
	// pre-hook check then errors, and every remaining case below is skipped.
	if hookFix && deployments != 0 && preHookCreateError(err) {
		t.Fatalf("%s: post-DeploymentHookFix the deployment hook sits behind the balance, nonce and collision checks, but it fired and then %v was returned", name, err)
	}
	switch {
	case !hookFix && deployments == 0:
		t.Fatalf("%s: pre-DeploymentHookFix the deployment hook runs ahead of every other check in create(), but it never fired (err=%v)", name, err)
	case deployments == 0:
		if err == nil {
			t.Fatalf("%s: deployed %x without the deployment hook firing", name, created)
		}
	case proxyFails(behaviour):
		if !errors.Is(err, vm.ErrNotAllowed) {
			t.Fatalf("%s: the DeployerProxy %s but err=%v, want ErrNotAllowed",
				name, []string{"", "reverts", "burns all gas"}[behaviour], err)
		}
	case err == nil:
		if created != wantCreated {
			t.Fatalf("%s: deployed at %x, want %x", name, created, wantCreated)
		}
	}
}

// TestHookDispatchReachesEveryCallVariant pins property 2 deterministically:
// upgradeTo(target, code) sent to 0x...7f01 by the RuntimeUpgrade contract
// replaces target's code through Call, CallCode, DelegateCall AND StaticCall,
// and is refused from any other caller through all four.
//
// It also records property 6, which is a finding rather than a guarantee: see
// TestStaticCallHookWritesState below.
//
// COR-244: the StaticCall iteration of this loop asserts the CURRENT behaviour,
// not the correct one. When COR-244 lands, a static frame will refuse the write,
// so for variant == dispatchStaticCall the first half must expect an error and
// no code at `upgraded`, while Call, CallCode and DelegateCall keep asserting
// what they assert today. Change this loop; do not relax it for all four.
func TestHookDispatchReachesEveryCallVariant(t *testing.T) {
	upgraded := common.HexToAddress("0x1234")
	input := upgradeToInput(t, upgraded, []byte{byte(vm.STOP)})

	for variant := dispatchCall; variant <= dispatchStaticCall; variant++ {
		t.Run(dispatchName(variant), func(t *testing.T) {
			// Served for the RuntimeUpgrade contract.
			base := newHookState(proxyReturnTrue, []byte{byte(vm.STOP)})
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{}), base, systemcontract.RuntimeUpgradeContractAddress, 0)
			_, _, err := runDispatch(cfg, variant, systemcontract.EvmHookRuntimeUpgradeAddress, input, 0)
			obs.checkDepth(t, dispatchName(variant))
			if err != nil {
				t.Fatalf("upgradeTo through %s: %v", dispatchName(variant), err)
			}
			if got := cfg.State.GetCode(upgraded); len(got) != 1 {
				t.Fatalf("%s did not reach the runtime-upgrade hook: code at %x is %d bytes, want 1",
					dispatchName(variant), upgraded, len(got))
			}
			if len(obs.entries) != 0 {
				t.Fatalf("%s: the DeployerProxy was consulted for a hook-address callee", dispatchName(variant))
			}

			// Refused for anyone else. Without precompileOrHook this address
			// holds no code and the frame would succeed silently.
			cfg2, _ := newHookConfig(chilizHookChainConfig(hookForks{}), base, hookOrigin, 0)
			_, _, err = runDispatch(cfg2, variant, systemcontract.EvmHookRuntimeUpgradeAddress, input, 0)
			if err == nil {
				t.Fatalf("%s: upgradeTo from a non-upgrader succeeded", dispatchName(variant))
			}
			if got := cfg2.State.GetCode(upgraded); len(got) != 0 {
				t.Fatalf("%s: a refused upgrade still wrote %d bytes", dispatchName(variant), len(got))
			}
		})
	}
}

// TestStaticCallHookWritesState records property 6 of COR-233 as it actually
// behaves, NOT as it ought to: a STATICCALL to the 0x...7f01 hook from the
// RuntimeUpgrade contract writes code, because precompileOrHook dispatches the
// hook before the interpreter's static guard and the hook writes through
// hookStateDBAdapter with no read-only check of its own.
//
// That is a violation of the static-call guarantee at the EVM level and is
// filed as COR-244; per COR-233 it is a finding to report, not to fix inside a
// test PR. Fixing it is a consensus change and needs a fork gate, since blocks
// produced today accept the write.
//
// The test therefore pins the CURRENT behaviour so the fix, when it lands, has
// to come here and say so deliberately. COR-244 breaks exactly three places in
// this file, and all three must be updated together:
//
//  1. this test, which becomes an assertion that the write is refused;
//  2. the StaticCall iteration of TestHookDispatchReachesEveryCallVariant;
//  3. the wellFormed arm of property 2 in FuzzHookDispatchVariants, reached by
//     the calleeHookAddrUpgrader seeds.
func TestStaticCallHookWritesState(t *testing.T) {
	upgraded := common.HexToAddress("0x1234")
	input := upgradeToInput(t, upgraded, []byte{byte(vm.STOP)})

	base := newHookState(proxyReturnTrue, []byte{byte(vm.STOP)})
	cfg, _ := newHookConfig(chilizHookChainConfig(hookForks{}), base, systemcontract.RuntimeUpgradeContractAddress, 0)
	if _, _, err := runDispatch(cfg, dispatchStaticCall, systemcontract.EvmHookRuntimeUpgradeAddress, input, 0); err != nil {
		t.Fatalf("StaticCall to the hook: %v", err)
	}
	if got := cfg.State.GetCode(upgraded); len(got) != 1 {
		t.Fatalf("code at %x is %d bytes, want 1 — if this now fails because the "+
			"static frame correctly refuses the write, COR-244 has been fixed: invert "+
			"this test, update the other two sites named above, and confirm the fix "+
			"is fork-gated", upgraded, len(got))
	}
}

// TestCreateWithAddressDeploysWhereTold pins the Chiliz-only create entry point
// (evm.go:783), whose single production caller is the genesis generator:
//
//   - it deploys at the caller-chosen address, not at a derived one;
//   - it does NOT bypass the deployment hook for an ordinary address — a
//     caller-chosen address is not a way around the DeployerProxy;
//   - it DOES bypass it for a system-contract address, which is what makes
//     genesis generation possible before any DeployerProxy exists.
func TestCreateWithAddressDeploysWhereTold(t *testing.T) {
	code := []byte{byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.RETURN)} // deploys empty code
	register := systemcontract.EvmHooksAbi.Methods["registerDeployedContract"]

	for _, tc := range []struct {
		name           string
		at             common.Address
		wantDeployHook bool
	}{
		{"ordinary address", common.HexToAddress("0x00000000000000000000000000000000deadbeef"), true},
		{"system contract", systemcontract.StakingPoolContractAddress, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newHookState(proxyReturnTrue, []byte{byte(vm.STOP)})
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{}), base, hookOrigin, 0)
			// newHookState gives every system contract STOP code so a call to
			// one has something to run; genesis deploys into empty accounts, so
			// clear the target or the create trips the collision check first.
			cfg.State.SetCode(tc.at, nil, tracing.CodeChangeUnspecified)
			created, _, err := runDispatch(cfg, dispatchCreateWithAddress, tc.at, code, 0)
			obs.checkDepth(t, "CreateWithAddress")
			if err != nil {
				t.Fatalf("CreateWithAddress: %v", err)
			}
			if created != tc.at {
				t.Fatalf("deployed at %x, want the address it was told, %x", created, tc.at)
			}
			// The line above is a tautology — CreateWithAddress discards the
			// address create() returns, so runDispatch can only report back the
			// one it passed in. The property this test is NAMED for is carried
			// by the state: EIP-158 sets a new contract's nonce to 1 (evm.go).
			// GetCode cannot stand in, because this init code deliberately
			// deploys EMPTY code, so an empty GetCode is the expected result
			// whether or not anything was created at all.
			if got := cfg.State.GetNonce(tc.at); got != 1 {
				t.Fatalf("CreateWithAddress reported success but the nonce at %x is %d, want 1: nothing was deployed there", tc.at, got)
			}
			deployments := 0
			for _, e := range obs.entries {
				if len(e.input) >= 4 && bytes.Equal(e.input[:4], register.ID) {
					deployments++
				}
			}
			if got := deployments > 0; got != tc.wantDeployHook {
				t.Fatalf("deployment hook fired %d time(s), want fired=%v", deployments, tc.wantDeployHook)
			}
		})
	}
}

// TestCreate2AddressUnaffectedByDeploymentHook pins property 8: the hook runs
// between the address derivation and the account creation, and must not move
// the address CREATE2 promises for (caller, salt, init code).
//
// As in dispatchCreatedAddress, the expectation is computed with production's
// own crypto.CreateAddress2, so this does not re-derive the address from the
// spec — it pins that the hook leaves the (caller, salt, init-code-hash) inputs
// and the resulting address alone.
func TestCreate2AddressUnaffectedByDeploymentHook(t *testing.T) {
	code := []byte{byte(vm.PUSH1), 0x00, byte(vm.PUSH1), 0x00, byte(vm.RETURN)}
	for _, salt := range []uint64{0, 1, ^uint64(0)} {
		t.Run(fmt.Sprintf("salt=%d", salt), func(t *testing.T) {
			base := newHookState(proxyReturnTrue, code)
			cfg, obs := newHookConfig(chilizHookChainConfig(hookForks{}), base, hookOrigin, 0)
			want := crypto.CreateAddress2(hookOrigin, uint256.NewInt(salt).Bytes32(), crypto.Keccak256(code))
			created, _, err := runDispatch(cfg, dispatchCreate2, common.Address{}, code, salt)
			obs.checkDepth(t, "Create2")
			if err != nil {
				t.Fatalf("Create2: %v", err)
			}
			if created != want {
				t.Fatalf("Create2 deployed at %x, want %x", created, want)
			}
			if len(obs.entries) == 0 {
				t.Fatal("Create2 deployed without the deployment hook firing")
			}
		})
	}
}
