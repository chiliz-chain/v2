package systemcontract

import (
	"github.com/ethereum/go-ethereum/common"
)

func CreateEvmHook(address common.Address, context EvmHookContext) EvmHook {
	if address == evmHookRuntimeUpgradeAddress {
		return &evmHookRuntimeUpgrade{context: context}
	}
	return nil
}
