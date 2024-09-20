package parlia

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	mrand "math/rand"
	"testing"
	"time"

	"golang.org/x/crypto/sha3"

	"github.com/ethereum/go-ethereum/common"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	upperLimitOfVoteBlockNumber = 11
)

func TestImpactOfValidatorOutOfService(t *testing.T) {
	testCases := []struct {
		totalValidators int
		downValidators  int
		turnLength      int
	}{
		{3, 1, 1},
		{5, 2, 1},
		{10, 1, 2},
		{10, 4, 2},
		{21, 1, 3},
		{21, 3, 3},
		{21, 5, 4},
		{21, 10, 5},
	}
	for _, tc := range testCases {
		simulateValidatorOutOfService(tc.totalValidators, tc.downValidators, tc.turnLength)
	}
}

// refer Snapshot.SignRecently
func signRecently(idx int, recents map[uint64]int, turnLength int) bool {
	recentSignTimes := 0
	for _, signIdx := range recents {
		if signIdx == idx {
			recentSignTimes += 1
		}
	}
	return recentSignTimes >= turnLength
}

// refer Snapshot.minerHistoryCheckLen
func minerHistoryCheckLen(totalValidators int, turnLength int) uint64 {
	return uint64(totalValidators/2+1)*uint64(turnLength) - 1
}

// refer Snapshot.inturnValidator
func inturnValidator(totalValidators int, turnLength int, height int) int {
	return height / turnLength % totalValidators
}

func simulateValidatorOutOfService(totalValidators int, downValidators int, turnLength int) {
	downBlocks := 10000
	recoverBlocks := 10000
	recents := make(map[uint64]int)

	validators := make(map[int]bool, totalValidators)
	down := make([]int, totalValidators)
	for i := 0; i < totalValidators; i++ {
		validators[i] = true
		down[i] = i
	}
	mrand.Shuffle(totalValidators, func(i, j int) {
		down[i], down[j] = down[j], down[i]
	})
	for i := 0; i < downValidators; i++ {
		delete(validators, down[i])
	}
	isRecentSign := func(idx int) bool {
		return signRecently(idx, recents, turnLength)
	}
	isInService := func(idx int) bool {
		return validators[idx]
	}

	downDelay := uint64(0)
	for h := 1; h <= downBlocks; h++ {
		if limit := minerHistoryCheckLen(totalValidators, turnLength) + 1; uint64(h) >= limit {
			delete(recents, uint64(h)-limit)
		}
		proposer := inturnValidator(totalValidators, turnLength, h)
		if !isInService(proposer) || isRecentSign(proposer) {
			candidates := make(map[int]bool, totalValidators/2)
			for v := range validators {
				if !isRecentSign(v) {
					candidates[v] = true
				}
			}
			if len(candidates) == 0 {
				panic("can not test such case")
			}
			idx, delay := producerBlockDelay(candidates, h, totalValidators)
			downDelay = downDelay + delay
			recents[uint64(h)] = idx
		} else {
			recents[uint64(h)] = proposer
		}
	}
	fmt.Printf("average delay is %v  when there is %d validators and %d is down \n",
		downDelay/uint64(downBlocks), totalValidators, downValidators)

	for i := 0; i < downValidators; i++ {
		validators[down[i]] = true
	}

	recoverDelay := uint64(0)
	lastseen := downBlocks
	for h := downBlocks + 1; h <= downBlocks+recoverBlocks; h++ {
		if limit := minerHistoryCheckLen(totalValidators, turnLength) + 1; uint64(h) >= limit {
			delete(recents, uint64(h)-limit)
		}
		proposer := inturnValidator(totalValidators, turnLength, h)
		if !isInService(proposer) || isRecentSign(proposer) {
			lastseen = h
			candidates := make(map[int]bool, totalValidators/2)
			for v := range validators {
				if !isRecentSign(v) {
					candidates[v] = true
				}
			}
			if len(candidates) == 0 {
				panic("can not test such case")
			}
			idx, delay := producerBlockDelay(candidates, h, totalValidators)
			recoverDelay = recoverDelay + delay
			recents[uint64(h)] = idx
		} else {
			recents[uint64(h)] = proposer
		}
	}
	fmt.Printf("total delay is %v after recover when there is %d validators down ever, last seen not proposer at height %d\n",
		recoverDelay, downValidators, lastseen)
}

func producerBlockDelay(candidates map[int]bool, height, numOfValidators int) (int, uint64) {
	s := mrand.NewSource(int64(height))
	r := mrand.New(s)
	n := numOfValidators
	backOffSteps := make([]int, 0, n)
	for idx := 0; idx < n; idx++ {
		backOffSteps = append(backOffSteps, idx)
	}
	r.Shuffle(n, func(i, j int) {
		backOffSteps[i], backOffSteps[j] = backOffSteps[j], backOffSteps[i]
	})
	minDelay := numOfValidators
	minCandidate := 0
	for c := range candidates {
		if minDelay > backOffSteps[c] {
			minDelay = backOffSteps[c]
			minCandidate = c
		}
	}
	delay := initialBackOffTime + uint64(minDelay)*wiggleTime
	return minCandidate, delay
}

func randomAddress() common.Address {
	addrBytes := make([]byte, 20)
	rand.Read(addrBytes)
	return common.BytesToAddress(addrBytes)
}

// =========================================================================
// =======     Simulator P2P network to verify fast finality    ============
// =========================================================================

type MockBlock struct {
	parent *MockBlock

	blockNumber uint64
	blockHash   common.Hash
	coinbase    *MockValidator
	td          uint64 // Total difficulty from genesis block to current block
	attestation uint64 // Vote attestation for parent block, zero means no attestation
}

var GenesisBlock = &MockBlock{
	parent:      nil,
	blockNumber: 0,
	blockHash:   common.Hash{},
	coinbase:    nil,
	td:          diffInTurn.Uint64(),
	attestation: 0,
}

func (b *MockBlock) Hash() (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	rlp.Encode(hasher, []interface{}{
		b.parent,
		b.blockNumber,
		b.coinbase,
		b.td,
		b.attestation,
	})
	hasher.Sum(hash[:0])
	return hash
}

func (b *MockBlock) IsConflicted(a *MockBlock) bool {
	if a.blockNumber > b.blockNumber {
		p := a.parent
		for ; p.blockNumber > b.blockNumber; p = p.parent {
		}

		return p.blockHash != b.blockHash
	}

	if a.blockNumber < b.blockNumber {
		p := b.parent
		for ; p.blockNumber > a.blockNumber; p = p.parent {
		}

		return p.blockHash != a.blockHash
	}

	return a.blockHash != b.blockHash
}

// GetJustifiedNumberAndHash returns number and hash of the highest justified block,
// keep same func signature with consensus even if `error` will be nil definitely
func (b *MockBlock) GetJustifiedNumberAndHash() (uint64, common.Hash, error) {
	justifiedBlock := GenesisBlock
	for curBlock := b; curBlock.blockNumber > 1; curBlock = curBlock.parent {
		// justified
		if curBlock.attestation != 0 {
			justifiedBlock = curBlock.parent
			break
		}
	}

	return justifiedBlock.blockNumber, justifiedBlock.blockHash, nil
}

func (b *MockBlock) GetJustifiedNumber() uint64 {
	justifiedBlockNumber, _, _ := b.GetJustifiedNumberAndHash()
	return justifiedBlockNumber
}

// GetFinalizedBlock returns highest finalized block,
// include current block's attestation.
func (b *MockBlock) GetFinalizedBlock() *MockBlock {
	if b.blockNumber < 3 {
		return GenesisBlock
	}

	if b.attestation != 0 && b.parent.attestation != 0 {
		return b.parent.parent
	}

	return b.parent.GetFinalizedBlock()
}

type MockValidator struct {
	index        int
	validatorSet int // validators number
	head         *MockBlock
	voteRecords  map[uint64]*types.VoteData
}

func NewMockValidator(index int, validatorSet int) *MockValidator {
	v := &MockValidator{
		index:        index,
		validatorSet: validatorSet,
		head:         GenesisBlock,
		voteRecords:  make(map[uint64]*types.VoteData),
	}
	return v
}

func (v *MockValidator) SignRecently() bool {
	parent := v.head
	for i := 0; i < v.validatorSet*1/2; i++ {
		if parent.blockNumber == 0 {
			return false
		}

		if parent.coinbase == v {
			return true
		}

		parent = parent.parent
	}

	return false
}

func (v *MockValidator) Produce(attestation uint64) (*MockBlock, error) {
	if v.SignRecently() {
		return nil, fmt.Errorf("v %d sign recently", v.index)
	}

	block := &MockBlock{
		parent:      v.head,
		blockNumber: v.head.blockNumber + 1,
		coinbase:    v,
		td:          v.head.td + 1,
		attestation: attestation,
	}

	if (block.blockNumber-1)%uint64(v.validatorSet) == uint64(v.index) {
		block.td = v.head.td + 2
	}

	block.blockHash = block.Hash()
	return block, nil
}

func (v *MockValidator) Vote(block *MockBlock) bool {
	// Rule 3: The block should be the latest block of canonical chain
	if block != v.head {
		return false
	}

	// Rule 1: No double vote
	if _, ok := v.voteRecords[block.blockNumber]; ok {
		return false
	}

	// Rule 2: No surround vote
	justifiedBlockNumber, justifiedBlockHash, _ := block.GetJustifiedNumberAndHash()
	for targetNumber := justifiedBlockNumber + 1; targetNumber < block.blockNumber; targetNumber++ {
		if vote, ok := v.voteRecords[targetNumber]; ok {
			if vote.SourceNumber > justifiedBlockNumber {
				return false
			}
		}
	}
	for targetNumber := block.blockNumber; targetNumber <= block.blockNumber+upperLimitOfVoteBlockNumber; targetNumber++ {
		if vote, ok := v.voteRecords[targetNumber]; ok {
			if vote.SourceNumber < justifiedBlockNumber {
				return false
			}
		}
	}

	v.voteRecords[block.blockNumber] = &types.VoteData{
		SourceNumber: justifiedBlockNumber,
		SourceHash:   justifiedBlockHash,
		TargetNumber: block.blockNumber,
		TargetHash:   block.blockHash,
	}
	return true
}

func (v *MockValidator) InsertBlock(block *MockBlock) {
	// Reject block too old.
	if block.blockNumber+13 < v.head.blockNumber {
		return
	}

	// The higher justified block is the longest chain.
	if block.GetJustifiedNumber() < v.head.GetJustifiedNumber() {
		return
	}
	if block.GetJustifiedNumber() > v.head.GetJustifiedNumber() {
		v.head = block
		return
	}

	// The same finalized number, the larger difficulty is the longest chain.
	if block.td > v.head.td {
		v.head = block
	}
}

type BlockSimulator struct {
	blockNumber   uint64
	coinbaseIndex int
	voteMap       uint64
	insertMap     uint64
}

type ChainSimulator []*BlockSimulator

func (s ChainSimulator) Valid() bool {
	var pre *BlockSimulator
	for index, bs := range s {
		if index == 0 {
			if bs.blockNumber != 1 {
				return false
			}
		} else {
			if bs.blockNumber != pre.blockNumber+1 {
				return false
			}
		}

		pre = bs
	}
	return true
}

type Coordinator struct {
	validators   []*MockValidator
	attestations map[common.Hash]uint64
}

func NewCoordinator(validatorsNumber int) *Coordinator {
	validators := make([]*MockValidator, validatorsNumber)
	for i := 0; i < validatorsNumber; i++ {
		validators[i] = NewMockValidator(i, validatorsNumber)
	}

	return &Coordinator{
		validators:   validators,
		attestations: make(map[common.Hash]uint64),
	}
}

// SimulateP2P simulate a P2P network
func (c *Coordinator) SimulateP2P(cs ChainSimulator) error {
	for _, bs := range cs {
		parent := c.validators[bs.coinbaseIndex].head
		if bs.blockNumber != parent.blockNumber+1 {
			return fmt.Errorf("can't produce discontinuous block, head block: %d, expect produce: %d", parent.blockNumber, bs.blockNumber)
		}
		attestation := c.attestations[parent.blockHash]
		block, err := c.validators[bs.coinbaseIndex].Produce(attestation)
		if err != nil {
			return fmt.Errorf("produce block %v error %v", bs, err)
		}

		c.PropagateBlock(bs, block)
		err = c.AggregateVotes(bs, block)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Coordinator) AggregateVotes(bs *BlockSimulator, block *MockBlock) error {
	var attestation uint64
	count := 0
	for index, voteMap := 0, bs.voteMap; voteMap > 0; index, voteMap = index+1, voteMap>>1 {
		if voteMap&0x1 == 0 {
			continue
		}

		if !c.validators[index].Vote(block) {
			return fmt.Errorf("validator(%d) couldn't vote for block %d produced by validator(%d)", index, block.blockNumber, block.coinbase.index)
		}
		attestation |= 1 << index
		count++
	}

	if count >= cmath.CeilDiv(len(c.validators)*2, 3) {
		c.attestations[block.blockHash] = attestation
	}

	return nil
}

func (c *Coordinator) PropagateBlock(bs *BlockSimulator, block *MockBlock) {
	for index, insertMap := 0, bs.insertMap; insertMap > 0; index, insertMap = index+1, insertMap>>1 {
		if insertMap&0x1 == 0 {
			continue
		}

		c.validators[index].InsertBlock(block)
	}
}

func (c *Coordinator) CheckChain() bool {
	// All validators highest finalized block should not be conflicted
	finalizedBlocks := make([]*MockBlock, len(c.validators))
	for index, val := range c.validators {
		finalizedBlocks[index] = val.head.GetFinalizedBlock()
	}

	for i := 0; i < len(finalizedBlocks)-1; i++ {
		for j := i + 1; j < len(finalizedBlocks); j++ {
			if finalizedBlocks[i].IsConflicted(finalizedBlocks[j]) {
				return false
			}
		}
	}

	return true
}

type TestSimulatorParam struct {
	validatorsNumber int
	cs               ChainSimulator
}

var simulatorTestcases = []*TestSimulatorParam{
	{
		// 3 validators, all active
		validatorsNumber: 3,
		cs: []*BlockSimulator{
			{1, 0, 0x7, 0x7},
			{2, 1, 0x7, 0x7},
			{3, 2, 0x7, 0x7},
			{4, 0, 0x7, 0x7},
			{5, 1, 0x7, 0x7},
		},
	},
	{
		// 5 validators, 4 active, 1 down
		validatorsNumber: 5,
		cs: []*BlockSimulator{
			{1, 0, 0x1f, 0x1f},
			{2, 1, 0x1f, 0x1f},
			{3, 2, 0x1f, 0x1f},
			{4, 3, 0x1f, 0x1f},
			{5, 0, 0x1f, 0x1f},
			{6, 1, 0x1f, 0x1f},
			{7, 2, 0x1f, 0x1f},
		},
	},
	{
		// 21 validators, all active
		validatorsNumber: 21,
		cs: []*BlockSimulator{
			{1, 0, 0x1fffff, 0x1fffff},
			{2, 1, 0x1fffff, 0x1fffff},
			{3, 2, 0x1fffff, 0x1fffff},
			{4, 3, 0x1fffff, 0x1fffff},
			{5, 4, 0x1fffff, 0x1fffff},
			{6, 5, 0x1fffff, 0x1fffff},
			{7, 6, 0x1fffff, 0x1fffff},
			{8, 7, 0x1fffff, 0x1fffff},
			{9, 8, 0x1fffff, 0x1fffff},
			{10, 9, 0x1fffff, 0x1fffff},
			{11, 10, 0x1fffff, 0x1fffff},
			{12, 11, 0x1fffff, 0x1fffff},
			{13, 12, 0x1fffff, 0x1fffff},
			{14, 13, 0x1fffff, 0x1fffff},
			{15, 14, 0x1fffff, 0x1fffff},
			{16, 0, 0x1fffff, 0x1fffff},
			{17, 1, 0x1fffff, 0x1fffff},
			{18, 2, 0x1fffff, 0x1fffff},
		},
	},
	{
		// 21 validators, all active, the finalized fork can keep grow
		validatorsNumber: 21,
		cs: []*BlockSimulator{
			{1, 1, 0x00fffe, 0x00fffe},
			{2, 2, 0x00fffe, 0x00fffe},
			{1, 0, 0x1f0001, 0x1fffff},
			{2, 16, 0x1f0001, 0x1ffff1},
			{3, 17, 0x1f0001, 0x1ffff1},
			{4, 18, 0x1f0001, 0x1ffff1},
			{5, 19, 0x1f0001, 0x1ffff1},
			{3, 3, 0x00fffe, 0x00fffe}, // justify block 2 and finalize block 1
			{6, 20, 0x1f0001, 0x1fffff},
			{4, 4, 0x00fffe, 0x1fffff},
			{5, 5, 0x00fffe, 0x1fffff},
			{6, 6, 0x00fffe, 0x1fffff},
			{7, 7, 0x1fffff, 0x1fffff},
			{8, 8, 0x1fffff, 0x1fffff},
		},
	},
	{
		// 21 validators, all active, the finalized fork can keep grow
		validatorsNumber: 21,
		cs: []*BlockSimulator{
			{1, 14, 0x00fffe, 0x00fffe},
			{2, 15, 0x00fffe, 0x00fffe}, // The block 3 will never produce
			{1, 0, 0x1f0001, 0x1fffff},
			{2, 16, 0x1f0001, 0x1fffff},
			{3, 1, 0x1f0001, 0x1fffff}, // based block produced by 15
			{4, 2, 0x1f0001, 0x1fffff},
			{5, 3, 0x1f0001, 0x1fffff},
			{6, 4, 0x1f0001, 0x1fffff},
			{7, 5, 0x1f0001, 0x1fffff},
			{8, 6, 0x1f0001, 0x1fffff},
			{9, 7, 0x1f0001, 0x1fffff},
			{10, 8, 0x1f0001, 0x1fffff},
			{11, 9, 0x1f0001, 0x1fffff},
			{12, 10, 0x1f0001, 0x1fffff},
			{13, 11, 0x1f0001, 0x1fffff},
			{14, 12, 0x1f0001, 0x1fffff},
			{15, 13, 0x1f0001, 0x1fffff},
			{16, 14, 0x1f0001, 0x1fffff},
			{17, 15, 0x1fffff, 0x1fffff}, // begin new round vote
			{18, 16, 0x1fffff, 0x1fffff}, // attestation for block 17
			{19, 17, 0x1fffff, 0x1fffff}, // attestation for block 18
		},
	},
}

func TestSimulateP2P(t *testing.T) {
	for index, testcase := range simulatorTestcases {
		c := NewCoordinator(testcase.validatorsNumber)
		err := c.SimulateP2P(testcase.cs)
		if err != nil {
			t.Fatalf("[Testcase %d] simulate P2P error: %v", index, err)
		}
		for _, val := range c.validators {
			t.Logf("[Testcase %d] validator(%d) head block: %d",
				index, val.index, val.head.blockNumber)
			t.Logf("[Testcase %d] validator(%d) highest justified block: %d",
				index, val.index, val.head.GetJustifiedNumber())
			t.Logf("[Testcase %d] validator(%d) highest finalized block: %d",
				index, val.index, val.head.GetFinalizedBlock().blockNumber)
		}

		if c.CheckChain() == false {
			t.Fatalf("[Testcase %d] chain not works as expected", index)
		}
	}
}

func TestGetNewSupplyForBlock(t *testing.T) {
	// These numbers are rounded. This test verifies that we are aligned with general vision.
	expectedPctLt13 := [][]float64{
		[]float64{7.20, 8.80},
		[]float64{5.96, 7.20},
		[]float64{5.00, 5.96},
		[]float64{4.25, 5.00},
		[]float64{3.66, 4.25},
		[]float64{3.21, 3.66},
		[]float64{2.85, 3.21},
		[]float64{2.57, 2.85},
		[]float64{2.36, 2.57},
		[]float64{2.19, 2.36},
		[]float64{2.06, 2.19},
		[]float64{1.96, 2.06},
		[]float64{0.0, 1.96},
	}
	tries := mrand.Intn(10)
	blocksPerYear := 10512000
	yearInSeconds := int64(365 * 24 * 60 * 60)
	forkTs := time.Now().Unix()
	totalSupply := big.NewInt(8_888_888_888)
	totalSupply.Mul(totalSupply, big.NewInt(1e18))

	getExpectedBlockAmount := func(totalSupply, infPct *big.Int) *big.Int {
		expectedAmount := big.NewInt(0)
		expectedAmount.Mul(totalSupply, infPct)
		expectedAmount.Div(expectedAmount, big.NewInt(1e18))
		expectedAmount.Div(expectedAmount, big.NewInt(100)) // inflPct is percent*1e18
		expectedAmount.Div(expectedAmount, big.NewInt(int64(blocksPerYear)))
		return expectedAmount
	}

	// test for first 13 years (this is just an approximation)
	// choose a pseudo random block within each year and verify the inflation %
	for i := 0; i < tries; i++ {
		for year := 1; year <= 13; year++ {
			endTs := forkTs + int64(year)*yearInSeconds
			startTs := endTs - yearInSeconds
			blockTs := startTs + mrand.Int63n(endTs-startTs)

			blockAmount, infPct := getNewSupplyForBlock(uint64(forkTs), uint64(blockTs), totalSupply)

			// check amount
			expectedAmount := getExpectedBlockAmount(totalSupply, infPct)
			if blockAmount.Cmp(expectedAmount) != 0 {
				t.Errorf("invalid block amount for year %d, blockTs %d. got %d expected %d", year, blockTs, blockAmount, expectedAmount)
			}

			// check inflation %
			infPct.Div(infPct, big.NewInt(1e16))
			infPctF, _ := infPct.Float64()
			infPctFRounded := math.Round(infPctF*10) / 1000.0
			expectedRange := expectedPctLt13[year-1]
			isWithinRange := infPctFRounded >= expectedRange[0] && infPctFRounded <= expectedRange[1]
			if !isWithinRange {
				t.Errorf("invalid inflation percentage for year %d. got %f expected range (%.2f, %.2f), seconds passed %d", year, infPctFRounded, expectedRange[0], expectedRange[1], blockTs-forkTs)
			}
		}
	}

	// test for years > 13
	expectedPctGt13 := big.NewInt(1880000000000000000)
	for i := 0; i < tries; i++ {
		year := 13 + tries
		endTs := forkTs + int64(year)*31536000
		startTs := endTs - 31536000
		blockTs := startTs + mrand.Int63n(endTs-startTs)

		blockAmount, infPct := getNewSupplyForBlock(uint64(forkTs), uint64(blockTs), totalSupply)

		// check amount
		expectedAmount := getExpectedBlockAmount(totalSupply, infPct)
		if blockAmount.Cmp(expectedAmount) != 0 {
			t.Errorf("invalid block amount for year %d, blockTs %d. got %d expected %d", year, blockTs, blockAmount, expectedAmount)
		}

		if infPct.Cmp(expectedPctGt13) != 0 {
			t.Errorf("invalid inflation percentage for year %d. got %f expected %f", year, infPct, expectedPctGt13)
		}
	}
}
