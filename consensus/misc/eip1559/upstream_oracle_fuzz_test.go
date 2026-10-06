package eip1559

// COR-210: differential oracle against upstream BSC.
//
// upstreamCalcBaseFee below is a verbatim copy of CalcBaseFee from
// bnb-chain/bsc tag v1.7.8, consensus/misc/eip1559/eip1559.go (the last merged
// upstream tag, see .github/bsc-sync/last-synced-tag). It is the oracle for
// FuzzUpstreamOracle_CalcBaseFee, which pins WHERE Chiliz is allowed to
// differ (CLAUDE.md §2 "EIP-1559 base fee floor" and §3):
//
//   - config.Parlia == nil: the two functions must agree exactly. (Both are
//     compiled against the Chiliz params package, so the shared constants
//     InitialBaseFee / DefaultBaseFeeChangeDenominator / ElasticityMultiplier
//     cancel out — the oracle pins the function body, not the constants.)
//
//   - config.Parlia != nil: upstream v1.7.8 short-circuits to a CONSTANT
//     params.InitialBaseFeeForBSC for every block (BSC runs a zero base fee).
//     Chiliz instead runs the regular EIP-1559 formula with a floor:
//       * parent is the last pre-London block  -> InitialBaseFeeForBSC;
//       * parent.GasUsed >= target             -> exactly the upstream formula
//         (the result of the oracle evaluated on the same config with
//         Parlia cleared);
//       * parent.GasUsed <  target             -> max(that formula, InitialBaseFeeForBSC).
//     The floor is therefore only applied on the decrease branch; the fuzz
//     target pins that shape exactly rather than a looser "max(upstream,
//     floor)". A corollary that is also asserted: once parent.BaseFee is at or
//     above the floor, the child base fee never falls below it.
//
// When an upstream sync legitimately changes CalcBaseFee, the oracle copy must
// be updated in the same PR and the tag in this comment bumped.

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// upstreamCalcBaseFee — bnb-chain/bsc v1.7.8, consensus/misc/eip1559/eip1559.go.
// Do not edit; see the file header.
func upstreamCalcBaseFee(config *params.ChainConfig, parent *types.Header) *big.Int {
	if config.IsInBSC() {
		return new(big.Int).SetUint64(params.InitialBaseFeeForBSC)
	}

	// If the current block is the first EIP-1559 block, return the InitialBaseFee.
	if !config.IsLondon(parent.Number) {
		return new(big.Int).SetUint64(params.InitialBaseFee)
	}

	parentGasTarget := parent.GasLimit / config.ElasticityMultiplier()
	// If the parent gasUsed is the same as the target, the baseFee remains unchanged.
	if parent.GasUsed == parentGasTarget {
		return new(big.Int).Set(parent.BaseFee)
	}

	var (
		num   = new(big.Int)
		denom = new(big.Int)
	)

	if parent.GasUsed > parentGasTarget {
		// If the parent block used more gas than its target, the baseFee should increase.
		// max(1, parentBaseFee * gasUsedDelta / parentGasTarget / baseFeeChangeDenominator)
		num.SetUint64(parent.GasUsed - parentGasTarget)
		num.Mul(num, parent.BaseFee)
		num.Div(num, denom.SetUint64(parentGasTarget))
		num.Div(num, denom.SetUint64(config.BaseFeeChangeDenominator()))
		if num.Cmp(common.Big1) < 0 {
			return num.Add(parent.BaseFee, common.Big1)
		}
		return num.Add(parent.BaseFee, num)
	} else {
		// Otherwise if the parent block used less gas than its target, the baseFee should decrease.
		// max(0, parentBaseFee * gasUsedDelta / parentGasTarget / baseFeeChangeDenominator)
		num.SetUint64(parentGasTarget - parent.GasUsed)
		num.Mul(num, parent.BaseFee)
		num.Div(num, denom.SetUint64(parentGasTarget))
		num.Div(num, denom.SetUint64(config.BaseFeeChangeDenominator()))

		baseFee := num.Sub(parent.BaseFee, num)
		if baseFee.Cmp(common.Big0) < 0 {
			baseFee = common.Big0
		}
		return baseFee
	}
}

const oracleLondonBlock = 5

func oracleBaseFeeConfig(parlia bool) *params.ChainConfig {
	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(1),
		LondonBlock: big.NewInt(oracleLondonBlock),
	}
	if parlia {
		cfg.Parlia = &params.ParliaConfig{}
	}
	return cfg
}

// FuzzUpstreamOracle_CalcBaseFee pins CalcBaseFee to the upstream oracle.
//
// Inputs are raw fuzz values shaped by mode bits toward the interesting
// regions: gasUsed around the gas target, baseFee around the Parlia floor,
// number around the London block. mode bit 7 selects a Parlia config.
func FuzzUpstreamOracle_CalcBaseFee(f *testing.F) {
	const floor = params.InitialBaseFeeForBSC
	for _, mode := range []uint8{0x00, 0x15, 0x2a, 0x3f, 0x80, 0x95, 0xaa, 0xbf, 0x84, 0x88, 0x8c} {
		f.Add(uint64(30_000_000), uint64(15_000_000), uint64(floor), uint64(oracleLondonBlock), mode)
		f.Add(uint64(30_000_000), uint64(0), uint64(floor), uint64(oracleLondonBlock+1), mode)
		f.Add(uint64(30_000_000), uint64(30_000_000), uint64(floor), uint64(oracleLondonBlock+100), mode)
		f.Add(uint64(30_000_000), uint64(1), uint64(floor-1), uint64(oracleLondonBlock+7), mode)
		f.Add(uint64(30_000_000), uint64(15_000_000), uint64(floor-1), uint64(oracleLondonBlock+7), mode)
		f.Add(uint64(30_000_000), uint64(29_999_999), uint64(1), uint64(oracleLondonBlock-1), mode)
		f.Add(uint64(5000), uint64(2500), uint64(params.InitialBaseFee), uint64(1), mode)
		f.Add(uint64(1), uint64(0), uint64(0), uint64(0), mode)
	}

	f.Fuzz(func(t *testing.T, gasLimit, gasUsedRaw, baseFeeRaw, numberRaw uint64, mode uint8) {
		parlia := mode&0x80 != 0
		cfg := oracleBaseFeeConfig(parlia)

		target := gasLimit / cfg.ElasticityMultiplier()

		var gasUsed uint64
		switch mode & 0x03 {
		case 0:
			gasUsed = gasUsedRaw
			if gasLimit < math.MaxUint64 {
				gasUsed %= gasLimit + 1
			}
		case 1:
			gasUsed = target
		case 2:
			gasUsed = min(target+gasUsedRaw%64, gasLimit)
		case 3:
			gasUsed = target - min(target, gasUsedRaw%64)
		}

		var baseFee uint64
		switch (mode >> 2) & 0x03 {
		case 0:
			baseFee = baseFeeRaw
		case 1:
			baseFee = floor
		case 2:
			baseFee = floor + baseFeeRaw%1_000_000
		case 3:
			baseFee = floor - baseFeeRaw%1_000_000
		}

		var number uint64
		switch (mode >> 4) & 0x03 {
		case 0:
			number = numberRaw
		case 1:
			number = oracleLondonBlock - 1
		case 2:
			number = oracleLondonBlock
		case 3:
			number = oracleLondonBlock + numberRaw%3
		}

		parent := &types.Header{
			Number:   new(big.Int).SetUint64(number),
			GasLimit: gasLimit,
			GasUsed:  gasUsed,
			BaseFee:  new(big.Int).SetUint64(baseFee),
		}

		// Domain: exactly the inputs on which the shared EIP-1559 arithmetic
		// divides by a zero gas target — post-London, with gasUsed off target.
		// A zero target is unreachable on-chain (gas limit >= params.MinGasLimit)
		// and would panic identically on both sides on a non-Parlia config, but
		// on Parlia only Chiliz reaches the division at all, so the panic cannot
		// be compared. Pre-London neither side divides and gasUsed == target
		// returns early, so those inputs stay in scope: that is where the
		// InitialBaseFee / InitialBaseFeeForBSC split lives.
		if target == 0 && gasUsed != target && cfg.IsLondon(parent.Number) {
			return
		}

		got := CalcBaseFee(cfg, parent)
		upstream := upstreamCalcBaseFee(cfg, parent)

		if !parlia {
			if got.Cmp(upstream) != 0 {
				t.Fatalf("non-Parlia CalcBaseFee diverged from upstream v1.7.8\n parent number=%d gasLimit=%d gasUsed=%d baseFee=%d\n chiliz=%s upstream=%s",
					number, gasLimit, gasUsed, baseFee, got, upstream)
			}
			return
		}

		// Parlia: pin the oracle's own shape first, so a future upstream change
		// to the BSC branch is noticed even though Chiliz does not follow it.
		if upstream.Cmp(new(big.Int).SetUint64(floor)) != 0 {
			t.Fatalf("upstream oracle no longer returns the constant InitialBaseFeeForBSC on Parlia: got %s — re-derive the allowed-difference relation", upstream)
		}

		floorBig := new(big.Int).SetUint64(floor)
		var want *big.Int
		switch {
		case !cfg.IsLondon(parent.Number):
			want = floorBig
		default:
			noParlia := *cfg
			noParlia.Parlia = nil
			want = upstreamCalcBaseFee(&noParlia, parent)
			if gasUsed < target && want.Cmp(floorBig) < 0 {
				want = floorBig
			}
		}
		if got.Cmp(want) != 0 {
			t.Fatalf("Parlia CalcBaseFee outside the allowed difference from upstream v1.7.8\n parent number=%d gasLimit=%d gasUsed=%d (target %d) baseFee=%d\n chiliz=%s want=%s",
				number, gasLimit, gasUsed, target, baseFee, got, want)
		}
		// Corollary: the floor is sticky once reached.
		if cfg.IsLondon(parent.Number) && baseFee >= floor && got.Cmp(floorBig) < 0 {
			t.Fatalf("Parlia CalcBaseFee fell below the floor from a parent at/above it: parent baseFee=%d child=%s", baseFee, got)
		}
	})
}
