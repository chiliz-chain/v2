package params

import (
	"math"
	"math/big"
	"testing"
)

// Every Chiliz fork exists twice: as a field on ChainConfig (read through an
// Is<Fork>() helper by consensus) and as a flag on Rules (read by the EVM).
// If the two ever disagree for some (number, timestamp), consensus and the EVM
// apply different fork schedules to the same block. FuzzChilizForkRules pins
// the two representations to each other and to the basic shape of a fork
// switch: monotone non-decreasing, and permanently off when unscheduled.
//
// Fork ORDERING (Snake8Fix after Snake8, Dragon8Fix after Dragon8, ...) is NOT
// enforced on arbitrary configs today: CheckConfigForkOrder only knows the BSC
// forks. COR-218 tracks adding the Chiliz forks to it. Until then, ordering is
// only asserted on the shipped network configs (config.TestEmbeddedChilizForkSchedules),
// never on fuzzed configs.

// chilizForkProbe evaluates one fork in both of its representations at a
// (number, timestamp) point.
type chilizForkProbe struct {
	name      string
	scheduled bool
	// helper is the ChainConfig-side view. For time forks it is the exported
	// Is<Fork>() method; the four block forks have no such method, so the
	// reference there is isBlockForked on the field — exactly what Rules() uses.
	helper func(c *ChainConfig, num *big.Int, ts uint64) bool
	// rule is the Rules-side view. nil for Pepper8/Pipe8, which are consensus-only
	// (one-time mints) and deliberately have no Rules flag.
	rule func(Rules) bool
	// blockFork marks the four block-number forks, the only ones for which a nil
	// block number is a meaningful probe.
	blockFork bool
}

func chilizForkProbes(c *ChainConfig) []chilizForkProbe {
	return []chilizForkProbe{
		{
			name: "RuntimeUpgrade", scheduled: c.RuntimeUpgradeBlock != nil,
			helper:    func(c *ChainConfig, num *big.Int, _ uint64) bool { return isBlockForked(c.RuntimeUpgradeBlock, num) },
			rule:      func(r Rules) bool { return r.HasRuntimeUpgrade },
			blockFork: true,
		},
		{
			name: "DeployOrigin", scheduled: c.DeployOriginBlock != nil,
			helper:    func(c *ChainConfig, num *big.Int, _ uint64) bool { return isBlockForked(c.DeployOriginBlock, num) },
			rule:      func(r Rules) bool { return r.HasDeployOrigin },
			blockFork: true,
		},
		{
			name: "DeploymentHookFix", scheduled: c.DeploymentHookFixBlock != nil,
			helper:    func(c *ChainConfig, num *big.Int, _ uint64) bool { return isBlockForked(c.DeploymentHookFixBlock, num) },
			rule:      func(r Rules) bool { return r.HasDeploymentHookFix },
			blockFork: true,
		},
		{
			name: "DeployerFactory", scheduled: c.DeployerFactoryBlock != nil,
			helper:    func(c *ChainConfig, num *big.Int, _ uint64) bool { return isBlockForked(c.DeployerFactoryBlock, num) },
			rule:      func(r Rules) bool { return r.DeployerFactory },
			blockFork: true,
		},
		{
			name: "Dragon8", scheduled: c.Dragon8Time != nil,
			helper: func(c *ChainConfig, _ *big.Int, ts uint64) bool { return c.IsDragon8(ts) },
			rule:   func(r Rules) bool { return r.Dragon8 },
		},
		{
			name: "Dragon8Fix", scheduled: c.Dragon8FixTime != nil,
			helper: func(c *ChainConfig, _ *big.Int, ts uint64) bool { return c.IsDragon8Fix(ts) },
			rule:   func(r Rules) bool { return r.Dragon8Fix },
		},
		{
			name: "Snake8", scheduled: c.Snake8Time != nil,
			helper: func(c *ChainConfig, _ *big.Int, ts uint64) bool { return c.IsSnake8(ts) },
			rule:   func(r Rules) bool { return r.Snake8 },
		},
		{
			name: "Snake8Fix", scheduled: c.Snake8FixTime != nil,
			helper: func(c *ChainConfig, _ *big.Int, ts uint64) bool { return c.IsSnake8Fix(ts) },
			rule:   func(r Rules) bool { return r.Snake8Fix },
		},
		{
			name: "Pepper8", scheduled: c.Pepper8Time != nil,
			helper: func(c *ChainConfig, _ *big.Int, ts uint64) bool { return c.IsPepper8Time(ts) },
		},
		{
			name: "Pipe8", scheduled: c.Pipe8Time != nil,
			helper: func(c *ChainConfig, _ *big.Int, ts uint64) bool { return c.IsPipe8Time(ts) },
		},
		{
			name: "DeployerProxySunset", scheduled: c.DeployerProxySunsetTime != nil,
			helper: func(c *ChainConfig, _ *big.Int, ts uint64) bool { return c.IsDeployerProxySunsetTime(ts) },
			rule:   func(r Rules) bool { return r.DeployerProxySunset },
		},
	}
}

func optBlock(set bool, v uint64) *big.Int {
	if !set {
		return nil
	}
	return new(big.Int).SetUint64(v)
}

func optTime(set bool, v uint64) *uint64 {
	if !set {
		return nil
	}
	return &v
}

func saturatingAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

// newFuzzedChilizConfig builds the minimal Parlia config Rules() can evaluate:
// Parlia non-nil (IsInBSC) and London at genesis (several Rules fields are
// London-gated). Every other BSC fork is left nil so only the Chiliz fields vary.
func newFuzzedChilizConfig(
	ruSet bool, ru uint64, doSet bool, do uint64, dhfSet bool, dhf uint64, dfSet bool, df uint64,
	d8Set bool, d8 uint64, d8fSet bool, d8f uint64, s8Set bool, s8 uint64, s8fSet bool, s8f uint64,
	p8Set bool, p8 uint64, pi8Set bool, pi8 uint64, dpsSet bool, dps uint64,
) *ChainConfig {
	return &ChainConfig{
		ChainID:                 big.NewInt(88888),
		LondonBlock:             big.NewInt(0),
		Parlia:                  &ParliaConfig{Period: 3, Epoch: 200},
		RuntimeUpgradeBlock:     optBlock(ruSet, ru),
		DeployOriginBlock:       optBlock(doSet, do),
		DeploymentHookFixBlock:  optBlock(dhfSet, dhf),
		DeployerFactoryBlock:    optBlock(dfSet, df),
		Dragon8Time:             optTime(d8Set, d8),
		Dragon8FixTime:          optTime(d8fSet, d8f),
		Snake8Time:              optTime(s8Set, s8),
		Snake8FixTime:           optTime(s8fSet, s8f),
		Pepper8Time:             optTime(p8Set, p8),
		Pipe8Time:               optTime(pi8Set, pi8),
		DeployerProxySunsetTime: optTime(dpsSet, dps),
	}
}

func FuzzChilizForkRules(f *testing.F) {
	// All forks unscheduled, genesis.
	f.Add(false, uint64(0), false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(0), false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(0), uint64(0), uint64(0), uint64(0))
	// Everything at genesis, probe at genesis and far ahead.
	f.Add(true, uint64(0), true, uint64(0), true, uint64(0), true, uint64(0),
		true, uint64(0), true, uint64(0), true, uint64(0), true, uint64(0),
		true, uint64(0), true, uint64(0), true, uint64(0),
		false, uint64(0), uint64(0), uint64(1<<40), uint64(1<<40))
	// Mainnet-shaped schedule (Dragon8Time absent, Dragon8Fix present — see
	// config.TestEmbeddedChilizForkSchedules), probe one second before Snake8.
	f.Add(true, uint64(0), true, uint64(0), true, uint64(0), true, uint64(5765560),
		false, uint64(0), true, uint64(1718611200), true, uint64(1760432400), false, uint64(0),
		true, uint64(1757410200), false, uint64(0), false, uint64(0),
		false, uint64(5765560), uint64(1760432399), uint64(1), uint64(1))
	// Exact activation boundaries for a block and a time fork.
	f.Add(true, uint64(100), false, uint64(0), false, uint64(0), false, uint64(0),
		true, uint64(1000), false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(0), false, uint64(0), true, uint64(2000),
		false, uint64(100), uint64(1000), uint64(0), uint64(1000))
	// nil block number at the first probe point.
	f.Add(true, uint64(0), true, uint64(0), true, uint64(0), true, uint64(0),
		false, uint64(0), false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(0), false, uint64(0), false, uint64(0),
		true, uint64(0), uint64(0), uint64(7), uint64(7))
	// The four block forks at four distinct heights, probed low and high. Every
	// other seed gives RuntimeUpgrade/DeployOrigin/DeploymentHookFix the same
	// height (0), so a Rules() line that reads the wrong one of those three
	// fields is invisible to a plain `go test` run of the corpus — only `-fuzz`
	// finds it. These two seeds give each of the four a distinct pair of
	// activation states across the two probe points (10/20/30/40 seen from 15..25
	// and from 35..45), so any swap among them fails at least one seed.
	f.Add(true, uint64(10), true, uint64(20), true, uint64(30), true, uint64(40),
		false, uint64(0), false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(15), uint64(0), uint64(10), uint64(0))
	f.Add(true, uint64(10), true, uint64(20), true, uint64(30), true, uint64(40),
		false, uint64(0), false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(35), uint64(0), uint64(10), uint64(0))
	// Saturation at the top of the range.
	f.Add(true, uint64(math.MaxUint64), false, uint64(0), false, uint64(0), false, uint64(0),
		true, uint64(math.MaxUint64), false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(0), false, uint64(0), false, uint64(0),
		false, uint64(math.MaxUint64-1), uint64(math.MaxUint64-1), uint64(5), uint64(5))

	f.Fuzz(func(t *testing.T,
		ruSet bool, ru uint64, doSet bool, do uint64, dhfSet bool, dhf uint64, dfSet bool, df uint64,
		d8Set bool, d8 uint64, d8fSet bool, d8f uint64, s8Set bool, s8 uint64, s8fSet bool, s8f uint64,
		p8Set bool, p8 uint64, pi8Set bool, pi8 uint64, dpsSet bool, dps uint64,
		numNil bool, num uint64, ts uint64, dnum uint64, dts uint64,
	) {
		cfg := newFuzzedChilizConfig(
			ruSet, ru, doSet, do, dhfSet, dhf, dfSet, df,
			d8Set, d8, d8fSet, d8f, s8Set, s8, s8fSet, s8f,
			p8Set, p8, pi8Set, pi8, dpsSet, dps,
		)

		// Two probe points with (num2, ts2) >= (num, ts). A nil block number
		// (Rules callers do pass one for header-less contexts) sits below every
		// block height, so it is only ever used at the first point.
		num1 := optBlock(!numNil, num)
		num2 := new(big.Int).SetUint64(saturatingAdd(num, dnum))
		if numNil {
			num2 = new(big.Int).SetUint64(dnum)
		}
		ts2 := saturatingAdd(ts, dts)

		rules1 := cfg.Rules(num1, false, ts)
		rules2 := cfg.Rules(num2, false, ts2)

		for _, fk := range chilizForkProbes(cfg) {
			h1 := fk.helper(cfg, num1, ts)
			h2 := fk.helper(cfg, num2, ts2)

			// 1. Rules <=> helpers, at both points.
			if fk.rule != nil {
				if r1 := fk.rule(rules1); r1 != h1 {
					t.Fatalf("%s: Rules(%v, %d)=%v but helper=%v (config %+v)", fk.name, num1, ts, r1, h1, chilizFields(cfg))
				}
				if r2 := fk.rule(rules2); r2 != h2 {
					t.Fatalf("%s: Rules(%v, %d)=%v but helper=%v (config %+v)", fk.name, num2, ts2, r2, h2, chilizFields(cfg))
				}
			}

			// 2. Monotone non-decreasing: once active, a fork never switches off.
			if h1 && !h2 {
				t.Fatalf("%s: active at (%v, %d) but inactive at later (%v, %d) (config %+v)", fk.name, num1, ts, num2, ts2, chilizFields(cfg))
			}

			// 3. Nil means never.
			if !fk.scheduled && (h1 || h2) {
				t.Fatalf("%s: unscheduled fork reported active at (%v, %d)=%v / (%v, %d)=%v", fk.name, num1, ts, h1, num2, ts2, h2)
			}
		}

		// A nil block number sits below every scheduled height.
		if numNil {
			for _, fk := range chilizForkProbes(cfg) {
				if !fk.blockFork {
					continue
				}
				if fk.helper(cfg, nil, ts) {
					t.Fatalf("%s: block fork active for a nil block number", fk.name)
				}
			}
		}
	})
}

// chilizFields renders only the Chiliz fork schedule of a config, keeping
// failure messages readable (ChainConfig.String() prints every BSC fork too).
func chilizFields(c *ChainConfig) map[string]any {
	return map[string]any{
		"RuntimeUpgradeBlock":     c.RuntimeUpgradeBlock,
		"DeployOriginBlock":       c.DeployOriginBlock,
		"DeploymentHookFixBlock":  c.DeploymentHookFixBlock,
		"DeployerFactoryBlock":    c.DeployerFactoryBlock,
		"Dragon8Time":             derefTime(c.Dragon8Time),
		"Dragon8FixTime":          derefTime(c.Dragon8FixTime),
		"Snake8Time":              derefTime(c.Snake8Time),
		"Snake8FixTime":           derefTime(c.Snake8FixTime),
		"Pepper8Time":             derefTime(c.Pepper8Time),
		"Pipe8Time":               derefTime(c.Pipe8Time),
		"DeployerProxySunsetTime": derefTime(c.DeployerProxySunsetTime),
	}
}

func derefTime(t *uint64) any {
	if t == nil {
		return nil
	}
	return *t
}
