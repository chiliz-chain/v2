package parlia

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// COR-213: the pre-Luban validator parser must locate the Snake8 frequency block
// structurally, never by the first occurrence of the "VFQ" bytes. These cases
// pin every shape the fix has to get right, on the live mainnet/Spicy layout.

func cor213FrequencyTail(t *testing.T, validators []common.Address) []byte {
	t.Helper()
	entries := make([]frequencyBlockEntry, len(validators))
	for i, v := range validators {
		entries[i] = frequencyBlockEntry{Address: v, Frequency: big.NewInt(int64(1000 * (i + 1)))}
	}
	out, err := rlp.EncodeToBytes(entries)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func cor213Header(number uint64, extra []byte) *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(number), Time: 2000, Extra: extra}
}

func TestPreLubanValidatorBytesWithVFQBytes(t *testing.T) {
	cfg, _ := extraFuzzConfig(extraFuzzSelMainnet)
	epoch := cfg.Parlia.Epoch

	low := common.HexToAddress("0x0100000000000000000000000000000000000001")
	midVFQ := common.HexToAddress("0x0200000000000000000000564651000000000002")   // "VFQ" at bytes 11..13
	startVFQ := common.HexToAddress("0x5646510000000000000000000000000000000003") // "VFQ" at bytes 0..2
	qStart := common.HexToAddress("0x5100000000000000000000000000000000000005")   // 'Q' first byte
	high := common.HexToAddress("0xff00000000000000000000000000000000000004")

	vanityVFQ := []byte("chiliz-VFQ-vanity")
	vanityVF := append(bytes.Repeat([]byte{0}, extraVanity-2), 'V', 'F') // "VF" ends the vanity

	cases := []struct {
		name       string
		vanity     []byte
		validators []common.Address
		tail       []byte // nil = honest empty tail (calcFrequencyRLP failed)
		// wantNil marks the shapes 2.10.6 also rejected. COR-213 fixes the cases
		// the byte-wise scan mangled *inside the validator list*; it must not
		// start accepting headers the deployed fleet rejects, because that splits
		// the chain across a rolling upgrade instead of halting it uniformly. The
		// two survivors are a "VFQ" beginning inside the vanity (the legacy scan
		// started at offset 0, so it set end < start) and a body that begins with
		// the prefix (legacy's explicit HasPrefix guard) — i.e. the lowest-sorted
		// validator starting with it. That last corner is the one part of COR-213
		// that cannot be fixed without a hardfork.
		wantNil bool
	}{
		{"VFQ mid-address", nil, []common.Address{low, midVFQ, high}, nil, false},
		{"VFQ starts an address", nil, []common.Address{low, startVFQ, high}, nil, false},
		{"VFQ starts the last address", nil, []common.Address{low, startVFQ}, nil, false},
		{"single validator containing VFQ, empty tail", nil, []common.Address{midVFQ}, nil, false},
		{"every in-list shape, with a frequency list", nil, []common.Address{low, midVFQ, startVFQ, high}, []byte{1}, false},

		{"VFQ starts the first address", nil, []common.Address{startVFQ, high}, nil, true},
		{"VFQ in the vanity", vanityVFQ, []common.Address{low, high}, nil, true},
		{"VFQ straddles vanity and body", vanityVF, []common.Address{qStart, high}, nil, true},
		{"VFQ in the vanity, with a frequency list", vanityVFQ, []common.Address{low, midVFQ, startVFQ, high}, []byte{1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail := tc.tail
			if tail != nil {
				tail = cor213FrequencyTail(t, tc.validators)
			}
			extra := buildPreLubanSnake8Extra(tc.vanity, tc.validators, 7777, tail, nil)
			header := cor213Header(epoch, extra)

			// COR-213 deliberately changes the *result* where 2.10.6 silently
			// truncated the validator list — that is the bug. What it must not do
			// is flip an outright rejection into an acceptance: a truncation
			// differs on both sides of an upgrade and is pinned by the history
			// replay, whereas a rejection->acceptance means old nodes reject a
			// block new nodes build on. The wantNil column marks the shapes where
			// the fix must stay rejecting, and the oracle keeps it honest.
			legacy := legacyPreLubanValidatorBytes(header, epoch)
			got := getValidatorBytesFromHeader(header, cfg, epoch)
			if tc.wantNil && legacy != nil {
				t.Fatalf("fixture is mislabelled: 2.10.6 accepted this header (%d bytes)", len(legacy))
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("header rejected by 2.10.6 now yields %d validator bytes (%x) — rolling-upgrade split", len(got), got)
				}
				if _, _, err := parseValidators(header, cfg, epoch); err == nil {
					t.Fatalf("parseValidators accepted a header 2.10.6 rejected")
				}
				return
			}

			gotVals, bls, err := parseValidators(header, cfg, epoch)
			if err != nil {
				t.Fatalf("parseValidators: %v (validator bytes = %x)", err, getValidatorBytesFromHeader(header, cfg, epoch))
			}
			if bls != nil {
				t.Fatalf("unexpected BLS keys on a pre-Luban header")
			}
			if len(gotVals) != len(tc.validators) {
				t.Fatalf("parseValidators returned %d validators %v, want %d %v", len(gotVals), gotVals, len(tc.validators), tc.validators)
			}
			for i := range gotVals {
				if gotVals[i] != tc.validators[i] {
					t.Fatalf("validator %d = %x, want %x", i, gotVals[i], tc.validators[i])
				}
			}
			if ts, ok := extractSnake8ParentTimestamp(header, cfg); !ok || ts != 7777 {
				t.Fatalf("extractSnake8ParentTimestamp = (%d, %v), want (7777, true)", ts, ok)
			}
			gotTail, err := parseValidatorFrequencies(header, cfg)
			if err != nil {
				t.Fatalf("parseValidatorFrequencies: %v", err)
			}
			if !bytes.Equal(gotTail, tail) {
				t.Fatalf("frequency tail = %x, want %x", gotTail, tail)
			}
		})
	}
}

func TestPreLubanValidatorBytesPreSnake8Unchanged(t *testing.T) {
	cfg, _ := extraFuzzConfig(extraFuzzSelPreSnake8)
	epoch := cfg.Parlia.Epoch
	vals := []common.Address{
		common.HexToAddress("0x0100000000000000000000000000000000000001"),
		common.HexToAddress("0x0200000000000000000000564651000000000002"), // VFQ inside, no frequency block anywhere
		common.HexToAddress("0x5646510000000000000000000000000000000003"),
	}
	extra := make([]byte, extraVanity)
	for _, v := range vals {
		extra = append(extra, v.Bytes()...)
	}
	extra = append(extra, make([]byte, extraSeal)...)
	header := cor213Header(epoch, extra)
	got := getValidatorBytesFromHeader(header, cfg, epoch)
	if want := extra[extraVanity : len(extra)-extraSeal]; !bytes.Equal(got, want) {
		t.Fatalf("pre-Snake8 epoch header: validator bytes = %x, want the whole body %x", got, want)
	}
	if got := getValidatorBytesFromHeader(cor213Header(epoch+1, extra), cfg, epoch); got == nil {
		// Junk in a non-epoch body is returned so verifyHeader rejects it (errExtraValidators).
		t.Fatalf("non-epoch header with a non-empty body returned nil; verifyHeader would accept it")
	}
}

func TestPreLubanValidatorBytesNonEpoch(t *testing.T) {
	cfg, _ := extraFuzzConfig(extraFuzzSelMainnet)
	epoch := cfg.Parlia.Epoch
	// Snake8 non-epoch header: body is the frequency block only.
	extra := buildPreLubanSnake8Extra(nil, nil, 7777, []byte{0xc0}, nil)
	if got := getValidatorBytesFromHeader(cor213Header(epoch+1, extra), cfg, epoch); got != nil {
		t.Fatalf("Snake8 non-epoch header returned validator bytes %x", got)
	}
	// Empty body.
	empty := make([]byte, extraVanity+extraSeal)
	if got := getValidatorBytesFromHeader(cor213Header(epoch+1, empty), cfg, epoch); got != nil {
		t.Fatalf("empty non-epoch body returned %x", got)
	}
	// Legacy quirk, preserved: junk body but "VFQ" inside the vanity -> nil (accepted).
	junkVanityVFQ := append([]byte("VFQ"), make([]byte, extraVanity-3)...)
	junkVanityVFQ = append(junkVanityVFQ, 1, 2, 3)
	junkVanityVFQ = append(junkVanityVFQ, make([]byte, extraSeal)...)
	if got := getValidatorBytesFromHeader(cor213Header(epoch+1, junkVanityVFQ), cfg, epoch); got != nil {
		t.Fatalf("legacy quirk: junk body with VFQ in the vanity must yield nil, got %x", got)
	}
	// The quirk's window is bounded by the seal: with a one-byte body "F", a
	// vanity ending in "V" and a seal starting with "Q", the legacy loop
	// (i <= end-3) never saw the straddling "VFQ" and returned the body for
	// rejection; the new code must too (review finding on PR #72).
	straddle := append(bytes.Repeat([]byte{0}, extraVanity-1), 'V', 'F')
	straddle = append(straddle, 'Q')
	straddle = append(straddle, make([]byte, extraSeal-1)...)
	if got := getValidatorBytesFromHeader(cor213Header(epoch+1, straddle), cfg, epoch); !bytes.Equal(got, []byte{'F'}) {
		t.Fatalf("one-byte body straddling into the seal = %x, want the body returned for rejection", got)
	}
	// With a two-byte body "FQ" the legacy loop did see the straddle -> nil.
	straddle2 := append(bytes.Repeat([]byte{0}, extraVanity-1), 'V', 'F', 'Q')
	straddle2 = append(straddle2, make([]byte, extraSeal)...)
	if got := getValidatorBytesFromHeader(cor213Header(epoch+1, straddle2), cfg, epoch); got != nil {
		t.Fatalf("two-byte body straddle must yield nil like the legacy scan, got %x", got)
	}
	// Junk body without the quirk -> returned, so verifyHeader rejects it.
	junk := append(make([]byte, extraVanity), 1, 2, 3)
	junk = append(junk, make([]byte, extraSeal)...)
	if got := getValidatorBytesFromHeader(cor213Header(epoch+1, junk), cfg, epoch); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("junk non-epoch body = %x, want it returned for rejection", got)
	}
}

func TestPreLubanValidatorBytesMalformedTail(t *testing.T) {
	cfg, _ := extraFuzzConfig(extraFuzzSelMainnet)
	epoch := cfg.Parlia.Epoch
	vals := []common.Address{
		common.HexToAddress("0x0100000000000000000000000000000000000001"),
		common.HexToAddress("0xff00000000000000000000000000000000000004"),
	}
	// A tail that is neither empty nor an RLP entry list is not a frequency
	// block, so the body has no candidate and falls back to the pre-Snake8 shape:
	// nil when the length is not a multiple of 20 ...
	extra := buildPreLubanSnake8Extra(nil, vals, 7777, []byte("nope"), nil)
	if got := getValidatorBytesFromHeader(cor213Header(epoch, extra), cfg, epoch); got != nil {
		t.Fatalf("malformed tail: got validator bytes %x, want nil", got)
	}
	// ... and the whole body when it is, in which case the Snake8 offset walkers
	// find no frequency block and verifySnake8Extra rejects the header.
	extra = buildPreLubanSnake8Extra(nil, vals, 7777, bytes.Repeat([]byte{0x01}, 9), nil) // 40 + 11 + 9 = 60
	header := cor213Header(epoch, extra)
	if got := getValidatorBytesFromHeader(header, cfg, epoch); len(got) != 60 {
		t.Fatalf("malformed 60-byte body: got %d validator bytes, want the whole body", len(got))
	}
	if _, ok := extractSnake8ParentTimestamp(header, cfg); ok {
		t.Fatalf("malformed tail must not yield a parent timestamp")
	}
}

func TestPreLubanFrequencyBlockOffset(t *testing.T) {
	addr := func(b byte) []byte { return bytes.Repeat([]byte{b}, validatorBytesLengthBeforeLuban) }
	vfqBlock := func(tail []byte) []byte {
		return append(append(append([]byte{}, validatorFrequencyDataPrefix...), make([]byte, 8)...), tail...)
	}
	list := cor213FrequencyTail(t, []common.Address{common.BytesToAddress(addr(1))})
	check := func(name string, body []byte, wantOff int, wantFound, wantHostile bool) {
		t.Helper()
		off, found, hostile := preLubanFrequencyBlockOffset(body)
		if off != wantOff || found != wantFound || hostile != wantHostile {
			t.Fatalf("%s: (%d, %v, %v), want (%d, %v, %v)", name, off, found, hostile, wantOff, wantFound, wantHostile)
		}
	}

	check("pre-Snake8 body", append(addr(1), addr(2)...), 0, false, false)
	check("empty tail", append(append(addr(1), addr(2)...), vfqBlock(nil)...), 40, true, false)
	check("list tail", append(addr(1), vfqBlock(list)...), 20, true, false)
	check("frequency block only", vfqBlock(list), 0, true, false)
	// Prefix at an aligned offset with a junk tail is not a candidate.
	fake := append([]byte("VFQ"), bytes.Repeat([]byte{0xee}, 17)...)
	check("junk-tail decoy", append(append(addr(1), fake...), vfqBlock(list)...), 40, true, false)
	// Prefix at an unaligned offset is never a candidate.
	check("unaligned decoy", append(append(addr(1), []byte("xxxxxVFQxxxxxxxxxxxx")...), vfqBlock(list)...), 40, true, false)
	// A complete block placed before the real one is not a candidate either: its
	// tail runs through the real block and has trailing data.
	decoy := vfqBlock(list)
	for len(decoy)%validatorBytesLengthBeforeLuban != 0 {
		decoy = append(decoy, 0)
	}
	check("padded decoy", append(append(addr(1), decoy...), vfqBlock(list)...), 20+len(decoy), true, false)

	// The feasible forgery: the attacker's own address, inside the honest
	// frequency list, carries "VFQ" so that it lands on an aligned offset exactly
	// eleven bytes before the seal, forming an empty-tail candidate. The
	// decodable candidate must win. Layout: 1 validator (20) + block header (11)
	// + list (100) = 131 bytes; the decoy sits at offset 120 = list[89:92].
	attacker := common.BytesToAddress(append(bytes.Repeat([]byte{0xaa}, 12), append([]byte("VFQ"), bytes.Repeat([]byte{0xaa}, 5)...)...))
	entries := []frequencyBlockEntry{
		{Address: common.BytesToAddress(addr(1)), Frequency: big.NewInt(1)},
		{Address: common.BytesToAddress(addr(2)), Frequency: big.NewInt(1000)},
		{Address: common.BytesToAddress(addr(3)), Frequency: big.NewInt(1000)},
		{Address: attacker, Frequency: big.NewInt(1000)},
	}
	forgedList, err := rlp.EncodeToBytes(entries)
	if err != nil {
		t.Fatal(err)
	}
	body := append(addr(9), vfqBlock(forgedList)...)
	if len(body) != 131 || !bytes.Equal(body[120:123], validatorFrequencyDataPrefix) {
		t.Fatalf("fixture drift: len(body)=%d body[120:123]=%x", len(body), body[120:123])
	}
	check("empty-tail decoy after the real block", body, 20, true, false)
	// And as a header: the single validator round-trips.
	cfg, _ := extraFuzzConfig(extraFuzzSelMainnet)
	epoch := cfg.Parlia.Epoch
	extra := append(append(make([]byte, extraVanity), body...), make([]byte, extraSeal)...)
	if got, _, err := parseValidators(cor213Header(epoch, extra), cfg, epoch); err != nil || len(got) != 1 || got[0] != common.BytesToAddress(addr(9)) {
		t.Fatalf("decoy header: parseValidators = %v, %v", got, err)
	}

	// Too many prefixed offsets to examine: hostile.
	var flood []byte
	for i := 0; i <= maxFrequencyBlockCandidates; i++ {
		flood = append(flood, fake...)
	}
	flood = append(addr(1), flood...)
	check("candidate flood", flood, 0, false, true)
	floodExtra := append(append(make([]byte, extraVanity), flood...), make([]byte, extraSeal)...)
	if got := getValidatorBytesFromHeader(cor213Header(epoch, floodExtra), cfg, epoch); got != nil {
		t.Fatalf("candidate flood must yield nil validator bytes, got %d bytes", len(got))
	}
}

// TestPreLubanFrequencyEmptyListDecoy builds the cheapest forgery a validator
// who controls only their own address and stake can mount against the
// structural search, and pins that it does not fire.
//
// The empty RLP list 0xc0 is one byte. If decodesAsFrequencyList accepted it,
// an attacker whose address carries "VFQ" at bytes 11..13 (a ~2^24 address
// grind), whose frequency encodes with 0xc0 as its last byte and whose entry
// leaves the list length ≡ 1 (mod 20) would put a second decodable candidate
// exactly twelve bytes before the seal — inside the honest frequency list they
// otherwise have no control over. Two decodable candidates are hostile, so
// getValidatorBytesFromHeader returns nil and every node rejects the epoch
// block: the chain halt COR-213 is about, re-introduced by the fix for it.
// calcFrequencyRLP never emits an empty list (it errors instead), so the entry
// requirement costs honest headers nothing.
func TestPreLubanFrequencyEmptyListDecoy(t *testing.T) {
	if decodesAsFrequencyList([]byte{0xc0}) {
		t.Fatalf("the empty RLP list must not count as a frequency list")
	}
	// The producer-side half of that rule: calcFrequencyRLP fails rather than
	// encoding an empty candidate set, so no honest header carries 0xc0. If this
	// ever changes, the parser above has to change with it.
	only := common.BytesToAddress(bytes.Repeat([]byte{7}, validatorBytesLengthBeforeLuban))
	snap := &Snapshot{Validators: map[common.Address]*ValidatorInfo{only: {}}}
	if out, err := snap.calcFrequencyRLP(map[common.Address]*big.Int{only: big.NewInt(0)}); err == nil || out != nil {
		t.Fatalf("calcFrequencyRLP with no eligible validators = (%x, %v), want (nil, error)", out, err)
	}
	if out, err := (&Snapshot{}).calcFrequencyRLP(nil); err == nil || out != nil {
		t.Fatalf("calcFrequencyRLP with an empty validator set = (%x, %v), want (nil, error)", out, err)
	}

	validator := common.BytesToAddress(bytes.Repeat([]byte{9}, validatorBytesLengthBeforeLuban))
	// "VFQ" at bytes 11..13, sorting last so the entry ends the list.
	attacker := common.BytesToAddress(append(append(
		bytes.Repeat([]byte{0xfe}, 11), validatorFrequencyDataPrefix...),
		bytes.Repeat([]byte{0xff}, 6)...))

	// Search the honest degrees of freedom the attacker can steer (entry count
	// and their own frequency's byte width) for a list whose length is ≡ 1 mod
	// 20 and whose last byte is 0xc0 — the layout that lands the decoy on a
	// 20-aligned offset eleven bytes before the end of the body.
	var body []byte
	var decoy int
	for fillers := 0; fillers < 40 && body == nil; fillers++ {
		for w := 1; w <= 8; w++ {
			entries := make([]frequencyBlockEntry, 0, fillers+1)
			for i := 0; i < fillers; i++ {
				entries = append(entries, frequencyBlockEntry{
					Address:   common.BytesToAddress(append(bytes.Repeat([]byte{1}, 19), byte(i))),
					Frequency: new(big.Int).Lsh(big.NewInt(1), uint(8*(w-1))),
				})
			}
			entries = append(entries, frequencyBlockEntry{Address: attacker, Frequency: big.NewInt(0x11c0)})
			list, err := rlp.EncodeToBytes(entries)
			if err != nil {
				t.Fatal(err)
			}
			if len(list)%validatorBytesLengthBeforeLuban != 1 || list[len(list)-1] != 0xc0 {
				continue
			}
			candidate := append(validator.Bytes(),
				append(append(append([]byte{}, validatorFrequencyDataPrefix...), make([]byte, 8)...), list...)...)
			off := len(candidate) - 12
			if off%validatorBytesLengthBeforeLuban != 0 || !bytes.Equal(candidate[off:off+3], validatorFrequencyDataPrefix) {
				continue
			}
			body, decoy = candidate, off
			break
		}
	}
	if body == nil {
		t.Fatalf("could not construct the decoy layout; the fixture needs revisiting, not the assertion")
	}
	if decoy == validatorBytesLengthBeforeLuban {
		t.Fatalf("fixture drift: the decoy coincides with the real block at %d", decoy)
	}

	off, found, hostile := preLubanFrequencyBlockOffset(body)
	if off != validatorBytesLengthBeforeLuban || !found || hostile {
		t.Fatalf("empty-list decoy at %d defeated the search: (%d, %v, %v), want (%d, true, false)",
			decoy, off, found, hostile, validatorBytesLengthBeforeLuban)
	}

	cfg, _ := extraFuzzConfig(extraFuzzSelMainnet)
	epoch := cfg.Parlia.Epoch
	extra := append(append(make([]byte, extraVanity), body...), make([]byte, extraSeal)...)
	header := cor213Header(epoch, extra)
	gotVals, _, err := parseValidators(header, cfg, epoch)
	if err != nil || len(gotVals) != 1 || gotVals[0] != validator {
		t.Fatalf("decoy header: parseValidators = %v, %v; want the single validator %x", gotVals, err, validator)
	}
	if ts, ok := extractSnake8ParentTimestamp(header, cfg); !ok || ts != 0 {
		t.Fatalf("decoy header: extractSnake8ParentTimestamp = (%d, %v), want (0, true)", ts, ok)
	}
}

// legacyPreLubanValidatorBytes is the pre-Luban branch of
// getValidatorBytesFromHeader exactly as 2.10.6 shipped it. It is the oracle for
// the wantNil column above: COR-213 may only *widen* what parses, never narrow
// it, and may not widen it on the two shapes the deployed fleet rejects.
func legacyPreLubanValidatorBytes(header *types.Header, epochLength uint64) []byte {
	if len(header.Extra) <= extraVanity+extraSeal {
		return nil
	}
	start := extraVanity
	end := len(header.Extra) - extraSeal
	if bytes.HasPrefix(header.Extra[start:end], validatorFrequencyDataPrefix) {
		return nil
	}
	for i := 0; i <= end-3; i++ {
		if bytes.Equal(header.Extra[i:i+3], validatorFrequencyDataPrefix) {
			end = i
			break
		}
	}
	if end <= start {
		return nil
	}
	if header.Number.Uint64()%epochLength == 0 && (end-start)%validatorBytesLengthBeforeLuban != 0 {
		return nil
	}
	return header.Extra[start:end]
}
