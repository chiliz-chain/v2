package parlia

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// prepareTestChainReader is a minimal consensus.ChainHeaderReader over a fixed
// header set — just enough for Parlia.Prepare on a non-epoch, Snake8-active block.
type prepareTestChainReader struct {
	config  *params.ChainConfig
	genesis *types.Header
	headers map[common.Hash]*types.Header
}

func (r *prepareTestChainReader) Config() *params.ChainConfig  { return r.config }
func (r *prepareTestChainReader) CurrentHeader() *types.Header { return r.genesis }
func (r *prepareTestChainReader) GenesisHeader() *types.Header { return r.genesis }
func (r *prepareTestChainReader) GetHeader(hash common.Hash, number uint64) *types.Header {
	return r.headers[hash]
}
func (r *prepareTestChainReader) GetHeaderByNumber(number uint64) *types.Header { return nil }
func (r *prepareTestChainReader) GetHeaderByHash(hash common.Hash) *types.Header {
	return r.headers[hash]
}
func (r *prepareTestChainReader) GetTd(hash common.Hash, number uint64) *big.Int { return nil }
func (r *prepareTestChainReader) GetHighestVerifiedHeader() *types.Header        { return nil }
func (r *prepareTestChainReader) GetVerifiedBlockByHash(hash common.Hash) *types.Header {
	return r.headers[hash]
}

// TestPrepareDifficultyMatchesEmbeddedFrequencyData pins the Snake8 producer
// coherence invariant (CLAUDE.md §2, COR-37): the difficulty prepare() stamps
// on a header must agree with the frequency data SetExtraData embeds in that
// same header, because verifiers (and our own Seal) judge in-turn-ness from
// the embedded data.
//
// The v1.7.6 sync regressed this: prepare() derived the difficulty from the
// snapshot's cached FrequencyRLP (stamped by whichever caller touched the LRU
// last — typically the parent block's data) while the fresh frequency data was
// only computed afterwards, in SetExtraData. Producers then emitted blocks
// that were invalid by construction and forked a live devnet three ways.
//
// The test reproduces the trigger directly: it seeds the snapshot LRU with a
// stale FrequencyRLP whose selection differs from what the freshly computed
// data (here: degraded to round-robin, since the test engine has no RPC
// backend for stake lookups) selects, runs Prepare, and asserts the
// verifier's view: difficulty recomputed from the *emitted header's* embedded
// frequency data must equal the stamped difficulty. On the pre-fix ordering
// this fails with stamped=1 vs expected=2.
func TestPrepareDifficultyMatchesEmbeddedFrequencyData(t *testing.T) {
	period := uint64(3)
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
		Parlia:              &params.ParliaConfig{Period: period, Epoch: 200},
	}

	// Three validators with a known sort order. Round-robin at child height 100
	// picks validators()[(99+1)/50%3] = validators()[2] — the highest address.
	valA := common.HexToAddress("0x0100000000000000000000000000000000000001")
	valB := common.HexToAddress("0x0200000000000000000000000000000000000002")
	valC := common.HexToAddress("0x0300000000000000000000000000000000000003")
	validators := []common.Address{valA, valB, valC}
	roundRobinPick := valC

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
		Coinbase:   valA,
		Extra:      make([]byte, extraVanity+extraSeal),
	}
	chain := &prepareTestChainReader{
		config:  config,
		genesis: genesis,
		headers: map[common.Hash]*types.Header{genesis.Hash(): genesis, parent.Hash(): parent},
	}

	db := rawdb.NewMemoryDatabase()
	engine := New(config, db, nil, genesis.Hash())
	defer engine.Close()
	engine.Authorize(roundRobinPick, nil, nil)

	// Stale frequency data: a single-entry distribution that always selects
	// valA. This emulates the LRU state left behind by a prior caller (e.g.
	// the worker stamping the parent block's embedded data).
	type candidateEntry struct {
		Address   common.Address
		Frequency *big.Int
	}
	staleFreq, err := rlp.EncodeToBytes([]candidateEntry{{Address: valA, Frequency: big.NewInt(validatorFrequencyPrecision)}})
	if err != nil {
		t.Fatalf("failed to encode stale frequency data: %v", err)
	}

	snap := newSnapshot(engine.config, engine.signatures, 99, parent.Hash(), validators, nil, nil, true)
	snap.TurnLength = 50
	snap.FrequencyRLP = staleFreq
	engine.recentSnaps.Add(parent.Hash(), snap)

	// Guard the test's discriminating power: the stale data must select a
	// different validator than the sealer, otherwise both orderings agree.
	if got := snap.inturnValidator(); got != valA || got == roundRobinPick {
		t.Fatalf("test setup broken: stale selection %v, want %v (!= sealer %v)", got, valA, roundRobinPick)
	}

	header := &types.Header{
		Number:     big.NewInt(100),
		ParentHash: parent.Hash(),
	}
	if err := engine.Prepare(chain, header); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// Verifier's view: recompute the expected difficulty from the frequency
	// data actually embedded in the produced header (verifyCascadingFields
	// does exactly this via snapshot(..., extraHeader=header)).
	embeddedFreq, err := parseValidatorFrequencies(header, config)
	if err != nil {
		t.Fatalf("produced header has unparseable Snake8 extra data: %v", err)
	}
	verifierSnap := newSnapshot(engine.config, engine.signatures, 99, parent.Hash(), validators, nil, nil, true)
	verifierSnap.TurnLength = 50
	verifierSnap.FrequencyRLP = embeddedFreq

	expected := diffNoTurn
	if verifierSnap.inturn(header.Coinbase) {
		expected = diffInTurn
	}
	if header.Difficulty.Cmp(expected) != 0 {
		t.Fatalf("producer/verifier difficulty mismatch: prepare stamped %v, but the header's own embedded frequency data implies %v — difficulty was derived from stale frequency data (COR-37 Snake8 ordering regression)",
			header.Difficulty, expected)
	}

	// Concrete tripwire on top of the property: with no stake backend the
	// fresh data degrades to round-robin, our sealer is the round-robin pick,
	// so the coherent outcome is in-turn.
	if header.Difficulty.Cmp(diffInTurn) != 0 {
		t.Fatalf("expected in-turn difficulty %v for the round-robin pick, got %v", diffInTurn, header.Difficulty)
	}
}
