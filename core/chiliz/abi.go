package chiliz

import (
	"github.com/ethereum/go-ethereum/accounts/abi"
	"log"
	"os"
)

func loadAbiFromFileOrFatal(fileName string) abi.ABI {
	file, err := os.OpenFile(fileName, os.O_RDONLY, 0)
	if err != nil {
		log.Fatalf("can't load abi file: %s", err)
	}
	result, err := abi.JSON(file)
	if err != nil {
		log.Fatalf("can't load abi file: %s", err)
	}
	return result
}

var (
	DeployerAbi   = loadAbiFromFileOrFatal("./core/chiliz/abi/DeployerV1.json")
	GovernanceAbi = loadAbiFromFileOrFatal("./core/chiliz/abi/GovernanceV1.json")
	ParliaAbi     = loadAbiFromFileOrFatal("./core/chiliz/abi/ParliaV1.json")
)
