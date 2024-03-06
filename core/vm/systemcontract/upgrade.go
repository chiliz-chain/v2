package systemcontract

import (
	"bytes"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

type evmHookRuntimeUpgrade struct {
	context EvmHookContext
}

func mustNewType(name string) abi.Type {
	typ, err := abi.NewType(name, name, nil)
	if err != nil {
		panic(err)
	}
	return typ
}

var (
	// abi types
	addressType = mustNewType("address")
	bytesType   = mustNewType("bytes")
	// input args
	upgradeToMethod = abi.NewMethod("upgradeTo(address,bytes)", "upgradeTo", abi.Function, "", false, false, abi.Arguments{
		abi.Argument{Type: addressType}, // system contract address
		abi.Argument{Type: bytesType},   // new byte code
	}, abi.Arguments{})
)

func matchesMethod(input []byte, method abi.Method) []interface{} {
	// check does call matches
	if len(input) < 4 || !bytes.Equal(input[:4], method.ID) {
		return nil
	}
	values, err := method.Inputs.UnpackValues(input[4:])
	if err != nil || len(values) != len(method.Inputs) {
		return nil
	}
	return values
}

var runtimeUpgradeContract = common.HexToAddress("0x0000000000000000000000000000000000007004")

func (sc *evmHookRuntimeUpgrade) Run(input []byte) ([]byte, error) {
	if !sc.context.ChainRules.HasRuntimeUpgrade {
		return nil, errNotSupported
	}
	// check the caller
	if sc.context.CallerAddress != runtimeUpgradeContract {
		return nil, errInvalidCaller
	}
	// if matches upgrade to method
	if values := matchesMethod(input, upgradeToMethod); values != nil {
		contractAddress, ok := values[0].(common.Address)
		if !ok {
			return nil, errFailedToUnpack
		}
		byteCode, ok := values[1].([]byte)
		if !ok {
			return nil, errFailedToUnpack
		}
		sc.context.StateDb.SetCode(contractAddress, byteCode)
		return nil, nil
	}
	return nil, errMethodNotFound
}

func (sc *evmHookRuntimeUpgrade) RequiredGas(input []byte) uint64 {
	// don't charge gas for these cals
	return 0
}
