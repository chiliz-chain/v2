// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package replay re-executes a single historical transaction offline, against the
// pre-state it actually ran on, using the consensus rules compiled into this build.
//
// It exists to answer one question: would this build reproduce the gas the chain
// charged? An un-fork-gated rule change makes historical blocks unimportable, which
// shows up as a resync stopping dead at some block (COR-193). Replaying the block's
// transactions here catches that from a fixture, with no node and no chain state.
//
// A Fixture is self-contained and serialisable, so cmd/replaycheck can assemble one
// from a live RPC endpoint and the same fixture can be committed as a regression test.
package replay

import (
	"encoding/json"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/config"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// Fixture is everything needed to re-execute one transaction: the block it sat in, the
// transaction itself, the pre-state it saw, and the gas the chain charged for it.
type Fixture struct {
	Comment string    `json:"comment,omitempty"`
	Genesis string    `json:"genesis"` // embedded genesis to take the fork schedule from
	Block   BlockInfo `json:"block"`
	Tx      TxInfo    `json:"tx"`

	// ExpectedGasUsed is the canonical receipt's gasUsed.
	ExpectedGasUsed uint64 `json:"expectedGasUsed"`

	// ExpectedFailed is the canonical receipt's status: true when the transaction reverted
	// on chain. A replay that succeeds where the chain failed, or the other way round, is a
	// divergence even if the gas happens to line up.
	ExpectedFailed bool `json:"expectedFailed,omitempty"`

	// Pre is the pre-state of every account the transaction touched, as captured by the
	// prestateTracer. That is exactly the state the transaction reads, so it is enough to
	// re-execute it even though it is not the whole world state.
	Pre types.GenesisAlloc `json:"pre"`
}

// BlockInfo is the part of a block header that reaches the EVM.
type BlockInfo struct {
	Number        uint64          `json:"number"`
	Hash          common.Hash     `json:"hash"`
	ParentHash    common.Hash     `json:"parentHash"`
	Time          uint64          `json:"time"`
	Coinbase      common.Address  `json:"coinbase"`
	Difficulty    *hexutil.Big    `json:"difficulty"`
	GasLimit      hexutil.Uint64  `json:"gasLimit"`
	BaseFeePerGas *hexutil.Big    `json:"baseFeePerGas,omitempty"`
	ExcessBlobGas *hexutil.Uint64 `json:"excessBlobGas,omitempty"`
	BlobGasUsed   *hexutil.Uint64 `json:"blobGasUsed,omitempty"`
	MixHash       common.Hash     `json:"mixHash"`
}

// TxInfo identifies the transaction and carries it in canonical signed form, so replay
// derives the sender, the fee fields and the access list the same way import does.
type TxInfo struct {
	Hash  common.Hash   `json:"hash"`
	Index int           `json:"index"`
	Raw   hexutil.Bytes `json:"raw"`

	// System marks a transaction parlia injects during Finalize. Those run through a
	// different execution path than user transactions (see Replay), and their pre-state
	// cannot be captured faithfully: consensus drains the fee pool to the coinbase after the
	// Tokenomics deposit, while the tracer that captures the pre-state drains it before the
	// block's first system transaction. A gas delta on one of these is not conclusive.
	System bool `json:"system,omitempty"`
}

// Result is the outcome of a replay.
type Result struct {
	GasUsed uint64
	// VMErr is the transaction's own failure, if it reverted or ran out of gas. A reverting
	// transaction is still a valid replay; only a non-nil error from Replay is a problem.
	VMErr error
}

// Matches reports whether the replay reproduced both the canonical gas and the canonical
// success or failure.
func (f *Fixture) Matches(r *Result) bool {
	return r.GasUsed == f.ExpectedGasUsed && (r.VMErr != nil) == f.ExpectedFailed
}

// Delta is how much more (or less) gas this build charges than the chain did.
func (f *Fixture) Delta(r *Result) int64 { return int64(r.GasUsed) - int64(f.ExpectedGasUsed) }

// ChainConfig returns the fork schedule of one of the embedded Chiliz networks. Reading it
// from the embedded genesis rather than restating it means a replay cannot drift away from
// what the network actually runs.
func ChainConfig(genesis string) (*params.ChainConfig, error) {
	switch genesis {
	case "chiliz", "mainnet":
		return config.ChilizMainnetGenesisConfig.Config, nil
	case "spicy":
		return config.SpicyGenesisConfig.Config, nil
	case "scoville":
		return config.ScovilleGenesisConfig.Config, nil
	default:
		return nil, fmt.Errorf("unknown genesis %q, want chiliz, spicy or scoville", genesis)
	}
}

// GenesisForChainID maps a chain ID to the embedded genesis that configures it.
func GenesisForChainID(chainID uint64) (string, error) {
	switch chainID {
	case 88888:
		return "chiliz", nil
	case 88882:
		return "spicy", nil
	case 88880:
		return "scoville", nil
	default:
		return "", fmt.Errorf("chain id %d is not a Chiliz network", chainID)
	}
}

// Header rebuilds the header fields the EVM reads, so the block context is derived the same
// way core.NewEVMBlockContext derives it during import.
func (f *Fixture) Header() *types.Header {
	header := &types.Header{
		ParentHash: f.Block.ParentHash,
		Coinbase:   f.Block.Coinbase,
		Number:     new(big.Int).SetUint64(f.Block.Number),
		GasLimit:   uint64(f.Block.GasLimit),
		Time:       f.Block.Time,
		MixDigest:  f.Block.MixHash,
		Difficulty: new(big.Int),
	}
	if f.Block.Difficulty != nil {
		header.Difficulty = f.Block.Difficulty.ToInt()
	}
	if f.Block.BaseFeePerGas != nil {
		header.BaseFee = f.Block.BaseFeePerGas.ToInt()
	}
	if f.Block.ExcessBlobGas != nil {
		excess := uint64(*f.Block.ExcessBlobGas)
		header.ExcessBlobGas = &excess
	}
	if f.Block.BlobGasUsed != nil {
		used := uint64(*f.Block.BlobGasUsed)
		header.BlobGasUsed = &used
	}
	return header
}

// Replay re-executes the fixture's transaction with this build's rules.
//
// getHash resolves the BLOCKHASH opcode; pass nil to resolve only the parent (the gas cost
// of BLOCKHASH does not depend on the answer, so a transaction whose gas this changes has
// to be reading an older block hash and acting on it).
func (f *Fixture) Replay(getHash func(uint64) common.Hash) (*Result, error) {
	chainConfig, err := ChainConfig(f.Genesis)
	if err != nil {
		return nil, err
	}
	statedb, err := PreState(f.Pre)
	if err != nil {
		return nil, err
	}
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(f.Tx.Raw); err != nil {
		return nil, fmt.Errorf("decode transaction: %w", err)
	}
	if got := tx.Hash(); got != f.Tx.Hash && f.Tx.Hash != (common.Hash{}) {
		return nil, fmt.Errorf("raw transaction is %s, fixture says %s", got, f.Tx.Hash)
	}
	header := f.Header()

	signer := types.MakeSigner(chainConfig, header.Number, header.Time)
	msg, err := core.TransactionToMessage(tx, signer, header.BaseFee)
	if err != nil {
		return nil, fmt.Errorf("derive message: %w", err)
	}
	// Parlia does not put its own system transactions through the sender, nonce and fee
	// checks, so neither can a faithful replay of one.
	msg.SkipTransactionChecks = f.Tx.System

	if getHash == nil {
		getHash = func(n uint64) common.Hash {
			if n == f.Block.Number-1 {
				return f.Block.ParentHash
			}
			return common.Hash{}
		}
	}
	var blobBaseFee *big.Int
	if header.ExcessBlobGas != nil {
		blobBaseFee = eip4844.CalcBlobFee(chainConfig, header)
	}
	var random *common.Hash
	if header.Difficulty.Sign() == 0 {
		random = &header.MixDigest
	}
	blockCtx := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     getHash,
		Coinbase:    header.Coinbase,
		BlockNumber: new(big.Int).Set(header.Number),
		Time:        header.Time,
		Difficulty:  new(big.Int).Set(header.Difficulty),
		GasLimit:    header.GasLimit,
		BaseFee:     header.BaseFee,
		BlobBaseFee: blobBaseFee,
		Random:      random,
	}
	evm := vm.NewEVM(blockCtx, statedb, chainConfig, vm.Config{})
	evm.SetTxContext(core.NewEVMTxContext(msg))
	statedb.SetTxContext(tx.Hash(), f.Tx.Index)

	if f.Tx.System {
		return replaySystemTx(evm, statedb, chainConfig, header, msg)
	}
	result, err := core.ApplyMessage(evm, msg, new(core.GasPool).AddGas(tx.Gas()))
	if err != nil {
		return nil, fmt.Errorf("apply message: %w", err)
	}
	return &Result{GasUsed: result.UsedGas, VMErr: result.Err}, nil
}

// replaySystemTx mirrors Parlia.applyMessage (consensus/parlia/parlia.go). Parlia does not
// put its own system transactions through core.ApplyMessage: it prepares the access list
// itself and calls straight into the EVM, so there is no intrinsic gas and no gas purchase,
// and the receipt records only what the EVM burned. Replaying one through ApplyMessage
// instead overcharges it by the intrinsic gas, which is not a divergence.
func replaySystemTx(evm *vm.EVM, statedb vm.StateDB, chainConfig *params.ChainConfig, header *types.Header, msg *core.Message) (*Result, error) {
	rules := chainConfig.Rules(evm.Context.BlockNumber, evm.Context.Random != nil, evm.Context.Time)
	if chainConfig.IsCancun(header.Number, header.Time) {
		statedb.Prepare(rules, msg.From, evm.Context.Coinbase, msg.To, vm.ActivePrecompiles(rules), msg.AccessList)
	} else {
		statedb.ClearAccessList()
	}
	_, returnGas, vmErr := evm.Call(msg.From, *msg.To, msg.Data, msg.GasLimit, uint256.MustFromBig(msg.Value))
	return &Result{GasUsed: msg.GasLimit - returnGas, VMErr: vmErr}, nil
}

// PreState builds an in-memory state from a captured account set.
func PreState(alloc types.GenesisAlloc) (*state.StateDB, error) {
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		return nil, err
	}
	for addr, account := range alloc {
		statedb.SetCode(addr, account.Code, tracing.CodeChangeUnspecified)
		statedb.SetNonce(addr, account.Nonce, tracing.NonceChangeUnspecified)
		if account.Balance != nil {
			statedb.SetBalance(addr, uint256.MustFromBig(account.Balance), tracing.BalanceChangeUnspecified)
		}
		for key, value := range account.Storage {
			statedb.SetState(addr, key, value)
		}
	}
	// Commit and reopen so the replay starts from a clean, committed state the way import
	// does, rather than from a state object full of pending writes.
	root, err := statedb.Commit(0, false, false)
	if err != nil {
		return nil, err
	}
	return state.New(root, statedb.Database())
}

// MarshalFixture serialises a fixture in the form the testdata files use.
func MarshalFixture(f *Fixture) ([]byte, error) { return json.MarshalIndent(f, "", " ") }

// UnmarshalFixture parses a fixture file.
func UnmarshalFixture(raw []byte) (*Fixture, error) {
	f := new(Fixture)
	if err := json.Unmarshal(raw, f); err != nil {
		return nil, err
	}
	return f, nil
}
