package vm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/holiman/uint256"
)

// deployerProxyHooksActive reports whether the DeployerProxy EVM hooks should
// fire: on once the RuntimeUpgrade fork is active (keeps the hooks out of
// pre-fork / non-Parlia upstream test fixtures), and off again once the
// DeployerProxy sunset fork activates.
func (evm *EVM) deployerProxyHooksActive() bool {
	return evm.chainRules.HasRuntimeUpgrade && !evm.chainRules.DeployerProxySunset
}

func applyChilizInvocationEvmHook(evm *EVM, addr common.Address, gas uint64) (leftOverGas uint64, err error) {
	if systemcontract.IsSystemContract(addr) {
		return gas, nil
	}
	input, err := systemcontract.EvmHooksAbi.Pack("checkContractActive", addr)
	if err != nil {
		return gas, ErrNotAllowed
	}
	// Increment depth so the tracer records this as a sub-call rather than a
	// second root-level call (the hook fires inside an already-open Call frame).
	evm.depth++
	defer func() { evm.depth-- }()
	// don't charge gas for this interceptor to let simple send be 21000 gas
	_, _, err = evm.Call(evm.Context.Coinbase, systemcontract.DeployerProxyContractAddress, input, 1_000_000, uint256.MustFromBig(big.NewInt(0)))
	if err != nil {
		return gas, ErrNotAllowed
	}
	return gas, nil
}

func applyChilizDeploymentEvmHook(evm *EVM, caller common.Address, addr common.Address, gas uint64) (leftOverGas uint64, err error) {
	if systemcontract.IsSystemContract(addr) {
		return gas, nil
	}
	var input []byte
	if evm.chainRules.HasDeployOrigin && !evm.chainRules.DeployerFactory {
		input, err = systemcontract.EvmHooksAbi.Pack("registerDeployedContract", evm.TxContext.Origin, addr)
	} else {
		input, err = systemcontract.EvmHooksAbi.Pack("registerDeployedContract", caller, addr)
	}
	if err != nil {
		return gas, ErrNotAllowed
	}
	// Increment depth so the tracer records this as a sub-call rather than a
	// second root-level call (the hook fires inside an already-open create frame).
	evm.depth++
	defer func() { evm.depth-- }()
	_, gas, err = evm.Call(evm.Context.Coinbase, systemcontract.DeployerProxyContractAddress, input, gas, uint256.MustFromBig(big.NewInt(0)))
	if err != nil {
		return gas, ErrNotAllowed
	}
	return gas, nil
}
