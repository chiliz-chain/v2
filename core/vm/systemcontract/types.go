package systemcontract

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/params"
)

type EvmHook interface {
	RequiredGas(input []byte) uint64
	Run(input []byte) ([]byte, error)
}

type StateDB interface {
	GetCodeHash(common.Address) common.Hash
	GetCode(common.Address) []byte
	SetCode(common.Address, []byte)
	GetCodeSize(common.Address) int
}

type EvmHookContext struct {
	CallerAddress common.Address
	StateDb       StateDB
	ChainConfig   *params.ChainConfig
	ChainRules    params.Rules
}
