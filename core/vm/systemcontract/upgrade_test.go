package systemcontract

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/require"
	"testing"
)

type fakeStateDb struct {
	codeState map[common.Address][]byte
}

func (s *fakeStateDb) GetCodeHash(common.Address) common.Hash {
	panic("not supported")
}

func (s *fakeStateDb) GetCode(addr common.Address) []byte {
	if s.codeState == nil {
		s.codeState = make(map[common.Address][]byte)
	}
	return s.codeState[addr]
}

func (s *fakeStateDb) SetCode(addr common.Address, byteCode []byte) {
	if s.codeState == nil {
		s.codeState = make(map[common.Address][]byte)
	}
	s.codeState[addr] = byteCode
}

func (s *fakeStateDb) GetCodeSize(addr common.Address) int {
	if s.codeState == nil {
		s.codeState = make(map[common.Address][]byte)
	}
	return len(s.codeState[addr])
}

func TestEvmHookRuntimeUpgrade_UpgradeShouldWork(t *testing.T) {
	statedb := &fakeStateDb{}
	evmHook := &evmHookRuntimeUpgrade{
		context: EvmHookContext{
			CallerAddress: common.HexToAddress("0x0000000000000000000000000000000000007004"),
			StateDb:       statedb,
			ChainConfig:   nil,
			ChainRules: params.Rules{
				HasRuntimeUpgrade: true,
			},
		},
	}
	_, err := evmHook.Run(hexutil.MustDecode("0x6fbc15e900000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000040000000000000000000000000000000000000000000000000000000000000008d60566037600b82828239805160001a607314602a57634e487b7160e01b600052600060045260246000fd5b30600052607381538281f3fe73000000000000000000000000000000000000000030146080604052600080fdfea2646970667358221220e8e6b1d3408504bc107c141b02b247333ed5bfb36f4a2948a69815b2f5aec0f264736f6c634300080b003300000000000000000000000000000000000000"))
	require.NoError(t, err)
	require.Equal(t, statedb.codeState[common.HexToAddress("0x0000000000000000000000000000000000001000")], hexutil.MustDecode("0x60566037600b82828239805160001a607314602a57634e487b7160e01b600052600060045260246000fd5b30600052607381538281f3fe73000000000000000000000000000000000000000030146080604052600080fdfea2646970667358221220e8e6b1d3408504bc107c141b02b247333ed5bfb36f4a2948a69815b2f5aec0f264736f6c634300080b0033"))
}

func TestEvmHookRuntimeUpgrade_DoesntWorkWhenDisabled(t *testing.T) {
	statedb := &fakeStateDb{}
	evmHook := &evmHookRuntimeUpgrade{
		context: EvmHookContext{
			CallerAddress: common.HexToAddress("0x0000000000000000000000000000000000007004"),
			StateDb:       statedb,
			ChainConfig:   nil,
			ChainRules: params.Rules{
				HasRuntimeUpgrade: false,
			},
		},
	}
	_, err := evmHook.Run(hexutil.MustDecode("0x6fbc15e900000000000000000000000000000000000000000000000000000000000010000000000000000000000000000000000000000000000000000000000000000040000000000000000000000000000000000000000000000000000000000000008d60566037600b82828239805160001a607314602a57634e487b7160e01b600052600060045260246000fd5b30600052607381538281f3fe73000000000000000000000000000000000000000030146080604052600080fdfea2646970667358221220e8e6b1d3408504bc107c141b02b247333ed5bfb36f4a2948a69815b2f5aec0f264736f6c634300080b003300000000000000000000000000000000000000"))
	require.Error(t, err)
}

func TestEvmHookRuntimeUpgrade_BadParams(t *testing.T) {
	statedb := &fakeStateDb{}
	evmHook := &evmHookRuntimeUpgrade{
		context: EvmHookContext{
			CallerAddress: common.HexToAddress("0x0000000000000000000000000000000000007004"),
			StateDb:       statedb,
			ChainConfig:   nil,
			ChainRules: params.Rules{
				HasRuntimeUpgrade: true,
			},
		},
	}
	_, err := evmHook.Run(hexutil.MustDecode("0x"))
	require.Error(t, err)
	_, err = evmHook.Run(hexutil.MustDecode("0x6fbc15e9"))
	require.Error(t, err)
	_, err = evmHook.Run(hexutil.MustDecode("0x6fbc15e90000000000000000000000000000000000000000000000000000000000001000"))
	require.Error(t, err)
}
