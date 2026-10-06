package parlia

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// dragon8ChainContext resolves every lookup to the same parent header, which is all
// distributeIncoming needs. Kept to exactly the six interface methods, so an upstream
// merge that grows core.ChainContext breaks the build instead of skipping these tests.
type dragon8ChainContext struct {
	config *params.ChainConfig
	parent *types.Header
	engine consensus.Engine
}

func (c *dragon8ChainContext) Config() *params.ChainConfig                 { return c.config }
func (c *dragon8ChainContext) CurrentHeader() *types.Header                { return c.parent }
func (c *dragon8ChainContext) GetHeader(common.Hash, uint64) *types.Header { return c.parent }
func (c *dragon8ChainContext) GetHeaderByNumber(uint64) *types.Header      { return c.parent }
func (c *dragon8ChainContext) GetHeaderByHash(common.Hash) *types.Header   { return c.parent }
func (c *dragon8ChainContext) Engine() consensus.Engine                    { return c.engine }

// dragon8TestConfig builds a Chiliz-shaped config by hand rather than from
// ParliaTestChainConfig, which the Chiliz BAS ABI cannot serve (CLAUDE.md, COR-39).
// Blob-scheduled forks stay unset: once Cancun is active core.NewEVMBlockContext
// reaches eip4844.CalcBlobFee and dereferences the blob schedule.
func dragon8TestConfig(dragon8Time, dragon8FixTime *uint64) *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:             big.NewInt(88888),
		HomesteadBlock:      big.NewInt(0),
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		BerlinBlock:         big.NewInt(0),
		LondonBlock:         big.NewInt(0),
		RamanujanBlock:      big.NewInt(0),
		Dragon8Time:         dragon8Time,
		Dragon8FixTime:      dragon8FixTime,
		Parlia:              &params.ParliaConfig{Period: 3, Epoch: 200},
	}
}

// newDragon8TestEngine builds a Parlia whose ethAPI is nil on purpose: any code path
// that reaches an eth_call nil-dereferences, which is the signal these tests rely on.
func newDragon8TestEngine(t *testing.T, config *params.ChainConfig) *Parlia {
	t.Helper()
	tABI, err := abi.JSON(strings.NewReader(tokenomicsABI))
	if err != nil {
		t.Fatalf("cannot parse tokenomics ABI: %v", err)
	}
	return &Parlia{
		chainConfig:   config,
		tokenomicsABI: tABI,
		signer:        types.LatestSigner(config),
		ethAPI:        nil,
	}
}

func newDragon8TestState(t *testing.T) *state.StateDB {
	t.Helper()
	sdb, err := state.New(types.EmptyRootHash,
		state.NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil))
	if err != nil {
		t.Fatalf("cannot create state: %v", err)
	}
	return sdb
}

func dragon8HeaderPair(blockTime uint64) (parent, header *types.Header) {
	coinbase := common.HexToAddress("0x8d9B6aB3Fe8EbF16d9242e48feFB89360fa62820")
	parent = &types.Header{
		Number:     big.NewInt(41),
		Time:       blockTime - 3,
		Coinbase:   coinbase,
		Difficulty: big.NewInt(2),
		GasLimit:   30_000_000,
	}
	header = &types.Header{
		Number:     big.NewInt(42),
		ParentHash: parent.Hash(),
		Time:       blockTime,
		Coinbase:   coinbase,
		Difficulty: big.NewInt(2),
		GasLimit:   30_000_000,
	}
	return parent, header
}

// runDistributeIncoming reports whether the Tokenomics eth_call was reached. ethAPI is
// nil, so reaching it panics; the stack is checked so only a panic that came through
// getLastSupplyFromTokenomics counts, and any other panic is re-raised as broken setup.
func runDistributeIncoming(p *Parlia, state *state.StateDB, header *types.Header,
	chain *dragon8ChainContext, txs *[]*types.Transaction) (reachedEthAPI bool, err error) {
	var (
		receipts []*types.Receipt
		usedGas  uint64
	)
	defer func() {
		if r := recover(); r != nil {
			if strings.Contains(string(debug.Stack()), "getLastSupplyFromTokenomics") {
				reachedEthAPI = true
				return
			}
			panic(r)
		}
	}()
	err = p.distributeIncoming(header.Coinbase, state, header, chain,
		txs, &receipts, nil, &usedGas, systemTxPacking, nil)
	return reachedEthAPI, err
}

// Under Dragon8Fix the mint comes from getNewSupplyForBlockDragon8Fix's fixed table, so
// distributeIncoming must never reach getLastSupplyFromTokenomics (COR-184). ethAPI is
// nil, so hoisting that read above the IsDragon8Fix/IsDragon8 branch fails here.
func TestDistributeIncomingDragon8FixSkipsTokenomicsRead(t *testing.T) {
	dragon8FixTime := uint64(1_000)
	config := dragon8TestConfig(nil, &dragon8FixTime) // Dragon8Time nil, as on mainnet
	p := newDragon8TestEngine(t, config)

	parent, header := dragon8HeaderPair(dragon8FixTime + 100)
	if !config.IsDragon8Fix(header.Time) || config.IsDragon8(header.Time) {
		t.Fatal("test setup: want Dragon8Fix active and legacy Dragon8 inactive")
	}

	var txs []*types.Transaction
	chain := &dragon8ChainContext{config: config, parent: parent, engine: p}
	reachedEthAPI, err := runDistributeIncoming(p, newDragon8TestState(t), header, chain, &txs)
	if reachedEthAPI {
		t.Fatal("distributeIncoming reached the Tokenomics eth_call under Dragon8Fix (COR-184 regression)")
	}
	if err != nil {
		t.Fatalf("distributeIncoming under Dragon8Fix: %v", err)
	}

	// The deposit must still be emitted, carrying the fixed schedule's year-0 row.
	if len(txs) != 1 {
		t.Fatalf("expected exactly 1 Tokenomics system tx, got %d", len(txs))
	}
	if to := txs[0].To(); to == nil || *to != systemcontract.TokenomicsContractAddress {
		t.Fatalf("system tx target = %v, want Tokenomics %v", to, systemcontract.TokenomicsContractAddress)
	}
	wantInflationPct, wantSupply, wantAmount := getNewSupplyForBlockDragon8Fix(dragon8FixTime, header.Time)
	if txs[0].Value().Cmp(wantAmount) != 0 {
		t.Errorf("minted amount = %s, want %s", txs[0].Value(), wantAmount)
	}

	args, err := p.tokenomicsABI.Methods["deposit"].Inputs.Unpack(txs[0].Data()[4:])
	if err != nil {
		t.Fatalf("cannot unpack deposit calldata: %v", err)
	}
	if got := args[0].(common.Address); got != header.Coinbase {
		t.Errorf("deposit validator = %v, want %v", got, header.Coinbase)
	}
	if got := args[1].(*big.Int); got.Cmp(wantSupply) != 0 {
		t.Errorf("deposit newTotalSupply = %s, want %s", got, wantSupply)
	}
	if got := args[2].(*big.Int); got.Cmp(wantInflationPct) != 0 {
		t.Errorf("deposit inflationPct = %s, want %s", got, wantInflationPct)
	}
}

// The read must be kept, not deleted: spicy still replays its historical
// dragon8Time -> dragon8FixTime window, where lastSupply feeds the mint, so a failed
// read must stay a hard failure rather than mint from a zero default (COR-184).
func TestDistributeIncomingLegacyDragon8StillReadsTokenomics(t *testing.T) {
	dragon8Time := uint64(1_000)
	farFuture := uint64(9_000_000_000)
	config := dragon8TestConfig(&dragon8Time, &farFuture) // Dragon8 active, Fix not yet
	p := newDragon8TestEngine(t, config)

	parent, header := dragon8HeaderPair(dragon8Time + 100)
	if config.IsDragon8Fix(header.Time) || !config.IsDragon8(header.Time) {
		t.Fatal("test setup: want legacy Dragon8 active and Dragon8Fix inactive")
	}

	var txs []*types.Transaction
	chain := &dragon8ChainContext{config: config, parent: parent, engine: p}
	reachedEthAPI, err := runDistributeIncoming(p, newDragon8TestState(t), header, chain, &txs)
	// Require the read specifically: a bare non-nil err would also be satisfied by an
	// unrelated failure after the read had been deleted.
	if !reachedEthAPI {
		t.Fatalf("legacy Dragon8 minted without consulting the Tokenomics supply; "+
			"the read must stay on this path (COR-184) [distributeIncoming err: %v]", err)
	}
	if len(txs) != 0 {
		t.Fatalf("no deposit may be emitted when the supply read fails, got %d txs", len(txs))
	}
}

// A source-level tripwire, because asserting which state the eth_call resolved would
// need a stub of the ~55-method ethapi.Backend that upstream syncs would keep breaking.
// By-number resolution puts a node-local index gap back on the path out of Finalize.
func TestGetLastSupplyFromTokenomicsPinsParentByHash(t *testing.T) {
	const target = "getLastSupplyFromTokenomics"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "parlia.go", nil, 0)
	if err != nil {
		t.Fatalf("cannot parse parlia.go: %v", err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == target && fn.Recv != nil {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatalf("method %s not found in parlia.go; update this guard alongside the rename", target)
	}

	var byNumber, byParentHash, requireCanonical bool
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "BlockNumberOrHashWithNumber":
			byNumber = true
		case "BlockNumberOrHashWithHash":
			// Must be exactly (header.ParentHash, false): RequireCanonical=true brings
			// the dependency back, failing with "hash is not currently canonical"
			// whenever the parent is not canonical at its height.
			if len(call.Args) != 2 {
				return true
			}
			field, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok || field.Sel.Name != "ParentHash" {
				return true
			}
			if recv, ok := field.X.(*ast.Ident); !ok || recv.Name != "header" {
				return true
			}
			if flag, ok := call.Args[1].(*ast.Ident); ok && flag.Name == "false" {
				byParentHash = true
			} else {
				requireCanonical = true
			}
		}
		return true
	})

	if byNumber {
		t.Errorf("%s resolves state by block number (COR-184 regression); "+
			"use rpc.BlockNumberOrHashWithHash(header.ParentHash, false)", target)
	}
	if requireCanonical {
		t.Errorf("%s must pass RequireCanonical=false; true reintroduces the "+
			"canonical-index dependency COR-184 removes", target)
	}
	if !byParentHash {
		t.Errorf("%s must pin its eth_call to header.ParentHash (COR-184)", target)
	}
}
