package vm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	vmsystemcontract "github.com/ethereum/go-ethereum/core/vm/systemcontract"
	"github.com/holiman/uint256"
)

// deployerProxyHooksActive reports whether the DeployerProxy EVM hooks should
// fire: on once the RuntimeUpgrade fork is active (keeps the hooks out of
// pre-fork / non-Parlia upstream test fixtures), and off again once the
// DeployerProxy sunset fork activates.
func (evm *EVM) deployerProxyHooksActive() bool {
	return evm.chainRules.HasRuntimeUpgrade && !evm.chainRules.DeployerProxySunset
}

// warmDeployerProxyOnHookDispatch adds the DeployerProxy to the transaction's
// EIP-2929 access list when addr dispatches to an EVM hook (COR-193).
//
// Up to client 2.6.0 applyChilizInvocationEvmHook ran at the very top of
// EVM.Call, for every callee including the hook addresses themselves. Its inner
// call to the DeployerProxy left that address warm for the rest of the calling
// frame. Client 2.7.3 moved the invocation hook behind the precompile/hook
// dispatch, so calling a hook address no longer touches the DeployerProxy and a
// later access to it is charged as cold — 2500 gas more than the canonical
// chain spent. That un-gated change broke replay of every block that runs a
// runtime upgrade *of the DeployerProxy*, the only shape of transaction that
// invokes a hook and then reads 0x...7005 (mainnet 31384697, spicy 32196267).
//
// Re-warming the address on hook dispatch restores exactly that one side effect
// of the 2.6.0 placement and nothing else: the hook itself is not re-run, so
// calls to precompiles, code-less accounts and non-existent accounts stay as
// they are today, and the warmth is journaled with the calling frame instead of
// surviving a revert.
func (evm *EVM) warmDeployerProxyOnHookDispatch(addr common.Address) {
	if !vmsystemcontract.IsEvmHook(addr) || !evm.chainRules.IsEIP2929 || !evm.deployerProxyHooksActive() {
		return
	}
	evm.StateDB.AddAddressToAccessList(systemcontract.DeployerProxyContractAddress)
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
