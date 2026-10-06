package vm

// COR-217: evm.depth is unexported, so the direct "depth is restored around a
// hook call" check lives here, next to the hooks. FuzzVmRuntimeWithHooks
// (core/vm/runtime) pins the same property for arbitrary code through the
// tracer's enter/exit depth pairing.

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// hookTestChainConfig is ParliaTestChainConfig with the RuntimeUpgrade fork
// live, so deployerProxyHooksActive() is true.
func hookTestChainConfig() *params.ChainConfig {
	cfg := *params.ParliaTestChainConfig
	cfg.RuntimeUpgradeBlock = big.NewInt(0)
	return &cfg
}

// newHookTestEVM builds an EVM over a fresh state with proxyCode installed as
// the DeployerProxy and a funded origin.
func newHookTestEVM(t *testing.T, proxyCode []byte, origin common.Address) (*EVM, *state.StateDB) {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	statedb.SetCode(systemcontract.DeployerProxyContractAddress, proxyCode, tracing.CodeChangeUnspecified)
	statedb.CreateAccount(origin)
	statedb.SetBalance(origin, uint256.NewInt(1_000_000), tracing.BalanceChangeUnspecified)
	ctx := BlockContext{
		CanTransfer: func(db StateDB, addr common.Address, amount *uint256.Int) bool {
			return db.GetBalance(addr).Cmp(amount) >= 0
		},
		Transfer: func(db StateDB, sender, recipient common.Address, amount *uint256.Int) {
			db.SubBalance(sender, amount, tracing.BalanceChangeTransfer)
			db.AddBalance(recipient, amount, tracing.BalanceChangeTransfer)
		},
		BlockNumber: new(big.Int),
		Time:        1,
		Coinbase:    common.HexToAddress("0xc01bba5e"),
		GasLimit:    10_000_000,
		BaseFee:     new(big.Int),
		BlobBaseFee: new(big.Int),
		Random:      &common.Hash{},
	}
	evm := NewEVM(ctx, statedb, hookTestChainConfig(), Config{})
	evm.SetTxContext(TxContext{Origin: origin, GasPrice: new(big.Int)})
	return evm, statedb
}

// TestHookDepthRestored checks evm.depth is back to its pre-call value after
// every hook outcome — accept, revert, out of gas — on the Call, Create and
// nested-CREATE paths, and around the hook functions themselves.
func TestHookDepthRestored(t *testing.T) {
	var (
		origin   = common.HexToAddress("0xf00d")
		contract = common.HexToAddress("0xc0de")
		factory  = common.HexToAddress("0xfac7")
		// The callee: one STOP. The factory: CREATE with empty init code, then STOP.
		stopCode    = []byte{byte(STOP)}
		factoryCode = []byte{byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(CREATE), byte(STOP)}
	)
	proxies := map[string]struct {
		code    []byte
		wantErr error
	}{
		"accept":     {[]byte{byte(STOP)}, nil},
		"revert":     {[]byte{byte(PUSH1), 0x00, byte(PUSH1), 0x00, byte(REVERT)}, ErrNotAllowed},
		"out of gas": {[]byte{byte(JUMPDEST), byte(PUSH1), 0x00, byte(JUMP)}, ErrNotAllowed},
	}
	for name, proxy := range proxies {
		t.Run(name, func(t *testing.T) {
			evm, statedb := newHookTestEVM(t, proxy.code, origin)
			statedb.SetCode(contract, stopCode, tracing.CodeChangeUnspecified)
			statedb.SetCode(factory, factoryCode, tracing.CodeChangeUnspecified)
			check := func(path string, err error) {
				t.Helper()
				if !errors.Is(err, proxy.wantErr) {
					t.Fatalf("%s: err=%v, want %v", path, err, proxy.wantErr)
				}
				if evm.depth != 0 {
					t.Fatalf("%s: evm.depth=%d after the call, want 0", path, evm.depth)
				}
			}

			_, _, err := evm.Call(origin, contract, nil, 1_000_000, new(uint256.Int))
			check("Call", err)

			_, _, _, err = evm.Create(origin, stopCode, 1_000_000, new(uint256.Int))
			check("Create", err)

			_, _, err = evm.Call(origin, factory, nil, 1_000_000, new(uint256.Int))
			check("Call(factory)", err)

			// The hook functions on their own, from a non-zero depth.
			evm.depth = 3
			_, err = applyChilizInvocationEvmHook(evm, contract, 1_000_000)
			if !errors.Is(err, proxy.wantErr) || evm.depth != 3 {
				t.Fatalf("applyChilizInvocationEvmHook: err=%v depth=%d, want %v / 3", err, evm.depth, proxy.wantErr)
			}
			_, err = applyChilizDeploymentEvmHook(evm, origin, contract, 1_000_000)
			if !errors.Is(err, proxy.wantErr) || evm.depth != 3 {
				t.Fatalf("applyChilizDeploymentEvmHook: err=%v depth=%d, want %v / 3", err, evm.depth, proxy.wantErr)
			}
			evm.depth = 0

			// A second root call on the same EVM must still start at depth 0:
			// the hook frame is recorded one level below the guarded call.
			var depths []int
			evm.Config.Tracer = &tracing.Hooks{
				OnEnter: func(depth int, _ byte, _ common.Address, to common.Address, _ []byte, _ uint64, _ *big.Int) {
					if to == systemcontract.DeployerProxyContractAddress {
						depths = append(depths, depth)
					}
				},
			}
			_, _, err = evm.Call(origin, contract, nil, 1_000_000, new(uint256.Int))
			check("Call (traced)", err)
			if len(depths) != 1 || depths[0] != 1 {
				t.Fatalf("hook frame depths %v, want [1]", depths)
			}
		})
	}
}

// TestHookInactiveOutsideWindow pins deployerProxyHooksActive(): off before
// RuntimeUpgrade, on inside the RuntimeUpgrade -> DeployerProxySunset window,
// off again after the sunset — and the hook code path is still compiled in and
// reachable, which replaying the historical window depends on.
func TestHookInactiveOutsideWindow(t *testing.T) {
	zero := uint64(0)
	for _, tc := range []struct {
		name   string
		mutate func(*params.ChainConfig)
		want   bool
	}{
		{"before RuntimeUpgrade", func(c *params.ChainConfig) { c.RuntimeUpgradeBlock = nil }, false},
		{"in window", func(*params.ChainConfig) {}, true},
		{"after sunset", func(c *params.ChainConfig) { c.DeployerProxySunsetTime = &zero }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := hookTestChainConfig()
			tc.mutate(cfg)
			evm := NewEVM(BlockContext{BlockNumber: new(big.Int), Time: 1, Random: &common.Hash{}}, nil, cfg, Config{})
			if got := evm.deployerProxyHooksActive(); got != tc.want {
				t.Fatalf("deployerProxyHooksActive()=%v, want %v", got, tc.want)
			}
		})
	}
}
