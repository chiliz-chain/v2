package parlia

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// The tests in this file pin the COR-174 invariant: Parlia.snapshot() must never
// write caller-dependent Snake8 data into the shared recentSnaps entry. Callers
// stamp with wildly different data — the verify path with a not-yet-validated
// candidate header, Seal with the block it just built, the parlia_getSnapshot RPC
// with whatever block was requested — and in-turn selection is derived from
// FrequencyRLP, so a shared stamp made the selected validator depend on call order.
//
// Each test therefore checks two things: the caller gets the view it asked for,
// AND the cached entry is untouched afterwards.

// snake8CacheChainReader is prepareTestChainReader with an explicit head, so
// API.GetSnapshot(nil) resolves to a header of our choosing.
type snake8CacheChainReader struct {
	*prepareTestChainReader
	head *types.Header
}

func (r *snake8CacheChainReader) CurrentHeader() *types.Header { return r.head }

// snake8CacheFixture is the shared setup: a Snake8-active chain with three
// validators, a genesis + parent(99) header pair, and an engine with no RPC
// backend (so stake lookups soft-fail and fresh frequency data degrades to
// round-robin — the discriminating signal these tests rely on).
type snake8CacheFixture struct {
	config     *params.ChainConfig
	chain      *snake8CacheChainReader
	engine     *Parlia
	validators []common.Address
	genesis    *types.Header
	parent     *types.Header
	valA       common.Address
	valB       common.Address
	valC       common.Address
}

// pristineTurnLength is deliberately neither defaultTurnLength nor Snake8's 50,
// so a leaked stamp is unmistakable.
const pristineTurnLength = 7

// snake8CacheConfig is a Snake8-from-genesis Parlia config with epoch 200. The epoch
// matters for TestSnapshotApplyValidatorSetSwitchAtSnake8Offset: it must exceed
// minerHistoryCheckLen() (99 with three validators under Snake8) for the validator-set
// switch to be reachable at all.
func snake8CacheConfig() *params.ChainConfig {
	period := uint64(3)
	return &params.ChainConfig{
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
}

func newSnake8CacheFixture(t *testing.T) *snake8CacheFixture {
	t.Helper()

	config := snake8CacheConfig()
	period := config.Parlia.Period

	// Same three validators (and the same round-robin reasoning) as
	// TestPrepareDifficultyMatchesEmbeddedFrequencyData: at child height 100
	// round-robin picks validators()[(99+1)/50%3] = validators()[2] = valC.
	valA := common.HexToAddress("0x0100000000000000000000000000000000000001")
	valB := common.HexToAddress("0x0200000000000000000000000000000000000002")
	valC := common.HexToAddress("0x0300000000000000000000000000000000000003")
	validators := []common.Address{valA, valB, valC}

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
	chain := &snake8CacheChainReader{
		prepareTestChainReader: &prepareTestChainReader{
			config:  config,
			genesis: genesis,
			headers: map[common.Hash]*types.Header{genesis.Hash(): genesis, parent.Hash(): parent},
		},
		head: parent,
	}

	engine := New(config, rawdb.NewMemoryDatabase(), nil, genesis.Hash())
	t.Cleanup(func() { engine.Close() })
	engine.Authorize(valC, nil, nil)

	return &snake8CacheFixture{
		config:     config,
		chain:      chain,
		engine:     engine,
		validators: validators,
		genesis:    genesis,
		parent:     parent,
		valA:       valA,
		valB:       valB,
		valC:       valC,
	}
}

// seedCache installs a snapshot for parent(99) in the LRU and returns it. It is seeded
// with values no caller would stamp (IsSnake8Fork false, TurnLength neither the default
// nor 50), so that any write from a caller's stamp is unmistakable.
func (f *snake8CacheFixture) seedCache(t *testing.T, freq []byte) *Snapshot {
	t.Helper()
	snap := newSnapshot(f.engine.config, f.engine.signatures, 99, f.parent.Hash(), f.validators, nil, nil, false)
	snap.TurnLength = pristineTurnLength
	snap.FrequencyRLP = freq
	f.engine.recentSnaps.Add(f.parent.Hash(), snap)
	return snap
}

// candidateHeader builds a child header at 100 embedding freq as its Snake8 VFQ
// section — the shape verifyCascadingFields passes as extraHeader.
func (f *snake8CacheFixture) candidateHeader(freq []byte) *types.Header {
	return &types.Header{
		Number:     big.NewInt(100),
		ParentHash: f.parent.Hash(),
		Time:       f.parent.Time + f.config.Parlia.Period,
		Coinbase:   f.valB,
		Difficulty: diffInTurn,
		Extra:      buildNonEpochExtra(f.parent.Time, true, freq),
	}
}

// encodeFrequency builds a single-entry distribution that always selects addr.
func encodeFrequency(t *testing.T, addr common.Address) []byte {
	t.Helper()
	type candidateEntry struct {
		Address   common.Address
		Frequency *big.Int
	}
	freq, err := rlp.EncodeToBytes([]candidateEntry{{Address: addr, Frequency: big.NewInt(validatorFrequencyPrecision)}})
	if err != nil {
		t.Fatalf("failed to encode frequency data: %v", err)
	}
	return freq
}

// TestSnapshotCacheNotMutatedByExtraHeader is the core COR-174 assertion: a
// verify-shaped snapshot() call gets the candidate header's frequency data, while
// the cached entry keeps its own. Before the fix the stamp landed on the cached
// object, letting an unvalidated foreign header write into shared consensus state.
func TestSnapshotCacheNotMutatedByExtraHeader(t *testing.T) {
	f := newSnake8CacheFixture(t)

	pristineFreq := encodeFrequency(t, f.valA)
	forgedFreq := encodeFrequency(t, f.valB)
	cached := f.seedCache(t, pristineFreq)

	// Verify path: key on the parent, stamp with the (unvalidated) candidate.
	snap, err := f.engine.snapshot(f.chain, 99, f.parent.Hash(), nil, true, f.candidateHeader(forgedFreq))
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}

	// The caller gets the view it asked for.
	if !bytes.Equal(snap.FrequencyRLP, forgedFreq) {
		t.Fatalf("caller's snapshot: FrequencyRLP = %x, want the candidate header's %x", snap.FrequencyRLP, forgedFreq)
	}
	if snap.TurnLength != snake8TurnLength {
		t.Fatalf("caller's snapshot: TurnLength = %d, want %d under Snake8", snap.TurnLength, snake8TurnLength)
	}
	if !snap.IsSnake8Fork {
		t.Fatal("caller's snapshot: IsSnake8Fork = false, want true under Snake8")
	}

	// ...on an object it owns, not the cached one.
	if snap == cached {
		t.Fatal("snapshot() returned the cached object itself; caller-dependent Snake8 data would be written into shared state (COR-174)")
	}

	// The cached entry keeps its own frequency data.
	//
	// Only FrequencyRLP is asserted here. IsSnake8Fork and TurnLength are inputs to
	// apply() rather than per-caller views, so they legitimately live on the cached
	// entry — see TestSnapshotApplyUsesSnake8TurnLength. This call applies no headers,
	// so it leaves them as seeded.
	got, ok := f.engine.recentSnaps.Get(f.parent.Hash())
	if !ok {
		t.Fatal("cache entry for parent disappeared")
	}
	if !bytes.Equal(got.FrequencyRLP, pristineFreq) {
		t.Fatalf("cached FrequencyRLP was mutated: got %x, want %x — an unvalidated candidate header wrote into shared consensus state (COR-174)", got.FrequencyRLP, pristineFreq)
	}
}

// TestSnapshotApplyUsesSnake8TurnLength guards a trap next to the COR-174 fix.
//
// Unlike FrequencyRLP, TurnLength is an *input* to Snapshot.apply(): the header loop
// reads it through minerHistoryCheckLen()/versionHistoryCheckLen(), which size the
// Recents and RecentForkHashes prune windows and pick the block offset at which the
// validator set switches. Snake8 forces TurnLength = 50, and before COR-174 that
// value reached the loop because it had been written onto the shared cache entry.
// Moving the write to a caller-owned copy alone would leave apply() running with
// defaultTurnLength (1) — with 21 validators the validator-set switch would move from
// number%epoch == 549 to == 10, and Recents would keep ~11 entries instead of ~550.
// Recents feeds SignRecently, which decides who is eligible in calcFrequencyRLP, so
// that is a consensus break on any node that rebuilds a snapshot chain under Snake8.
//
// The assertion is behavioural: with three validators the prune limit is 100 when
// TurnLength is 50 (deletes Recents[0]) but 2 when it is 1 (deletes Recents[98]).
func TestSnapshotApplyUsesSnake8TurnLength(t *testing.T) {
	f := newSnake8CacheFixture(t)

	// apply() ecrecovers every header it applies, so the header needs a real seal.
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	signer := crypto.PubkeyToAddress(key.PublicKey)

	// A cache entry as apply() lineage produces one: never stamped, so TurnLength is
	// still the default rather than Snake8's 50.
	validators := []common.Address{signer, f.valA, f.valB}
	snap := newSnapshot(f.engine.config, f.engine.signatures, 99, f.parent.Hash(), validators, nil, nil, false)
	snap.Recents = map[uint64]common.Address{0: f.valA, 98: f.valB}
	f.engine.recentSnaps.Add(f.parent.Hash(), snap)
	if snap.TurnLength != defaultTurnLength {
		t.Fatalf("test setup broken: seeded TurnLength is %d, want the default %d", snap.TurnLength, defaultTurnLength)
	}

	header := &types.Header{
		Number:     big.NewInt(100),
		ParentHash: f.parent.Hash(),
		Time:       f.parent.Time + f.config.Parlia.Period,
		Coinbase:   signer,
		Difficulty: diffInTurn,
		Extra:      make([]byte, extraVanity+extraSeal),
	}
	sig, err := crypto.Sign(types.SealHash(header, f.config.ChainID).Bytes(), key)
	if err != nil {
		t.Fatalf("failed to sign header: %v", err)
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sig)
	f.chain.headers[header.Hash()] = header

	// Full production path: cache miss on the child, hit on the parent, apply one header.
	got, err := f.engine.snapshot(f.chain, 100, header.Hash(), nil, true, nil)
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}

	if got.TurnLength != snake8TurnLength {
		t.Fatalf("applied snapshot has TurnLength %d, want %d", got.TurnLength, snake8TurnLength)
	}
	if _, ok := got.Recents[98]; !ok {
		t.Fatal("Recents[98] was pruned: apply() ran with the default TurnLength instead of Snake8's 50, which moves the Recents window and the validator-set switch offset (COR-174 regression)")
	}
	if _, ok := got.Recents[0]; ok {
		t.Fatal("Recents[0] survived: apply() did not prune at the Snake8 window")
	}

	// The entry cached for the child must carry it too, otherwise the next apply()
	// on top of it starts from the wrong TurnLength again.
	cached, ok := f.engine.recentSnaps.Get(header.Hash())
	if !ok {
		t.Fatal("no cache entry for the applied block")
	}
	if cached.TurnLength != snake8TurnLength {
		t.Fatalf("cached snapshot has TurnLength %d, want %d", cached.TurnLength, snake8TurnLength)
	}
	if !cached.IsSnake8Fork {
		t.Fatal("cached snapshot has IsSnake8Fork = false")
	}
}

// TestSnapshotPersistenceExcludesFrequencyData pins both halves of the on-disk side of
// the COR-174 invariant: store() must not write FrequencyRLP, and loadSnapshot() must
// not return one — including from a blob written by an older client, which did persist
// the field. The load side matters because copy() now carries FrequencyRLP, so a legacy
// value on a cache entry would propagate into every snapshot derived from it.
func TestSnapshotPersistenceExcludesFrequencyData(t *testing.T) {
	config := snake8CacheConfig()
	db := rawdb.NewMemoryDatabase()
	hash := common.HexToHash("0xfeed")

	snap := newSnapshot(config.Parlia, nil, 1024, hash,
		[]common.Address{common.HexToAddress("0x0100000000000000000000000000000000000001")}, nil, nil, true)
	snap.TurnLength = snake8TurnLength
	snap.FrequencyRLP = encodeFrequency(t, common.HexToAddress("0x0100000000000000000000000000000000000001"))

	// store() side: the written blob must not mention the field at all.
	if err := snap.store(db); err != nil {
		t.Fatalf("store failed: %v", err)
	}
	blob, err := db.Get(append([]byte("parlia-"), hash[:]...))
	if err != nil {
		t.Fatalf("reading back the stored blob failed: %v", err)
	}
	if bytes.Contains(blob, []byte("frequency_rlp")) {
		t.Fatalf("store() persisted caller-dependent frequency data (COR-174): %s", blob)
	}
	// Sanity: the blob is otherwise intact, so the assertion above isn't vacuous.
	if !bytes.Contains(blob, []byte("turn_length")) {
		t.Fatalf("stored blob looks malformed, expected other fields present: %s", blob)
	}

	// loadSnapshot() side: simulate a pre-COR-174 record, which did carry the field —
	// marshalling the Snapshot directly is exactly what the old store() did.
	legacy, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshalling a legacy-shaped blob failed: %v", err)
	}
	if !bytes.Contains(legacy, []byte("frequency_rlp")) {
		t.Fatal("test setup broken: the legacy blob should contain frequency_rlp")
	}
	if err := db.Put(append([]byte("parlia-"), hash[:]...), legacy); err != nil {
		t.Fatalf("writing the legacy blob failed: %v", err)
	}

	loaded, err := loadSnapshot(config.Parlia, nil, db, hash, nil, true)
	if err != nil {
		t.Fatalf("loadSnapshot failed: %v", err)
	}
	if len(loaded.FrequencyRLP) != 0 {
		t.Fatalf("loadSnapshot returned frequency data from a legacy record: %x — copy() carries the field, so this would propagate into every derived snapshot (COR-174)", loaded.FrequencyRLP)
	}
	if loaded.TurnLength != snake8TurnLength {
		t.Fatalf("loaded TurnLength is %d, want %d under Snake8", loaded.TurnLength, snake8TurnLength)
	}
}

// TestSnapshotApplyValidatorSetSwitchAtSnake8Offset covers apply()'s epoch
// validator-set switch, the second consumer of minerHistoryCheckLen() and the one the
// devnet could not exercise (its static validator set makes the switch a near no-op).
//
// The switch fires when number%epochLength == minerHistoryCheckLen(). With three
// validators that offset is 99 under Snake8 (TurnLength 50) but 1 at the default
// TurnLength, so applying a header at block 99 with epoch 200 fires it only if the
// Snake8 turn length is in effect inside apply(). The checkpoint header encodes a
// *different* validator set than the snapshot holds, so a fired switch is directly
// observable — and a missed one leaves the stale set behind.
func TestSnapshotApplyValidatorSetSwitchAtSnake8Offset(t *testing.T) {
	config := snake8CacheConfig()
	epochLength := config.Parlia.Epoch

	// The applied header needs a real seal, and its signer must be in the pre-switch
	// validator set (apply rejects unknown signers before reaching the switch).
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	signer := crypto.PubkeyToAddress(key.PublicKey)
	valB := common.HexToAddress("0x0200000000000000000000000000000000000002")
	valC := common.HexToAddress("0x0300000000000000000000000000000000000003")
	incoming := common.HexToAddress("0x0400000000000000000000000000000000000004")

	oldSet := []common.Address{signer, valB, valC}
	newSet := []common.Address{signer, valB, incoming}

	// Block 0 is the checkpoint FindAncientHeader will land on (99 blocks back from
	// block 99). Pre-Luban epoch layout: vanity + 20 bytes per validator + seal.
	checkpointExtra := make([]byte, extraVanity)
	for _, v := range newSet {
		checkpointExtra = append(checkpointExtra, v.Bytes()...)
	}
	checkpointExtra = append(checkpointExtra, make([]byte, extraSeal)...)
	checkpoint := &types.Header{
		Number:     common.Big0,
		Time:       1_700_000_000,
		Difficulty: big.NewInt(1),
		Extra:      checkpointExtra,
	}

	// Blocks 1..98 only need to exist and chain correctly — FindAncientHeader walks
	// them, nothing parses them.
	headers := map[common.Hash]*types.Header{checkpoint.Hash(): checkpoint}
	parent := checkpoint
	for n := uint64(1); n <= 98; n++ {
		h := &types.Header{
			Number:     new(big.Int).SetUint64(n),
			ParentHash: parent.Hash(),
			Time:       checkpoint.Time + n*config.Parlia.Period,
			Difficulty: diffInTurn,
			Coinbase:   signer,
			Extra:      make([]byte, extraVanity+extraSeal),
		}
		headers[h.Hash()] = h
		parent = h
	}

	header := &types.Header{
		Number:     big.NewInt(99),
		ParentHash: parent.Hash(),
		Time:       checkpoint.Time + 99*config.Parlia.Period,
		Difficulty: diffInTurn,
		Coinbase:   signer,
		Extra:      make([]byte, extraVanity+extraSeal),
	}
	sig, err := crypto.Sign(types.SealHash(header, config.ChainID).Bytes(), key)
	if err != nil {
		t.Fatalf("failed to sign header: %v", err)
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sig)
	headers[header.Hash()] = header

	chain := &snake8CacheChainReader{
		prepareTestChainReader: &prepareTestChainReader{config: config, genesis: checkpoint, headers: headers},
		head:                   parent,
	}

	engine := New(config, rawdb.NewMemoryDatabase(), nil, checkpoint.Hash())
	defer engine.Close()

	// The cached snapshot at 98 holds the OLD set and an unstamped turn length, which
	// is what an apply() lineage produces once no caller writes into the cache.
	snap := newSnapshot(engine.config, engine.signatures, 98, parent.Hash(), oldSet, nil, nil, false)
	snap.EpochLength = epochLength
	engine.recentSnaps.Add(parent.Hash(), snap)

	// Pin the arithmetic the test depends on, so a change in validator count or epoch
	// turns into a clear setup failure rather than a silently vacuous pass.
	snake8Offset := (uint64(len(oldSet))/2+1)*uint64(snake8TurnLength) - 1
	defaultOffset := (uint64(len(oldSet))/2+1)*uint64(defaultTurnLength) - 1
	if snake8Offset != header.Number.Uint64()%epochLength {
		t.Fatalf("test setup broken: Snake8 switch offset is %d, need %d to fire at block %v",
			snake8Offset, header.Number.Uint64()%epochLength, header.Number)
	}
	if defaultOffset == snake8Offset {
		t.Fatal("test setup broken: default and Snake8 offsets coincide, so the test cannot discriminate")
	}

	got, err := engine.snapshot(chain, 99, header.Hash(), nil, true, nil)
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}

	if _, ok := got.Validators[incoming]; !ok {
		t.Fatalf("validator-set switch did not fire at block 99: %v is absent, so apply() computed the switch offset as %d (default turn length) instead of %d (Snake8) — the same minerHistoryCheckLen() that sizes the Recents window also picks this offset (COR-174)",
			incoming, defaultOffset, snake8Offset)
	}
	if _, ok := got.Validators[valC]; ok {
		t.Fatalf("stale validator %v survived the switch", valC)
	}
	if len(got.Validators) != len(newSet) {
		t.Fatalf("post-switch validator set has %d entries, want %d", len(got.Validators), len(newSet))
	}
	if got.TurnLength != snake8TurnLength {
		t.Fatalf("post-switch TurnLength is %d, want %d", got.TurnLength, snake8TurnLength)
	}
	if got.Number != 99 {
		t.Fatalf("applied snapshot number is %d, want 99", got.Number)
	}
}

// TestSnapshotFrequencyIndependentOfCallOrder is the ticket's first acceptance
// criterion: a snapshot fetched for a given block hash carries the same data
// regardless of which code path touched the cache before it.
func TestSnapshotFrequencyIndependentOfCallOrder(t *testing.T) {
	f := newSnake8CacheFixture(t)

	pristineFreq := encodeFrequency(t, f.valA)
	forgedFreq := encodeFrequency(t, f.valB)
	f.seedCache(t, pristineFreq)

	// Baseline: what a nil-extraHeader caller sees before anything else runs.
	before, err := f.engine.snapshot(f.chain, 99, f.parent.Hash(), nil, true, nil)
	if err != nil {
		t.Fatalf("baseline snapshot failed: %v", err)
	}
	baselineFreq := bytes.Clone(before.FrequencyRLP)
	baselineInturn := before.inturnValidator()

	// A verify-shaped call stamps a forged distribution for itself.
	forged, err := f.engine.snapshot(f.chain, 99, f.parent.Hash(), nil, true, f.candidateHeader(forgedFreq))
	if err != nil {
		t.Fatalf("verify-shaped snapshot failed: %v", err)
	}
	if forged.inturnValidator() != f.valB {
		t.Fatalf("test setup broken: forged data selects %v, want %v", forged.inturnValidator(), f.valB)
	}
	if baselineInturn == f.valB {
		t.Fatal("test setup broken: baseline already selects the forged validator, so a leak would be invisible")
	}

	// The same fetch after it must be unchanged.
	after, err := f.engine.snapshot(f.chain, 99, f.parent.Hash(), nil, true, nil)
	if err != nil {
		t.Fatalf("post-verify snapshot failed: %v", err)
	}
	if !bytes.Equal(after.FrequencyRLP, baselineFreq) {
		t.Fatalf("FrequencyRLP depends on call order: %x before the verify call, %x after (COR-174)", baselineFreq, after.FrequencyRLP)
	}
	if got := after.inturnValidator(); got != baselineInturn {
		t.Fatalf("in-turn validator depends on call order: %v before the verify call, %v after (COR-174)", baselineInturn, got)
	}

	// The Snake8 callers above must not have written their stamp onto the shared entry.
	// A non-Snake8 caller has nothing to stamp, so it receives that entry itself and
	// would see any leak. Note this is not a claim that cache entries are always
	// fork-agnostic: apply() does set IsSnake8Fork/TurnLength on the snapshots it
	// builds, since the header loop depends on them. The claim is narrower — the
	// per-caller stamp in snapshot() never reaches the cache.
	plain, err := f.engine.snapshot(f.chain, 99, f.parent.Hash(), nil, false, nil)
	if err != nil {
		t.Fatalf("non-Snake8 snapshot failed: %v", err)
	}
	if plain.TurnLength != pristineTurnLength {
		t.Fatalf("non-Snake8 caller sees TurnLength %d, want %d — Snake8's 50 leaked through the cache (COR-174)", plain.TurnLength, pristineTurnLength)
	}
	if plain.IsSnake8Fork {
		t.Fatal("non-Snake8 caller sees IsSnake8Fork = true — leaked through the cache (COR-174)")
	}
}

// TestGetSnapshotRPCDoesNotWriteCache covers the ticket's second acceptance
// criterion. During the COR-37 split forensics parlia_getSnapshot returned
// identical snapshots on both sides of the split, because the RPC call itself
// re-stamped the cache from the requested block — the debugging surface lied.
func TestGetSnapshotRPCDoesNotWriteCache(t *testing.T) {
	f := newSnake8CacheFixture(t)

	// api.go keys the snapshot on the requested block itself and stamps it with that
	// block's own embedded data, so give the parent a VFQ section whose distribution
	// differs from what the cache holds. Extra must be set before seeding, since it
	// changes the header hash the cache is keyed on.
	pristineFreq := encodeFrequency(t, f.valA)
	forgedFreq := encodeFrequency(t, f.valB)
	f.parent.Extra = buildNonEpochExtra(f.genesis.Time, true, forgedFreq)
	f.chain.headers[f.parent.Hash()] = f.parent
	f.chain.head = f.parent
	f.seedCache(t, pristineFreq)

	api := &API{chain: f.chain, parlia: f.engine}
	if _, err := api.GetSnapshot(nil); err != nil {
		t.Fatalf("GetSnapshot failed: %v", err)
	}
	if _, err := api.GetSnapshotAtHash(f.parent.Hash()); err != nil {
		t.Fatalf("GetSnapshotAtHash failed: %v", err)
	}

	got, ok := f.engine.recentSnaps.Get(f.parent.Hash())
	if !ok {
		t.Fatal("cache entry for parent disappeared")
	}
	if !bytes.Equal(got.FrequencyRLP, pristineFreq) {
		t.Fatalf("parlia_getSnapshot wrote into the cache: FrequencyRLP is %x, want %x — diagnostics must be read-only (COR-174)", got.FrequencyRLP, pristineFreq)
	}
	if got.TurnLength != pristineTurnLength || got.IsSnake8Fork {
		t.Fatalf("parlia_getSnapshot wrote into the cache: TurnLength %d, IsSnake8Fork %v (COR-174)", got.TurnLength, got.IsSnake8Fork)
	}
}

// TestPrepareAfterVerifyIgnoresStampedFrequencyData is the verify → prepare
// sequence from the ticket's acceptance criteria. It is the production shape of
// the COR-37 devnet split: a verify call stamps foreign data, then the producer
// builds a block. The producer must derive both difficulty and embedded data from
// its own fresh computation, never from what the verify call left behind.
func TestPrepareAfterVerifyIgnoresStampedFrequencyData(t *testing.T) {
	f := newSnake8CacheFixture(t)

	forgedFreq := encodeFrequency(t, f.valB)
	f.seedCache(t, encodeFrequency(t, f.valA))

	// A verify-shaped call runs first, stamping a distribution that would make
	// valB in-turn — while our sealer is valC.
	if _, err := f.engine.snapshot(f.chain, 99, f.parent.Hash(), nil, true, f.candidateHeader(forgedFreq)); err != nil {
		t.Fatalf("verify-shaped snapshot failed: %v", err)
	}

	header := &types.Header{Number: big.NewInt(100), ParentHash: f.parent.Hash()}
	if err := f.engine.Prepare(f.chain, header); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// With no stake backend the fresh computation degrades to round-robin, which
	// yields empty frequency data — so the forged bytes must not appear.
	embedded, err := parseValidatorFrequencies(header, f.config)
	if err != nil {
		t.Fatalf("produced header has unparseable Snake8 extra data: %v", err)
	}
	if bytes.Equal(embedded, forgedFreq) {
		t.Fatal("Prepare embedded the frequency data left behind by the verify call instead of its own fresh computation (COR-174)")
	}
	if len(embedded) != 0 {
		t.Fatalf("expected empty embedded frequency data with no stake backend, got %x", embedded)
	}

	// The producer/verifier coherence property, on the same header: difficulty
	// must agree with what the embedded data implies.
	verifierSnap := newSnapshot(f.engine.config, f.engine.signatures, 99, f.parent.Hash(), f.validators, nil, nil, true)
	verifierSnap.TurnLength = snake8TurnLength
	verifierSnap.FrequencyRLP = embedded
	expected := diffNoTurn
	if verifierSnap.inturn(header.Coinbase) {
		expected = diffInTurn
	}
	if header.Difficulty.Cmp(expected) != 0 {
		t.Fatalf("producer/verifier difficulty mismatch: stamped %v, embedded data implies %v", header.Difficulty, expected)
	}
	if header.Difficulty.Cmp(diffInTurn) != 0 {
		t.Fatalf("expected in-turn difficulty %v for the round-robin pick, got %v", diffInTurn, header.Difficulty)
	}

	// And Prepare must not have written its fresh data into the cache either.
	got, _ := f.engine.recentSnaps.Get(f.parent.Hash())
	if !bytes.Equal(got.FrequencyRLP, encodeFrequency(t, f.valA)) {
		t.Fatalf("Prepare mutated the cached snapshot: FrequencyRLP is %x (COR-174)", got.FrequencyRLP)
	}
}
