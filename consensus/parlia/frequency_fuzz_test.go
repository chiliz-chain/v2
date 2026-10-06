package parlia

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// COR-209: property-based coverage of the Snake8 frequency data.
//
// Post-Snake8Fix (COR-173) the frequency bytes embedded in every header are
// consensus-verified: each verifier recomputes them from parent state in
// Finalize (verifySnake8FrequencyData) and rejects a byte mismatch. Any
// non-determinism in (*Snapshot).calcFrequencyRLP — in particular anything that
// depends on Go map iteration order — is therefore a chain split, not a
// cosmetic bug. FuzzCalcFrequencyRLP pins determinism plus the arithmetic
// invariants of the normalization, and FuzzSelectValidatorFromFrequencyRLP pins
// the selection side: no panic, no zero in-turn validator, and a round-robin
// fallback whenever the bytes cannot be used.

// fuzzMaxValidators bounds the fuzzed validator set (Chiliz runs 2k+1 <= 21).
const fuzzMaxValidators = 21

// fuzzMaxRecents bounds how many Recents entries a fuzz input may stamp.
const fuzzMaxRecents = 64

// frequencyOrderRepeats is how many times each map-order view of an input is
// re-evaluated when checking that calcFrequencyRLP is order independent. See
// the comment in FuzzCalcFrequencyRLP for why both the repeats and the several
// views are needed; `go test` without -fuzz runs only the seed corpus, so this
// per-input detection power is all a plain CI run has.
const frequencyOrderRepeats = 4

// frequencyCandidate mirrors the unexported CandidateEntry shape used by both
// calcFrequencyRLP and selectValidatorFromFrequencyRLP, so the tests decode
// with exactly the RLP layout consensus uses.
type frequencyCandidate struct {
	Address   common.Address
	Frequency *big.Int
}

// deriveFuzzValidators derives n distinct, non-zero validator addresses from a
// seed. Distinctness matters: a duplicate would silently shrink the map-based
// validator set and skew every count below.
func deriveFuzzValidators(seed uint64, n int) []common.Address {
	out := make([]common.Address, 0, n)
	seen := make(map[common.Address]bool, n)
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], seed)
	for ctr := uint64(0); len(out) < n; ctr++ {
		binary.LittleEndian.PutUint64(buf[8:], ctr)
		h := sha256.Sum256(buf[:])
		addr := common.BytesToAddress(h[12:])
		if addr == (common.Address{}) || seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, addr)
	}
	return out
}

// encodeFuzzStakes serialises stakes for the fuzz corpus in the format
// decodeFuzzStakes reads: one length byte (0..32) then that many big-endian
// magnitude bytes per validator. A length of 0 encodes a zero stake.
//
// It panics above 32 bytes rather than truncating. decodeFuzzStakes reduces the
// length byte mod 33, so a 33-byte magnitude would encode as 0 — that validator
// silently gets a zero stake and its 33 payload bytes are then re-read as length
// bytes for every validator after it, turning the whole seed into something
// other than what it says. 32 bytes is the full uint256 range, so any seed that
// trips this is a mistake in the seed, not a limit worth working around.
func encodeFuzzStakes(stakes []*big.Int) []byte {
	var out []byte
	for i, s := range stakes {
		b := s.Bytes()
		if len(b) > 32 {
			panic(fmt.Sprintf("encodeFuzzStakes: stake %d needs %d bytes, the wire format holds 32", i, len(b)))
		}
		out = append(out, byte(len(b)))
		out = append(out, b...)
	}
	return out
}

// decodeFuzzStakes reads one stake per validator from arbitrary fuzz bytes.
// Length bytes are reduced mod 33 so any input parses; running out of bytes
// yields zero stakes for the remaining validators. Every returned value is
// non-nil, matching the production precondition (parlia.go seeds missing
// stakes with zero, never nil, because calcFrequencyRLP dereferences them).
func decodeFuzzStakes(data []byte, n int) []*big.Int {
	stakes := make([]*big.Int, n)
	pos := 0
	for i := 0; i < n; i++ {
		stakes[i] = new(big.Int)
		if pos >= len(data) {
			continue
		}
		l := int(data[pos] % 33)
		pos++
		end := pos + l
		if end > len(data) {
			end = len(data)
		}
		stakes[i].SetBytes(data[pos:end])
		pos = end
	}
	return stakes
}

// xorshift64 is a tiny deterministic PRNG for permuting insertion order; it
// keeps the test free of math/rand seeding semantics.
type xorshift64 uint64

func (x *xorshift64) next() uint64 {
	v := uint64(*x)
	if v == 0 {
		v = 0x9E3779B97F4A7C15
	}
	v ^= v << 13
	v ^= v >> 7
	v ^= v << 17
	*x = xorshift64(v)
	return v
}

// permuteAddresses returns a shuffled copy of addrs driven by seed.
func permuteAddresses(addrs []common.Address, seed uint64) []common.Address {
	out := make([]common.Address, len(addrs))
	copy(out, addrs)
	rng := xorshift64(seed)
	for i := len(out) - 1; i > 0; i-- {
		j := int(rng.next() % uint64(i+1))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// newFuzzSnapshot builds a Snapshot the way the existing snapshot tests do
// (newSnapshot with nil chain plumbing), inserting validators in the order
// given, then stamps TurnLength and the fuzzed Recents. Recents entry i is
// stamped at block number-i so that, depending on TurnLength and
// minerHistoryCheckLen, some validators end up excluded by SignRecently. The
// recent signer is resolved through canonical (the derivation order), so two
// snapshots built from different insertion orders carry identical Recents.
func newFuzzSnapshot(number uint64, turnLength uint8, canonical, order []common.Address, recentBytes []byte) *Snapshot {
	snap := newSnapshot(nil, nil, number, common.Hash{}, order, nil, nil, true)
	snap.TurnLength = turnLength
	for i, b := range recentBytes {
		if i >= fuzzMaxRecents || uint64(i) > number {
			break
		}
		snap.Recents[number-uint64(i)] = canonical[int(b)%len(canonical)]
	}
	return snap
}

// fuzzCase is one fully-decoded FuzzCalcFrequencyRLP input.
type fuzzCase struct {
	validators []common.Address
	stakes     map[common.Address]*big.Int
	number     uint64
	turnLength uint8
	recents    []byte
	permSeed   uint64
}

func decodeFuzzCase(seed uint64, nRaw uint8, stakeBytes, recentBytes []byte, permSeed, number uint64, turnLength uint8) fuzzCase {
	n := 1 + int(nRaw)%fuzzMaxValidators
	if turnLength == 0 {
		turnLength = 1 // TurnLength 0 is unrepresentable on chain (round-robin divides by it)
	}
	validators := deriveFuzzValidators(seed, n)
	stakeList := decodeFuzzStakes(stakeBytes, n)
	stakes := make(map[common.Address]*big.Int, n)
	for i, addr := range validators {
		stakes[addr] = stakeList[i]
	}
	return fuzzCase{
		validators: validators,
		stakes:     stakes,
		number:     number,
		turnLength: turnLength,
		recents:    recentBytes,
		permSeed:   permSeed,
	}
}

// seedStakes converts whole-CHZ amounts into wei stakes.
func seedStakes(chz ...int64) []*big.Int {
	d := big.NewInt(1e18)
	out := make([]*big.Int, len(chz))
	for i, v := range chz {
		out[i] = new(big.Int).Mul(big.NewInt(v), d)
	}
	return out
}

// selectionAlgorithmStakes is the distribution from TestValidatorSelectionAlgorithm.
var selectionAlgorithmStakes = []int64{400, 300, 50, 50, 50, 50, 40, 34, 18, 8}

// equalStakes returns n equal whole-CHZ stakes.
func equalStakes(n int, chz int64) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = chz
	}
	return out
}

// dominantStakes returns n stakes where the first holds exactly 99.9% of the
// total: 999,000 units against 1,000 units shared equally by the others (the
// share is exact for every n whose n-1 divides 1,000, which covers the 21 the
// seeds use; otherwise the remainder goes to the last minor validator so the
// total, and hence the 99.9%, is still exact).
func dominantStakes(n int) []int64 {
	out := make([]int64, n)
	out[0] = 999_000
	if n == 1 {
		return out
	}
	minor := int64(1_000)
	each := minor / int64(n-1)
	for i := 1; i < n; i++ {
		out[i] = each
	}
	out[n-1] += minor - each*int64(n-1)
	return out
}

// TestDominantStakesShare pins the fixture's documented share, since the
// extreme-skew corpus case depends on it.
func TestDominantStakesShare(t *testing.T) {
	for _, n := range []int{2, 3, 11, 21} {
		st := dominantStakes(n)
		var total int64
		for _, v := range st {
			total += v
		}
		if got := float64(st[0]) / float64(total); got < 0.9989 || got > 0.9991 {
			t.Fatalf("dominantStakes(%d): first validator holds %.4f%%, want 99.9%%", n, got*100)
		}
	}
}

func addCalcSeeds(f *testing.F) {
	// TestValidatorSelectionAlgorithm's distribution, no recents.
	f.Add(uint64(1), uint8(9), encodeFuzzStakes(seedStakes(selectionAlgorithmStakes...)), []byte{}, uint64(7), uint64(1000), uint8(1))
	// Same distribution under the Snake8 turn length with a few recent signers.
	f.Add(uint64(2), uint8(9), encodeFuzzStakes(seedStakes(selectionAlgorithmStakes...)), []byte{0, 1, 2, 2, 5}, uint64(11), uint64(5000), uint8(50))
	// A single validator.
	f.Add(uint64(3), uint8(0), encodeFuzzStakes(seedStakes(1)), []byte{}, uint64(0), uint64(1), uint8(1))
	// 21 validators with equal stakes.
	f.Add(uint64(4), uint8(20), encodeFuzzStakes(seedStakes(equalStakes(21, 1000)...)), []byte{}, uint64(3), uint64(28800), uint8(50))
	// One validator holding 99.9% of the stake, 20 others sharing 0.1%.
	f.Add(uint64(5), uint8(20), encodeFuzzStakes(seedStakes(dominantStakes(21)...)), []byte{}, uint64(5), uint64(28801), uint8(50))
	// Sub-1e18 dust next to a whale: dust validators still count as candidates
	// (non-zero raw stake) but contribute 0 to the total and are lifted to minFreq.
	dust := []*big.Int{big.NewInt(1e18), big.NewInt(1), big.NewInt(1e17), big.NewInt(999_999_999_999_999_999)}
	f.Add(uint64(6), uint8(3), encodeFuzzStakes(dust), []byte{}, uint64(9), uint64(42), uint8(1))
	// Huge stakes (2^255-ish) to exercise big.Int paths well beyond int64.
	huge := new(big.Int).Lsh(big.NewInt(1), 255)
	f.Add(uint64(7), uint8(4), encodeFuzzStakes([]*big.Int{huge, huge, big.NewInt(1e18), new(big.Int).Sub(huge, big.NewInt(1)), big.NewInt(3e18)}), []byte{}, uint64(13), uint64(77), uint8(1))
	// All zero stakes -> "no eligible validators".
	f.Add(uint64(8), uint8(4), encodeFuzzStakes(seedStakes(0, 0, 0, 0, 0)), []byte{}, uint64(0), uint64(10), uint8(1))
	// All dust -> candidates exist but total is zero -> error, never an empty list.
	f.Add(uint64(9), uint8(2), encodeFuzzStakes([]*big.Int{big.NewInt(1), big.NewInt(5), big.NewInt(1e17)}), []byte{}, uint64(0), uint64(10), uint8(1))
	// Everyone recently signed (TurnLength 1, 3 validators, 3 recents at the head).
	f.Add(uint64(10), uint8(2), encodeFuzzStakes(seedStakes(10, 20, 30)), []byte{0, 1, 2}, uint64(0), uint64(100), uint8(1))
	// The SignRecently exclusion at the PRODUCTION turn length. Every other seed
	// passes turnLength 1 or stamps a handful of recents, and signRecentlyByCounts
	// needs seenTimes >= TurnLength — so at the Snake8 TurnLength of 50 none of
	// them excludes anybody, and the eligibility filter (a consensus input to
	// calcFrequencyRLP) was pinned only at turnLength 1. Plain CI runs execute
	// seeds only, so that gap was the coverage.
	//
	// 64 recents all resolving to canonical[0] (fuzzMaxRecents is 64) puts
	// seenTimes at 64 against TurnLength 50: the first validator is excluded, and
	// seenTimes > TurnLength also reaches the "produce more blocks than expected"
	// branch, which no seed reached before. number must exceed the recent count so
	// all 64 get stamped.
	f.Add(uint64(11), uint8(4), encodeFuzzStakes(seedStakes(equalStakes(5, 1000)...)), bytes.Repeat([]byte{0}, fuzzMaxRecents), uint64(17), uint64(28800), uint8(50))
}

// FuzzCalcFrequencyRLP pins the consensus-critical properties of the frequency
// bytes produced by calcFrequencyRLP:
//
//  1. Order independence: the same validator set and stakes, inserted into the
//     maps in several different orders and computed repeatedly, yield
//     byte-identical RLP — and agree on the error path too.
//  2. Every emitted frequency is >= minFreq = precision / (2*N), N being the
//     number of eligible candidates (non-zero raw stake, not recently signed).
//  3. The frequencies sum to precision minus a truncation deficit of at most
//     N-1 (each of the <=N floor divisions in the proportional adjustment loses
//     strictly less than one unit), i.e. sum in [precision-N+1, precision].
//  4. Entries are strictly ascending by address, cover exactly the eligible
//     set, and re-encoding the decoded entries reproduces the bytes.
//  5. No eligible candidate, or a zero total, is an error — never an empty but
//     well-formed list that a verifier would accept.
func FuzzCalcFrequencyRLP(f *testing.F) {
	addCalcSeeds(f)

	f.Fuzz(func(t *testing.T, seed uint64, nRaw uint8, stakeBytes, recentBytes []byte, permSeed, number uint64, turnLength uint8) {
		tc := decodeFuzzCase(seed, nRaw, stakeBytes, recentBytes, permSeed, number, turnLength)

		// Build the "reference" snapshot in derivation order plus two more whose
		// maps were filled in different orders, and evaluate each of them
		// repeatedly. Both halves are needed. Go randomises map iteration, but
		// for a map this small it only picks a rotation of one fixed slot
		// layout, so a single snapshot yields just len(validators) distinct
		// orders and heavily favours one of them (measured over 20k iterations:
		// 88% for two validators, 19% for 21) — repetition alone is a weak
		// detector for small sets. Filling the map in a different order changes
		// the layout, and for five or more validators the resulting rotation set
		// is disjoint from the reference's, so the extra views widen the orders
		// covered rather than resampling the same ones.
		refSnap := newFuzzSnapshot(tc.number, tc.turnLength, tc.validators, tc.validators, tc.recents)
		type freqView struct {
			name   string
			snap   *Snapshot
			stakes map[common.Address]*big.Int
		}
		views := []freqView{{"reference", refSnap, tc.stakes}}
		// Derive the second seed multiplicatively, not by XOR. xorshift64.next
		// substitutes 0x9E3779B97F4A7C15 for a zero state, so seed 0 and seed
		// 0^0x9E3779B97F4A7C15 produce bit-identical streams — and 0 is one of the
		// values a fuzzer supplies most often, so the three "distinct map layouts"
		// collapsed to two for a large share of inputs (verified for n=2,5,10,21).
		// x*k+1 is injective on uint64 for odd k, and maps 0 to 1 rather than back
		// onto the substituted state.
		for _, ps := range []uint64{tc.permSeed, tc.permSeed*0x9E3779B97F4A7C15 + 1} {
			permOrder := permuteAddresses(tc.validators, ps)
			permStakes := make(map[common.Address]*big.Int, len(permOrder))
			for _, addr := range permOrder {
				permStakes[addr] = new(big.Int).Set(tc.stakes[addr])
			}
			views = append(views, freqView{
				name:   fmt.Sprintf("permuted(seed %d)", ps),
				snap:   newFuzzSnapshot(tc.number, tc.turnLength, tc.validators, permOrder, tc.recents),
				stakes: permStakes,
			})
		}

		// Eligibility as the code defines it: non-zero raw stake and not
		// recently signed. Dust below 1e18 is eligible (it contributes 0 to the
		// total and is lifted to minFreq); a zero total with candidates is the
		// "all dust" error path.
		eligible := make(map[common.Address]bool)
		total := new(big.Int)
		decimals := big.NewInt(1e18)
		for _, addr := range tc.validators {
			if tc.stakes[addr].Sign() == 0 || refSnap.SignRecently(addr) {
				continue
			}
			eligible[addr] = true
			total.Add(total, new(big.Int).Div(tc.stakes[addr], decimals))
		}
		wantErr := len(eligible) == 0 || total.Sign() == 0

		ref, err := refSnap.calcFrequencyRLP(tc.stakes)
		if wantErr {
			// Property 5.
			if err == nil {
				t.Fatalf("expected error for %d eligible candidates / total %s, got %d bytes %x", len(eligible), total, len(ref), ref)
			}
			// Every view must fail on every evaluation — an error on one node and
			// bytes on another is the same split as differing bytes, and the
			// error path is reached from the same map walk as the success path.
			for i := 0; i < frequencyOrderRepeats; i++ {
				for _, v := range views {
					if _, err2 := v.snap.calcFrequencyRLP(v.stakes); err2 == nil {
						t.Fatalf("%s view succeeded on repeat %d where the reference errored: %v", v.name, i, err)
					}
				}
			}
			return
		}
		if err != nil {
			t.Fatalf("unexpected error with %d eligible candidates (total %s): %v", len(eligible), total, err)
		}

		// Property 1: order independence and repeatability.
		for i := 0; i < frequencyOrderRepeats; i++ {
			for _, v := range views {
				again, err := v.snap.calcFrequencyRLP(v.stakes)
				if err != nil {
					t.Fatalf("%s view errored on repeat %d: %v", v.name, i, err)
				}
				if !bytes.Equal(ref, again) {
					t.Fatalf("insertion-order dependent output (%s view, repeat %d):\n ref %x\n got %x", v.name, i, ref, again)
				}
			}
		}

		// Property 4: decode with the consensus shape, check ordering and
		// coverage, and round-trip.
		var entries []frequencyCandidate
		if err := rlp.DecodeBytes(ref, &entries); err != nil {
			t.Fatalf("output does not decode as []CandidateEntry: %v (%x)", err, ref)
		}
		if len(entries) != len(eligible) {
			t.Fatalf("got %d entries, want %d eligible candidates", len(entries), len(eligible))
		}
		for i, e := range entries {
			if !eligible[e.Address] {
				t.Fatalf("entry %d %s is not an eligible candidate", i, e.Address.Hex())
			}
			if i > 0 && bytes.Compare(entries[i-1].Address.Bytes(), e.Address.Bytes()) >= 0 {
				t.Fatalf("entries not strictly ascending at %d: %s then %s", i, entries[i-1].Address.Hex(), e.Address.Hex())
			}
		}
		reenc, err := rlp.EncodeToBytes(entries)
		if err != nil {
			t.Fatalf("re-encode failed: %v", err)
		}
		if !bytes.Equal(reenc, ref) {
			t.Fatalf("round-trip mismatch:\n ref   %x\n reenc %x", ref, reenc)
		}

		// Properties 2 and 3.
		n := int64(len(entries))
		precision := big.NewInt(validatorFrequencyPrecision)
		minFreq := new(big.Int).Div(precision, big.NewInt(2*n))
		sum := new(big.Int)
		for _, e := range entries {
			if e.Frequency == nil {
				t.Fatalf("nil frequency for %s", e.Address.Hex())
			}
			if e.Frequency.Cmp(minFreq) < 0 {
				t.Fatalf("frequency %s for %s below minFreq %s (N=%d)", e.Frequency, e.Address.Hex(), minFreq, n)
			}
			sum.Add(sum, e.Frequency)
		}
		lower := new(big.Int).Sub(precision, big.NewInt(n-1))
		if sum.Cmp(lower) < 0 || sum.Cmp(precision) > 0 {
			t.Fatalf("frequency sum %s outside [%s, %s] (N=%d, minFreq=%s, entries=%v)", sum, lower, precision, n, minFreq, describeEntries(entries))
		}
	})
}

func describeEntries(entries []frequencyCandidate) string {
	var b bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&b, "%s:%s ", e.Address.Hex()[:10], e.Frequency)
	}
	return b.String()
}

// freqRLPFor computes frequency bytes for a seed distribution, for use as a
// well-formed corpus entry of the selection fuzzer.
func freqRLPFor(f *testing.F, seed uint64, chz []int64) []byte {
	validators := deriveFuzzValidators(seed, len(chz))
	stakes := make(map[common.Address]*big.Int, len(chz))
	for i, s := range seedStakes(chz...) {
		stakes[validators[i]] = s
	}
	snap := newFuzzSnapshot(1, 1, validators, validators, nil)
	out, err := snap.calcFrequencyRLP(stakes)
	if err != nil {
		f.Fatalf("seed frequency computation failed: %v", err)
	}
	return out
}

// FuzzSelectValidatorFromFrequencyRLP feeds arbitrary bytes to the selection
// side and pins: no panic; the zero address is never returned; a decodable,
// non-empty candidate list always selects one of its own entries; and an
// undecodable or empty list selects exactly what round-robin would.
//
// The zero-address property is asserted for every input that does not itself
// encode a zero-address candidate (garbage-in: a list containing 0x0 makes 0x0
// a legitimate "one of the candidates" outcome, and consensus-verified bytes
// can only carry addresses from the validator set). That case is kept in the
// corpus so the behaviour is visible, not hidden.
func FuzzSelectValidatorFromFrequencyRLP(f *testing.F) {
	f.Add(freqRLPFor(f, 1, selectionAlgorithmStakes), uint64(1000), uint64(1), uint8(9), uint8(50))
	f.Add(freqRLPFor(f, 3, []int64{1}), uint64(1), uint64(3), uint8(0), uint8(1))
	f.Add(freqRLPFor(f, 4, equalStakes(21, 1000)), uint64(28800), uint64(4), uint8(20), uint8(50))
	f.Add(freqRLPFor(f, 5, dominantStakes(21)), uint64(28801), uint64(5), uint8(20), uint8(50))
	// The two fallback cases from TestSelectValidatorFromFrequencyRLPFallback.
	f.Add([]byte{0xff, 0xff, 0xff}, uint64(7), uint64(1), uint8(2), uint8(1))
	f.Add([]byte{0xc0}, uint64(7), uint64(1), uint8(2), uint8(1))
	f.Add([]byte{}, uint64(7), uint64(1), uint8(2), uint8(1))
	// Frequencies summing to zero: target is never < cumulative, so the "returning
	// last candidate" branch fires for every block number.
	// Fatal, not "_": on an encode failure the seed would be nil bytes, which
	// routes into the round-robin fallback branch — so the two cases these doc
	// comments single out would quietly stop being tested, with no signal.
	zeroFreq, err := rlp.EncodeToBytes([]frequencyCandidate{{Address: common.HexToAddress("0x01"), Frequency: big.NewInt(0)}, {Address: common.HexToAddress("0x02"), Frequency: big.NewInt(0)}})
	if err != nil {
		f.Fatalf("encoding the zero-frequency seed: %v", err)
	}
	f.Add(zeroFreq, uint64(123), uint64(1), uint8(1), uint8(1))
	// A candidate list carrying the zero address (see doc comment).
	zeroAddr, err := rlp.EncodeToBytes([]frequencyCandidate{{Address: common.Address{}, Frequency: big.NewInt(1000)}})
	if err != nil {
		f.Fatalf("encoding the zero-address seed: %v", err)
	}
	f.Add(zeroAddr, uint64(5), uint64(1), uint8(2), uint8(1))

	f.Fuzz(func(t *testing.T, freqRLP []byte, number, seed uint64, nRaw, turnLength uint8) {
		n := 1 + int(nRaw)%fuzzMaxValidators
		if turnLength == 0 {
			turnLength = 1
		}
		validators := deriveFuzzValidators(seed, n)
		snap := newFuzzSnapshot(number, turnLength, validators, validators, nil)
		equiv := newFuzzSnapshot(number, turnLength, validators, validators, nil)

		got := snap.selectValidatorFromFrequencyRLP(freqRLP)

		var candidates []frequencyCandidate
		decodeErr := rlp.DecodeBytes(freqRLP, &candidates)
		if decodeErr != nil || len(candidates) == 0 {
			want := equiv.selectValidatorRoundRobin()
			if got != want {
				t.Fatalf("fallback mismatch (decodeErr=%v, candidates=%d): got %s, round-robin %s", decodeErr, len(candidates), got.Hex(), want.Hex())
			}
			if got == (common.Address{}) {
				t.Fatalf("round-robin fallback returned the zero address (n=%d, number=%d, turnLength=%d)", n, number, turnLength)
			}
			return
		}

		encodesZero := false
		member := false
		for _, c := range candidates {
			if c.Address == (common.Address{}) {
				encodesZero = true
			}
			if c.Address == got {
				member = true
			}
		}
		if !member {
			t.Fatalf("selected %s is not among the %d encoded candidates (%s)", got.Hex(), len(candidates), describeEntries(candidates))
		}
		if got == (common.Address{}) && !encodesZero {
			t.Fatalf("zero address selected from a list that does not contain it: %s", describeEntries(candidates))
		}
		// "One of the encoded candidates" is not the property that matters in
		// production. The frequency bytes can be stale or foreign — that is what
		// parlia_prepare_test.go's staleFreq fixture models — and then
		// inturnValidator() names an address outside snap.Validators, no validator
		// in the set is in-turn for the whole turn, and difficulty and backoff
		// degrade. So when every decoded candidate IS in the set, the selection
		// must be too; it may not synthesise an outsider from insider inputs.
		allInSet := true
		for _, c := range candidates {
			if _, ok := snap.Validators[c.Address]; !ok {
				allInSet = false
				break
			}
		}
		if allInSet {
			if _, ok := snap.Validators[got]; !ok {
				t.Fatalf("every candidate is in the validator set but the selection %s is not (%s)", got.Hex(), describeEntries(candidates))
			}
		}
	})
}

// TestFrequencyTargetOverflowFallback measures, for the seed stake
// distributions, how often the selection target (uniform in [0, precision))
// lands at or beyond the cumulative frequency total, so that
// selectValidatorFromFrequencyRLP takes its "returning last candidate" branch.
// The rate is a direct consequence of the truncation deficit pinned by
// FuzzCalcFrequencyRLP property 3 (rate ~= (precision - sum) / precision); it is
// reported, not asserted.
func TestFrequencyTargetOverflowFallback(t *testing.T) {
	const blocks = 28800 * 30 // one month of Chiliz blocks
	precision := big.NewInt(validatorFrequencyPrecision)

	distributions := []struct {
		name string
		chz  []int64
	}{
		{"TestValidatorSelectionAlgorithm", selectionAlgorithmStakes},
		{"single validator", []int64{1}},
		{"21 equal stakes", equalStakes(21, 1000)},
		{"99.9% dominant", dominantStakes(21)},
	}

	for _, d := range distributions {
		validators := deriveFuzzValidators(1, len(d.chz))
		stakes := make(map[common.Address]*big.Int, len(d.chz))
		for i, s := range seedStakes(d.chz...) {
			stakes[validators[i]] = s
		}
		snap := newFuzzSnapshot(1, 1, validators, validators, nil)
		freqRLP, err := snap.calcFrequencyRLP(stakes)
		if err != nil {
			t.Fatalf("%s: %v", d.name, err)
		}
		var entries []frequencyCandidate
		if err := rlp.DecodeBytes(freqRLP, &entries); err != nil {
			t.Fatalf("%s: decode: %v", d.name, err)
		}
		sum := new(big.Int)
		for _, e := range entries {
			sum.Add(sum, e.Frequency)
		}

		// Replicate the target derivation to count how often it is >= sum, and
		// cross-check by observing the selection itself on the same numbers.
		fallbacks := 0
		lastAddr := entries[len(entries)-1].Address
		lastSelected := 0
		seedBytes := make([]byte, 8)
		for number := uint64(0); number < blocks; number++ {
			binary.LittleEndian.PutUint64(seedBytes, number+1)
			h := sha256.Sum256(seedBytes)
			target := new(big.Int).Mod(new(big.Int).SetBytes(h[:]), precision)
			if target.Cmp(sum) >= 0 {
				fallbacks++
				snap.Number = number
				if snap.selectValidatorFromFrequencyRLP(freqRLP) == lastAddr {
					lastSelected++
				}
			}
		}
		if lastSelected != fallbacks {
			t.Fatalf("%s: fallback branch should return the last candidate every time (%d of %d)", d.name, lastSelected, fallbacks)
		}
		t.Logf("%-32s N=%2d sum=%4s deficit=%2s fallback fired %d / %d blocks (%.4f%%)",
			d.name, len(entries), sum, new(big.Int).Sub(precision, sum), fallbacks, blocks, 100*float64(fallbacks)/float64(blocks))
	}
}
