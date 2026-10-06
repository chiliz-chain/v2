package core

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/pepper8"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// TestPreCheckChilizFeeExemption drives preCheck itself, not just the predicate.
//
// COR-214 review: FuzzSystemTxClassification pins IsChilizFeeExemptMessage in
// isolation, but nothing put a message through the call site. Drop the "!" at
// core/state_transition.go:398 and every test in the tree still passes,
// TestReplayFixtures included, while every system transaction on a live node
// fails with ErrFeeCapTooLow — the chain stops producing blocks. This closes
// that: the exempt cases must NOT see ErrFeeCapTooLow and the others MUST.
func TestPreCheckChilizFeeExemption(t *testing.T) {
	var (
		coinbase = common.HexToAddress("0x00000000000000000000000000000000000000c0")
		other    = common.HexToAddress("0x00000000000000000000000000000000000000ff")
		sysTo    = common.HexToAddress(systemcontract.TokenomicsContract)
		pepperTo = pepper8.Pepper8RecipientAddress
		userTo   = common.HexToAddress("0x00000000000000000000000000000000000000aa")
		baseFee  = big.NewInt(2_500_000_000_000) // params.InitialBaseFee on Chiliz
	)
	for _, tc := range []struct {
		name     string
		from     common.Address
		to       *common.Address
		gasPrice *big.Int
		exempt   bool
	}{
		{"system tx: coinbase -> system contract, zero price", coinbase, &sysTo, common.Big0, true},
		{"pepper8 deposit: coinbase -> mint recipient, zero price", coinbase, &pepperTo, common.Big0, true},
		{"not the coinbase", other, &sysTo, common.Big0, false},
		{"not a system destination", coinbase, &userTo, common.Big0, false},
		{"non-zero gas price", coinbase, &sysTo, big.NewInt(1), false},
		{"contract creation", coinbase, nil, common.Big0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
			if err != nil {
				t.Fatal(err)
			}
			statedb.SetBalance(tc.from, uint256.MustFromBig(big.NewInt(1e18)), 0)

			blockCtx := vm.BlockContext{
				CanTransfer: CanTransfer,
				Transfer:    Transfer,
				Coinbase:    coinbase,
				BlockNumber: big.NewInt(1),
				Time:        1,
				Difficulty:  big.NewInt(1),
				GasLimit:    30_000_000,
				BaseFee:     baseFee,
			}
			evm := vm.NewEVM(blockCtx, statedb, params.TestChainConfig, vm.Config{})

			// A fee cap below the base fee: only the Chiliz exemption can let this
			// through preCheck. GasPrice is the effective price the exemption reads.
			msg := &Message{
				From:            tc.from,
				To:              tc.to,
				Value:           common.Big0,
				GasLimit:        100_000,
				GasPrice:        tc.gasPrice,
				GasFeeCap:       common.Big0,
				GasTipCap:       common.Big0,
				SkipNonceChecks: true,
			}
			_, err = ApplyMessage(evm, msg, new(GasPool).AddGas(msg.GasLimit))
			gotFeeCapTooLow := errors.Is(err, ErrFeeCapTooLow)
			if tc.exempt && gotFeeCapTooLow {
				t.Fatalf("exempt message rejected with ErrFeeCapTooLow: %v", err)
			}
			if !tc.exempt && !gotFeeCapTooLow {
				t.Fatalf("non-exempt message with feeCap %s < baseFee %s was not rejected (err = %v)", msg.GasFeeCap, baseFee, err)
			}
		})
	}
}
