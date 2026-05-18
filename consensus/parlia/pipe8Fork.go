package parlia

import (
	"math/big"

	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/log"
)

const (
	pipe8MintAmount = "455700467000000000000000000"
)

func (p *Parlia) IsPipe8Block(currentBlockTime uint64, parentBlockTime uint64) bool {
	return !p.chainConfig.IsPipe8Time(parentBlockTime) && p.chainConfig.IsPipe8Time(currentBlockTime)
}

func (p *Parlia) GetPipe8MintAmount() *big.Int {
	log.Trace("GetPipe8MintAmount", "pipe8MintAmount", pipe8MintAmount)
	return cmath.MustParseBig256(pipe8MintAmount)
}
