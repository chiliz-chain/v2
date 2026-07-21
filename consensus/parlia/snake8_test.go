package parlia

import (
	"encoding/binary"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// These tests cover the BLK-3750 / COR-35 hardening of the Snake8 fork's
// validator-frequency extra-data handling: the bounds-checked parent-timestamp
// extractor, the authoritative cross-validation against the real parent, and the
// round-robin fallback when frequency RLP is malformed.

const (
	snake8TestEpoch     = 200
	snake8TestActivTime = 1000 // Snake8 activates at parent.Time >= 1000
)

// snake8TestConfig returns a ChainConfig with Snake8 active at snake8TestActivTime
// and Luban active from genesis (so the post-Luban epoch layout is exercised).
func snake8TestConfig() *params.ChainConfig {
	st := uint64(snake8TestActivTime)
	return &params.ChainConfig{
		ChainID:    big.NewInt(1),
		Parlia:     &params.ParliaConfig{Epoch: snake8TestEpoch, Period: 3},
		Snake8Time: &st,
		LubanBlock: big.NewInt(0),
	}
}

// buildNonEpochExtra builds a non-epoch-block Extra field. When withFreq is true
// it embeds a well-formed frequency block (prefix + little-endian parentTS +
// freqData) between the vanity and the seal.
func buildNonEpochExtra(parentTS uint64, withFreq bool, freqData []byte) []byte {
	extra := make([]byte, extraVanity)
	if withFreq {
		extra = append(extra, validatorFrequencyDataPrefix...)
		ts := make([]byte, 8)
		binary.LittleEndian.PutUint64(ts, parentTS)
		extra = append(extra, ts...)
		extra = append(extra, freqData...)
	}
	return append(extra, make([]byte, extraSeal)...)
}

func TestExtractSnake8ParentTimestamp(t *testing.T) {
	cfg := snake8TestConfig()
	nonEpochNum := big.NewInt(snake8TestEpoch + 1) // not a multiple of the epoch

	tests := []struct {
		name     string
		extra    []byte
		wantOK   bool
		wantTime uint64
	}{
		{
			name:     "well-formed frequency block",
			extra:    buildNonEpochExtra(1234, true, []byte{0xc0}),
			wantOK:   true,
			wantTime: 1234,
		},
		{
			name:   "no frequency block (vanity+seal only)",
			extra:  buildNonEpochExtra(0, false, nil),
			wantOK: false,
		},
		{
			name:   "extra too short",
			extra:  make([]byte, extraVanity+extraSeal-1),
			wantOK: false,
		},
		{
			name: "wrong prefix bytes",
			extra: func() []byte {
				e := buildNonEpochExtra(1234, true, []byte{0xc0})
				copy(e[extraVanity:extraVanity+len(validatorFrequencyDataPrefix)], []byte("XXX"))
				return e
			}(),
			wantOK: false,
		},
		{
			name: "truncated timestamp (prefix present, ts spills into seal)",
			// vanity + prefix + only 4 ts bytes + seal: tsEnd would read into seal.
			extra: func() []byte {
				e := make([]byte, extraVanity)
				e = append(e, validatorFrequencyDataPrefix...)
				e = append(e, []byte{1, 2, 3, 4}...)
				return append(e, make([]byte, extraSeal)...)
			}(),
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &types.Header{Number: nonEpochNum, Extra: tc.extra}
			got, ok := extractSnake8ParentTimestamp(h, cfg)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.wantTime {
				t.Fatalf("ts = %d, want %d", got, tc.wantTime)
			}
		})
	}
}

// snake8PreLubanConfig mirrors snake8TestConfig but leaves Luban inactive, which
// is the layout Chiliz mainnet/Spicy actually run (no lubanBlock in the embedded
// genesis). On this path epoch blocks carry 20-byte validator entries with no
// count byte, and no vote attestation is ever appended.
func snake8PreLubanConfig() *params.ChainConfig {
	cfg := snake8TestConfig()
	cfg.LubanBlock = nil
	return cfg
}

// TestExtractSnake8ParentTimestampPreLubanEpochBlock exercises the
// validator-skipping path on a pre-Luban epoch block — the one that is actually
// live on Chiliz. snake8FreqDataOffset must walk the 20-byte validator entries
// (located via getValidatorBytesFromHeader, which stops at the VFQ prefix) and
// land exactly on the frequency block.
func TestExtractSnake8ParentTimestampPreLubanEpochBlock(t *testing.T) {
	cfg := snake8PreLubanConfig()

	// vanity | 2 * 20-byte validators | VFQ | ts | freq | seal
	extra := make([]byte, extraVanity)
	extra = append(extra, make([]byte, 2*validatorBytesLengthBeforeLuban)...) // two validator entries
	extra = append(extra, validatorFrequencyDataPrefix...)
	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, 7777)
	extra = append(extra, ts...)
	extra = append(extra, byte(0xc0)) // empty freq RLP list
	extra = append(extra, make([]byte, extraSeal)...)

	h := &types.Header{Number: big.NewInt(snake8TestEpoch), Extra: extra}
	got, ok := extractSnake8ParentTimestamp(h, cfg)
	if !ok {
		t.Fatalf("expected ok for well-formed pre-Luban epoch block")
	}
	if got != 7777 {
		t.Fatalf("ts = %d, want 7777", got)
	}
}

// TestExtractSnake8ParentTimestampTrailingData verifies the extractor reads only
// the prefix + 8-byte timestamp and is unaffected by however many bytes follow
// (frequency RLP, and on a Luban-enabled network a trailing vote attestation).
func TestExtractSnake8ParentTimestampTrailingData(t *testing.T) {
	cfg := snake8TestConfig()
	nonEpochNum := big.NewInt(snake8TestEpoch + 1)

	// A long freq payload plus extra trailing bytes standing in for an appended
	// vote attestation; the embedded timestamp must still be read correctly.
	freqAndTrailer := make([]byte, 64)
	for i := range freqAndTrailer {
		freqAndTrailer[i] = byte(i)
	}
	extra := buildNonEpochExtra(98765, true, freqAndTrailer)

	h := &types.Header{Number: nonEpochNum, Extra: extra}
	got, ok := extractSnake8ParentTimestamp(h, cfg)
	if !ok {
		t.Fatalf("expected ok with trailing data present")
	}
	if got != 98765 {
		t.Fatalf("ts = %d, want 98765", got)
	}
}

// TestVerifySnake8ExtraPreForkAttestationLikeBytes guards against false rejection
// of an honest pre-fork block whose post-vanity bytes happen to begin with an RLP
// list header (0xc0+), as a vote attestation would. Such bytes must not be
// mistaken for a "VFQ" frequency block, so the pre-fork block is accepted.
func TestVerifySnake8ExtraPreForkAttestationLikeBytes(t *testing.T) {
	p := &Parlia{chainConfig: snake8TestConfig()}
	nonEpochNum := big.NewInt(snake8TestEpoch + 1)

	extra := make([]byte, extraVanity)
	extra = append(extra, []byte{0xc4, 0x83, 'a', 'b', 'c'}...) // RLP-list-looking blob, not "VFQ"
	extra = append(extra, make([]byte, extraSeal)...)

	header := &types.Header{Number: nonEpochNum, Extra: extra}
	parent := &types.Header{Number: big.NewInt(snake8TestEpoch), Time: snake8TestActivTime - 1}
	if err := p.verifySnake8Extra(header, parent); err != nil {
		t.Fatalf("honest pre-fork block wrongly rejected: %v", err)
	}
}

// TestExtractSnake8ParentTimestampEpochBlock exercises the validator-skipping
// path on a post-Luban epoch block.
func TestExtractSnake8ParentTimestampEpochBlock(t *testing.T) {
	cfg := snake8TestConfig()

	// vanity | count(=1) | 1*68 validator bytes | VFQ | ts | freq | seal
	extra := make([]byte, extraVanity)
	extra = append(extra, byte(1))                               // validator count
	extra = append(extra, make([]byte, validatorBytesLength)...) // one validator entry
	extra = append(extra, validatorFrequencyDataPrefix...)
	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, 4242)
	extra = append(extra, ts...)
	extra = append(extra, byte(0xc0)) // empty freq RLP list
	extra = append(extra, make([]byte, extraSeal)...)

	h := &types.Header{Number: big.NewInt(snake8TestEpoch), Extra: extra}
	got, ok := extractSnake8ParentTimestamp(h, cfg)
	if !ok {
		t.Fatalf("expected ok for well-formed epoch block")
	}
	if got != 4242 {
		t.Fatalf("ts = %d, want 4242", got)
	}
}

func TestVerifySnake8Extra(t *testing.T) {
	p := &Parlia{chainConfig: snake8TestConfig()}
	nonEpochNum := big.NewInt(snake8TestEpoch + 1)

	tests := []struct {
		name       string
		parentTime uint64 // real parent.Time
		extra      []byte
		wantErr    error
	}{
		{
			name:       "pre-fork without frequency block is accepted",
			parentTime: snake8TestActivTime - 1,
			extra:      buildNonEpochExtra(0, false, nil),
			wantErr:    nil,
		},
		{
			name:       "pre-fork carrying a frequency block is rejected (delayed-activation forgery)",
			parentTime: snake8TestActivTime - 1,
			extra:      buildNonEpochExtra(snake8TestActivTime, true, []byte{0xc0}),
			wantErr:    errInvalidSnake8Extra,
		},
		{
			name:       "post-fork with matching embedded timestamp is accepted",
			parentTime: snake8TestActivTime + 5,
			extra:      buildNonEpochExtra(snake8TestActivTime+5, true, []byte{0xc0}),
			wantErr:    nil,
		},
		{
			name:       "post-fork with forged embedded timestamp is rejected",
			parentTime: snake8TestActivTime + 5,
			extra:      buildNonEpochExtra(snake8TestActivTime+999, true, []byte{0xc0}),
			wantErr:    errMismatchedSnake8ParentTime,
		},
		{
			name:       "post-fork without a frequency block is rejected",
			parentTime: snake8TestActivTime + 5,
			extra:      buildNonEpochExtra(0, false, nil),
			wantErr:    errInvalidSnake8Extra,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header := &types.Header{Number: nonEpochNum, Extra: tc.extra}
			parent := &types.Header{Number: big.NewInt(snake8TestEpoch), Time: tc.parentTime}
			err := p.verifySnake8Extra(header, parent)
			if err != tc.wantErr {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestSelectValidatorFromFrequencyRLPFallback verifies that malformed or empty
// frequency RLP never yields the zero address (which would break in-turn
// selection for the whole epoch) but instead falls back to round-robin.
func TestSelectValidatorFromFrequencyRLPFallback(t *testing.T) {
	validators := []common.Address{
		common.HexToAddress("0x1111111111111111111111111111111111111111"),
		common.HexToAddress("0x2222222222222222222222222222222222222222"),
		common.HexToAddress("0x3333333333333333333333333333333333333333"),
	}
	snap := newSnapshot(nil, nil, 7, common.Hash{}, validators, nil, nil, true)
	wantRoundRobin := snap.selectValidatorRoundRobin()

	tests := []struct {
		name    string
		freqRLP []byte
	}{
		{name: "undecodable RLP", freqRLP: []byte{0xff, 0xff, 0xff}},
		{name: "empty candidate list", freqRLP: []byte{0xc0}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := snap.selectValidatorFromFrequencyRLP(tc.freqRLP)
			if got == (common.Address{}) {
				t.Fatalf("got zero address, expected round-robin fallback")
			}
			if got != wantRoundRobin {
				t.Fatalf("got %s, want round-robin %s", got.Hex(), wantRoundRobin.Hex())
			}
		})
	}
}
