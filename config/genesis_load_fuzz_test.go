package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/params"
)

// Genesis JSON is parsed in two places in this tree, and the difference matters:
//
//   - embeded.go parses the three compile-time-embedded network configs at
//     package init and PANICS on a bad one. That is the right behaviour for a
//     file we ship: a node that cannot read its own network config must not
//     start with half of one.
//   - cmd/geth/config.go readGenesisConfig and cmd/utils/flags.go
//     (--genesis / --override.genesis) decode an OPERATOR-supplied file with
//     json.NewDecoder(...).Decode and Fatalf on error. The staging fleet runs
//     per-host genesis.json files (chainId 88883), hand-edited often enough
//     that this parser sees input nobody reviewed.
//
// FuzzGenesisJSON feeds both parsers arbitrary bytes and mutations of the three
// shipped configs, and pins:
//
//  1. Parsing never panics, and the two parsers agree. They differ in exactly
//     one documented way — Decode stops after the first JSON value and ignores
//     trailing bytes, Unmarshal rejects them — so a file that is a single JSON
//     value is accepted by both or by neither. If they ever disagreed on
//     anything else, the config a node runs would depend on which flag loaded
//     it.
//  2. A parse failure is an error, not a half-filled config: encoding/json
//     fills fields until it fails, so the only thing standing between a
//     truncated genesis and a node running on it is that the caller must not
//     use the value. mustParseGenesisConfigFromJson enforces that by panicking;
//     this asserts it does, for every input the parser rejects.
//  3. A config that survives a marshal/unmarshal round trip is unchanged.
//     Anything else means `geth dumpgenesis` (or any tool that re-serialises a
//     genesis) emits a file that describes a different chain than it read. This
//     one holds for non-negative values only: upstream's math.HexOrDecimal256
//     parses a negative into a value it cannot serialise back, so such a genesis
//     has no re-parsable form for the round trip to preserve. See
//     assertGenesisRoundTrips.
//  4. Cross-check: the three embedded network configs still parse and still
//     pass CheckConfigForkOrder. This runs on every iteration, so any campaign
//     — even the seed corpus alone under plain `go test` — re-asserts the
//     shipped schedules. It is what catches a blobSchedule entry going missing
//     from a network file: CheckConfigForkOrder rejects a config that schedules
//     cancun/prague/osaka/bpo*/amsterdam without one.
//
// Properties 1 to 3 are skipped when the input is an UNMUTATED shipped file: a
// compile-time constant whose verdict cannot vary between iterations, and by far
// the most expensive input this target sees. They are asserted on all three of
// those files once, in TestEmbeddedGenesisParsesAndRoundTrips. Property 4 still
// runs on every iteration.
//
// CheckConfigForkOrder is also run on every successfully parsed fuzz input, for
// the no-panic half of the property: it is the same function `geth init` runs
// on whatever the operator hands it. "Successfully parsed" means by EITHER
// parser — an input the operator's Decode accepts and Unmarshal rejects, which
// is the trailing-bytes case above, still reaches the fork-order check through
// the operator result.

// Mutation operators. Each is a shape of edit an operator file plausibly takes
// on: a corrupted byte, a truncated copy, a key deleted, a key pasted twice, a
// fork time zeroed. The last one matters on its own, because nil and 0 are
// different: a missing snake8Time means "never", snake8Time: 0 means "since
// genesis". Deleting the line and zeroing it are two different chains.
const (
	genesisOpNone uint8 = iota
	genesisOpFlipByte
	genesisOpTruncate
	genesisOpDeleteLine
	genesisOpDuplicateLine
	genesisOpZeroValue
	genesisOpSplice
	genesisOpCount
)

// genesisBases are the inputs a fuzz case can mutate: raw fuzzer bytes, then
// the three shipped network files.
func genesisBases(raw []byte) [][]byte {
	return [][]byte{raw, chilizRawGenesisConfig, spicyRawGenesisConfig, scovilleRawGenesisConfig}
}

// genesisLineBounds returns the half-open bounds of the line containing pos,
// excluding the trailing newline. The embedded files are pretty-printed one key
// per line, which is what makes line-level edits key-level edits.
func genesisLineBounds(src []byte, pos int) (start, end int) {
	start = bytes.LastIndexByte(src[:pos], '\n') + 1
	if rel := bytes.IndexByte(src[pos:], '\n'); rel >= 0 {
		end = pos + rel
	} else {
		end = len(src)
	}
	return start, end
}

// genesisMutate applies one operator to src at pos. It never returns src's
// backing array, so a mutation cannot corrupt the embedded configs (which are
// package-level []byte and therefore writable — a fuzz target that edited them
// in place would poison every later iteration and every other test in the
// package).
func genesisMutate(src []byte, op uint8, pos uint32, extra []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	if len(out) == 0 {
		if op%genesisOpCount == genesisOpSplice {
			return append(out, extra...)
		}
		return out
	}
	at := int(pos % uint32(len(out)))

	switch op % genesisOpCount {
	case genesisOpFlipByte:
		out[at] ^= 0xff
		return out

	case genesisOpTruncate:
		return out[:at]

	case genesisOpDeleteLine:
		start, end := genesisLineBounds(out, at)
		if end < len(out) {
			end++ // take the newline with the line
		}
		return append(out[:start:start], out[end:]...)

	case genesisOpDuplicateLine:
		start, end := genesisLineBounds(out, at)
		if end < len(out) {
			end++
		}
		line := out[start:end]
		dup := make([]byte, 0, len(out)+len(line))
		dup = append(dup, out[:end]...)
		dup = append(dup, line...)
		return append(dup, out[end:]...)

	case genesisOpZeroValue:
		// "someTime": 12345,  ->  "someTime": 0,
		start, end := genesisLineBounds(out, at)
		line := out[start:end]
		colon := bytes.IndexByte(line, ':')
		if colon < 0 {
			return out
		}
		replacement := []byte(" 0")
		if bytes.HasSuffix(line, []byte(",")) {
			replacement = []byte(" 0,")
		}
		zeroed := make([]byte, 0, len(out))
		zeroed = append(zeroed, out[:start+colon+1]...)
		zeroed = append(zeroed, replacement...)
		return append(zeroed, out[end:]...)

	case genesisOpSplice:
		spliced := make([]byte, 0, len(out)+len(extra))
		spliced = append(spliced, out[:at]...)
		spliced = append(spliced, extra...)
		return append(spliced, out[at:]...)
	}
	return out
}

// embeddedNetworks is the shipped set, with the chain id each file must carry.
// TestEmbeddedChilizForkSchedules checks the Chiliz fork implications on the
// same three; this target checks that they parse and stay well-ordered.
func embeddedNetworks() []struct {
	name    string
	chainID int64
	genesis *core.Genesis
} {
	return []struct {
		name    string
		chainID int64
		genesis *core.Genesis
	}{
		{"chiliz", 88888, ChilizMainnetGenesisConfig},
		{"spicy", 88882, SpicyGenesisConfig},
		{"scoville", 88880, ScovilleGenesisConfig},
	}
}

func FuzzGenesisJSON(f *testing.F) {
	// Offsets of interesting keys in the shipped files, resolved here so the
	// seeds name a key rather than a magic number.
	keyPos := func(src []byte, key string) uint32 {
		i := bytes.Index(src, []byte(key))
		if i < 0 {
			panic("seed key " + key + " not found in embedded genesis")
		}
		return uint32(i)
	}

	// The three embedded configs, verbatim.
	f.Add(uint8(1), genesisOpNone, uint32(0), []byte(nil))
	f.Add(uint8(2), genesisOpNone, uint32(0), []byte(nil))
	f.Add(uint8(3), genesisOpNone, uint32(0), []byte(nil))

	// nil versus zero for a Chiliz fork field, on mainnet's snake8Time: the key
	// removed (never) against the key zeroed (active since genesis). Same file,
	// same parser, two different chains.
	snake8 := keyPos(chilizRawGenesisConfig, `"snake8Time"`)
	f.Add(uint8(1), genesisOpDeleteLine, snake8, []byte(nil))
	f.Add(uint8(1), genesisOpZeroValue, snake8, []byte(nil))
	f.Add(uint8(1), genesisOpDuplicateLine, snake8, []byte(nil))
	// Same pair on spicy's dragon8Time, which mainnet does not schedule at all.
	dragon8 := keyPos(spicyRawGenesisConfig, `"dragon8Time"`)
	f.Add(uint8(2), genesisOpDeleteLine, dragon8, []byte(nil))
	f.Add(uint8(2), genesisOpZeroValue, dragon8, []byte(nil))

	// The blob schedule and the forks that require one.
	f.Add(uint8(1), genesisOpDeleteLine, keyPos(chilizRawGenesisConfig, `"blobSchedule"`), []byte(nil))
	f.Add(uint8(1), genesisOpZeroValue, keyPos(chilizRawGenesisConfig, `"cancunTime"`), []byte(nil))
	f.Add(uint8(1), genesisOpDeleteLine, keyPos(chilizRawGenesisConfig, `"baseFeeUpdateFraction"`), []byte(nil))

	// Structural damage: a flipped byte in the chain id, a truncated file, a
	// closing brace spliced into the middle.
	f.Add(uint8(1), genesisOpFlipByte, keyPos(chilizRawGenesisConfig, `"chainId"`)+11, []byte(nil))
	f.Add(uint8(1), genesisOpTruncate, uint32(len(chilizRawGenesisConfig)/2), []byte(nil))
	f.Add(uint8(3), genesisOpSplice, keyPos(scovilleRawGenesisConfig, `"alloc"`), []byte("}}"))

	// Raw inputs, including the smallest genesis both parsers accept and the
	// same one with trailing bytes — the one documented disagreement between
	// Unmarshal (rejects) and Decode (ignores them).
	minimal := []byte(`{"config":{"chainId":88888,"parlia":{"period":3,"epoch":28800}},"gasLimit":"0x2625a00","difficulty":"0x1","alloc":{}}`)
	f.Add(uint8(0), genesisOpNone, uint32(0), minimal)
	f.Add(uint8(0), genesisOpNone, uint32(0), append(append([]byte(nil), minimal...), []byte(" trailing")...))

	// A negative value in each of the three fields that serialise through
	// math.HexOrDecimal256. These parse and then have no re-parsable
	// serialisation — the upstream asymmetry assertGenesisRoundTrips documents,
	// and one inserted byte away from minimal's "0x1" (or from any shipped
	// file's). Seeded so the narrowing there is exercised by a plain
	// `go test ./config/`, rather than only when a campaign rediscovers it and
	// leaves a committed crasher behind.
	f.Add(uint8(0), genesisOpNone, uint32(0), []byte(`{"config":{"chainId":88888,"parlia":{"period":3,"epoch":28800}},"gasLimit":"0x2625a00","difficulty":"0x-1","alloc":{}}`))
	f.Add(uint8(0), genesisOpNone, uint32(0), []byte(`{"config":{"chainId":88888,"parlia":{"period":3,"epoch":28800}},"gasLimit":"0x2625a00","difficulty":"0x1","baseFeePerGas":"0x-1","alloc":{}}`))
	f.Add(uint8(0), genesisOpNone, uint32(0), []byte(`{"config":{"chainId":88888,"parlia":{"period":3,"epoch":28800}},"gasLimit":"0x2625a00","difficulty":"0x1","alloc":{"0000000000000000000000000000000000000001":{"balance":"0x-1"}}}`))

	// The decoder-only acceptance domain, with a fork schedule in it. A complete
	// genesis followed by trailing bytes is accepted by json.Decoder (the
	// --genesis path an operator uses) and rejected by json.Unmarshal (the
	// embedded path), so it is the one input shape that reaches
	// CheckConfigForkOrder through the operator parser alone — the domain the
	// early return below used to skip. chainId 88883 is the staging fleet's,
	// whose per-host genesis.json files are hand-edited and whose trailing
	// comments are exactly how a file ends up in this shape; the schedule is a
	// well-ordered BSC one so the fork-order walk has something to walk.
	staging := []byte(`{"config":{"chainId":88883,"parlia":{"period":3,"epoch":200},` +
		`"mirrorSyncBlock":0,"brunoBlock":0,"gibbsBlock":0,"planckBlock":0,` +
		`"hertzBlock":0,"hertzfixBlock":0,"keplerTime":100,"cancunTime":200,` +
		`"blobSchedule":{"cancun":{"target":3,"max":6,"baseFeeUpdateFraction":3338477}}},` +
		`"gasLimit":"0x2625a00","difficulty":"0x1","alloc":{}}`)
	f.Add(uint8(0), genesisOpNone, uint32(0), append(append([]byte(nil), staging...), []byte("\n# host notes\n")...))

	for _, raw := range []string{"", "{}", "null", "[]", "0", `{"config":null}`, `{"config":{}}`, `{"alloc":{}}`, `{"config":{"chainId":1},"gasLimit":"0x1","difficulty":"0x0","alloc":{}}`} {
		f.Add(uint8(0), genesisOpNone, uint32(0), []byte(raw))
	}

	f.Fuzz(func(t *testing.T, base uint8, op uint8, pos uint32, raw []byte) {
		bases := genesisBases(raw)
		baseIdx := int(base) % len(bases)
		src := genesisMutate(bases[baseIdx], op, pos, raw)

		// An unmutated shipped file is a CONSTANT input, and a ~150 kB one: base 0
		// is the fuzzer's own bytes, 1-3 are the network configs, and a case lands
		// on this shape whenever the operator selector resolves to genesisOpNone
		// or the edit turns out to be a no-op. `op`, `pos` and `raw` can all be
		// mutated without changing the result, so a large share of the corpus the
		// fuzzer derives from the three verbatim seeds stays in it.
		//
		// Everything below property 4 is a pure function of src, so on those
		// inputs it re-derives the same verdict every time — at ~6 ms per parse of
		// a 150 kB document (measured: json.Unmarshal 6.5 ms, Decoder 5.3 ms,
		// against 19 µs for the whole round trip), three parses deep. It is
		// asserted once instead, deterministically and under plain `go test`, by
		// TestEmbeddedGenesisParsesAndRoundTrips.
		unmutatedShipped := baseIdx != 0 && bytes.Equal(src, bases[baseIdx])

		// Property 4, first: the shipped configs, on every iteration. They are
		// parsed once at package init, so this costs three fork-order walks and
		// nothing else — cheap enough to re-assert on every case, and it means
		// a broken network file fails this target and not only its neighbour
		// TestEmbeddedChilizForkSchedules.
		for _, n := range embeddedNetworks() {
			if n.genesis == nil || n.genesis.Config == nil {
				t.Fatalf("embedded %s genesis has no chain config", n.name)
			}
			if n.genesis.Config.ChainID == nil || n.genesis.Config.ChainID.Int64() != n.chainID {
				t.Fatalf("embedded %s genesis has chain id %v, want %d", n.name, n.genesis.Config.ChainID, n.chainID)
			}
			if err := n.genesis.Config.CheckConfigForkOrder(); err != nil {
				t.Fatalf("embedded %s genesis no longer passes CheckConfigForkOrder: %v", n.name, err)
			}
		}

		// The constant inputs stop here — see unmutatedShipped above.
		if unmutatedShipped {
			return
		}

		// Property 1: both parsers, no panic, same verdict on a single JSON
		// value, same result when both accept.
		embedded := new(core.Genesis)
		embeddedErr := json.Unmarshal(src, embedded)

		operator := new(core.Genesis)
		operatorErr := json.NewDecoder(bytes.NewReader(src)).Decode(operator)

		if embeddedErr == nil && operatorErr != nil {
			t.Fatalf("json.Unmarshal accepted the input but json.Decoder rejected it (%v): the embedded loader and the --genesis loader disagree\ninput: %s", operatorErr, genesisExcerpt(src))
		}
		if operatorErr == nil && embeddedErr != nil && json.Valid(src) {
			t.Fatalf("json.Decoder accepted a well-formed JSON document but json.Unmarshal rejected it (%v): the two loaders disagree on more than trailing bytes\ninput: %s", embeddedErr, genesisExcerpt(src))
		}
		if embeddedErr == nil && operatorErr == nil && !reflect.DeepEqual(embedded, operator) {
			t.Fatalf("the two genesis loaders parsed the same bytes into different genesis blocks\ninput: %s", genesisExcerpt(src))
		}

		// CheckConfigForkOrder is what `geth init` runs on an operator file: it
		// must reach a verdict, not panic, whatever the file contains.
		//
		// It runs on BOTH parse results, and above property 2's early return,
		// because the two acceptance domains are not the same one. The operator
		// parser (Decode, the --genesis path) accepts a strict superset: a
		// complete genesis followed by trailing bytes is accepted there and
		// rejected by Unmarshal, so an input in that domain returns below
		// without ever reaching this call. Checking only the embedded result
		// left the operator-only domain — the one an operator file is most
		// likely to land in — out of the target entirely.
		if operatorErr == nil && operator.Config != nil {
			_ = operator.Config.CheckConfigForkOrder()
		}
		if embeddedErr == nil && embedded.Config != nil {
			// Redundant by construction whenever both parsers accept (the two
			// configs are DeepEqual, asserted just above), and kept anyway so
			// that neither parser's coverage of this call depends on the other
			// continuing to agree with it.
			_ = embedded.Config.CheckConfigForkOrder()
		}

		// Property 2: a rejected input never yields a usable config. The
		// production wrapper turns the error into a panic; what must never
		// happen is a return with the half-filled struct encoding/json leaves
		// behind.
		if embeddedErr != nil {
			assertGenesisParsePanics(t, src, embeddedErr)
			return
		}
		if got := mustParseGenesisConfigFromJson(src); !reflect.DeepEqual(got, embedded) {
			t.Fatalf("mustParseGenesisConfigFromJson returned a different genesis than json.Unmarshal for the same bytes\ninput: %s", genesisExcerpt(src))
		}

		assertGenesisRoundTrips(t, src, embedded)
	})
}

// assertGenesisRoundTrips pins property 3 on one parsed genesis. The comparison
// is on re-serialised bytes rather than DeepEqual, because a few fields have two
// equal representations (a nil and an empty ExtraData both write "0x"), and a
// difference that does not survive serialisation is not a difference a node can
// see.
func assertGenesisRoundTrips(t *testing.T, src []byte, parsed *core.Genesis) {
	t.Helper()

	// Precondition: the property only holds for non-negative values, because
	// upstream go-ethereum's math.HexOrDecimal256 parses negatives that it
	// cannot serialise back (common/math/big.go):
	//
	//	parse   "0x-1" -> -1, ok=true   // strips "0x", then SetString("-1", 16)
	//	marshal        -> "-0x1"        // MarshalText writes %#x
	//	reparse "-0x1" -> nil, ok=false // no "0x" prefix, so the base-10 path
	//
	// So a negative value parses, and then there is no genesis file at all that
	// re-reads as it — not a different chain, no chain. The property below asks
	// whether re-serialising preserves the chain; on such an input there is
	// nothing to preserve, and asserting it would only restate the upstream
	// asymmetry. It is NOT a Chiliz regression, and it is one inserted byte away
	// from any shipped file ("0x1" -> "0x-1"), so leaving it to fail would hand
	// the weekly campaign a permanent committed crasher under
	// config/testdata/fuzz/ — the false positive the ratchet is meant to treat
	// as real.
	//
	// Narrowed, not disabled. Excused here are ONLY negative values of the
	// fields that serialise through math.HexOrDecimal256. Everything else still
	// goes through the full round trip, including negatives elsewhere: the
	// *big.Int fork blocks and chain id on ChainConfig marshal as JSON numbers
	// and do round trip negative (asserted by TestGenesisRoundTripNarrowing),
	// and the math.HexOrDecimal64 fields (nonce, gasLimit, excessBlobGas) reject
	// "0x-1" at parse and so never reach this function.
	if negativeHexOrDecimal256Field(parsed) != "" {
		return
	}

	first, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("a parsed genesis failed to marshal: %v\ninput: %s", err, genesisExcerpt(src))
	}
	reparsed := new(core.Genesis)
	if err := json.Unmarshal(first, reparsed); err != nil {
		t.Fatalf("a marshalled genesis no longer parses: %v\nmarshalled: %s", err, genesisExcerpt(first))
	}
	second, err := json.Marshal(reparsed)
	if err != nil {
		t.Fatalf("a re-parsed genesis failed to marshal: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("genesis changed across a marshal/unmarshal round trip\nfirst:  %s\nsecond: %s", genesisExcerpt(first), genesisExcerpt(second))
	}
	if !sameChainConfigJSON(t, parsed.Config, reparsed.Config) {
		t.Fatalf("the chain config changed across a marshal/unmarshal round trip\ninput: %s", genesisExcerpt(src))
	}
}

// negativeHexOrDecimal256Field names the first genesis field that holds a
// negative value AND is serialised through math.HexOrDecimal256, or returns ""
// if there is none. It is the precondition of the round-trip property — see
// assertGenesisRoundTrips for the upstream asymmetry it works around.
//
// The set is Genesis's two *math.HexOrDecimal256 fields (core/genesis.go,
// genesisSpecMarshaling) plus Account.Balance (core/types/gen_account.go). It is
// spelled out rather than derived because the override types are unexported. If
// an upstream merge adds a third such field, this list goes stale and
// FuzzGenesisJSON finds the same asymmetry on the new field — which is the right
// failure: it says to add the field here, never to widen the skip.
func negativeHexOrDecimal256Field(g *core.Genesis) string {
	if g == nil {
		return ""
	}
	if g.Difficulty != nil && g.Difficulty.Sign() < 0 {
		return "difficulty"
	}
	if g.BaseFee != nil && g.BaseFee.Sign() < 0 {
		return "baseFeePerGas"
	}
	for addr, account := range g.Alloc {
		if account.Balance != nil && account.Balance.Sign() < 0 {
			return "alloc[" + addr.Hex() + "].balance"
		}
	}
	return ""
}

// TestGenesisRoundTripNarrowing pins how narrow the skip in
// assertGenesisRoundTrips is, in both directions. Without this, a later reader
// has no way to tell a precondition from a disabled assertion, and widening the
// skip — to any negative *big.Int, say — would silently stop the round trip from
// checking most of what it exists to check.
func TestGenesisRoundTripNarrowing(t *testing.T) {
	const cfg = `"config":{"chainId":88888,%s"parlia":{"period":3,"epoch":28800}}`

	// Excused: the three math.HexOrDecimal256 fields, which parse a negative and
	// then marshal it as "-0x1", which no longer parses.
	for _, tc := range []struct{ name, field, src string }{
		{"difficulty", "difficulty",
			`{` + fmt.Sprintf(cfg, "") + `,"gasLimit":"0x2625a00","difficulty":"0x-1","alloc":{}}`},
		{"baseFeePerGas", "baseFeePerGas",
			`{` + fmt.Sprintf(cfg, "") + `,"gasLimit":"0x2625a00","difficulty":"0x1","baseFeePerGas":"0x-1","alloc":{}}`},
		{"allocBalance", "alloc[0x0000000000000000000000000000000000000001].balance",
			`{` + fmt.Sprintf(cfg, "") + `,"gasLimit":"0x2625a00","difficulty":"0x1","alloc":{"0000000000000000000000000000000000000001":{"balance":"0x-1"}}}`},
	} {
		t.Run("excused/"+tc.name, func(t *testing.T) {
			parsed := new(core.Genesis)
			if err := json.Unmarshal([]byte(tc.src), parsed); err != nil {
				t.Fatalf("the input no longer parses, so it no longer exercises the narrowing: %v", err)
			}
			if got := negativeHexOrDecimal256Field(parsed); got != tc.field {
				t.Fatalf("negativeHexOrDecimal256Field = %q, want %q", got, tc.field)
			}
			// The point of the skip: this must not fail the test.
			assertGenesisRoundTrips(t, []byte(tc.src), parsed)
		})
	}

	// Still asserted: negatives that do NOT go through math.HexOrDecimal256. The
	// ChainConfig fork blocks and chain id are plain *big.Int, marshal as JSON
	// numbers, and round trip negative — so the skip must not fire on them, or
	// the round trip would stop covering the chain config entirely.
	for _, tc := range []struct{ name, src string }{
		{"runtimeUpgradeBlock", `{` + fmt.Sprintf(cfg, `"runtimeUpgradeBlock":-1,`) + `,"gasLimit":"0x2625a00","difficulty":"0x1","alloc":{}}`},
		{"chainId", `{"config":{"chainId":-88888,"parlia":{"period":3,"epoch":28800}},"gasLimit":"0x2625a00","difficulty":"0x1","alloc":{}}`},
	} {
		t.Run("asserted/"+tc.name, func(t *testing.T) {
			parsed := new(core.Genesis)
			if err := json.Unmarshal([]byte(tc.src), parsed); err != nil {
				t.Fatalf("the input no longer parses: %v", err)
			}
			if got := negativeHexOrDecimal256Field(parsed); got != "" {
				t.Fatalf("the narrowing excused %q on an input with no negative HexOrDecimal256 field: it is too wide", got)
			}
			assertGenesisRoundTrips(t, []byte(tc.src), parsed)
		})
	}

	// Never reached: the math.HexOrDecimal64 fields reject "0x-1" outright, so
	// the round trip never sees a negative one and the skip needs no entry for
	// them. If upstream ever made ParseUint64 accept it, this test says so.
	for _, name := range []string{"nonce", "gasLimit", "excessBlobGas"} {
		t.Run("rejected/"+name, func(t *testing.T) {
			src := `{` + fmt.Sprintf(cfg, "") + `,"gasLimit":"0x2625a00","difficulty":"0x1","` + name + `":"0x-1","alloc":{}}`
			if err := json.Unmarshal([]byte(src), new(core.Genesis)); err == nil {
				t.Fatalf("a negative %s now parses; it reaches the round trip, so negativeHexOrDecimal256Field must cover it", name)
			}
		})
	}
}

// TestEmbeddedGenesisParsesAndRoundTrips carries properties 1 to 3 for the three
// shipped network files, which FuzzGenesisJSON returns early on.
//
// The inputs are compile-time constants, so a fuzz campaign re-derived the same
// verdict on them over and over at ~18 ms a go (three parses of a ~150 kB
// document); asserting them here costs that once, is deterministic rather than
// dependent on the corpus reaching a particular case, and — unlike a fuzz seed —
// runs on `go test ./config/` with no campaign. Every mutated input, which is
// what the target is actually hunting, still goes through the full body.
func TestEmbeddedGenesisParsesAndRoundTrips(t *testing.T) {
	for _, n := range []struct {
		name string
		src  []byte
	}{
		{"chiliz", chilizRawGenesisConfig},
		{"spicy", spicyRawGenesisConfig},
		{"scoville", scovilleRawGenesisConfig},
	} {
		t.Run(n.name, func(t *testing.T) {
			// Property 1: both parsers accept it and agree.
			embedded := new(core.Genesis)
			if err := json.Unmarshal(n.src, embedded); err != nil {
				t.Fatalf("the shipped %s genesis no longer parses: %v", n.name, err)
			}
			operator := new(core.Genesis)
			if err := json.NewDecoder(bytes.NewReader(n.src)).Decode(operator); err != nil {
				t.Fatalf("the shipped %s genesis is rejected by the --genesis loader: %v", n.name, err)
			}
			if !reflect.DeepEqual(embedded, operator) {
				t.Fatalf("the two genesis loaders parsed the shipped %s genesis into different genesis blocks", n.name)
			}
			if err := embedded.Config.CheckConfigForkOrder(); err != nil {
				t.Fatalf("the shipped %s genesis does not pass CheckConfigForkOrder: %v", n.name, err)
			}
			// Property 2, in its accepting direction: the production wrapper
			// returns what json.Unmarshal returns.
			if got := mustParseGenesisConfigFromJson(n.src); !reflect.DeepEqual(got, embedded) {
				t.Fatalf("mustParseGenesisConfigFromJson returned a different genesis than json.Unmarshal for the shipped %s genesis", n.name)
			}
			// Property 3. The skip in assertGenesisRoundTrips is a precondition
			// on the input, so on a shipped file it would be silent: assert it
			// cannot apply here, or a negative difficulty/baseFee/balance landing
			// in a network file would turn this assertion into a no-op instead of
			// failing it.
			if field := negativeHexOrDecimal256Field(embedded); field != "" {
				t.Fatalf("the shipped %s genesis has a negative %s, which would skip the round-trip assertion rather than fail it", n.name, field)
			}
			assertGenesisRoundTrips(t, n.src, embedded)
		})
	}
}

// assertGenesisParsePanics pins property 2: the embedded loader converts a
// parse error into a panic rather than handing back the partially populated
// struct encoding/json leaves behind on failure.
func assertGenesisParsePanics(t *testing.T, src []byte, parseErr error) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("mustParseGenesisConfigFromJson returned instead of panicking on input json.Unmarshal rejects (%v)\ninput: %s", parseErr, genesisExcerpt(src))
		}
	}()
	got := mustParseGenesisConfigFromJson(src)
	// Unreachable unless the wrapper stops panicking; naming what it returned
	// makes that failure self-explanatory.
	t.Logf("returned genesis: %+v", got)
}

// sameChainConfigJSON compares two chain configs by their JSON form, which is
// exactly the surface a genesis file carries.
func sameChainConfigJSON(t *testing.T, a, b *params.ChainConfig) bool {
	t.Helper()
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	encA, errA := json.Marshal(a)
	encB, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		t.Fatalf("a parsed chain config failed to marshal: %v / %v", errA, errB)
	}
	return bytes.Equal(encA, encB)
}

// genesisExcerpt keeps failure messages readable: the shipped genesis files are
// ~150 kB, almost all of it the allocation table.
func genesisExcerpt(src []byte) string {
	const limit = 512
	if len(src) <= limit {
		return string(src)
	}
	return string(src[:limit]) + "... (" + strconv.Itoa(len(src)) + " bytes)"
}
