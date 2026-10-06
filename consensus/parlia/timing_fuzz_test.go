package parlia

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// COR-236: property-based coverage of header timing.
//
// Timing rules decide which headers a node accepts from its peers. Drift in
// either direction is a production incident: too lenient and two halves of the
// fleet disagree about a block that arrived early (a fork); too strict and the
// node rejects the honest head and stalls. The pieces pinned here are:
//
//   - the Chiliz **+1 second tolerance** on the future-block check in
//     verifyHeader (CLAUDE.md §6). Upstream BSC rejects any header whose
//     timestamp is past the current wall-clock second; Chiliz grants exactly
//     one second of grace. It is a single expression inside a single `if`,
//     with no test of its own before this one — precisely the shape of change
//     an upstream sync reverts silently while the build stays green and the
//     unit suite stays green. TestChilizFutureBlockTolerance asserts the
//     tolerance **by value**: it measures the largest accepted offset from the
//     production code and compares it against chilizFutureBlockTolerance, so
//     dropping the grace (0) and widening it (2) both fail. FuzzHeaderTiming
//     keeps only the part of that property which consumes a fuzzed byte: the
//     accept/reject verdict at an arbitrary int64 offset.
//   - monotonicity: a child timestamp at or before its parent's is rejected
//     under every block-interval regime.
//   - the Ramanujan inequality: blockTimeVerifyForRamanujanFork accepts
//     exactly `header.MilliTimestamp() >= parent.MilliTimestamp() + interval +
//     backoff`, probed at threshold-1 / threshold / threshold+1 so an
//     off-by-one in the comparison cannot survive.
//   - Delay's bounds, isIntentionalDelayMining's no-panic and timing term, and
//     the fork-driven block-interval selection in Snapshot.apply.
//
// BOUNDARY WITH COR-230 (scheduling_fuzz_test.go, FuzzBackOffTime): that target
// owns backOffTime's internals — no panic, zero for the in-turn validator,
// boundedness, distinctness across competing validators, insertion-order
// independence, and the pre-Bohr snap.Number seeding. It reaches full statement
// coverage of backOffTime from its seeds. This target deliberately does NOT
// re-assert any of that. Here backOffTime appears only as an *input* to the
// Ramanujan inequality (property 3): whatever it returns, the accept/reject
// boundary must sit exactly at parent + interval + that value. Keeping the two
// targets disjoint keeps a failure attributable to one of them.

// chilizFutureBlockTolerance is the Chiliz grace period, in seconds, that
// verifyHeader grants a header arriving from the future (CLAUDE.md §6). The
// production expression is
//
//	header.Time > uint64(time.Now().Unix()+time.Second.Milliseconds()/1000)
//
// i.e. `now + 1`. Upstream BSC has no `+ ...` term at all, so an upstream merge
// that takes their side of this line silently sets the tolerance to 0. This
// constant is the literal the target pins; do not "sync" it with the
// production expression by re-deriving it here, or the assertion becomes a
// tautology that survives exactly the regression it exists to catch.
const chilizFutureBlockTolerance = int64(1)

// timingToleranceProbeLow/High bound the offsets measureFutureBlockTolerance
// scans when it determines the tolerance empirically. The window straddles the
// boundary on both sides so that removing the tolerance (accepted tops out at
// 0) and widening it (accepted reaches 2 or 3) are both observable, not merely
// "some tolerance exists".
const (
	timingToleranceProbeLow  = int64(-2)
	timingToleranceProbeHigh = int64(3)
)

// timingClockRetries is how many times a wall-clock probe is retried when the
// second ticks across the verifyHeader call. verifyHeader reads time.Now()
// itself, so a probe is only attributable to a known `now` if the clock did not
// advance between the test's sample and the call's own read. Sixteen attempts
// make an unattributable verdict effectively impossible; the probe skips rather
// than guessing if they all race.
const timingClockRetries = 16

// timingChainReader adds GetBlock to prepareTestChainReader so it satisfies
// consensus.ChainReader, which is what (*Parlia).Delay takes. Delay never
// reaches for a body, so returning nil is faithful.
type timingChainReader struct {
	*prepareTestChainReader
}

func (r *timingChainReader) GetBlock(hash common.Hash, number uint64) *types.Block { return nil }

// timingSnapshotDB backs the test engines' snapshot store. Parlia.snapshot()
// consults loadSnapshot(p.db, ...) whenever the walk reaches a checkpoint-
// aligned block number, and dereferences p.db unconditionally there, so a
// struct-literal engine with a nil db segfaults on those numbers. Every engine
// New() builds carries a real database, so this is a property of the test
// harness and not of production; the database is shared because nothing in
// this file ever writes to it (a store only happens when the walk applied at
// least one header, which the cache hits here never do).
var timingSnapshotDB = rawdb.NewMemoryDatabase()

// newTimingParlia builds an engine by struct literal rather than through New().
// New() parses five ABIs per call (too slow for a fuzz body) and, worse, writes
// the package-level `defaultEpochLength` from the config it is handed — a
// global that other tests in this package read. Only the fields the timing
// paths touch are populated.
func newTimingParlia(cfg *params.ChainConfig) *Parlia {
	return &Parlia{
		chainConfig: cfg,
		config:      cfg.Parlia,
		db:          timingSnapshotDB,
		recentSnaps: lru.NewCache[common.Hash, *Snapshot](inMemorySnapshots),
		signatures:  lru.NewCache[common.Hash, common.Address](inMemorySignatures),
	}
}

// timingBaseConfig is a Parlia chain config in the fork regime Chiliz mainnet
// is in today, minus the block-interval forks: every fork the timing paths
// consult and that mainnet has already passed is active from genesis, and
// Lorentz / Maxwell / Fermi (the three that move the block interval) and Bohr
// are left unscheduled. Callers overwrite the fields they want to move.
//
// PlanckBlock is load-bearing and easy to omit by accident. snap.Recents is
// consumed on exactly one reachable path — snap.countRecents() inside
// `if p.chainConfig.IsPlanck(header.Number)` in backOffTime — so with
// PlanckBlock nil that whole branch is dead, the recent-signer stamping
// newFuzzSnapshot does is dead weight, and the `recents` fuzz dimension is
// inert. Mainnet has `"planckBlock": 13189711` and has been past it since
// block 13.2M, i.e. the regime a Planck-less fixture never enters is the only
// one production uses. Proven by mutation: gating backOffTime's Planck branch
// off entirely (`if false && p.chainConfig.IsPlanck(...)`) left this target
// green on every seed before PlanckBlock was added here, and fails on the
// recents-carrying seeds after.
func timingBaseConfig() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:             big.NewInt(88888),
		HomesteadBlock:      common.Big0,
		EIP150Block:         common.Big0,
		EIP155Block:         common.Big0,
		EIP158Block:         common.Big0,
		ByzantiumBlock:      common.Big0,
		ConstantinopleBlock: common.Big0,
		PetersburgBlock:     common.Big0,
		IstanbulBlock:       common.Big0,
		MuirGlacierBlock:    common.Big0,
		BerlinBlock:         common.Big0,
		LondonBlock:         common.Big0,
		RamanujanBlock:      common.Big0,
		NielsBlock:          common.Big0,
		MirrorSyncBlock:     common.Big0,
		BrunoBlock:          common.Big0,
		PlanckBlock:         common.Big0,
		Parlia:              &params.ParliaConfig{Period: 3, Epoch: 200},
	}
}

// setMilliTimestamp stamps an exact millisecond timestamp onto a header, using
// the same split consensus uses: whole seconds in Time, the remainder in the
// MixDigest (types.Header.SetMilliseconds). Post-Lorentz block intervals are
// sub-second (1500ms, 750ms, 450ms), so every timing comparison in Parlia runs
// on MilliTimestamp, not Time — a target that only moved Time could not see an
// off-by-one smaller than a second.
func setMilliTimestamp(h *types.Header, ms uint64) {
	h.Time = ms / 1000
	h.SetMilliseconds(ms % 1000)
}

// timingHeader builds a header carrying an exact millisecond timestamp.
func timingHeader(number uint64, parentHash common.Hash, ms uint64, coinbase common.Address, difficulty *big.Int) *types.Header {
	h := &types.Header{
		Number:     new(big.Int).SetUint64(number),
		ParentHash: parentHash,
		Difficulty: new(big.Int).Set(difficulty),
		Coinbase:   coinbase,
		Extra:      make([]byte, extraVanity+extraSeal),
	}
	setMilliTimestamp(h, ms)
	return h
}

// futureProbeTimestamp converts "the wall-clock second `base`, plus `offset`
// seconds" into the uint64 a header carries, saturating instead of wrapping.
//
// CORRECTION (kept deliberately, per the wave-two rule on self-inflicted
// crashers): futureBlockRejected originally wrote the probe timestamp as
// `uint64(before + offset)`. futureOffset is an unrestricted fuzz int64, so any
// offset further into the past than the current Unix second (today, anything
// below about -1.79e9) makes that sum negative, and the conversion reinterprets
// it as a value near 2^64. verifyHeader then correctly rejects that header as
// far-future, while property 1's oracle — `wantRejected := futureOffset >
// chilizFutureBlockTolerance` — expects every negative offset to be accepted.
// The result was a crasher manufactured entirely by the harness, reporting
// "header at now-3000000000: rejected=true, want false" with nothing wrong in
// consensus. The seed corpus never hit it (the most negative seed, -1e9, still
// lands in 2001), but a campaign finds it quickly.
//
// The fix belongs here rather than in the oracle: a pre-epoch timestamp is not
// representable in an unsigned header field at all, so the faithful rendering
// of "absurdly far in the past" is the epoch itself — which is exactly as
// un-future as the offset the fuzzer asked for, leaving the oracle true as
// written. The int64 overflow at the other end is saturated for symmetry: it
// happens to produce the right verdict by wrapping today, but by accident, and
// an accident is not a property.
func futureProbeTimestamp(base, offset int64) uint64 {
	sum := base + offset
	switch {
	case offset > 0 && sum < base:
		// int64 overflow past the end of time; stay as far-future as int64 goes.
		return futureProbeMaxTime
	case offset < 0 && sum > base:
		// int64 underflow past the start of time. Unreachable while base is a
		// Unix second (positive), since no int64 offset can drag a positive
		// base below MinInt64 — kept so the function is total in its own
		// right rather than only under an assumption about its caller.
		return 0
	case sum < 0:
		// Representable as an int64, but before the Unix epoch, and header
		// timestamps are unsigned. Clamp to the epoch. This is the branch the
		// correction above is about.
		return 0
	default:
		return uint64(sum)
	}
}

// futureProbeMaxTime is the saturation point futureProbeTimestamp uses for an
// offset that overflows int64. It is far past any wall clock, so verifyHeader
// rejects it as a future block, which is what the oracle wants for a positive
// offset.
const futureProbeMaxTime = uint64(math.MaxInt64)

// The three committed corpus entries under testdata/fuzz/FuzzHeaderTiming pin
// the saturation above. Their discriminating power is not equal, and the
// difference is recorded here rather than left to be rediscovered:
//
//   - pre-epoch-future-offset (offset -3e9) is the crasher the CORRECTION note
//     above describes. Under the original `uint64(before + offset)` it wrapped
//     to a near-2^64 timestamp and was rejected while the oracle expected
//     acceptance; under the clamp it is accepted. The two versions disagree,
//     so the seed distinguishes them.
//   - int64-min-future-offset (offset MinInt64) is the same, harder: the sum
//     underflows int64 as well as going pre-epoch.
//   - int64-max-future-offset (offset MaxInt64) does NOT distinguish anything.
//     Wrapping and saturation both produce a timestamp verifyHeader rejects,
//     and the oracle wants rejection either way, so the old code and the new
//     code return the same verdict on it. It is kept because it is a free
//     boundary probe of the positive end and costs one execution, not because
//     it witnesses the bug the other two witness.

// futureBlockRejected reports whether verifyHeader rejects a header whose
// timestamp is exactly `offset` seconds past the wall-clock second that
// verifyHeader itself observes (saturated at the epoch for pre-epoch offsets,
// see futureProbeTimestamp).
//
// The header is deliberately left structurally invalid (no vanity in Extra) so
// the two outcomes are unambiguous and cannot be confused with each other:
//
//	rejected as future  -> consensus.ErrFutureBlock, from the first statement
//	                       of verifyHeader, before anything else runs;
//	not rejected        -> errMissingVanity, from VerifyUnsealedHeader, which
//	                       is the very next call.
//
// This matters because ErrFutureBlock is also returned much deeper down by
// blockTimeVerifyForRamanujanFork; keying on the error alone without making the
// header fail fast afterwards would let the Ramanujan check masquerade as the
// tolerance and vice versa.
func futureBlockRejected(t *testing.T, p *Parlia, chain consensus.ChainHeaderReader, offset int64) bool {
	t.Helper()
	for attempt := 0; attempt < timingClockRetries; attempt++ {
		before := time.Now().Unix()
		h := &types.Header{Number: big.NewInt(1), Time: futureProbeTimestamp(before, offset)}
		err := p.verifyHeader(chain, h, nil)
		if time.Now().Unix() != before {
			// The clock ticked across the call, so the verdict cannot be
			// attributed to a known `now`. Retry rather than assert.
			continue
		}
		switch {
		case errors.Is(err, consensus.ErrFutureBlock):
			return true
		case errors.Is(err, errMissingVanity):
			return false
		default:
			t.Fatalf("verifyHeader at now%+d returned %v, want ErrFutureBlock or errMissingVanity "+
				"(the probe header is meant to fail fast on its Extra once it is past the future check)", offset, err)
		}
	}
	// Not t.Skip. This helper is also the measuring instrument of
	// TestChilizFutureBlockTolerance, the by-value tripwire for the Chiliz +1s
	// grace (CLAUDE.md §6). t.Skip unwinds through runtime.Goexit, so a skip here
	// would exit that whole test and report it as skipped rather than failed — a
	// tripwire that can pass without having checked anything, and `go test` exits
	// 0 on it. Inside FuzzHeaderTiming it would abandon properties 2-5 and be
	// counted as a successful execution. Failing is the only verdict that cannot
	// be mistaken for a green run.
	//
	// The condition is close to impossible — the wall clock ticks once a second
	// and verifyHeader returns in microseconds, so this needs timingClockRetries
	// consecutive ticks across the call — which is exactly why failing costs
	// nothing and skipping is the expensive choice.
	t.Fatalf("wall clock ticked across all %d future-block probe attempts at offset %+d; "+
		"no verdict could be attributed to a known `now`", timingClockRetries, offset)
	return false
}

// measureFutureBlockTolerance scans offsets from timingToleranceProbeLow up to
// timingToleranceProbeHigh and returns the largest offset verifyHeader still
// accepts — that is, the tolerance the production code actually implements.
// It also asserts the verdict is a single step (accept for every offset at or
// below the boundary, reject for every offset above), so a tolerance that is
// not a simple threshold is caught too.
func measureFutureBlockTolerance(t *testing.T, p *Parlia, chain consensus.ChainHeaderReader) int64 {
	t.Helper()
	measured := timingToleranceProbeLow - 1
	for off := timingToleranceProbeLow; off <= timingToleranceProbeHigh; off++ {
		if futureBlockRejected(t, p, chain, off) {
			continue
		}
		if measured != off-1 {
			t.Fatalf("future-block verdict is not a threshold: offset %+d accepted but %+d rejected", off, off-1)
		}
		measured = off
	}
	if measured < timingToleranceProbeLow {
		t.Fatalf("verifyHeader rejected every probed offset down to %+d; the future-block check is not a tolerance at all", timingToleranceProbeLow)
	}
	return measured
}

// TestChilizFutureBlockTolerance pins the Chiliz +1s grace **by value**, and is
// the reason this file exists. CLAUDE.md §6 lists the tolerance as a Chiliz
// behavioural tweak with no test; upstream BSC's line is
// `if header.Time > uint64(time.Now().Unix())`, so taking their side of a
// conflict — or an upstream refactor that rewrites the condition — silently
// removes it. Measuring the boundary from the production code and comparing it
// to a literal catches removal (measured 0), widening (measured 2+), and a
// verdict that is not a simple threshold.
//
// This is a plain test rather than a fuzz property because it reads no byte of
// any fuzz input: measureFutureBlockTolerance scans a fixed six offsets and the
// two restatements below probe two more, so running it per-exec re-derived one
// constant eight times per execution and spent the campaign budget doing it.
// The part of the property that *does* vary with the input — the accept/reject
// verdict at an arbitrary int64 offset — stays in FuzzHeaderTiming.
//
// The probe headers carry no Extra at all, so once they are past the future
// check they fail immediately on errMissingVanity, which is the first statement
// of VerifyUnsealedHeader and touches nothing else. The chain reader is
// therefore never consulted; an empty one is faithful.
func TestChilizFutureBlockTolerance(t *testing.T) {
	cfg := timingBaseConfig()
	p := newTimingParlia(cfg)
	chain := &prepareTestChainReader{config: cfg, headers: map[common.Hash]*types.Header{}}

	measured := measureFutureBlockTolerance(t, p, chain)
	if measured != chilizFutureBlockTolerance {
		t.Fatalf("future-block tolerance is %d second(s), want exactly %d "+
			"(CLAUDE.md §6: Chiliz grants +1s over upstream BSC's zero; a merge that dropped or widened it lands here)",
			measured, chilizFutureBlockTolerance)
	}
	// Restate the boundary on the two offsets that define it, so the failure
	// message names them even when the scan above is changed.
	if futureBlockRejected(t, p, chain, chilizFutureBlockTolerance) {
		t.Fatalf("a header at now+%d was rejected as a future block; the Chiliz tolerance is gone", chilizFutureBlockTolerance)
	}
	if !futureBlockRejected(t, p, chain, chilizFutureBlockTolerance+1) {
		t.Fatalf("a header at now+%d was accepted; the future-block tolerance is wider than the %d second Chiliz grants",
			chilizFutureBlockTolerance+1, chilizFutureBlockTolerance)
	}
}

// timingFuzzMaxParentSeconds bounds the fuzzed parent timestamp. Header
// timestamps are converted to milliseconds everywhere in the timing path
// (Time*1000), so an unbounded uint64 second would overflow the arithmetic the
// test itself does to predict the answer, turning every such input into a
// spurious failure about the test's own model rather than about consensus.
// 10^12 seconds is the year 33658 — far past anything the chain will see, while
// Time*1000 stays four orders of magnitude below the uint64 ceiling.
const timingFuzzMaxParentSeconds = uint64(1_000_000_000_000)

// timingIntervals is the block interval each fork regime selects, in
// milliseconds, written as literals rather than by referring to the production
// constants. The whole point of property 6 is to fail when a merge selects the
// wrong interval for a fork, and naming the production constant on both sides
// of that comparison would make it unfalsifiable.
const (
	timingDefaultIntervalMs = uint64(3000)
	timingLorentzIntervalMs = uint64(1500)
	timingMaxwellIntervalMs = uint64(750)
	timingFermiIntervalMs   = uint64(450)
)

// TestTimingIntervalConstants pins the production constants against the
// literals above. Keeping it a plain test (not a fuzz property) means a merge
// that changes one of the four constants fails here, naming the constant,
// instead of surfacing as an opaque interval mismatch inside the fuzz body.
func TestTimingIntervalConstants(t *testing.T) {
	for _, c := range []struct {
		name string
		got  uint64
		want uint64
	}{
		{"defaultBlockInterval", defaultBlockInterval, timingDefaultIntervalMs},
		{"lorentzBlockInterval", lorentzBlockInterval, timingLorentzIntervalMs},
		{"maxwellBlockInterval", maxwellBlockInterval, timingMaxwellIntervalMs},
		{"fermiBlockInterval", fermiBlockInterval, timingFermiIntervalMs},
	} {
		if c.got != c.want {
			t.Fatalf("%s = %d ms, want %d ms", c.name, c.got, c.want)
		}
	}
}

// expectedBlockIntervalAt restates the fork -> interval mapping independently
// of the production if-chain: the interval is decided by the header's own
// timestamp against each fork time, highest fork wins, and a header before
// every scheduled fork leaves the snapshot's interval untouched (it is sticky,
// not reset to the default — that distinction is itself worth pinning, since a
// merge that "tidied" the else branch into `= defaultBlockInterval` would
// rewind the interval on any chain whose forks are unscheduled but whose
// snapshot was loaded from disk with a non-default value).
func expectedBlockIntervalAt(cfg *params.ChainConfig, headerTime uint64, prev uint64) uint64 {
	switch {
	case cfg.FermiTime != nil && headerTime >= *cfg.FermiTime:
		return timingFermiIntervalMs
	case cfg.MaxwellTime != nil && headerTime >= *cfg.MaxwellTime:
		return timingMaxwellIntervalMs
	case cfg.LorentzTime != nil && headerTime >= *cfg.LorentzTime:
		return timingLorentzIntervalMs
	default:
		return prev
	}
}

const (
	// The interval walk starts at block 10 rather than block 1 so that no
	// header number collides with minerHistoryCheckLen() (2 for this fixture),
	// which is where Snapshot.apply performs an epoch validator-set switch.
	timingIntervalFirstNumber = uint64(10)
	timingIntervalChainLen    = 9
	timingIntervalGenesisTime = uint64(1_800_000_000)
	timingIntervalPeriod      = uint64(3)
	// A prime epoch length that matches none of the package's epoch constants,
	// so the fixture is immune to defaultEpochLength having been rewritten by
	// another test's call to New().
	timingIntervalEpoch   = uint64(977)
	timingIntervalVals    = 4
	timingIntervalSeed    = uint64(0x7157_1D1E_2B0C_4A11)
	timingIntervalUnsched = timingIntervalChainLen + 1
)

// timingIntervalValidators is the fixed validator set the interval walk uses.
// It does not depend on the fuzz input, so it is derived once.
var timingIntervalValidators = deriveFuzzValidators(timingIntervalSeed, timingIntervalVals)

// checkBlockIntervalSelection walks a short chain across the Lorentz / Maxwell
// / Fermi boundaries and asserts, after every single header, that
// Snapshot.apply selected the interval the fork rules call for — and that the
// switch happens at the first header at or after the fork time, not one block
// early or late.
//
// lorentzAt is the chain index at which Lorentz activates, or
// timingIntervalUnsched meaning "no interval fork is scheduled at all"; gap is
// the distance, in blocks, to Maxwell and then Fermi. Gaps that run past the
// end of the chain exercise "scheduled but not yet reached".
//
// The headers are not signed. Snapshot.apply resolves the producer through
// ecrecover, which consults its signature LRU by header hash first, so
// pre-seeding that cache is equivalent to signing and costs three orders of
// magnitude less. Signature recovery itself is covered by COR-221/222
// (snapshot_apply_fuzz_test.go); mixing it in here would only slow things down.
func checkBlockIntervalSelection(t *testing.T, lorentzAt, gap int) {
	t.Helper()

	cfg := timingBaseConfig()
	if lorentzAt <= timingIntervalUnsched-1 {
		at := func(idx int) *uint64 {
			v := timingIntervalGenesisTime + uint64(idx)*timingIntervalPeriod
			return &v
		}
		cfg.LorentzTime = at(lorentzAt)
		cfg.MaxwellTime = at(lorentzAt + gap)
		cfg.FermiTime = at(lorentzAt + 2*gap)
	}

	sigCache := lru.NewCache[common.Hash, common.Address](inMemorySignatures)

	base := &types.Header{
		Number:     new(big.Int).SetUint64(timingIntervalFirstNumber - 1),
		Time:       timingIntervalGenesisTime - timingIntervalPeriod,
		Difficulty: big.NewInt(1),
		Extra:      make([]byte, extraVanity+extraSeal),
	}
	headers := make([]*types.Header, 0, timingIntervalChainLen)
	byHash := map[common.Hash]*types.Header{base.Hash(): base}
	parent := base
	for i := 0; i < timingIntervalChainLen; i++ {
		number := timingIntervalFirstNumber + uint64(i)
		producer := timingIntervalValidators[number%timingIntervalVals]
		h := &types.Header{
			Number:     new(big.Int).SetUint64(number),
			ParentHash: parent.Hash(),
			Time:       timingIntervalGenesisTime + uint64(i)*timingIntervalPeriod,
			Difficulty: big.NewInt(1),
			Coinbase:   producer,
			Extra:      make([]byte, extraVanity+extraSeal),
		}
		sigCache.Add(h.Hash(), producer)
		byHash[h.Hash()] = h
		headers = append(headers, h)
		parent = h
	}

	chain := &prepareTestChainReader{config: cfg, genesis: base, headers: byHash}
	snap := newSnapshot(cfg.Parlia, sigCache, base.Number.Uint64(), base.Hash(), timingIntervalValidators, nil, nil, false)
	snap.EpochLength = timingIntervalEpoch
	snap.TurnLength = 1

	if snap.BlockInterval != timingDefaultIntervalMs {
		t.Fatalf("a fresh snapshot starts at interval %d ms, want the default %d ms", snap.BlockInterval, timingDefaultIntervalMs)
	}

	want := timingDefaultIntervalMs
	firstAt := map[uint64]int{}
	for i, h := range headers {
		want = expectedBlockIntervalAt(cfg, h.Time, want)
		next, err := snap.apply(headers[i:i+1], chain, nil, cfg, false)
		if err != nil {
			t.Fatalf("apply(header %d) failed: %v", h.Number, err)
		}
		snap = next
		if snap.BlockInterval != want {
			t.Fatalf("after block %d (time %d, lorentz=%v maxwell=%v fermi=%v): interval %d ms, want %d ms",
				h.Number, h.Time, derefTime(cfg.LorentzTime), derefTime(cfg.MaxwellTime), derefTime(cfg.FermiTime),
				snap.BlockInterval, want)
		}
		if _, seen := firstAt[snap.BlockInterval]; !seen {
			firstAt[snap.BlockInterval] = i
		}
	}

	// The per-header assertion above already pins the boundary, but state it
	// once more in the form the invariant is written in: the index at which
	// each interval first appears must be the index of the first header at or
	// after that fork's time. "One block early or late" is the classic way a
	// merge breaks a timestamp fork (>= silently becoming >), and this says so
	// in the failure message.
	for _, fk := range []struct {
		name     string
		forkTime *uint64
		interval uint64
	}{
		{"Lorentz", cfg.LorentzTime, timingLorentzIntervalMs},
		{"Maxwell", cfg.MaxwellTime, timingMaxwellIntervalMs},
		{"Fermi", cfg.FermiTime, timingFermiIntervalMs},
	} {
		if fk.forkTime == nil {
			continue
		}
		wantIdx := -1
		for i, h := range headers {
			if h.Time >= *fk.forkTime {
				wantIdx = i
				break
			}
		}
		// A later fork that activates on the same header hides the earlier
		// one's interval entirely; that is the if-chain's documented priority,
		// not an off-by-one, so only assert when the interval was reachable.
		reachable := wantIdx >= 0 && expectedBlockIntervalAt(cfg, headers[wantIdx].Time, timingDefaultIntervalMs) == fk.interval
		gotIdx, seen := firstAt[fk.interval]
		if !reachable {
			continue
		}
		if !seen || gotIdx != wantIdx {
			t.Fatalf("%s interval %d ms first appeared at chain index %d (seen=%v), want %d (fork time %d, block times %d..%d)",
				fk.name, fk.interval, gotIdx, seen, wantIdx, *fk.forkTime,
				headers[0].Time, headers[len(headers)-1].Time)
		}
	}
}

// TestBlockIntervalSelection runs checkBlockIntervalSelection over every fork
// placement it can distinguish: eleven Lorentz activation indices — the nine
// chain positions (timingIntervalChainLen is 9, so indices 0..8), index 9 for a
// fork scheduled one period past the last header and therefore never reached,
// and index timingIntervalUnsched for "nothing scheduled" — crossed with the
// three gaps to Maxwell and Fermi. 33 cases, enumerated rather than sampled.
//
// This used to be property 6 of FuzzHeaderTiming, driven by a uint8 forkSel.
// It never read anything else from the fuzz input, so those 33 meanings were
// all it could ever express, while each execution rebuilt the nine-header chain
// and ran nine Snapshot.apply calls (nine deep copies of a snapshot) to express
// one of them. Enumerating them once is both exhaustive — the fuzzer had no
// guarantee of covering all 33 — and off the campaign's critical path.
func TestBlockIntervalSelection(t *testing.T) {
	for lorentzAt := 0; lorentzAt <= timingIntervalUnsched; lorentzAt++ {
		for gap := 1; gap <= 3; gap++ {
			t.Run(fmt.Sprintf("lorentzAt=%d/gap=%d", lorentzAt, gap), func(t *testing.T) {
				checkBlockIntervalSelection(t, lorentzAt, gap)
			})
		}
	}
}

// FuzzHeaderTiming pins the header timing rules listed at the top of this file.
//
// Properties, in the order they are checked:
//
//  1. A header at a fuzzed offset from the wall clock is rejected with
//     ErrFutureBlock exactly when that offset exceeds the Chiliz tolerance.
//     (That the tolerance *is* one second is TestChilizFutureBlockTolerance's
//     job — it reads no fuzz input, so it does not belong in the loop.)
//  2. Monotonicity: a child timestamp at or before its parent's is rejected
//     under every block-interval regime.
//  3. blockTimeVerifyForRamanujanFork accepts exactly the headers satisfying
//     header.MilliTimestamp() >= parent.MilliTimestamp() + interval + backoff,
//     for every interval, probed on both sides of the threshold; and accepts
//     unconditionally when Ramanujan is not active.
//  4. Delay's own clamps hold, and delayForRamanujanFork's pre-fork wiggle
//     stays inside the interval the constants define.
//  5. isIntentionalDelayMining never panics, and each of its four conjuncts is
//     falsifiable by the fuzz input.
//
// The fork -> block-interval selection that used to be property 6 is now
// TestBlockIntervalSelection: it depended on nothing but a single uint8 with 33
// distinct meanings, so enumerating them is both exhaustive and cheaper.
func FuzzHeaderTiming(f *testing.F) {
	// seed layout:
	//   parentSecRaw, childDeltaMs, parentMillis, numberRaw, nValRaw,
	//   valIdxRaw, turnLen50, recents, futureOffset, leftOverMs

	// --- property 1 seeds: the tolerance boundary, from both sides. ---
	// Exactly now+1 — accepted only because the Chiliz tolerance exists.
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(1), uint16(0))
	// Exactly now+2 — the first rejected offset; a widened tolerance accepts it.
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(2), uint16(0))
	// now and now-1 — must stay accepted whatever happens to the tolerance.
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(0), uint16(0))
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(-1), uint16(0))
	// Genuinely far future (a day, and ~31 years) — never acceptable.
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(86400), uint16(0))
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(1_000_000_000), uint16(0))

	// --- property 2 seeds: monotonicity. ---
	// Child timestamp equal to the parent's.
	f.Add(uint64(1_700_000_000), int64(0), uint16(0), uint16(100), uint8(4), uint16(0x0001), false, []byte{1, 2}, int64(0), uint16(0))
	// Child exactly one second before its parent.
	f.Add(uint64(1_700_000_000), int64(-1000), uint16(0), uint16(100), uint8(4), uint16(0x0001), false, []byte{1, 2}, int64(0), uint16(0))
	// Child one millisecond before its parent (sub-second regression).
	f.Add(uint64(1_700_000_000), int64(-1), uint16(500), uint16(100), uint8(4), uint16(0x0001), false, []byte{}, int64(0), uint16(0))

	// --- property 3 seeds: one per fork interval, at and around threshold. ---
	// 3000ms (default), 1500ms (Lorentz), 750ms (Maxwell), 450ms (Fermi): the
	// fuzzed child delta lands on each interval so at least one seed puts the
	// child exactly on the interval with no backoff.
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(21), uint16(0x0000), false, []byte{}, int64(0), uint16(0))
	f.Add(uint64(1_700_000_000), int64(1500), uint16(0), uint16(100), uint8(21), uint16(0x0000), false, []byte{}, int64(0), uint16(0))
	f.Add(uint64(1_700_000_000), int64(750), uint16(250), uint16(100), uint8(21), uint16(0x0003), false, []byte{}, int64(0), uint16(0))
	f.Add(uint64(1_700_000_000), int64(450), uint16(999), uint16(100), uint8(21), uint16(0x0007), false, []byte{}, int64(0), uint16(0))

	// --- turn length 1 vs 50, and the first / last block of a turn. ---
	// Snake8 forces TurnLength = 50 (CLAUDE.md §2). Parent number 99 makes the
	// child (100) the first block of a turn; parent 148 makes the child (149)
	// the last. Both are checked at turn length 1 as well, where every block is
	// both the first and the last of its turn.
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(99), uint8(21), uint16(0x0005), true, []byte{3, 1, 4}, int64(1), uint16(0))
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(148), uint8(21), uint16(0x0005), true, []byte{3, 1, 4}, int64(1), uint16(0))
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(99), uint8(21), uint16(0x0005), false, []byte{3, 1, 4}, int64(1), uint16(0))
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(148), uint8(21), uint16(0x0005), false, []byte{3, 1, 4}, int64(1), uint16(0))

	// --- recents seeds: the Planck recent-signer regime. ---
	// snap.Recents is read only under IsPlanck (see timingBaseConfig) and only
	// for an out-of-turn validator, so reaching it needs the probed validator
	// to be out-of-turn AND the Recents window to actually cover it —
	// minerHistoryCheckLen is (len(validators)/2+1)*TurnLength-1, so with
	// TurnLength 1 only the first couple of stamped entries count. These two
	// were chosen by enumeration against the derived validator set, not by
	// guessing, and they cover the two shapes that matter:
	//
	//   nVals 4, child index 0: the probed validator itself signed recently,
	//     so backOffTime takes the `signRecentlyByCounts(val, ...) -> 0`
	//     early return;
	//   nVals 2, child index 1: the probed validator did NOT, but the in-turn
	//     validator did, so `delay = 0` — which is the precondition for the
	//     Lorentz `backOffSteps[idx] == 0 -> return 0` path that property 3's
	//     Lorentz regime then reaches.
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(3), uint16(0x0001), false, []byte{0, 1, 2, 3}, int64(0), uint16(0))
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(1), uint16(0x0100), false, []byte{0, 1, 2, 3}, int64(0), uint16(0))

	// --- property 4 seeds: Delay with leftOver at both ends of its domain. ---
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(0), uint16(0))
	f.Add(uint64(1_700_000_000), int64(3000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(0), uint16(2999))
	// A parent (and therefore child) timestamp in the future, so Delay's wait
	// is genuinely positive and the leftOver subtraction runs. Every other seed
	// uses a past timestamp, where the wait collapses to zero before it.
	f.Add(uint64(4_000_000_000), int64(3000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(0), uint16(1200))
	f.Add(uint64(4_000_000_000), int64(450), uint16(0), uint16(100), uint8(4), uint16(0x0000), true, []byte{}, int64(1), uint16(1))

	// --- property 5 seeds: one per conjunct of isIntentionalDelayMining. ---
	// All four conjuncts true, so the flag is genuinely set: same coinbase
	// (valIdxRaw 0x0000 -> parent index 0, child index 0), parent in-turn
	// (nValRaw bit 7 clear), child 5s after a 3s interval.
	f.Add(uint64(1_700_000_000), int64(5000), uint16(0), uint16(100), uint8(4), uint16(0x0000), false, []byte{}, int64(0), uint16(0))
	// Coinbases differ (0x0100 -> parent index 0, child index 1) and nothing else
	// changes: deleting the `header.Coinbase == parent.Coinbase` conjunct from
	// production turns this input from false to true.
	f.Add(uint64(1_700_000_000), int64(5000), uint16(0), uint16(100), uint8(4), uint16(0x0100), false, []byte{}, int64(0), uint16(0))
	// Parent out-of-turn (nValRaw 0x84 -> bit 7 set, nVals 7) with the same
	// coinbase (0x0202 -> parent and child both index 2) and a delayed child:
	// deleting the `parent.Difficulty.Cmp(diffInTurn) == 0` conjunct turns this
	// input from false to true.
	f.Add(uint64(1_700_000_000), int64(5000), uint16(0), uint16(100), uint8(0x84), uint16(0x0202), false, []byte{}, int64(0), uint16(0))

	// --- degenerate inputs. ---
	f.Add(uint64(0), int64(0), uint16(0), uint16(0), uint8(0), uint16(0x0000), false, []byte(nil), int64(0), uint16(0))
	f.Add(^uint64(0), int64(-1<<62), ^uint16(0), ^uint16(0), ^uint8(0), ^uint16(0), true, []byte{255, 255, 255, 255}, int64(-1_000_000_000), ^uint16(0))

	f.Fuzz(func(t *testing.T,
		parentSecRaw uint64,
		childDeltaMs int64,
		parentMillis uint16,
		numberRaw uint16,
		nValRaw uint8,
		valIdxRaw uint16,
		turnLen50 bool,
		recents []byte,
		futureOffset int64,
		leftOverMs uint16,
	) {
		// ---------- decode ----------
		parentSec := parentSecRaw % timingFuzzMaxParentSeconds
		parentMs := parentSec*1000 + uint64(parentMillis)%1000
		// Parent number 1..65536: block 0 is the genesis dead-end that
		// verifyCascadingFields short-circuits, and Delay/isIntentionalDelayMining
		// both index number-1.
		parentNumber := uint64(numberRaw) + 1
		childNumber := parentNumber + 1

		nVals := 1 + int(nValRaw)%fuzzMaxValidators
		validators := deriveFuzzValidators(parentSecRaw^uint64(numberRaw), nVals)
		turnLength := uint8(1)
		if turnLen50 {
			turnLength = snake8TurnLength
		}
		// Parent and child coinbases are selected independently — the low byte
		// of valIdxRaw picks the parent's, the high byte the child's.
		// Both must vary, and vary independently, or isIntentionalDelayMining's
		// `header.Coinbase == parent.Coinbase` conjunct is true for every input
		// the fuzzer can generate and the oracle silently stops testing it
		// (deleting that conjunct from production then leaves this target green
		// on every seed — measured, before this split existed).
		//
		// A byte per index, not a nibble. valIdxRaw was a uint8 split into two
		// nibbles, so each index ran 0..15 while nVals goes to
		// fuzzMaxValidators (21): for any set larger than 16, validators 16..20
		// could never be drawn as parent or child coinbase. That is not an
		// oracle error — every assertion here holds for the validators it did
		// reach — but it silently narrowed the dimension on exactly the set
		// sizes Chiliz actually runs. A nibble cannot address 21 entries, so the
		// field itself had to widen; the seeds below carry the same parent/child
		// index pairs they did as nibbles.
		parentCoinbase := validators[int(valIdxRaw&0xff)%nVals]
		childCoinbase := validators[int(valIdxRaw>>8)%nVals]
		// Likewise the parent's difficulty: bit 7 of nValRaw decides whether the
		// parent was in-turn. It does not determine nVals — every set size
		// 1..21 is reachable with the bit both set and clear, since
		// 1+nValRaw%21 has about a dozen representatives spread over 0..255 —
		// so this costs no coverage of the validator-count dimension while
		// making the `parent.Difficulty.Cmp(diffInTurn) == 0` conjunct
		// falsifiable.
		parentDifficulty := diffInTurn
		if nValRaw&0x80 != 0 {
			parentDifficulty = diffNoTurn
		}

		// Child millisecond timestamp, clamped into range. The clamp is what
		// lets childDeltaMs be an unrestricted int64 (so "far in the past" and
		// "far in the future" are both reachable) without the test's own
		// arithmetic wrapping.
		var childMs uint64
		switch {
		case childDeltaMs >= 0:
			d := uint64(childDeltaMs)
			if d > timingFuzzMaxParentSeconds*1000 {
				d = timingFuzzMaxParentSeconds * 1000
			}
			childMs = parentMs + d
		default:
			d := uint64(-childDeltaMs)
			if d > parentMs {
				childMs = 0
			} else {
				childMs = parentMs - d
			}
		}

		cfg := timingBaseConfig()
		p := newTimingParlia(cfg)

		parent := timingHeader(parentNumber, common.Hash{}, parentMs, parentCoinbase, parentDifficulty)
		child := timingHeader(childNumber, parent.Hash(), childMs, childCoinbase, diffInTurn)

		chainHeaders := map[common.Hash]*types.Header{parent.Hash(): parent, child.Hash(): child}
		reader := &prepareTestChainReader{config: cfg, genesis: parent, headers: chainHeaders}
		chain := &timingChainReader{prepareTestChainReader: reader}

		// The validator insertion order is deliberately NOT fuzzed here (it was
		// permuteAddresses(validators, parentSecRaw) until the COR-236 review).
		// backOffTime resolves a validator's index through the *sorted*
		// snap.validators(), and inturnValidator() -> selectValidatorRoundRobin
		// sorts too, so insertion order cannot affect anything this target
		// observes — it was a fuzz dimension that could only ever produce the
		// same answer. Insertion-order independence is a real property and it is
		// asserted where it can fail: COR-230's FuzzBackOffTime.
		snap := newFuzzSnapshot(parentNumber, turnLength, validators, validators, recents)
		snap.Hash = parent.Hash()
		// The timing paths never consult Snake8 selection; keep the snapshot's
		// view consistent with the config (which schedules no Snake8) so
		// backOffTime's in-turn question is answered by round-robin and a
		// reviewer is not left wondering which selector ran.
		snap.IsSnake8Fork = false
		p.recentSnaps.Add(snap.Hash, snap)

		// ---------- property 1: the future-block verdict at a fuzzed offset ----------
		//
		// That the tolerance is exactly one second — the reason this file
		// exists — is pinned by TestChilizFutureBlockTolerance, which measures
		// it from the production code and compares it to a literal. That
		// measurement reads no fuzz input, so it ran here only to be re-derived
		// once per execution; what belongs in the loop is the verdict at an
		// arbitrary offset.
		//
		// The offset is an unrestricted int64 on purpose; futureProbeTimestamp
		// saturates the two ends that a header timestamp cannot represent, so
		// this oracle holds over the whole domain without narrowing what the
		// fuzzer may try.
		//
		// One blind spot, recorded rather than papered over: because the offset
		// is an int64 and futureProbeTimestamp saturates at MaxInt64, no probe
		// ever hands verifyHeader a Time above MaxInt64. A merge that rewrote
		// the check with a signed comparison — `int64(header.Time) > now+1` —
		// would accept every header in the top half of the uint64 range, and
		// this target would not see it. Reaching that regime needs a probe
		// timestamp chosen as a uint64 rather than derived from an offset.
		wantRejected := futureOffset > chilizFutureBlockTolerance
		if got := futureBlockRejected(t, p, chain, futureOffset); got != wantRejected {
			t.Fatalf("header at now%+d: rejected=%v, want %v (tolerance %d)", futureOffset, got, wantRejected, chilizFutureBlockTolerance)
		}

		// ---------- properties 2 and 3: the Ramanujan inequality ----------
		//
		// Property 3 is the exactness statement: accept iff
		//   header.MilliTimestamp() >= parent.MilliTimestamp() + interval + backoff.
		// backOffTime is an INPUT here, not the thing under test — COR-230's
		// FuzzBackOffTime owns its internals (see the file header). Whatever it
		// returns, the accept/reject boundary must sit exactly on it.
		//
		// Property 2 falls out of the same inequality: every interval is
		// strictly positive and backoff is non-negative, so a child at or
		// before its parent can never clear the threshold. That is what makes
		// COR-216's "the mint boundary fires exactly once" argument sound —
		// FuzzMintBoundaryFiresOnce (fork_boundary_fuzz_test.go) walks a
		// *strictly increasing* timestamp sequence and concludes a one-time
		// mint cannot fire twice. It may only assume that because consensus
		// refuses non-increasing timestamps here. If this property ever
		// weakens, that one is no longer a statement about real chains.
		//
		// The three sub-second intervals only exist post-Lorentz, and
		// backOffTime's initial back-off is itself Lorentz-gated — on
		// parent.Time, deliberately, because a miner temporarily sets
		// header.Time to now+1. Sweeping 1500/750/450 ms against a config with
		// LorentzTime nil therefore validated (interval, backoff) pairs the
		// chain cannot produce: every probe took defaultInitialBackOffTime, and
		// both lorentzInitialBackOffTime and the `delay == 0 && isParerntLorentz
		// && backOffSteps[idx] == 0 -> return 0` case were unreachable.
		//
		// Each interval is now probed under the fork regime that selects it, and
		// that takes three configs rather than one. Pairing Maxwell's 750 ms and
		// Fermi's 450 ms with a Lorentz-only config would be the same mistake one
		// level down: it makes no behavioural difference today, because
		// backOffTime's only fork gate is Lorentz — but "the regime that selects
		// it" is precisely the claim a reviewer leans on after a BSC sync adds a
		// Maxwell- or Fermi-gated branch, and it has to be true when they do. The
		// forks are cumulative, so each regime keeps the earlier ones active.
		forkTime := parentSec // parent.Time == parentSec, so these are "already active"
		forkEngine := func(maxwell, fermi bool) *Parlia {
			fcfg := timingBaseConfig()
			fcfg.LorentzTime = &forkTime
			if maxwell {
				fcfg.MaxwellTime = &forkTime
			}
			if fermi {
				fcfg.FermiTime = &forkTime
			}
			pe := newTimingParlia(fcfg)
			pe.recentSnaps.Add(snap.Hash, snap)
			return pe
		}

		for _, regime := range []struct {
			interval uint64
			engine   *Parlia
		}{
			{timingDefaultIntervalMs, p},
			{timingLorentzIntervalMs, forkEngine(false, false)},
			{timingMaxwellIntervalMs, forkEngine(true, false)},
			{timingFermiIntervalMs, forkEngine(true, true)},
		} {
			interval, pe := regime.interval, regime.engine
			snap.BlockInterval = interval

			backoff := pe.backOffTime(snap, parent, child, child.Coinbase)
			threshold := parentMs + interval + backoff

			// The fuzzed pair.
			err := pe.blockTimeVerifyForRamanujanFork(snap, child, parent)
			wantOK := childMs >= threshold
			if (err == nil) != wantOK {
				t.Fatalf("interval %d ms: blockTimeVerifyForRamanujanFork(child=%d, parent=%d, backoff=%d) = %v, want ok=%v (threshold %d)",
					interval, childMs, parentMs, backoff, err, wantOK, threshold)
			}

			// Property 2, stated on its own terms so its failure is legible.
			if childMs <= parentMs && err == nil {
				t.Fatalf("interval %d ms: a child at %d ms was accepted against a parent at %d ms — "+
					"timestamps must strictly increase, and COR-216's one-time-mint argument depends on it",
					interval, childMs, parentMs)
			}

			// Off-by-one probe: threshold-1 must be rejected, threshold and
			// threshold+1 accepted.
			probes := []struct {
				ms   uint64
				want bool
			}{
				{threshold - 1, false},
				{threshold, true},
				{threshold + 1, true},
			}
			// threshold is parentMs + interval + backoff and every interval in
			// the table above is at least timingFermiIntervalMs (450), so it can
			// never be 0 and threshold-1 can never wrap. This used to drop the
			// threshold-1 probe silently on that impossible condition — the same
			// shape as the `continue` two blocks down, which the previous commit
			// converted to a hard assertion for the same reason: a guard that
			// cannot fire is either wrong or, if a later change makes it fire,
			// quietly disables the probe it guards. The two dead guards in this
			// loop are now treated the same way.
			if threshold == 0 {
				t.Fatalf("interval %d ms: threshold is 0 (parent %d ms + interval + backoff %d) — "+
					"every block interval is at least %d ms, so this is unreachable; if an interval of 0 was "+
					"introduced, the off-by-one probes must be rebuilt rather than skipped",
					interval, parentMs, backoff, timingFermiIntervalMs)
			}
			// The three probes must share one backoff, or they are not probing
			// a single threshold. This used to be a `continue` justified by "a
			// fork boundary falling inside the 2 ms window", but no such
			// boundary exists: backOffTime's only dependence on header.Time is
			// IsBohr(header.Number, header.Time), the fixture schedules no
			// BohrTime, and the three probes share childNumber and Coinbase —
			// so the guard could never fire and the justification was wrong.
			// It is kept as an assertion rather than deleted: if BohrTime is
			// ever added to timingBaseConfig, a silent `continue` would stop
			// the off-by-one check from running at all, whereas this says so.
			for _, pr := range probes {
				h := timingHeader(childNumber, parent.Hash(), pr.ms, child.Coinbase, diffInTurn)
				if got := pe.backOffTime(snap, parent, h, h.Coinbase); got != backoff {
					t.Fatalf("interval %d ms: backOffTime is not constant across the off-by-one window "+
						"(probe at %d ms gives %d, the threshold was built from %d) — the three probes no longer share a threshold; "+
						"if a header.Time-dependent fork (BohrTime) was added to timingBaseConfig, the probes must be rebuilt, not skipped",
						interval, pr.ms, got, backoff)
				}
			}
			for _, pr := range probes {
				h := timingHeader(childNumber, parent.Hash(), pr.ms, child.Coinbase, diffInTurn)
				got := pe.blockTimeVerifyForRamanujanFork(snap, h, parent) == nil
				if got != pr.want {
					t.Fatalf("interval %d ms: header at %d ms (threshold %d = parent %d + interval %d + backoff %d) accepted=%v, want %v — "+
						"the comparison is off by one",
						interval, pr.ms, threshold, parentMs, interval, backoff, got, pr.want)
				}
			}
		}

		// Pre-Ramanujan there is no timestamp check in Parlia at all: the
		// function is a no-op whatever the timestamps say. Chiliz sets
		// ramanujanBlock: 0 on mainnet, spicy and scoville, so this branch is
		// dead on every live network — it is pinned so a merge that made the
		// check unconditional (retroactively invalidating historical
		// pre-Ramanujan BSC blocks during replay) is visible.
		preRamanujan := timingBaseConfig()
		preRamanujan.RamanujanBlock = nil
		pPre := newTimingParlia(preRamanujan)
		snap.BlockInterval = timingDefaultIntervalMs
		if err := pPre.blockTimeVerifyForRamanujanFork(snap, child, parent); err != nil {
			t.Fatalf("pre-Ramanujan: blockTimeVerifyForRamanujanFork rejected (child=%d, parent=%d) with %v, want nil — the check is Ramanujan-gated",
				childMs, parentMs, err)
		}

		// ---------- property 4: Delay's clamps ----------
		//
		// Delay tells the miner how long to wait before sealing. It must never
		// be negative (a negative wait is a nonsense argument to the worker's
		// timer) and never exceed one block interval — a producer that waited
		// longer than its own slot would hand its turn to the next validator
		// while still claiming in-turn difficulty.
		//
		// Read these for what they are: assertions about Delay's OWN clamping,
		// not about the delay it clamps. Delay caps the top unconditionally
		// (`delay > timeForMining -> delay = timeForMining`) and swallows
		// everything at or below leftOver (`*leftOver >= delay -> return 0`,
		// which absorbs negatives too), and the loop below always passes a
		// leftOver inside the valid domain. So all three bounds hold for *any*
		// value delayForRamanujanFork returns — they pin the clamps and nothing
		// upstream of them. The arithmetic the clamps hide is pinned directly,
		// on delayForRamanujanFork itself, further down.
		//
		// The bound holds for the valid leftOver domain, leftOver < the block
		// interval. Delay's own guard logs an error and returns the raw,
		// possibly negative delay for leftOver at or above the interval; that
		// branch is upstream behaviour for an argument the worker never
		// passes, so it is exercised for absence of panics only.
		for _, interval := range []uint64{
			timingDefaultIntervalMs,
			timingLorentzIntervalMs,
			timingMaxwellIntervalMs,
			timingFermiIntervalMs,
		} {
			snap.BlockInterval = interval
			period := time.Duration(interval) * time.Millisecond
			leftOver := time.Duration(uint64(leftOverMs)%interval) * time.Millisecond

			d := p.Delay(chain, child, &leftOver)
			if d == nil {
				t.Fatalf("interval %d ms: Delay returned nil with the parent snapshot cached", interval)
			}
			if *d < 0 {
				t.Fatalf("interval %d ms: Delay returned %v, which is negative (leftOver %v, child at %d ms)", interval, *d, leftOver, childMs)
			}
			if *d > period {
				t.Fatalf("interval %d ms: Delay returned %v, longer than one block period %v — the producer would sit past its own turn "+
					"(leftOver %v, child at %d ms)", interval, *d, period, leftOver, childMs)
			}
			// The wait plus the reserved finalize time never exceeds the
			// period. leftOver is documented as "the time reserved for block
			// finalize (calculate root, distribute income...)", so this is the
			// statement that makes Delay's arithmetic meaningful: whatever the
			// producer waits, it still has its whole leftOver budget inside its
			// own slot.
			//
			// CORRECTION (kept deliberately, per the wave-two rule on wrong
			// assertions): this check first read `leftOver >= *d implies *d ==
			// 0`, which seed #19 falsified — and the seed was right, the
			// assertion was wrong. Delay subtracts leftOver from the *capped*
			// delay, so a 1500ms period with leftOver=1200ms legitimately
			// returns 300ms, and 1200 >= 300 says nothing at all. The
			// zero-collapse is a property of the pre-subtraction delay, which
			// the caller cannot observe; the sum bound is the observable form.
			if *d+leftOver > period {
				t.Fatalf("interval %d ms: delay %v + leftOver %v exceeds the block period %v — the producer would overrun its slot",
					interval, *d, leftOver, period)
			}

			invalid := period + time.Millisecond
			noPanic(t, "Delay[invalid leftOver]", func() { p.Delay(chain, child, &invalid) })
		}
		snap.BlockInterval = timingDefaultIntervalMs

		// Pre-Ramanujan, delayForRamanujanFork adds a fixed back-off plus a
		// *random* wiggle for an out-of-turn producer. Chiliz sets
		// ramanujanBlock: 0 on mainnet, spicy and scoville, so this branch is
		// dead on live networks; it is pinned because a merge that changed the
		// wiggle arithmetic could otherwise emit a negative or over-long wait
		// unnoticed.
		//
		// The assertion is on delayForRamanujanFork DIRECTLY, not on Delay.
		// Going through Delay cannot pin this: Delay caps at the block interval
		// and returns 0 for anything at or below leftOver, so `0 <= d <= period`
		// holds whatever delayForRamanujanFork returns — including values the
		// wiggle arithmetic should never produce. The bound below is the real
		// one: the increment over the header's own arrival time is 0 in-turn,
		// and lands in [fixedBackOffTimeBeforeFork, fixedBackOffTimeBeforeFork +
		// wiggle) out-of-turn, where wiggle = (len(validators)/2+1) *
		// wiggleTimeBeforeFork.
		//
		// The probe header's timestamp is taken from the wall clock rather than
		// from childMs, because the fuzzed one reaches year 33658: time.Until on
		// a timestamp that far out saturates near the maximum Duration, and the
		// `delay += fixedBackOffTimeBeforeFork + wiggle` that follows then
		// overflows int64 into a large negative. That overflow is real but
		// harmless — Delay's `*leftOver >= delay -> return 0` swallows it, and
		// no Chiliz network can reach the branch at all — and it is exactly what
		// the Delay-mediated bound above is blind to. Probing near `now` keeps
		// the arithmetic in range so the wiggle itself is what is asserted.
		pPreDelay := newTimingParlia(preRamanujan)
		preSnap := newFuzzSnapshot(parentNumber, turnLength, validators, validators, recents)
		preSnap.Hash = parent.Hash()
		preSnap.IsSnake8Fork = false
		pPreDelay.recentSnaps.Add(preSnap.Hash, preSnap)
		preReader := &prepareTestChainReader{config: preRamanujan, genesis: parent, headers: chainHeaders}
		preChain := &timingChainReader{prepareTestChainReader: preReader}
		probeMs := uint64(time.Now().UnixMilli()) + timingDefaultIntervalMs
		wiggle := time.Duration(len(preSnap.Validators)/2+1) * wiggleTimeBeforeFork
		for _, diff := range []*big.Int{diffInTurn, diffNoTurn} {
			h := timingHeader(childNumber, parent.Hash(), probeMs, parentCoinbase, diff)
			chainHeaders[h.Hash()] = h

			// lo/hi bracket the increment delayForRamanujanFork may add on top
			// of "time until the header's own timestamp".
			var lo, hi time.Duration
			if diff.Cmp(diffNoTurn) == 0 {
				lo, hi = fixedBackOffTimeBeforeFork, fixedBackOffTimeBeforeFork+wiggle-1
			}
			// time.Until is re-read inside the call, so sandwich it: the base it
			// used is somewhere in [after, before].
			before := time.Until(time.UnixMilli(int64(probeMs)))
			raw := pPreDelay.delayForRamanujanFork(preSnap, h)
			after := time.Until(time.UnixMilli(int64(probeMs)))
			if raw < after+lo || raw > before+hi {
				t.Fatalf("pre-Ramanujan delayForRamanujanFork(difficulty %v) = %v, want within [%v, %v] "+
					"(base wait %v..%v plus a back-off of [%v, %v] for %d validators) — the wiggle arithmetic changed",
					diff, raw, after+lo, before+hi, after, before, lo, hi, len(preSnap.Validators))
			}

			// And Delay's clamps still hold on top of it, with the fuzzed
			// leftOver.
			leftOver := time.Duration(uint64(leftOverMs)%timingDefaultIntervalMs) * time.Millisecond
			period := time.Duration(timingDefaultIntervalMs) * time.Millisecond
			d := pPreDelay.Delay(preChain, h, &leftOver)
			if d == nil {
				t.Fatalf("pre-Ramanujan: Delay returned nil with the parent snapshot cached")
			}
			if *d < 0 || *d > period {
				t.Fatalf("pre-Ramanujan (difficulty %v): Delay returned %v, want within [0, %v]", diff, *d, period)
			}
		}

		// A header whose parent snapshot is not reachable must yield a nil
		// delay rather than a zero one: the worker treats nil as "cannot
		// schedule" and zero as "seal now", so collapsing the two would make a
		// node with a cold snapshot cache seal immediately.
		orphan := timingHeader(childNumber, common.HexToHash("0xdead"), childMs, childCoinbase, diffInTurn)
		orphanLeftOver := time.Duration(0)
		if d := p.Delay(chain, orphan, &orphanLeftOver); d != nil {
			t.Fatalf("Delay on a header with no reachable parent snapshot returned %v, want nil", *d)
		}
		if _, err := p.isIntentionalDelayMining(chain, orphan); err == nil {
			t.Fatalf("isIntentionalDelayMining on a header with no parent returned no error, want one")
		}

		// The other failure shape: the parent header is present but its own
		// ancestry is not, so the parent lookup succeeds and the snapshot walk
		// is what fails. Both isIntentionalDelayMining and BlockInterval must
		// surface that as an error rather than silently answering from the
		// default interval — a node that reported "not intentional delay"
		// because it could not read its own chain would be hiding a stall.
		stranded := timingHeader(childNumber, common.HexToHash("0xdead"), childMs, childCoinbase, diffInTurn)
		chainHeaders[stranded.Hash()] = stranded
		grandchild := timingHeader(childNumber+1, stranded.Hash(), childMs+timingDefaultIntervalMs, childCoinbase, diffInTurn)
		chainHeaders[grandchild.Hash()] = grandchild
		if _, err := p.isIntentionalDelayMining(chain, grandchild); err == nil {
			t.Fatalf("isIntentionalDelayMining returned no error for a header whose snapshot walk cannot complete")
		}
		if iv, err := p.BlockInterval(chain, grandchild); err == nil || iv != timingDefaultIntervalMs {
			t.Fatalf("BlockInterval on an unreachable snapshot = (%d, %v), want (%d, non-nil error)", iv, err, timingDefaultIntervalMs)
		}

		// BlockInterval's own degenerate inputs: a nil header and the genesis
		// block both fall back to the default interval, and the nil header
		// additionally reports errUnknownBlock. A merge that made either return
		// 0 would leave the Ramanujan threshold with no interval term at all.
		if iv, err := p.BlockInterval(chain, nil); iv != timingDefaultIntervalMs || !errors.Is(err, errUnknownBlock) {
			t.Fatalf("BlockInterval(nil header) = (%d, %v), want (%d, errUnknownBlock)", iv, err, timingDefaultIntervalMs)
		}
		genesisHeader := timingHeader(0, common.Hash{}, parentMs, parentCoinbase, diffInTurn)
		if iv, err := p.BlockInterval(chain, genesisHeader); iv != timingDefaultIntervalMs || err != nil {
			t.Fatalf("BlockInterval(genesis) = (%d, %v), want (%d, nil)", iv, err, timingDefaultIntervalMs)
		}

		// ---------- property 5: isIntentionalDelayMining ----------
		//
		// It flags a producer that held its own in-turn slot open past the
		// block interval. It must never panic, and must be false for any child
		// at or before its parent: the interval is strictly positive, so
		// parent + interval < child cannot hold there. A merge that flipped
		// the comparison to `<=`, or that compared seconds instead of
		// milliseconds, would start flagging ordinary blocks.
		//
		// The oracle below restates production's expression, which is only
		// worth anything if every conjunct is falsifiable by some input. Two of
		// them were not: parent and child were built from one coinbase and both
		// with diffInTurn, so `child.Coinbase == parent.Coinbase` and
		// `parent.Difficulty.Cmp(diffInTurn) == 0` held for every input the
		// fuzzer could generate, and deleting either one from parlia.go left
		// this target green on every seed. The decode step now draws the two
		// coinbases from separate nibbles of valIdxRaw and the parent's
		// difficulty from a bit of nValRaw, so all four conjuncts can fail.
		var (
			intentional bool
			intErr      error
		)
		noPanic(t, "isIntentionalDelayMining", func() {
			intentional, intErr = p.isIntentionalDelayMining(chain, child)
		})
		if intErr != nil {
			t.Fatalf("isIntentionalDelayMining failed with the parent header and snapshot both present: %v", intErr)
		}
		wantIntentional := child.Coinbase == parent.Coinbase &&
			child.Difficulty.Cmp(diffInTurn) == 0 && parent.Difficulty.Cmp(diffInTurn) == 0 &&
			parentMs+timingDefaultIntervalMs < childMs
		if intentional != wantIntentional {
			t.Fatalf("isIntentionalDelayMining = %v, want %v (parent %d ms, child %d ms, interval %d ms)",
				intentional, wantIntentional, parentMs, childMs, timingDefaultIntervalMs)
		}
		if childMs <= parentMs && intentional {
			t.Fatalf("isIntentionalDelayMining flagged a child at %d ms against a parent at %d ms; a non-increasing timestamp is never intentional delay",
				childMs, parentMs)
		}
		// Out-of-turn difficulty is never intentional delay, whatever the
		// timestamps: the flag is about a validator stretching a turn it
		// actually holds. This probe keeps the PARENT's coinbase on purpose, so
		// the coinbase conjunct is satisfied and the child's difficulty is the
		// only thing that can make the answer false.
		noTurnChild := timingHeader(childNumber, parent.Hash(), childMs, parentCoinbase, diffNoTurn)
		chainHeaders[noTurnChild.Hash()] = noTurnChild
		var noTurnFlag bool
		noPanic(t, "isIntentionalDelayMining[noTurn]", func() {
			noTurnFlag, intErr = p.isIntentionalDelayMining(chain, noTurnChild)
		})
		if intErr != nil {
			t.Fatalf("isIntentionalDelayMining(out-of-turn child) failed: %v", intErr)
		}
		if noTurnFlag {
			t.Fatalf("isIntentionalDelayMining flagged an out-of-turn child (difficulty %v)", noTurnChild.Difficulty)
		}
	})
}
