package parlia

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"
	"runtime/debug"
	"sort"
	"testing"
	"unsafe"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// COR-208: native fuzz coverage of the header.Extra layout parsers.
//
// Every function exercised here parses bytes that arrive from remote peers
// (header.Extra is attacker-controlled until the seal is verified), so the
// parsers must never panic, never return memory outside the vanity..seal body,
// and must agree with each other about where the Snake8 frequency block starts.
//
//	getValidatorBytesFromHeader   parlia.go
//	getVoteAttestationFromHeader  parlia.go
//	snake8FreqDataOffset          snapshot.go
//	extractSnake8ParentTimestamp  snapshot.go
//	parseValidatorFrequencies     snapshot.go
//	parseValidators               snapshot.go
//	parseTurnLength               snapshot.go

// Fork selectors for FuzzHeaderExtraLayout. The low bits pick a chain config;
// the high bit forces the block number onto an epoch boundary so the
// validator-carrying layout is hit far more often than 1/epoch of the time.
const (
	extraFuzzSelPreSnake8 uint8 = iota // pre-Luban, Snake8 never scheduled
	extraFuzzSelMainnet                // pre-Luban, Snake8 from genesis (live mainnet/Spicy shape)
	extraFuzzSelDevnet                 // Luban+Bohr+Snake8 from genesis (devnet-style)
	extraFuzzSelBoundary               // pre-Luban, Snake8 at a mid-range timestamp (time toggles it)
	extraFuzzSelCount

	extraFuzzEpochBit uint8 = 0x80

	extraFuzzMainnetEpoch = 28800 // Chiliz mainnet epoch length
	extraFuzzDevnetEpoch  = 200   // matches snake8TestConfig
)

// extraFuzzConfig maps a fuzzed selector onto a ChainConfig. Only fields the
// parsers consult are set: Parlia.Epoch, LubanBlock, LondonBlock+BohrTime
// (IsBohr requires London), Snake8Time.
func extraFuzzConfig(sel uint8) (*params.ChainConfig, string) {
	switch sel % extraFuzzSelCount {
	case extraFuzzSelPreSnake8:
		return &params.ChainConfig{
			ChainID: big.NewInt(88888),
			Parlia:  &params.ParliaConfig{Epoch: extraFuzzMainnetEpoch, Period: 3},
		}, "pre-luban/pre-snake8"
	case extraFuzzSelMainnet:
		return &params.ChainConfig{
			ChainID:    big.NewInt(88888),
			Parlia:     &params.ParliaConfig{Epoch: extraFuzzMainnetEpoch, Period: 3},
			Snake8Time: new(uint64),
		}, "pre-luban/snake8"
	case extraFuzzSelDevnet:
		return &params.ChainConfig{
			ChainID:     big.NewInt(88883),
			Parlia:      &params.ParliaConfig{Epoch: extraFuzzDevnetEpoch, Period: 3},
			LondonBlock: common.Big0,
			LubanBlock:  common.Big0,
			BohrTime:    new(uint64),
			Snake8Time:  new(uint64),
		}, "luban+bohr/snake8"
	default:
		st := uint64(snake8TestActivTime)
		return &params.ChainConfig{
			ChainID:    big.NewInt(88882),
			Parlia:     &params.ParliaConfig{Epoch: extraFuzzMainnetEpoch, Period: 3},
			Snake8Time: &st,
		}, "pre-luban/snake8-boundary"
	}
}

// Real Chiliz mainnet header.Extra bytes, fetched from the public RPC
// (https://rpc.chiliz.com, eth_getBlockByNumber) with no credentials. Both are
// post-Snake8, pre-Luban (mainnet schedules no Luban), 13 validators.
const (
	// Block 35,884,800 = 1246 * 28800 (epoch block), time 1784407964.
	// vanity[0:32] | 13*20 validators [32:292] | VFQ [292:295] | LE ts [295:303] | freq RLP [303:606] | seal [606:671]
	mainnetEpochExtraHex    = "d983020801846765746889676f312e32342e3133856c696e75780000abcd6c3931db81188a5cc391857624f668dda57ba7f2b07431dd5a7429ae591d2d73935c001dd148fabdd2cf579391c9865545000d8922acf71a660521cc640484af28c88be1916ceb199394dd6a1493a1287ada8d9b6ab3fe8ebf16d9242e48fefb89360fa62820a4c22224c33fcbf204841399df1f268d13de0339a4c864e043ba00843fd502d85896c96ba5738c4ca90da6fba59d0ba2fb1348fd2f03b880742fa00ebf968b8a455da0dfc0852899c334b6f5f6e20b5fc10ae5cd2c63e4065f81e241c93237e06e12d41becb60190a0bf9f4d8aa8cc2f0293aa91192d3187f57c7a5bcb023ab18683a46fa25a00fb19d651bef84aed72066e635fd9b0f2fbddbdb77a8761d02856465199e75b6a00000000f9012cd69431db81188a5cc391857624f668dda57ba7f2b07472d69431dd5a7429ae591d2d73935c001dd148fabdd2cf31d694579391c9865545000d8922acf71a660521cc640437d69484af28c88be1916ceb199394dd6a1493a1287ada50d6948d9b6ab3fe8ebf16d9242e48fefb89360fa6282074d694a4c22224c33fcbf204841399df1f268d13de033938d694a4c864e043ba00843fd502d85896c96ba5738c4c46d694a90da6fba59d0ba2fb1348fd2f03b880742fa00e52d794bf968b8a455da0dfc0852899c334b6f5f6e20b5f8180d694c10ae5cd2c63e4065f81e241c93237e06e12d41b26d694ecb60190a0bf9f4d8aa8cc2f0293aa91192d318752d694f57c7a5bcb023ab18683a46fa25a00fb19d651be35d694f84aed72066e635fd9b0f2fbddbdb77a8761d028476be38fe138828528cb56bbb4ce509fe2152a058f4fcfd0e6de69347ceb11814c1d040b0423700046e48424e969f62da6fc29fc54147f59bb9b7a0e9fa9aa6bb500"
	mainnetEpochExtraNumber = uint64(35884800)
	mainnetEpochExtraTime   = uint64(1784407964)

	// Block 35,883,008 (non-epoch block), time 1784402505.
	// vanity[0:32] | VFQ [32:35] | LE ts [35:43] | freq RLP [43:346] | seal [346:411]
	mainnetNonEpochExtraHex    = "d983020801846765746889676f312e32342e3133856c696e75780000abcd6c3956465146d25b6a00000000f9012cd69431db81188a5cc391857624f668dda57ba7f2b07472d69431dd5a7429ae591d2d73935c001dd148fabdd2cf31d694579391c9865545000d8922acf71a660521cc640437d69484af28c88be1916ceb199394dd6a1493a1287ada50d6948d9b6ab3fe8ebf16d9242e48fefb89360fa6282074d694a4c22224c33fcbf204841399df1f268d13de033938d694a4c864e043ba00843fd502d85896c96ba5738c4c46d694a90da6fba59d0ba2fb1348fd2f03b880742fa00e52d794bf968b8a455da0dfc0852899c334b6f5f6e20b5f8180d694c10ae5cd2c63e4065f81e241c93237e06e12d41b26d694ecb60190a0bf9f4d8aa8cc2f0293aa91192d318752d694f57c7a5bcb023ab18683a46fa25a00fb19d651be35d694f84aed72066e635fd9b0f2fbddbdb77a8761d028471def4a79ddfcea8a131f7d4d988f89a960be1b30f0820289e1fe052eed2cabe259525f59c4842e83b2e39f0aa75b4f1c8f2c25504e64248ab4bb60e028efab0000"
	mainnetNonEpochExtraNumber = uint64(35883008)
	mainnetNonEpochExtraTime   = uint64(1784402505)

	mainnetValidatorCount = 13
)

// mainnetEpochExtraParts splits the real epoch-block Extra into the producer's
// fields so the round-trip generator can be seeded with (and pinned against)
// genuine mainnet bytes.
func mainnetEpochExtraParts() (vanity, validators []byte, parentTS uint64, freq, seal []byte) {
	extra := common.FromHex(mainnetEpochExtraHex)
	valEnd := extraVanity + mainnetValidatorCount*validatorBytesLengthBeforeLuban
	tsStart := valEnd + len(validatorFrequencyDataPrefix)
	freqStart := tsStart + 8
	sealStart := len(extra) - extraSeal
	return extra[:extraVanity],
		extra[extraVanity:valEnd],
		binary.LittleEndian.Uint64(extra[tsStart:freqStart]),
		extra[freqStart:sealStart],
		extra[sealStart:]
}

// buildPreLubanSnake8Extra assembles header.Extra exactly as SetExtraData does
// on a pre-Luban Snake8 network: 32-byte vanity (vanity bytes + nextForkHash,
// opaque here) | sorted 20-byte validators (epoch blocks only) | "VFQ" | LE
// parent timestamp | frequency RLP | 65-byte seal. Vanity and seal are padded
// or truncated to their fixed widths.
func buildPreLubanSnake8Extra(vanity []byte, validators []common.Address, parentTS uint64, freq, seal []byte) []byte {
	extra := make([]byte, 0, extraVanity+len(validators)*common.AddressLength+len(validatorFrequencyDataPrefix)+8+len(freq)+extraSeal)
	extra = append(extra, fitBytes(vanity, extraVanity)...)
	for _, v := range validators {
		extra = append(extra, v.Bytes()...)
	}
	extra = append(extra, validatorFrequencyDataPrefix...)
	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, parentTS)
	extra = append(extra, ts...)
	extra = append(extra, freq...)
	extra = append(extra, fitBytes(seal, extraSeal)...)
	return extra
}

// fitBytes returns a copy of b zero-padded or truncated to exactly n bytes.
func fitBytes(b []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, b)
	return out
}

// extraBodyOffset returns the offset of sub inside extra, asserting that sub is
// a genuine sub-slice (same backing array) of extra that lies strictly within the
// vanity..seal body. Checked via pointers, never via content, so an accidental
// copy or a slice into a different buffer is caught. Empty slices are exempt
// (nothing can be read through them).
func assertWithinExtraBody(t *testing.T, name string, sub, extra []byte) int {
	t.Helper()
	if len(sub) == 0 {
		return -1
	}
	base := uintptr(unsafe.Pointer(unsafe.SliceData(extra)))
	p := uintptr(unsafe.Pointer(unsafe.SliceData(sub)))
	if p < base || p-base > uintptr(len(extra)) {
		t.Fatalf("%s: returned slice is not backed by header.Extra (base=%#x ptr=%#x len(extra)=%d)", name, base, p, len(extra))
	}
	off := int(p - base)
	if off < extraVanity {
		t.Fatalf("%s: returned slice starts inside the vanity (offset %d < %d)", name, off, extraVanity)
	}
	if off+len(sub) > len(extra)-extraSeal {
		t.Fatalf("%s: returned slice reaches into the seal (offset %d + len %d > %d)", name, off, len(sub), len(extra)-extraSeal)
	}
	return off
}

// noPanic runs fn and converts any panic into a t.Fatalf carrying the function
// name and stack, so a fuzz crasher names the parser that blew up.
func noPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v\n%s", name, r, debug.Stack())
		}
	}()
	fn()
}

// FuzzHeaderExtraLayout throws arbitrary Extra bytes at every layout parser
// under each fork configuration and checks the structural invariants:
//
//  1. no parser panics;
//  2. every returned byte slice is a sub-slice of header.Extra lying strictly
//     between the vanity and the seal (pointer check, not content);
//  3. extractSnake8ParentTimestamp and parseValidatorFrequencies, which derive
//     their position from snake8FreqDataOffset independently, agree: when
//     Snake8 is active for the header's time, ts-ok <=> freq-err == nil;
//  4. on a pre-Luban epoch block the validator bytes are a whole number of
//     20-byte entries and parseValidators reproduces them one for one.
func FuzzHeaderExtraLayout(f *testing.F) {
	const (
		mainnetEpochBlock    = uint64(3 * extraFuzzMainnetEpoch)
		mainnetNonEpochBlock = mainnetEpochBlock + 1
		devnetEpochBlock     = uint64(2 * extraFuzzDevnetEpoch)
		postSnake8Time       = uint64(snake8TestActivTime + 500)
		preSnake8Time        = uint64(snake8TestActivTime - 500)
	)
	vanityAndSeal := func() []byte { return make([]byte, extraVanity+extraSeal) }

	// Degenerate shapes.
	f.Add([]byte(nil), uint64(0), uint64(0), extraFuzzSelMainnet)
	f.Add(vanityAndSeal(), mainnetNonEpochBlock, postSnake8Time, extraFuzzSelMainnet)
	f.Add(vanityAndSeal(), mainnetEpochBlock, postSnake8Time, extraFuzzSelMainnet)
	f.Add(vanityAndSeal(), devnetEpochBlock, postSnake8Time, extraFuzzSelDevnet)

	// Valid pre-Snake8 epoch block: vanity | 2 validators | seal.
	preSnake8Epoch := make([]byte, extraVanity)
	preSnake8Epoch = append(preSnake8Epoch, bytes.Repeat([]byte{0xaa}, validatorBytesLengthBeforeLuban)...)
	preSnake8Epoch = append(preSnake8Epoch, bytes.Repeat([]byte{0xbb}, validatorBytesLengthBeforeLuban)...)
	preSnake8Epoch = append(preSnake8Epoch, make([]byte, extraSeal)...)
	f.Add(preSnake8Epoch, mainnetEpochBlock, preSnake8Time, extraFuzzSelPreSnake8)
	f.Add(preSnake8Epoch, mainnetEpochBlock, preSnake8Time, extraFuzzSelBoundary)

	// Valid Snake8 non-epoch block.
	f.Add(buildNonEpochExtra(1234, true, []byte{0xc0}), mainnetNonEpochBlock, postSnake8Time, extraFuzzSelMainnet)
	f.Add(buildNonEpochExtra(1234, true, []byte{0xc0}), mainnetNonEpochBlock, postSnake8Time, extraFuzzSelBoundary)

	// Valid Snake8 epoch blocks, pre-Luban (live shape) and Luban+Bohr (devnet).
	preLubanEpoch := buildPreLubanSnake8Extra(nil, []common.Address{
		common.HexToAddress("0x0100000000000000000000000000000000000001"),
		common.HexToAddress("0x0200000000000000000000000000000000000002"),
	}, 7777, []byte{0xc0}, nil)
	f.Add(preLubanEpoch, mainnetEpochBlock, postSnake8Time, extraFuzzSelMainnet)
	f.Add(preLubanEpoch, mainnetEpochBlock, postSnake8Time, extraFuzzSelBoundary)

	lubanEpoch := make([]byte, extraVanity)
	lubanEpoch = append(lubanEpoch, byte(1))                               // validator count
	lubanEpoch = append(lubanEpoch, make([]byte, validatorBytesLength)...) // one 68-byte entry
	lubanEpoch = append(lubanEpoch, byte(1))                               // turn length (Bohr)
	lubanEpoch = append(lubanEpoch, validatorFrequencyDataPrefix...)
	lubanEpoch = append(lubanEpoch, make([]byte, 8)...)
	lubanEpoch = append(lubanEpoch, byte(0xc0))
	lubanEpoch = append(lubanEpoch, make([]byte, extraSeal)...)
	f.Add(lubanEpoch, devnetEpochBlock, postSnake8Time, extraFuzzSelDevnet)

	// Malformed shapes mirrored from TestExtractSnake8ParentTimestamp*.
	f.Add(make([]byte, extraVanity+extraSeal-1), mainnetNonEpochBlock, postSnake8Time, extraFuzzSelMainnet) // too short
	wrongPrefix := buildNonEpochExtra(1234, true, []byte{0xc0})
	copy(wrongPrefix[extraVanity:], "XXX")
	f.Add(wrongPrefix, mainnetNonEpochBlock, postSnake8Time, extraFuzzSelMainnet)
	truncatedTS := append(append(make([]byte, extraVanity), validatorFrequencyDataPrefix...), 1, 2, 3, 4)
	truncatedTS = append(truncatedTS, make([]byte, extraSeal)...)
	f.Add(truncatedTS, mainnetNonEpochBlock, postSnake8Time, extraFuzzSelMainnet)
	trailer := make([]byte, 64)
	for i := range trailer {
		trailer[i] = byte(i)
	}
	f.Add(buildNonEpochExtra(98765, true, trailer), mainnetNonEpochBlock, postSnake8Time, extraFuzzSelMainnet)
	attestationLike := append(make([]byte, extraVanity), 0xc4, 0x83, 'a', 'b', 'c')
	attestationLike = append(attestationLike, make([]byte, extraSeal)...)
	f.Add(attestationLike, mainnetNonEpochBlock, preSnake8Time, extraFuzzSelBoundary)

	// VFQ at the very last offset getValidatorBytesFromHeader's scan may inspect
	// (end-3). Pins the inclusive `i <= end-3` bound: with an off-by-one there the
	// scan misses it, the region stays a clean multiple of 20 and the VFQ prefix
	// is handed back as validator bytes. Random mutation does not reach this shape
	// (three fixed bytes at one offset in a length-constrained buffer), so it has
	// to be seeded.
	trailingVFQ := bytes.Repeat([]byte{0x11}, extraVanity+validatorBytesLengthBeforeLuban+extraSeal)
	copy(trailingVFQ[extraVanity+validatorBytesLengthBeforeLuban-len(validatorFrequencyDataPrefix):], validatorFrequencyDataPrefix)
	f.Add(trailingVFQ, mainnetEpochBlock, postSnake8Time, extraFuzzSelMainnet)

	// Real mainnet bytes.
	f.Add(common.FromHex(mainnetEpochExtraHex), mainnetEpochExtraNumber, mainnetEpochExtraTime, extraFuzzSelMainnet)
	f.Add(common.FromHex(mainnetNonEpochExtraHex), mainnetNonEpochExtraNumber, mainnetNonEpochExtraTime, extraFuzzSelMainnet)

	f.Fuzz(func(t *testing.T, extra []byte, number, time uint64, forkSel uint8) {
		cfg, cfgName := extraFuzzConfig(forkSel)
		epoch := cfg.Parlia.Epoch
		if forkSel&extraFuzzEpochBit != 0 {
			number -= number % epoch
		}
		header := &types.Header{Number: new(big.Int).SetUint64(number), Time: time, Extra: extra}
		isEpoch := number%epoch == 0
		isLuban := cfg.IsLuban(header.Number)
		isBohr := cfg.IsBohr(header.Number, time)
		snake8 := cfg.IsSnake8(time)
		desc := fmt.Sprintf("cfg=%s number=%d time=%d epoch=%v luban=%v bohr=%v snake8=%v len(extra)=%d",
			cfgName, number, time, isEpoch, isLuban, isBohr, snake8, len(extra))

		// Property 1: nothing panics.
		var (
			valBytes  []byte
			att       *types.VoteAttestation
			attErr    error
			off       int
			offOK     bool
			ts        uint64
			tsOK      bool
			freq      []byte
			freqErr   error
			vals      []common.Address
			blsKeys   []types.BLSPublicKey
			valsErr   error
			turnLen   *uint8
			turnErr   error
			validPath = func(name string, fn func()) { noPanic(t, name+" ["+desc+"]", fn) }
		)
		validPath("getValidatorBytesFromHeader", func() { valBytes = getValidatorBytesFromHeader(header, cfg, epoch) })
		validPath("getVoteAttestationFromHeader", func() { att, attErr = getVoteAttestationFromHeader(header, cfg, epoch) })
		validPath("snake8FreqDataOffset", func() { off, offOK = snake8FreqDataOffset(header, cfg) })
		validPath("extractSnake8ParentTimestamp", func() { ts, tsOK = extractSnake8ParentTimestamp(header, cfg) })
		validPath("parseValidatorFrequencies", func() { freq, freqErr = parseValidatorFrequencies(header, cfg) })
		validPath("parseValidators", func() { vals, blsKeys, valsErr = parseValidators(header, cfg, epoch) })
		validPath("parseTurnLength", func() { turnLen, turnErr = parseTurnLength(header, cfg, epoch) })
		if turnLen != nil && turnErr != nil {
			t.Fatalf("parseTurnLength returned both a length (%d) and an error (%v) [%s]", *turnLen, turnErr, desc)
		}
		// parseTurnLength and snake8FreqDataOffset locate the turn-length byte
		// from the raw count byte independently; they must land on the same byte,
		// or the frequency block is read one byte off on every Bohr epoch block.
		// Only asserted post-Luban: pre-Luban there is no count byte, and
		// snake8FreqDataOffset walks the validator bytes instead (no Chiliz
		// network schedules Bohr without Luban, so the case never arises).
		if turnLen != nil && offOK && isEpoch && isBohr && isLuban {
			pos := off - turnLengthSize
			if pos < extraVanity || pos >= len(extra) || extra[pos] != *turnLen {
				t.Fatalf("turn length %d is not the byte at snake8FreqDataOffset-1 = %d [%s]", *turnLen, pos, desc)
			}
		}

		// Property 2: returned slices live inside the body.
		valOff := assertWithinExtraBody(t, "getValidatorBytesFromHeader ["+desc+"]", valBytes, extra)
		freqOff := assertWithinExtraBody(t, "parseValidatorFrequencies ["+desc+"]", freq, extra)
		if freqErr != nil && freq != nil {
			t.Fatalf("parseValidatorFrequencies returned bytes alongside an error (%v) [%s]", freqErr, desc)
		}
		if offOK {
			if off < extraVanity || off >= len(extra)-extraSeal {
				t.Fatalf("snake8FreqDataOffset returned out-of-body offset %d [%s]", off, desc)
			}
			if !isEpoch && off != extraVanity {
				t.Fatalf("snake8FreqDataOffset = %d on a non-epoch block, want %d [%s]", off, extraVanity, desc)
			}
			if isEpoch && !isLuban && off != extraVanity+len(valBytes) {
				t.Fatalf("snake8FreqDataOffset = %d, want vanity+len(validators) = %d [%s]", off, extraVanity+len(valBytes), desc)
			}
		} else if off != 0 {
			t.Fatalf("snake8FreqDataOffset returned (%d, false), want 0 on failure [%s]", off, desc)
		}
		wantValOff := extraVanity // pre-Luban: no count byte
		if isLuban {
			wantValOff += validatorNumberSize
		}
		if len(valBytes) > 0 && valOff != wantValOff {
			t.Fatalf("validator bytes start at %d, want %d [%s]", valOff, wantValOff, desc)
		}

		// Property 3: the two consumers of snake8FreqDataOffset agree.
		if snake8 {
			if tsOK != (freqErr == nil) {
				t.Fatalf("offset disagreement: extractSnake8ParentTimestamp ok=%v but parseValidatorFrequencies err=%v [%s]", tsOK, freqErr, desc)
			}
		} else if freqErr == nil {
			t.Fatalf("parseValidatorFrequencies succeeded on a pre-Snake8 header [%s]", desc)
		}
		if tsOK {
			if !offOK {
				t.Fatalf("timestamp extracted without a valid offset [%s]", desc)
			}
			prefixLen := len(validatorFrequencyDataPrefix)
			if !bytes.Equal(extra[off:off+prefixLen], validatorFrequencyDataPrefix) {
				t.Fatalf("timestamp extracted but no VFQ prefix at offset %d [%s]", off, desc)
			}
			if want := binary.LittleEndian.Uint64(extra[off+prefixLen : off+prefixLen+8]); ts != want {
				t.Fatalf("timestamp = %d, want %d read at offset %d [%s]", ts, want, off+prefixLen, desc)
			}
			if freqErr == nil && freqOff >= 0 && freqOff != off+prefixLen+8 {
				t.Fatalf("frequency bytes start at %d, want %d [%s]", freqOff, off+prefixLen+8, desc)
			}
		} else if !offOK && freqErr == nil {
			t.Fatalf("parseValidatorFrequencies succeeded without a valid offset [%s]", desc)
		}

		// Property 4/5: validator bytes and parseValidators are coherent.
		if !isLuban {
			if att != nil || attErr != nil {
				t.Fatalf("pre-Luban header yielded a vote attestation (%v, %v) [%s]", att, attErr, desc)
			}
			if blsKeys != nil {
				t.Fatalf("pre-Luban parseValidators returned BLS keys [%s]", desc)
			}
			if isEpoch {
				if len(valBytes)%validatorBytesLengthBeforeLuban != 0 {
					t.Fatalf("pre-Luban epoch validator bytes length %d is not a multiple of %d [%s]", len(valBytes), validatorBytesLengthBeforeLuban, desc)
				}
			}
			if len(valBytes) == 0 {
				if valsErr == nil {
					t.Fatalf("parseValidators succeeded with no validator bytes [%s]", desc)
				}
			} else {
				if valsErr != nil {
					t.Fatalf("parseValidators failed with %d validator bytes: %v [%s]", len(valBytes), valsErr, desc)
				}
				if want := len(valBytes) / validatorBytesLengthBeforeLuban; len(vals) != want {
					t.Fatalf("parseValidators returned %d validators, want %d [%s]", len(vals), want, desc)
				}
				for i, v := range vals {
					if !bytes.Equal(v.Bytes(), valBytes[i*validatorBytesLengthBeforeLuban:(i+1)*validatorBytesLengthBeforeLuban]) {
						t.Fatalf("validator %d = %x does not match its bytes [%s]", i, v, desc)
					}
				}
			}
		} else {
			if !isEpoch && valBytes != nil {
				t.Fatalf("Luban non-epoch header yielded validator bytes [%s]", desc)
			}
			if len(valBytes)%validatorBytesLength != 0 {
				t.Fatalf("Luban validator bytes length %d is not a multiple of %d [%s]", len(valBytes), validatorBytesLength, desc)
			}
			if len(valBytes) > 0 {
				if valsErr != nil {
					t.Fatalf("parseValidators failed with %d validator bytes: %v [%s]", len(valBytes), valsErr, desc)
				}
				if want := len(valBytes) / validatorBytesLength; len(vals) != want || len(blsKeys) != want {
					t.Fatalf("parseValidators returned %d/%d validators/keys, want %d [%s]", len(vals), len(blsKeys), want, desc)
				}
			}
			if att != nil && attErr != nil {
				t.Fatalf("getVoteAttestationFromHeader returned both an attestation and an error (%v) [%s]", attErr, desc)
			}
		}
	})
}

// FuzzHeaderExtraRoundTrip builds a well-formed pre-Luban Snake8 Extra the way
// SetExtraData does (vanity | sorted validators on epoch blocks | VFQ | LE
// parent timestamp | frequency RLP | seal) from fuzzed components and checks the
// parsers return exactly what was embedded.
func FuzzHeaderExtraRoundTrip(f *testing.F) {
	twoAddrs := append(
		common.HexToAddress("0x0100000000000000000000000000000000000001").Bytes(),
		common.HexToAddress("0x0200000000000000000000000000000000000002").Bytes()...)
	f.Add([]byte(nil), twoAddrs, seedNval(2), uint64(7777), []byte{0xc0}, []byte(nil), true, uint32(1))
	f.Add([]byte(nil), twoAddrs, seedNval(2), uint64(7777), []byte{0xc0}, []byte(nil), false, uint32(1))
	f.Add([]byte("chiliz"), twoAddrs, seedNval(2), uint64(0), []byte(nil), []byte{0xff}, true, uint32(0))
	f.Add([]byte(nil), []byte(nil), seedNval(21), ^uint64(0), bytes.Repeat([]byte{0x56, 0x46, 0x51}, 4), []byte(nil), true, uint32(7))

	// "VFQ" straddling two adjacent validator addresses. The structural parser
	// only recognises the prefix at a validator-aligned offset whose tail decodes,
	// so a sequence spanning two fields is not a candidate and must round-trip
	// untouched. Under the old byte-wise scan it truncated the validator list,
	// which is why the generator used to exclude it; it is a seed precisely
	// because the fix makes it legal.
	//
	// The vanity/first-validator straddle is deliberately NOT seeded here: that
	// one begins inside the vanity, which 2.10.6 rejected, so the parser still
	// rejects it. See TestPreLubanValidatorBytesWithVFQBytes.
	straddlePair := make([]byte, 2*common.AddressLength)
	copy(straddlePair[common.AddressLength-2:], []byte{0x56, 0x46})
	straddlePair[common.AddressLength] = 0x51
	f.Add([]byte(nil), straddlePair, seedNval(2), uint64(7777), []byte{0xc0}, []byte(nil), true, uint32(3))
	// COR-213: addresses that contain, or start with, the "VFQ" bytes must
	// round-trip like any other — as long as they are not the lowest-sorted one.
	// A vanity carrying the sequence, and a first address starting with it, stay
	// rejected for parity with 2.10.6; those live in
	// TestPreLubanValidatorBytesWithVFQBytes as wantNil cases, not here.
	vfqAddrs := append(append(
		common.HexToAddress("0x0200000000000000000000564651000000000002").Bytes(),
		common.HexToAddress("0x5646510000000000000000000000000000000003").Bytes()...),
		common.HexToAddress("0xff00000000000000000000000000000000000004").Bytes()...)
	f.Add([]byte("chiliz"), vfqAddrs, seedNval(3), uint64(7777), []byte{0xc0}, []byte(nil), true, uint32(3))
	f.Add([]byte("chiliz"), vfqAddrs, seedNval(3), uint64(7777), []byte(nil), []byte(nil), true, uint32(3))

	vanity, validators, parentTS, freq, seal := mainnetEpochExtraParts()
	f.Add(vanity, validators, seedNval(mainnetValidatorCount), parentTS, freq, seal, true, uint32(mainnetEpochExtraNumber/extraFuzzMainnetEpoch))
	f.Add(vanity, validators, seedNval(mainnetValidatorCount), parentTS, freq, seal, false, uint32(mainnetEpochExtraNumber/extraFuzzMainnetEpoch))

	cfg, _ := extraFuzzConfig(extraFuzzSelMainnet)
	epoch := cfg.Parlia.Epoch

	f.Fuzz(func(t *testing.T, vanity, addrBytes []byte, nval uint8, parentTS uint64, freq, seal []byte, epochBlock bool, blockIdx uint32) {
		n := 1 + int(nval)%21
		validators := roundTripValidators(addrBytes, n)
		sort.Sort(validatorsAscending(validators))
		vanity = fitBytes(vanity, extraVanity)
		excludeLegacyVFQRejections(vanity, validators)
		// The tail must be a shape an honest producer emits (empty, or the RLP
		// frequency list): the parser locates the frequency block structurally.
		freq = roundTripFrequencyTail(validators, freq)

		number := uint64(blockIdx) * epoch
		if !epochBlock {
			number += 1 + uint64(blockIdx)%(epoch-1)
		}
		var embedded []common.Address
		if epochBlock {
			embedded = validators
		}
		extra := buildPreLubanSnake8Extra(vanity, embedded, parentTS, freq, seal)
		header := &types.Header{Number: new(big.Int).SetUint64(number), Time: parentTS + 3, Extra: extra}
		desc := fmt.Sprintf("number=%d epoch=%v n=%d len(freq)=%d", number, epochBlock, n, len(freq))

		wantOff := extraVanity
		if epochBlock {
			wantOff += n * common.AddressLength
		}
		if off, ok := snake8FreqDataOffset(header, cfg); !ok || off != wantOff {
			t.Fatalf("snake8FreqDataOffset = (%d, %v), want (%d, true) [%s]", off, ok, wantOff, desc)
		}

		gotTS, ok := extractSnake8ParentTimestamp(header, cfg)
		if !ok || gotTS != parentTS {
			t.Fatalf("extractSnake8ParentTimestamp = (%d, %v), want (%d, true) [%s]", gotTS, ok, parentTS, desc)
		}

		gotFreq, err := parseValidatorFrequencies(header, cfg)
		if err != nil {
			t.Fatalf("parseValidatorFrequencies: %v [%s]", err, desc)
		}
		if !bytes.Equal(gotFreq, freq) {
			t.Fatalf("parseValidatorFrequencies = %x, want %x [%s]", gotFreq, freq, desc)
		}
		assertWithinExtraBody(t, "parseValidatorFrequencies", gotFreq, extra)

		valBytes := getValidatorBytesFromHeader(header, cfg, epoch)
		gotVals, bls, err := parseValidators(header, cfg, epoch)
		if !epochBlock {
			if valBytes != nil {
				t.Fatalf("non-epoch block yielded validator bytes %x [%s]", valBytes, desc)
			}
			if err == nil {
				t.Fatalf("parseValidators succeeded on a non-epoch block: %v [%s]", gotVals, desc)
			}
			return
		}
		if len(valBytes) != n*validatorBytesLengthBeforeLuban {
			t.Fatalf("getValidatorBytesFromHeader returned %d bytes, want %d [%s]", len(valBytes), n*validatorBytesLengthBeforeLuban, desc)
		}
		if err != nil {
			t.Fatalf("parseValidators: %v [%s]", err, desc)
		}
		if bls != nil {
			t.Fatalf("parseValidators returned BLS keys on a pre-Luban block [%s]", desc)
		}
		if len(gotVals) != len(validators) {
			t.Fatalf("parseValidators returned %d validators, want %d [%s]", len(gotVals), len(validators), desc)
		}
		for i := range validators {
			if gotVals[i] != validators[i] {
				t.Fatalf("validator %d = %x, want %x [%s]", i, gotVals[i], validators[i], desc)
			}
		}
	})
}

// TestRoundTripBuilderMatchesMainnetExtra pins the round-trip generator to the
// producer's real layout: rebuilding the mainnet epoch block from its parts must
// reproduce the on-chain bytes exactly.
func TestRoundTripBuilderMatchesMainnetExtra(t *testing.T) {
	vanity, validatorBytes, parentTS, freq, seal := mainnetEpochExtraParts()
	validators := make([]common.Address, mainnetValidatorCount)
	for i := range validators {
		validators[i] = common.BytesToAddress(validatorBytes[i*common.AddressLength : (i+1)*common.AddressLength])
	}
	if !sort.IsSorted(validatorsAscending(validators)) {
		t.Fatalf("mainnet validators are not sorted ascending")
	}
	rebuilt := buildPreLubanSnake8Extra(vanity, validators, parentTS, freq, seal)
	if want := common.FromHex(mainnetEpochExtraHex); !bytes.Equal(rebuilt, want) {
		t.Fatalf("rebuilt extra differs from mainnet bytes:\n got %x\nwant %x", rebuilt, want)
	}
	if parentTS != mainnetEpochExtraTime-3 {
		t.Fatalf("embedded parent ts = %d, want header time - 3 = %d", parentTS, mainnetEpochExtraTime-3)
	}

	cfg, _ := extraFuzzConfig(extraFuzzSelMainnet)
	header := &types.Header{Number: new(big.Int).SetUint64(mainnetEpochExtraNumber), Time: mainnetEpochExtraTime, Extra: rebuilt}
	got, _, err := parseValidators(header, cfg, cfg.Parlia.Epoch)
	if err != nil {
		t.Fatalf("parseValidators on mainnet block: %v", err)
	}
	if len(got) != mainnetValidatorCount {
		t.Fatalf("parseValidators returned %d validators, want %d", len(got), mainnetValidatorCount)
	}
}

// TestCOR213VFQByteSequenceInValidatorAddress is the regression test for
// COR-213: getValidatorBytesFromHeader's pre-Luban branch used to find the end
// of the validator list by scanning header.Extra from offset 0 for the first
// "VFQ" (0x56 0x46 0x51), so a validator address containing those three bytes
// was mistaken for the frequency-block prefix.
func TestCOR213VFQByteSequenceInValidatorAddress(t *testing.T) {
	// Before the COR-213 fix both cases failed on develop: the mid-address case
	// lost the whole epoch validator set ("invalid validators bytes"), the
	// start-of-address case silently returned 1 of 3 validators.
	cfg, _ := extraFuzzConfig(extraFuzzSelMainnet)
	epoch := cfg.Parlia.Epoch

	// Sorted ascending, as the producer emits them.
	low := common.HexToAddress("0x0100000000000000000000000000000000000001")
	midVFQ := common.HexToAddress("0x0200000000000000000000564651000000000002")   // "VFQ" at bytes 11..13
	startVFQ := common.HexToAddress("0x5646510000000000000000000000000000000003") // "VFQ" at bytes 0..2
	high := common.HexToAddress("0xff00000000000000000000000000000000000004")

	cases := []struct {
		name       string
		validators []common.Address
	}{
		{"sequence mid-address", []common.Address{low, midVFQ, high}},
		{"sequence at start of address", []common.Address{low, startVFQ, high}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !sort.IsSorted(validatorsAscending(tc.validators)) {
				t.Fatalf("test fixture is not sorted")
			}
			// A real frequency list over this validator set: calcFrequencyRLP
			// emits either that or nothing, never the empty list 0xc0 (see
			// decodesAsFrequencyList).
			extra := buildPreLubanSnake8Extra(nil, tc.validators, 7777, cor213FrequencyTail(t, tc.validators), nil)
			header := &types.Header{Number: new(big.Int).SetUint64(epoch), Time: 2000, Extra: extra}

			got, _, err := parseValidators(header, cfg, epoch)
			if err != nil {
				t.Fatalf("parseValidators: %v (validator bytes = %x)", err, getValidatorBytesFromHeader(header, cfg, epoch))
			}
			if len(got) != len(tc.validators) {
				t.Fatalf("parseValidators returned %d validators %v, want %d %v", len(got), got, len(tc.validators), tc.validators)
			}
			for i := range got {
				if got[i] != tc.validators[i] {
					t.Fatalf("validator %d = %x, want %x", i, got[i], tc.validators[i])
				}
			}
			if ts, ok := extractSnake8ParentTimestamp(header, cfg); !ok || ts != 7777 {
				t.Fatalf("extractSnake8ParentTimestamp = (%d, %v), want (7777, true)", ts, ok)
			}
		})
	}
}

// seedNval converts a desired validator count into the nval byte a seed has to
// carry: the fuzz body decodes it as n = 1 + nval%21, so passing the count
// itself silently seeds count+1 validators and pads the address bytes with a
// phantom all-zero validator that sorts ahead of the real ones.
func seedNval(n int) uint8 { return uint8(n - 1) }

// excludeLegacyVFQRejections steers the generated header clear of the two shapes
// getValidatorBytesFromHeader still rejects for parity with 2.10.6, and only
// those two: a "VFQ" occurrence beginning inside the vanity (including one
// straddling into the first two bytes of the body), and a lowest-sorted
// validator address that itself begins with the prefix. Everything else the old
// byte-wise scan used to mangle — the sequence inside any address, at the start
// of any address but the first, or straddling two adjacent addresses — is legal
// after COR-213 and is deliberately left for the fuzzer to generate.
//
// The vanity rules are per-field (no occurrence inside it; its last two bytes
// cannot begin one that finishes in the body), so they survive the sort. The
// address rule constrains only the minimum, so it is applied to the minimum and
// re-sorted until it holds; each pass fixes one address for good, so it
// terminates in at most len(validators) rounds.
func excludeLegacyVFQRejections(vanity []byte, validators []common.Address) {
	for {
		i := bytes.Index(vanity, validatorFrequencyDataPrefix)
		if i < 0 {
			break
		}
		vanity[i+2] ^= 0xff
	}
	if n := len(vanity); n >= 1 && vanity[n-1] == validatorFrequencyDataPrefix[0] {
		vanity[n-1] ^= 0xff
	} else if n >= 2 && vanity[n-2] == validatorFrequencyDataPrefix[0] && vanity[n-1] == validatorFrequencyDataPrefix[1] {
		vanity[n-1] ^= 0xff
	}
	for range validators {
		if !bytes.HasPrefix(validators[0].Bytes(), validatorFrequencyDataPrefix) {
			return
		}
		raw := validators[0].Bytes()
		raw[2] ^= 0xff
		validators[0] = common.BytesToAddress(raw)
		sort.Sort(validatorsAscending(validators))
	}
}

// roundTripValidators builds the validator set for the round trip under the
// COR-213 threat model: the fuzzer fully controls ONE address (the attacker's,
// the first 20 bytes of addrBytes) while the remaining n-1 are derived from
// addrBytes by hashing, i.e. honest keypair-shaped addresses the attacker cannot
// choose. Giving the fuzzer every address models a colluding majority, which can
// already do worse than confuse this parser. When addrBytes carries n full
// addresses (the real mainnet seed) they are used verbatim.
func roundTripValidators(addrBytes []byte, n int) []common.Address {
	validators := make([]common.Address, n)
	if len(addrBytes) == n*common.AddressLength {
		for i := range validators {
			validators[i] = common.BytesToAddress(addrBytes[i*common.AddressLength : (i+1)*common.AddressLength])
		}
		return validators
	}
	validators[0] = common.BytesToAddress(fitBytes(addrBytes, common.AddressLength))
	for i := 1; i < n; i++ {
		validators[i] = common.BytesToAddress(crypto.Keccak256(addrBytes, []byte{byte(i)}))
	}
	return validators
}

// roundTripFrequencyTail turns fuzzed bytes into a frequency tail an honest
// producer can emit: an already well-formed tail (empty, or an RLP list of
// entries such as the real mainnet bytes) is kept verbatim; anything else seeds
// per-validator frequencies for a freshly encoded list.
func roundTripFrequencyTail(validators []common.Address, seed []byte) []byte {
	if len(seed) == 0 || decodesAsFrequencyList(seed) {
		return seed
	}
	entries := make([]frequencyBlockEntry, len(validators))
	for i, v := range validators {
		entries[i] = frequencyBlockEntry{Address: v, Frequency: new(big.Int).SetUint64(uint64(seed[i%len(seed)]))}
	}
	out, err := rlp.EncodeToBytes(entries)
	if err != nil {
		panic(err)
	}
	return out
}
