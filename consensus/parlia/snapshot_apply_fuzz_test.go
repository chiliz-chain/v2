package parlia

import (
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// FuzzSnapshotApplyBatching (COR-215) fuzzes Snapshot.apply — the Parlia state
// machine — with signed header sequences and pins four properties that turn the
// COR-174 class of bug (shared snapshot state that depends on who applied what,
// and in which chunks) into a failing test:
//
//  1. Batching equivalence: apply(h[0:n]) == apply(h[0:i]) then apply(h[i:n]) for a
//     fuzzed split i, == one header at a time. Batched prefixes are compared along
//     the way too (all of them for short sequences, a stride for long ones — see
//     checkPrefix), not just the final state, so a divergence that later heals (a
//     prune window that realigns after a few blocks) is still caught.
//  2. Receiver immutability: the receiver of every apply() call is byte-for-byte
//     unchanged afterwards — including when the isSnake8Fork argument differs
//     from s.IsSnake8Fork, and when len(headers) == 0.
//  3. Error determinism: a batched apply that fails, fails with the same error as
//     the one-at-a-time replay, and the split replay fails on the segment that
//     contains the offending header.
//  4. Recents bound: after any successful apply the Recents window holds at most
//     minerHistoryCheckLen()+1 block entries, all within the window ending at
//     snap.Number (Bohr's epochKey markers are excluded from the count).
//
// Harness. k validator keypairs are derived deterministically from the fuzz seed.
// The genesis header (number 0, an epoch block) embeds the full validator set and a
// genesis Snapshot is built via newSnapshot. Each of the n headers chains on the
// previous one (ParentHash, Number, Time = parent+period), names a producer chosen
// by a fuzz byte mod k (round-robin when the bytes run out) and is signed with that
// producer's key via types.SealHash + crypto.Sign, exactly like the existing
// snapshot tests. Epoch blocks embed a validator set (the full set, or a fuzzed
// subset) so the validator-set switch in apply() is reachable; when Snake8 is
// active at the parent the Extra also carries the "VFQ" section a Chiliz producer
// would embed. The chain reader is prepareTestChainReader from parlia_prepare_test.go.
//
// Fork selectors. isSnake8Fork is a free argument (it is what production passes to
// apply(), derived from the parent's time — decoupling it lets property 2 cover the
// mismatch case). chainConfig.Snake8Time is separately never / genesis / mid-sequence,
// which controls the header layout. Bohr implies London+Luban (Bohr's turn-length
// parsing assumes the Luban epoch layout, so Bohr alone would be malformed) and
// switches epoch blocks to the Luban+Bohr layout with a fuzzed turnLength byte;
// off, epoch blocks use the pre-Luban layout live on Chiliz (20 bytes per validator).
//
// A recently-signed or unauthorized producer is a legitimate fuzz outcome: apply()
// must reject it deterministically, so such sequences are exercised, not filtered.
//
// Two regions are excluded from the generator until their fixes land (COR-221,
// COR-222); the inputs that exposed them are pinned as skipped reproducers in
// TestKnownSnapshotApplyDivergences.
func FuzzSnapshotApplyBatching(f *testing.F) {
	// Seeds equivalent to the existing snapshot tests. producers=nil means round-robin.
	//
	// 3-validator round-robin, no forks.
	f.Add(uint64(1), uint8(1), uint8(29), uint8(10), false, uint8(0), false, false, uint8(1), []byte(nil), []byte(nil))
	// Crosses the epoch boundary at 100 (validator-set switch at 101 with TurnLength 1).
	f.Add(uint64(2), uint8(1), uint8(119), uint8(100), false, uint8(0), false, false, uint8(1), []byte(nil), []byte(nil))
	// Snake8 on from genesis: switch at 99 and 199, epoch boundary at 100.
	f.Add(uint64(3), uint8(1), uint8(199), uint8(99), true, uint8(1), false, false, uint8(1), []byte(nil), []byte(nil))
	// Snake8 activates mid-sequence (headers past ~100 carry the VFQ section).
	f.Add(uint64(4), uint8(1), uint8(199), uint8(150), true, uint8(100), false, false, uint8(1), []byte(nil), []byte(nil))
	// Bohr/Luban layout, 5 validators, 200-block epoch.
	f.Add(uint64(5), uint8(3), uint8(199), uint8(50), false, uint8(0), true, true, uint8(1), []byte(nil), []byte(nil))
	// Validator-set shrink: mask 0x03 drops validator 2 from the block-100 epoch
	// header, so the switch at 101 installs a smaller set and the next round-robin
	// turn is unauthorized. Without a seed carrying a non-nil epochMasks the
	// validator-set switch is dead code under a plain `go test` run, which only
	// replays the seed corpus.
	//
	// kSel=2 (k=4), not 1 (k=3): the switch itself is already covered by seed #0
	// (snapshot.go:487 count 1), and what is uncovered is the *prune* arithmetic
	// COR-222 lives in. With k=3 the mask leaves 2 validators, oldLimit and
	// newLimit are both 2, `newLimit < oldLimit` at snapshot.go:481 is false and
	// the delete loop never runs. k=4 keeping 2 gives oldLimit 3, newLimit 2 —
	// snapshot.go:481 and :482 go from count 0 to 1. Without this, CI will not
	// execute the lines the COR-222 fix changes and a later regression there
	// passes green.
	f.Add(uint64(6), uint8(2), uint8(199), uint8(120), false, uint8(0), false, false, uint8(1), []byte(nil), []byte{0x03})
	// Same shrink under Bohr, which clears Recents at the switch instead of
	// pruning, so it is unaffected by the kSel choice above.
	f.Add(uint64(7), uint8(1), uint8(199), uint8(120), false, uint8(0), true, false, uint8(1), []byte(nil), []byte{0x03})
	// A Bohr checkpoint that actually CHANGES the turn length. Every other seed
	// passes turnLength=1, which equals the value already in the snapshot, so
	// `snap.TurnLength = *turnLength` is a no-op, minerHistoryCheckLen never
	// grows, and the epochKey double-switch guard at snapshot.go:437 is
	// unreachable — count 0 across the whole corpus, for the one mechanism
	// COR-221 is about. TurnLength jumps 1->3 at the block-1 switch and switches
	// again at block 5 inside the same epoch, which puts snapshot.go:437 at
	// count 1. bohr=true with snake8Arg=false keeps it clear of the COR-221
	// exclusion, which pins turnLength to snake8TurnLength.
	f.Add(uint64(5), uint8(1), uint8(199), uint8(50), false, uint8(0), true, false, uint8(3), []byte(nil), []byte(nil))

	f.Fuzz(func(t *testing.T, seed uint64, kSel, nSel, splitSel uint8, snake8Arg bool, snake8At uint8, bohr, longEpoch bool, turnLength uint8, producers, epochMasks []byte) {
		// Two regions are excluded until their fixes land; each is pinned verbatim
		// as a skipped reproducer in TestKnownSnapshotApplyDivergences.
		//
		// TODO(COR-221): under Bohr, a checkpoint's turnLength leaks into the rest
		// of a batch because apply re-asserts the Snake8 turn length only after
		// the loop. What diverges is the *mismatch* between the checkpoint byte and
		// snake8TurnLength, so pinning the byte to snake8TurnLength — rather than
		// switching Bohr off — keeps the whole Bohr+Snake8 plane reachable (Bohr's
		// SignRecently path, its Recents clearing and epochKey guard, at TurnLength
		// 50) and collapses only the turnLength dimension. Drop this line once the
		// turn length is re-asserted inside the loop.
		if bohr && snake8Arg {
			turnLength = snake8TurnLength
		}
		// TODO(COR-222): the pre-Bohr shrink pruning deletes oldLimit-newLimit
		// Recents entries at a one-block scale, which is wrong once TurnLength is 50,
		// so it leaves stale entries behind. Only the isSnake8Fork *argument* can
		// raise TurnLength above 1 on that path (a Bohr checkpoint's turnLength
		// reaches only the branch that clears Recents outright), so the exclusion is
		// keyed on that argument alone: a Snake8 schedule in the config with
		// isSnake8Fork=false leaves TurnLength at 1 and must keep exercising
		// validator-set churn. Drop this line once the pruning is TurnLength-aware.
		if snake8Arg {
			epochMasks = nil
		}
		runSnapshotApplyBatchingCase(t, seed, kSel, nSel, splitSel, snake8Arg, snake8At, bohr, longEpoch, turnLength, producers, epochMasks)
	})
}

// TestKnownSnapshotApplyDivergences replays, verbatim, the two inputs
// FuzzSnapshotApplyBatching found on develop. Both are real defects in
// Snapshot.apply, tracked separately; the fix PRs un-skip their case and remove
// the matching generator exclusion above.
func TestKnownSnapshotApplyDivergences(t *testing.T) {
	cases := []struct {
		name, issue string
		run         func(t *testing.T)
	}{
		{
			// k=5, n=200, split 50, isSnake8Fork=true, Snake8 never scheduled in the
			// config, Bohr on, epoch 200, checkpoint turnLength 1. At the switch (block
			// 149) apply installs turnLength 1 for the rest of the batch while the
			// one-at-a-time replay re-stamps 50 after every header: the prune windows
			// and SignRecently threshold diverge. Latent: no Chiliz network schedules Bohr.
			name: "bohr checkpoint turnLength leaks into the batch", issue: "COR-221",
			run: func(t *testing.T) {
				runSnapshotApplyBatchingCase(t, 5, 3, 199, '2', true, 0, true, true, 1, nil, nil)
			},
		},
		{
			// k=2, n=200, split 150, Snake8 active from header 100, epoch 100, mask
			// 0x31 drops validator 1 at block 100. At the switch (block 199) the
			// pre-Bohr code deletes oldLimit-newLimit = 1 Recents entry although the
			// window shrank by 50, so ~50 stale entries outlive the per-block prune.
			name: "recents leak after set shrink under Snake8", issue: "COR-222",
			run: func(t *testing.T) {
				runSnapshotApplyBatchingCase(t, 4, 0, 199, 0x96, true, 'd', false, false, 1, nil, []byte("1"))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Skip(tc.issue + ": known failure, un-skip in the fix PR")
			tc.run(t)
		})
	}
}

// runSnapshotApplyBatchingCase is the body of FuzzSnapshotApplyBatching for one
// input, shared with TestKnownSnapshotApplyDivergences.
func runSnapshotApplyBatchingCase(t *testing.T, seed uint64, kSel, nSel, splitSel uint8, snake8Arg bool, snake8At uint8, bohr, longEpoch bool, turnLength uint8, producers, epochMasks []byte) {
	k := int(kSel)%6 + 2   // 2..7 validators
	n := int(nSel)%200 + 1 // 1..200 headers
	split := int(splitSel) % (n + 1)
	epoch := uint64(100)
	if longEpoch {
		epoch = 200
	}
	if turnLength == 0 {
		// verifyTurnLength only admits a header byte equal to the staking contract's
		// value, so a zero never reaches apply(); with it minerHistoryCheckLen()
		// underflows and every window computation is meaningless.
		turnLength = 1
	}

	fx := newApplyFuzzFixture(seed, k, n, epoch, snake8At, bohr, turnLength, producers, epochMasks)
	s0 := fx.genesisSnapshot()
	h := fx.headers

	// Structural rejects: the sanity checks must fail without touching the receiver.
	// The generator always chains correctly, so a malformed batch has to be built
	// here — two cases for the checks of headers[0] against the receiver, and two for
	// the pairwise loop over the batch itself, which no other input can reach.
	if n >= 2 {
		fx.mustRejectUnchanged(t, s0, h[1:], snake8Arg, errOutOfRangeChain, "number gap vs receiver")
		unchained := types.CopyHeader(h[1])
		unchained.ParentHash = common.HexToHash("0xdead")
		fx.mustRejectUnchanged(t, s0, []*types.Header{h[0], unchained}, snake8Arg, errBlockHashInconsistent, "parent hash break inside the batch")
	}
	if n >= 3 {
		fx.mustRejectUnchanged(t, s0, []*types.Header{h[0], h[2]}, snake8Arg, errOutOfRangeChain, "number gap inside the batch")
	}
	detached := types.CopyHeader(h[0])
	detached.ParentHash = common.HexToHash("0xdead")
	fx.mustRejectUnchanged(t, s0, []*types.Header{detached}, snake8Arg, errBlockHashInconsistent, "parent hash mismatch vs receiver")

	// Property 2 on the empty path, with whatever arg/receiver mismatch the fuzz chose.
	before := snapshotFingerprint(s0)
	if _, err := s0.apply(nil, fx.chain, nil, fx.config, snake8Arg); err != nil {
		t.Fatalf("apply(no headers) returned %v", err)
	}
	if after := snapshotFingerprint(s0); after != before {
		t.Fatalf("apply(no headers, isSnake8Fork=%v) mutated its receiver (IsSnake8Fork=%v):\n--- before\n%s\n--- after\n%s", snake8Arg, s0.IsSnake8Fork, before, after)
	}

	// Reference: one header at a time. incremental[j] is the state after h[0..j].
	incremental := make([]*Snapshot, 0, n)
	failAt, failErr := -1, error(nil)
	cur := s0
	for j := 0; j < n; j++ {
		next, err := fx.applyChecked(t, cur, h[j:j+1], snake8Arg, fmt.Sprintf("incremental step %d", j))
		if err != nil {
			failAt, failErr = j, err
			break
		}
		incremental = append(incremental, next)
		cur = next
	}

	// Property 1 + 3 against batched prefixes: apply(h[0:j+1]) from genesis. Every
	// prefix when n is small; a stride when n is large (each prefix costs O(j)
	// header hashes, so all of them would be O(n^2) and starve the fuzzer), always
	// including the split point, the end and the failing header.
	for j := 0; j < n; j++ {
		if !fx.checkPrefix(j, n, split, failAt) {
			continue
		}
		batch, err := fx.applyChecked(t, s0, h[:j+1], snake8Arg, fmt.Sprintf("batch prefix [0:%d]", j+1))
		switch {
		case failAt >= 0 && j >= failAt:
			if err == nil {
				t.Fatalf("batch apply(h[0:%d]) succeeded but one-at-a-time replay failed at header %d (number %d, producer %s): %v",
					j+1, failAt, h[failAt].Number.Uint64(), h[failAt].Coinbase, failErr)
			}
			if err.Error() != failErr.Error() {
				t.Fatalf("batch apply(h[0:%d]) failed with %q, one-at-a-time replay failed at header %d with %q", j+1, err, failAt, failErr)
			}
		case err != nil:
			t.Fatalf("batch apply(h[0:%d]) failed with %v but one-at-a-time replay succeeded through header %d", j+1, err, j)
		default:
			if got, want := snapshotFingerprint(batch), snapshotFingerprint(incremental[j]); got != want {
				t.Fatalf("batching divergence at prefix [0:%d] (k=%d, isSnake8Fork=%v, snake8At=%d, bohr=%v, epoch=%d, turnLength=%d):\n--- batch\n%s\n--- one at a time\n%s",
					j+1, k, snake8Arg, snake8At, bohr, epoch, turnLength, got, want)
			}
		}
	}

	// Property 1 + 3 for the fuzzed split: apply(h[0:i]) then apply(h[i:n]).
	first, err := fx.applyChecked(t, s0, h[:split], snake8Arg, fmt.Sprintf("split first segment [0:%d]", split))
	if err != nil {
		if failAt < 0 || failAt >= split {
			t.Fatalf("split first segment [0:%d] failed with %v, but one-at-a-time replay failed at %d", split, err, failAt)
		}
		if err.Error() != failErr.Error() {
			t.Fatalf("split first segment [0:%d] failed with %q, one-at-a-time replay failed with %q", split, err, failErr)
		}
		return
	}
	if failAt >= 0 && failAt < split {
		t.Fatalf("split first segment [0:%d] succeeded but one-at-a-time replay failed at header %d: %v", split, failAt, failErr)
	}
	second, err := fx.applyChecked(t, first, h[split:], snake8Arg, fmt.Sprintf("split second segment [%d:%d]", split, n))
	if err != nil {
		if failAt < 0 {
			t.Fatalf("split second segment [%d:%d] failed with %v but one-at-a-time replay succeeded", split, n, err)
		}
		if err.Error() != failErr.Error() {
			t.Fatalf("split second segment [%d:%d] failed with %q, one-at-a-time replay failed at %d with %q", split, n, err, failAt, failErr)
		}
		return
	}
	if failAt >= 0 {
		t.Fatalf("split apply [0:%d]+[%d:%d] succeeded but one-at-a-time replay failed at header %d: %v", split, split, n, failAt, failErr)
	}
	if got, want := snapshotFingerprint(second), snapshotFingerprint(incremental[n-1]); got != want {
		t.Fatalf("batching divergence for split [0:%d]+[%d:%d] (k=%d, isSnake8Fork=%v, snake8At=%d, bohr=%v, epoch=%d, turnLength=%d):\n--- split\n%s\n--- one at a time\n%s",
			split, split, n, k, snake8Arg, snake8At, bohr, epoch, turnLength, got, want)
	}

	// Property 5: the checkpoint resolved out of candidateParents instead of the
	// chain DB must give the same snapshot.
	//
	// Every arm above passes parents == nil and the fixture pre-inserts every
	// header into the chain reader, so chain.GetHeader always succeeds and
	// FindAncientHeader's candidateParents branch (snapshot.go:782-791) and the
	// ErrUnknownAncestor return (snapshot.go:443-445) were never reached. That is
	// the production shape this harness models: parlia.go:1123 passes a non-nil
	// parents when verifyCascadingFields verifies a batch of headers that are not
	// in the chain DB yet, and there FindAncientHeader *must* resolve the
	// checkpoint out of parents. A batching bug that only shows up on that path
	// would have gone unseen.
	withheld := fx.chainWithout(h)
	viaParents, err := s0.apply(h, withheld, h, fx.config, snake8Arg)
	if err != nil {
		t.Fatalf("apply(h[0:%d]) with the batch withheld from the chain DB and passed as parents failed with %v, but the same batch against the full chain succeeded", n, err)
	}
	if got, want := snapshotFingerprint(viaParents), snapshotFingerprint(incremental[n-1]); got != want {
		t.Fatalf("checkpoint resolved via candidateParents diverges from the chain-DB path (k=%d, isSnake8Fork=%v, snake8At=%d, bohr=%v, epoch=%d, turnLength=%d):\n--- via parents\n%s\n--- one at a time\n%s",
			k, snake8Arg, snake8At, bohr, epoch, turnLength, got, want)
	}
	// With neither the chain DB nor parents, apply must either resolve without a
	// checkpoint walk or report the missing ancestor — never anything else. Which
	// of the two depends on where the batch sits relative to the epoch, so the
	// assertion is on the error's identity, not on whether one occurs.
	if _, err := s0.apply(h, withheld, nil, fx.config, snake8Arg); err != nil && !errors.Is(err, consensus.ErrUnknownAncestor) {
		t.Fatalf("apply(h[0:%d]) with the checkpoint in neither the chain DB nor parents failed with %v, want nil or ErrUnknownAncestor", n, err)
	}
}

// chainWithout returns a chain reader with the given headers removed, so apply()
// has to resolve the checkpoint out of its candidateParents argument.
func (fx *applyFuzzFixture) chainWithout(headers []*types.Header) *prepareTestChainReader {
	sub := make(map[common.Hash]*types.Header, len(fx.chain.headers))
	for k, v := range fx.chain.headers {
		sub[k] = v
	}
	for _, h := range headers {
		delete(sub, h.Hash())
	}
	return &prepareTestChainReader{config: fx.chain.config, genesis: fx.chain.genesis, headers: sub}
}

// applyFuzzFixture holds one generated chain: keys, headers, chain reader and config.
type applyFuzzFixture struct {
	config     *params.ChainConfig
	chain      *prepareTestChainReader
	sigCache   *lru.Cache[common.Hash, common.Address]
	validators []common.Address
	voteAddrs  []types.BLSPublicKey
	genesis    *types.Header
	headers    []*types.Header
	epoch      uint64
}

const (
	applyFuzzGenesisTime = uint64(1_700_000_000)
	applyFuzzPeriod      = uint64(3)
)

func newApplyFuzzFixture(seed uint64, k, n int, epoch uint64, snake8At uint8, bohr bool, turnLength uint8, producers, epochMasks []byte) *applyFuzzFixture {
	keys := make([]*ecdsa.PrivateKey, k)
	validators := make([]common.Address, k)
	voteAddrs := make([]types.BLSPublicKey, k)
	for i := range keys {
		keys[i] = deriveFuzzKey(seed, i)
		validators[i] = crypto.PubkeyToAddress(keys[i].PublicKey)
		copy(voteAddrs[i][:], crypto.Keccak256(fuzzTag(seed, i, "bls-a"), fuzzTag(seed, i, "bls-b")))
	}

	config := &params.ChainConfig{
		ChainID: big.NewInt(88883),
		Parlia:  &params.ParliaConfig{Period: applyFuzzPeriod, Epoch: epoch},
	}
	if bohr {
		// Bohr's turn-length parsing assumes the Luban epoch layout and IsBohr
		// requires London, so the three come together (as on BSC).
		config.LondonBlock = common.Big0
		config.LubanBlock = common.Big0
		config.BohrTime = new(uint64)
	}
	switch {
	case snake8At == 0:
		// never
	case snake8At == 1:
		config.Snake8Time = new(uint64) // from genesis
	default:
		at := applyFuzzGenesisTime + uint64(int(snake8At)%n)*applyFuzzPeriod
		config.Snake8Time = &at
	}

	fx := &applyFuzzFixture{
		config:     config,
		sigCache:   lru.NewCache[common.Hash, common.Address](2 * n),
		validators: validators,
		voteAddrs:  voteAddrs,
		epoch:      epoch,
	}

	// Genesis: an epoch block embedding the full set, never signed (apply() never
	// ecrecovers it; it is only ever a checkpoint for FindAncientHeader).
	fx.genesis = &types.Header{
		Number:     common.Big0,
		Time:       applyFuzzGenesisTime,
		Difficulty: big.NewInt(1),
		Extra:      fx.buildExtra(0, validators, 0xff, turnLength, false, 0),
	}
	headers := map[common.Hash]*types.Header{fx.genesis.Hash(): fx.genesis}
	fx.chain = &prepareTestChainReader{config: config, genesis: fx.genesis, headers: headers}

	parent := fx.genesis
	fx.headers = make([]*types.Header, n)
	epochIdx := 0
	for j := 0; j < n; j++ {
		number := uint64(j + 1)
		producer := j % k
		if j < len(producers) {
			producer = int(producers[j]) % k
		}
		var embedded []common.Address
		mask := byte(0xff)
		if number%epoch == 0 {
			if epochIdx < len(epochMasks) {
				mask = epochMasks[epochIdx]
			}
			epochIdx++
			embedded = validators
		}
		h := &types.Header{
			Number:     new(big.Int).SetUint64(number),
			ParentHash: parent.Hash(),
			Time:       parent.Time + applyFuzzPeriod,
			Coinbase:   validators[producer],
			Difficulty: diffInTurn,
			Extra:      fx.buildExtra(number, embedded, mask, turnLength, config.IsSnake8(parent.Time), parent.Time),
		}
		binary.LittleEndian.PutUint32(h.Extra[extraVanity-nextForkHashSize:extraVanity], uint32(producer))
		sig, err := crypto.Sign(types.SealHash(h, config.ChainID).Bytes(), keys[producer])
		if err != nil {
			panic(fmt.Sprintf("signing header %d: %v", number, err))
		}
		copy(h.Extra[len(h.Extra)-extraSeal:], sig)
		headers[h.Hash()] = h
		fx.headers[j] = h
		parent = h
	}
	return fx
}

// buildExtra lays out header.Extra:
//
//	|--vanity--|--validators (epoch blocks only)--|--VFQ|parentTS|freq (Snake8 at parent)--|--seal--|
//
// Pre-Luban: 20 bytes per validator, the layout live on Chiliz mainnet/Spicy.
// Luban+Bohr: count byte, 68 bytes per validator, turnLength byte. mask bit i clear
// drops validator i from the embedded set (the full set is kept if that would leave
// none), so validator-set switches are observable and can shrink the set.
func (fx *applyFuzzFixture) buildExtra(number uint64, validators []common.Address, mask byte, turnLength uint8, snake8 bool, parentTS uint64) []byte {
	extra := make([]byte, extraVanity)
	if len(validators) > 0 {
		var set []int
		for i := range validators {
			if mask&(1<<(i%8)) != 0 {
				set = append(set, i)
			}
		}
		if len(set) == 0 {
			for i := range validators {
				set = append(set, i)
			}
		}
		if fx.config.IsLuban(new(big.Int).SetUint64(number)) {
			extra = append(extra, byte(len(set)))
			for _, i := range set {
				extra = append(extra, validators[i].Bytes()...)
				extra = append(extra, fx.voteAddrs[i].Bytes()...)
			}
			if fx.config.IsBohr(new(big.Int).SetUint64(number), applyFuzzGenesisTime+number*applyFuzzPeriod) {
				extra = append(extra, turnLength)
			}
		} else {
			for _, i := range set {
				extra = append(extra, validators[i].Bytes()...)
			}
		}
	}
	if snake8 {
		extra = append(extra, validatorFrequencyDataPrefix...)
		ts := make([]byte, 8)
		binary.LittleEndian.PutUint64(ts, parentTS)
		extra = append(extra, ts...)
		extra = append(extra, 0xc0) // empty frequency list: no stake backend, round-robin
	}
	return append(extra, make([]byte, extraSeal)...)
}

// genesisSnapshot mirrors what Parlia.snapshot() builds at the genesis hash. The
// epoch length is set explicitly because newSnapshot reads the package-level
// defaultEpochLength, which Parlia.New() overwrites from whatever config the
// previous test in the package used.
func (fx *applyFuzzFixture) genesisSnapshot() *Snapshot {
	var voteAddrs []types.BLSPublicKey
	if fx.config.IsLuban(common.Big0) {
		voteAddrs = fx.voteAddrs
	}
	snap := newSnapshot(fx.config.Parlia, fx.sigCache, 0, fx.genesis.Hash(), fx.validators, voteAddrs, nil, fx.config.IsSnake8(applyFuzzGenesisTime))
	snap.EpochLength = fx.epoch
	return snap
}

// checkPrefix says whether the batched prefix h[0:j+1] is compared against the
// one-at-a-time reference: all of them up to 32 headers, every n/32-th beyond, plus
// the split boundary, the last header and the header the reference failed on (and
// the one before it, which must still succeed).
func (fx *applyFuzzFixture) checkPrefix(j, n, split, failAt int) bool {
	stride := n / 32
	if stride < 1 {
		stride = 1
	}
	return j%stride == 0 || j == n-1 || j == split-1 || j == failAt || j == failAt-1
}

// applyChecked runs one apply() and asserts properties 2 and 4 around it: the
// receiver is unchanged afterwards, and a successful result respects the Recents
// window. The apply result and error are returned for the caller's own checks.
func (fx *applyFuzzFixture) applyChecked(t *testing.T, s *Snapshot, headers []*types.Header, isSnake8Fork bool, what string) (*Snapshot, error) {
	t.Helper()
	before := snapshotFingerprint(s)
	got, err := s.apply(headers, fx.chain, nil, fx.config, isSnake8Fork)
	if after := snapshotFingerprint(s); after != before {
		t.Fatalf("%s: apply(%d headers, isSnake8Fork=%v) mutated its receiver (Number=%d, IsSnake8Fork=%v) — shared LRU state is not safe (COR-174):\n--- before\n%s\n--- after\n%s",
			what, len(headers), isSnake8Fork, s.Number, s.IsSnake8Fork, before, after)
	}
	if err != nil {
		return nil, err
	}
	if got == nil {
		t.Fatalf("%s: apply returned nil snapshot and nil error", what)
	}
	if len(headers) > 0 && got.Number != s.Number+uint64(len(headers)) {
		t.Fatalf("%s: apply advanced Number from %d to %d for %d headers", what, s.Number, got.Number, len(headers))
	}
	assertRecentsBound(t, got, what)
	return got, nil
}

// mustRejectUnchanged asserts apply() fails with the given sentinel and leaves the
// receiver untouched.
func (fx *applyFuzzFixture) mustRejectUnchanged(t *testing.T, s *Snapshot, headers []*types.Header, isSnake8Fork bool, want error, what string) {
	t.Helper()
	got, err := fx.applyChecked(t, s, headers, isSnake8Fork, what)
	if err == nil {
		t.Fatalf("%s: apply accepted a structurally invalid batch (result Number=%d), want %v", what, got.Number, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s: apply failed with %v, want %v", what, err, want)
	}
}

// assertRecentsBound is property 4: at most minerHistoryCheckLen()+1 block entries
// in Recents, all inside the window (Number-limit, Number]. Bohr's epochKey markers
// (keys far above any block number) are skipped.
func assertRecentsBound(t *testing.T, snap *Snapshot, what string) {
	t.Helper()
	limit := snap.minerHistoryCheckLen() + 1
	count := uint64(0)
	stale := []uint64{}
	for number := range snap.Recents {
		if number > snap.Number {
			continue // epochKey marker
		}
		count++
		if number+limit <= snap.Number {
			stale = append(stale, number)
		}
	}
	if len(stale) > 0 {
		sort.Slice(stale, func(i, j int) bool { return stale[i] < stale[j] })
		t.Fatalf("%s: Recents holds %d block(s) outside the window (%d, %d], oldest %d (TurnLength=%d, validators=%d)",
			what, len(stale), snap.Number-limit, snap.Number, stale[0], snap.TurnLength, len(snap.Validators))
	}
	if count > limit {
		t.Fatalf("%s: Recents holds %d block entries, want at most minerHistoryCheckLen()+1 = %d (TurnLength=%d, validators=%d)",
			what, count, limit, snap.TurnLength, len(snap.Validators))
	}
}

// snapshotFingerprint is the canonical serialization the properties compare: every
// consensus-relevant field, maps emitted in sorted key order, nil and empty maps
// rendering identically. It deliberately covers the unexported config/ethAPI/sigCache
// pointers too, since apply() must carry them over unchanged.
func snapshotFingerprint(s *Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "number=%d hash=%s epoch=%d interval=%d turn=%d snake8=%v\n", s.Number, s.Hash.Hex(), s.EpochLength, s.BlockInterval, s.TurnLength, s.IsSnake8Fork)
	fmt.Fprintf(&b, "config=%p ethAPI=%p sigCache=%p\n", s.config, s.ethAPI, s.sigCache)
	fmt.Fprintf(&b, "frequencyRLP=%x\n", s.FrequencyRLP)
	if s.Attestation == nil {
		b.WriteString("attestation=nil\n")
	} else {
		fmt.Fprintf(&b, "attestation=%d/%s->%d/%s\n", s.Attestation.SourceNumber, s.Attestation.SourceHash.Hex(), s.Attestation.TargetNumber, s.Attestation.TargetHash.Hex())
	}
	for _, v := range s.validators() {
		info := s.Validators[v]
		fmt.Fprintf(&b, "validator %x index=%d vote=%x\n", v, info.Index, info.VoteAddress[:])
	}
	recents := make([]uint64, 0, len(s.Recents))
	for number := range s.Recents {
		recents = append(recents, number)
	}
	sort.Slice(recents, func(i, j int) bool { return recents[i] < recents[j] })
	for _, number := range recents {
		fmt.Fprintf(&b, "recent %d %x\n", number, s.Recents[number])
	}
	forkHashes := make([]uint64, 0, len(s.RecentForkHashes))
	for number := range s.RecentForkHashes {
		forkHashes = append(forkHashes, number)
	}
	sort.Slice(forkHashes, func(i, j int) bool { return forkHashes[i] < forkHashes[j] })
	for _, number := range forkHashes {
		fmt.Fprintf(&b, "forkhash %d %s\n", number, s.RecentForkHashes[number])
	}
	return b.String()
}

// deriveFuzzKey derives validator i's key from the fuzz seed. Keccak output is a
// valid secp256k1 scalar with overwhelming probability; re-hash on the off chance.
func deriveFuzzKey(seed uint64, i int) *ecdsa.PrivateKey {
	material := fuzzTag(seed, i, "key")
	for {
		key, err := crypto.ToECDSA(material)
		if err == nil {
			return key
		}
		material = crypto.Keccak256(material)
	}
}

func fuzzTag(seed uint64, i int, tag string) []byte {
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], seed)
	binary.LittleEndian.PutUint64(buf[8:], uint64(i))
	return crypto.Keccak256(buf[:], []byte(tag))
}
