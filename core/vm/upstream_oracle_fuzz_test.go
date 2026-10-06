package vm

// COR-210: differential oracle against upstream BSC.
//
// upstreamPrecompile below is a verbatim copy of (*EVM).precompile from
// bnb-chain/bsc tag v1.7.8, core/vm/evm.go (the last merged upstream tag, see
// .github/bsc-sync/last-synced-tag), rewritten as a free function so it can sit
// next to the Chiliz method of the same name. Upstream dispatches every
// CALL/CALLCODE/DELEGATECALL/STATICCALL target through evm.precompile(addr);
// Chiliz replaces each of those sites with evm.precompileOrHook(addr, caller).
//
// SCOPE — read this before relying on the target after an upstream sync. It
// pins ONE of the pieces CLAUDE.md §1 calls "EVM hooks": the address dispatch
// inside the precompileOrHook wrapper, and nothing else. Specifically NOT
// covered, and each of them is a piece §1 flags as more fragile than this one:
//
//   - the bare evm.warmDeployerProxyOnHookDispatch(addr) line in Call
//     (core/vm/evm.go, COR-193). Delete it and this target stays green; it is
//     what made mainnet 31384697 and spicy 32196267 unimportable, and its only
//     tripwire is TestReplayFixtures.
//   - the deployerProxyHooksActive()-gated hook insertion points in Call and
//     create, including the HasDeploymentHookFix placement selector.
//
// So a green run here after an EVM.Call/create refactor says the wrapper still
// dispatches correctly. It does NOT say the hooks are still wired in. Run the
// replay check (cmd/replaycheck, docs/replaycheck.md) for that.
//
// Within its scope, FuzzUpstreamOracle_PrecompileOrHook pins WHERE the two are
// allowed to differ:
//
//   - Any addr for which systemcontract.IsEvmHook(addr) is false: identical
//     (PrecompiledContract, bool) to upstream, for every fork-rule combination
//     — including HasRuntimeUpgrade / DeployerProxySunset on and off, and both
//     Parlia and non-Parlia configs. This covers the whole precompile range,
//     the BAS system contracts 0x...7001-0x...7006 (which are ordinary code
//     accounts as far as dispatch is concerned) and arbitrary addresses.
//
//   - The single hook address 0x...7f01: Chiliz dispatches to the
//     runtime-upgrade EVM hook (non-nil contract, ok == true) in EVERY rule
//     combination, while upstream never treats it as a precompile (ok == false).
//     The fork gate is NOT at dispatch: it lives inside the hook's Run(), which
//     refuses with an error whenever rules.HasRuntimeUpgrade is false (and
//     whenever the caller is not the RuntimeUpgrade system contract). The test
//     pins both halves — unconditional dispatch, gated execution — because
//     that is the shape every historical block was produced with (re-syncing
//     the pre-fork range must keep dispatching to the hook and keep failing).
//     Hooks also charge zero gas (RequiredGas == 0), which is what makes the
//     RunPrecompiledContract input-copy in contracts.go necessary.
//
// The EVM is built with a nil StateDB: dispatch never touches state, and the
// hook's Run is only invoked on paths that error before any state write.
//
// When an upstream sync legitimately changes (*EVM).precompile, the oracle copy
// must be updated in the same PR and the tag in this comment bumped.

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	csystemcontract "github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core/vm/systemcontract"
	"github.com/ethereum/go-ethereum/params"
)

// upstreamPrecompile — bnb-chain/bsc v1.7.8, core/vm/evm.go, (*EVM).precompile
// with the receiver turned into the first parameter. Do not edit; see the file header.
func upstreamPrecompile(evm *EVM, addr common.Address) (PrecompiledContract, bool) {
	p, ok := evm.precompiles[addr]
	return p, ok
}

// Fork ladder for the oracle EVM: block-based forks every 10 blocks up to 100,
// time-based forks every 100 s up to 1000 s, so a (number, time) sweep visits
// every precompile table in activePrecompiledContracts. The Chiliz forks sit
// mid-ladder so they flip independently of the BSC/Ethereum ones.
const (
	oracleRuntimeUpgradeBlock = 45
	oracleSunsetTime          = 350
	oracleMaxNumber           = 120
	oracleMaxTime             = 1200
)

func oracleEVMChainConfig(parlia, runtimeUpgrade, sunset bool) *params.ChainConfig {
	b := func(v int64) *big.Int { return big.NewInt(v) }
	u := func(v uint64) *uint64 { return &v }
	cfg := &params.ChainConfig{
		ChainID:             b(1),
		HomesteadBlock:      b(0),
		EIP150Block:         b(0),
		EIP155Block:         b(0),
		EIP158Block:         b(0),
		ByzantiumBlock:      b(10),
		ConstantinopleBlock: b(20),
		PetersburgBlock:     b(20),
		IstanbulBlock:       b(30),
		MuirGlacierBlock:    b(30),
		RamanujanBlock:      b(30),
		NielsBlock:          b(30),
		MirrorSyncBlock:     b(30),
		BrunoBlock:          b(30),
		EulerBlock:          b(30),
		NanoBlock:           b(40),
		MoranBlock:          b(50),
		GibbsBlock:          b(50),
		PlanckBlock:         b(60),
		LubanBlock:          b(70),
		PlatoBlock:          b(80),
		BerlinBlock:         b(90),
		LondonBlock:         b(90),
		HertzBlock:          b(100),
		HertzfixBlock:       b(100),
		ShanghaiTime:        u(100),
		KeplerTime:          u(100),
		FeynmanTime:         u(100),
		FeynmanFixTime:      u(100),
		CancunTime:          u(200),
		HaberTime:           u(300),
		HaberFixTime:        u(300),
		BohrTime:            u(400),
		PascalTime:          u(500),
		PragueTime:          u(500),
		LorentzTime:         u(600),
		MaxwellTime:         u(700),
		FermiTime:           u(800),
		OsakaTime:           u(900),
		MendelTime:          u(950),
		PasteurTime:         u(1000),
	}
	if parlia {
		cfg.Parlia = &params.ParliaConfig{Period: 3, Epoch: 200}
	}
	if runtimeUpgrade {
		cfg.RuntimeUpgradeBlock = b(oracleRuntimeUpgradeBlock)
	}
	if sunset {
		cfg.DeployerProxySunsetTime = u(oracleSunsetTime)
	}
	return cfg
}

// oracleAddress shapes a fuzzed address toward the interesting regions.
func oracleAddress(sel uint8, low uint16) common.Address {
	switch sel % 4 {
	case 0: // precompile range 0x00-0x13
		return common.BytesToAddress([]byte{byte(low % 0x14)})
	case 1: // hook neighbourhood 0x...7f00-0x...7f03 (0x7f01 is the hook)
		return common.BytesToAddress([]byte{0x7f, byte(low % 4)})
	case 2: // BAS system contracts 0x...7000-0x...7007
		return common.BytesToAddress([]byte{0x70, byte(low % 8)})
	default: // anything in the low 16 bits (covers 0x64-0x69, 0x100, ...)
		return common.BytesToAddress([]byte{byte(low >> 8), byte(low)})
	}
}

// FuzzUpstreamOracle_PrecompileOrHook pins precompileOrHook to the upstream
// precompile dispatch away from the hook address, and pins the hook contract's
// unconditional dispatch / gated execution at the hook address.
//
// mode bits: 0 = Parlia config, 1 = RuntimeUpgrade fork scheduled, 2 =
// DeployerProxySunset fork scheduled, 3 = sweep number/time along the fork
// ladder (else raw). callerSeed even -> caller is the RuntimeUpgrade system
// contract (the only caller the hook accepts).
func FuzzUpstreamOracle_PrecompileOrHook(f *testing.F) {
	for _, mode := range []uint8{0x00, 0x01, 0x03, 0x07, 0x0f, 0x09, 0x0b, 0x0d} {
		for sel := uint8(0); sel < 4; sel++ {
			f.Add(sel, uint16(0x01), uint64(0), uint64(0), uint64(0), mode, []byte{})
			f.Add(sel, uint16(0x7f01), uint64(oracleRuntimeUpgradeBlock), uint64(oracleSunsetTime), uint64(0x7004), mode, []byte{0x6f, 0xbc, 0x15, 0xe9})
			f.Add(sel, uint16(0x7005), uint64(oracleRuntimeUpgradeBlock-1), uint64(oracleSunsetTime-1), uint64(1), mode, []byte{1, 2, 3})
			f.Add(sel, uint16(0x0100), uint64(oracleMaxNumber-1), uint64(oracleMaxTime-1), uint64(2), mode, []byte{})
			f.Add(sel, uint16(0x0013), uint64(95), uint64(950), uint64(3), mode, []byte{0xff})
		}
	}

	f.Fuzz(func(t *testing.T, sel uint8, low uint16, numberRaw, timeRaw, callerSeed uint64, mode uint8, input []byte) {
		parlia := mode&0x01 != 0
		runtimeUpgrade := mode&0x02 != 0
		sunset := mode&0x04 != 0
		number, time := numberRaw, timeRaw
		if mode&0x08 != 0 {
			number %= oracleMaxNumber
			time %= oracleMaxTime
		}
		cfg := oracleEVMChainConfig(parlia, runtimeUpgrade, sunset)
		evm := NewEVM(BlockContext{BlockNumber: new(big.Int).SetUint64(number), Time: time}, nil, cfg, Config{})

		addr := oracleAddress(sel, low)
		caller := common.BigToAddress(new(big.Int).SetUint64(callerSeed))
		if callerSeed%2 == 0 {
			caller = csystemcontract.RuntimeUpgradeContractAddress
		}

		gotP, gotOK := evm.precompileOrHook(addr, caller)
		wantP, wantOK := upstreamPrecompile(evm, addr)
		ownP, ownOK := evm.precompile(addr)

		if ownOK != wantOK || ownP != wantP {
			t.Fatalf("(*EVM).precompile itself diverged from upstream v1.7.8 for %s (number=%d time=%d)", addr, number, time)
		}

		if !systemcontract.IsEvmHook(addr) {
			if gotOK != wantOK || gotP != wantP {
				t.Fatalf("precompileOrHook diverged from upstream precompile for non-hook %s\n number=%d time=%d parlia=%v runtimeUpgrade=%v sunset=%v\n chiliz=(%T,%v) upstream=(%T,%v)",
					addr, number, time, parlia, runtimeUpgrade, sunset, gotP, gotOK, wantP, wantOK)
			}
			return
		}

		// Hook address: unconditional dispatch to the hook, never a precompile upstream.
		if wantOK {
			t.Fatalf("upstream precompile table now contains the hook address %s — the hook and a precompile collide", addr)
		}
		if !gotOK || gotP == nil {
			t.Fatalf("precompileOrHook did not dispatch %s to the hook (number=%d time=%d runtimeUpgrade=%v sunset=%v)", addr, number, time, runtimeUpgrade, sunset)
		}
		if gas := gotP.RequiredGas(input); gas != 0 {
			t.Fatalf("hook at %s charges %d gas; hooks must be free", addr, gas)
		}
		// Gated execution: inert unless the RuntimeUpgrade fork is active AND
		// the caller is the RuntimeUpgrade system contract. Only those refusing
		// paths are exercised (they return before any state write).
		rules := cfg.Rules(new(big.Int).SetUint64(number), false, time)
		if rules.HasRuntimeUpgrade != (runtimeUpgrade && number >= oracleRuntimeUpgradeBlock) {
			t.Fatalf("HasRuntimeUpgrade=%v does not match the configured fork (runtimeUpgrade=%v number=%d)", rules.HasRuntimeUpgrade, runtimeUpgrade, number)
		}
		if !rules.HasRuntimeUpgrade || caller != csystemcontract.RuntimeUpgradeContractAddress {
			out, err := gotP.Run(input)
			if err == nil || out != nil {
				t.Fatalf("hook at %s executed while inactive (HasRuntimeUpgrade=%v caller=%s): out=%x err=%v", addr, rules.HasRuntimeUpgrade, caller, out, err)
			}
		}
	})
}
