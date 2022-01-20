package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/trie"
	"math/big"
)

//go:embed testnet.json
var testnetGenesisConfig []byte

//go:embed mainnet.json
var mainnetGenesisConfig []byte

type artifactData struct {
	Bytecode         string `json:"bytecode"`
	DeployedBytecode string `json:"deployedBytecode"`
}

func createExtraData(validators []common.Address) []byte {
	extra := make([]byte, 32+20*len(validators)+65)
	for i, v := range validators {
		copy(extra[32+20*i:], v.Bytes())
	}
	return extra
}

type dummyChainContext struct {
}

func (d *dummyChainContext) Engine() consensus.Engine {
	return nil
}

func (d *dummyChainContext) GetHeader(h common.Hash, n uint64) *types.Header {
	return nil
}

var deployerAddress = common.HexToAddress("0x0000000000000000000000000000000000000010")
var governanceAddress = common.HexToAddress("0x0000000000000000000000000000000000000020")
var parliaAddress = common.HexToAddress("0x0000000000000000000000000000000000000030")

var systemContracts = map[common.Address]string{
	deployerAddress:   "./genesis/build/contracts/Deployer.json",
	governanceAddress: "./genesis/build/contracts/Governance.json",
	parliaAddress:     "./genesis/build/contracts/Parlia.json",
}

func simulateSystemContract(genesis *core.Genesis, systemContract common.Address, rawArtifact []byte, constructor []byte) error {
	artifact := &artifactData{}
	if err := json.Unmarshal(rawArtifact, artifact); err != nil {
		return err
	}
	bytecode := hexutil.MustDecode(artifact.Bytecode)
	ethdb := rawdb.NewDatabase(memorydb.New())
	db := state.NewDatabaseWithConfig(ethdb, &trie.Config{})
	statedb, err := state.New(common.Hash{}, db, nil)
	if err != nil {
		return err
	}
	block := genesis.ToBlock(nil)
	blockContext := core.NewEVMBlockContext(block.Header(), &dummyChainContext{}, &common.Address{})
	txContext := core.NewEVMTxContext(
		types.NewMessage(common.Address{}, &systemContract, 0, big.NewInt(0), 10_000_000, big.NewInt(0), []byte{}, nil, false),
	)
	evm := vm.NewEVM(blockContext, txContext, statedb, genesis.Config, vm.Config{})
	fullBytecode := append(bytecode, constructor...)
	_, _, err = evm.CreateWithAddress(vm.AccountRef(common.Address{}), fullBytecode, 10_000_000, big.NewInt(0), systemContract)
	if err != nil {
		return err
	}
	contractState := statedb.GetOrNewStateObject(systemContract)
	storage := contractState.GetDirtyStorage()
	println()
	fmt.Printf("Affected storage for contract: %s\n", systemContract.Hex())
	for key, value := range storage {
		fmt.Printf(" ~ %s -> %s\n", key.Hex(), value.Hex())
	}
	println()
	return nil
}

//go:embed build/contracts/Deployer.json
var deployerRawArtifact []byte

//go:embed build/contracts/Governance.json
var governanceRawArtifact []byte

//go:embed build/contracts/Parlia.json
var parliaRawArtifact []byte

func parseRawArtifact(rawArtifact []byte) *artifactData {
	artifact := &artifactData{}
	if err := json.Unmarshal(rawArtifact, artifact); err != nil {
		panic(err)
	}
	return artifact
}

func newArguments(typeNames ...string) abi.Arguments {
	var args abi.Arguments
	for i, tn := range typeNames {
		abiType, err := abi.NewType(tn, tn, nil)
		if err != nil {
			panic(err)
		}
		args = append(args, abi.Argument{Name: fmt.Sprintf("%d", i), Type: abiType})
	}
	return args
}

type genesisConfig struct {
	Deployers  []common.Address
	Validators []common.Address
	Owner      common.Address
}

func createGenesisConfig(rawGenesis []byte, config genesisConfig) error {
	genesis := &core.Genesis{}
	if err := json.Unmarshal(rawGenesis, genesis); err != nil {
		return err
	}
	// deployer
	{
		arguments := newArguments("address[]")
		ctor, err := arguments.Pack(config.Deployers)
		if err != nil {
			return err
		}
		if err := simulateSystemContract(genesis, deployerAddress, deployerRawArtifact, ctor); err != nil {
			return err
		}
	}
	// governance
	{
		arguments := newArguments("address")
		ctor, err := arguments.Pack(config.Owner)
		if err != nil {
			return err
		}
		if err := simulateSystemContract(genesis, governanceAddress, governanceRawArtifact, ctor); err != nil {
			return err
		}
	}
	// parlia
	{
		arguments := newArguments("address[]")
		ctor, err := arguments.Pack(config.Validators)
		if err != nil {
			return err
		}
		if err := simulateSystemContract(genesis, parliaAddress, parliaRawArtifact, ctor); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	config := genesisConfig{
		Deployers: []common.Address{
			common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
		},
		Validators: []common.Address{
			common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
		},
		Owner: common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
	}
	if err := createGenesisConfig(testnetGenesisConfig, config); err != nil {
		panic(err)
	}
}
