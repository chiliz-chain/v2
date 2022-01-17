package systemcontracts

import (
	"bytes"
	_ "embed"
	"log"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

//go:embed abi/DeployerV1.json
var deployerJsonABI []byte

//go:embed abi/GovernanceV1.json
var governanceJsonABI []byte

//go:embed abi/ParliaV1.json
var parliaJsonABI []byte

func loadJsonAbiOrFatal(jsonAbi []byte) abi.ABI {
	result, err := abi.JSON(bytes.NewReader(jsonAbi))
	if err != nil {
		log.Fatalf("can't load abi file: %s", err)
	}
	return result
}

var (
	DeployerAbi   = loadJsonAbiOrFatal(deployerJsonABI)
	GovernanceAbi = loadJsonAbiOrFatal(governanceJsonABI)
	ParliaAbi     = loadJsonAbiOrFatal(parliaJsonABI)
)
