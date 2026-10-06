package parlia

import (
	"math"
	"math/big"
	"testing"

	cmath "github.com/ethereum/go-ethereum/common/math"
)

// COR-211: fuzz the Dragon8 / Dragon8Fix inflation arithmetic (CLAUDE.md §2).
//
// getInflationPct, getNewSupplyForBlock and getNewSupplyForBlockDragon8Fix mint
// CHZ on every block. Attacker control over their inputs is low (header.Time is
// bounded by the future-block check and forkTs comes from the chain config), but
// the arithmetic goes through float64, an int64 conversion, a uint64 subtraction
// that can underflow, and a hardcoded table. Everything below is a property that
// must hold for every input; where the current code has a quirk (uint64 underflow,
// three internally inconsistent schedule rows) the observed behaviour is PINNED so
// that a change to it is a visible test diff. Pinned is not endorsed.

const (
	// inflationYearSecs mirrors yearInSecs in getNewSupplyForBlockDragon8Fix and
	// the 1/31536000 factor in getInflationPct.
	inflationYearSecs = uint64(31536000)
	// inflationBlocksPerYear mirrors the 10512000 divisor in getNewSupplyForBlock
	// (one block every 3 seconds for a year).
	inflationBlocksPerYear = int64(10512000)

	// inflationPctFloor is the year-13 floor: 1.88% as percent*1e18. Pinned to the
	// wei so the int64(1.88 * 1e18) float conversion is checked for exactness too.
	inflationPctFloor = "1880000000000000000"
	// inflationPctTop is getInflationPct(0): 9.24*e^(-0.25*1)+1.60 = 8.79611923557978%
	// as percent*1e18, after the code's float64 round-trip. This is the largest
	// value the function can return and it sits 427252801274995711 below
	// math.MaxInt64 (9223372036854775807), i.e. a 4.6% margin before the
	// int64(inflationPct * 1e18) conversion would overflow.
	//
	// Architecture note: this constant is only as portable as math.Pow(math.E,
	// -0.25), which routes through math.Exp — hand-written assembly on amd64,
	// arm64, loong64 and s390x, pure Go elsewhere, none of them required to agree
	// in the last bit. The int64 here has a granularity of 1024 (the float64 ULP
	// at 8.8e18) and a single-ULP move in the Pow result shifts it by ~1026, so
	// this pin has no slack at all. It was measured identical on darwin/arm64 and
	// darwin/amd64 (GOAMD64=v1) — the developer and CI architectures — and the
	// 1.88 floor is plain IEEE arithmetic and portable everywhere. getInflationPct
	// as a whole is NOT architecture-stable: replaying the spicy Dragon8 window
	// (dragon8Time 1714381800 -> dragon8FixTime 1717582793) on the two
	// architectures gives a different inflationPct for ~14% of blocks, and the
	// legacy branch feeds that value both into the coinbase mint and into the
	// Tokenomics deposit() calldata. Tracked as COR-223; do not relax this pin to
	// paper over it.
	inflationPctTop = "8796119235579780096"
	// inflationFloorAfterSecs is the last secondsPassed value that still uses the
	// decay formula. getInflationPct computes year = secondsPassed/yearSecs + 1 and
	// floors when year > 13, so the floor starts one second after 12 full years.
	inflationFloorAfterSecs = 12 * inflationYearSecs
)

// dragon8FixGolden is a golden copy of the inflationData table in
// getNewSupplyForBlockDragon8Fix: {inflation fraction*1e18, total supply (wei),
// per-block amount (wei)}. Note the scale: 1e18 here means 100%, unlike
// getInflationPct which returns percent*1e18. A future edit of the production
// table (or a bad upstream-merge resolution) shows up as a diff against this copy.
var dragon8FixGolden = [14][3]string{
	{"87961192355797800", "8888888888000000000000000000", "74379496319128800000"},  // year 0
	{"72043432957447300", "9670766153000000000000000000", "66278081527102500000"},  // year 1
	{"55304937824698300", "10516081346000000000000000000", "55326410292998500000"}, // year 2
	{"46543801590000000", "11097672571000000000000000000", "49136973934551000000"}, // year 3
	{"39673708820000000", "11614200441000000000000000000", "43833562214611900000"}, // year 4
	{"39673708820000000", "12074978847000000000000000000", "39395232686453600000"}, // year 5
	{"34295934680000000", "12489101533000000000000000000", "35751611491628600000"}, // year 6
	{"30091911660000000", "12864922473000000000000000000", "34885308219178100000"}, // year 7
	{"25738888349516300", "13231636833000000000000000000", "32397985455982600000"}, // year 8
	{"23584653872848300", "13572204456000000000000000000", "30450508407284500000"}, // year 9
	{"21906934375499800", "13892300200000000000000000000", "28951456317173600000"}, // year 10
	{"20600325117190600", "14196637909000000000000000000", "27821095556737700000"}, // year 11
	{"19582736803651100", "14489093265000000000000000000", "26991638121944800000"}, // year 12
	{"18800000000000000", "14772829365000000000000000000", "26420204724736900000"}, // year 13
}

// dragon8FixKnownRowDelta pins the rows of the schedule whose per-block amount does
// NOT match the row's own inflation percentage (tracked as COR-219; found by COR-211). For every other
// row, amount*10512000 is within 1 CHZ of supply*pct/1e18; for these three rows the
// per-block amount implies a lower percentage than the pct column (row 5 mints at
// row 6's 3.4296%, row 6 at row 7's 3.0092%, row 7 at 2.8505%), so the minted
// amount is smaller than the percentage reported to Tokenomics via deposit() by
// the amounts below (wei per year). The deltas are pinned exactly, not tolerated:
// fixing the table is a fork-scheduled change to production code, and a change to
// any of these rows must show up here.
var dragon8FixKnownRowDelta = map[int]string{
	5: "-64936508783537087340000000", // ~ -64.94M CHZ / year
	6: "-52504470387656021240000000", // ~ -52.50M CHZ / year
	7: "-20415750570264547980000000", // ~ -20.42M CHZ / year
}

// dragon8FixRow returns the golden row for a schedule year, clamped to row 13 like
// the production code.
func dragon8FixRow(year uint64) (pct, supply, amount *big.Int) {
	if year > 13 {
		year = 13
	}
	row := dragon8FixGolden[year]
	return cmath.MustParseBig256(row[0]), cmath.MustParseBig256(row[1]), cmath.MustParseBig256(row[2])
}

// checkDragon8FixRowConsistency asserts that the per-block amount of a golden row
// re-derives its own annual inflation (supply * pct / 1e18) within 1 CHZ, or, for
// the rows listed in dragon8FixKnownRowDelta, reproduces the pinned discrepancy.
func checkDragon8FixRowConsistency(t testing.TB, year uint64) {
	if year > 13 {
		// Clamp like dragon8FixRow does, so the pinned-delta lookup below cannot
		// name a different row than the one actually checked (and int(year) can
		// never go negative).
		year = 13
	}
	pct, supply, amount := dragon8FixRow(year)
	annualFromPct := new(big.Int).Mul(supply, pct)
	annualFromPct.Div(annualFromPct, big.NewInt(1e18))
	annualFromBlocks := new(big.Int).Mul(amount, big.NewInt(inflationBlocksPerYear))
	delta := new(big.Int).Sub(annualFromBlocks, annualFromPct)

	if pinned, ok := dragon8FixKnownRowDelta[int(year)]; ok {
		if want := cmath.MustParseBig256(pinned); delta.Cmp(want) != 0 {
			t.Fatalf("Dragon8Fix row %d: amount*blocks - supply*pct/1e18 = %s wei, pinned discrepancy is %s (table row changed?)", year, delta, want)
		}
		return
	}
	if new(big.Int).Abs(delta).Cmp(big.NewInt(1e18)) > 0 {
		t.Fatalf("Dragon8Fix row %d: amount*blocks - supply*pct/1e18 = %s wei, exceeds 1 CHZ (schedule row internally inconsistent)", year, delta)
	}
}

// inflationSupplyFromFuzz turns fuzz bytes into a 256-bit lastSupply, biased towards
// the realistic post-fork supply (~8.89e27 wei) and towards zero.
func inflationSupplyFromFuzz(mode uint8, raw []byte) *big.Int {
	if len(raw) > 32 {
		raw = raw[:32]
	}
	switch mode % 3 {
	case 0:
		return new(big.Int)
	case 1:
		// 8888888888 CHZ plus an up-to-8-byte perturbation (< 18.5 CHZ).
		supply := cmath.MustParseBig256(dragon8FixGolden[0][1])
		if len(raw) > 8 {
			raw = raw[:8]
		}
		return supply.Add(supply, new(big.Int).SetBytes(raw))
	default:
		return new(big.Int).SetBytes(raw)
	}
}

func FuzzInflation(f *testing.F) {
	realisticFork := uint64(1718611200) // mainnet dragon8FixTime
	realisticSupply := []byte{}         // mode 1 + empty bytes = exactly 8888888888 CHZ
	twoPow255 := append([]byte{0x80}, make([]byte, 31)...)

	// secondsPassed seeds on a realistic fork timestamp.
	for _, secs := range []uint64{0, inflationYearSecs - 1, inflationYearSecs, 13 * inflationYearSecs, 14 * inflationYearSecs} {
		f.Add(realisticFork, realisticFork+secs, uint8(1), realisticSupply, uint64(0))
	}
	f.Add(uint64(0), uint64(math.MaxUint64), uint8(1), realisticSupply, uint64(0)) // secondsPassed = MaxUint64
	// currentTs < forkTs: uint64 underflow of currentTs - forkTs, by 1 and by 1e9.
	f.Add(realisticFork, realisticFork-1, uint8(1), realisticSupply, uint64(0))
	f.Add(realisticFork, realisticFork-1e9, uint8(1), realisticSupply, uint64(0))
	// lastSupply seeds: 0, 8888888888e18, 2^255.
	f.Add(realisticFork, realisticFork+1, uint8(0), []byte{}, uint64(1))
	f.Add(realisticFork, realisticFork+1, uint8(1), realisticSupply, inflationYearSecs)
	f.Add(realisticFork, realisticFork+1, uint8(2), twoPow255, uint64(math.MaxUint64))

	floor := cmath.MustParseBig256(inflationPctFloor)
	top := cmath.MustParseBig256(inflationPctTop)
	maxInt64 := big.NewInt(math.MaxInt64)
	blocksPerYear := big.NewInt(inflationBlocksPerYear)

	f.Fuzz(func(t *testing.T, forkTs, currentTs uint64, supplyMode uint8, supplyBytes []byte, extraSeconds uint64) {
		lastSupply := inflationSupplyFromFuzz(supplyMode, supplyBytes)

		// Property 1: no panic for any input, including currentTs < forkTs. The
		// production functions subtract forkTs from currentTs in uint64 and wrap;
		// they never notice the underflow. Pinned, not endorsed: a fork timestamp
		// in the future of the block should arguably be rejected, but today it
		// yields the wrapped-argument result and this test asserts exactly that.
		secondsPassed := currentTs - forkTs
		pct := getInflationPct(secondsPassed)
		blockAmount, pctFromSupply := getNewSupplyForBlock(forkTs, currentTs, lastSupply)
		fixPct, fixSupply, fixAmount := getNewSupplyForBlockDragon8Fix(forkTs, currentTs)
		if currentTs < forkTs {
			if pctFromSupply.Cmp(pct) != 0 {
				t.Fatalf("underflow forkTs=%d currentTs=%d: getNewSupplyForBlock pct %s != getInflationPct(wrapped %d) %s", forkTs, currentTs, pctFromSupply, secondsPassed, pct)
			}
			// For every realistic underflow (forkTs - currentTs far below 2^64 -
			// 13y) the wrapped value is astronomically large, so the legacy
			// schedule sits on the floor and the fixed schedule on row 13.
			if secondsPassed > inflationFloorAfterSecs && pct.Cmp(floor) != 0 {
				t.Fatalf("underflow forkTs=%d currentTs=%d: expected floor %s, got %s", forkTs, currentTs, floor, pct)
			}
			if secondsPassed/inflationYearSecs >= 13 {
				if p13, s13, a13 := dragon8FixRow(13); fixPct.Cmp(p13) != 0 || fixSupply.Cmp(s13) != 0 || fixAmount.Cmp(a13) != 0 {
					t.Fatalf("underflow forkTs=%d currentTs=%d: Dragon8Fix did not clamp to row 13: got (%s, %s, %s)", forkTs, currentTs, fixPct, fixSupply, fixAmount)
				}
			}
		}

		// Property 2: getInflationPct is positive, fits int64 (the code converts via
		// int64(float64)), stays within [floor, top], is exact at both ends, and is
		// non-increasing in secondsPassed.
		if pct.Sign() <= 0 {
			t.Fatalf("getInflationPct(%d) = %s, must be > 0", secondsPassed, pct)
		}
		if !pct.IsInt64() || pct.Cmp(maxInt64) > 0 {
			t.Fatalf("getInflationPct(%d) = %s exceeds MaxInt64", secondsPassed, pct)
		}
		if pct.Cmp(floor) < 0 || pct.Cmp(top) > 0 {
			t.Fatalf("getInflationPct(%d) = %s outside [%s, %s]", secondsPassed, pct, floor, top)
		}
		if secondsPassed > inflationFloorAfterSecs {
			if pct.Cmp(floor) != 0 {
				t.Fatalf("getInflationPct(%d) = %s, expected the 1.88%% floor %s after 12 years", secondsPassed, pct, floor)
			}
		} else if pct.Cmp(floor) <= 0 {
			t.Fatalf("getInflationPct(%d) = %s, decay formula must stay strictly above the floor %s before 12 years", secondsPassed, pct, floor)
		}
		if secondsPassed == 0 && pct.Cmp(top) != 0 {
			t.Fatalf("getInflationPct(0) = %s, want exactly %s", pct, top)
		}
		secondsPassed2 := secondsPassed + extraSeconds
		if secondsPassed2 < secondsPassed { // saturate instead of wrapping
			secondsPassed2 = math.MaxUint64
		}
		if pct2 := getInflationPct(secondsPassed2); pct2.Cmp(pct) > 0 {
			t.Fatalf("getInflationPct not monotonic: pct(%d) = %s > pct(%d) = %s", secondsPassed2, pct2, secondsPassed, pct)
		}

		// Property 3: getNewSupplyForBlock rounds down only. The nested integer
		// divisions (1e18, 100, 10512000) compose to a single floor division, so
		// blockAmount*blocks lands in (annual - blocks, annual] where
		// annual = lastSupply * pct / 1e20.
		if blockAmount.Sign() < 0 {
			t.Fatalf("getNewSupplyForBlock(%d, %d, %s) blockAmount %s < 0", forkTs, currentTs, lastSupply, blockAmount)
		}
		annual := new(big.Int).Mul(lastSupply, pct)
		annual.Div(annual, new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
		minted := new(big.Int).Mul(blockAmount, blocksPerYear)
		if minted.Cmp(annual) > 0 {
			t.Fatalf("getNewSupplyForBlock(%d, %d, %s) rounds up: blockAmount*%d = %s > lastSupply*pct/1e20 = %s", forkTs, currentTs, lastSupply, inflationBlocksPerYear, minted, annual)
		}
		if new(big.Int).Sub(annual, minted).Cmp(blocksPerYear) >= 0 {
			t.Fatalf("getNewSupplyForBlock(%d, %d, %s) truncates more than one block: annual %s - minted %s >= %d", forkTs, currentTs, lastSupply, annual, minted, inflationBlocksPerYear)
		}
		if lastSupply.Sign() == 0 && blockAmount.Sign() != 0 {
			t.Fatalf("getNewSupplyForBlock with zero supply minted %s", blockAmount)
		}

		// Property 4: getNewSupplyForBlockDragon8Fix always resolves to a row in
		// [0, 13] and returns exactly the golden triple for min(year, 13).
		year := secondsPassed / inflationYearSecs
		if year > 13 {
			year = 13
		}
		wantPct, wantSupply, wantAmount := dragon8FixRow(year)
		if fixPct.Cmp(wantPct) != 0 || fixSupply.Cmp(wantSupply) != 0 || fixAmount.Cmp(wantAmount) != 0 {
			t.Fatalf("getNewSupplyForBlockDragon8Fix(%d, %d) year %d: got (%s, %s, %s), want (%s, %s, %s)", forkTs, currentTs, year, fixPct, fixSupply, fixAmount, wantPct, wantSupply, wantAmount)
		}
		if fixPct.Sign() <= 0 || fixSupply.Sign() <= 0 || fixAmount.Sign() <= 0 {
			t.Fatalf("getNewSupplyForBlockDragon8Fix(%d, %d) returned a non-positive value: (%s, %s, %s)", forkTs, currentTs, fixPct, fixSupply, fixAmount)
		}
		checkDragon8FixRowConsistency(t, year)
	})
}

// TestDragon8FixSupplyRecurrence pins the relation between adjacent golden rows:
// supply[n+1] = supply[n] + amount[n]*blocksPerYear, within 1 CHZ of rounding,
// except for the step from year 1 to year 2, which additionally carries the
// Pepper8 one-time mint (pepper8MintAmount, 148.6M CHZ; the table comment says
// "one time inflation of 148600000 CHZ during year 1"). The amounts and supplies
// therefore describe one coherent curve; the pct column is the odd one out for
// rows 5-7 (COR-219, see checkDragon8FixRowConsistency).
func TestDragon8FixSupplyRecurrence(t *testing.T) {
	oneCHZ := big.NewInt(1e18)
	blocks := big.NewInt(inflationBlocksPerYear)
	pepper8 := cmath.MustParseBig256(pepper8MintAmount)
	for n := uint64(0); n < 13; n++ {
		_, s0, a0 := dragon8FixRow(n)
		_, s1, _ := dragon8FixRow(n + 1)
		delta := new(big.Int).Sub(s1, new(big.Int).Add(s0, new(big.Int).Mul(a0, blocks)))
		if n == 1 {
			delta.Sub(delta, pepper8)
		}
		if new(big.Int).Abs(delta).Cmp(oneCHZ) > 0 {
			t.Fatalf("Dragon8Fix rows %d->%d: supply[n+1] - (supply[n] + amount[n]*blocks) off by %s wei (year-1 step is net of the Pepper8 mint)", n, n+1, delta)
		}
	}
}

// TestDragon8FixScheduleRows is the plain-test twin of FuzzInflation's property 4:
// every year 0..13 (and the clamped years beyond) must return the golden row, and
// every row must re-derive its own inflation from the per-block amount, except the
// three rows whose discrepancy is pinned in dragon8FixKnownRowDelta.
func TestDragon8FixScheduleRows(t *testing.T) {
	fork := uint64(1718611200) // mainnet dragon8FixTime

	for year := uint64(0); year <= 13; year++ {
		wantPct, wantSupply, wantAmount := dragon8FixRow(year)
		// First and last second of the schedule year.
		for _, offset := range []uint64{0, inflationYearSecs - 1} {
			pct, supply, amount := getNewSupplyForBlockDragon8Fix(fork, fork+year*inflationYearSecs+offset)
			if pct.Cmp(wantPct) != 0 || supply.Cmp(wantSupply) != 0 || amount.Cmp(wantAmount) != 0 {
				t.Fatalf("year %d (+%ds): got (%s, %s, %s), want golden (%s, %s, %s)", year, offset, pct, supply, amount, wantPct, wantSupply, wantAmount)
			}
		}
		checkDragon8FixRowConsistency(t, year)
	}

	// Beyond the table the schedule clamps to row 13 forever.
	wantPct, wantSupply, wantAmount := dragon8FixRow(13)
	for _, year := range []uint64{14, 15, 100, math.MaxUint64 / inflationYearSecs} {
		pct, supply, amount := getNewSupplyForBlockDragon8Fix(fork, fork+year*inflationYearSecs)
		if pct.Cmp(wantPct) != 0 || supply.Cmp(wantSupply) != 0 || amount.Cmp(wantAmount) != 0 {
			t.Fatalf("year %d: got (%s, %s, %s), want row 13 (%s, %s, %s)", year, pct, supply, amount, wantPct, wantSupply, wantAmount)
		}
	}

	// The schedule's own year-13 row carries 1.88% and the legacy formula floors at
	// the same percentage (different scale: fraction*1e18 vs percent*1e18).
	if got := new(big.Int).Mul(wantPct, big.NewInt(100)); got.Cmp(cmath.MustParseBig256(inflationPctFloor)) != 0 {
		t.Fatalf("row 13 pct*100 = %s, legacy floor = %s", got, inflationPctFloor)
	}
	if got := getInflationPct(13 * inflationYearSecs); got.Cmp(cmath.MustParseBig256(inflationPctFloor)) != 0 {
		t.Fatalf("getInflationPct(13y) = %s, want floor %s", got, inflationPctFloor)
	}
	if got := getInflationPct(0); got.Cmp(cmath.MustParseBig256(inflationPctTop)) != 0 {
		t.Fatalf("getInflationPct(0) = %s, want %s", got, inflationPctTop)
	}
}
