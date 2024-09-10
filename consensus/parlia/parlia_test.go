package parlia

import (
	"fmt"
	"math/big"
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

func TestImpactOfValidatorOutOfService(t *testing.T) {
	testCases := []struct {
		totalValidators int
		downValidators  int
	}{
		{3, 1},
		{5, 2},
		{10, 1},
		{10, 4},
		{21, 1},
		{21, 3},
		{21, 5},
		{21, 10},
	}
	for _, tc := range testCases {
		simulateValidatorOutOfService(tc.totalValidators, tc.downValidators)
	}
}

func simulateValidatorOutOfService(totalValidators int, downValidators int) {
	downBlocks := 10000
	recoverBlocks := 10000
	recents := make(map[uint64]int)

	validators := make(map[int]bool, totalValidators)
	down := make([]int, totalValidators)
	for i := 0; i < totalValidators; i++ {
		validators[i] = true
		down[i] = i
	}
	rand.Shuffle(totalValidators, func(i, j int) {
		down[i], down[j] = down[j], down[i]
	})
	for i := 0; i < downValidators; i++ {
		delete(validators, down[i])
	}
	isRecentSign := func(idx int) bool {
		for _, signIdx := range recents {
			if signIdx == idx {
				return true
			}
		}
		return false
	}
	isInService := func(idx int) bool {
		return validators[idx]
	}

	downDelay := uint64(0)
	for h := 1; h <= downBlocks; h++ {
		if limit := uint64(totalValidators/2 + 1); uint64(h) >= limit {
			delete(recents, uint64(h)-limit)
		}
		proposer := h % totalValidators
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
		if limit := uint64(totalValidators/2 + 1); uint64(h) >= limit {
			delete(recents, uint64(h)-limit)
		}
		proposer := h % totalValidators
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

	s := rand.NewSource(int64(height))
	r := rand.New(s)
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

// TestDistributeIncomingForkAndBurn tests the distributeIncoming function with a fork in the chain and checks if the total burned and current supply are correct.
func TestDistributeIncomingForkAndBurn(t *testing.T) {
	// Setup the initial state
	db := rawdb.NewMemoryDatabase()
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	address := crypto.PubkeyToAddress(key.PublicKey)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  core.GenesisAlloc{address: {Balance: big.NewInt(1000000000)}},
	}
	genesisBlock := genesis.MustCommit(db)

	parliaEngine := parlia.New(nil) // Initialize Parlia engine
	blockchain, _ := core.NewBlockChain(db, nil, genesis.Config, parliaEngine, vm.Config{}, nil, nil)
	defer blockchain.Stop()

	// Generate a chain with a fork
	blocks, _ := core.GenerateChain(genesis.Config, genesisBlock, parliaEngine, db, 4, func(i int, block *core.BlockGen) {
		tx, err := types.SignTx(types.NewTransaction(block.TxNonce(address), common.Address{}, big.NewInt(1), 21000, big.NewInt(1), nil), types.HomesteadSigner{}, key)
		if err != nil {
			t.Fatal(err)
		}
		block.AddTx(tx)
	})

	// Insert the chain
	if _, err := blockchain.InsertChain(blocks); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	// Create a header for the new block
	header := &types.Header{
		ParentHash: blocks[len(blocks)-1].Hash(),
		Coinbase:   address,
		Number:     big.NewInt(5),
		GasLimit:   8000000,
	}

	// Create a stateDB
	state, _ := state.New(blocks[len(blocks)-1].Root(), state.NewDatabase(db), nil)

	// Set the balance of the system address
	state.SetBalance(consensus.SystemAddress, big.NewInt(1000000))

	// Call distributeIncoming
	var txs []*types.Transaction
	var receipts []*types.Receipt
	var systemTxs []*types.Transaction
	var usedGas uint64
	err := parliaEngine.DistributeIncoming(address, state, header, blockchain, &txs, &receipts, &systemTxs, &usedGas, false)
	if err != nil {
		t.Fatalf("distributeIncoming failed: %v", err)
	}

	// Check the total burned and current supply
	expectedBurned := new(big.Int).Div(new(big.Int).Mul(big.NewInt(1000000), big.NewInt(burnPercentage)), big.NewInt(100))
	expectedCurrentSupply := new(big.Int).Sub(supplyLog.TotalSupplyGenesis, expectedBurned)

	if supplyLog.TotalBurned.Cmp(expectedBurned) != 0 {
		t.Errorf("TotalBurned mismatch: got %v, want %v", supplyLog.TotalBurned, expectedBurned)
	}
	if supplyLog.CurrentSupply.Cmp(expectedCurrentSupply) != 0 {
		t.Errorf("CurrentSupply mismatch: got %v, want %v", supplyLog.CurrentSupply, expectedCurrentSupply)
	}
}
