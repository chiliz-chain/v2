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

// IsEvmHook reports whether address dispatches to an EVM hook instead of to
// ordinary account code. Keep in sync with CreateEvmHook.
func IsEvmHook(address common.Address) bool {
	return address == evmHookRuntimeUpgradeAddress
}
