package vm

import (
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"math/big"
)

var (
	systemCaller = common.HexToAddress("0x0000000000000000000000000000000000000000")
	deployerContract   = common.HexToAddress("0x0000000000000000000000000000000000000020")
	governanceContract = common.HexToAddress("0x0000000000000000000000000000000000000030")
	parliaContract     = common.HexToAddress("0x0000000000000000000000000000000000000040")
)

func newDeployerAbi() *abi.ABI {
	addressType, _ := abi.NewType("address", "", nil)
	boolType, _ := abi.NewType("bool", "", nil)
	return &abi.ABI{
		Methods: map[string]abi.Method{
			"registerDeployedContract(address,address)": {
				Name:    "registerDeployedContract",
				RawName: "registerDeployedContract(address,address)",
				Inputs: abi.Arguments{
					abi.Argument{Type: addressType},
				},
				Outputs: abi.Arguments{
				},
			},
			"isContractActive(address)": {
				Name:    "isContractActive",
				RawName: "isContractActive(address)",
				Inputs: abi.Arguments{
					abi.Argument{Type: addressType},
				},
				Outputs: abi.Arguments{
					abi.Argument{Type: boolType},
				},
			},
			"checkContractActive(address)": {
				Name:    "checkContractActive",
				RawName: "checkContractActive(address)",
				Inputs: abi.Arguments{
					abi.Argument{Type: addressType},
				},
				Outputs: abi.Arguments{
				},
			},
		},
	}
}

func (evm *EVM) registerDeployedContract(caller ContractRef, contractAddress common.Address, gas uint64) (leftOverGas uint64, err error) {
	contractAbi := newDeployerAbi()
	input, err := contractAbi.Pack("registerDeployedContract(address,address)", caller.Address(), contractAddress)
	if err != nil {
		return gas, err
	}
	_, leftOverGas, err = evm.Call(AccountRef(systemCaller), deployerContract, input, gas, big.NewInt(0))
	return leftOverGas, err
}

func (evm *EVM) getContractDeployer(caller ContractRef, contractAddress common.Address, gas uint64) (common.Address, error) {
	contractAbi := newDeployerAbi()
	input, err := contractAbi.Pack("getContractDeployer(address)", caller.Address(), contractAddress)
	if err != nil {
		return common.Address{}, err
	}
	ret, _, err := evm.Call(AccountRef(systemCaller), deployerContract, input, gas, big.NewInt(0))
	if err != nil {
		return common.Address{}, err
	}
	result := common.Address{}
	err = contractAbi.Unpack(&result, "getContractDeployer(address)", ret)
	return result, nil
}

func (evm *EVM) checkContractActive(contractAddress common.Address, gas uint64) (leftOverGas uint64, err error) {
	contractAbi := newDeployerAbi()
	input, err := contractAbi.Pack("checkContractActive(address)", contractAddress)
	if err != nil {
		return gas, err
	}
	_, leftOverGas, err = evm.Call(AccountRef(systemCaller), deployerContract, input, gas, big.NewInt(0))
	return leftOverGas, err
}
