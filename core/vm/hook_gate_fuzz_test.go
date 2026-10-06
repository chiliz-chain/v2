package vm

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// deployerProxyHooksActive is the single predicate gating the DeployerProxy
// EVM hooks (CLAUDE.md section 1): on from the RuntimeUpgrade fork, off again
// from the DeployerProxySunset fork. The hooks themselves must stay in the
// code path forever (historical blocks replay through them), so the gate is
// the only thing that turns them off for new blocks.
//
// FuzzDeployerProxyHooksActive pins the gate to the Rules the EVM was built
// with — the same Rules consensus derives for the block — for every schedule
// shape (either fork nil or scheduled) and every (number, time).
func FuzzDeployerProxyHooksActive(f *testing.F) {
	// Neither fork scheduled.
	f.Add(false, uint64(0), false, uint64(0), uint64(0), uint64(0))
	// RuntimeUpgrade at genesis, no sunset: the shape mainnet and spicy run today.
	f.Add(true, uint64(0), false, uint64(0), uint64(35890218), uint64(1760432400))
	// Exact activation boundaries on both sides.
	f.Add(true, uint64(100), true, uint64(1000), uint64(100), uint64(999))
	f.Add(true, uint64(100), true, uint64(1000), uint64(100), uint64(1000))
	f.Add(true, uint64(100), true, uint64(1000), uint64(99), uint64(1000))
	// Sunset scheduled but RuntimeUpgrade not: hooks never fire.
	f.Add(false, uint64(0), true, uint64(0), uint64(1<<40), uint64(1<<40))
	// Top of the range.
	f.Add(true, uint64(math.MaxUint64), true, uint64(math.MaxUint64), uint64(math.MaxUint64), uint64(math.MaxUint64))

	f.Fuzz(func(t *testing.T, ruSet bool, ru uint64, sunsetSet bool, sunset uint64, number uint64, time uint64) {
		cfg := &params.ChainConfig{
			ChainID:     big.NewInt(88888),
			LondonBlock: big.NewInt(0),
			Parlia:      &params.ParliaConfig{Period: 3, Epoch: 200},
		}
		if ruSet {
			cfg.RuntimeUpgradeBlock = new(big.Int).SetUint64(ru)
		}
		if sunsetSet {
			cfg.DeployerProxySunsetTime = &sunset
		}

		blockNumber := new(big.Int).SetUint64(number)
		evm := NewEVM(BlockContext{BlockNumber: blockNumber, Time: time}, nil, cfg, Config{})
		rules := cfg.Rules(blockNumber, false, time)

		got := evm.deployerProxyHooksActive()
		want := rules.HasRuntimeUpgrade && !rules.DeployerProxySunset
		if got != want {
			t.Fatalf("deployerProxyHooksActive()=%v, want %v (HasRuntimeUpgrade=%v DeployerProxySunset=%v; RuntimeUpgradeBlock=%v DeployerProxySunsetTime=%v at number=%d time=%d)",
				got, want, rules.HasRuntimeUpgrade, rules.DeployerProxySunset, cfg.RuntimeUpgradeBlock, derefTime(cfg.DeployerProxySunsetTime), number, time)
		}
		if !ruSet && got {
			t.Fatalf("hooks active with RuntimeUpgradeBlock nil (number=%d time=%d)", number, time)
		}
		if sunsetSet && time >= sunset && got {
			t.Fatalf("hooks active past DeployerProxySunsetTime=%d at time=%d", sunset, time)
		}
	})
}

// derefTime renders an optional fork time as its value rather than as the
// pointer address %v would print for a *uint64.
func derefTime(t *uint64) any {
	if t == nil {
		return nil
	}
	return *t
}
