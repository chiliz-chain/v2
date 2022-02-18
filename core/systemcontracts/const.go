package systemcontracts

import (
	"bytes"
	_ "embed"
	"log"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// genesis contracts
const (
	ValidatorContract          = "0x0000000000000000000000000000000000001000"
	SlashContract              = "0x0000000000000000000000000000000000001001"
	SystemRewardContract       = "0x0000000000000000000000000000000000001002"
	LightClientContract        = "0x0000000000000000000000000000000000001003"
	TokenHubContract           = "0x0000000000000000000000000000000000001004"
	RelayerIncentivizeContract = "0x0000000000000000000000000000000000001005"
	RelayerHubContract         = "0x0000000000000000000000000000000000001006"
	GovHubContract             = "0x0000000000000000000000000000000000001007"
	TokenManagerContract       = "0x0000000000000000000000000000000000001008"
	CrossChainContract         = "0x0000000000000000000000000000000000002000"
)

// chiliz v2 contacts
const (
	ContractDeployerContract = "0x0000000000000000000000000000000000007001"
	GovernanceContract       = "0x0000000000000000000000000000000000007002"
)

var ContractDeployerContractAddress = common.HexToAddress(ContractDeployerContract)

func IsContractDeployer(address common.Address) bool {
	return bytes.Equal(address.Bytes(), ContractDeployerContractAddress.Bytes())
}

//go:embed abi/IEvmHooks.json
var evmHooksAbi []byte

func loadJsonAbiOrFatal(jsonAbi []byte) abi.ABI {
	result, err := abi.JSON(bytes.NewReader(jsonAbi))
	if err != nil {
		log.Fatalf("can't load abi file: %s", err)
	}
	return result
}

var (
	EvmHooksAbi = loadJsonAbiOrFatal(evmHooksAbi)
)
