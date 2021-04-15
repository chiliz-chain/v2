package vm

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/chiliz"
	"math/big"
)

func registerDeployedContract(evm *EVM, caller ContractRef, contractAddress common.Address) (err error) {
	input, err := chiliz.DeployerAbi.Pack("registerDeployedContract", caller.Address(), contractAddress)
	if err != nil {
		return err
	}
	_, _, err = evm.Call(AccountRef(chiliz.SystemCaller), chiliz.DeployerContract, input, 100_000, big.NewInt(0))
	return err
}

func getContractDeployer(evm *EVM, caller ContractRef, contractAddress common.Address) (common.Address, error) {
	input, err := chiliz.DeployerAbi.Pack("getContractDeployer", caller.Address(), contractAddress)
	if err != nil {
		return common.Address{}, err
	}
	ret, _, err := evm.Call(AccountRef(chiliz.SystemCaller), chiliz.DeployerContract, input, 100_000, big.NewInt(0))
	if err != nil {
		return common.Address{}, err
	}
	result := common.Address{}
	err = chiliz.DeployerAbi.Unpack(&result, "getContractDeployer", ret)
	return result, nil
}

func checkContractActive(evm *EVM, contractAddress common.Address) (err error) {
	if chiliz.IsSystemContract(contractAddress) {
		return nil
	}
	input, err := chiliz.DeployerAbi.Pack("checkContractActive", contractAddress)
	if err != nil {
		return err
	}
	_, _, err = evm.Call(AccountRef(chiliz.SystemCaller), chiliz.DeployerContract, input, 100_000, big.NewInt(0))
	return err
}
