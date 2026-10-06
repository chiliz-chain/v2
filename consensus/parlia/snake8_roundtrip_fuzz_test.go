package parlia

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

// COR-231: property-based coverage of the Snake8 producer/verifier round trip.
//
// Post-Snake8Fix (COR-173) the validator-frequency bytes in every header are
// consensus-verified: the producer computes them from the PARENT block's state,
// embeds them in header.Extra together with the parent timestamp, and every
// importing node recomputes them from its own copy of that state and rejects a
// mismatch. The loop spans four functions:
//
//	compute          (*Parlia).refreshFrequencyRLP
//	embed            (*Parlia).SetExtraData
//	cross-validate   (*Parlia).verifySnake8Extra
//	recompute+compare(*Parlia).verifySnake8FrequencyData
//
// COR-209 (frequency_fuzz_test.go) proved calcFrequencyRLP is deterministic in
// isolation. Determinism is necessary but not sufficient: producer and verifier
// must additionally agree on WHICH parent state is read and WHICH header bytes
// are compared. A break anywhere in that agreement does not corrupt the chain
// quietly — it makes every honest node reject the honest head, i.e. a halt. That
// is the failure mode this target exists to catch, and it is the reason the
// error CLASSIFICATION is pinned as hard as the bytes: a verifier that fails
// closed on a node-local stake-lookup failure rejects the honest head under RPC
// load, and a producer that fails open emits blocks nobody can verify.
//
// WHAT THE BYTE COMPARISON DOES NOT PROVE. The round trip is two-sided about
// WHICH STATE is read — the producer and each verifier are handed different
// "latest" tables, and only the parent-pinned table is shared — but it is a
// tautology about WHAT IS COMPUTED: both sides reach the same production
// function, so any change to the frequency algorithm itself moves both sides
// together and the comparison stays green. That is by design, and it is
// measured, not assumed: mutating `minFrequencyFactor` from 2 to 4
// (snapshot.go, inside calcFrequencyRLP) leaves all of this target's seeds
// passing, while the same mutation fails 5 of FuzzCalcFrequencyRLP's seeds.
// Value-pinning the algorithm is COR-209's job and it does it. So a green run
// here must never be cited as evidence that the frequency algorithm is pinned;
// what it pins is that the loop around the algorithm is closed.
//
// Harness shape follows COR-217 (core/vm/runtime/chiliz_runtime_fuzz_test.go):
// a stub, not a chain. There is no blockchain, no state database and no RPC
// backend — just a two-header chain reader (prepareTestChainReader, reused from
// parlia_prepare_test.go), a snapshot seeded straight into the engine's LRU, and
// a fake stake backend injected through the `stakeReader` seam that
// refreshFrequencyRLP already exposes for exactly this purpose.
//
// The stake backend is what makes parent pinning observable. It serves TWO
// tables: a `pinned` one for reads resolved against the parent's hash and a
// `latest` one for everything else (state == nil, or a state resolved by
// number). Post-fork every engine shares the pinned table but gets its OWN
// latest table, modelling the real world — nodes agree about the parent's state
// and disagree about their own head. So a read that silently drifts to latest
// state shows up twice: directly, because every recorded read is asserted to be
// hash-pinned, and indirectly, because two verifiers then compute different
// bytes.

const (
	// snake8RoundTripBaseTime is the genesis timestamp of the fixture chain. It
	// is deliberately in the far future: blockTimeForRamanujanFork clamps a
	// block time that has already passed to time.Now(), which would make the
	// produced header — and therefore every byte of this test — depend on the
	// wall clock. A fuzz target must be reproducible when the fuzzer re-runs a
	// cached input, so the fixture stays ahead of `now`.
	snake8RoundTripBaseTime = uint64(4_000_000_000)

	// The fixture produces block 100 on top of block 99. Both are non-epoch
	// blocks under the fixture's epoch of 200, which keeps prepareValidators /
	// prepareTurnLength out of the picture: they would need a real contract
	// backend, and the frequency block's position in Extra does not depend on
	// them (snake8FreqDataOffset returns extraVanity for a non-epoch block).
	snake8RoundTripParentNumber = uint64(99)
	snake8RoundTripChildNumber  = uint64(100)
)

// stakeReaderFn mirrors the Parlia.stakeReader seam refreshFrequencyRLP calls.
type stakeReaderFn = func(validator common.Address, blockNumber uint64, state *rpc.BlockNumberOrHash) (*big.Int, error)

// stakeOutcome enumerates the ways a stake read can fail. All of them are
// NODE-LOCAL conditions — they say nothing about the block under judgment — and
// refreshFrequencyRLP is required to classify every one of them as
// errSnake8StakeLookup on the pinned path, because that classification is what
// verifySnake8FrequencyData keys its fail-open branch off.
type stakeOutcome uint8

const (
	// stakeOK: the backend answers.
	stakeOK stakeOutcome = iota
	// stakeClassifiedErr: the backend returns an error that is already wrapped
	// as errSnake8StakeLookup (what a deeper layer would hand up).
	stakeClassifiedErr
	// stakeUnrelatedErr: a plain error with no Chiliz classification — an RPC
	// gas cap, a missing historical trie node. It must still end up classified
	// at the refreshFrequencyRLP boundary, otherwise verifySnake8FrequencyData's
	// `return err` branch fires and a local condition condemns a valid block.
	stakeUnrelatedErr
	// stakeNilNoErr: the contract call succeeds but decodes to no value. The
	// production code treats it as its own failure shape ("contract returned no
	// stake") rather than as a zero stake, because a zero would silently drop
	// the validator out of the frequency set on one node only.
	stakeNilNoErr
	stakeOutcomeCount
)

func (o stakeOutcome) String() string {
	switch o {
	case stakeOK:
		return "ok"
	case stakeClassifiedErr:
		return "pre-classified error"
	case stakeUnrelatedErr:
		return "unrelated error"
	case stakeNilNoErr:
		return "nil stake, no error"
	}
	return fmt.Sprintf("stakeOutcome(%d)", uint8(o))
}

// errRoundTripUnrelated is the "not ours" failure shape: it must never be
// mistaken for a classified stake-lookup failure by anything except the
// classification performed inside refreshFrequencyRLP itself.
var errRoundTripUnrelated = errors.New("round-trip fixture: rpc gas cap exceeded")

// stakeRead is one recorded call into the stake backend. BOTH arguments are
// recorded because production parameterises the read by both, and each of them
// selects a different thing:
//
//   - `state` selects WHICH BLOCK'S STATE the eth_call runs against
//     (refreshFrequencyRLP, parlia.go).
//   - `number` selects WHICH EPOCH is packed into the calldata:
//     getValidatorTotalDelegated computes `epoch := blockNumber /
//     p.chainConfig.Parlia.Epoch` and packs `getValidatorStatusAtEpoch(addr,
//     epoch)`. The contract keeps per-epoch stake records, so a wrong epoch is a
//     wrong answer read from the right state — invisible to a state-reference
//     assertion.
//
// refreshFrequencyRLP passes `number-1`, i.e. the PARENT's height, on both sides
// of Snake8Fix. Getting that off by one is silent on every block except an epoch
// boundary, where `number` and `number-1` fall in different epochs: on mainnet
// (Epoch 28800) one block in 28800 would then recompute from a different epoch's
// stakes, mismatch the embedded bytes and be condemned with
// errMismatchedSnake8FrequencyData — a resync stopping dead at a block the fleet
// already produced (the COR-193 class). So the number is asserted alongside the
// state reference, not discarded.
type stakeRead struct {
	number uint64
	state  *rpc.BlockNumberOrHash
}

// honestStakeReader serves `pinned` for reads resolved against parentHash and
// `latest` for anything else, recording every call it is handed.
//
// The split is the whole point of the fixture: post-Snake8Fix the production
// code must resolve by parent HASH (never by number, never latest — see COR-184
// for what a by-number state read costs on a live chain), so `latest` is a
// tripwire, not a fallback. Pre-fork the opposite holds: the historical bytes on
// mainnet and spicy were produced from unpinned latest state, so the pre-fork
// path must keep reading `latest` and must never be "fixed".
func honestStakeReader(parentHash common.Hash, pinned, latest map[common.Address]*big.Int, record *[]stakeRead) stakeReaderFn {
	return func(validator common.Address, blockNumber uint64, state *rpc.BlockNumberOrHash) (*big.Int, error) {
		if record != nil {
			*record = append(*record, stakeRead{number: blockNumber, state: state})
		}
		table := latest
		if state != nil && state.BlockHash != nil && *state.BlockHash == parentHash {
			table = pinned
		}
		v, ok := table[validator]
		if !ok {
			// Never nil: production seeds missing stakes with zero because
			// calcFrequencyRLP dereferences every entry.
			return new(big.Int), nil
		}
		return new(big.Int).Set(v), nil
	}
}

// describeStateRef renders a state reference for a failure message. rpc's own
// %#v would print the address of the embedded *BlockNumber, which differs run to
// run and would make a recorded failure impossible to compare.
func describeStateRef(state *rpc.BlockNumberOrHash) string {
	switch {
	case state == nil:
		return "latest (nil)"
	case state.BlockHash != nil:
		return fmt.Sprintf("hash %s (requireCanonical=%v)", state.BlockHash.Hex(), state.RequireCanonical)
	case state.BlockNumber != nil:
		return fmt.Sprintf("number %d", int64(*state.BlockNumber))
	}
	return "empty reference"
}

// failingStakeReader fails every read with the given shape.
func failingStakeReader(outcome stakeOutcome) stakeReaderFn {
	return func(validator common.Address, blockNumber uint64, _ *rpc.BlockNumberOrHash) (*big.Int, error) {
		switch outcome {
		case stakeClassifiedErr:
			return nil, fmt.Errorf("%w: fixture failure for %s at %d", errSnake8StakeLookup, validator, blockNumber)
		case stakeNilNoErr:
			return nil, nil
		default:
			return nil, errRoundTripUnrelated
		}
	}
}

// snake8RoundTrip is one fuzz input's fixture: a genesis + parent pair, the
// validator set, the fuzzed Recents, and the fork schedule under which the pair
// is judged. Engines are minted from it on demand, each with its own caches, so
// no two of them can share memoized frequency bytes or a snapshot pointer.
type snake8RoundTrip struct {
	config     *params.ChainConfig
	chain      *prepareTestChainReader
	genesis    *types.Header
	parent     *types.Header
	validators []common.Address
	recents    []byte

	// staleFrequency is the FrequencyRLP newEngine leaves on the SHARED snapshot
	// LRU entry — see newCachedSnapshot for why the entry is seeded hostile.
	staleFrequency []byte

	// A SECOND parent at the same height, differing from `parent` only in its
	// state root and therefore in its hash. It exists so a memoized computation
	// can be asked for a different parent on an engine that has already answered
	// for `parent`: a frequencyCache keyed by anything constant-per-engine is
	// otherwise invisible, because every engine in an iteration sees exactly one
	// parent. Its validator set is disjoint from the real one and it carries no
	// Recents, so its bytes are always non-empty and can never coincide with the
	// real parent's — the property keeps its teeth on every input.
	altParent     *types.Header
	altValidators []common.Address
	altStakes     map[common.Address]*big.Int
}

// newSnake8RoundTripConfig builds the fixture chain config for one of the three
// fork combinations. It layers onto newSnake8FixTestConfig (snake8fix_test.go)
// so the two suites cannot drift apart on anything but Snake8Time itself.
func newSnake8RoundTripConfig(snake8, snake8Fix bool) *params.ChainConfig {
	config := newSnake8FixTestConfig(snake8Fix)
	if !snake8 {
		config.Snake8Time = nil
	}
	return config
}

// staleFrequencyBytes returns a one-entry frequency list that always selects
// `pick`: selectValidatorFromFrequencyRLP walks the candidates accumulating
// frequencies until it passes a target drawn from [0, precision), and a lone
// candidate holding the full precision passes every possible target.
//
// It models what a PRIOR caller left on the shared snapshot LRU entry. That is
// not a contrived state: Snake8 forces a turn length of 50, so the parent's own
// data names the same validator for fifty consecutive blocks, and "the last
// caller's data says this block's coinbase is in turn" is the ordinary content
// of that cache entry. Seeding it is what gives assertDifficultyMatchesEmbedded
// its teeth — with the entry left at FrequencyRLP=nil, a producer that stamps
// difficulty BEFORE refreshing the frequency data merely falls back to
// round-robin, which agrees with the honest answer often enough to hide the
// COR-37 ordering regression.
func staleFrequencyBytes(pick common.Address) []byte {
	type candidateEntry struct {
		Address   common.Address
		Frequency *big.Int
	}
	out, err := rlp.EncodeToBytes([]candidateEntry{{Address: pick, Frequency: big.NewInt(validatorFrequencyPrecision)}})
	if err != nil {
		panic(fmt.Sprintf("round-trip fixture: cannot encode stale frequency data: %v", err))
	}
	return out
}

// newSnake8RoundTrip builds the fixture. `stalePick` is the validator the stale
// frequency data on the shared LRU entry names as in-turn; callers pass the
// block's coinbase, so a producer reading that entry instead of the fresh
// computation stamps in-turn difficulty on every input.
func newSnake8RoundTrip(config *params.ChainConfig, seed uint64, validators []common.Address, recents []byte, stalePick common.Address) *snake8RoundTrip {
	period := config.Parlia.Period
	genesis := &types.Header{
		Number:     common.Big0,
		Time:       snake8RoundTripBaseTime,
		Difficulty: big.NewInt(1),
		Extra:      make([]byte, extraVanity+extraSeal),
	}
	parent := &types.Header{
		Number:     new(big.Int).SetUint64(snake8RoundTripParentNumber),
		ParentHash: genesis.Hash(),
		Time:       snake8RoundTripBaseTime + snake8RoundTripParentNumber*period,
		Difficulty: diffInTurn,
		Coinbase:   validators[0],
		// Fold the fuzz seed into the parent so its hash — the value every
		// post-fork stake read must pin to — varies across inputs. A constant
		// parent hash would make "pinned to the parent" indistinguishable from
		// "pinned to some fixed hash".
		Root:  common.BigToHash(new(big.Int).SetUint64(seed)),
		Extra: make([]byte, extraVanity+extraSeal),
	}
	// The second parent: same height, different state root, therefore a
	// different hash. It is deliberately NOT registered in the chain reader —
	// nothing resolves it through the chain, it is only ever handed to
	// refreshFrequencyRLP directly, exactly as the verifier path does.
	altParent := types.CopyHeader(parent)
	altParent.Root = common.BigToHash(new(big.Int).SetUint64(seed + 1))
	altValidators := deriveFuzzValidators(seed^0xa17e_a17e, len(validators))
	return &snake8RoundTrip{
		config:  config,
		genesis: genesis,
		parent:  parent,
		chain: &prepareTestChainReader{
			config:  config,
			genesis: genesis,
			headers: map[common.Hash]*types.Header{genesis.Hash(): genesis, parent.Hash(): parent},
		},
		validators:     validators,
		recents:        recents,
		staleFrequency: staleFrequencyBytes(stalePick),
		altParent:      altParent,
		altValidators:  altValidators,
		altStakes:      roundTripStakeTable(altValidators, rampStakes(len(altValidators))),
	}
}

// variant re-judges the very same block pair under a different fork schedule.
// The headers (and therefore the parent hash the stake reads pin to) are shared
// on purpose: properties 4 and 6 are about one header being accepted on one side
// of a fork boundary and rejected on the other.
func (fx *snake8RoundTrip) variant(config *params.ChainConfig) *snake8RoundTrip {
	out := *fx
	out.config = config
	out.chain = &prepareTestChainReader{
		config:  config,
		genesis: fx.genesis,
		headers: fx.chain.headers,
	}
	return &out
}

func (fx *snake8RoundTrip) snake8Active() bool {
	return fx.config.IsSnake8(fx.parent.Time)
}

// newEngine mints a Parlia engine over the fixture with the given stake backend.
//
// The engine is assembled as a struct literal rather than through New(). New()
// parses ~120 KB of system-contract ABI JSON on every call and a single fuzz
// iteration needs half a dozen engines; the ABIs are unreachable from the paths
// under test (block 100 is a non-epoch block, so prepareValidators and
// prepareTurnLength return before touching a contract). The fields that ARE
// reachable are all set below — and refreshFrequencyRLP's own nil-guards for
// struct-literal engines are pinned separately by TestSnake8FixNilSeamsDoNotPanic.
func (fx *snake8RoundTrip) newEngine(reader stakeReaderFn) *Parlia {
	engine := &Parlia{
		chainConfig:    fx.config,
		config:         fx.config.Parlia,
		genesisHash:    fx.genesis.Hash(),
		db:             rawdb.NewMemoryDatabase(),
		recentSnaps:    lru.NewCache[common.Hash, *Snapshot](inMemorySnapshots),
		recentHeaders:  lru.NewCache[string, common.Hash](inMemoryHeaders),
		signatures:     lru.NewCache[common.Hash, common.Address](inMemorySignatures),
		frequencyCache: lru.NewCache[common.Hash, []byte](inMemoryFrequencies),
		stakeReader:    reader,
	}
	engine.recentSnaps.Add(fx.parent.Hash(), fx.newCachedSnapshot(engine))
	return engine
}

// newSnapshot builds the CALLER'S view of the parent snapshot — the shape
// Parlia.snapshot() hands back to a Snake8 caller, and therefore the shape the
// producer and every verifier compute from.
func (fx *snake8RoundTrip) newSnapshot(engine *Parlia) *Snapshot {
	return fx.snapshotFor(engine, fx.validators, fx.parent.Hash(), fx.recents)
}

// snapshotFor builds one snapshot. It reuses newFuzzSnapshot (COR-209) for the
// validator set and the Recents stamping, then re-points the fields
// newFuzzSnapshot leaves at their standalone-test defaults.
//
// EpochLength is stamped explicitly rather than inherited from the package-level
// defaultEpochLength: that variable is rewritten by every New() call anywhere in
// the package, so a snapshot that trusted it would make this target's results
// depend on test execution order.
func (fx *snake8RoundTrip) snapshotFor(engine *Parlia, validators []common.Address, hash common.Hash, recents []byte) *Snapshot {
	turnLength := defaultTurnLength
	if fx.snake8Active() {
		turnLength = snake8TurnLength
	}
	snap := newFuzzSnapshot(snake8RoundTripParentNumber, turnLength, validators, validators, recents)
	snap.config = engine.config
	snap.sigCache = engine.signatures
	snap.Hash = hash
	snap.EpochLength = fx.config.Parlia.Epoch
	snap.BlockInterval = defaultBlockInterval
	snap.IsSnake8Fork = fx.snake8Active()
	return snap
}

// newCachedSnapshot builds the entry newEngine seeds into the engine's snapshot
// LRU — the SHARED entry, which is a different object from the caller's view
// above and must stay different.
//
// It is deliberately seeded in a NON-Snake8 shape (IsSnake8Fork=false,
// defaultTurnLength, a stale FrequencyRLP from an unrelated caller). That is a
// state the real LRU reaches: snapshot() caches whatever apply() returned for
// whichever caller missed first, so an entry populated by a non-Snake8 caller —
// a pre-fork verification, an api.go diagnostic — carries exactly this shape,
// and every Snake8 caller afterwards is served it. COR-174 is the rule that
// makes that safe: Parlia.snapshot(…, isSnake8Fork=true, …) returns a caller-
// owned COPY and stamps IsSnake8Fork/TurnLength on it, so the caller computes
// from the right shape and refreshFrequencyRLP's write never reaches the shared
// entry.
//
// Seeding the entry already in Snake8 shape — as this fixture originally did —
// makes copy-and-stamp indistinguishable from use-in-place, and the whole
// COR-174 claim becomes coverage that does not exist: handing the verifier the
// shared entry instead of a copy then leaves the entire package green.
func (fx *snake8RoundTrip) newCachedSnapshot(engine *Parlia) *Snapshot {
	snap := fx.newSnapshot(engine)
	snap.IsSnake8Fork = false
	snap.TurnLength = defaultTurnLength
	snap.FrequencyRLP = bytes.Clone(fx.staleFrequency)
	return snap
}

// describeCachedSnapshot renders the consensus-visible content of an engine's
// shared LRU entry, so a before/after comparison can assert that nothing wrote
// through to it. The unexported plumbing fields are excluded on purpose: they
// are pointers into the engine and say nothing about consensus.
func describeCachedSnapshot(engine *Parlia, hash common.Hash) string {
	snap, ok := engine.recentSnaps.Get(hash)
	if !ok {
		return "<no cached snapshot>"
	}
	return fmt.Sprintf("snake8=%v turnLength=%d epoch=%d interval=%d number=%d hash=%s validators=%d recents=%d frequency=%x",
		snap.IsSnake8Fork, snap.TurnLength, snap.EpochLength, snap.BlockInterval,
		snap.Number, snap.Hash.Hex(), len(snap.Validators), len(snap.Recents), snap.FrequencyRLP)
}

// produce runs the full producer path — Prepare -> prepare -> refreshFrequencyRLP
// -> SetExtraData — for the given coinbase.
func (fx *snake8RoundTrip) produce(engine *Parlia, coinbase common.Address) (*types.Header, error) {
	engine.Authorize(coinbase, nil, nil)
	header := &types.Header{
		Number:     new(big.Int).SetUint64(snake8RoundTripChildNumber),
		ParentHash: fx.parent.Hash(),
	}
	return header, engine.Prepare(fx.chain, header)
}

// recompute runs refreshFrequencyRLP on a throwaway snapshot, i.e. exactly what
// the verifier does inside verifySnake8FrequencyData, and hands back the bytes.
func (fx *snake8RoundTrip) recompute(engine *Parlia) ([]byte, error) {
	snap := fx.newSnapshot(engine)
	if err := engine.refreshFrequencyRLP(snap, snake8RoundTripChildNumber, fx.parent); err != nil {
		return nil, err
	}
	return snap.FrequencyRLP, nil
}

// altRecompute is `recompute` for the fixture's SECOND parent: same engine seam,
// same entry point, a different parent hash and a disjoint candidate set.
func (fx *snake8RoundTrip) altRecompute(engine *Parlia) ([]byte, error) {
	snap := fx.snapshotFor(engine, fx.altValidators, fx.altParent.Hash(), nil)
	if err := engine.refreshFrequencyRLP(snap, snake8RoundTripChildNumber, fx.altParent); err != nil {
		return nil, err
	}
	return snap.FrequencyRLP, nil
}

// withAltParent lets a stake backend answer reads pinned to the SECOND parent
// from fx.altStakes, delegating everything else to `base`.
//
// Those reads only ever happen from altRecompute, so the wrapper is invisible to
// every other property: it does not record, and a read it answers never reaches
// the recording honest reader underneath.
func (fx *snake8RoundTrip) withAltParent(base stakeReaderFn) stakeReaderFn {
	altHash := fx.altParent.Hash()
	return func(validator common.Address, blockNumber uint64, state *rpc.BlockNumberOrHash) (*big.Int, error) {
		if state != nil && state.BlockHash != nil && *state.BlockHash == altHash {
			if v, ok := fx.altStakes[validator]; ok {
				return new(big.Int).Set(v), nil
			}
			return new(big.Int), nil
		}
		return base(validator, blockNumber, state)
	}
}

// assertDifficultyMatchesEmbedded is the COR-37 chain-split tripwire, applied to
// the whole fuzzed space instead of to one hand-built fixture.
//
// Under Snake8 a verifier judges header.Difficulty against the frequency data
// embedded in THAT SAME HEADER (verifyCascadingFields snapshots with
// extraHeader=header), so a producer that stamps difficulty from anything else
// emits blocks that are invalid by construction and forks the chain. That is
// exactly what upstream v1.7.6's Prepare/SetExtraData split did: it moved the
// frequency refresh after the difficulty stamp, leaving prepare() to derive
// difficulty from whatever FrequencyRLP the shared LRU entry happened to carry.
//
// The byte round trip below cannot see that regression — both sides still
// recompute the same bytes, and only the stamped difficulty is wrong — so the
// difficulty is asserted here, from the verifier's side: re-derive it from the
// header's own embedded data and require the producer to have stamped the same.
func (fx *snake8RoundTrip) assertDifficultyMatchesEmbedded(t *testing.T, engine *Parlia, header *types.Header) {
	t.Helper()
	judge := fx.newSnapshot(engine)
	if fx.snake8Active() {
		embedded, err := parseValidatorFrequencies(header, fx.config)
		if err != nil {
			t.Fatalf("honest header carries unparseable Snake8 extra data: %v (extra %x)", err, header.Extra)
		}
		judge.FrequencyRLP = embedded
	}
	want := diffNoTurn
	if judge.inturn(header.Coinbase) {
		want = diffInTurn
	}
	if header.Difficulty == nil || header.Difficulty.Cmp(want) != 0 {
		t.Fatalf("producer/verifier difficulty mismatch: prepare stamped %v, but the header's OWN embedded frequency data selects %s and therefore implies %v for coinbase %s — difficulty was derived from something other than the embedded data (COR-37 Snake8 ordering regression). extra %x",
			header.Difficulty, judge.inturnValidator().Hex(), want, header.Coinbase.Hex(), header.Extra)
	}
}

// tamperFrequencyBytes returns a copy of header whose embedded frequency bytes
// differ in exactly one position. When the honest header embeds an empty tail
// (the deterministic zero/dust degradation) there is no byte to flip, so a byte
// is inserted just before the seal instead — the verifier must reject the
// forged tail just as it rejects a flipped one.
func tamperFrequencyBytes(header *types.Header, config *params.ChainConfig, pos int, delta byte) (*types.Header, error) {
	cp := types.CopyHeader(header)
	embedded, err := parseValidatorFrequencies(cp, config)
	if err != nil {
		return nil, err
	}
	if len(embedded) == 0 {
		body := cp.Extra[:len(cp.Extra)-extraSeal]
		seal := cp.Extra[len(cp.Extra)-extraSeal:]
		grown := make([]byte, 0, len(cp.Extra)+1)
		grown = append(grown, body...)
		grown = append(grown, 0x80|delta)
		grown = append(grown, seal...)
		cp.Extra = grown
		return cp, nil
	}
	// embedded aliases cp.Extra, so this writes through to the header.
	embedded[pos%len(embedded)] ^= delta
	return cp, nil
}

// tamperFrequencyPrefix returns a copy of header whose "VFQ" marker is corrupted,
// i.e. a header that structurally does not carry a frequency block where a
// post-Snake8 header must. It exists to separate the two rejection regimes: a
// malformed header is a statement ABOUT THE BLOCK and must be rejected, whereas
// an unreadable local stake table is a statement about the node and must not be.
// A fail-open branch that widened to cover parse failures would let a header
// with no frequency data at all through Finalize.
func tamperFrequencyPrefix(header *types.Header, config *params.ChainConfig, delta byte) (*types.Header, error) {
	cp := types.CopyHeader(header)
	start, ok := snake8FreqDataOffset(cp, config)
	if !ok {
		return nil, errors.New("no snake8 frequency block to tamper with")
	}
	cp.Extra[start] ^= delta
	return cp, nil
}

// tamperParentTimestamp returns a copy of header whose embedded parent timestamp
// differs in exactly one byte. Any single-byte change to a little-endian uint64
// changes its value, so the forged timestamp can never accidentally equal the
// real parent's.
func tamperParentTimestamp(header *types.Header, config *params.ChainConfig, pos int, delta byte) (*types.Header, error) {
	cp := types.CopyHeader(header)
	start, ok := snake8FreqDataOffset(cp, config)
	if !ok {
		return nil, errors.New("no snake8 frequency block to tamper with")
	}
	tsStart := start + len(validatorFrequencyDataPrefix)
	cp.Extra[tsStart+pos%8] ^= delta
	return cp, nil
}

// roundTripStakeTable maps the fuzzed stake list onto the validator set.
func roundTripStakeTable(validators []common.Address, stakes []*big.Int) map[common.Address]*big.Int {
	out := make(map[common.Address]*big.Int, len(validators))
	for i, addr := range validators {
		out[addr] = stakes[i]
	}
	return out
}

// rampStakes is the stake list decoy flavour 2 hands out: validator i holds
// (n-i) whole CHZ. It is factored out as a list so a seed can pin the PARENT
// stakes to exactly the same distribution — see the pre-fork accept-branch seed
// in addSnake8RoundTripSeeds.
func rampStakes(n int) []*big.Int {
	out := make([]*big.Int, n)
	for i := range out {
		out[i] = new(big.Int).Mul(big.NewInt(int64(n-i)), big.NewInt(1e18))
	}
	return out
}

// decoyStakeTable builds a "latest state" view that is a deliberate distortion
// of the parent view. Three different decoys are in play per iteration (see
// FuzzSnake8ProducerVerifierRoundTrip) so that a post-fork read which drifts to
// latest state makes the engines that hold them disagree.
//
//   - flavour 0 zeroes every stake, so calcFrequencyRLP errors and the bytes
//     degrade to empty;
//   - flavour 1 hands the first validator everything, leaving a one-entry list;
//   - flavour 2 is the descending ramp above. Unlike the other two it leaves
//     EVERY validator eligible, so a computation that consumes it yields a full,
//     non-degenerate frequency list.
//
// Flavour 2 is what the producer (and the standalone SetExtraData engine, which
// must land on identical bytes) holds as its latest view. That matters only
// pre-Snake8Fix, where refreshFrequencyRLP reads latest by design: with a
// degenerate decoy there the producer embedded an EMPTY tail on every pre-fork
// input, which silently emptied out three things at once — fuzzed stakes never
// reached the pre-fork producer, tamperFrequencyBytes could only ever take its
// insert-a-byte branch, and the accept half of property 4's post-fork pairing
// was unreachable from the seed corpus (i.e. from CI, which runs seeds only).
//
// Producer, verifier and second verifier hold three DIFFERENT flavours on
// purpose: each pairwise comparison in the target is then a real disagreement
// test, and giving the producer the verifier's flavour would have made the
// property-1 byte comparison vacuous under a drift-to-latest regression.
func decoyStakeTable(validators []common.Address, flavour int) map[common.Address]*big.Int {
	out := make(map[common.Address]*big.Int, len(validators))
	if flavour == 2 {
		for i, v := range rampStakes(len(validators)) {
			out[validators[i]] = v
		}
		return out
	}
	for i, addr := range validators {
		switch {
		case flavour == 0:
			out[addr] = new(big.Int)
		case i == 0:
			out[addr] = new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(1e18))
		default:
			out[addr] = new(big.Int)
		}
	}
	return out
}

func addSnake8RoundTripSeeds(f *testing.F) {
	plain := encodeFuzzStakes(seedStakes(400, 300, 50))
	// Recents dense enough to matter at the Snake8 turn length of 50: 64 entries
	// all resolving to the same validator push seenTimes past TurnLength, so
	// SignRecently excludes it from the candidate set on BOTH sides. It is a
	// consensus input to the frequency bytes, so producer and verifier have to
	// derive it identically from the snapshot they each build.
	heavyRecents := bytes.Repeat([]byte{0}, fuzzMaxRecents)

	// --- one seed per fork combination (isolating property 6 / property 4) ---
	// Snake8 off: the header must carry no frequency block at all, and the
	// Snake8-on sibling config must reject it for that. Nothing else in the
	// target is exercised on this path, so a regression that makes a pre-fork
	// producer emit a VFQ block shows up here and only here.
	f.Add(uint64(1), uint8(2), plain, []byte{}, uint8(0), uint8(0), uint8(0), uint32(0), uint8(0xff))
	// Snake8 on, Fix off: the historical, unpinned-latest-state path. Isolates
	// property 4 — the verifier accepts bytes it would reject post-fork — and
	// the "pre-fork reads stay unpinned" half of the pinning property.
	f.Add(uint64(2), uint8(2), plain, []byte{}, uint8(1), uint8(0), uint8(1), uint32(0), uint8(0xff))
	// Both on: the full round trip.
	f.Add(uint64(3), uint8(2), plain, []byte{}, uint8(2), uint8(0), uint8(2), uint32(0), uint8(0xff))

	// --- one seed per stake-lookup outcome (isolating property 3) ---
	// Each drives the SAME asymmetry — verifier fails open, producer refuses to
	// seal — through a different failure shape, so a classification that stops
	// covering one shape (e.g. a refactor that returns the raw error for the
	// nil-stake case) fails on that seed alone.
	f.Add(uint64(4), uint8(2), plain, []byte{}, uint8(2), uint8(stakeClassifiedErr), uint8(0), uint32(1), uint8(0x01))
	f.Add(uint64(5), uint8(2), plain, []byte{}, uint8(2), uint8(stakeUnrelatedErr), uint8(1), uint32(2), uint8(0x7f))
	f.Add(uint64(6), uint8(2), plain, []byte{}, uint8(2), uint8(stakeNilNoErr), uint8(2), uint32(3), uint8(0x10))
	// The same three shapes pre-fork, where they must degrade softly instead:
	// a pre-Snake8Fix sealer that cannot read stakes still produces a block.
	// All three are seeded rather than left to the fuzzer because the assertion
	// they drive is the one that would catch the errSnake8StakeLookup
	// classification being hoisted out of refreshFrequencyRLP's `if pinned`
	// block — that refactor would make a PRE-fork sealer refuse to seal — and
	// plain `go test` (i.e. CI) runs the seed corpus only. Note `blindOutcome`
	// remaps stakeOK to stakeUnrelatedErr, so a seed must name a failing shape
	// explicitly to reach its own.
	f.Add(uint64(7), uint8(2), plain, []byte{}, uint8(1), uint8(stakeUnrelatedErr), uint8(0), uint32(0), uint8(0x21))
	f.Add(uint64(14), uint8(2), plain, []byte{}, uint8(1), uint8(stakeClassifiedErr), uint8(1), uint32(4), uint8(0x33))
	f.Add(uint64(15), uint8(2), plain, []byte{}, uint8(1), uint8(stakeNilNoErr), uint8(2), uint32(6), uint8(0x44))

	// --- every stake is dust: calcFrequencyRLP errors, the producer embeds an
	// empty tail, and the verifier must recompute the same emptiness. This is
	// the one soft failure left post-fork; if it ever stopped being symmetric
	// the chain would halt on the first block with no eligible candidates.
	dust := []*big.Int{big.NewInt(1), big.NewInt(5), big.NewInt(999_999_999_999_999_999)}
	f.Add(uint64(8), uint8(2), encodeFuzzStakes(dust), []byte{}, uint8(2), uint8(0), uint8(0), uint32(0), uint8(0x01))
	// All stakes zero: no eligible candidates at all, same degradation by a
	// different route (zero raw stake, not zero normalized stake).
	f.Add(uint64(9), uint8(2), encodeFuzzStakes(seedStakes(0, 0, 0)), []byte{}, uint8(2), uint8(0), uint8(1), uint32(0), uint8(0x02))

	// --- a single-validator set. Its frequency list has exactly one entry
	// whose frequency is the full precision regardless of the stake, so the
	// EMBEDDED BYTES cannot distinguish a pinned read from a latest read here;
	// only the per-read assertion that every state is hash-pinned can. That is
	// precisely why the assertion exists alongside the byte comparison.
	f.Add(uint64(10), uint8(0), encodeFuzzStakes(seedStakes(1000)), []byte{}, uint8(2), uint8(0), uint8(0), uint32(0), uint8(0x40))

	// --- 21 validators with equal stakes plus the heavy Recents, at the
	// production turn length: the widest candidate set the chain runs, with the
	// SignRecently exclusion actually firing.
	f.Add(uint64(11), uint8(20), encodeFuzzStakes(seedStakes(equalStakes(21, 1000)...)), heavyRecents, uint8(2), uint8(0), uint8(5), uint32(7), uint8(0x55))
	// --- one dominant holder (99.9%) among 21: the normalization's extreme.
	f.Add(uint64(12), uint8(20), encodeFuzzStakes(seedStakes(dominantStakes(21)...)), []byte{}, uint8(2), uint8(0), uint8(9), uint32(13), uint8(0x0f))
	// --- dust beside a whale, pre-fork, to pair the degradation with property 4.
	f.Add(uint64(13), uint8(3), encodeFuzzStakes([]*big.Int{big.NewInt(1e18), big.NewInt(1), big.NewInt(1e17), big.NewInt(3e18)}), []byte{1, 2}, uint8(1), uint8(0), uint8(3), uint32(5), uint8(0x80))

	// --- pre-fork, with the PARENT stakes pinned to exactly the ramp the
	// producer's latest view serves. Every other pre-fork seed distorts latest
	// away from the parent, so the post-fork re-judgment in property 4 always
	// lands in the REJECT branch; this one makes the producer's (latest-derived)
	// bytes equal the post-fork recomputation's (parent-derived) bytes, which is
	// the only way the ACCEPT branch of that pairing executes. Without it the
	// accept branch is dead in the seed corpus — dead in CI, and dead in the
	// mutation evidence anyone gathers by running plain `go test`.
	f.Add(uint64(16), uint8(2), encodeFuzzStakes(rampStakes(3)), []byte{}, uint8(1), uint8(0), uint8(0), uint32(2), uint8(0x09))
}

// FuzzSnake8ProducerVerifierRoundTrip pins the six properties that make the
// Snake8Fix frequency-data loop closed:
//
//  1. Round trip. The bytes an honest producer embeds are exactly the bytes an
//     independent verifier recomputes, and verifySnake8FrequencyData returns nil
//     for every honestly produced header. verifySnake8Extra accepts it too — the
//     embedded parent timestamp must be the PARENT's, not the block's own. The
//     recomputation reads a caller-owned COPY of the snapshot: the shared LRU
//     entry it derives from comes out unchanged (COR-174).
//  2. Tamper detection. Changing any byte of the embedded parent timestamp is
//     errMismatchedSnake8ParentTime (from Snake8 on, since verifySnake8Extra is
//     gated on Snake8 rather than Snake8Fix), and post-Snake8Fix changing any
//     byte of the embedded frequency data is errMismatchedSnake8FrequencyData.
//     Corrupting the frequency block's structure is rejected too, and — unlike a
//     stake-lookup failure — it is rejected even by a node that cannot read
//     stakes at all: fail-open is scoped to the lookup, not to the header.
//  3. Fail-open is precisely scoped. A stake-lookup failure on the VERIFIER
//     yields nil (COR-173: an error out of Finalize is a bad-block verdict, so a
//     node-local condition must never condemn the honest head) while the same
//     failure on the PRODUCER is fatal and it refuses to seal. Both halves are
//     asserted; asserting only the happy path would let either half invert
//     unnoticed, and each inversion is its own outage.
//  4. Pre-fork acceptance is never tightened. With Snake8Fix inactive the
//     verifier accepts bytes that the very same verifier rejects once the fork
//     is on, because historical mainnet/spicy blocks were produced from unpinned
//     latest state and must keep replaying.
//  5. Parent pinning. Post-fork the computed bytes are a pure function of the
//     parent, so two engines with different "latest" state and the same parent
//     agree — which is what makes memoizing them by parent hash in
//     frequencyCache sound. The memo is then held to that key: an engine already
//     warmed on one parent must recompute for a second one, not serve the first
//     one's bytes.
//  6. verifySnake8Extra rejects a pre-fork header carrying a frequency block and
//     a post-fork header lacking one. Covered here by a single assertion in each
//     direction; COR-232's FuzzVerifySnake8ExtraPair owns this property
//     standalone and explores it far more deeply.
//  7. Producer coherence (COR-37). The difficulty the producer stamps agrees
//     with the frequency data embedded in that same header. This is a separate
//     claim from the round trip, and the byte comparison is blind to it: both
//     sides recompute the same bytes whatever order the stamping happens in, so
//     the v1.7.6 Prepare/SetExtraData split that forked a devnet passes a pure
//     round-trip check untouched. One hand-built instance of this is pinned by
//     TestPrepareDifficultyMatchesEmbeddedFrequencyData (3 validators, no
//     Recents, no stakes); the assertion here widens it to the fuzzed space.
func FuzzSnake8ProducerVerifierRoundTrip(f *testing.F) {
	addSnake8RoundTripSeeds(f)

	f.Fuzz(func(t *testing.T,
		seed uint64,
		nRaw uint8,
		stakeBytes []byte,
		recentBytes []byte,
		forkSel uint8,
		lookupSel uint8,
		coinbaseSel uint8,
		tamperPos uint32,
		tamperDelta uint8,
	) {
		n := 1 + int(nRaw)%fuzzMaxValidators
		validators := deriveFuzzValidators(seed, n)
		parentStakes := roundTripStakeTable(validators, decodeFuzzStakes(stakeBytes, n))
		coinbase := validators[int(coinbaseSel)%n]
		outcome := stakeOutcome(lookupSel % uint8(stakeOutcomeCount))
		// The producer whose header everything else is judged against is always
		// honest, so `outcome` only ever drives the deliberately blind engines —
		// and those need a failure. stakeOK is remapped rather than skipped so
		// that EVERY input exercises the fail-open/hard-fail asymmetry: an
		// inverted branch there is an outage, and leaving its coverage to the
		// fraction of inputs that happen to pick a failing shape would be a hole
		// in the seed corpus (plain `go test` runs seeds only).
		blindOutcome := outcome
		if blindOutcome == stakeOK {
			blindOutcome = stakeUnrelatedErr
		}
		if tamperDelta == 0 {
			tamperDelta = 1 // a zero XOR delta would tamper with nothing
		}
		// Masked to 31 bits, not just converted: on a 32-bit GOARCH `int` is
		// 32 bits wide, so a tamperPos >= 2^31 would convert to a NEGATIVE int
		// and `pos % len(embedded)` would index backwards — a crasher belonging
		// to the fixture, not to the code under test.
		pos := int(tamperPos & 0x7fff_ffff)

		snake8 := forkSel%3 != 0
		snake8Fix := forkSel%3 == 2

		fx := newSnake8RoundTrip(newSnake8RoundTripConfig(snake8, snake8Fix), seed, validators, recentBytes, coinbase)
		parentHash := fx.parent.Hash()

		// The producer is honest: it reads the real parent state. Its "latest"
		// view is the ramp decoy — a tripwire no honest POST-fork computation may
		// ever consult, and the (non-degenerate) source of the pre-fork bytes,
		// since pre-fork refreshFrequencyRLP reads latest by design. The
		// alt-parent wrapper is inert during production and only answers the
		// second-parent reads property 5 drives afterwards.
		var producerReads []stakeRead
		producer := fx.newEngine(fx.withAltParent(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 2), &producerReads)))
		header, err := fx.produce(producer, coinbase)
		if err != nil {
			t.Fatalf("honest producer failed to prepare a block (fork snake8=%v fix=%v, %d validators): %v", snake8, snake8Fix, n, err)
		}
		// Property 7, on both sides of Snake8: the stamped difficulty must agree
		// with the header's own embedded data. Pre-Snake8 there is nothing
		// embedded and the selection is round-robin, which is still a claim the
		// producer makes and a verifier checks.
		fx.assertDifficultyMatchesEmbedded(t, producer, header)

		// ---- fork off: nothing is embedded, and the post-fork rule says so ----
		if !snake8 {
			if _, ok := extractSnake8ParentTimestamp(header, fx.config); ok {
				t.Fatalf("pre-Snake8 producer embedded a frequency block: extra %x", header.Extra)
			}
			if err := producer.verifySnake8Extra(header, fx.parent); err != nil {
				t.Fatalf("pre-Snake8 header rejected by verifySnake8Extra: %v", err)
			}
			// A GATE tripwire, and nothing more. verifySnake8FrequencyData's
			// first statement is `if !IsSnake8Fix(parent.Time) { return nil }`,
			// so on this path it returns before parsing, before snapshotting and
			// before any stake read. What is pinned here is that the gate is
			// still there and still keyed on the parent's timestamp — a check
			// that grew an unconditional parse or recomputation would condemn
			// every historical pre-fork block on replay. Nothing about the
			// pre-fork verification path is EVALUATED by this call, because
			// there is no pre-fork verification path.
			if err := producer.verifySnake8FrequencyData(fx.chain, header, fx.parent); err != nil {
				t.Fatalf("pre-Snake8 header must not be checked against a recomputation: %v", err)
			}
			// Property 6, second half: the same header judged with Snake8 active
			// is missing a mandatory section. (COR-232 owns this standalone.)
			onFx := fx.variant(newSnake8RoundTripConfig(true, false))
			onEngine := onFx.newEngine(honestStakeReader(parentHash, parentStakes, nil, nil))
			if err := onEngine.verifySnake8Extra(header, onFx.parent); !errors.Is(err, errInvalidSnake8Extra) {
				t.Fatalf("a Snake8-active verifier accepted a header with no frequency block: %v", err)
			}
			return
		}

		// ---- Snake8 active from here on ----

		// Property 1 (cross-validation half) and, transitively, the one thing
		// SetExtraData can get wrong without changing a single frequency byte:
		// embedding header.Time instead of parent.Time.
		if err := producer.verifySnake8Extra(header, fx.parent); err != nil {
			t.Fatalf("verifySnake8Extra rejected an honest Snake8 header: %v", err)
		}
		embedded, err := parseValidatorFrequencies(header, fx.config)
		if err != nil {
			t.Fatalf("honest header carries unparseable Snake8 extra data: %v (extra %x)", err, header.Extra)
		}
		// SetExtraData is also reached STANDALONE, from the miner's MEV path, and
		// its own doc comment claims the recomputation it performs there yields
		// "exactly the bytes prepare() derived the difficulty from". This asserts
		// the first half of that claim — the two paths agree on every byte of
		// Extra, not just the frequency tail, since the vanity padding and fork
		// hash are rebuilt too. The second half, that the difficulty prepare()
		// stamped really was derived from those bytes, is asserted separately by
		// assertDifficultyMatchesEmbedded above; this comparison cannot see it,
		// because both paths recompute the same bytes whatever order the stamping
		// happens in.
		solo := &types.Header{
			Number:     new(big.Int).SetUint64(snake8RoundTripChildNumber),
			ParentHash: parentHash,
			Time:       header.Time,
		}
		// Same latest flavour as the producer: pre-fork both read latest, and the
		// assertion below is that the two paths agree on every byte, not that
		// they happen to degenerate to the same empty tail.
		soloEngine := fx.newEngine(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 2), nil))
		if err := soloEngine.SetExtraData(fx.chain, solo); err != nil {
			t.Fatalf("standalone SetExtraData failed on the fixture parent: %v", err)
		}
		if !bytes.Equal(solo.Extra, header.Extra) {
			t.Fatalf("standalone SetExtraData disagrees with the producer path:\n prepare %x\n solo    %x", header.Extra, solo.Extra)
		}

		// Property 6, first half: with Snake8 off, that block has no business
		// being there. (COR-232 owns this standalone.)
		offFx := fx.variant(newSnake8RoundTripConfig(false, false))
		offEngine := offFx.newEngine(honestStakeReader(parentHash, parentStakes, nil, nil))
		if err := offEngine.verifySnake8Extra(header, offFx.parent); !errors.Is(err, errInvalidSnake8Extra) {
			t.Fatalf("a pre-Snake8 verifier accepted a header carrying a frequency block: %v", err)
		}

		// Which state the producer read is itself consensus-relevant, in both
		// directions. Post-fork every read must be pinned to the parent hash
		// (COR-173); pre-fork every read must stay unpinned, because that is how
		// the historical bytes on mainnet and spicy were produced and a
		// retroactive "fix" would make them unreproducible.
		if len(producerReads) == 0 {
			t.Fatal("the producer never consulted the stake backend for a Snake8 block")
		}
		for i, read := range producerReads {
			state := read.state
			// The epoch argument, on BOTH sides of Snake8Fix. See stakeRead: the
			// block number is what getValidatorTotalDelegated turns into the
			// `epoch` it packs into getValidatorStatusAtEpoch, so a read pinned
			// to the right state can still ask for the wrong epoch's stakes.
			// refreshFrequencyRLP must ask for the parent's height; asking for
			// the block's own height reads a different epoch at every epoch
			// boundary and makes those blocks unreproducible.
			if read.number != snake8RoundTripChildNumber-1 {
				t.Fatalf("stake read %d was parameterised by block %d, want the parent's height %d (that number selects the epoch packed into getValidatorStatusAtEpoch)", i, read.number, snake8RoundTripChildNumber-1)
			}
			if snake8Fix {
				if state == nil || state.BlockHash == nil || *state.BlockHash != parentHash {
					t.Fatalf("post-Snake8Fix stake read %d resolved against %s, want the parent hash %s", i, describeStateRef(state), parentHash.Hex())
				}
				// RequireCanonical is part of the reference, not decoration. The
				// fixture's backend keys its pinned table off the hash alone, so
				// flipping the flag would leave every byte in this target
				// unchanged — but eth/api_backend.go's
				// StateAndHeaderByNumberOrHash answers a RequireCanonical read
				// with "hash is not currently canonical" whenever the parent is
				// not on the node's current canonical chain. That is exactly the
				// situation in which the read must still succeed: a producer
				// building on a head that is about to be reorged, and any node
				// re-executing a side branch. Pinning must select the parent's
				// state, not assert the parent's canonicality.
				if state.RequireCanonical {
					t.Fatalf("post-Snake8Fix stake read %d pinned the parent with RequireCanonical=true (%s); the parent's state must resolve off the canonical chain too", i, describeStateRef(state))
				}
			} else if state != nil {
				t.Fatalf("pre-Snake8Fix stake read %d resolved against %s, want latest state (historical bytes were produced unpinned)", i, describeStateRef(state))
			}
		}

		// Property 2 (parent-timestamp half) holds on both sides of Snake8Fix:
		// verifySnake8Extra is gated on Snake8, not on Snake8Fix.
		tsTampered, err := tamperParentTimestamp(header, fx.config, pos, tamperDelta)
		if err != nil {
			t.Fatalf("could not tamper with the embedded parent timestamp: %v", err)
		}
		noPanic(t, "verifySnake8Extra(ts-tampered)", func() {
			if err := producer.verifySnake8Extra(tsTampered, fx.parent); !errors.Is(err, errMismatchedSnake8ParentTime) {
				t.Fatalf("forged parent timestamp accepted: got %v, want errMismatchedSnake8ParentTime (extra %x)", err, tsTampered.Extra)
			}
		})

		if !snake8Fix {
			// ---- Property 4: pre-fork acceptance is never tightened. ----
			//
			// The producer above read unpinned latest state (the ramp decoy), so
			// its bytes are generally NOT what a post-fork recomputation from the
			// parent yields. A Snake8Fix-inactive verifier must accept them
			// anyway — whatever stake view it holds.
			//
			// Like the pre-Snake8 call further up, this is a GATE tripwire: the
			// function returns nil on its first line, so the decoy table it is
			// handed is never consulted and no recomputation happens. It is the
			// gate that is pinned, and that is the thing worth pinning, because
			// the whole of property 4 is "this gate is never tightened
			// retroactively". The teeth of the property are in the post-fork
			// re-judgment below, which does run the full path.
			preVerifier := fx.newEngine(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 1), nil))
			if err := preVerifier.verifySnake8FrequencyData(fx.chain, header, fx.parent); err != nil {
				t.Fatalf("pre-Snake8Fix verifier rejected a historical header: %v", err)
			}
			// A pre-fork sealer that cannot read stakes still produces a block:
			// the whole pre-fork path is soft, degrading to zero stakes and
			// round-robin selection rather than refusing to seal.
			softProducer := fx.newEngine(failingStakeReader(blindOutcome))
			if _, err := fx.produce(softProducer, coinbase); err != nil {
				t.Fatalf("pre-Snake8Fix sealer must degrade, not refuse, on a %s stake read: %v", blindOutcome, err)
			}

			// The pairing that gives this property its teeth: the SAME bytes,
			// judged under Snake8Fix, are accepted exactly when they match the
			// parent-state recomputation and rejected otherwise. Without this
			// half, "the verifier accepted it" would also be satisfied by a
			// verifier that accepts everything.
			fixFx := fx.variant(newSnake8RoundTripConfig(true, true))
			fixVerifier := fixFx.newEngine(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 1), nil))
			want, err := fixFx.recompute(fixFx.newEngine(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 1), nil)))
			if err != nil {
				t.Fatalf("post-fork recomputation of the pre-fork header failed: %v", err)
			}
			err = fixVerifier.verifySnake8FrequencyData(fixFx.chain, header, fixFx.parent)
			if bytes.Equal(embedded, want) {
				if err != nil {
					t.Fatalf("post-fork verifier rejected bytes equal to its own recomputation: %v", err)
				}
			} else if !errors.Is(err, errMismatchedSnake8FrequencyData) {
				t.Fatalf("post-fork verifier accepted pre-fork bytes %x that do not match its recomputation %x: %v", embedded, want, err)
			}
			return
		}

		// ---- Snake8Fix active: the consensus-verified round trip. ----

		// Property 1: a verifier holding the same parent state and a DIFFERENT
		// latest state recomputes byte-for-byte what the producer embedded.
		verifier := fx.newEngine(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 1), nil))
		recomputed, err := fx.recompute(verifier)
		if err != nil {
			t.Fatalf("verifier recomputation failed on an honest header: %v", err)
		}
		if !bytes.Equal(embedded, recomputed) {
			t.Fatalf("producer/verifier round trip broken:\n embedded   %x\n recomputed %x\n(%d validators, coinbase %s)", embedded, recomputed, n, coinbase.Hex())
		}
		// The same acceptance, but through the FULL verifier entry point on an
		// engine that has not already computed anything for this parent.
		//
		// A fresh engine is load-bearing, not hygiene. refreshFrequencyRLP
		// memoizes post-fork bytes in frequencyCache keyed by parent.Hash(), so
		// calling this on `verifier` — which fx.recompute above just warmed —
		// returns straight from the cache without a single stake read, and the
		// assertion degenerates into comparing `embedded` against the value the
		// line above already proved equal to it. What that hides is everything
		// verifySnake8FrequencyData does that fx.recompute does not: its own
		// p.snapshot(chain, number-1, header.ParentHash, nil, true, nil)
		// derivation of the candidate set — validator set, Recents, TurnLength,
		// and the COR-174 isSnake8Fork=true copy semantics. Producer and
		// verifier disagreeing about the candidate set is precisely the chain
		// halt this target exists to catch, and a cached call cannot see it.
		// The other verifySnake8FrequencyData assertions below already use fresh
		// engines for the same reason.
		freshVerifier := fx.newEngine(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 1), nil))
		// COR-174, made observable: newCachedSnapshot seeds the shared LRU entry
		// in a non-Snake8 shape with a stale FrequencyRLP, so the copy-and-stamp
		// Parlia.snapshot() performs for a Snake8 caller is distinguishable from
		// use-in-place. Handing the verifier the shared entry instead of a copy
		// would (a) recompute from defaultTurnLength rather than Snake8's 50,
		// changing which validators SignRecently excludes, and (b) write the
		// recomputed bytes into consensus state that the miner and RPC handlers
		// read concurrently, off a candidate header that has not been validated
		// yet. The entry must come out of the call byte for byte as it went in.
		cachedBefore := describeCachedSnapshot(freshVerifier, parentHash)
		if err := freshVerifier.verifySnake8FrequencyData(fx.chain, header, fx.parent); err != nil {
			t.Fatalf("verifySnake8FrequencyData rejected an honestly produced header: %v", err)
		}
		if cachedAfter := describeCachedSnapshot(freshVerifier, parentHash); cachedAfter != cachedBefore {
			t.Fatalf("verification wrote through to the SHARED snapshot LRU entry (COR-174):\n before %s\n after  %s", cachedBefore, cachedAfter)
		}

		// Property 5: the bytes are a pure function of the parent, so a second
		// verifier whose latest state differs again lands on the same bytes.
		// This is the soundness argument for memoizing by parent hash alone.
		other := fx.newEngine(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 0), nil))
		otherBytes, err := fx.recompute(other)
		if err != nil {
			t.Fatalf("second verifier recomputation failed: %v", err)
		}
		if !bytes.Equal(recomputed, otherBytes) {
			t.Fatalf("frequency bytes depend on node-local latest state, not just the parent:\n verifier A %x\n verifier B %x", recomputed, otherBytes)
		}

		// Property 5, second half: purity buys the memo, and the memo must then
		// be keyed by the PARENT. Every other engine in this iteration sees
		// exactly one parent, so a frequencyCache keyed by anything
		// constant-per-engine is invisible to them — yet on a live node that is
		// the worst failure this file covers: block N is served block N-1's
		// bytes forever, so every producer emits bytes no verifier reproduces.
		//
		// `producer` has already memoized fx.parent's bytes (prepare and
		// SetExtraData both went through refreshFrequencyRLP), so asking it for
		// the SECOND parent is what a real node does on the next block. The
		// answer must equal a cold engine's, i.e. must be recomputed.
		warmAlt, err := fx.altRecompute(producer)
		if err != nil {
			t.Fatalf("warm producer failed to compute frequency bytes for a second parent: %v", err)
		}
		coldEngine := fx.newEngine(fx.withAltParent(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 1), nil)))
		coldAlt, err := fx.altRecompute(coldEngine)
		if err != nil {
			t.Fatalf("cold engine failed to compute frequency bytes for a second parent: %v", err)
		}
		if !bytes.Equal(warmAlt, coldAlt) {
			t.Fatalf("frequency bytes are memoized under a key that is not the parent hash: an engine warmed on parent %s answered\n warm %x\n cold %x\nfor parent %s", parentHash.Hex(), warmAlt, coldAlt, fx.altParent.Hash().Hex())
		}
		// The fixture's discriminating power, asserted rather than assumed: the
		// two parents must not yield the same bytes, or the comparison above is
		// satisfied by a cache that ignores its key entirely. altStakes is a full
		// ramp over a disjoint validator set with no Recents, so coldAlt is always
		// a non-empty list of addresses that cannot appear in `recomputed`.
		if bytes.Equal(recomputed, coldAlt) {
			t.Fatalf("fixture lost its discriminating power: both parents yield %x, so a constant cache key would be invisible", coldAlt)
		}

		// Property 2: any change to the embedded frequency data is a mismatch.
		freqTampered, err := tamperFrequencyBytes(header, fx.config, pos, tamperDelta)
		if err != nil {
			t.Fatalf("could not tamper with the embedded frequency data: %v", err)
		}
		noPanic(t, "verifySnake8FrequencyData(freq-tampered)", func() {
			tamperVerifier := fx.newEngine(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 1), nil))
			if err := tamperVerifier.verifySnake8FrequencyData(fx.chain, freqTampered, fx.parent); !errors.Is(err, errMismatchedSnake8FrequencyData) {
				t.Fatalf("tampered frequency data accepted: got %v, want errMismatchedSnake8FrequencyData (embedded %x, extra %x)", err, embedded, freqTampered.Extra)
			}
		})

		// Property 3, verifier half: fail OPEN. A node that cannot read stakes
		// has not evaluated the block and must not condemn it — an error out of
		// Finalize is a bad-block verdict, so failing closed here turns RPC load
		// (or a nil-ethAPI engine, or missing historical state during a trace
		// replay) into a rejection of the honest head.
		blindVerifier := fx.newEngine(failingStakeReader(blindOutcome))
		if err := blindVerifier.verifySnake8FrequencyData(fx.chain, header, fx.parent); err != nil {
			t.Fatalf("verifier failed closed on a node-local %s stake read: %v", blindOutcome, err)
		}
		// ...and it must fail open for the same reason even when the bytes it
		// cannot check are garbage: the classification, not the payload, decides.
		if err := blindVerifier.verifySnake8FrequencyData(fx.chain, freqTampered, fx.parent); err != nil {
			t.Fatalf("verifier failed closed on a %s stake read while judging tampered bytes: %v", blindOutcome, err)
		}

		// ...but fail-open is scoped to the stake lookup, not to the header. A
		// structurally broken frequency block never reaches the recomputation,
		// so it must be rejected outright even by a node that could not have
		// read stakes anyway.
		prefixTampered, err := tamperFrequencyPrefix(header, fx.config, tamperDelta)
		if err != nil {
			t.Fatalf("could not corrupt the frequency-block prefix: %v", err)
		}
		noPanic(t, "verify(prefix-tampered)", func() {
			if err := producer.verifySnake8Extra(prefixTampered, fx.parent); !errors.Is(err, errInvalidSnake8Extra) {
				t.Fatalf("header with a corrupted frequency-block prefix accepted by verifySnake8Extra: %v", err)
			}
			structVerifier := fx.newEngine(honestStakeReader(parentHash, parentStakes, decoyStakeTable(validators, 1), nil))
			if err := structVerifier.verifySnake8FrequencyData(fx.chain, prefixTampered, fx.parent); err == nil {
				t.Fatalf("header with a corrupted frequency-block prefix accepted by verifySnake8FrequencyData (extra %x)", prefixTampered.Extra)
			}
			// The node that could not read stakes is the one that matters here.
			// A verifier with a working stake backend rejects the malformed
			// header no matter where the parse sits relative to the
			// recomputation, so the assertion above cannot tell the two layouts
			// apart. Reusing blindVerifier can: today parseValidatorFrequencies
			// runs BEFORE refreshFrequencyRLP, so the structural rejection
			// happens before fail-open is ever reachable. A refactor that hoists
			// the recomputation above the parse — an easy, innocent-looking
			// reordering — would make errSnake8StakeLookup swallow this header
			// whole, and every node under RPC load would start accepting headers
			// carrying no frequency block at all.
			if err := blindVerifier.verifySnake8FrequencyData(fx.chain, prefixTampered, fx.parent); err == nil {
				t.Fatalf("a verifier that cannot read stakes accepted a header with a corrupted frequency-block prefix: fail-open is scoped to the %s stake lookup, not to malformed header data (extra %x)", blindOutcome, prefixTampered.Extra)
			}
		})

		// Property 3, producer half: HARD FAIL. The asymmetry is the invariant —
		// a sealer that fails open would emit bytes no verifier can reproduce.
		blindProducer := fx.newEngine(failingStakeReader(blindOutcome))
		_, err = fx.produce(blindProducer, coinbase)
		if err == nil {
			t.Fatalf("post-Snake8Fix sealer produced a block despite a %s stake read", blindOutcome)
		}
		if !errors.Is(err, errSnake8StakeLookup) {
			t.Fatalf("a %s stake read must be classified as errSnake8StakeLookup (that classification is what the verifier's fail-open branch keys on), got: %v", blindOutcome, err)
		}
		// The MEV path enters through SetExtraData rather than prepare(), so it
		// carries its own copy of the refusal. prepare() failing first would hide
		// a regression there, hence the standalone call.
		blindSolo := &types.Header{
			Number:     new(big.Int).SetUint64(snake8RoundTripChildNumber),
			ParentHash: parentHash,
			Time:       header.Time,
		}
		if err := fx.newEngine(failingStakeReader(blindOutcome)).SetExtraData(fx.chain, blindSolo); !errors.Is(err, errSnake8StakeLookup) {
			t.Fatalf("standalone SetExtraData must refuse to embed on a %s stake read, got: %v", blindOutcome, err)
		}
		// The same classification, straight out of refreshFrequencyRLP, so a
		// refactor that moves the wrapping into Prepare is still caught.
		if _, err := fx.recompute(blindProducer); !errors.Is(err, errSnake8StakeLookup) {
			t.Fatalf("refreshFrequencyRLP must classify a %s stake read as errSnake8StakeLookup, got: %v", blindOutcome, err)
		}
	})
}
