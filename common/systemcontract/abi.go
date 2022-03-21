package systemcontract

import (
	"bytes"
	_ "embed"
	"log"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

func loadJsonAbiOrFatal(jsonAbi []byte) abi.ABI {
	result, err := abi.JSON(bytes.NewReader(jsonAbi))
	if err != nil {
		log.Fatalf("can't load abi file: %s", err)
	}
	return result
}

//go:embed abi/IEvmHooks.json
var evmHooksAbi []byte

var (
	EvmHooksAbi = loadJsonAbiOrFatal(evmHooksAbi)
)
