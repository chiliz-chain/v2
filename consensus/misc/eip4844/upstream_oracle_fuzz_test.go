package eip4844

// COR-210: differential oracle against upstream BSC.
//
// The upstream* declarations below are copies of the blob-fee machinery from
// bnb-chain/bsc tag v1.7.8, consensus/misc/eip4844/eip4844.go (the last merged
// upstream tag, see .github/bsc-sync/last-synced-tag): minBlobGasPrice,
// BlobConfig (+ maxBlobGas/blobBaseFee/blobPrice), latestBlobConfig,
// CalcExcessBlobGas, calcExcessBlobGas, CalcBlobFee and fakeExponential. The
// bodies are verbatim; the only edits are the `upstream` identifier prefix so
// the copies can live next to the Chiliz originals in the same package.
//
// They are the oracle for FuzzUpstreamOracle_BlobFee, which pins WHERE Chiliz
// is allowed to differ (CLAUDE.md §3 "Gas & fee rules"). Chiliz's only change
// to this file is MinBlobGasprice: the blob-fee floor is
// params.BlobTxMinBlobGaspriceForBSC (150 gwei) on Parlia configs and the
// EIP-4844 minimum (1 wei, == upstream's constant) elsewhere. Hence:
//
//   - CalcExcessBlobGas: IDENTICAL to upstream for every config, Parlia or
//     not. The min price only enters excess-gas maths through the EIP-7918
//     reserve-price branch, and that branch is (upstream and Chiliz alike)
//     disabled on Parlia via IsNotInBSC(); on non-Parlia configs the min
//     price is upstream's 1 wei.
//
//   - CalcBlobFee: IDENTICAL to upstream on non-Parlia configs. On Parlia
//     configs it equals the upstream fakeExponential evaluated with the
//     factor swapped for BlobTxMinBlobGaspriceForBSC, i.e.
//       upstreamFakeExponential(150 gwei, excessBlobGas, updateFraction)
//     — NOT upstream * 150e9 (the Taylor loop floors at each step), which is
//     why the relation is pinned through the oracle helper rather than as a
//     multiple. Two weaker corollaries are asserted too: the Parlia fee is
//     never below 150 gwei and never below the upstream fee.
//
// The retention divisors Chiliz changed in params/protocol_params.go
// (MinBlocksForBlobRequests, DefaultExtraReserveForBlobRequests) are not
// consumed by this package; they are p2p/pruning policy, not fee maths, and
// are therefore out of scope here.
//
// When an upstream sync legitimately changes any of the copied functions, the
// oracle copy must be updated in the same PR and the tag in this comment bumped.

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// ---- bnb-chain/bsc v1.7.8, consensus/misc/eip4844/eip4844.go. Do not edit. ----

var (
	upstreamMinBlobGasPrice = big.NewInt(params.BlobTxMinBlobGasprice)
)

// upstreamBlobConfig contains the parameters for blob-related formulas.
// These can be adjusted in a fork.
type upstreamBlobConfig struct {
	Target         int
	Max            int
	UpdateFraction uint64
}

//nolint:unused // verbatim upstream copy: kept so the oracle matches bsc v1.7.8 line for line.
func (bc *upstreamBlobConfig) maxBlobGas() uint64 {
	return uint64(bc.Max) * params.BlobTxBlobGasPerBlob
}

// blobBaseFee computes the blob fee.
func (bc *upstreamBlobConfig) blobBaseFee(excessBlobGas uint64) *big.Int {
	return upstreamFakeExponential(upstreamMinBlobGasPrice, new(big.Int).SetUint64(excessBlobGas), new(big.Int).SetUint64(bc.UpdateFraction))
}

// blobPrice returns the price of one blob in Wei.
func (bc *upstreamBlobConfig) blobPrice(excessBlobGas uint64) *big.Int {
	f := bc.blobBaseFee(excessBlobGas)
	return new(big.Int).Mul(f, big.NewInt(params.BlobTxBlobGasPerBlob))
}

func upstreamLatestBlobConfig(cfg *params.ChainConfig, time uint64) *upstreamBlobConfig {
	if cfg.BlobScheduleConfig == nil {
		return nil
	}
	var (
		london = cfg.LondonBlock
		s      = cfg.BlobScheduleConfig
		bc     *params.BlobConfig
	)
	switch {
	case cfg.IsBPO5(london, time) && s.BPO5 != nil:
		bc = s.BPO5
	case cfg.IsBPO4(london, time) && s.BPO4 != nil:
		bc = s.BPO4
	case cfg.IsBPO3(london, time) && s.BPO3 != nil:
		bc = s.BPO3
	case cfg.IsBPO2(london, time) && s.BPO2 != nil:
		bc = s.BPO2
	case cfg.IsBPO1(london, time) && s.BPO1 != nil:
		bc = s.BPO1
	case cfg.IsOsaka(london, time) && s.Osaka != nil:
		bc = s.Osaka
	case cfg.IsPrague(london, time) && s.Prague != nil:
		bc = s.Prague
	case cfg.IsCancun(london, time) && s.Cancun != nil:
		bc = s.Cancun
	default:
		return nil
	}

	return &upstreamBlobConfig{
		Target:         bc.Target,
		Max:            bc.Max,
		UpdateFraction: bc.UpdateFraction,
	}
}

// upstreamCalcExcessBlobGas calculates the excess blob gas after applying the set of
// blobs on top of the excess blob gas.
func upstreamCalcExcessBlobGas(config *params.ChainConfig, parent *types.Header, headTimestamp uint64) uint64 {
	eip7918 := config.IsOsaka(config.LondonBlock, headTimestamp) && config.IsNotInBSC()
	bcfg := upstreamLatestBlobConfig(config, headTimestamp)

	// BEP-657: for non-recalculation blocks (N % BlobEligibleBlockInterval != 1), inherit parent's ExcessBlobGas
	if config.IsMendel(config.LondonBlock, headTimestamp) && parent.Number.Uint64()%params.BlobEligibleBlockInterval != 0 {
		if parent.ExcessBlobGas != nil {
			return *parent.ExcessBlobGas
		}
		return 0
	}

	return upstreamCalcExcessBlobGasInner(eip7918, bcfg, parent)
}

func upstreamCalcExcessBlobGasInner(eip7918 bool, bcfg *upstreamBlobConfig, parent *types.Header) uint64 {
	var parentExcessBlobGas, parentBlobGasUsed uint64
	if parent.ExcessBlobGas != nil {
		parentExcessBlobGas = *parent.ExcessBlobGas
		parentBlobGasUsed = *parent.BlobGasUsed
	}

	var (
		excessBlobGas = parentExcessBlobGas + parentBlobGasUsed
		targetGas     = uint64(bcfg.Target) * params.BlobTxBlobGasPerBlob
	)
	if excessBlobGas < targetGas {
		return 0
	}

	// EIP-7918 (post-Osaka) introduces a different formula for computing excess,
	// in cases where the price is lower than a 'reserve price'.
	if eip7918 {
		var (
			baseCost     = big.NewInt(params.BlobBaseCost)
			reservePrice = baseCost.Mul(baseCost, parent.BaseFee)
			blobPrice    = bcfg.blobPrice(parentExcessBlobGas)
		)
		if reservePrice.Cmp(blobPrice) > 0 {
			scaledExcess := parentBlobGasUsed * uint64(bcfg.Max-bcfg.Target) / uint64(bcfg.Max)
			return parentExcessBlobGas + scaledExcess
		}
	}

	// Original EIP-4844 formula.
	return excessBlobGas - targetGas
}

// upstreamCalcBlobFee calculates the blobfee from the header's excess blob gas field.
func upstreamCalcBlobFee(config *params.ChainConfig, header *types.Header) *big.Int {
	blobConfig := upstreamLatestBlobConfig(config, header.Time)
	if blobConfig == nil {
		panic("calculating blob fee on unsupported fork")
	}
	return blobConfig.blobBaseFee(*header.ExcessBlobGas)
}

// upstreamFakeExponential approximates factor * e ** (numerator / denominator) using
// Taylor expansion.
func upstreamFakeExponential(factor, numerator, denominator *big.Int) *big.Int {
	var (
		output = new(big.Int)
		accum  = new(big.Int).Mul(factor, denominator)
	)
	for i := 1; accum.Sign() > 0; i++ {
		output.Add(output, accum)

		accum.Mul(accum, numerator)
		accum.Div(accum, denominator)
		accum.Div(accum, big.NewInt(int64(i)))
	}
	return output.Div(output, denominator)
}

// ---- end of upstream copy ----

// Fork ladder for the oracle config: blob schedule changes at each step, and
// Mendel (BEP-657 inheritance) sits between Prague and Osaka so every
// combination of {schedule, Mendel on/off, EIP-7918 on/off} is reachable.
const (
	oraclePragueTime = 1000
	oracleMendelTime = 1500
	oracleOsakaTime  = 2000
	oracleBPO1Time   = 3000
)

func oracleBlobConfig(parlia bool) *params.ChainConfig {
	u := func(v uint64) *uint64 { return &v }
	cfg := &params.ChainConfig{
		ChainID:      big.NewInt(1),
		LondonBlock:  big.NewInt(0),
		ShanghaiTime: u(0),
		CancunTime:   u(0),
		PragueTime:   u(oraclePragueTime),
		MendelTime:   u(oracleMendelTime),
		OsakaTime:    u(oracleOsakaTime),
		BPO1Time:     u(oracleBPO1Time),
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: params.DefaultCancunBlobConfig,
			Prague: params.DefaultPragueBlobConfig,
			Osaka:  params.DefaultOsakaBlobConfig,
			BPO1:   params.DefaultBPO1BlobConfig,
		},
	}
	if parlia {
		cfg.Parlia = &params.ParliaConfig{}
	}
	return cfg
}

// maxOracleExcess bounds excessBlobGas so numerator/denominator in the Taylor
// loop stays <= 64 for every schedule (the smallest UpdateFraction is
// Cancun's). Beyond that the loop runs for numerator/denominator iterations on
// ever-growing big.Ints, which is a hang, not a divergence; on-chain excess is
// bounded far below this by the per-block blob cap anyway.
var maxOracleExcess = 64 * params.DefaultCancunBlobConfig.UpdateFraction

// FuzzUpstreamOracle_BlobFee pins CalcExcessBlobGas and CalcBlobFee to the
// upstream oracle.
//
// mode bits: 0 = Parlia config, 1 = parent carries blob fields, 2-3 = head
// time bias (raw / just before / at / just after a fork boundary picked by
// parentNumber), 4 = bias excess toward small values.
func FuzzUpstreamOracle_BlobFee(f *testing.F) {
	blob := uint64(params.BlobTxBlobGasPerBlob)
	for _, mode := range []uint8{0x00, 0x01, 0x02, 0x03, 0x13, 0x0f, 0x1f, 0x0b} {
		f.Add(uint64(0), uint64(0), uint64(0), uint64(0), uint64(params.InitialBaseFeeForBSC), mode)
		f.Add(3*blob, 6*blob, uint64(10), uint64(500), uint64(params.InitialBaseFeeForBSC), mode)
		f.Add(10*blob, 9*blob, uint64(11), uint64(oracleMendelTime), uint64(1), mode)
		f.Add(50*blob, 3*blob, uint64(12), uint64(oracleOsakaTime), uint64(1_000_000_000), mode)
		f.Add(maxOracleExcess-1, 15*blob, uint64(15), uint64(oracleBPO1Time), uint64(7), mode)
		f.Add(uint64(1), uint64(1), uint64(1), uint64(oraclePragueTime-1), uint64(0), mode)
	}

	f.Fuzz(func(t *testing.T, excessRaw, blobGasUsedRaw, parentNumber, headTimeRaw, parentBaseFee uint64, mode uint8) {
		parlia := mode&0x01 != 0
		hasBlobFields := mode&0x02 != 0
		cfg := oracleBlobConfig(parlia)

		headTime := headTimeRaw
		if bias := (mode >> 2) & 0x03; bias != 0 {
			boundaries := [...]uint64{oraclePragueTime, oracleMendelTime, oracleOsakaTime, oracleBPO1Time}
			b := boundaries[parentNumber%uint64(len(boundaries))]
			headTime = b - 2 + uint64(bias) // b-1, b, b+1
		}

		excess := excessRaw % maxOracleExcess
		if mode&0x10 != 0 {
			excess %= 4 * blob
		}
		blobGasUsed := blobGasUsedRaw % (64 * blob)

		parent := &types.Header{
			Number:  new(big.Int).SetUint64(parentNumber),
			BaseFee: new(big.Int).SetUint64(parentBaseFee),
		}
		if hasBlobFields {
			parent.ExcessBlobGas = &excess
			parent.BlobGasUsed = &blobGasUsed
		}

		// CalcExcessBlobGas: identical on every config.
		gotExcess := CalcExcessBlobGas(cfg, parent, headTime)
		wantExcess := upstreamCalcExcessBlobGas(cfg, parent, headTime)
		if gotExcess != wantExcess {
			t.Fatalf("CalcExcessBlobGas diverged from upstream v1.7.8 (parlia=%v)\n parent number=%d excess=%v blobGasUsed=%v baseFee=%d headTime=%d\n chiliz=%d upstream=%d",
				parlia, parentNumber, ptrOrNil(parent.ExcessBlobGas), ptrOrNil(parent.BlobGasUsed), parentBaseFee, headTime, gotExcess, wantExcess)
		}

		// CalcBlobFee on a header carrying the (bounded) excess.
		head := &types.Header{Time: headTime, ExcessBlobGas: &excess}
		gotFee := CalcBlobFee(cfg, head)
		upstreamFee := upstreamCalcBlobFee(cfg, head)

		if !parlia {
			if MinBlobGasprice(cfg).Cmp(upstreamMinBlobGasPrice) != 0 {
				t.Fatalf("non-Parlia MinBlobGasprice=%s, upstream constant=%s", MinBlobGasprice(cfg), upstreamMinBlobGasPrice)
			}
			if gotFee.Cmp(upstreamFee) != 0 {
				t.Fatalf("non-Parlia CalcBlobFee diverged from upstream v1.7.8\n excess=%d headTime=%d\n chiliz=%s upstream=%s", excess, headTime, gotFee, upstreamFee)
			}
			return
		}

		bcfg := upstreamLatestBlobConfig(cfg, headTime)
		floor := big.NewInt(params.BlobTxMinBlobGaspriceForBSC)
		wantFee := upstreamFakeExponential(floor, new(big.Int).SetUint64(excess), new(big.Int).SetUint64(bcfg.UpdateFraction))
		if gotFee.Cmp(wantFee) != 0 {
			t.Fatalf("Parlia CalcBlobFee outside the allowed difference from upstream v1.7.8\n excess=%d headTime=%d updateFraction=%d\n chiliz=%s want=%s upstream=%s",
				excess, headTime, bcfg.UpdateFraction, gotFee, wantFee, upstreamFee)
		}
		if gotFee.Cmp(floor) < 0 {
			t.Fatalf("Parlia CalcBlobFee below BlobTxMinBlobGaspriceForBSC: %s", gotFee)
		}
		if gotFee.Cmp(upstreamFee) < 0 {
			t.Fatalf("Parlia CalcBlobFee below the upstream fee: chiliz=%s upstream=%s", gotFee, upstreamFee)
		}
	})
}

func ptrOrNil(p *uint64) any {
	if p == nil {
		return nil
	}
	return *p
}
