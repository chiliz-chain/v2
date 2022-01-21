package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"io/ioutil"
	"math/big"

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
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

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
var systemAddress = common.HexToAddress("0xfffffffffffffffffffffffffffffffffffffffe")

func simulateSystemContract(genesis *core.Genesis, systemContract common.Address, rawArtifact []byte, constructor []byte) error {
	artifact := &artifactData{}
	if err := json.Unmarshal(rawArtifact, artifact); err != nil {
		return err
	}
	bytecode := append(hexutil.MustDecode(artifact.Bytecode), constructor...)
	// simulate constructor execution
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
	deployedBytecode, _, err := evm.CreateWithAddress(vm.AccountRef(common.Address{}), bytecode, 10_000_000, big.NewInt(0), systemContract)
	if err != nil {
		return err
	}
	contractState := statedb.GetOrNewStateObject(systemContract)
	storage := contractState.GetDirtyStorage()
	// read state changes from state database
	genesisAccount := core.GenesisAccount{
		Code:    deployedBytecode,
		Storage: storage,
		Balance: big.NewInt(0),
		Nonce:   0,
	}
	fmt.Printf("Affected storage for contract: %s\n", systemContract.Hex())
	for key, value := range storage {
		fmt.Printf(" ~ %s -> %s\n", key.Hex(), value.Hex())
	}
	if genesis.Alloc == nil {
		genesis.Alloc = make(core.GenesisAlloc)
	}
	genesis.Alloc[systemContract] = genesisAccount
	return nil
}

//go:embed build/contracts/Deployer.json
var deployerRawArtifact []byte

//go:embed build/contracts/Governance.json
var governanceRawArtifact []byte

//go:embed build/contracts/Parlia.json
var parliaRawArtifact []byte

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
	Genesis         *core.Genesis
	Deployers       []common.Address
	Validators      []common.Address
	GovernanceOwner common.Address
	VotingPeriod    int64
	Faucet          map[common.Address]string
}

func createGenesisConfig(config genesisConfig, targetFile string) error {
	genesis := config.Genesis
	// extra data
	genesis.ExtraData = createExtraData(config.Validators)
	// execute system contracts
	ctor, err := newArguments("address[]").Pack(config.Deployers)
	if err != nil {
		return err
	}
	if err := simulateSystemContract(genesis, deployerAddress, deployerRawArtifact, ctor); err != nil {
		return err
	}
	ctor, err = newArguments("address", "uint256").Pack(config.GovernanceOwner, big.NewInt(config.VotingPeriod))
	if err != nil {
		return err
	}
	if err := simulateSystemContract(genesis, governanceAddress, governanceRawArtifact, ctor); err != nil {
		return err
	}
	ctor, err = newArguments("address[]").Pack(config.Validators)
	if err != nil {
		return err
	}
	if err := simulateSystemContract(genesis, parliaAddress, parliaRawArtifact, ctor); err != nil {
		return err
	}
	// create system contract
	genesis.Alloc[systemAddress] = core.GenesisAccount{
		Balance: big.NewInt(0),
	}
	// apply faucet
	for key, value := range config.Faucet {
		balance, ok := new(big.Int).SetString(value[2:], 16)
		if !ok {
			return fmt.Errorf("failed to parse number (%s)", value)
		}
		genesis.Alloc[key] = core.GenesisAccount{
			Balance: balance,
		}
	}
	// save to file
	newJson, _ := json.MarshalIndent(genesis, "", "  ")
	return ioutil.WriteFile(targetFile, newJson, fs.ModePerm)
}

func defaultGenesisConfig(chainId int64) *core.Genesis {
	chainConfig := &params.ChainConfig{
		ChainID:             big.NewInt(chainId),
		HomesteadBlock:      big.NewInt(0),
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		MuirGlacierBlock:    big.NewInt(0),
		RamanujanBlock:      big.NewInt(0),
		NielsBlock:          big.NewInt(0),
		MirrorSyncBlock:     big.NewInt(0),
		BrunoBlock:          big.NewInt(0),
		Parlia: &params.ParliaConfig{
			Period: 3,
			Epoch:  200,
		},
	}
	return &core.Genesis{
		Config:     chainConfig,
		Nonce:      0,
		Timestamp:  0x5e9da7ce,
		ExtraData:  nil,
		GasLimit:   0x2625a00,
		Difficulty: big.NewInt(0x01),
		Mixhash:    common.Hash{},
		Coinbase:   common.Address{},
		Alloc:      nil,
		Number:     0x00,
		GasUsed:    0x00,
		ParentHash: common.Hash{},
	}
}

var testnetConfig = genesisConfig{
	Genesis: defaultGenesisConfig(17242),
	// who is able to deploy smart contract from genesis block
	Deployers: []common.Address{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
		common.HexToAddress("0xbAdCab1E02FB68dDD8BBB0A45Cc23aBb60e174C8"),
		common.HexToAddress("0xEbCf9D06cf9333706E61213F17A795B2F7c55F1b"),
		common.HexToAddress("0x57BA24bE2cF17400f37dB3566e839bfA6A2d018a"),
	},
	// list of default validators
	Validators: []common.Address{
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
	},
	// owner of the governance
	GovernanceOwner: common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"),
	VotingPeriod:    60, // 3 minutes
	// faucet
	Faucet: map[common.Address]string{
		common.HexToAddress("0x86d274133714A88CE821F279e5eD3fb0BfB42503"): "0x21e19e0c9bab2400000",
		common.HexToAddress("0x00a601f45688dba8a070722073b015277cf36725"): "0x21e19e0c9bab2400000",
		common.HexToAddress("0xbAdCab1E02FB68dDD8BBB0A45Cc23aBb60e174C8"): "0x21e19e0c9bab2400000",
		common.HexToAddress("0x57BA24bE2cF17400f37dB3566e839bfA6A2d018a"): "0x21e19e0c9bab2400000",
		common.HexToAddress("0xEbCf9D06cf9333706E61213F17A795B2F7c55F1b"): "0x21e19e0c9bab2400000",
	},
}

func main() {
	if err := createGenesisConfig(testnetConfig, "testnet.json"); err != nil {
		panic(err)
	}
}
