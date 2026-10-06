package config

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// TestEmbeddedChilizForkSchedules checks the three shipped network configs for
// the fork-ordering relations that CheckConfigForkOrder does not enforce on
// Chiliz forks (COR-218 tracks adding them). It lives here rather than next to
// FuzzChilizForkRules in params because config imports core, which imports
// params — loading the embedded genesis from params would be an import cycle.
//
// Snake8Fix ⇒ Snake8 must hold everywhere: post-Snake8Fix verification
// recomputes bytes that only exist in Snake8-format headers.
//
// Dragon8Fix ⇒ Dragon8 holds wherever Dragon8 is scheduled (spicy replays a
// dragon8Time → dragon8FixTime window). Mainnet is the deliberate exception:
// it schedules dragon8FixTime with dragon8Time absent (CLAUDE.md section 2,
// COR-184), because distributeIncoming branches on IsDragon8Fix first and the
// pre-fix schedule never ran there. That exception is pinned to chain 88888
// only, so it cannot spread to another network unnoticed.
func TestEmbeddedChilizForkSchedules(t *testing.T) {
	networks := []struct {
		name    string
		chainID int64
		cfg     *params.ChainConfig
		// dragon8FixWithoutDragon8 documents the mainnet exception described above.
		dragon8FixWithoutDragon8 bool
	}{
		{"chiliz", 88888, ChilizMainnetGenesisConfig.Config, true},
		{"spicy", 88882, SpicyGenesisConfig.Config, false},
		{"scoville", 88880, ScovilleGenesisConfig.Config, false},
	}

	for _, n := range networks {
		t.Run(n.name, func(t *testing.T) {
			cfg := n.cfg
			if cfg == nil || cfg.ChainID == nil || cfg.ChainID.Int64() != n.chainID {
				t.Fatalf("embedded config chain id = %v, want %d", cfg.ChainID, n.chainID)
			}
			if cfg.Parlia == nil {
				t.Fatalf("embedded config has no Parlia section")
			}
			if err := cfg.CheckConfigForkOrder(); err != nil {
				t.Fatalf("CheckConfigForkOrder: %v", err)
			}

			hasDragon8FixOnly := cfg.Dragon8FixTime != nil && cfg.Dragon8Time == nil
			if hasDragon8FixOnly != n.dragon8FixWithoutDragon8 {
				t.Fatalf("dragon8FixTime-without-dragon8Time = %v, want %v (dragon8Time=%v dragon8FixTime=%v)",
					hasDragon8FixOnly, n.dragon8FixWithoutDragon8, deref(cfg.Dragon8Time), deref(cfg.Dragon8FixTime))
			}
			if cfg.Snake8FixTime != nil && cfg.Snake8Time == nil {
				t.Fatalf("snake8FixTime=%d scheduled without snake8Time", *cfg.Snake8FixTime)
			}

			// Probe every scheduled fork time and its neighbours. Block forks
			// are irrelevant to the time-fork implications, so the block number
			// is pinned far past every scheduled height.
			num := new(big.Int).SetUint64(1 << 40)
			for _, ts := range probeTimestamps(cfg) {
				rules := cfg.Rules(num, false, ts)

				if rules.Snake8Fix && !rules.Snake8 {
					t.Fatalf("t=%d: Snake8Fix active without Snake8", ts)
				}
				if rules.Snake8Fix != cfg.IsSnake8Fix(ts) || rules.Snake8 != cfg.IsSnake8(ts) {
					t.Fatalf("t=%d: Snake8 rules/helpers disagree (rules %v/%v, helpers %v/%v)",
						ts, rules.Snake8, rules.Snake8Fix, cfg.IsSnake8(ts), cfg.IsSnake8Fix(ts))
				}
				if rules.Dragon8Fix != cfg.IsDragon8Fix(ts) || rules.Dragon8 != cfg.IsDragon8(ts) {
					t.Fatalf("t=%d: Dragon8 rules/helpers disagree (rules %v/%v, helpers %v/%v)",
						ts, rules.Dragon8, rules.Dragon8Fix, cfg.IsDragon8(ts), cfg.IsDragon8Fix(ts))
				}
				if !n.dragon8FixWithoutDragon8 && rules.Dragon8Fix && !rules.Dragon8 {
					t.Fatalf("t=%d: Dragon8Fix active without Dragon8", ts)
				}
				if n.dragon8FixWithoutDragon8 && rules.Dragon8 {
					t.Fatalf("t=%d: Dragon8 active on a network that schedules only Dragon8Fix", ts)
				}
			}
		})
	}
}

// probeTimestamps returns every scheduled Chiliz fork time and its ±1 neighbours.
func probeTimestamps(cfg *params.ChainConfig) []uint64 {
	var out []uint64
	for _, ft := range []*uint64{
		cfg.Dragon8Time, cfg.Dragon8FixTime, cfg.Snake8Time, cfg.Snake8FixTime,
		cfg.Pepper8Time, cfg.Pipe8Time, cfg.DeployerProxySunsetTime,
	} {
		if ft == nil {
			continue
		}
		if *ft > 0 {
			out = append(out, *ft-1)
		}
		out = append(out, *ft, *ft+1)
	}
	return out
}

func deref(t *uint64) any {
	if t == nil {
		return nil
	}
	return *t
}
