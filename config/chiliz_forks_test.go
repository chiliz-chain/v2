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
//
// Snake8Fix and DeployerProxySunset must be scheduled more than two block
// periods apart, so that at normal block cadence the two activations do not
// share a boundary window (COR-225). This is a scheduling rule, not a runtime
// guarantee: after a halt spanning both timestamps, the first resumed block is
// stamped with wall-clock time and the two can still activate on consecutive
// blocks.
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
			// Snake8Fix is gated on parent.Time and DeployerProxySunset on the
			// block's own time, so a shared timestamp activates them one block
			// apart and puts two changes in a two-block boundary window
			// (COR-225). At normal cadence, timestamps within a couple of block
			// periods of each other collapse into the same window, so require
			// more than two periods between them, in either order. A halt that
			// spans both timestamps defeats any gap; that is accepted.
			if cfg.Snake8FixTime != nil && cfg.DeployerProxySunsetTime != nil {
				a, b := *cfg.Snake8FixTime, *cfg.DeployerProxySunsetTime
				gap := max(a, b) - min(a, b)
				if gap <= 2*cfg.Parlia.Period {
					t.Fatalf("snake8FixTime=%d and deployerProxySunsetTime=%d are %ds apart, want more than %ds (two block periods)",
						a, b, gap, 2*cfg.Parlia.Period)
				}
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

// TestEmbeddedRolloutForkTimes pins the activation timestamps of the forks that
// are being rolled out network by network (COR-198 Snake8Fix, COR-225
// DeployerProxySunset). The expected values are golden literals taken from the
// rollout tickets, not read back from the embedded files, so an accidental edit
// or a merge that drops or moves a key fails here instead of shipping a
// different activation time to the fleet. nil means "not scheduled".
func TestEmbeddedRolloutForkTimes(t *testing.T) {
	u64 := func(v uint64) *uint64 { return &v }
	networks := []struct {
		name                    string
		cfg                     *params.ChainConfig
		snake8FixTime           *uint64
		deployerProxySunsetTime *uint64
	}{
		{"chiliz", ChilizMainnetGenesisConfig.Config, nil, nil},
		// COR-227 sunset: Thu 2026-10-08 08:00 UTC; COR-200 Snake8Fix two
		// hours later, 10:00 UTC.
		{"spicy", SpicyGenesisConfig.Config, u64(1791453600), u64(1791446400)},
		{"scoville", ScovilleGenesisConfig.Config, nil, nil},
	}
	for _, n := range networks {
		t.Run(n.name, func(t *testing.T) {
			for _, f := range []struct {
				key       string
				got, want *uint64
			}{
				{"snake8FixTime", n.cfg.Snake8FixTime, n.snake8FixTime},
				{"deployerProxySunsetTime", n.cfg.DeployerProxySunsetTime, n.deployerProxySunsetTime},
			} {
				if (f.got == nil) != (f.want == nil) || (f.got != nil && *f.got != *f.want) {
					t.Errorf("%s = %v, want %v", f.key, deref(f.got), deref(f.want))
				}
			}
		})
	}
}
