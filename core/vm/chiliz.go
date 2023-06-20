package vm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/systemcontract"
)

func applyChilizInvocationEvmHook(evm *EVM, addr common.Address, gas uint64) (leftOverGas uint64, err error) {
	if systemcontract.IsSystemContract(addr) {
		return gas, nil
	}
	input, err := systemcontract.EvmHooksAbi.Pack("checkContractActive", addr)
	if err != nil {
		return gas, ErrNotAllowed
	}
	// don't charge gas for this interceptor to let simple send be 21000 gas
	_, _, err = evm.Call(AccountRef(evm.Context.Coinbase), systemcontract.DeployerProxyContractAddress, input, 1_000_000, big.NewInt(0))
	if err != nil {
		return gas, ErrNotAllowed
	}
	return gas, nil
}

func applyChilizDeploymentEvmHook(evm *EVM, caller ContractRef, addr common.Address, gas uint64) (leftOverGas uint64, err error) {
	if systemcontract.IsSystemContract(addr) {
		return gas, nil
	}
	var input []byte
	if evm.chainRules.HasDeployOrigin && !evm.chainRules.DeployerFactory {
		input, err = systemcontract.EvmHooksAbi.Pack("registerDeployedContract", evm.TxContext.Origin, addr)
	} else {
		input, err = systemcontract.EvmHooksAbi.Pack("registerDeployedContract", caller.Address(), addr)
	}
	if err != nil {
		return gas, ErrNotAllowed
	}
	_, gas, err = evm.Call(AccountRef(evm.Context.Coinbase), systemcontract.DeployerProxyContractAddress, input, gas, big.NewInt(0))
	if err != nil {
		return gas, ErrNotAllowed
	}
	return gas, nil
}
