package parlia

// COR-210: differential oracle against upstream BSC.
//
// upstreamGetValidatorBytesFromHeader below is a verbatim copy of
// getValidatorBytesFromHeader from bnb-chain/bsc tag v1.7.8,
// consensus/parlia/parlia.go (the last merged upstream tag, see
// .github/bsc-sync/last-synced-tag). It is the oracle for FuzzUpstreamOracle_
// GetValidatorBytesFromHeader, which pins WHERE Chiliz is allowed to differ:
//
//   - Chiliz's only change to this function is the Snake8 frequency-data
//     section (a "VFQ"-prefixed trailer, see CLAUDE.md §2 "Snake8"), and that
//     change lives entirely in the pre-Luban branch. The two functions must be
//     byte-identical for every fork layout (pre-Luban, Luban, Luban+Bohr),
//     every block number (epoch and non-epoch) and every Extra length, except
//     on a pre-Luban header carrying a VFQ sequence in the range the Chiliz
//     scan actually covers (extra[:len(extra)-extraSeal]). VFQ bytes that fall
//     inside the seal, and VFQ bytes on a Luban header, are in scope: neither
//     implementation looks at them.
//
// When an upstream sync legitimately changes getValidatorBytesFromHeader, the
// oracle copy must be updated in the same PR and the tag in this comment
// bumped. The package constants (extraVanity, extraSeal, validatorBytesLength,
// validatorBytesLengthBeforeLuban, validatorNumberSize, turnLengthSize) are the
// shared Chiliz ones; they are identical to upstream v1.7.8 (checked when this
// copy was taken), so the oracle pins the function body, not the constants.

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// upstreamGetValidatorBytesFromHeader — bnb-chain/bsc v1.7.8, consensus/parlia/parlia.go.
// Do not edit; see the file header.
func upstreamGetValidatorBytesFromHeader(header *types.Header, chainConfig *params.ChainConfig, epochLength uint64) []byte {
	if len(header.Extra) <= extraVanity+extraSeal {
		return nil
	}

	if !chainConfig.IsLuban(header.Number) {
		if header.Number.Uint64()%epochLength == 0 && (len(header.Extra)-extraSeal-extraVanity)%validatorBytesLengthBeforeLuban != 0 {
			return nil
		}
		return header.Extra[extraVanity : len(header.Extra)-extraSeal]
	}

	if header.Number.Uint64()%epochLength != 0 {
		return nil
	}
	num := int(header.Extra[extraVanity])
	start := extraVanity + validatorNumberSize
	end := start + num*validatorBytesLength
	extraMinLen := end + extraSeal
	if chainConfig.IsBohr(header.Number, header.Time) {
		extraMinLen += turnLengthSize
	}
	if num == 0 || len(header.Extra) < extraMinLen {
		return nil
	}
	return header.Extra[start:end]
}

const oracleEpochLength = uint64(200)

// oracleForkConfig returns the chain config for a fork selector:
// 0 = pre-Luban, 1 = Luban, 2 = Luban + Bohr (Bohr needs London for IsBohr).
func oracleForkConfig(sel uint8) *params.ChainConfig {
	cfg := &params.ChainConfig{
		ChainID: big.NewInt(1),
		Parlia:  &params.ParliaConfig{Epoch: oracleEpochLength, Period: 3},
	}
	switch sel % 3 {
	case 1:
		cfg.LubanBlock = big.NewInt(0)
	case 2:
		cfg.LubanBlock = big.NewInt(0)
		cfg.LondonBlock = big.NewInt(0)
		bohr := uint64(0)
		cfg.BohrTime = &bohr
	}
	return cfg
}

// sameValidatorBytes treats nil and empty as equal; both callers only ever
// range over or len() the result.
func sameValidatorBytes(a, b []byte) bool {
	return len(a) == 0 && len(b) == 0 || bytes.Equal(a, b)
}

// FuzzUpstreamOracle_GetValidatorBytesFromHeader pins getValidatorBytesFromHeader
// to the upstream oracle on every header whose Extra carries no VFQ sequence.
//
// Inputs: extra (raw header.Extra), number, time, and sel whose low bits pick
// the fork layout and whose high bit snaps number onto an epoch boundary (the
// interesting branch of both functions).
func FuzzUpstreamOracle_GetValidatorBytesFromHeader(f *testing.F) {
	vanity := make([]byte, extraVanity)
	seal := make([]byte, extraSeal)
	oneValidatorPreLuban := append(append(append([]byte{}, vanity...), bytes.Repeat([]byte{0xaa}, validatorBytesLengthBeforeLuban)...), seal...)
	twoValidatorsLuban := append(append(append([]byte{}, vanity...), append([]byte{2}, bytes.Repeat([]byte{0xbb}, 2*validatorBytesLength)...)...), seal...)
	twoValidatorsBohr := append(append(append([]byte{}, vanity...), append(append([]byte{2}, bytes.Repeat([]byte{0xcc}, 2*validatorBytesLength)...), 4)...), seal...)
	junk := append(append(append([]byte{}, vanity...), []byte{1, 2, 3, 4, 5, 6, 7}...), seal...)
	// VFQ inside the seal: outside the range either implementation scans, so
	// these must still agree byte for byte on every fork layout.
	vfqInSeal := append(append(append([]byte{}, vanity...), bytes.Repeat([]byte{0xaa}, validatorBytesLengthBeforeLuban)...), append(append([]byte{}, validatorFrequencyDataPrefix...), seal[3:]...)...)
	// VFQ in the validator region: pre-Luban this is the Chiliz-only branch
	// (skipped by the domain guard), Luban/Bohr must still match upstream.
	vfqInBody := append(append(append([]byte{}, vanity...), append(append([]byte{2}, validatorFrequencyDataPrefix...), bytes.Repeat([]byte{0xbb}, 2*validatorBytesLength-3)...)...), seal...)

	for _, sel := range []uint8{0, 1, 2, 0x80, 0x81, 0x82} {
		f.Add(vfqInSeal, oracleEpochLength, uint64(10), sel)
		f.Add(vfqInBody, oracleEpochLength, uint64(10), sel)
		f.Add([]byte{}, uint64(0), uint64(0), sel)
		f.Add(append([]byte{}, vanity...), oracleEpochLength, uint64(0), sel)
		f.Add(oneValidatorPreLuban, oracleEpochLength, uint64(10), sel)
		f.Add(oneValidatorPreLuban, oracleEpochLength+1, uint64(10), sel)
		f.Add(twoValidatorsLuban, oracleEpochLength, uint64(10), sel)
		f.Add(twoValidatorsBohr, 3*oracleEpochLength, uint64(10), sel)
		f.Add(junk, oracleEpochLength, uint64(0), sel)
		f.Add(junk, uint64(7), uint64(0), sel)
	}

	f.Fuzz(func(t *testing.T, extra []byte, number uint64, time uint64, sel uint8) {
		if sel&0x80 != 0 {
			number -= number % oracleEpochLength
		}
		cfg := oracleForkConfig(sel)
		header := &types.Header{
			Number: new(big.Int).SetUint64(number),
			Time:   time,
			Extra:  extra,
		}

		// Domain: exactly the inputs on which Chiliz reads the Snake8 VFQ
		// trailer, and no more. That is the pre-Luban branch only (the
		// Luban/Bohr branch is byte-identical to upstream, VFQ or not), only
		// when the branch is actually reached (len(extra) > vanity+seal), and
		// only for a VFQ inside extra[:len(extra)-extraSeal] — the exact range
		// the Chiliz scan loop covers (i <= end-3, reading extra[i:i+3]).
		// Snake8 headers themselves are Chiliz-only by design and are covered
		// by snake8_test.go. Keep this guard as narrow as the implementation:
		// widening it hides real divergences.
		if !cfg.IsLuban(header.Number) && len(extra) > extraVanity+extraSeal &&
			bytes.Contains(extra[:len(extra)-extraSeal], validatorFrequencyDataPrefix) {
			return
		}

		got := getValidatorBytesFromHeader(header, cfg, oracleEpochLength)
		want := upstreamGetValidatorBytesFromHeader(header, cfg, oracleEpochLength)
		if !sameValidatorBytes(got, want) {
			t.Fatalf("getValidatorBytesFromHeader diverged from upstream v1.7.8 outside the Snake8 VFQ domain\n fork sel=%d number=%d time=%d len(extra)=%d\n chiliz  =%x\n upstream=%x",
				sel%3, number, time, len(extra), got, want)
		}
	})
}

// TestUpstreamOracleSnake8DivergenceStillExists is the positive control for
// FuzzUpstreamOracle_GetValidatorBytesFromHeader.
//
// That target asserts Chiliz matches upstream *outside* the Snake8 VFQ domain,
// and returns early inside it. Correct, but one-sided: it stays green when the
// divergence it is guarding disappears. Verified by reverting the pre-Luban
// branch to the exact v1.7.8 body — the fuzz target passed, and only
// snake8_test.go caught it. A syncer reading this file's header ("pins WHERE
// Chiliz is allowed to differ"), seeing the target green after resolving this
// function as "theirs", would conclude the divergence survived.
//
// So: assert the divergence is still there. These two headers MUST parse
// differently on the two implementations. If this test fails, the Snake8 VFQ
// handling has been lost — restore it, do not relax the test.
func TestUpstreamOracleSnake8DivergenceStillExists(t *testing.T) {
	cfg := oracleForkConfig(0) // pre-Luban
	if cfg.IsLuban(new(big.Int).SetUint64(oracleEpochLength)) {
		t.Fatalf("oracleForkConfig(0) must be the pre-Luban shape")
	}
	// A REAL one-entry frequency list, not 0xc0. Since COR-213 the Chiliz parser
	// locates the block structurally and accepts a candidate only when its tail
	// decodes as a non-empty frequency list (decodesAsFrequencyList requires
	// len(entries) > 0), so an empty list makes it find no block at all — and
	// then it falls through to the same %20 check as upstream and the two AGREE,
	// silently turning this control into a no-op. That is exactly what happened
	// when COR-210 and COR-213 first landed together on develop.
	freqList, err := rlp.EncodeToBytes([]frequencyBlockEntry{{
		Address:   common.HexToAddress("0x0100000000000000000000000000000000000001"),
		Frequency: big.NewInt(1),
	}})
	if err != nil {
		t.Fatalf("encoding the frequency list: %v", err)
	}
	frequencyBlock := append(append([]byte{}, validatorFrequencyDataPrefix...), make([]byte, 8)...)
	frequencyBlock = append(frequencyBlock, freqList...)

	for _, tc := range []struct {
		name   string
		number uint64
		body   []byte
	}{
		{
			// Epoch header: |2 validators|VFQ|ts|freq|. Chiliz stops at the
			// frequency block and returns the two validators; upstream takes the
			// whole body, which is not a whole number of validator entries, and
			// returns nil. No byte count here on purpose — it moved once already
			// when the tail stopped being 0xc0, and the assertion below keeps the
			// claim honest without one.
			name:   "epoch header with a frequency block",
			number: oracleEpochLength,
			body:   append(bytes.Repeat([]byte{0xab}, 2*validatorBytesLengthBeforeLuban), frequencyBlock...),
		},
		{
			// Non-epoch header carrying only the frequency block: Chiliz returns
			// nil, upstream hands the block back as validator bytes.
			name:   "non-epoch header carrying only a frequency block",
			number: oracleEpochLength + 1,
			body:   frequencyBlock,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The epoch case's reasoning above rests on the body not being a whole
			// number of validator entries — that is what makes upstream return nil
			// while Chiliz returns just the validators. Pin it, so an edit to the
			// frequency tail cannot quietly turn the two sides into "both return
			// the whole body", which would still differ but for another reason.
			if tc.number%oracleEpochLength == 0 && len(tc.body)%validatorBytesLengthBeforeLuban == 0 {
				t.Fatalf("fixture body is %d bytes, a whole number of %d-byte validator entries — "+
					"upstream would return the body rather than nil and this case no longer tests what its comment says",
					len(tc.body), validatorBytesLengthBeforeLuban)
			}
			extra := append(append(make([]byte, extraVanity), tc.body...), make([]byte, extraSeal)...)
			header := &types.Header{Number: new(big.Int).SetUint64(tc.number), Time: 10, Extra: extra}

			got := getValidatorBytesFromHeader(header, cfg, oracleEpochLength)
			want := upstreamGetValidatorBytesFromHeader(header, cfg, oracleEpochLength)
			if sameValidatorBytes(got, want) {
				t.Fatalf("Chiliz now agrees with upstream v1.7.8 on a Snake8 frequency-block header: both = %x.\n"+
					"The Snake8 VFQ handling in getValidatorBytesFromHeader has been lost — most likely an upstream\n"+
					"sync resolved this function as \"theirs\". Restore it; do not relax this test.", got)
			}
		})
	}
}
