package parlia

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// Snake8Fix (COR-173) test fixture: a three-validator chain at block 99 with both
// Snake8 and (optionally) Snake8Fix active from genesis, driven through the same
// minimal chain reader as TestPrepareDifficultyMatchesEmbeddedFrequencyData.

var (
	snake8FixValA = common.HexToAddress("0x0100000000000000000000000000000000000001")
	snake8FixValB = common.HexToAddress("0x0200000000000000000000000000000000000002")
	snake8FixValC = common.HexToAddress("0x0300000000000000000000000000000000000003")
)

func newSnake8FixTestConfig(snake8FixActive bool) *params.ChainConfig {
	config := &params.ChainConfig{
		ChainID:             big.NewInt(714),
		HomesteadBlock:      common.Big0,
		EIP150Block:         common.Big0,
		EIP155Block:         common.Big0,
		EIP158Block:         common.Big0,
		ByzantiumBlock:      common.Big0,
		ConstantinopleBlock: common.Big0,
		PetersburgBlock:     common.Big0,
		IstanbulBlock:       common.Big0,
		MuirGlacierBlock:    common.Big0,
		BerlinBlock:         common.Big0,
		LondonBlock:         common.Big0,
		RamanujanBlock:      common.Big0,
		NielsBlock:          common.Big0,
		MirrorSyncBlock:     common.Big0,
		BrunoBlock:          common.Big0,
		Snake8Time:          new(uint64), // Snake8 active from genesis
		Parlia:              &params.ParliaConfig{Period: 3, Epoch: 200},
	}
	if snake8FixActive {
		config.Snake8FixTime = new(uint64)
	}
	return config
}

// newSnake8FixTestChain builds genesis + parent(99) headers, a chain reader over
// them, and a Parlia engine whose snapshot LRU is seeded at the parent.
func newSnake8FixTestChain(t *testing.T, config *params.ChainConfig) (*Parlia, *prepareTestChainReader, *types.Header) {
	t.Helper()
	period := config.Parlia.Period
	genesis := &types.Header{
		Number:     common.Big0,
		Time:       1_700_000_000,
		Difficulty: big.NewInt(1),
		Extra:      make([]byte, extraVanity+extraSeal),
	}
	parent := &types.Header{
		Number:     big.NewInt(99),
		ParentHash: genesis.Hash(),
		Time:       1_700_000_000 + 99*period,
		Difficulty: diffInTurn,
		Coinbase:   snake8FixValA,
		Extra:      make([]byte, extraVanity+extraSeal),
	}
	chain := &prepareTestChainReader{
		config:  config,
		genesis: genesis,
		headers: map[common.Hash]*types.Header{genesis.Hash(): genesis, parent.Hash(): parent},
	}

	engine := New(config, rawdb.NewMemoryDatabase(), nil, genesis.Hash())
	t.Cleanup(func() { engine.Close() })

	validators := []common.Address{snake8FixValA, snake8FixValB, snake8FixValC}
	snap := newSnapshot(engine.config, engine.signatures, 99, parent.Hash(), validators, nil, nil, true)
	snap.TurnLength = 50
	engine.recentSnaps.Add(parent.Hash(), snap)

	return engine, chain, parent
}

// stakesOf returns a stakeReader serving a fixed stake table.
func stakesOf(table map[common.Address]*big.Int) func(common.Address, uint64, *rpc.BlockNumberOrHash) (*big.Int, error) {
	return func(validator common.Address, blockNumber uint64, state *rpc.BlockNumberOrHash) (*big.Int, error) {
		return table[validator], nil
	}
}

func chz(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18))
}

// TestSnake8FixPrepareEmbedsPinnedStakeData asserts the producer side of the
// Snake8Fix fork: every stake read Prepare performs is pinned to the parent
// block's hash (not latest), the embedded frequency bytes equal an independent
// recomputation from those stakes, the stamped difficulty is coherent with the
// embedded bytes, and the produced header passes verifySnake8FrequencyData —
// the exact check every importing node will run in Finalize.
func TestSnake8FixPrepareEmbedsPinnedStakeData(t *testing.T) {
	config := newSnake8FixTestConfig(true)
	engine, chain, parent := newSnake8FixTestChain(t, config)

	stakes := map[common.Address]*big.Int{
		snake8FixValA: chz(100),
		snake8FixValB: chz(200),
		snake8FixValC: chz(700),
	}
	var recorded []*rpc.BlockNumberOrHash
	inner := stakesOf(stakes)
	engine.stakeReader = func(validator common.Address, blockNumber uint64, state *rpc.BlockNumberOrHash) (*big.Int, error) {
		recorded = append(recorded, state)
		if blockNumber != 99 {
			t.Errorf("stake read at block %d, want 99 (parent)", blockNumber)
		}
		return inner(validator, blockNumber, state)
	}
	engine.Authorize(snake8FixValC, nil, nil)

	header := &types.Header{Number: big.NewInt(100), ParentHash: parent.Hash()}
	if err := engine.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	if len(recorded) == 0 {
		t.Fatal("stake reader was never invoked")
	}
	for i, state := range recorded {
		if state == nil || state.BlockHash == nil || *state.BlockHash != parent.Hash() {
			t.Fatalf("stake read %d not pinned to parent hash: got %v", i, state)
		}
	}

	// Independent recomputation of the expected bytes from the same stakes.
	validators := []common.Address{snake8FixValA, snake8FixValB, snake8FixValC}
	refSnap := newSnapshot(engine.config, engine.signatures, 99, parent.Hash(), validators, nil, nil, true)
	refSnap.TurnLength = 50
	expectedFreq, err := refSnap.calcFrequencyRLP(stakes)
	if err != nil {
		t.Fatalf("reference calcFrequencyRLP failed: %v", err)
	}
	embedded, err := parseValidatorFrequencies(header, config)
	if err != nil {
		t.Fatalf("produced header has unparseable Snake8 extra data: %v", err)
	}
	if string(embedded) != string(expectedFreq) {
		t.Fatalf("embedded frequency bytes differ from pinned-stake recomputation:\nembedded:   %x\nrecomputed: %x", embedded, expectedFreq)
	}

	// Producer coherence (same property TestPrepareDifficultyMatchesEmbeddedFrequencyData pins).
	refSnap.FrequencyRLP = embedded
	expectedDiff := diffNoTurn
	if refSnap.inturn(header.Coinbase) {
		expectedDiff = diffInTurn
	}
	if header.Difficulty.Cmp(expectedDiff) != 0 {
		t.Fatalf("difficulty %v incoherent with embedded frequency data (want %v)", header.Difficulty, expectedDiff)
	}

	// The verifier's consensus check accepts the honestly produced header.
	if err := engine.verifySnake8FrequencyData(chain, header, parent); err != nil {
		t.Fatalf("verifySnake8FrequencyData rejected an honest header: %v", err)
	}
}

// TestSnake8FixVerifyRejectsTamperedFrequencyData asserts the first acceptance
// criterion of COR-173: post-fork, a header whose embedded frequency bytes differ
// from the state-derived recomputation is rejected. Two attack shapes:
//  1. a producer whose stake view was skewed in its own favor (the ticket's
//     {self: 100%} scenario) is rejected by a verifier reading the real stakes;
//  2. bytes tampered in flight are rejected.
func TestSnake8FixVerifyRejectsTamperedFrequencyData(t *testing.T) {
	config := newSnake8FixTestConfig(true)

	trueStakes := map[common.Address]*big.Int{
		snake8FixValA: chz(100),
		snake8FixValB: chz(200),
		snake8FixValC: chz(700),
	}
	// The malicious producer claims it holds all the stake.
	skewedStakes := map[common.Address]*big.Int{
		snake8FixValA: chz(1000),
		snake8FixValB: big.NewInt(0),
		snake8FixValC: big.NewInt(0),
	}

	producer, chain, parent := newSnake8FixTestChain(t, config)
	producer.stakeReader = stakesOf(skewedStakes)
	producer.Authorize(snake8FixValA, nil, nil)

	header := &types.Header{Number: big.NewInt(100), ParentHash: parent.Hash()}
	if err := producer.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	verifier, _, _ := newSnake8FixTestChain(t, config)
	verifier.stakeReader = stakesOf(trueStakes)
	if err := verifier.verifySnake8FrequencyData(chain, header, parent); !errors.Is(err, errMismatchedSnake8FrequencyData) {
		t.Fatalf("verifier accepted a header with self-serving frequency data: err=%v", err)
	}

	// Honest producer, bytes flipped in flight.
	honest, chain2, parent2 := newSnake8FixTestChain(t, config)
	honest.stakeReader = stakesOf(trueStakes)
	honest.Authorize(snake8FixValA, nil, nil)
	header2 := &types.Header{Number: big.NewInt(100), ParentHash: parent2.Hash()}
	if err := honest.Prepare(chain2, header2); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	embedded, err := parseValidatorFrequencies(header2, config)
	if err != nil {
		t.Fatalf("parseValidatorFrequencies failed: %v", err)
	}
	embedded[len(embedded)-1] ^= 0xff // aliases header2.Extra
	if err := honest.verifySnake8FrequencyData(chain2, header2, parent2); !errors.Is(err, errMismatchedSnake8FrequencyData) {
		t.Fatalf("verifier accepted a header with tampered frequency bytes: err=%v", err)
	}
}

// TestSnake8FixVerifySkipsPreFork asserts the second acceptance criterion:
// pre-fork blocks are never checked against a recomputation, whatever their
// embedded bytes contain — historical blocks were produced with unpinned,
// degradation-prone stake reads and must keep replaying unchanged.
func TestSnake8FixVerifySkipsPreFork(t *testing.T) {
	config := newSnake8FixTestConfig(false) // Snake8 active, Snake8Fix not scheduled
	engine, chain, parent := newSnake8FixTestChain(t, config)
	// A stake reader whose results nothing could have embedded: were the check
	// active, any header would mismatch.
	engine.stakeReader = stakesOf(map[common.Address]*big.Int{
		snake8FixValA: chz(1), snake8FixValB: chz(2), snake8FixValC: chz(3),
	})

	// Degraded historical header: produced with no stake backend at all
	// (round-robin, empty frequency bytes).
	producer, chain2, _ := newSnake8FixTestChain(t, config)
	producer.Authorize(snake8FixValC, nil, nil)
	header := &types.Header{Number: big.NewInt(100), ParentHash: parent.Hash()}
	if err := producer.Prepare(chain2, header); err != nil {
		t.Fatalf("pre-fork Prepare must keep succeeding without a stake backend (historical degradation): %v", err)
	}

	if err := engine.verifySnake8FrequencyData(chain, header, parent); err != nil {
		t.Fatalf("pre-fork header must not be checked against recomputation: %v", err)
	}
}

// TestSnake8FixPrepareFailsOnStakeLookupError asserts the third acceptance
// criterion (the stake-lookup-failure behavior): post-fork a sealer that cannot
// read stakes refuses to produce instead of embedding degraded bytes that
// verifiers would reject; a verifier that cannot read stakes fails OPEN — a
// node-local inability to evaluate must not condemn the block (an error out of
// Finalize is treated as a bad block, so failing closed would let RPC load or a
// nil-ethAPI engine reject the honest head).
func TestSnake8FixPrepareFailsOnStakeLookupError(t *testing.T) {
	config := newSnake8FixTestConfig(true)
	engine, chain, parent := newSnake8FixTestChain(t, config)
	// No override: the default stakeReader has no RPC backend and fails.
	engine.Authorize(snake8FixValC, nil, nil)

	header := &types.Header{Number: big.NewInt(100), ParentHash: parent.Hash()}
	err := engine.Prepare(chain, header)
	if err == nil {
		t.Fatal("post-Snake8Fix Prepare must fail when stakes cannot be read")
	}
	if !errors.Is(err, errSnake8StakeLookup) {
		t.Fatalf("Prepare error must be classified as errSnake8StakeLookup, got: %v", err)
	}

	// Verifier side: an honestly produced header cannot be judged without
	// stakes — accept it (fail-open on the local failure), never reject.
	producer, chain2, _ := newSnake8FixTestChain(t, config)
	producer.stakeReader = stakesOf(map[common.Address]*big.Int{
		snake8FixValA: chz(100), snake8FixValB: chz(200), snake8FixValC: chz(700),
	})
	producer.Authorize(snake8FixValC, nil, nil)
	header2 := &types.Header{Number: big.NewInt(100), ParentHash: parent.Hash()}
	if err := producer.Prepare(chain2, header2); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if err := engine.verifySnake8FrequencyData(chain, header2, parent); err != nil {
		t.Fatalf("verifier must fail open on a local stake-lookup failure, got: %v", err)
	}
}

// TestSnake8FixFrequencyCacheSharesOneStakeSweep pins the memoization: post-fork
// frequency bytes are a pure function of the parent hash, so prepare +
// SetExtraData + the Finalize verification perform exactly one stake sweep per
// parent (one reader call per validator), not one per code path.
func TestSnake8FixFrequencyCacheSharesOneStakeSweep(t *testing.T) {
	config := newSnake8FixTestConfig(true)
	engine, chain, parent := newSnake8FixTestChain(t, config)

	var calls int
	inner := stakesOf(map[common.Address]*big.Int{
		snake8FixValA: chz(100), snake8FixValB: chz(200), snake8FixValC: chz(700),
	})
	engine.stakeReader = func(validator common.Address, blockNumber uint64, state *rpc.BlockNumberOrHash) (*big.Int, error) {
		calls++
		return inner(validator, blockNumber, state)
	}
	engine.Authorize(snake8FixValC, nil, nil)

	header := &types.Header{Number: big.NewInt(100), ParentHash: parent.Hash()}
	if err := engine.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if calls != 3 {
		t.Fatalf("prepare+SetExtraData should share one stake sweep (3 validators), got %d reader calls", calls)
	}
	if err := engine.verifySnake8FrequencyData(chain, header, parent); err != nil {
		t.Fatalf("verifySnake8FrequencyData failed: %v", err)
	}
	if calls != 3 {
		t.Fatalf("verification should reuse the memoized bytes, got %d reader calls", calls)
	}
}

// TestSnake8FixNilSeamsDoNotPanic pins the struct-literal escape hatch: engines
// assembled without New() (as several existing tests do) leave stakeReader and
// frequencyCache nil; refreshFrequencyRLP must fall back to the real lookup and
// skip the cache instead of nil-panicking.
func TestSnake8FixNilSeamsDoNotPanic(t *testing.T) {
	config := newSnake8FixTestConfig(true)
	engine, _, parent := newSnake8FixTestChain(t, config)
	engine.stakeReader = nil
	engine.frequencyCache = nil

	validators := []common.Address{snake8FixValA, snake8FixValB, snake8FixValC}
	snap := newSnapshot(engine.config, engine.signatures, 99, parent.Hash(), validators, nil, nil, true)
	snap.TurnLength = 50

	// Fallback is getValidatorTotalDelegated, which has no RPC backend here, so
	// the pinned path must surface the classified local error — not panic.
	err := engine.refreshFrequencyRLP(snap, 100, parent)
	if !errors.Is(err, errSnake8StakeLookup) {
		t.Fatalf("expected errSnake8StakeLookup from the fallback reader, got: %v", err)
	}
}

// TestSnake8FixZeroStakesDegradeConsistently pins the one deliberate soft spot
// left post-fork: calcFrequencyRLP's "no eligible validators" degradation is a
// deterministic function of the pinned stakes, so a producer that embeds empty
// frequency bytes (round-robin selection) is accepted by a verifier recomputing
// the same emptiness — the chain degrades but never splits or halts.
func TestSnake8FixZeroStakesDegradeConsistently(t *testing.T) {
	config := newSnake8FixTestConfig(true)
	zeroStakes := stakesOf(map[common.Address]*big.Int{
		snake8FixValA: big.NewInt(0), snake8FixValB: big.NewInt(0), snake8FixValC: big.NewInt(0),
	})

	producer, chain, parent := newSnake8FixTestChain(t, config)
	producer.stakeReader = zeroStakes
	producer.Authorize(snake8FixValC, nil, nil)
	header := &types.Header{Number: big.NewInt(100), ParentHash: parent.Hash()}
	if err := producer.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare must succeed on deterministic zero stakes: %v", err)
	}
	embedded, err := parseValidatorFrequencies(header, config)
	if err != nil {
		t.Fatalf("parseValidatorFrequencies failed: %v", err)
	}
	if len(embedded) != 0 {
		t.Fatalf("zero stakes must embed empty frequency bytes, got %x", embedded)
	}

	verifier, _, _ := newSnake8FixTestChain(t, config)
	verifier.stakeReader = zeroStakes
	if err := verifier.verifySnake8FrequencyData(chain, header, parent); err != nil {
		t.Fatalf("verifier must accept the deterministic degraded header: %v", err)
	}
}
