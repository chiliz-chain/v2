package systemcontracts

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
)

var (
	DeployerContract   = common.HexToAddress("0x0000000000000000000000000000000000000010")
	GovernanceContract = common.HexToAddress("0x0000000000000000000000000000000000000020")
	ParliaContract     = common.HexToAddress("0x0000000000000000000000000000000000000030")
)

func IsSystemContract(address common.Address) bool {
	return bytes.Equal(DeployerContract.Bytes(), address.Bytes()) ||
		bytes.Equal(GovernanceContract.Bytes(), address.Bytes()) ||
		bytes.Equal(ParliaContract.Bytes(), address.Bytes())
}
