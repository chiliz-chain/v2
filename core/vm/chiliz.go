package vm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
)

func applyChilizInvocationEvmHook(evm *EVM, addr common.Address, gas uint64) (leftOverGas uint64, err error) {
	if systemcontract.IsSystemContract(addr) {
		return gas, nil
	}
	input, err := systemcontracts.EvmHooksAbi.Pack("checkContractActive", addr)
	if err != nil {
		return gas, ErrNotAllowed
	}
	// don't charge gas for this interceptor to let simple send be 21000 gas
	_, _, err = evm.Call(AccountRef(evm.Context.Coinbase), systemcontracts.ContractDeployerAddress, input, 1_000_000, big.NewInt(0))
	if err != nil {
		return gas, ErrNotAllowed
	}
	return gas, nil
}

func applyChilizDeploymentEvmHook(evm *EVM, caller ContractRef, addr common.Address, gas uint64) (leftOverGas uint64, err error) {
	if systemcontracts.IsSystemContract(addr) {
		return gas, nil
	}
	input, err := systemcontracts.EvmHooksAbi.Pack("registerDeployedContract", caller.Address(), addr)
	if err != nil {
		return gas, ErrNotAllowed
	}
	_, gas, err = evm.Call(AccountRef(evm.Context.Coinbase), systemcontracts.ContractDeployerAddress, input, gas, big.NewInt(0))
	if err != nil {
		return gas, ErrNotAllowed
	}
	return gas, nil
}
