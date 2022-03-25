package systemcontract

import (
	_ "embed"

	"github.com/ethereum/go-ethereum/common"
)

// BSC contracts
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

// BAS contacts
const (
	StakingPoolContract    = "0x0000000000000000000000000000000000007001"
	GovernanceContract     = "0x0000000000000000000000000000000000007002"
	ChainConfigContract    = "0x0000000000000000000000000000000000007003"
	RuntimeUpgradeContract = "0x0000000000000000000000000000000000007004"
	DeployerProxyContract  = "0x0000000000000000000000000000000000007005"
)

var (
	StakingPoolContractAddress    = common.HexToAddress(StakingPoolContract)
	GovernanceContractAddress     = common.HexToAddress(GovernanceContract)
	ChainConfigContractAddress    = common.HexToAddress(ChainConfigContract)
	RuntimeUpgradeContractAddress = common.HexToAddress(RuntimeUpgradeContract)
	DeployerProxyContractAddress  = common.HexToAddress(DeployerProxyContract)
)

var systemContracts = map[common.Address]bool{
	common.HexToAddress(ValidatorContract):    true,
	common.HexToAddress(SlashContract):        true,
	common.HexToAddress(SystemRewardContract): true,
	// we don't have these smart contract for BAS, it's not strictly required to disable them since they're not deployed
	common.HexToAddress(LightClientContract):        false,
	common.HexToAddress(RelayerHubContract):         false,
	common.HexToAddress(GovHubContract):             false,
	common.HexToAddress(TokenHubContract):           false,
	common.HexToAddress(RelayerIncentivizeContract): false,
	common.HexToAddress(CrossChainContract):         false,
	// BAS smart contracts
	common.HexToAddress(StakingPoolContract):    true,
	common.HexToAddress(GovernanceContract):     true,
	common.HexToAddress(ChainConfigContract):    true,
	common.HexToAddress(RuntimeUpgradeContract): true,
	common.HexToAddress(DeployerProxyContract):  true,
}

func IsSystemContract(address common.Address) bool {
	return systemContracts[address]
}

var EvmHookRuntimeUpgradeAddress = common.HexToAddress("0x0000000000000000000000000000000000007f01")
var EvmHookDeployerProxyAddress = common.HexToAddress("0x0000000000000000000000000000000000007f02")
