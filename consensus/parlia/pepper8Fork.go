package parlia

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/log"
)

const (
	pepper8MintAmount                    = "148600000000000000000000000"
	pepper8RecipientAddress              = "0xE0d17A41C1A4Fe527e375C644F9D2A02e96111ED"
	deterministicDeploymentProxyBytecode = "604580600e600039806000f350fe7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe03601600081602082378035828234f58015156039578182fd5b8082525050506014600cf3"
	deterministicDeploymentProxyAddress  = "0x4e59b44847b379578588920cA78FbF26c0B4956C"
)

func (p *Parlia) IsPepper8Block(currentBlockTime uint64, parentBlockTime uint64) bool {
	return !p.chainConfig.IsPepper8Time(parentBlockTime) && p.chainConfig.IsPepper8Time(currentBlockTime)
}
func (p *Parlia) GetPepper8MintAmount() *big.Int {
	log.Info("GetPepper8MintAmount", "pepper8MintAmount", pepper8MintAmount)
	return cmath.MustParseBig256(pepper8MintAmount)
}
func (p *Parlia) getPepper8RecipientAddress() common.Address {
	return common.HexToAddress(pepper8RecipientAddress)
}
func getPepper8RecipientAddress() common.Address {
	return common.HexToAddress(pepper8RecipientAddress)
}

func (p *Parlia) getPepper8DeterministicDeploymentProxyBytecode() string {
	return deterministicDeploymentProxyBytecode
}
func (p *Parlia) getPepper8DeterministicDeploymentProxyAddress() common.Address {
	return common.HexToAddress(deterministicDeploymentProxyAddress)
}
