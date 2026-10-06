package systemcontract

// COR-212: fuzz the RuntimeUpgrade EVM hook.
//
// evmHookRuntimeUpgrade.Run is the one EVM hook that writes state: a call from
// the RuntimeUpgrade system contract (0x...7004) carrying upgradeTo(address,bytes)
// replaces a contract's bytecode in place via StateDB.SetCode. Everything that
// protects that write is in Run itself: the HasRuntimeUpgrade rule, the caller
// gate, and the ABI decode. These targets pin, for arbitrary inputs:
//
//   - no panic, whatever the input (matchesMethod -> UnpackValues on garbage);
//   - the guard order: HasRuntimeUpgrade is checked before the caller, so when
//     both fail errNotSupported wins;
//   - no state write, and no state read, on any refusing path (the recorder
//     panics on reads and records writes);
//   - a well-formed upgradeTo from the right caller performs exactly one
//     SetCode with the decoded address and byte-identical code;
//   - every malformation returns errMethodNotFound (the errFailedToUnpack
//     branch is unreachable: UnpackValues always yields common.Address and
//     []byte for these types);
//   - two ABI-decoder leniencies the hook inherits from accounts/abi: trailing
//     bytes after a valid encoding are ignored (UnpackValues never checks that
//     the calldata is fully consumed) and truncating only the final zero-padding
//     word still decodes (lengthPrefixPointsTo requires offset+32+length <= len,
//     not equality). Every block since RuntimeUpgrade went live was validated by
//     a decoder that accepts both, so tightening either one is a consensus
//     change on a live chain unless it is fork-gated — whether or not a
//     historical upgradeTo call actually happened to be non-canonical. They are
//     pinned here so that a tightening has to be a deliberate, fork-gated act;
//   - RequiredGas is 0 for every input;
//   - the factory and IsEvmHook agree on every address, and the hook address
//     is never also a system contract.

import (
	"bytes"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	syscommon "github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/params"
)

// codeWrite is one recorded StateDB.SetCode call.
type codeWrite struct {
	addr common.Address
	code []byte
}

// recordingStateDb is the minimum StateDB the hook context requires. SetCode
// records; every read panics, so a fuzz run doubles as proof that the hook
// never touches state on a refusing path (a panic fails the fuzz target).
type recordingStateDb struct {
	writes []codeWrite
}

func (s *recordingStateDb) GetCodeHash(common.Address) common.Hash {
	panic("RuntimeUpgrade hook must not read GetCodeHash")
}

func (s *recordingStateDb) GetCode(common.Address) []byte {
	panic("RuntimeUpgrade hook must not read GetCode")
}

func (s *recordingStateDb) GetCodeSize(common.Address) int {
	panic("RuntimeUpgrade hook must not read GetCodeSize")
}

func (s *recordingStateDb) SetCode(addr common.Address, code []byte) {
	// Copy: the hook hands us a sub-slice of its input; a later mutation of the
	// input must not be able to hide a mismatch.
	s.writes = append(s.writes, codeWrite{addr: addr, code: bytes.Clone(code)})
}

// upgradeToABI wraps the hook's method so abi.ABI.Pack produces selector+args
// exactly the way a caller contract would.
var upgradeToABI = abi.ABI{Methods: map[string]abi.Method{"upgradeTo": upgradeToMethod}}

// Input shaping modes for FuzzRuntimeUpgradeHook. Each mode has a fixed
// expected outcome under an open gate (see expectedForShape).
const (
	shapeWellFormed        uint8 = iota // abi.Pack(upgradeTo, addr, code)
	shapeSelectorRawTail                // upgradeTo selector + fuzzed tail
	shapeTrailingGarbage                // well-formed + trailing bytes
	shapeOffsetPastEnd                  // well-formed, bytes offset points past the end
	shapeTruncatedIntoData              // well-formed, cut into the code bytes
	shapeTruncatedPadding               // well-formed, cut only inside the padding word
	shapeRaw                            // fuzzed bytes as-is (any selector)
	shapeCount
)

// packUpgradeTo returns the canonical upgradeTo(address,bytes) calldata and the
// number of zero padding bytes that follow the code in that encoding.
func packUpgradeTo(t *testing.T, addr common.Address, code []byte) (packed []byte, padding int) {
	packed, err := upgradeToABI.Pack("upgradeTo", addr, code)
	if err != nil {
		t.Fatalf("abi.Pack(upgradeTo, %s, %d bytes): %v", addr.Hex(), len(code), err)
	}
	return packed, (32 - len(code)%32) % 32
}

// shapeInput builds the Run input for a shaping mode. It returns the input and
// the mode actually used (a mode that cannot be built for this code falls back
// to shapeWellFormed).
func shapeInput(t *testing.T, mode uint8, raw []byte, addr common.Address, code []byte, delta uint8) ([]byte, uint8) {
	mode %= shapeCount
	switch mode {
	case shapeWellFormed:
		packed, _ := packUpgradeTo(t, addr, code)
		return packed, mode
	case shapeSelectorRawTail:
		return append(bytes.Clone(upgradeToMethod.ID), raw...), mode
	case shapeTrailingGarbage:
		packed, _ := packUpgradeTo(t, addr, code)
		if len(raw) == 0 {
			raw = []byte{delta}
		}
		return append(packed, raw...), mode
	case shapeOffsetPastEnd:
		packed, _ := packUpgradeTo(t, addr, code)
		// Word 2 of the arguments (input[4+32:4+64]) is the offset of the
		// dynamic bytes argument, measured from the start of the arguments.
		// Any offset >= len(args) - 31 makes offset+32 exceed the slice.
		tailLen := len(packed) - 4
		offset := new(big.Int).SetInt64(int64(tailLen + int(delta)))
		copy(packed[4+32:4+64], common.LeftPadBytes(offset.Bytes(), 32))
		return packed, mode
	case shapeTruncatedIntoData:
		packed, padding := packUpgradeTo(t, addr, code)
		// Cut strictly past the padding so at least one code byte (or, for
		// empty code, part of the length word) is missing.
		span := len(code)
		if span == 0 {
			span = 1
		}
		cut := padding + 1 + int(delta)%span
		return packed[:len(packed)-cut], mode
	case shapeTruncatedPadding:
		packed, padding := packUpgradeTo(t, addr, code)
		if padding == 0 {
			return packed, shapeWellFormed
		}
		cut := 1 + int(delta)%padding
		return packed[:len(packed)-cut], mode
	default: // shapeRaw
		return raw, mode
	}
}

// shapeCaller derives the hook caller from the fuzz inputs, biased towards the
// legitimate RuntimeUpgrade contract and its near misses.
func shapeCaller(mode uint8, raw []byte, delta uint8) common.Address {
	switch mode % 4 {
	case 0:
		return runtimeUpgradeContract
	case 1:
		// One-byte neighbour: flip a single bit of one byte, so the address
		// differs from the legitimate caller by exactly one byte.
		addr := runtimeUpgradeContract
		addr[int(delta)%common.AddressLength] ^= 1 << (delta % 8)
		return addr
	case 2:
		// Numeric neighbour: the adjacent system contracts 0x...7003/0x...7005.
		step := big.NewInt(1)
		if delta%2 == 0 {
			step = big.NewInt(-1)
		}
		return common.BigToAddress(new(big.Int).Add(runtimeUpgradeContract.Big(), step))
	default:
		return common.BytesToAddress(raw)
	}
}

// expectation is the reference model's verdict for one Run call.
type expectation struct {
	err   error
	write *codeWrite // nil when no SetCode must happen
}

// expectedForShape is the reference model for an open gate (HasRuntimeUpgrade
// and the right caller). Modes with a fixed outcome are decided without
// consulting the ABI decoder; only the two free-form modes use UnpackValues as
// the oracle, and there the value-level check against the recorder still binds.
func expectedForShape(mode uint8, input []byte, addr common.Address, code []byte) expectation {
	switch mode {
	case shapeWellFormed, shapeTrailingGarbage, shapeTruncatedPadding:
		return expectation{write: &codeWrite{addr: addr, code: code}}
	case shapeOffsetPastEnd, shapeTruncatedIntoData:
		return expectation{err: errMethodNotFound}
	}
	if len(input) < 4 || !bytes.Equal(input[:4], upgradeToMethod.ID) {
		return expectation{err: errMethodNotFound}
	}
	values, err := upgradeToMethod.Inputs.UnpackValues(input[4:])
	if err != nil || len(values) != 2 {
		return expectation{err: errMethodNotFound}
	}
	return expectation{write: &codeWrite{addr: values[0].(common.Address), code: values[1].([]byte)}}
}

func FuzzRuntimeUpgradeHook(f *testing.F) {
	// Pin the selector the chain has been calling since RuntimeUpgrade went
	// live: keccak256("upgradeTo(address,bytes)")[:4].
	if want := common.Hex2Bytes("6fbc15e9"); !bytes.Equal(upgradeToMethod.ID, want) {
		f.Fatalf("upgradeTo selector = %x, want %x", upgradeToMethod.ID, want)
	}

	historicalCode := common.Hex2Bytes("60566037600b82828239805160001a607314602a57634e487b7160e01b600052600060045260246000fd5b30600052607381538281f3fe73000000000000000000000000000000000000000030146080604052600080fdfea2646970667358221220e8e6b1d3408504bc107c141b02b247333ed5bfb36f4a2948a69815b2f5aec0f264736f6c634300080b0033")
	validatorAddr := common.Hex2Bytes("0000000000000000000000000000000000001000")

	// Seeds: (inputMode, raw, addrRaw, code, delta, callerMode, callerRaw, callerDelta, hasRuntimeUpgrade)
	for mode := uint8(0); mode < shapeCount; mode++ {
		f.Add(mode, []byte{}, validatorAddr, historicalCode, uint8(0), uint8(0), []byte{}, uint8(0), true)
		f.Add(mode, []byte{0xff}, validatorAddr, []byte{}, uint8(7), uint8(0), []byte{}, uint8(0), true)
	}
	// Both gates closed at once: pins which error wins.
	f.Add(shapeWellFormed, []byte{}, validatorAddr, historicalCode, uint8(0), uint8(1), []byte{}, uint8(0), false)
	// Rule off, caller legitimate: the fork gate alone must refuse. Without
	// this seed the seed corpus never exercises the fork gate on its own, so a
	// regression that only closes it for non-RuntimeUpgrade callers (i.e. a
	// pre-fork upgradeTo from 0x...7004 succeeding) would run the corpus green.
	f.Add(shapeWellFormed, []byte{}, validatorAddr, historicalCode, uint8(0), uint8(0), []byte{}, uint8(0), false)
	// Right rule, wrong caller.
	f.Add(shapeWellFormed, []byte{}, validatorAddr, historicalCode, uint8(0), uint8(1), []byte{}, uint8(3), true)
	f.Add(shapeWellFormed, []byte{}, validatorAddr, historicalCode, uint8(0), uint8(2), []byte{}, uint8(1), true)
	f.Add(shapeWellFormed, []byte{}, validatorAddr, historicalCode, uint8(0), uint8(3), []byte{0x70, 0x04}, uint8(0), true)
	// Free-form tails: empty, selector only, selector + one head word, bogus selector.
	f.Add(shapeRaw, []byte{}, []byte{}, []byte{}, uint8(0), uint8(0), []byte{}, uint8(0), true)
	f.Add(shapeRaw, common.Hex2Bytes("6fbc15e9"), []byte{}, []byte{}, uint8(0), uint8(0), []byte{}, uint8(0), true)
	f.Add(shapeRaw, common.Hex2Bytes("6fbc15e90000000000000000000000000000000000000000000000000000000000001000"), []byte{}, []byte{}, uint8(0), uint8(0), []byte{}, uint8(0), true)
	f.Add(shapeRaw, common.Hex2Bytes("deadbeef"), []byte{}, []byte{}, uint8(0), uint8(0), []byte{}, uint8(0), true)
	// The real historical upgradeTo calldata, byte for byte, fed through the
	// free-form path: the only seed on which shapeRaw reaches the accepting
	// branch of expectedForShape, so the corpus covers a raw success and not
	// just raw refusals.
	f.Add(shapeRaw, common.Hex2Bytes("6fbc15e900000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000040000000000000000000000000000000000000000000000000000000000000008d60566037600b82828239805160001a607314602a57634e487b7160e01b600052600060045260246000fd5b30600052607381538281f3fe73000000000000000000000000000000000000000030146080604052600080fdfea2646970667358221220e8e6b1d3408504bc107c141b02b247333ed5bfb36f4a2948a69815b2f5aec0f264736f6c634300080b003300000000000000000000000000000000000000"), []byte{}, []byte{}, uint8(0), uint8(0), []byte{}, uint8(0), true)
	// Selector + head that claims an absurd bytes offset / length.
	f.Add(shapeSelectorRawTail, common.Hex2Bytes("0000000000000000000000000000000000000000000000000000000000001000ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"), []byte{}, []byte{}, uint8(0), uint8(0), []byte{}, uint8(0), true)
	f.Add(shapeSelectorRawTail, common.Hex2Bytes("000000000000000000000000000000000000000000000000000000000000100000000000000000000000000000000000000000000000000000000000000040ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"), []byte{}, []byte{}, uint8(0), uint8(0), []byte{}, uint8(0), true)

	f.Fuzz(func(t *testing.T, inputMode uint8, raw, addrRaw, code []byte, delta, callerMode uint8, callerRaw []byte, callerDelta uint8, hasRuntimeUpgrade bool) {
		targetAddr := common.BytesToAddress(addrRaw)
		input, mode := shapeInput(t, inputMode, raw, targetAddr, code, delta)
		caller := shapeCaller(callerMode, callerRaw, callerDelta)

		// Property 7: abi.Pack round-trip. Every derived-from-packed shape
		// starts with the method selector.
		if mode != shapeRaw && !bytes.Equal(input[:4], upgradeToMethod.ID) {
			t.Fatalf("mode %d: packed input selector = %x, want %x", mode, input[:4], upgradeToMethod.ID)
		}

		recorder := &recordingStateDb{}
		hook := &evmHookRuntimeUpgrade{context: EvmHookContext{
			CallerAddress: caller,
			StateDb:       recorder,
			ChainRules:    params.Rules{HasRuntimeUpgrade: hasRuntimeUpgrade},
		}}

		// Property 6: the hook is free for every input, before and after the gate.
		if gas := hook.RequiredGas(input); gas != 0 {
			t.Fatalf("RequiredGas(%d bytes) = %d, want 0", len(input), gas)
		}

		// Property 1 (no panic) is implicit: a panic here fails the target.
		out, err := hook.Run(input)

		if out != nil {
			t.Fatalf("Run returned %d bytes of output, want nil (hook never returns data)", len(out))
		}
		if errors.Is(err, errFailedToUnpack) {
			t.Fatalf("Run returned errFailedToUnpack; that branch is unreachable for (address,bytes) decoding (input=%x)", input)
		}

		// Reference model. Guard order matters and is pinned here: the fork
		// rule is consulted before the caller, so with both closed the
		// verdict is errNotSupported, never errInvalidCaller.
		var want expectation
		switch {
		case !hasRuntimeUpgrade:
			want = expectation{err: errNotSupported}
		case caller != runtimeUpgradeContract:
			want = expectation{err: errInvalidCaller}
		default:
			want = expectedForShape(mode, input, targetAddr, code)
		}

		if !hasRuntimeUpgrade && caller != runtimeUpgradeContract && !errors.Is(err, errNotSupported) {
			t.Fatalf("guard order: rule off and caller %s wrong, got %v, want errNotSupported (rule check must come first)", caller.Hex(), err)
		}

		// Properties 2, 3, 5: the refusing verdict.
		if want.err != nil {
			if !errors.Is(err, want.err) {
				t.Fatalf("mode %d hasRuntimeUpgrade=%t caller=%s: Run err = %v, want %v (input=%x)", mode, hasRuntimeUpgrade, caller.Hex(), err, want.err, input)
			}
			if len(recorder.writes) != 0 {
				t.Fatalf("mode %d hasRuntimeUpgrade=%t caller=%s: Run refused with %v but wrote state: %d SetCode call(s), first to %s", mode, hasRuntimeUpgrade, caller.Hex(), err, len(recorder.writes), recorder.writes[0].addr.Hex())
			}
			return
		}

		// Property 4: the accepting verdict — exactly one write, with the
		// decoded address and byte-identical code.
		if err != nil {
			t.Fatalf("mode %d: Run err = %v, want nil (input=%x)", mode, err, input)
		}
		if len(recorder.writes) != 1 {
			t.Fatalf("mode %d: Run succeeded with %d SetCode call(s), want exactly 1", mode, len(recorder.writes))
		}
		got := recorder.writes[0]
		if got.addr != want.write.addr {
			t.Fatalf("mode %d: SetCode address = %s, want %s", mode, got.addr.Hex(), want.write.addr.Hex())
		}
		if !bytes.Equal(got.code, want.write.code) {
			t.Fatalf("mode %d: SetCode code (%d bytes) differs from the encoded code (%d bytes)", mode, len(got.code), len(want.write.code))
		}
	})
}

func FuzzEvmHookFactory(f *testing.F) {
	// Seeds: (mode, delta, raw)
	f.Add(uint8(0), uint8(0), []byte{})                    // the hook address
	f.Add(uint8(1), uint8(19), []byte{})                   // last-byte neighbour
	f.Add(uint8(1), uint8(0), []byte{})                    // first-byte neighbour
	f.Add(uint8(2), uint8(4), []byte{})                    // RuntimeUpgrade system contract
	f.Add(uint8(2), uint8(0), []byte{})                    // 0x...7000
	f.Add(uint8(2), uint8(7), []byte{})                    // 0x...7007
	f.Add(uint8(3), uint8(0), []byte{0x7f, 0x01})          // hook address via raw bytes
	f.Add(uint8(3), uint8(0), []byte{0x7f, 0x00})          // numeric neighbour below
	f.Add(uint8(3), uint8(0), []byte{0x7f, 0x02})          // numeric neighbour above
	f.Add(uint8(3), uint8(0), []byte{})                    // zero address
	f.Add(uint8(3), uint8(0), bytes.Repeat([]byte{1}, 32)) // longer than an address

	f.Fuzz(func(t *testing.T, mode, delta uint8, raw []byte) {
		// Built per iteration: the recorder is mutable, and sharing one across
		// iterations would make any future assertion on it order-dependent.
		ctx := EvmHookContext{
			CallerAddress: runtimeUpgradeContract,
			StateDb:       &recordingStateDb{},
			ChainRules:    params.Rules{HasRuntimeUpgrade: true},
		}

		var addr common.Address
		switch mode % 4 {
		case 0:
			addr = evmHookRuntimeUpgradeAddress
		case 1:
			addr = evmHookRuntimeUpgradeAddress
			addr[int(delta)%common.AddressLength] ^= 1 << (delta % 8)
		case 2:
			addr = common.BigToAddress(big.NewInt(0x7000 + int64(delta%8)))
		default:
			addr = common.BytesToAddress(raw)
		}

		isHook := IsEvmHook(addr)
		hook := CreateEvmHook(addr, ctx)

		// IsEvmHook(addr) <=> CreateEvmHook(addr, ctx) != nil.
		if isHook != (hook != nil) {
			t.Fatalf("%s: IsEvmHook = %t but CreateEvmHook nil = %t; the two must stay in sync", addr.Hex(), isHook, hook == nil)
		}
		// The dispatch table is exactly one address, shared with the registry
		// the EVM consults (common/systemcontract).
		if isHook != (addr == evmHookRuntimeUpgradeAddress) {
			t.Fatalf("%s: IsEvmHook = %t, want %t", addr.Hex(), isHook, addr == evmHookRuntimeUpgradeAddress)
		}
		if isHook != (addr == syscommon.EvmHookRuntimeUpgradeAddress) {
			t.Fatalf("%s: core/vm/systemcontract and common/systemcontract disagree on the hook address", addr.Hex())
		}
		// A hook address is never a system contract: system contracts bypass
		// hooks, so an overlap would make the address unreachable one way.
		if isHook && syscommon.IsSystemContract(addr) {
			t.Fatalf("%s is both an EVM hook and a system contract", addr.Hex())
		}
		if hook == nil {
			return
		}
		if hook.Name() == "" {
			t.Fatalf("%s: created hook has an empty Name", addr.Hex())
		}
		if gas := hook.RequiredGas(raw); gas != 0 {
			t.Fatalf("%s: RequiredGas = %d, want 0", addr.Hex(), gas)
		}
	})
}
