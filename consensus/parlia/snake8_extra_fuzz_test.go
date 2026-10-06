package parlia

import (
	"encoding/binary"
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// COR-232: native fuzz coverage of the Snake8 fork-activation decision.
//
// COR-208 fuzzes the layout parsers underneath (snake8FreqDataOffset,
// extractSnake8ParentTimestamp); this target fuzzes the two functions built on
// top of them, which decide which side of the Snake8 fork a block is judged on:
//
//	isSnake8Enabled    parlia.go — provisional, may fall back to producer bytes
//	verifySnake8Extra  parlia.go — authoritative, decides from the real parent
//
// The pair is the COR-197 forgery guard. isSnake8Enabled prefers the trusted
// parent.Time and only falls back to the timestamp embedded in header.Extra
// when the parent is not in the local store (batch sync). That embedded value
// is producer-controlled, so the fallback is deliberately provisional:
// verifySnake8Extra cross-checks it against the real parent during full
// verification, and a forged value must never survive.
//
// The oracle is construction, not re-parsing: the target assembles every Extra
// itself, so it knows independently whether a well-formed frequency block is
// present and what timestamp is embedded in it.

// Where Snake8 sits relative to the parent's timestamp. The boundary cases
// matter: IsSnake8 is `>=`, so a fork scheduled exactly at parent.Time is
// active and one second later is not.
const (
	snake8ExtraForkUnscheduled  uint8 = iota // Snake8Time nil — inactive forever
	snake8ExtraForkGenesis                   // 0 — active for every parent
	snake8ExtraForkAtParent                  // == parent.Time — active (boundary)
	snake8ExtraForkAfterParent               // == parent.Time+1 — inactive (boundary)
	snake8ExtraForkBeforeParent              // == parent.Time-1 — active
	snake8ExtraForkMax                       // MaxUint64 — inactive short of saturation
	snake8ExtraForkCount
)

// Header layout: the config (pre-/post-Luban) crossed with the block position
// (epoch/non-epoch). **No Chiliz network schedules Luban** — not mainnet, not
// Spicy, not Scoville (no `lubanBlock` in any config/embedded/*.json) and not
// the devnet (`genesis/create-genesis.go` sets `LubanBlock: nil`). The two Luban
// layouts are defence-in-depth against a future schedule or an upstream merge,
// not live shapes; do not read them as a reason to prioritise or tighten that
// path. Line 184 words the same fact correctly for the epoch case.
//
// Three of the four move the frequency block's offset. The fourth,
// snake8ExtraLayoutLubanNonEpoch, is a control: snake8FreqDataOffset returns
// extraVanity for any non-epoch header before it consults IsLuban, so enabling
// Luban must NOT move a non-epoch offset. It is cheap and it fails loudly if
// that early return ever grows a Luban branch.
const (
	snake8ExtraLayoutPreLubanNonEpoch uint8 = iota // block starts at the vanity
	snake8ExtraLayoutLubanNonEpoch                 // same, post-Luban config
	snake8ExtraLayoutLubanEpoch                    // vanity | count | count*68 | block
	snake8ExtraLayoutPreLubanEpoch                 // vanity | n*20 | block (COR-213 structural)
	snake8ExtraLayoutCount
)

// What the body carries. Only snake8ExtraShapeWellFormed produces a frequency
// block the extractor may accept; every other shape must read as "absent",
// whatever the layout.
const (
	snake8ExtraShapeWellFormed      uint8 = iota // "VFQ" | LE parent ts | frequency tail
	snake8ExtraShapeAbsent                       // no block at all
	snake8ExtraShapeWrongPrefix                  // "XXX" in place of "VFQ"
	snake8ExtraShapeTruncatedTS                  // prefix present, timestamp spills into the seal
	snake8ExtraShapeTooShort                     // Extra shorter than vanity+seal
	snake8ExtraShapeAttestationLike              // RLP-list-looking bytes (a vote attestation)
	snake8ExtraShapeCount
)

// What the producer embedded as the parent timestamp. Everything other than
// snake8ExtraTSParent is a forgery whenever it differs from the real parent.
const (
	snake8ExtraTSParent       uint8 = iota // honest
	snake8ExtraTSParentPlus1               // off by one
	snake8ExtraTSParentMinus1              // off by one the other way
	snake8ExtraTSZero
	snake8ExtraTSMax
	snake8ExtraTSRaw // whatever the fuzzer supplied
	snake8ExtraTSCount
)

// snake8ExtraFuzzForkTime maps a selector onto a Snake8Time relative to the
// parent's timestamp, saturating rather than wrapping at the ends of the range.
func snake8ExtraFuzzForkTime(sel uint8, parentTime uint64) *uint64 {
	var t uint64
	switch sel % snake8ExtraForkCount {
	case snake8ExtraForkUnscheduled:
		return nil
	case snake8ExtraForkGenesis:
		t = 0
	case snake8ExtraForkAtParent:
		t = parentTime
	case snake8ExtraForkAfterParent:
		if parentTime == math.MaxUint64 {
			t = math.MaxUint64 // saturated: stays active, still a legal schedule
		} else {
			t = parentTime + 1
		}
	case snake8ExtraForkBeforeParent:
		if parentTime == 0 {
			t = 0
		} else {
			t = parentTime - 1
		}
	default:
		t = math.MaxUint64
	}
	return &t
}

// snake8ExtraFuzzEmbeddedTS maps a selector onto the timestamp the producer
// writes into the frequency block.
func snake8ExtraFuzzEmbeddedTS(sel uint8, parentTime, raw uint64) uint64 {
	switch sel % snake8ExtraTSCount {
	case snake8ExtraTSParent:
		return parentTime
	case snake8ExtraTSParentPlus1:
		if parentTime == math.MaxUint64 {
			return 0
		}
		return parentTime + 1
	case snake8ExtraTSParentMinus1:
		if parentTime == 0 {
			return math.MaxUint64
		}
		return parentTime - 1
	case snake8ExtraTSZero:
		return 0
	case snake8ExtraTSMax:
		return math.MaxUint64
	default:
		return raw
	}
}

// snake8ExtraFuzzConfig returns the chain config for a layout. Snake8Time is
// set by the caller; London/Bohr stay unset so IsBohr is false and no
// turn-length byte shifts the frequency block.
func snake8ExtraFuzzConfig(layout uint8) *params.ChainConfig {
	cfg := &params.ChainConfig{
		ChainID: big.NewInt(88888),
		Parlia:  &params.ParliaConfig{Epoch: snake8TestEpoch, Period: 3},
	}
	switch layout % snake8ExtraLayoutCount {
	case snake8ExtraLayoutLubanNonEpoch, snake8ExtraLayoutLubanEpoch:
		cfg.LubanBlock = big.NewInt(0)
	}
	return cfg
}

// snake8ExtraFuzzIsEpoch reports whether a layout puts the header on an epoch
// boundary, where a validator section precedes the frequency block.
func snake8ExtraFuzzIsEpoch(layout uint8) bool {
	switch layout % snake8ExtraLayoutCount {
	case snake8ExtraLayoutLubanEpoch, snake8ExtraLayoutPreLubanEpoch:
		return true
	}
	return false
}

// snake8ExtraFuzzValidators returns nVals*20 bytes of filler for a pre-Luban
// epoch header's validator section, with every 0x56 rewritten so the section
// cannot contain the "VFQ" prefix. Without that, fuzzed filler could plant a
// second structural candidate and make the frequency block's location — and
// therefore this target's construction-based oracle — ambiguous. Locating the
// block among decoys is COR-208/COR-213's property, not this target's.
//
// The post-Luban counterpart is scoped out for the same reason: the generator
// always writes a truthful validator count byte, so the one producer-controlled
// field that moves a Luban epoch header's offset is never forged here. A header
// declaring zero validators while carrying entries would make the extractor read
// its "parent timestamp" out of attacker bytes — the post-Luban shape of COR-197
// — but forging it would desynchronise this target's construction-based oracle.
// COR-208's FuzzHeaderExtraLayout fuzzes raw Extra bytes and reaches it; the
// guard itself still holds either way, since verifySnake8Extra compares against
// the real parent.Time wherever the embedded value came from. No Chiliz network
// schedules Luban today.
func snake8ExtraFuzzValidators(nVals int, payload []byte) []byte {
	// The caller derives nVals from len(payload), so payload is non-empty
	// whenever this loop runs at all.
	out := make([]byte, nVals*validatorBytesLengthBeforeLuban)
	for i := range out {
		b := payload[i%len(payload)]
		if b == validatorFrequencyDataPrefix[0] {
			b++
		}
		out[i] = b
	}
	return out
}

// snake8ExtraFuzzTail returns the bytes that follow the embedded timestamp.
//
// On a pre-Luban epoch header the tail is not inert: preLubanFrequencyBlockOffset
// locates the block by finding the one aligned "VFQ" whose tail decodes as a
// frequency list, so the tail must be a real encoding for the block to be found
// at all. Everywhere else nothing parses these bytes and the fuzzer's own
// payload goes in unchanged.
func snake8ExtraFuzzTail(t *testing.T, layout uint8, payload []byte) []byte {
	t.Helper()
	if layout%snake8ExtraLayoutCount != snake8ExtraLayoutPreLubanEpoch {
		return payload
	}
	n := 1 + len(payload)%3
	addrs := make([]common.Address, n)
	for i := range addrs {
		addrs[i][common.AddressLength-1] = byte(i + 1)
	}
	return cor213FrequencyTail(t, addrs)
}

// snake8ExtraFuzzExtra assembles header.Extra for one (layout, shape) pair and
// reports whether the result carries a well-formed frequency block — known by
// construction, never by asking the parser under test.
func snake8ExtraFuzzExtra(t *testing.T, layout, shape uint8, embeddedTS uint64, payload []byte) (extra []byte, wellFormed bool) {
	t.Helper()

	if shape%snake8ExtraShapeCount == snake8ExtraShapeTooShort {
		// Below vanity+seal every parser bails before reading a body byte.
		return make([]byte, extraVanity+extraSeal-1), false
	}

	extra = make([]byte, extraVanity)

	if snake8ExtraFuzzIsEpoch(layout) {
		nVals := len(payload) % 4
		if layout%snake8ExtraLayoutCount == snake8ExtraLayoutLubanEpoch {
			// Post-Luban: a count byte, then count fixed-width entries.
			extra = append(extra, byte(nVals))
			extra = append(extra, make([]byte, nVals*validatorBytesLength)...)
		} else {
			extra = append(extra, snake8ExtraFuzzValidators(nVals, payload)...)
		}
	}

	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, embeddedTS)

	switch shape % snake8ExtraShapeCount {
	case snake8ExtraShapeWellFormed:
		extra = append(extra, validatorFrequencyDataPrefix...)
		extra = append(extra, ts...)
		extra = append(extra, snake8ExtraFuzzTail(t, layout, payload)...)
		wellFormed = true
	case snake8ExtraShapeAbsent:
		// Nothing between the validator section and the seal.
	case snake8ExtraShapeWrongPrefix:
		extra = append(extra, []byte("XXX")...)
		extra = append(extra, ts...)
		extra = append(extra, snake8ExtraFuzzTail(t, layout, payload)...)
	case snake8ExtraShapeTruncatedTS:
		// Prefix present but only half a timestamp: the read would reach into
		// the seal, so the extractor must refuse it.
		extra = append(extra, validatorFrequencyDataPrefix...)
		extra = append(extra, ts[:4]...)
	case snake8ExtraShapeAttestationLike:
		// An honest pre-fork body whose first byte is an RLP list header, as a
		// vote attestation's is. Must not be mistaken for a frequency block.
		extra = append(extra, []byte{0xc4, 0x83, 'a', 'b', 'c'}...)
	}

	return append(extra, make([]byte, extraSeal)...), wellFormed
}

// FuzzVerifySnake8ExtraPair fuzzes the Snake8 activation decision over
// (parent, header) pairs: a parent the chain reader may or may not know, a
// header whose Extra is assembled from a known layout and shape, and a Snake8
// schedule placed on either side of the parent's timestamp.
//
// The properties, in the order CLAUDE.md section 2 and COR-197 state them:
//
//  1. With the parent known, activation is decided from the trusted parent
//     alone: isSnake8Enabled == IsSnake8(parent.Time) for every embedded value,
//     forged or not. With the parent absent it falls back to the embedded value
//     when one is well-formed, and to false otherwise — provisional by design.
//  2. A forged embedded timestamp never survives verification: post-fork, any
//     value other than the real parent.Time yields errMismatchedSnake8ParentTime.
//  3. A pre-fork header must not carry a frequency block (errInvalidSnake8Extra).
//  4. A post-fork header must carry one (errInvalidSnake8Extra).
//  5. Neither function panics on attacker-controlled bytes.
//  6. No crossing: verifySnake8Extra returns nil only when fork activation and
//     the presence of a frequency block agree.
func FuzzVerifySnake8ExtraPair(f *testing.F) {
	const (
		liveTime = uint64(1784407964) // a real Chiliz mainnet block timestamp
		payload  = "\xc0"             // an inert frequency tail; only the pre-Luban epoch layout parses one
	)

	// --- Parent known vs absent, crossed with the embedded values named in
	// the issue: correct, off by one either way, zero, and MaxUint64. ---
	// Honest post-fork block: the only combination that must verify clean.
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(false, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte(payload))
	// Off by one: the cheapest forgery, and the one an honest producer racing
	// the clock could otherwise be mistaken for.
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParentPlus1, uint64(0), []byte(payload))
	f.Add(false, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParentMinus1, uint64(0), []byte(payload))
	// The COR-197 attack itself: a known parent crossed with an embedded value
	// on the OTHER side of the fork, both ways round. These two are the only
	// seeds on which "decide from the parent" and "decide from the embedded
	// bytes" disagree while the block is otherwise well-formed — every other
	// combination has the two agreeing by accident.
	f.Add(true, liveTime, snake8ExtraForkAtParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSZero, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkAfterParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSMax, uint64(0), []byte(payload))
	// Zero and MaxUint64: the two values that flip the fallback's verdict
	// wholesale when the parent is absent.
	f.Add(false, liveTime, snake8ExtraForkAtParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSZero, uint64(0), []byte(payload))
	f.Add(false, liveTime, snake8ExtraForkAtParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSMax, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkAtParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSMax, uint64(0), []byte(payload))

	// --- The fork boundary itself: scheduled exactly at the parent's time
	// (active) and one second after it (not). Same header both times, so the
	// schedule is the only thing separating accept from reject. ---
	f.Add(true, liveTime, snake8ExtraForkAfterParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkAfterParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeAbsent, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkAtParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeAbsent, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkBeforeParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte(payload))
	// Unscheduled and never-scheduled: no Chiliz network has to run Snake8.
	f.Add(true, liveTime, snake8ExtraForkUnscheduled, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeAbsent, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkMax, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte(payload))

	// --- Malformed and decoy bodies. ---
	// Shorter than vanity+seal.
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeTooShort, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(false, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeTooShort, snake8ExtraTSParent, uint64(0), []byte(payload))
	// Pre-fork header carrying attestation-like bytes — the
	// TestVerifySnake8ExtraPreForkAttestationLikeBytes fixture, which must be
	// accepted, paired with the same bytes post-fork, which must not.
	f.Add(true, liveTime, snake8ExtraForkAfterParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeAttestationLike, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeAttestationLike, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWrongPrefix, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeTruncatedTS, snake8ExtraTSParent, uint64(0), []byte(payload))

	// --- Each layout carries the frequency block at a different offset, so
	// every one needs its own accept/reject pair. ---
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutLubanEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutLubanEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParentPlus1, uint64(0), []byte{1, 2, 3})
	f.Add(true, liveTime, snake8ExtraForkAfterParent, snake8ExtraLayoutLubanEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte{1, 2, 3})
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte{1, 2, 3})
	f.Add(false, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParentMinus1, uint64(0), []byte{1, 2, 3})
	f.Add(true, liveTime, snake8ExtraForkAfterParent, snake8ExtraLayoutPreLubanEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte{1, 2, 3})
	f.Add(true, liveTime, snake8ExtraForkGenesis, snake8ExtraLayoutPreLubanEpoch, snake8ExtraShapeAbsent, snake8ExtraTSParent, uint64(0), []byte{1, 2, 3})

	// --- Range ends: parent.Time at 0 and MaxUint64, where the boundary
	// selectors saturate. ---
	f.Add(true, uint64(0), snake8ExtraForkAtParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParent, uint64(0), []byte(payload))
	f.Add(true, uint64(math.MaxUint64), snake8ExtraForkAfterParent, snake8ExtraLayoutPreLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSParentPlus1, uint64(0), []byte(payload))
	f.Add(false, uint64(0), snake8ExtraForkUnscheduled, snake8ExtraLayoutLubanNonEpoch, snake8ExtraShapeWellFormed, snake8ExtraTSRaw, uint64(math.MaxUint64), []byte(payload))

	f.Fuzz(func(t *testing.T, parentKnown bool, parentTime uint64, forkSel, layoutSel, shapeSel, tsSel uint8, rawTS uint64, payload []byte) {
		cfg := snake8ExtraFuzzConfig(layoutSel)
		cfg.Snake8Time = snake8ExtraFuzzForkTime(forkSel, parentTime)

		number := uint64(snake8TestEpoch + 1)
		if snake8ExtraFuzzIsEpoch(layoutSel) {
			number = snake8TestEpoch
		}

		embeddedTS := snake8ExtraFuzzEmbeddedTS(tsSel, parentTime, rawTS)
		extra, wellFormed := snake8ExtraFuzzExtra(t, layoutSel, shapeSel, embeddedTS, payload)

		parent := &types.Header{
			Number: new(big.Int).SetUint64(number - 1),
			Time:   parentTime,
			Extra:  make([]byte, extraVanity+extraSeal),
		}
		headerTime := parentTime
		if parentTime < math.MaxUint64-cfg.Parlia.Period {
			headerTime += cfg.Parlia.Period
		}
		header := &types.Header{
			Number:     new(big.Int).SetUint64(number),
			ParentHash: parent.Hash(),
			Time:       headerTime,
			Extra:      extra,
		}

		reader := &prepareTestChainReader{config: cfg, headers: map[common.Hash]*types.Header{}}
		if parentKnown {
			reader.headers[parent.Hash()] = parent
		}

		p := &Parlia{chainConfig: cfg}

		// Activation is computed here rather than read from cfg.IsSnake8, which is
		// the very function both subjects call: an oracle built on it follows the
		// boundary wherever it moves. Flipping isTimestampForked from `*s <= head`
		// to `*s < head` shifts every Chiliz timestamp fork one second later, and
		// with an aliased oracle the "exactly at parent.Time is active, one second
		// later is not" seeds below would stop separating anything while staying
		// green. No other test in the tree pins that boundary either.
		snake8Active := func(t uint64) bool {
			return cfg.Snake8Time != nil && t >= *cfg.Snake8Time
		}
		active := snake8Active(parentTime)

		// Property 1 + 5: the provisional decision, and no panic on it.
		var enabled bool
		noPanic(t, "isSnake8Enabled", func() { enabled = p.isSnake8Enabled(reader, header) })

		wantEnabled := false
		switch {
		case parentKnown:
			// The trusted parent decides, whatever the producer embedded.
			wantEnabled = active
		case wellFormed:
			// Batch sync: provisionally believe the producer, pending
			// verifySnake8Extra.
			wantEnabled = snake8Active(embeddedTS)
		}
		if enabled != wantEnabled {
			t.Fatalf("isSnake8Enabled = %v, want %v (parentKnown=%v parent.Time=%d embedded=%d wellFormed=%v snake8Time=%v)",
				enabled, wantEnabled, parentKnown, parentTime, embeddedTS, wellFormed, derefTime(cfg.Snake8Time))
		}

		// Properties 2, 3, 4 + 5: the authoritative verdict.
		var err error
		noPanic(t, "verifySnake8Extra", func() { err = p.verifySnake8Extra(header, parent) })

		var wantErr error
		switch {
		case active && !wellFormed:
			wantErr = errInvalidSnake8Extra // property 4
		case active && embeddedTS != parentTime:
			wantErr = errMismatchedSnake8ParentTime // property 2 — the COR-197 hole
		case !active && wellFormed:
			wantErr = errInvalidSnake8Extra // property 3
		}
		// Property 6 (no crossing) is a consequence of the switch above, not a
		// separate check: wantErr is nil only when active == wellFormed and the
		// embedded timestamp matches, so an accepted header cannot straddle the
		// fork boundary. Asserting it again after this comparison could never
		// fire, and would relax in step with the switch if the switch were ever
		// loosened.
		if err != wantErr {
			t.Fatalf("verifySnake8Extra = %v, want %v (active=%v wellFormed=%v parent.Time=%d embedded=%d layout=%d shape=%d)",
				err, wantErr, active, wellFormed, parentTime, embeddedTS, layoutSel%snake8ExtraLayoutCount, shapeSel%snake8ExtraShapeCount)
		}
	})
}
