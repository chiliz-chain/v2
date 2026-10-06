package parlia

// COR-230: fuzz the scheduling layer — who is in-turn at a block, what
// difficulty that implies, and how long every other validator waits.
//
// COR-37 was a real chain split in exactly this layer: an upstream
// Prepare/SetExtraData split moved difficulty stamping ahead of the frequency
// refresh, so producers emitted blocks whose Difficulty disagreed with the
// frequency data embedded in the same header. It was caught on a devnet, not by
// a test. TestPrepareDifficultyMatchesEmbeddedFrequencyData now pins the
// ordering, but the selection functions underneath it were at 0% coverage and
// are pure functions of a Snapshot plus a header:
//
//	(*Snapshot).inturnValidator, inturn, selectValidatorRoundRobin
//	(*Snapshot).lastBlockInOneTurn, enoughDistance, indexOfVal
//	calcDifficulty, (*Parlia).backOffTime
//
// COR-209 already fuzzes calcFrequencyRLP and selectValidatorFromFrequencyRLP;
// this file fuzzes the layer composed on top of them. The harness is COR-209's
// (deriveFuzzValidators, newFuzzSnapshot, permuteAddresses, decodeFuzzStakes),
// so a snapshot here is built exactly the way one is built there.

import (
	"bytes"
	"fmt"
	"math/big"
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// What the snapshot's FrequencyRLP holds. The first four are the shapes an
// honest node produces or tolerates; schedForeign is the degraded case COR-173
// gated behind Snake8Fix, kept here so the degradation's shape is pinned rather
// than assumed impossible.
const (
	schedFreqNone    uint8 = iota // nil — inturnValidator takes round-robin
	schedFreqEmpty                // []byte{} — same, via the length check
	schedFreqOwn                  // well-formed, computed from this snapshot's own validators
	schedFreqGarbage              // undecodable — selectValidatorFromFrequencyRLP falls back to round-robin
	schedFreqForeign              // well-formed but naming addresses outside the validator set
	schedFreqCount
)

func schedFreqName(mode uint8) string {
	return [...]string{"none", "empty", "own", "garbage", "foreign"}[mode%schedFreqCount]
}

// schedFreqIsHonest reports whether a mode is one an honest producer can emit
// for this validator set. Under these the selection must name a member of the
// set; schedFreqForeign deliberately does not.
func schedFreqIsHonest(mode uint8) bool {
	return mode%schedFreqCount != schedFreqForeign
}

// schedSnapshot builds the snapshot under test plus a twin whose validators were
// inserted in a permuted order. Everything else about the two is identical, so
// any difference between them is Go map iteration order leaking into consensus.
//
// TurnLength 0 is unrepresentable on chain — selectValidatorRoundRobin divides
// by it — and COR-209's decoder makes the same substitution, so 0 becomes 1.
// pinInTurnRecent makes the in-turn validator itself a recent signer, which is
// the only shape in which Planck zeroes the initial delay — and, with Lorentz,
// the only way into backOffTime's [0, 2, 3, ...] schedule. Left to chance the
// fuzzer reaches it rarely, so it is a fuzz bit rather than a hope.
func schedSnapshot(seed uint64, nRaw uint8, number uint64, turnLength uint8, isSnake8 bool,
	freqMode uint8, stakeBytes, recentBytes []byte, permSeed uint64, pinInTurnRecent bool) (snap, twin *Snapshot, validators []common.Address, freq []byte, mode uint8) {
	n := 1 + int(nRaw)%fuzzMaxValidators
	if turnLength == 0 {
		turnLength = 1
	}
	validators = deriveFuzzValidators(seed, n)
	mode = freqMode % schedFreqCount

	build := func(order []common.Address) *Snapshot {
		s := newFuzzSnapshot(number, turnLength, validators, order, recentBytes)
		s.IsSnake8Fork = isSnake8
		return s
	}

	snap = build(validators)
	freq, mode = schedFrequencyBytes(snap, mode, seed, stakeBytes, n)
	snap.FrequencyRLP = freq

	twin = build(permuteAddresses(validators, permSeed))
	twin.FrequencyRLP = bytes.Clone(freq)

	if pinInTurnRecent {
		// Stamped after the frequency bytes are computed, so the two snapshots
		// still describe the same block; selection never reads Recents, so this
		// moves backoff only.
		inTurn := snap.inturnValidator()
		for _, s := range []*Snapshot{snap, twin} {
			for i := uint64(0); i < uint64(s.TurnLength); i++ {
				if i > s.Number {
					break
				}
				s.Recents[s.Number-i] = inTurn
			}
		}
	}
	return snap, twin, validators, freq, mode
}

// schedLubanSnapshot builds the same validator set the post-Luban way, with vote
// addresses, so newSnapshot populates ValidatorInfo.Index. That is what switches
// indexOfVal from its scan onto its map lookup; Chiliz mainnet is pre-Luban and
// takes the scan, a devnet with vote addresses takes the other one, and the two
// must agree or backoff slots differ between node configurations.
func schedLubanSnapshot(number uint64, turnLength uint8, validators []common.Address, isSnake8 bool, freq []byte) *Snapshot {
	voteAddrs := make([]types.BLSPublicKey, len(validators))
	for i, v := range validators {
		copy(voteAddrs[i][:], v.Bytes())
	}
	s := newSnapshot(nil, nil, number, common.Hash{}, validators, voteAddrs, nil, isSnake8)
	s.TurnLength = turnLength
	s.FrequencyRLP = bytes.Clone(freq)
	return s
}

// schedFrequencyBytes produces the FrequencyRLP for a mode. It returns the mode
// actually realised: calcFrequencyRLP legitimately fails when no validator is
// eligible (every stake zero, or everyone signed recently), and a run that falls
// back must be checked as the mode it became, not the one it asked for.
func schedFrequencyBytes(snap *Snapshot, mode uint8, seed uint64, stakeBytes []byte, n int) ([]byte, uint8) {
	switch mode {
	case schedFreqEmpty:
		return []byte{}, mode
	case schedFreqGarbage:
		// Not valid RLP for []CandidateEntry, and not empty.
		return []byte{0xff, 0xfe, 0xfd}, mode
	case schedFreqOwn:
		stakeList := decodeFuzzStakes(stakeBytes, n)
		stakes := make(map[common.Address]*big.Int, n)
		for i, addr := range snap.validators() {
			stakes[addr] = stakeList[i]
		}
		out, err := snap.calcFrequencyRLP(stakes)
		if err != nil {
			return nil, schedFreqNone
		}
		return out, mode
	case schedFreqForeign:
		// A well-formed list over addresses derived from a different seed, so
		// disjoint from the validator set with overwhelming probability.
		others := deriveFuzzValidators(^seed, n)
		entries := make([]frequencyCandidate, n)
		for i, addr := range others {
			entries[i] = frequencyCandidate{Address: addr, Frequency: big.NewInt(int64(validatorFrequencyPrecision / n))}
		}
		out, err := rlp.EncodeToBytes(entries)
		if err != nil {
			return nil, schedFreqNone
		}
		return out, mode
	default:
		return nil, schedFreqNone
	}
}

// addSchedulingSeeds registers the corpus shared by both targets in this file.
// One seed per branch the issue names: turn length 1 versus the Snake8 50;
// Snake8 on versus off; each FrequencyRLP shape; a single-validator set; 21
// equal stakes; and a set whose in-turn validator has also signed recently.
func addSchedulingSeeds(add func(seed uint64, nRaw uint8, number uint64, turnLength uint8, isSnake8 bool, freqMode uint8, stakeBytes, recentBytes []byte, permSeed uint64)) {
	equal21 := encodeFuzzStakes(seedStakes(equalStakes(21, 1000)...))
	dominant21 := encodeFuzzStakes(seedStakes(dominantStakes(21)...))
	one := encodeFuzzStakes(seedStakes(1000))

	// Round-robin, the pre-Snake8 and number==0 path: turn length 1 and 50.
	add(1, 20, 1000, 1, false, schedFreqNone, equal21, nil, 7)
	add(1, 20, 1000, snake8TurnLength, false, schedFreqNone, equal21, nil, 7)
	// Block 0 short-circuits to round-robin whatever else is set.
	add(1, 20, 0, snake8TurnLength, true, schedFreqOwn, equal21, nil, 7)
	// Snake8 with each frequency shape.
	add(2, 20, 1000, snake8TurnLength, true, schedFreqOwn, equal21, nil, 11)
	add(2, 20, 1000, snake8TurnLength, true, schedFreqOwn, dominant21, nil, 11)
	add(2, 20, 1000, snake8TurnLength, true, schedFreqEmpty, equal21, nil, 11)
	add(2, 20, 1000, snake8TurnLength, true, schedFreqGarbage, equal21, nil, 11)
	add(2, 20, 1000, snake8TurnLength, true, schedFreqForeign, equal21, nil, 11)
	// A single validator: every block is its turn, and validatorNum==1 is a
	// special case inside enoughDistance.
	add(3, 0, 1000, 1, false, schedFreqNone, one, nil, 3)
	add(3, 0, 1000, snake8TurnLength, true, schedFreqOwn, one, nil, 3)
	// The in-turn validator has also signed recently: Planck's backoff path
	// zeroes the initial delay only in this shape.
	add(4, 20, 1000, 1, false, schedFreqNone, equal21, bytes.Repeat([]byte{0}, 32), 5)
	add(4, 20, 1000, snake8TurnLength, true, schedFreqOwn, equal21, bytes.Repeat([]byte{0}, 64), 5)
	// Turn boundaries: (number+1) exactly divisible by the turn length, and one
	// either side, so lastBlockInOneTurn and the round-robin offset both flip.
	add(5, 12, uint64(snake8TurnLength)-1, snake8TurnLength, false, schedFreqNone, equal21, nil, 9)
	add(5, 12, uint64(snake8TurnLength), snake8TurnLength, false, schedFreqNone, equal21, nil, 9)
	add(5, 12, uint64(snake8TurnLength)+1, snake8TurnLength, false, schedFreqNone, equal21, nil, 9)
	// Two validators, where "distinct backoff slots" is tightest.
	add(6, 1, 1000, 1, false, schedFreqNone, encodeFuzzStakes(seedStakes(1000, 1000)), nil, 2)
	// Live mainnet shape: 13 validators, Snake8 on, epoch-sized block number.
	add(7, 12, 35884800, snake8TurnLength, true, schedFreqOwn, equal21, nil, 13)
	// The top of the block-number range, where (Number+1) wraps.
	add(8, 20, ^uint64(0), snake8TurnLength, false, schedFreqNone, equal21, nil, 17)
	add(8, 20, ^uint64(0)-1, 1, true, schedFreqOwn, equal21, nil, 17)
}

// FuzzInturnSelection fuzzes who is in-turn and the difficulty that follows.
//
//  1. Exactly one validator is in-turn per block — for every snapshot whose
//     selection names a member of the set, exactly one member satisfies
//     inturn(). None would mean no validator has priority for the whole turn and
//     the chain falls back to backoff; more than one is a safety failure.
//  2. The selection is a member of Validators and never the zero address, for
//     every FrequencyRLP shape an honest producer can emit.
//  3. Determinism: repeated calls agree, and two snapshots built by inserting
//     the same validators in different map orders agree. Non-determinism here is
//     a chain split, the COR-209 class.
//  4. calcDifficulty(snap, signer) is diffInTurn iff snap.inturn(signer) — the
//     exact relation verifiers judge — and returns a fresh big.Int each time.
//  5. Turn coherence: on the round-robin path the selection is constant across
//     one turn and advances by exactly one validator at the boundary. On the
//     Snake8 frequency path it is seeded per block, so TurnLength does not enter
//     into it at all; both facts are pinned below.
//  6. indexOfVal agrees with the sorted order whether or not ValidatorInfo.Index
//     is populated, and enoughDistance and indexOfVal tolerate an outsider.
func FuzzInturnSelection(f *testing.F) {
	addSchedulingSeeds(func(seed uint64, nRaw uint8, number uint64, turnLength uint8, isSnake8 bool, freqMode uint8, stakeBytes, recentBytes []byte, permSeed uint64) {
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, false)
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, true)
	})

	f.Fuzz(func(t *testing.T, seed uint64, nRaw uint8, number uint64, turnLength uint8, isSnake8 bool, freqMode uint8, stakeBytes, recentBytes []byte, permSeed uint64, pinInTurnRecent bool) {
		snap, twin, validators, freq, mode := schedSnapshot(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, pinInTurnRecent)
		desc := fmt.Sprintf("n=%d number=%d turnLength=%d snake8=%v freq=%s", len(validators), snap.Number, snap.TurnLength, snap.IsSnake8Fork, schedFreqName(mode))

		// Property 3: determinism, over repeats and over insertion order.
		got := snap.inturnValidator()
		if again := snap.inturnValidator(); again != got {
			t.Fatalf("inturnValidator is not idempotent: %s then %s (%s)", got.Hex(), again.Hex(), desc)
		}
		if twinGot := twin.inturnValidator(); twinGot != got {
			t.Fatalf("inturnValidator depends on map insertion order: %s vs %s (%s)", got.Hex(), twinGot.Hex(), desc)
		}

		// Selection reads the validator set, the block number, the turn length
		// and the frequency bytes — never Recents. A dependency on Recents would
		// make two nodes with different recent windows disagree about who is
		// in-turn, which is a split rather than a timing difference.
		noRecents := newFuzzSnapshot(snap.Number, snap.TurnLength, validators, validators, nil)
		noRecents.IsSnake8Fork = snap.IsSnake8Fork
		noRecents.FrequencyRLP = bytes.Clone(freq)
		if bare := noRecents.inturnValidator(); bare != got {
			t.Fatalf("inturnValidator consulted Recents: %s with, %s without (%s)", got.Hex(), bare.Hex(), desc)
		}

		// Properties 1 and 2.
		//
		// The count below is structural, not a safety check, and the difference
		// matters. Snapshot.inturn(v) is literally `inturnValidator() == v`, so
		// the count can only ever be 1 (the selection is a member) or 0 (it is
		// not), and selectedIsMember already decides which — neither arm is
		// reachable today. It is kept as a cheap tripwire for exactly one change:
		// reimplementing inturn independently of inturnValidator, after which
		// "exactly one validator has priority" stops being free and becomes a
		// claim. Do not read it as catching a live two-in-turn safety failure;
		// the assertion doing real work here is selectedIsMember.
		_, selectedIsMember := snap.Validators[got]
		inturnCount := 0
		for v := range snap.Validators {
			if snap.inturn(v) {
				inturnCount++
			}
		}
		switch {
		case selectedIsMember && inturnCount != 1:
			t.Fatalf("selection %s is a member but %d validators are in-turn, want exactly 1 (%s)", got.Hex(), inturnCount, desc)
		case !selectedIsMember && inturnCount != 0:
			t.Fatalf("selection %s is an outsider but %d validators report in-turn (%s)", got.Hex(), inturnCount, desc)
		}
		if schedFreqIsHonest(mode) {
			if !selectedIsMember {
				t.Fatalf("honest frequency shape %s selected the outsider %s (%s)", schedFreqName(mode), got.Hex(), desc)
			}
			if got == (common.Address{}) {
				t.Fatalf("honest frequency shape %s selected the zero address (%s)", schedFreqName(mode), desc)
			}
		} else if selectedIsMember {
			// Not a failure — the foreign list may coincidentally name a member
			// — but then the member relation above governs, which it does.
			t.Logf("foreign frequency list coincidentally selected a member (%s)", desc)
		}

		// Property 4: the difficulty relation, probed over every member, the
		// selection itself and an address outside the set.
		outsider := deriveFuzzValidators(^seed, 1)[0]
		probes := append(append([]common.Address{}, validators...), got, outsider, common.Address{})
		for _, signer := range probes {
			want := diffNoTurn
			if snap.inturn(signer) {
				want = diffInTurn
			}
			d := calcDifficulty(snap, signer)
			if d.Cmp(want) != 0 {
				t.Fatalf("calcDifficulty(%s) = %s, want %s (inturn=%v, %s)", signer.Hex(), d, want, snap.inturn(signer), desc)
			}
			// The returned value must not alias the package-level constants: a
			// caller that adjusts it would otherwise corrupt every later block's
			// difficulty process-wide.
			d.Add(d, big.NewInt(1))
			if diffInTurn.Cmp(big.NewInt(2)) != 0 || diffNoTurn.Cmp(big.NewInt(1)) != 0 {
				t.Fatalf("calcDifficulty returned an alias of diffInTurn/diffNoTurn (now %s/%s)", diffInTurn, diffNoTurn)
			}
		}

		// Property 6: indexOfVal has a fast path (ValidatorInfo.Index, populated
		// only post-Luban) and a scan over the sorted set. They must agree;
		// Chiliz mainnet is pre-Luban, so the scan is the live path and a devnet
		// with vote addresses takes the other one.
		sorted := snap.validators()
		for i, v := range sorted {
			if idx := snap.indexOfVal(v); idx != i {
				t.Fatalf("indexOfVal(%s) = %d, want %d (its position in the sorted set, %s)", v.Hex(), idx, i, desc)
			}
		}
		if idx := snap.indexOfVal(outsider); idx != -1 {
			t.Fatalf("indexOfVal(outsider) = %d, want -1 (%s)", idx, desc)
		}
		// The post-Luban snapshot carries ValidatorInfo.Index, so indexOfVal takes
		// its map lookup instead of the scan. Both must answer the same, and the
		// stored index must not move the in-turn validator either.
		luban := schedLubanSnapshot(snap.Number, snap.TurnLength, validators, snap.IsSnake8Fork, freq)
		for i, v := range luban.validators() {
			if idx := luban.indexOfVal(v); idx != i {
				t.Fatalf("post-Luban indexOfVal(%s) = %d, want %d (%s)", v.Hex(), idx, i, desc)
			}
		}
		if idx := luban.indexOfVal(outsider); idx != -1 {
			t.Fatalf("post-Luban indexOfVal(outsider) = %d, want -1 (%s)", idx, desc)
		}
		if lubanGot := luban.inturnValidator(); lubanGot != got {
			t.Fatalf("stored validator indices moved the selection: %s vs %s (%s)", got.Hex(), lubanGot.Hex(), desc)
		}
		header := &types.Header{Number: new(big.Int).SetUint64(snap.Number), Coinbase: got}
		if !snap.enoughDistance(outsider, header) {
			t.Fatalf("enoughDistance denied an address outside the set (%s)", desc)
		}
		for _, v := range sorted {
			snap.enoughDistance(v, header) // must not panic; the verdict itself is BSC's, not Chiliz's
		}

		// Property 5, round-robin half: the selection is constant across one
		// turn and advances by exactly one validator at the boundary. The turn
		// containing block N is the set of blocks with the same (N+1)/TurnLength.
		checkRoundRobinTurn(t, validators, snap.Number, snap.TurnLength, desc)

		// Property 5, Snake8 half: on the frequency path the seed is Number+1
		// itself, not Number/TurnLength, so the in-turn validator changes every
		// block and TurnLength does not enter into the selection at all. That is
		// current consensus behaviour on both live networks; an upstream or
		// local "fix" that divided by TurnLength here would re-time every block
		// after Snake8 and split the chain.
		// The guard is "the frequency path was actually taken", not merely "bytes
		// are present": undecodable or empty bytes fall back to round-robin,
		// which does move with TurnLength.
		var candidates []frequencyCandidate
		freqPathTaken := snap.Number != 0 && snap.IsSnake8Fork && len(freq) > 0 &&
			rlp.DecodeBytes(freq, &candidates) == nil && len(candidates) > 0
		if freqPathTaken {
			other := newFuzzSnapshot(snap.Number, otherTurnLength(snap.TurnLength), validators, validators, recentBytes)
			other.IsSnake8Fork = true
			other.FrequencyRLP = bytes.Clone(freq)
			if alt := other.inturnValidator(); alt != got {
				t.Fatalf("Snake8 frequency selection moved with TurnLength %d -> %d: %s vs %s (%s)",
					snap.TurnLength, other.TurnLength, got.Hex(), alt.Hex(), desc)
			}
		}

		// A node that restarts must schedule the same block the same way. store()
		// strips FrequencyRLP (COR-174), so the reloaded snapshot takes the
		// round-robin path — but its TurnLength must still be the Snake8 50,
		// because loadSnapshot re-applies the override. Drop that override and a
		// restarted node round-robins on a 1-block turn while its peers use 50,
		// which is a split rather than a slow start.
		//
		// This is the scheduling slice of the round trip only; COR-237 owns the
		// serialization of the struct as a whole.
		checkReloadedSchedule(t, snap, validators, desc)

		// lastBlockInOneTurn is the turn boundary the rest of this depends on.
		if want := (snap.Number+1)%uint64(snap.TurnLength) == 0; snap.lastBlockInOneTurn(snap.Number) != want {
			t.Fatalf("lastBlockInOneTurn(%d) disagrees with (n+1)%%%d (%s)", snap.Number, snap.TurnLength, desc)
		}
	})
}

// checkReloadedSchedule stores the snapshot, loads it back under the same fork
// view and asserts the reloaded copy schedules identically: the same turn length
// and, for the round-robin path both copies then take, the same in-turn
// validator.
func checkReloadedSchedule(t *testing.T, snap *Snapshot, validators []common.Address, desc string) {
	t.Helper()
	db := rawdb.NewMemoryDatabase()
	if err := snap.store(db); err != nil {
		t.Fatalf("storing the snapshot: %v (%s)", err, desc)
	}
	back, err := loadSnapshot(nil, nil, db, snap.Hash, nil, snap.IsSnake8Fork)
	if err != nil {
		t.Fatalf("reloading the snapshot: %v (%s)", err, desc)
	}
	// Under Snake8 the reloaded snapshot must carry turn length 50 whatever the
	// stored blob said. That is not a formality: newSnapshot leaves TurnLength at
	// the default 1 even when isSnake8Fork is set, which is exactly why the
	// override is repeated at loadSnapshot, at both ends of apply and in
	// Parlia.snapshot(). This harness builds snapshots the newSnapshot way, so it
	// holds that unrepaired combination on purpose and checks the repair here.
	switch {
	case snap.IsSnake8Fork && back.TurnLength != snake8TurnLength:
		t.Fatalf("Snake8 is active but the reloaded snapshot has turn length %d, want %d (%s)",
			back.TurnLength, snake8TurnLength, desc)
	case !snap.IsSnake8Fork && back.TurnLength != snap.TurnLength:
		t.Fatalf("reloaded turn length %d, want %d (%s)", back.TurnLength, snap.TurnLength, desc)
	}
	// FrequencyRLP is deliberately not persisted, so both sides must agree on
	// the round-robin answer at the turn length the reload settled on.
	bare := newFuzzSnapshot(snap.Number, back.TurnLength, validators, validators, nil)
	bare.IsSnake8Fork = snap.IsSnake8Fork
	if got, want := back.inturnValidator(), bare.inturnValidator(); got != want {
		t.Fatalf("reloaded snapshot selects %s, a fresh one selects %s (%s)", got.Hex(), want.Hex(), desc)
	}
}

// otherTurnLength returns a turn length different from the one given, so a
// selection can be compared across two of them.
func otherTurnLength(t uint8) uint8 {
	if t == snake8TurnLength {
		return 1
	}
	return snake8TurnLength
}

// checkRoundRobinTurn pins the round-robin turn structure around block number:
// every block of one turn selects the same validator, and the next turn selects
// the next validator in ascending order. Blocks are addressed by turn index
// t = (number+1)/turnLength, whose turn spans t*turnLength-1 .. t*turnLength+turnLength-2.
func checkRoundRobinTurn(t *testing.T, validators []common.Address, number uint64, turnLength uint8, desc string) {
	t.Helper()
	tl := uint64(turnLength)
	at := func(num uint64) common.Address {
		s := newFuzzSnapshot(num, turnLength, validators, validators, nil)
		return s.selectValidatorRoundRobin()
	}

	turn := (number + 1) / tl
	// The first block of this turn is turn*tl-1, which underflows for turn 0;
	// that turn starts at block 0.
	var first uint64
	if turn > 0 {
		first = turn*tl - 1
	}
	last := first + tl - 1
	if turn == 0 {
		last = tl - 2 // blocks 0 .. tl-2 all have (n+1)/tl == 0
	}
	if tl == 1 {
		first, last = number, number // every block is its own turn
	}
	if last < first || last > number+tl { // saturated arithmetic near the top of the range
		return
	}

	want := at(number)
	for _, n := range []uint64{first, number, last} {
		if n > last {
			continue
		}
		if got := at(n); got != want {
			t.Fatalf("round-robin moved inside one turn: block %d selects %s, block %d selects %s (%s)",
				number, want.Hex(), n, got.Hex(), desc)
		}
	}

	// The next turn advances by exactly one position in the sorted set.
	//
	// Not at the very top of the range: selectValidatorRoundRobin offsets by
	// (Number+1), so at block 2^64-1 that wraps to 0 and the offset restarts at
	// the first validator instead of advancing. That is the code's real
	// behaviour, unreachable at any block height a chain can occupy, and
	// asserting otherwise would be asserting a property the code does not have.
	if last >= ^uint64(0)-1 {
		return
	}
	// Ascending order, which is the order the round-robin offset indexes.
	sorted := newFuzzSnapshot(number, turnLength, validators, validators, nil).validators()
	next := at(last + 1)
	wantIdx := -1
	nextIdx := -1
	for i, v := range sorted {
		if v == want {
			wantIdx = i
		}
		if v == next {
			nextIdx = i
		}
	}
	if wantIdx < 0 || nextIdx < 0 {
		t.Fatalf("round-robin selected an address outside the set (%s)", desc)
	}
	if expect := (wantIdx + 1) % len(sorted); nextIdx != expect {
		t.Fatalf("round-robin jumped from index %d to %d at the turn boundary after block %d, want %d (%s)",
			wantIdx, nextIdx, last, expect, desc)
	}
}

// TestBackOffConstantsAreUnchanged pins the two magnitudes every backoff wait is
// built from. They are deliberately literal.
//
// schedExpectedBackOff below recomputes a wait as `delay + steps[idx]*wiggleTime`
// from the same symbols backOffTime uses. That pins the SEED choice — the
// permutation is driven by snap.Number, and substituting any other seed fails —
// but not the SPACING. Nothing else in the package pins it either: parlia_test.go's
// own expectation is computed from the same constants, so mutating wiggleTime
// 1000 -> 1500 leaves the entire consensus/parlia suite green.
//
// That matters because upstream tunes this layer — lorentzInitialBackOffTime
// arrived from BSC that way — and because CLAUDE.md section 3 records the
// ForBSC-suffixed-constant trap, where reaching for the wrong name compiles and
// is silently wrong. A retune re-spaces every Chiliz validator's release across
// the fleet, and without this test nothing would notice.
func TestBackOffConstantsAreUnchanged(t *testing.T) {
	if defaultInitialBackOffTime != 1000 {
		t.Fatalf("defaultInitialBackOffTime = %d ms, want 1000", defaultInitialBackOffTime)
	}
	if wiggleTime != 1000 {
		t.Fatalf("wiggleTime = %d ms, want 1000", wiggleTime)
	}
	if lorentzInitialBackOffTime != 2000 {
		t.Fatalf("lorentzInitialBackOffTime = %d ms, want 2000", lorentzInitialBackOffTime)
	}
}

// FuzzBackOffTime fuzzes how long each out-of-turn validator waits.
//
//  1. backOffTime never panics, for any validator including one outside the set.
//  2. It is 0 for the in-turn validator.
//  3. It is bounded by the initial delay plus one wiggle per validator.
//  4. Distinct eligible validators get distinct waits. Two sharing a slot would
//     release at the same instant and collide every turn.
//  5. It does not depend on map insertion order.
//  6. Without Bohr — which no Chiliz network schedules — the shuffle is seeded
//     from snap.Number, so the wait does not move with header.Number.
func FuzzBackOffTime(f *testing.F) {
	addSchedulingSeeds(func(seed uint64, nRaw uint8, number uint64, turnLength uint8, isSnake8 bool, freqMode uint8, stakeBytes, recentBytes []byte, permSeed uint64) {
		// The live Chiliz fork set: Planck on, Bohr and Lorentz off.
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, false, true, false, false)
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, true, true, false, false)
	})
	// The same corpus under the fork combinations Chiliz does not schedule but
	// upstream keeps alive, so a merge that reorders them is still covered.
	addSchedulingSeeds(func(seed uint64, nRaw uint8, number uint64, turnLength uint8, isSnake8 bool, freqMode uint8, stakeBytes, recentBytes []byte, permSeed uint64) {
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, true, false, false, false)
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, true, true, true, false)
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, true, true, true, true)
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, true, true, false, true)
		// The remaining three of the eight (planck, bohr, lorentz) combinations.
		// (planck=false, lorentz=true) is a genuinely distinct path: the Lorentz
		// initial delay applied over the UNFILTERED validator list, with no
		// recent-signer early return and no delay==0 short circuit. `go test`
		// without -fuzz runs only the corpus, so an unseeded combination is one
		// ordinary CI never executes. No chain schedules Lorentz without Planck,
		// so these are merge tripwires rather than live paths.
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, true, false, true, false)
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, true, false, false, true)
		f.Add(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, true, false, true, true)
	})

	f.Fuzz(func(t *testing.T, seed uint64, nRaw uint8, number uint64, turnLength uint8, isSnake8 bool, freqMode uint8, stakeBytes, recentBytes []byte, permSeed uint64, pinInTurnRecent, planck, bohr, lorentz bool) {
		snap, twin, validators, _, mode := schedSnapshot(seed, nRaw, number, turnLength, isSnake8, freqMode, stakeBytes, recentBytes, permSeed, pinInTurnRecent)
		p := &Parlia{chainConfig: schedChainConfig(planck, bohr, lorentz)}
		parent, header, ok := schedHeaders(snap.Number)
		if !ok {
			t.Skip("snapshot at MaxUint64 has no representable child header")
		}
		desc := fmt.Sprintf("n=%d number=%d turnLength=%d snake8=%v freq=%s planck=%v bohr=%v lorentz=%v",
			len(validators), snap.Number, snap.TurnLength, snap.IsSnake8Fork, schedFreqName(mode), planck, bohr, lorentz)

		inTurn := snap.inturnValidator()
		counts := snap.countRecents()
		bound := lorentzInitialBackOffTime + uint64(len(validators))*wiggleTime

		waits := make(map[common.Address]uint64, len(validators))
		eligible := make([]common.Address, 0, len(validators))
		for _, v := range snap.validators() {
			w := p.backOffTime(snap, parent, header, v)
			waits[v] = w

			// Property 3.
			if w > bound {
				t.Fatalf("backOffTime(%s) = %d, above the %d bound (%s)", v.Hex(), w, bound, desc)
			}
			// Property 2.
			if v == inTurn && w != 0 {
				t.Fatalf("the in-turn validator %s waits %dms, want 0 (%s)", v.Hex(), w, desc)
			}
			// Property 5.
			if tw := p.backOffTime(twin, parent, header, v); tw != w {
				t.Fatalf("backOffTime(%s) depends on map insertion order: %d vs %d (%s)", v.Hex(), w, tw, desc)
			}
			// Property 6, the strong half: on the live Chiliz fork set the whole
			// wait is recomputed from snap.Number by an independent oracle, so a
			// different seed choice - snap.Number+1, snap.Number/TurnLength, a
			// constant - fails here even though it would leave the stability
			// check below satisfied.
			if planck && !bohr && !lorentz {
				if want := schedExpectedBackOff(snap, v); w != want {
					t.Fatalf("backOffTime(%s) = %d, want %d recomputed from snap.Number=%d (%s)",
						v.Hex(), w, want, snap.Number, desc)
				}
			}
			// A validator that is neither in-turn nor barred by Planck's
			// recent-signer rule is competing for this block, and its wait is
			// the slot it must not share.
			if v != inTurn && !(planck && snap.signRecentlyByCounts(v, counts)) {
				eligible = append(eligible, v)
			}
		}

		// Property 4: no two competing validators release at the same instant.
		// backOffSteps is a permutation over the filtered set, so distinct
		// indices must give distinct waits; a collision means the permutation
		// stopped being one, or the filter stopped matching the index lookup.
		seen := make(map[uint64]common.Address, len(eligible))
		for _, v := range eligible {
			if other, clash := seen[waits[v]]; clash {
				t.Fatalf("%s and %s both wait %dms (%d eligible of %d, %s)",
					other.Hex(), v.Hex(), waits[v], len(eligible), len(validators), desc)
			}
			seen[waits[v]] = v
		}

		// Property 1: an address outside the set is "not authorized" and waits 0.
		outsider := deriveFuzzValidators(^seed, 1)[0]
		if w := p.backOffTime(snap, parent, header, outsider); w != 0 {
			t.Fatalf("an address outside the validator set waits %dms, want 0 (%s)", w, desc)
		}

		// Property 6: without Bohr the shuffle is seeded from snap.Number, so
		// moving header.Number must not move anyone's wait. Bohr seeds from
		// header.Number/TurnLength instead and is expected to move; no Chiliz
		// network schedules it, so an ungated change to this seed would re-time
		// every live validator's release.
		if !bohr {
			far := &types.Header{
				Number:   new(big.Int).SetUint64(header.Number.Uint64() / 2),
				Time:     header.Time,
				Coinbase: header.Coinbase,
			}
			for _, v := range validators {
				if w, fw := waits[v], p.backOffTime(snap, parent, far, v); w != fw {
					t.Fatalf("pre-Bohr backOffTime(%s) moved with header.Number: %d vs %d (%s)", v.Hex(), w, fw, desc)
				}
			}
		}
	})
}

// schedChainConfig returns a Parlia config with the three forks backOffTime
// branches on. Planck is block-based; Bohr and Lorentz are timestamp forks that
// additionally require London, which is why it is set whenever either is.
func schedChainConfig(planck, bohr, lorentz bool) *params.ChainConfig {
	zero := func() *uint64 { z := uint64(0); return &z }
	cfg := &params.ChainConfig{
		ChainID: big.NewInt(88888),
		Parlia:  &params.ParliaConfig{Epoch: 200, Period: 3},
	}
	if planck {
		cfg.PlanckBlock = big.NewInt(0)
	}
	if bohr || lorentz {
		cfg.LondonBlock = big.NewInt(0)
	}
	if bohr {
		cfg.BohrTime = zero()
	}
	if lorentz {
		cfg.LorentzTime = zero()
	}
	return cfg
}

// schedHeaders returns the (parent, header) pair backOffTime reads: it takes the
// fork decisions from parent.Number/parent.Time for Lorentz and from
// header.Number/header.Time for Planck and Bohr.
// It mirrors the production relation: every caller takes the snapshot at
// header.Number-1 (parlia.go, `p.snapshot(chain, number-1, header.ParentHash, …)`
// then `blockTimeVerifyForRamanujanFork(snap, header, parent)`), so the parent
// sits AT the snapshot height and the header one above it. Building the pair a
// block lower would exercise backOffTime with a combination no node can produce
// and would hide off-by-one changes in the Planck and Bohr fork checks or in the
// Bohr shuffle seed, which divides header.Number.
//
// A snapshot at MaxUint64 has no representable child; ok is false there and the
// caller skips the backoff checks rather than wrapping to block 0.
func schedHeaders(snapNumber uint64) (parent, header *types.Header, ok bool) {
	if snapNumber == ^uint64(0) {
		return nil, nil, false
	}
	parent = &types.Header{Number: new(big.Int).SetUint64(snapNumber), Time: 1_700_000_000}
	header = &types.Header{
		Number:   new(big.Int).SetUint64(snapNumber + 1),
		Time:     parent.Time + 3,
		Coinbase: common.Address{},
	}
	return parent, header, true
}

// schedExpectedBackOff recomputes backOffTime independently for the live Chiliz
// fork set (Planck on, Bohr and Lorentz unscheduled), so the shuffle SEED is
// pinned rather than merely its stability.
//
// Asserting only "the wait does not move with header.Number" is too weak: the
// production seed could be changed to snap.Number+1, or to
// snap.Number/TurnLength, and every such assertion would still hold while every
// live validator was re-timed. This oracle names snap.Number, so any other seed
// choice fails.
//
// It mirrors parlia.go's algorithm deliberately — the point is the seed, and the
// shuffle itself is math/rand's, not ours.
func schedExpectedBackOff(snap *Snapshot, val common.Address) uint64 {
	if snap.inturn(val) {
		return 0
	}
	counts := snap.countRecents()
	if snap.signRecentlyByCounts(val, counts) {
		return 0
	}
	delay := defaultInitialBackOffTime
	if snap.signRecentlyByCounts(snap.inturnValidator(), counts) {
		delay = 0
	}
	// Planck filters recently-signed validators; without Bohr the in-turn
	// validator stays in the list.
	filtered := make([]common.Address, 0, len(snap.Validators))
	for _, addr := range snap.validators() {
		if snap.signRecentlyByCounts(addr, counts) {
			continue
		}
		filtered = append(filtered, addr)
	}
	idx := -1
	for i, addr := range filtered {
		if addr == val {
			idx = i
		}
	}
	if idx < 0 {
		return 0
	}
	n := len(filtered)
	steps := make([]uint64, 0, n)
	for i := uint64(0); i < uint64(n); i++ {
		steps = append(steps, i)
	}
	r := rand.New(rand.NewSource(int64(snap.Number))) //nolint:gosec // mirrors consensus
	r.Shuffle(n, func(i, j int) { steps[i], steps[j] = steps[j], steps[i] })
	return delay + steps[idx]*wiggleTime
}
