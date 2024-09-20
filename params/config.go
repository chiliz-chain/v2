// Copyright 2016 The go-ethereum Authors
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

package params

import (
	"github.com/ethereum/go-ethereum/common"
	"math/big"
)

// Genesis hashes to enforce below configs on.
var (
	MainnetGenesisHash = common.HexToHash("0xd4e56740f876aef8c010b86a40d5f56745a118d0906a34e69aec8c0db1cb8fa3")

	BSCGenesisHash            = common.HexToHash("0x0d21840abff46b96c84b2ac9e10e4f5cdaeb5693cb665db62a2f3b02d2d57b5b")
	ChapelGenesisHash         = common.HexToHash("0x6d3c66c5357ec91d5c43af47e234a939b22557cbb552dc45bebbceeed90fbe34")
	RialtoGenesisHash         = common.HexToHash("0xaabe549bfa85c84f7aee9da7010b97453ad686f2c2d8ce00503d1a00c72cad54")
	ChilizScovilleGenesisHash = common.HexToHash("0xa148378fbfd7562cd43c8622d20ad056b735fdc0f968f56d0033294c33ededf2")
	ChilizSpicyGenesisHash    = common.HexToHash("0x9e0e07ae4ee9b0ef66a4206656677020306259d0b0b845ad3bb6b09fb91485ff")
	ChilizMainnetGenesisHash  = common.HexToHash("")
)

// Helper to define uint64 values
func newUint64(val uint64) *uint64 { return &val }

var (
	// Mainnet chain configuration, including standard Ethereum forks
	MainnetChainConfig = &ChainConfig{
		ChainID:        big.NewInt(1),
		HomesteadBlock: big.NewInt(1_150_000),
		EIP150Block:    big.NewInt(2_463_000),
		Ethash:         new(EthashConfig),
	}

	// Chiliz V2 chain configuration with the burn block added.
	ChilizChainConfig = &ChainConfig{
		ChainID:        big.NewInt(56),
		BurnFee50Block: big.NewInt(13_000_000), // Block where the burn will be activated (replace with actual block number)
		Ethash:         new(EthashConfig),
	}
)

// ChainConfig is the core config which determines the blockchain settings.
type ChainConfig struct {
	ChainID *big.Int `json:"chainId"` // Chain ID for replay protection

	HomesteadBlock *big.Int `json:"homesteadBlock,omitempty"` // Homestead switch block
	BurnFee50Block *big.Int `json:"burnFee50Block,omitempty"` // Block number for burning gas fees

	// Remaining fields...
	Ethash *EthashConfig `json:"ethash,omitempty"` // Consensus engine (Proof of Work)
}

// IsBurnFee50Block: Function to check if the burn fee block is activated
func (c *ChainConfig) IsBurnFee50Block(num *big.Int) bool {
	return isBlockForked(c.BurnFee50Block, num)
}

// isBlockForked: Helper to check if a block number has passed a specific fork block
func isBlockForked(s, head *big.Int) bool {
	if s == nil || head == nil {
		return false
	}
	return s.Cmp(head) <= 0
}

// EthashConfig is the consensus engine configs for proof-of-work based sealing.
type EthashConfig struct{}

// String implements the stringer interface, returning the consensus engine details.
func (c *EthashConfig) String() string {
	return "ethash"
}
