package parlia

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// FuzzSnapshotRoundTrip (COR-237) fuzzes the Snapshot struct's persistence layer —
// store / loadSnapshot / copy / updateAttestation — and pins six properties.
//
// Why this is worth a fuzz target at all. A Snapshot is the Parlia validator set,
// the recent-signer window and the finality attestation, and it reaches the next
// block through one of two paths: the shared LRU cache, or a JSON blob on disk
// written at every checkpoint. Unlike a header, a snapshot read back from disk is
// never re-validated against anything — apply() takes it as ground truth and builds
// the next state on top. So a field that silently fails to survive a store/load
// cycle does not produce an error; it produces a node that, after a restart,
// computes a different in-turn validator, a different backoff, or a different
// finalized height than the rest of the fleet. That is a fork, discovered on a
// restart, which is exactly when nobody is looking for a consensus change.
//
// The Chiliz-specific exposure is the two fields the fork added to an upstream
// struct: IsSnake8Fork and FrequencyRLP (see CLAUDE.md §2, Snake8). An upstream
// merge that rewrites this struct — or a hand edit that drops a `json:"..."` tag —
// loses them with no compile error and no test failure anywhere else in the tree.
// IsSnake8Fork in particular decides whether TurnLength is 50 or 1, which moves the
// epoch validator-set switch offset and the whole in-turn schedule.
//
// Properties
//
//  1. Round trip. store() then loadSnapshot() reproduces the snapshot, field for
//     field, compared through COR-215's canonical snapshotFingerprint (which covers
//     every field including the unexported config/ethAPI/sigCache pointers) — modulo
//     three transformations that loadSnapshot performs by contract and that are
//     pinned separately below: the COR-174 FrequencyRLP strip (1b), the legacy
//     zero-value default fill, and the Snake8 overrides (6).
//
//     1b. store() strips FrequencyRLP (COR-174). The written blob must not even
//     mention the field, and the loaded snapshot must carry nil. This is intended
//     behaviour, not a lost field: FrequencyRLP is per-block, caller-supplied data,
//     so persisting it would make an on-disk record depend on whichever caller
//     happened to build it.
//
//  2. nil versus empty FrequencyRLP. The field is the one place in the struct where
//     a nil slice and a zero-length slice are distinguishable, so the target states
//     explicitly where the difference survives and where it does not: every reader
//     treats them alike (inturnValidator falls back to round-robin for both), and
//     copy() preserves the distinction exactly. That last one matters because copy()
//     is the COR-174 boundary: bytes.Clone(nil) must stay nil rather than becoming a
//     shared empty slice, so that "no frequency data" cannot mutate into "some
//     caller's frequency data" further down a copy chain. The disk side is
//     deliberately NOT asserted — store() and the `omitempty` tag erase the
//     distinction before anything is written, so a store/load comparison of the two
//     spellings is true by construction; see the note on
//     assertFrequencyNilEmptyInterchangeable.
//
//  3. copy() is deep. Mutating any map, any *ValidatorInfo, the Attestation pointer
//     or the FrequencyRLP slice through the copy leaves the original byte-identical,
//     and vice versa. This is the COR-174 fragility class (shared snapshot state that
//     depends on who touched it last) at the struct level rather than the apply()
//     level: Parlia.snapshot() hands out copies of a shared LRU entry, so a shallow
//     copy means one verifier's Recents window leaks into another's.
//
//  4. loadSnapshot never panics. A corrupted, truncated or bit-flipped database
//     value must come back as an error, not a panic: the blob is read during startup
//     and during a cache miss deep inside header verification, where a panic takes
//     the node down rather than resyncing the snapshot. The target feeds it raw fuzz
//     bytes, the real blob truncated at a fuzzed offset, and the real blob with one
//     byte flipped. Whenever it does return successfully, three invariants must hold
//     for arbitrary input: TurnLength != 0 (selectValidatorRoundRobin divides by it),
//     FrequencyRLP == nil, and IsSnake8Fork == the argument.
//
//  5. updateAttestation is monotone and conservative. An absent, undecodable or
//     target-mismatched attestation leaves the field byte-identical; pre-Luban is a
//     no-op; and a forward-progressing chain of well-formed attestations never moves
//     the stored TargetNumber backwards. Property 5 is what the pre-Fermi
//     target-is-direct-parent guard buys, and an un-fork-gated removal of that guard
//     would let a header roll a node's finality view backwards.
//
//  6. The isSnake8Fork argument is applied, not dropped. The stored snapshot's flag
//     and the load argument are independent fuzz inputs, so the corpus contains
//     mismatched pairs: the loaded value must always follow the argument, and when
//     the argument is set TurnLength must be forced to snake8TurnLength (50)
//     regardless of what was on disk. Loading with the argument equal to the stored
//     flag would make this property vacuous — hence two separate bools.
//
// Generator notes
//
//   - RecentForkHashes values are hex strings, because that is what production puts
//     there (snapshot.go: hex.EncodeToString of the 4 nextForkHash bytes). Arbitrary
//     byte strings are deliberately NOT generated: encoding/json replaces invalid
//     UTF-8 with U+FFFD, so a non-hex value would "fail" the round trip without any
//     reachable bug behind it. Worth knowing the latent sharp edge exists; it is not
//     what this target is for.
//   - Recents optionally carries a Bohr epochKey marker (MaxUint64 - number/epoch,
//     mapped to the zero address), gated on the low bit of recentBytes so both the
//     with-marker and without-marker shapes are in the corpus. Those keys sit at the
//     very top of the uint64 range, which is where a JSON integer-key encoding that
//     went through float64 would lose precision.
//   - Index and VoteAddress are always non-zero. A dropped field whose zero value
//     equals the value under test is invisible, so the generator never hands the
//     round trip a field it could lose for free.
//   - The struct is built literally rather than through newSnapshot, so Index and
//     VoteAddress are free inputs instead of newSnapshot's derived idx+1.
//
// Note on the two malformed struct tags this target walks past on purpose:
// ValidatorInfo.Index is tagged `json:"index:omitempty"` and Snapshot.Attestation
// `json:"attestation:omitempty"` — a comma was meant, so the JSON member names are
// literally "index:omitempty" and "attestation:omitempty". Marshal and Unmarshal
// agree with each other, so the round trip is intact and there is nothing to fix
// here: changing either tag now would make every snapshot already on every node's
// disk unreadable for that field. The round trip is the contract, not the spelling.
func FuzzSnapshotRoundTrip(f *testing.F) {
	// Seeds. Each one is the minimal shape that separates one property from the
	// others; `go test` without -fuzz replays only this corpus, so a property with
	// no seed of its own is a property CI does not check.
	//
	// args: seed, nSel, number, epochRaw, intervalRaw, turnLength, snapFlag,
	//       loadFlag, freqSel, freqLen, attestSel, recentBytes, forkHashBytes, corrupt

	// Everything empty: no recents, no fork hashes, nil frequency data, nil
	// attestation — the only seed where both maps are empty and both pointer-ish
	// fields are nil. It is the degenerate shape the other seeds never reach: copy()
	// takes its nil branch for Attestation and its bytes.Clone(nil) branch for
	// FrequencyRLP, and assertCopyIsDeep's overwrite-and-delete probes find nothing
	// to iterate over, so a round trip or a copy that only holds together once there
	// is data in the maps fails here and nowhere else.
	//
	// Note what this seed does NOT pin, because it reads like it should: whether an
	// empty map comes back empty or nil. snapshotFingerprint renders nil and empty
	// maps identically, so the round trip cannot see the difference, and nothing
	// downstream depends on it — apply() opens with `snap := s.copy()` and copy()
	// make()s all three maps unconditionally, so a nil map out of loadSnapshot can
	// never reach a write. That contract is stated once, at assertLoadSnapshotTotal.
	f.Add(uint64(1), uint8(0), uint64(0), uint64(200), uint64(3000), uint8(1), false, false, uint8(0), uint16(0), uint8(0), []byte(nil), []byte(nil), []byte(nil))
	// Populated maps, 3 validators, no epochKey marker (recentBytes[0] even).
	f.Add(uint64(2), uint8(2), uint64(1024), uint64(200), uint64(3000), uint8(1), false, false, uint8(0), uint16(0), uint8(0), []byte{0x02, 0x11, 0x22, 0x33}, []byte{0xaa, 0xbb}, []byte(nil))
	// Same, plus a Bohr epochKey marker (recentBytes[0] odd): a key at
	// MaxUint64-5 alongside ordinary block keys.
	f.Add(uint64(3), uint8(2), uint64(1024), uint64(200), uint64(3000), uint8(1), false, false, uint8(0), uint16(0), uint8(0), []byte{0x03, 0x11, 0x22, 0x33}, []byte{0xaa, 0xbb}, []byte(nil))
	// FrequencyRLP empty (len 0, non-nil) — the nil/empty distinction of property 2.
	f.Add(uint64(4), uint8(2), uint64(1024), uint64(200), uint64(3000), uint8(1), true, true, uint8(1), uint16(0), uint8(0), []byte{0x01, 0x44}, []byte{0xcc}, []byte(nil))
	// FrequencyRLP populated with 300 bytes — the shape store() must strip, and
	// large enough that a non-stripping store() is obvious in the blob.
	f.Add(uint64(5), uint8(4), uint64(5_000_000), uint64(200), uint64(3000), uint8(50), true, true, uint8(2), uint16(300), uint8(0), []byte{0x01, 0x55, 0x66}, []byte{0xdd}, []byte(nil))
	// Attestation populated: the only seed where the Attestation field is non-nil,
	// so it alone separates a round trip that drops it.
	f.Add(uint64(6), uint8(2), uint64(1024), uint64(200), uint64(3000), uint8(1), false, false, uint8(0), uint16(0), uint8(1), []byte{0x01, 0x77}, []byte{0xee}, []byte(nil))
	// TurnLength 50 (Snake8) with the flag stored AND loaded — the coherent
	// Snake8 shape a live Chiliz node actually holds.
	f.Add(uint64(7), uint8(20), uint64(32_000_000), uint64(28800), uint64(2000), uint8(50), true, true, uint8(2), uint16(64), uint8(1), []byte{0x01, 0x88, 0x99}, []byte{0xff, 0x01}, []byte(nil))
	// Stored flag FALSE, loaded with TRUE: the argument must win and force
	// TurnLength 1 -> 50. This is the seed that kills "drop the isSnake8Fork
	// assignment in loadSnapshot"; without a mismatched pair the property is
	// satisfied by the JSON value alone.
	f.Add(uint64(8), uint8(2), uint64(7_200), uint64(7200), uint64(3000), uint8(1), false, true, uint8(0), uint16(0), uint8(1), []byte{0x01, 0xa1}, []byte{0x02}, []byte(nil))
	// Stored flag TRUE, loaded with FALSE: the mirror direction. TurnLength 50 is
	// kept here (loadSnapshot only forces 50 on, never off) — pinning that the
	// override is one-way.
	f.Add(uint64(9), uint8(2), uint64(7_200), uint64(7200), uint64(3000), uint8(50), true, false, uint8(0), uint16(0), uint8(1), []byte{0x01, 0xa2}, []byte{0x03}, []byte(nil))
	// Legacy record: EpochLength, BlockInterval and TurnLength all zero on disk,
	// which is what a pre-upgrade blob looks like. loadSnapshot must fill the
	// defaults rather than hand back a snapshot that divides by zero.
	f.Add(uint64(10), uint8(2), uint64(64), uint64(0), uint64(0), uint8(0), false, false, uint8(0), uint16(0), uint8(0), []byte{0x00, 0xb1}, []byte{0x04}, []byte(nil))
	// A genuinely corrupted stored value: valid JSON prefix, truncated mid-object.
	// Property 4's headline case — it must error, not panic.
	f.Add(uint64(11), uint8(2), uint64(1024), uint64(200), uint64(3000), uint8(1), false, false, uint8(0), uint16(0), uint8(0), []byte{0x01, 0xc1}, []byte{0x05}, []byte(`{"number":1024,"recents":{"18446744073709551610":`))
	// Raw binary garbage, not JSON at all.
	f.Add(uint64(12), uint8(1), uint64(1), uint64(200), uint64(3000), uint8(1), false, false, uint8(0), uint16(0), uint8(0), []byte(nil), []byte(nil), []byte{0xff, 0x00, 0xde, 0xad, 0xbe, 0xef, 0x7f, 0x80})
	// Maximum validator set (21) with every map populated and both Chiliz fields
	// set — the widest fingerprint the target ever compares.
	f.Add(uint64(13), uint8(20), uint64(88_888_888), uint64(28800), uint64(2000), uint8(50), true, true, uint8(2), uint16(1023), uint8(1),
		[]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a}, []byte{0x10, 0x20, 0x30, 0x40}, []byte(`{}`))

	f.Fuzz(func(t *testing.T,
		seed uint64, nSel uint8, number, epochRaw, intervalRaw uint64, turnLength uint8,
		snapFlag, loadFlag bool, freqSel uint8, freqLen uint16, attestSel uint8,
		recentBytes, forkHashBytes, corrupt []byte,
	) {
		orig := buildFuzzSerdeSnapshot(seed, nSel, number, epochRaw, intervalRaw, turnLength, snapFlag, freqSel, freqLen, attestSel, recentBytes, forkHashBytes)

		// ---- property 3: copy() is deep --------------------------------------
		// Run first, on the pristine snapshot, so a shallow copy is reported as a
		// copy bug rather than surfacing later as a confusing round-trip diff.
		assertCopyIsDeep(t, orig)

		// ---- property 2 (copy half): nil and empty FrequencyRLP stay distinct --
		// copy() is the only place the struct must tell them apart. bytes.Clone(nil)
		// is nil; bytes.Clone([]byte{}) is a non-nil empty slice. If copy() ever
		// normalised one into the other, "this snapshot carries no frequency data"
		// and "this snapshot carries an empty computation" would become the same
		// value, and the COR-174 reasoning about which callers own the field would
		// lose its footing.
		assertFrequencyNilnessPreservedByCopy(t, orig)

		// ---- properties 1, 1b and 6: the store/load cycle ---------------------
		db := rawdb.NewMemoryDatabase()
		if err := orig.store(db); err != nil {
			t.Fatalf("store failed: %v", err)
		}
		key := append([]byte("parlia-"), orig.Hash[:]...)
		blob, err := db.Get(key)
		if err != nil {
			t.Fatalf("stored blob missing under its own hash key: %v", err)
		}

		// 1b. COR-174: the field is not written at all, so no on-disk record can
		// carry one caller's per-block view into another node's state.
		if bytes.Contains(blob, []byte("frequency_rlp")) {
			t.Fatalf("store() persisted caller-dependent frequency data (COR-174), blob: %s", blob)
		}
		// Guard against a vacuous assertion: if store() wrote something that does
		// not look like a snapshot at all, the check above proves nothing.
		for _, marker := range []string{"turn_length", "is_snake8_fork", "recents", "recent_fork_hashes", "epoch_length", "block_interval"} {
			if !bytes.Contains(blob, []byte(marker)) {
				t.Fatalf("stored blob is missing the %q member — a field lost its json tag, which loses it silently on every restart: %s", marker, blob)
			}
		}

		loaded, err := loadSnapshot(orig.config, orig.sigCache, db, orig.Hash, orig.ethAPI, loadFlag)
		if err != nil {
			t.Fatalf("loadSnapshot failed on a blob store() had just written: %v", err)
		}

		// 6. The argument is applied to the loaded value, not dropped in favour of
		// whatever JSON happened to hold.
		if loaded.IsSnake8Fork != loadFlag {
			t.Fatalf("loadSnapshot returned IsSnake8Fork=%v for argument isSnake8Fork=%v (stored value was %v) — the argument must win, it is derived from the parent's time and the JSON is only a cache",
				loaded.IsSnake8Fork, loadFlag, snapFlag)
		}
		if loadFlag && loaded.TurnLength != snake8TurnLength {
			t.Fatalf("loadSnapshot returned TurnLength=%d under Snake8, want %d (stored value was %d) — the override decides the epoch validator-set switch offset and the whole in-turn schedule",
				loaded.TurnLength, snake8TurnLength, orig.TurnLength)
		}
		// 1b, load half.
		if loaded.FrequencyRLP != nil {
			t.Fatalf("loadSnapshot returned frequency data %x; copy() carries the field, so it would propagate into every snapshot derived from this one (COR-174)", loaded.FrequencyRLP)
		}
		// Property-4 invariant, asserted here too because this is the path every
		// node takes on a cache miss: a zero TurnLength makes
		// selectValidatorRoundRobin divide by zero.
		if loaded.TurnLength == 0 {
			t.Fatal("loadSnapshot returned TurnLength 0; selectValidatorRoundRobin divides by it")
		}

		// 1. Everything else is identity. `want` is the original with exactly the
		// three documented transformations applied — anything the round trip drops
		// beyond them shows up as a fingerprint diff.
		want := orig.copy()
		want.FrequencyRLP = nil // 1b, above
		if want.EpochLength == 0 {
			want.EpochLength = defaultEpochLength // legacy record with no epoch_length
		}
		if want.BlockInterval == 0 {
			want.BlockInterval = defaultBlockInterval
		}
		if want.TurnLength == 0 {
			want.TurnLength = defaultTurnLength
		}
		if loadFlag {
			want.TurnLength = snake8TurnLength // 6, above
		}
		want.IsSnake8Fork = loadFlag
		if got, wantFP := snapshotFingerprint(loaded), snapshotFingerprint(want); got != wantFP {
			t.Fatalf("snapshot did not survive store/load intact.\n got:\n%s\nwant:\n%s", got, wantFP)
		}

		// ---- property 2 (reader half): nil and empty read alike ---------------
		assertFrequencyNilEmptyInterchangeable(t, orig, loaded.TurnLength)

		// ---- property 4: loadSnapshot never panics on arbitrary stored bytes ---
		assertLoadSnapshotTotal(t, orig, blob, corrupt, loadFlag)

		// ---- property 5: updateAttestation is monotone and conservative --------
		// Driven from `loaded` rather than `orig`: loadSnapshot guarantees a
		// non-zero EpochLength, and getVoteAttestationFromHeader takes
		// header.Number % epochLength. A zero epoch length would divide by zero,
		// but no snapshot reaching updateAttestation in production has one
		// (newSnapshot, loadSnapshot and apply all set it), so generating that
		// case would be inventing an unreachable panic rather than finding one.
		assertUpdateAttestation(t, loaded, seed)
	})
}

// buildFuzzSerdeSnapshot decodes one fuzz input into the snapshot under test.
//
// The struct is filled literally rather than through newSnapshot so that Index and
// VoteAddress are free inputs. Every field is given a non-zero value where the fuzz
// input allows one: a field that a broken round trip drops comes back as its zero
// value, so seeding it with a zero would hide the very failure being hunted.
func buildFuzzSerdeSnapshot(
	seed uint64, nSel uint8, number, epochRaw, intervalRaw uint64, turnLength uint8,
	snapFlag bool, freqSel uint8, freqLen uint16, attestSel uint8,
	recentBytes, forkHashBytes []byte,
) *Snapshot {
	n := 1 + int(nSel)%fuzzMaxValidators
	validators := deriveFuzzValidators(seed, n)

	snap := &Snapshot{
		// The unexported plumbing is part of the fingerprint (COR-215 compares the
		// pointers), and loadSnapshot re-installs whatever the caller passes — so
		// real, distinct pointers here make "the caller's plumbing is reinstalled"
		// an actual assertion rather than a comparison of two nils.
		config:           &params.ParliaConfig{Period: 3, Epoch: 200},
		sigCache:         lru.NewCache[common.Hash, common.Address](8),
		ethAPI:           nil, // constructing a real BlockChainAPI needs a full backend; nil is what every snapshot test uses
		Number:           number,
		Hash:             common.BytesToHash(fuzzTag(seed, 0, "snapshot-hash")),
		EpochLength:      epochRaw,
		BlockInterval:    intervalRaw,
		TurnLength:       turnLength,
		IsSnake8Fork:     snapFlag,
		Validators:       make(map[common.Address]*ValidatorInfo, n),
		Recents:          make(map[uint64]common.Address),
		RecentForkHashes: make(map[uint64]string),
	}

	for i, v := range validators {
		info := &ValidatorInfo{Index: i + 1} // offset by 1, as newSnapshot does; never 0
		// A non-zero BLS vote address: a dropped vote_address tag would otherwise
		// come back as the all-zero key and compare equal.
		copy(info.VoteAddress[:], fuzzBytes(seed, fmt.Sprintf("vote-%d", i), len(info.VoteAddress)))
		snap.Validators[v] = info
	}

	// Recents: entry i at block number-i, resolved through the derivation order so
	// the map is a deterministic function of the input.
	for i, b := range recentBytes {
		if i >= fuzzMaxRecents || uint64(i) > number {
			break
		}
		snap.Recents[number-uint64(i)] = validators[int(b)%len(validators)]
	}
	// Bohr's epochKey marker (snapshot.go: MaxUint64 - number/epochLength, mapped to
	// the zero address so countRecents skips it). Gated on the low bit so both
	// shapes are in the corpus. These keys live at the top of the uint64 range,
	// which is where an integer-map-key encoding that round-tripped through float64
	// would quietly lose precision.
	if len(recentBytes) > 0 && recentBytes[0]%2 == 1 {
		epoch := epochRaw
		if epoch == 0 {
			epoch = defaultEpochLength
		}
		snap.Recents[^uint64(0)-number/epoch] = common.Address{}
	}

	// RecentForkHashes: hex strings, matching what apply() writes
	// (hex.EncodeToString of the 4 nextForkHash bytes). See the generator note on
	// the target for why arbitrary byte strings are not used.
	for i, b := range forkHashBytes {
		if i >= fuzzMaxRecents || uint64(i) > number {
			break
		}
		snap.RecentForkHashes[number-uint64(i)] = hex.EncodeToString(fuzzBytes(seed, fmt.Sprintf("fork-%d-%d", i, b), nextForkHashSize))
	}

	switch freqSel % 3 {
	case 0:
		snap.FrequencyRLP = nil
	case 1:
		snap.FrequencyRLP = []byte{}
	default:
		snap.FrequencyRLP = fuzzBytes(seed, "frequency", int(freqLen%1024))
	}

	if attestSel%2 == 1 {
		snap.Attestation = &types.VoteData{
			SourceNumber: number / 2,
			SourceHash:   common.BytesToHash(fuzzTag(seed, 1, "source")),
			TargetNumber: number/2 + 1,
			TargetHash:   common.BytesToHash(fuzzTag(seed, 2, "target")),
		}
	}

	return snap
}

// fuzzBytes derives n deterministic bytes from (seed, tag), extending fuzzTag's
// 32-byte output by chaining.
func fuzzBytes(seed uint64, tag string, n int) []byte {
	out := make([]byte, 0, n)
	for i := 0; len(out) < n; i++ {
		out = append(out, fuzzTag(seed, i, tag)...)
	}
	return out[:n]
}

// assertCopyIsDeep is property 3: every reference-typed field of the copy is its
// own storage, in both directions.
//
// Both directions matter and they fail differently. Parlia.snapshot() returns a
// copy of a shared LRU entry, so copy->original aliasing lets a verifier corrupt
// the cache for everyone; original->copy aliasing lets the next cache write silently
// rewrite a snapshot a caller is already reasoning about. COR-174 was the second
// shape.
func assertCopyIsDeep(t *testing.T, orig *Snapshot) {
	t.Helper()

	// --- mutate through the copy, original must not move ---
	before := snapshotFingerprint(orig)
	cpy := orig.copy()
	if got := snapshotFingerprint(cpy); got != before {
		t.Fatalf("copy() is not a faithful view of the original.\n got:\n%s\nwant:\n%s", got, before)
	}

	// Probe keys must be keys the snapshot does not already hold. Picking a constant
	// near MaxUint64 looks safe and is not: Bohr's epochKey marker is
	// MaxUint64 - Number/EpochLength, so for a small Number/EpochLength ratio it lands
	// exactly on the obvious probes. Corpus entry 5814b4f9da7f6559 (Number 64, epoch
	// 32) put the marker on MaxUint64-2, which the restore step below then deleted as
	// if it were the probe — the harness destroyed real data and reported itself.
	// That was this helper being wrong, not copy(); the input is kept as a seed.
	probeA, probeB := freshRecentKeys(orig)

	// A fresh key in each map, plus an overwrite of an existing one, plus a delete.
	cpy.Recents[probeA] = common.HexToAddress("0xdeadbeef")
	cpy.RecentForkHashes[probeA] = "cafebabe"
	for k := range cpy.Recents {
		cpy.Recents[k] = common.HexToAddress("0xfeedface")
		delete(cpy.Recents, k)
		break
	}
	for k := range cpy.RecentForkHashes {
		cpy.RecentForkHashes[k] = "00000000"
		delete(cpy.RecentForkHashes, k)
		break
	}
	// Validators holds *ValidatorInfo, so a map clone that reused the pointers would
	// still let this write through. copy() allocates a new ValidatorInfo per entry.
	for _, v := range cpy.validators() {
		cpy.Validators[v].Index = -999
		cpy.Validators[v].VoteAddress[0] ^= 0xff
		break
	}
	for _, v := range cpy.validators() {
		delete(cpy.Validators, v)
		break
	}
	cpy.Validators[common.HexToAddress("0xabadcafe")] = &ValidatorInfo{Index: 42}
	if cpy.Attestation != nil {
		cpy.Attestation.TargetNumber ^= 0x5a5a5a5a
		cpy.Attestation.TargetHash[0] ^= 0xff
	}
	if len(cpy.FrequencyRLP) > 0 {
		cpy.FrequencyRLP[0] ^= 0xff
	}

	if after := snapshotFingerprint(orig); after != before {
		t.Fatalf("mutating the copy changed the original — copy() is shallow.\n before:\n%s\n after:\n%s", before, after)
	}

	// --- mutate through the original, the copy must not move ---
	pristine := orig.copy()
	snapshot := snapshotFingerprint(pristine)
	orig.Recents[probeB] = common.HexToAddress("0x1234")
	orig.RecentForkHashes[probeB] = "12345678"
	for _, v := range orig.validators() {
		orig.Validators[v].Index = -1234
		break
	}
	if orig.Attestation != nil {
		orig.Attestation.TargetNumber ^= 0xa5a5a5a5
	}
	if len(orig.FrequencyRLP) > 0 {
		orig.FrequencyRLP[0] ^= 0xff
	}
	if after := snapshotFingerprint(pristine); after != snapshot {
		t.Fatalf("mutating the original changed a copy taken earlier — copy() is shallow.\n before:\n%s\n after:\n%s", snapshot, after)
	}

	// Undo, so the rest of the properties see the snapshot the generator built.
	delete(orig.Recents, probeB)
	delete(orig.RecentForkHashes, probeB)
	for _, v := range orig.validators() {
		orig.Validators[v].Index = pristine.Validators[v].Index
		break
	}
	if orig.Attestation != nil {
		orig.Attestation.TargetNumber ^= 0xa5a5a5a5
	}
	if len(orig.FrequencyRLP) > 0 {
		orig.FrequencyRLP[0] ^= 0xff
	}
	if restored := snapshotFingerprint(orig); restored != before {
		t.Fatalf("test harness bug: failed to restore the snapshot after the deep-copy checks.\n got:\n%s\nwant:\n%s", restored, before)
	}
}

// freshRecentKeys returns two distinct block numbers that appear in neither
// Recents nor RecentForkHashes, so the deep-copy probes below can insert and then
// remove them without disturbing what the generator built. See the comment at the
// call site for the corpus entry that made this necessary.
func freshRecentKeys(s *Snapshot) (uint64, uint64) {
	out := make([]uint64, 0, 2)
	for k := ^uint64(0); len(out) < 2; k-- {
		if _, ok := s.Recents[k]; ok {
			continue
		}
		if _, ok := s.RecentForkHashes[k]; ok {
			continue
		}
		out = append(out, k)
	}
	return out[0], out[1]
}

// assertFrequencyNilnessPreservedByCopy is the copy() half of property 2.
func assertFrequencyNilnessPreservedByCopy(t *testing.T, orig *Snapshot) {
	t.Helper()
	cpy := orig.copy()
	// snapshotFingerprint renders nil and empty identically (%x of both is ""),
	// which is correct for every other property — here the nil-ness itself is the
	// thing under test, so it is compared directly.
	if (orig.FrequencyRLP == nil) != (cpy.FrequencyRLP == nil) {
		t.Fatalf("copy() changed FrequencyRLP nil-ness: original nil=%v, copy nil=%v — bytes.Clone must map nil to nil and empty to empty (COR-174)",
			orig.FrequencyRLP == nil, cpy.FrequencyRLP == nil)
	}
	if len(orig.FrequencyRLP) != len(cpy.FrequencyRLP) {
		t.Fatalf("copy() changed FrequencyRLP length: %d -> %d", len(orig.FrequencyRLP), len(cpy.FrequencyRLP))
	}
	if len(orig.FrequencyRLP) > 0 && &orig.FrequencyRLP[0] == &cpy.FrequencyRLP[0] {
		t.Fatal("copy() shares the FrequencyRLP backing array with the original (COR-174)")
	}
}

// assertFrequencyNilEmptyInterchangeable is the reader half of property 2: wherever
// the code does NOT distinguish nil from empty, it must not start to. inturnValidator
// explicitly treats both as "no data" and falls back to round-robin — if that ever
// diverged, a producer whose calcFrequencyRLP returned an empty (rather than nil)
// result would select a different in-turn validator than a verifier that had
// normalised it away.
//
// There is deliberately no disk half here, and its absence is the point. A store/load
// comparison of the two spellings is true by construction: store() sets FrequencyRLP
// to nil before marshalling, the tag is `json:",omitempty"` so neither spelling is
// ever emitted anyway, and loadSnapshot nils the field unconditionally (snapshot.go).
// By the time anything reaches a blob the distinction no longer exists, so such a
// check stays green even with the COR-174 strip deleted from store() — it was
// asserting nothing. What actually pins the strip is the
// bytes.Contains(blob, "frequency_rlp") check in the target body and the member list
// in TestSnapshotRoundTripStoredShape; deleting `persisted.FrequencyRLP = nil` fails
// both, and fails neither of the checks below.
//
// The reader check has to be set up deliberately, because inturnValidator reaches the
// FrequencyRLP clause only after two earlier gates:
//
//	if s.Number == 0 || !s.IsSnake8Fork { return s.selectValidatorRoundRobin() }
//
// The probes are copies of the round-tripped snapshot, so left alone they inherit its
// Number and IsSnake8Fork. Whenever the snapshot is pre-Snake8 (snapFlag false: seeds
// 2, 3, 6, 8, 10, 11 and 12 — 7 of the 13, and roughly half the fuzz space) or sits at
// height 0 (seed 1), both calls return through that short-circuit and agree for a
// reason that has nothing to do with the property. So the probes force
// IsSnake8Fork = true and a non-zero Number: the only configuration in which the
// nil/empty clause is the thing being compared. Both probes are forced identically,
// so the round-robin offset they would fall back to stays the same on both sides and
// the comparison is still apples to apples.
//
// Even so, the check is over-determined once it does reach the clause, which is worth
// knowing before trusting it: dropping the `len(s.FrequencyRLP) == 0` clause from
// inturnValidator alone does NOT fail it, because selectValidatorFromFrequencyRLP
// then hits an RLP decode error on the empty slice and falls back to round-robin by a
// second route, reaching the same address. It fails once both fallbacks are gone —
// which is exactly the state in which an empty result would pick a different in-turn
// validator than a nil one. It is a defence-in-depth assertion over the composition,
// not a mutation test of either branch on its own.
func assertFrequencyNilEmptyInterchangeable(t *testing.T, orig *Snapshot, turnLength uint8) {
	t.Helper()

	// inturnValidator divides by len(validators) and by TurnLength; the generator
	// guarantees at least one validator, and turnLength comes from the round-tripped
	// snapshot, which loadSnapshot guarantees non-zero.
	if len(orig.Validators) == 0 || turnLength == 0 {
		t.Fatalf("test harness bug: snapshot has %d validators and TurnLength %d", len(orig.Validators), turnLength)
	}
	// See the doc comment: both gates above the clause under test are opened here,
	// identically on both probes.
	number := orig.Number
	if number == 0 {
		number = 1
	}
	withNil := orig.copy()
	withNil.FrequencyRLP = nil
	withNil.TurnLength = turnLength
	withNil.IsSnake8Fork = true
	withNil.Number = number
	withEmpty := orig.copy()
	withEmpty.FrequencyRLP = []byte{}
	withEmpty.TurnLength = turnLength
	withEmpty.IsSnake8Fork = true
	withEmpty.Number = number

	if got, want := withEmpty.inturnValidator(), withNil.inturnValidator(); got != want {
		t.Fatalf("inturnValidator disagrees between empty (%s) and nil (%s) FrequencyRLP — both mean 'no data' and must fall back to round-robin identically", got.Hex(), want.Hex())
	}
}

// assertLoadSnapshotTotal is property 4: loadSnapshot is total over stored bytes.
//
// It is fed three flavours of damage, because they reach different decoders:
// arbitrary fuzz bytes (usually not JSON at all), the real blob truncated (valid
// JSON prefix, unterminated), and the real blob with one byte flipped (structurally
// valid JSON with an out-of-range or mistyped member). The last is the realistic
// one — a single flipped bit on disk or in a page cache.
func assertLoadSnapshotTotal(t *testing.T, orig *Snapshot, blob, corrupt []byte, loadFlag bool) {
	t.Helper()

	variants := map[string][]byte{"raw-fuzz-bytes": corrupt}
	if len(blob) > 0 {
		cut := 0
		if len(corrupt) > 0 {
			cut = int(corrupt[0]) * len(blob) / 256
		}
		variants["truncated"] = blob[:cut]

		flipped := bytes.Clone(blob)
		// corrupt[1] is at most 255, so the product is at most 255*len(blob)/256,
		// which is always < len(blob) — no clamp needed, and one written here would
		// be dead code that reads as if the bound were in doubt.
		pos := 0
		if len(corrupt) > 1 {
			pos = int(corrupt[1]) * len(blob) / 256
		}
		flipped[pos] ^= 0xff
		variants["bit-flipped"] = flipped
	}

	db := rawdb.NewMemoryDatabase()
	key := append([]byte("parlia-"), orig.Hash[:]...)

	// Absent key: the database has no record at all. This is the ordinary cache-miss
	// outcome that tells Parlia.snapshot() to keep walking back through headers, so
	// it must be a plain error rather than a panic or a zero-valued snapshot.
	if snap, err := loadSnapshot(orig.config, orig.sigCache, db, orig.Hash, orig.ethAPI, loadFlag); err == nil {
		t.Fatalf("loadSnapshot succeeded with no record stored, returning %+v", snap)
	} else if snap != nil {
		t.Fatal("loadSnapshot returned a snapshot alongside a not-found error")
	}

	for name, value := range variants {
		if err := db.Put(key, value); err != nil {
			t.Fatalf("%s: writing the damaged blob failed: %v", name, err)
		}
		// A panic here fails the test with the stack, which is the report we want:
		// this runs on a cache miss inside header verification, where a panic stops
		// the node instead of resyncing one snapshot.
		snap, err := loadSnapshot(orig.config, orig.sigCache, db, orig.Hash, orig.ethAPI, loadFlag)
		if err != nil {
			if snap != nil {
				t.Fatalf("%s: loadSnapshot returned both a snapshot and an error %v — callers branch on err and would use the half-built value", name, err)
			}
			continue
		}
		// Arbitrary bytes CAN be valid JSON (`{}` is), so success is allowed. What
		// is not allowed is a successful load that hands back a snapshot violating
		// the three invariants every caller relies on.
		if snap == nil {
			t.Fatalf("%s: loadSnapshot returned (nil, nil)", name)
		}
		if snap.TurnLength == 0 {
			t.Fatalf("%s: loadSnapshot accepted a blob and returned TurnLength 0; selectValidatorRoundRobin divides by it", name)
		}
		if snap.FrequencyRLP != nil {
			t.Fatalf("%s: loadSnapshot returned frequency data %x from a stored record (COR-174)", name, snap.FrequencyRLP)
		}
		if snap.IsSnake8Fork != loadFlag {
			t.Fatalf("%s: loadSnapshot returned IsSnake8Fork=%v for argument %v", name, snap.IsSnake8Fork, loadFlag)
		}
		if loadFlag && snap.TurnLength != snake8TurnLength {
			t.Fatalf("%s: loadSnapshot returned TurnLength=%d under Snake8, want %d", name, snap.TurnLength, snake8TurnLength)
		}
		// The blob may legitimately have decoded without maps (`{}` has none), and
		// loadSnapshot does not allocate them. That is the existing contract and it
		// is safe: every consumer reaches the maps through apply(), which opens with
		// `snap := s.copy()`, and copy() make()s all three unconditionally — so a nil
		// map from here is never written to. Fingerprinting it is the assertion that
		// a nil-mapped snapshot can at least be rendered without panicking; pinned so
		// that a change to the contract is a deliberate one.
		_ = snapshotFingerprint(snap)
	}
}

// attestationMaxOffset is the largest offset above `base` at which
// assertUpdateAttestation constructs a block number: the target of the "target is not
// the direct parent" probe. The monotone half only walks base..base+7, and the
// pre-Luban probe reaches backwards. The overflow guard reserves exactly this much, so
// a probe added further out has to move this constant with it.
const attestationMaxOffset = 41

// assertUpdateAttestation is property 5.
//
// Two halves, and the second is the one that guards a real consensus behaviour.
//
// Conservative: a header with no attestation section, an undecodable one, or one
// naming a target that is not the header's direct parent must leave s.Attestation
// byte-identical. The third case is the pre-Fermi guard in updateAttestation.
//
// That guard is dead code on Chiliz today, and this property is a tripwire against a
// future BSC merge rather than a live protection — the earlier claim that it is "the
// only thing standing between a crafted header and a node's finality view" is wrong,
// and the first commit's message repeats it. Fast finality never switches on here at
// all: `config/embedded/{chiliz,spicy,scoville}.json` carry no lubanBlock (nor
// platoBlock or fermiTime), so updateAttestation returns at its first line,
// `if !chainConfig.IsLuban(header.Number)` (snapshot.go), on every Chiliz network.
// Snapshot.Attestation is never populated and there is no finality view for a crafted
// header to move — which is the known no-fast-finality behaviour, not a gap. The
// property is still worth keeping: it runs against a locally built LubanBlock: 0
// config, so it pins the semantics upstream relies on, and it is what would catch a
// BSC sync that loosened the pre-Fermi target check before Chiliz ever schedules
// Luban.
//
// Monotone: replaying a forward-progressing chain of well-formed attestations never
// moves TargetNumber backwards. The chain deliberately starts above whatever the
// snapshot already holds — a stored attestation ahead of the header being applied
// cannot occur on an honest chain (apply() feeds headers in order, and each
// attestation targets that header's parent), so generating it would be constructing
// a decrease that production cannot reach.
func assertUpdateAttestation(t *testing.T, loaded *Snapshot, seed uint64) {
	t.Helper()

	epoch := loaded.EpochLength
	if epoch < 2 {
		// Every block is an epoch block at epoch length 1, which switches
		// getVoteAttestationFromHeader to the validator-set layout and makes the
		// header shape below meaningless. No Chiliz network runs an epoch below
		// 1200 (Scoville), so this is not a case worth constructing.
		return
	}
	luban := &params.ChainConfig{
		ChainID:    big.NewInt(714),
		LubanBlock: common.Big0,
		Parlia:     &params.ParliaConfig{Period: 3, Epoch: epoch},
	}
	preLuban := &params.ChainConfig{
		ChainID: big.NewInt(714),
		Parlia:  &params.ParliaConfig{Period: 3, Epoch: epoch},
	}

	// Start the chain above both the snapshot height and any attestation it holds,
	// and keep every number clear of an epoch boundary (number%epoch == 1).
	base := loaded.Number
	if loaded.Attestation != nil && loaded.Attestation.TargetNumber > base {
		base = loaded.Attestation.TargetNumber
	}
	// The headroom has to be computed without wrapping, and `epoch` is a raw fuzz
	// input (loaded.EpochLength comes straight from epochRaw). Writing the guard as
	// `base > ^uint64(0)-16*epoch` alone looks safe and is not: from epoch == 2^60
	// the product overflows to a small number, the right-hand side collapses to
	// ~MaxUint64, the guard can never fire, and the line below wraps `base` back to
	// near zero while `last` still holds the stored TargetNumber. The monotone half
	// then reports a finality regression that updateAttestation never performed —
	// a phantom consensus bug raised by a harness that overflowed. Corpus entry
	// epoch-2pow60-attestation-base-overflow (number MaxUint64, epoch 2^60, an
	// attestation stored at 2^63) is that input; it reported
	// "moved TargetNumber backwards at block 1: 9223372036854775808 -> 0". That was
	// this helper being wrong, not updateAttestation. The epoch clause is the fix, and
	// it must come first — it is what keeps the second clause's arithmetic in range.
	//
	// The 16*epoch term alone is not enough headroom either, and at small epochs it is
	// short by more than the slack it leaves. The alignment on the line below raises
	// `base` by up to epoch+1, and the largest offset the body then applies is
	// attestationMaxOffset (the "target is not the direct parent" case). At epoch == 2
	// the 16*epoch reserve is 32, so an aligned base can sit within 30 of MaxUint64 and
	// base+attestationMaxOffset wraps — reachable with epochRaw == 2 and `number` near
	// MaxUint64. Benign today, because a wrapped target still is not the header's
	// parent and the case still rejects, but the guard would be promising headroom it
	// does not hold, and a later probe at a larger offset would inherit a silently
	// false contract. So the reserve is derived from the offset the body actually uses
	// rather than assumed: attestationMaxOffset on top of 16*epoch. The alignment's own
	// growth needs no term of its own — epoch+1 <= 16*epoch for every epoch >= 1.
	if epoch > (^uint64(0)-attestationMaxOffset)/16 || base > ^uint64(0)-16*epoch-attestationMaxOffset {
		return // no room left in the uint64 range to walk forward; nothing to pin
	}
	base = base - base%epoch + epoch + 1

	// --- conservative half ---
	for _, tc := range []struct {
		name   string
		config *params.ChainConfig
		header *types.Header
	}{
		{
			// No attestation section at all: Extra is vanity+seal, which
			// getVoteAttestationFromHeader short-circuits on.
			name:   "no attestation section",
			config: luban,
			header: attestationFuzzHeader(base, nil),
		},
		{
			// Bytes that are not RLP: getVoteAttestationFromHeader errors and
			// updateAttestation drops the header on the floor.
			name:   "undecodable attestation",
			config: luban,
			header: attestationFuzzHeader(base, []byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa}),
		},
		{
			// Well-formed, but targeting a block that is not this header's parent.
			// Pre-Fermi this must be rejected: accepting it lets a header move a
			// node's finality view to an arbitrary height.
			name:   "target is not the direct parent",
			config: luban,
			header: attestationFuzzHeader(base, encodeFuzzAttestation(base+attestationMaxOffset-1, base+attestationMaxOffset, seed)),
		},
		{
			// Pre-Luban: fast finality does not exist yet, so the whole function
			// is a no-op regardless of what the header carries.
			name:   "pre-Luban",
			config: preLuban,
			header: attestationFuzzHeader(base, encodeFuzzAttestation(base-2, base-1, seed)),
		},
	} {
		snap := loaded.copy()
		before := snapshotFingerprint(snap)
		snap.updateAttestation(tc.header, tc.config)
		if after := snapshotFingerprint(snap); after != before {
			t.Fatalf("updateAttestation changed the snapshot for %q; it must leave the attestation untouched.\n before:\n%s\n after:\n%s", tc.name, before, after)
		}
	}

	// --- monotone half ---
	snap := loaded.copy()
	last := uint64(0)
	if snap.Attestation != nil {
		last = snap.Attestation.TargetNumber
	}
	for i := uint64(0); i < 8; i++ {
		number := base + i
		// Corpus entry 8498f3ef8c5bd0b1 (epoch length 7, blocks 169..176) found this
		// the hard way: the loop walked onto block 175, a multiple of the epoch, and
		// the assertions below then failed with "stored TargetNumber 173 for a header
		// at block 175". That was this harness being wrong, not updateAttestation.
		// On an epoch block getVoteAttestationFromHeader reads the *validator-set*
		// layout — Extra[32] is a validator count, and the attestation (if any) starts
		// after the validator bytes — so a header built with the plain
		// vanity||attestation||seal layout does not carry an attestation there at all,
		// and the correct behaviour is exactly what happened: leave the field alone.
		// Asserting "every header advances the target" over a block that, by the
		// format, carries no attestation was the bug. The input is kept as a
		// regression seed; epoch blocks are skipped here and the epoch layout stays
		// out of scope for this target (FuzzHeaderExtraLayout owns it).
		if number%epoch == 0 {
			continue
		}
		target := number - 1
		// Alternate the two branches of updateAttestation: source+1 == target
		// replaces the VoteData wholesale, source+2 == target updates only the
		// target fields in place on the existing pointer.
		// No underflow guard on target-2 is needed, and one here would be dead: the
		// alignment above makes base == (some multiple of epoch) + epoch + 1, and
		// epoch >= 2 is enforced at the top, so base >= 3 and target >= 2 always.
		source := target - 1
		if i%2 == 1 {
			source = target - 2
		}
		header := attestationFuzzHeader(number, encodeFuzzAttestation(source, target, seed+i))
		snap.updateAttestation(header, luban)

		if snap.Attestation == nil {
			t.Fatalf("updateAttestation left a nil attestation after a well-formed header at block %d", number)
		}
		if snap.Attestation.TargetNumber < last {
			t.Fatalf("updateAttestation moved TargetNumber backwards at block %d: %d -> %d — a node's finalized view must never regress",
				number, last, snap.Attestation.TargetNumber)
		}
		if snap.Attestation.TargetNumber != target {
			t.Fatalf("updateAttestation stored TargetNumber %d for a header at block %d whose attestation targets %d", snap.Attestation.TargetNumber, number, target)
		}
		last = snap.Attestation.TargetNumber
	}
}

// attestationFuzzHeader builds a non-epoch header whose Extra is
// vanity || attestation || seal — the layout getVoteAttestationFromHeader reads for
// any block that is not an epoch block. A nil payload yields the "no attestation"
// shape (Extra is exactly vanity+seal, which the function short-circuits on).
//
// ParentHash is derived from number-1 so that the pre-Fermi guard
// (targetHash == header.ParentHash) is satisfied by encodeFuzzAttestation's
// matching derivation, and violated by any other target.
func attestationFuzzHeader(number uint64, attestation []byte) *types.Header {
	extra := make([]byte, extraVanity, extraVanity+len(attestation)+extraSeal)
	extra = append(extra, attestation...)
	extra = append(extra, make([]byte, extraSeal)...)
	return &types.Header{
		Number:     new(big.Int).SetUint64(number),
		ParentHash: attestationBlockHash(number - 1),
		Time:       1_700_000_000 + number*3,
		Extra:      extra,
	}
}

// attestationBlockHash is the stand-in block hash for a height in the attestation
// checks: deterministic, distinct per height, and never the zero hash.
func attestationBlockHash(number uint64) common.Hash {
	var h common.Hash
	h[0] = 0x9a
	new(big.Int).SetUint64(number).FillBytes(h[24:])
	return h
}

// encodeFuzzAttestation RLP-encodes a VoteAttestation the way a producer embeds it.
// The aggregated signature and vote bitset are not validated by updateAttestation
// (verifyVoteAttestation does that, earlier and elsewhere), so they carry derived
// filler rather than a real BLS aggregate.
func encodeFuzzAttestation(source, target, seed uint64) []byte {
	att := &types.VoteAttestation{
		VoteAddressSet: types.ValidatorsBitSet(seed | 1), // non-zero: bitset.Count() is only a metric, but zero would be an odd thing to pin
		Data: &types.VoteData{
			SourceNumber: source,
			SourceHash:   attestationBlockHash(source),
			TargetNumber: target,
			TargetHash:   attestationBlockHash(target),
		},
	}
	copy(att.AggSignature[:], fuzzBytes(seed, "agg-signature", len(att.AggSignature)))
	encoded, err := rlp.EncodeToBytes(att)
	if err != nil {
		// The input is a fixed-shape struct of fixed-size fields; an error here
		// would be a bug in this helper, not a finding about the code under test.
		panic(fmt.Sprintf("encodeFuzzAttestation: %v", err))
	}
	return encoded
}

// snapshotFieldCount is the number of fields on Snapshot: the three unexported
// plumbing pointers (config, ethAPI, sigCache) plus the eleven serialized ones.
const snapshotFieldCount = 14

// TestSnapshotRoundTripStoredShape is the human-readable companion to the fuzz
// target: it pins the exact JSON member names a stored snapshot carries.
//
// The fuzz target compares a snapshot against itself, so it catches a field that
// fails to round trip — but it cannot catch a rename that is symmetric (marshal and
// unmarshal both move to a new name at once). A rename like that is invisible in the
// round trip and silently orphans the field on every snapshot already on disk.
//
// What that costs depends entirely on which member it is, so the example has to be one
// where nothing reconstructs the value. `recents` is the sharp one: loadSnapshot fills
// no default for it, and apply() only ever adds to the map it was handed. A node that
// restarts from a checkpoint blob whose recents member was renamed comes back with an
// empty spam-protection window, so countRecents sees nothing, SignRecently returns
// false for everyone, and errRecentlySigned stops firing — the node will accept, and
// as a validator produce, a block from a validator that just signed. `attestation`
// is the same shape one level down: it comes back nil, getFinalizedNumber() drops to
// 0, and the node's finality view resets on every restart.
//
// is_snake8_fork is deliberately NOT the example, though it is the member the fork
// added and the tempting one to name. Losing it from disk has no production effect at
// all: loadSnapshot assigns IsSnake8Fork from its argument and forces TurnLength to
// snake8TurnLength from the same argument, both after the unmarshal — that is the
// target's own property 6. The member is still worth pinning here, because a rename
// is a signal that someone edited the struct, but the reason to keep this test is the
// other nine.
func TestSnapshotRoundTripStoredShape(t *testing.T) {
	snap := buildFuzzSerdeSnapshot(42, 2, 1024, 200, 3000, snake8TurnLength, true, 2, 64, 1, []byte{0x01, 0x02, 0x03}, []byte{0x04})
	db := rawdb.NewMemoryDatabase()
	if err := snap.store(db); err != nil {
		t.Fatalf("store failed: %v", err)
	}
	blob, err := db.Get(append([]byte("parlia-"), snap.Hash[:]...))
	if err != nil {
		t.Fatalf("reading the stored blob failed: %v", err)
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(blob, &members); err != nil {
		t.Fatalf("stored blob is not a JSON object: %v", err)
	}

	// The malformed ":omitempty" spellings are deliberate and load-bearing — see the
	// note on FuzzSnapshotRoundTrip. They are on-disk names now; fixing the comma
	// would orphan the field on every existing node.
	want := []string{
		"number", "hash", "epoch_length", "block_interval", "turn_length",
		"validators", "recents", "recent_fork_hashes", "attestation:omitempty",
		"is_snake8_fork",
	}
	for _, name := range want {
		if _, ok := members[name]; !ok {
			t.Errorf("stored snapshot is missing the %q member; a renamed or dropped json tag loses this field on every node's existing on-disk snapshots", name)
		}
	}
	// COR-174: this one must NOT be there.
	if _, ok := members["frequency_rlp"]; ok {
		t.Error("stored snapshot carries frequency_rlp; store() must strip the caller-supplied per-block value (COR-174)")
	}
	if len(members) != len(want) {
		t.Errorf("stored snapshot has %d members, expected exactly %d (%v) — a new field on Snapshot needs a decision about whether it belongs on disk, and a line here", len(members), len(want), want)
	}

	// The member count above only sees fields that were actually emitted, which makes
	// it blind in exactly the case that matters most. A new field like
	// `Turn *TurnInfo \`json:"turn,omitempty"\`` left nil is shallow-copied by copy(),
	// omitted from the blob, absent from `members`, and therefore never counted and
	// never probed by assertCopyIsDeep — and the target would still read as covering
	// copy() and the stored shape. Counting the struct's fields closes that: a field
	// added to Snapshot fails here whether or not it ever reaches the JSON, forcing
	// the two decisions (does it belong on disk, and does copy() need to deep-copy it)
	// to be made rather than defaulted. Bump the number only together with a line in
	// `want` above, or a deliberate note that the new field is intentionally transient.
	if got, want := reflect.TypeOf(Snapshot{}).NumField(), snapshotFieldCount; got != want {
		t.Errorf("Snapshot now has %d fields, expected %d — a new field must be handled in store()/loadSnapshot()/copy() and accounted for here; a nil-valued one is invisible to every other check in this file", got, want)
	}

	var vals map[string]map[string]json.RawMessage
	if err := json.Unmarshal(members["validators"], &vals); err != nil {
		t.Fatalf("validators member is not an object of objects: %v", err)
	}
	for addr, info := range vals {
		for _, name := range []string{"index:omitempty", "vote_address"} {
			if _, ok := info[name]; !ok {
				t.Errorf("validator %s is missing the %q member", addr, name)
			}
		}
	}
}
