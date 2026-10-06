package parlia

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/common/pepper8"
	"github.com/ethereum/go-ethereum/common/pipe8"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// COR-235: fuzz the mint *accounting* of distributeIncoming (CLAUDE.md §2).
//
// distributeIncoming is where every CHZ the protocol ever mints is credited: the
// Dragon8 / Dragon8Fix per-block inflation, the Pepper8 and Pipe8 one-time mints,
// and, on top of all that, BSC's own gas-fee redistribution. CLAUDE.md calls it
// "the single most conflict-prone function" in the repo, and until this target it
// had 0% fuzz coverage.
//
// The neighbouring wave-one targets cover the pieces:
//
//	COR-211 inflation_fuzz_test.go      the pure arithmetic (getInflationPct,
//	                                    getNewSupplyForBlock*, the golden schedule)
//	COR-216 fork_boundary_fuzz_test.go  IsPepper8Block / IsPipe8Block fire once
//	COR-214 systemtx_fuzz_test.go       what counts as a system transaction
//
// This one covers the layer above them: given a fork combination and a supply
// read, *which* branch runs, *how much* balance it creates, *where* that balance
// ends up, and whether the `deposit` calldata handed to the Tokenomics contract
// describes the same mint that was actually credited. It reuses COR-211's golden
// schedule table (dragon8FixRow / checkDragon8FixRowConsistency) and COR-214's
// fuzzAddrFromSeed rather than restating them.
//
// The harness follows the COR-217 "stub, not a chain" pattern: a real
// state.StateDB wrapped in a recording ledger, a stub ethapi backend answering
// getTotalSupply from a hand-assembled contract, a two-header chain reader. No
// blockchain, no genesis, no block import.
//
// Properties (numbered as in COR-235):
//
//  1. Conservation — the balance the block creates is exactly what the branch's
//     schedule prescribes, no more, no less, and it lands in the prescribed
//     accounts. Gas fees are moved, never created.
//  2. Exactly one mint branch per block — Dragon8Fix excludes Dragon8; the
//     one-time mints fire only on their transition block.
//  3. No overflow, no negative balance — including a supply read of 0 and of
//     2^256-1.
//  4. The supply read is hard-fail, parent-hash pinned and branch-confined
//     (COR-184).
//  5. Under Dragon8 the BSC system-reward split is skipped entirely and the
//     minted amount leaves the coinbase for the Tokenomics contract.
//  6. The deposit calldata matches the mint (with COR-219's known-bad rows
//     pinned as exceptions, never tolerated), and the ValidatorSet deposit names
//     the block's validator rather than the coinbase.
//  7. Every emitted system transaction has the shape IsSystemTransaction accepts,
//     so eth/tracers/api.go and eth/state_accessor.go mirror it identically.

// ---------------------------------------------------------------------------
// Fixed points of the harness
// ---------------------------------------------------------------------------

const (
	// distBase is the timestamp all fuzzed times are expressed relative to. It
	// sits in the same era as the real Chiliz fork times, so a fuzzed int32
	// offset can reach ~68 years either side of them.
	distBase = uint64(1_700_000_000)
	// distBlockNumber is the height of the block under test. Nothing in
	// distributeIncoming is height-dependent (every Chiliz mint is
	// timestamp-gated), so it is fixed.
	distBlockNumber = int64(1000)
	// distGasLimit / distBaseFee give the header realistic non-zero values.
	// distBaseFee is InitialBaseFeeForBSC (2,500 Gwei), Chiliz's floor -- NOT
	// params.InitialBaseFee, which is upstream's 1 Gwei default (CLAUDE.md §3).
	distGasLimit = uint64(100_000_000)
	distBaseFee  = int64(params.InitialBaseFeeForBSC)
	// distKeys is the number of coinbase keys precomputed once per process.
	// Deriving a secp256k1 key costs more than the rest of an exec put together,
	// and the identity of the coinbase is not what this target varies.
	distKeys = 8
)

// Fork bits of the fuzzed forkMask. A fork is "scheduled" when its bit is set;
// whether it is *active* at the block under test is then decided by comparing
// its fuzzed timestamp with the fuzzed header/parent timestamps.
const (
	distForkDragon8 uint8 = 1 << iota
	distForkDragon8Fix
	distForkPepper8
	distForkPipe8
)

// ---------------------------------------------------------------------------
// Golden values -- deliberately duplicated from production, never imported
// ---------------------------------------------------------------------------
//
// An oracle that reads the constant it claims to pin cannot catch a change to
// that constant: production and expectation move together and every fuzz case
// still passes. The values below are therefore written out as literals, in the
// style of COR-211's dragon8FixGolden, and are the *only* source the
// expectations use. Each is consensus-critical and each is exactly the shape an
// upstream BSC merge can silently revert, so a divergence from production must
// surface as a test failure rather than as a quietly relabelled oracle.
//
// Reviewing a change here: if a diff touches these literals, the question is
// never "does the test still pass" but "was the production constant meant to
// change, and on which fork".
const (
	// goldenPepper8Mint is the Pepper8 one-time mint, 148,600,000 CHZ in wei
	// (CLAUDE.md §2: "148.6M for Pepper8"). Production holds the same number as
	// pepper8MintAmount in pepper8Fork.go -- do not substitute that constant.
	goldenPepper8Mint = "148600000000000000000000000"
	// goldenPipe8Mint is the Pipe8 one-time mint, 455,700,467 CHZ in wei
	// (CLAUDE.md §2: "~455.7M for Pipe8"). Production: pipe8MintAmount.
	goldenPipe8Mint = "455700467000000000000000000"
	// goldenMintRecipient is the account both one-time mints are forwarded to.
	// CLAUDE.md §2 names it inline ("the recipient address
	// (0xE0d17A41C1A4Fe527e375C644F9D2A02e96111ED)"), which is what anchors this
	// literal outside the code it pins. Production holds the same address twice,
	// as pepper8.Pepper8RecipientAddress and pipe8.Pipe8RecipientAddress.
	//
	// It was the last expected value in this target still read from production:
	// the expectation credited the production constant, and
	// TestMintRecipientsAreOneAddress compared the two production constants to
	// *each other*, so editing both packages moved production and oracle in
	// lockstep and left every case green -- the mint would have been forwarded
	// to a new address with nothing to say so.
	goldenMintRecipient = "0xE0d17A41C1A4Fe527e375C644F9D2A02e96111ED"

	// goldenProxyAddress and goldenProxyRuntimeCode are the deterministic
	// deployment proxy Pepper8 installs. They are not Chiliz values at all: they
	// are the cross-chain deployment proxy (Arachnid's "deterministic deployment
	// proxy", the one forge and hardhat use for CREATE2), which is why the golden
	// copy can be stated independently of anything in this repo.
	//
	// The address is fixed by the proxy's own deployment transaction, and every
	// CREATE2 address anyone has ever computed against it assumes these exact 69
	// bytes of runtime code at that exact address. Anything else there -- valid
	// bytecode included -- silently changes every CREATE2 deployment on the chain,
	// which is why "the account has some code" is not an acceptable check.
	// TestDeterministicDeploymentProxyGolden re-derives the well-known codehash
	// from this literal, so the golden itself is anchored outside this repo.
	goldenProxyAddress     = "0x4e59b44847b379578588920cA78FbF26c0B4956C"
	goldenProxyRuntimeCode = "7fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe03601600081602082378035828234f58015156039578182fd5b8082525050506014600cf3"
	// goldenProxyCodeHash is keccak256(goldenProxyRuntimeCode), the published
	// codehash of the deterministic deployment proxy on every chain it exists on.
	goldenProxyCodeHash = "0x2fa86add0aed31f33a762c9d88e807c475bd51d0f52bd0955754b2608f7e4989"

	// goldenSystemRewardDivisor is Chiliz's systemRewardPercent: gas fees are
	// split 1/5 to the SystemReward pool, 4/5 to the validator set. **BSC uses 4**,
	// and CLAUDE.md §2 names this divergence as one that must survive every
	// upstream merge -- a merge that restores upstream's 4 is the specific,
	// realistic regression this literal exists to catch.
	goldenSystemRewardDivisor = 5
	// goldenMaxSystemBalance is maxSystemBalance, the 100 CHZ (1e20 wei) ceiling
	// above which the SystemReward pool stops being topped up and the whole fee
	// split is skipped.
	goldenMaxSystemBalance = "100000000000000000000"
)

var (
	distSystemReward = common.HexToAddress(systemcontract.SystemRewardContract)
	distValidatorSet = common.HexToAddress(systemcontract.ValidatorContract)
	// Derived from the golden literal, not from deterministicDeploymentProxyAddress:
	// a production change of the address leaves the golden account codeless and
	// fails the proxy check, instead of following the change.
	distProxyAddress = common.HexToAddress(goldenProxyAddress)
	// Likewise derived from the golden literal, not from
	// pepper8.Pepper8RecipientAddress: every expectation about where the one-time
	// mints land names this account, so a production recipient that moves shows
	// up as a mint that never arrived (and as a credit to an account no branch
	// names), instead of being followed silently.
	distMintRecipient = common.HexToAddress(goldenMintRecipient)
)

// ---------------------------------------------------------------------------
// The recording ledger (the "stub state database" of the COR-217 pattern)
// ---------------------------------------------------------------------------

// mintLedgerEntry is one balance movement, signed, with the reason the caller
// gave. Amounts are copied out of the uint256 the caller passed: several of the
// values that flow through distributeIncoming are live pointers into state
// (GetBalance) that later code mutates in place (balance.Sub in the system-reward
// split), so recording the pointer would silently rewrite history.
//
// The reason is not decoration: it is what a tracer sees. The consensus-level
// mints and the fee sweep are made by distributeIncoming itself and carry
// tracing.BalanceChangeUnspecified; the value each system transaction then
// moves is made by core.Transfer inside evm.Call and carries
// tracing.BalanceChangeTransfer. Which of the two a movement is recorded under
// is a tracing-visible difference (CLAUDE.md §5), so distCheckMintReasons
// asserts on it rather than the field being recorded and forgotten.
type mintLedgerEntry struct {
	addr   common.Address
	delta  *big.Int
	reason tracing.BalanceChangeReason
}

// mintLedger is a vm.StateDB that records every balance movement and delegates
// everything else to a real state.StateDB. Embedding the concrete StateDB (rather
// than hand-writing the ~50-method interface) means the EVM that executes the
// emitted system transactions runs against real semantics, while every credit and
// debit -- whether made directly by distributeIncoming or by core.Transfer inside
// evm.Call -- lands in one ordered ledger.
type mintLedger struct {
	*state.StateDB
	entries []mintLedgerEntry
}

func (l *mintLedger) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	l.entries = append(l.entries, mintLedgerEntry{addr, amount.ToBig(), reason})
	return l.StateDB.AddBalance(addr, amount, reason)
}

func (l *mintLedger) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	l.entries = append(l.entries, mintLedgerEntry{addr, new(big.Int).Neg(amount.ToBig()), reason})
	return l.StateDB.SubBalance(addr, amount, reason)
}

func (l *mintLedger) SetBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) {
	delta := new(big.Int).Sub(amount.ToBig(), l.StateDB.GetBalance(addr).ToBig())
	l.entries = append(l.entries, mintLedgerEntry{addr, delta, reason})
	l.StateDB.SetBalance(addr, amount, reason)
}

// net returns the signed total the ledger recorded for one address.
func (l *mintLedger) net(addr common.Address) *big.Int {
	sum := new(big.Int)
	for _, e := range l.entries {
		if e.addr == addr {
			sum.Add(sum, e.delta)
		}
	}
	return sum
}

// created returns the signed total over every address: the balance the block
// brought into existence. Moving value between accounts nets to zero, so this is
// exactly the mint.
func (l *mintLedger) created() *big.Int {
	sum := new(big.Int)
	for _, e := range l.entries {
		sum.Add(sum, e.delta)
	}
	return sum
}

// createdUnder is created() restricted to the entries recorded under one
// balance-change reason.
func (l *mintLedger) createdUnder(reason tracing.BalanceChangeReason) *big.Int {
	sum := new(big.Int)
	for _, e := range l.entries {
		if e.reason == reason {
			sum.Add(sum, e.delta)
		}
	}
	return sum
}

// touched returns every address the ledger mentions, in first-touch order.
func (l *mintLedger) touched() []common.Address {
	var (
		seen = make(map[common.Address]bool)
		out  []common.Address
	)
	for _, e := range l.entries {
		if !seen[e.addr] {
			seen[e.addr] = true
			out = append(out, e.addr)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The stub ethapi backend behind getLastSupplyFromTokenomics
// ---------------------------------------------------------------------------

// distSupplyBackend is an ethapi.Backend that answers exactly one question:
// the eth_call getLastSupplyFromTokenomics makes against the Tokenomics contract.
// Everything else is left to the embedded nil interface: reaching any other
// method is a nil dereference, which is the point -- consensus must not be
// making other RPC calls from here.
//
// It also records how the caller asked for its state, which is what makes
// COR-184 testable: `queries` holds every rpc.BlockNumberOrHash the consensus
// path resolved, so the target can assert both the count (branch confinement)
// and the shape (hash-pinned, never by number).
type distSupplyBackend struct {
	ethapi.Backend

	config  *params.ChainConfig
	engine  consensus.Engine
	header  *types.Header
	supply  *big.Int
	fail    bool
	queries []rpc.BlockNumberOrHash
}

func (b *distSupplyBackend) ChainConfig() *params.ChainConfig { return b.config }
func (b *distSupplyBackend) Engine() consensus.Engine         { return b.engine }
func (b *distSupplyBackend) CurrentHeader() *types.Header     { return b.header }
func (b *distSupplyBackend) RPCGasCap() uint64                { return 0 }
func (b *distSupplyBackend) RPCEVMTimeout() time.Duration     { return 0 }
func (b *distSupplyBackend) HeaderByNumber(_ context.Context, _ rpc.BlockNumber) (*types.Header, error) {
	return b.header, nil
}

func (b *distSupplyBackend) GetEVM(_ context.Context, statedb *state.StateDB, _ *types.Header, vmConfig *vm.Config, blockCtx *vm.BlockContext) *vm.EVM {
	return vm.NewEVM(*blockCtx, statedb, b.config, *vmConfig)
}

// StateAndHeaderByNumberOrHash is the seam. A failing read reproduces the
// COR-184 fingerprint exactly: "header not found" is the error
// StateAndHeaderByNumber returns when the canonical number->hash index does not
// cover the requested height, which is the node-local condition that condemned
// mainnet block 35,890,218 on one RPC node for three weeks.
func (b *distSupplyBackend) StateAndHeaderByNumberOrHash(_ context.Context, blockNrOrHash rpc.BlockNumberOrHash) (*state.StateDB, *types.Header, error) {
	b.queries = append(b.queries, blockNrOrHash)
	if b.fail {
		return nil, nil, errors.New("header not found")
	}
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		return nil, nil, err
	}
	statedb.SetCode(systemcontract.TokenomicsContractAddress, distSupplyCode(b.supply), tracing.CodeChangeUnspecified)
	return statedb, b.header, nil
}

// distSupplyCode hand-assembles a getTotalSupply() stand-in: it ignores its
// calldata and returns one 32-byte word.
//
//	PUSH32 <supply> PUSH1 0x00 MSTORE PUSH1 0x20 PUSH1 0x00 RETURN
func distSupplyCode(supply *big.Int) []byte {
	var word [32]byte
	supply.FillBytes(word[:]) // every fuzzed supply is < 2^256; see distSupply
	code := append([]byte{byte(vm.PUSH32)}, word[:]...)
	return append(code,
		byte(vm.PUSH1), 0x00, byte(vm.MSTORE),
		byte(vm.PUSH1), 0x20, byte(vm.PUSH1), 0x00, byte(vm.RETURN))
}

// distSystemDestCode is the runtime code the harness installs at each system
// transaction's destination so that executing one actually costs gas.
//
// Without it every destination is codeless, evm.Call returns immediately, and
// applyTransaction records gasUsed = 0 for every system transaction -- which
// makes "the receipts sum to usedGas" compare 0 against 0 and hold for any gas
// accounting whatsoever (doubling every contribution leaves it green). n
// JUMPDESTs cost n gas, always succeed (execution falls off the end, which is a
// STOP) and return nothing, so the value transfer and every balance expectation
// are untouched while the gas the block charges becomes a number that can
// disagree.
func distSystemDestCode(n int) []byte {
	code := make([]byte, n)
	for i := range code {
		code[i] = byte(vm.JUMPDEST)
	}
	return code
}

// distSystemDestGas gives every possible destination its own cost, so that gas
// says *which* system transactions ran rather than merely how many.
//
// distCheckSystemTxs asserts this per receipt: a system transaction is charged
// no intrinsic gas, so its gasUsed is exactly the JUMPDEST count installed at
// its destination, and a transaction that went somewhere else reports somebody
// else's number. The summed form is then checked against both that expectation
// and usedGas.
//
// Per receipt, not only summed, and deliberately: pepper8 and pipe8 share a
// recipient and therefore a cost, so the multiset of costs a block can produce
// does not have unique sums (3+3+5 collides with 11). The four costs are
// distinct so that each *receipt* is attributable; uniqueness of the total is
// not a property this map has, and the summed check leans on the per-receipt one
// rather than the other way round.
//
// The deterministic deployment proxy's address is deliberately absent: Pepper8
// installs the real 69 bytes there and the target compares them byte for byte.
// It is never a system-transaction destination, and distCheckSystemTxs fails
// loudly if an expected destination is missing from this map rather than
// silently scoring it 0.
var distSystemDestGas = map[common.Address]int{
	distValidatorSet:                         11,
	distSystemReward:                         7,
	systemcontract.TokenomicsContractAddress: 5,
	distMintRecipient:                        3,
}

// ---------------------------------------------------------------------------
// Harness construction
// ---------------------------------------------------------------------------

// distHarness is the per-process fixture: one engine, one backend, one set of
// coinbase keys. The chain config is swapped per exec (every fork combination
// needs its own), which is safe because everything New() derives from the config
// -- the signer, the Parlia sub-config -- is held fixed across execs.
type distHarness struct {
	engine  *Parlia
	backend *distSupplyBackend
	keys    [distKeys]*ecdsa.PrivateKey
	addrs   [distKeys]common.Address
}

// distParliaConfig is the one ParliaConfig every exec shares. New() writes
// Epoch into the package-level defaultEpochLength, so keeping it constant keeps
// that global stable for the rest of the package's tests.
var distParliaConfig = &params.ParliaConfig{Period: 3, Epoch: 200}

// newDistHarness builds the engine. Note the knot: the backend needs the engine
// (ethapi's ChainContext asks it for Engine().Author()), and the engine needs the
// ethapi built on the backend. It is tied by constructing the backend first and
// filling in its engine afterwards.
func newDistHarness(tb testing.TB) *distHarness {
	h := &distHarness{}
	for i := 0; i < distKeys; i++ {
		var seed [8]byte
		binary.LittleEndian.PutUint64(seed[:], uint64(i)+1)
		key, err := crypto.ToECDSA(crypto.Keccak256(seed[:]))
		if err != nil {
			tb.Fatalf("harness key %d: %v", i, err)
		}
		h.keys[i] = key
		h.addrs[i] = crypto.PubkeyToAddress(key.PublicKey)
	}
	h.backend = &distSupplyBackend{
		config: distChainConfig(0, 0, 0, 0, 0),
		header: &types.Header{
			Number:     big.NewInt(distBlockNumber - 1),
			Time:       distBase,
			Difficulty: big.NewInt(2),
			GasLimit:   distGasLimit,
		},
		supply: new(big.Int),
	}
	h.engine = New(h.backend.config, rawdb.NewMemoryDatabase(), ethapi.NewBlockChainAPI(h.backend), common.Hash{})
	h.backend.engine = h.engine
	tb.Cleanup(func() { h.engine.Close() })
	return h
}

// distChainConfig is the minimal Parlia config the harness runs on: every
// Ethereum and BSC fork the engine needs at block 0, no Plato/Feynman (so
// New()'s fork-gated bid-block ABI assertion stays off -- CLAUDE.md §2), and the
// four Chiliz mint forks scheduled per the fuzzed mask.
func distChainConfig(mask uint8, d8, d8fix, p8, pi8 uint64) *params.ChainConfig {
	cfg := &params.ChainConfig{
		ChainID:             big.NewInt(88888),
		HomesteadBlock:      common.Big0,
		EIP150Block:         common.Big0,
		EIP155Block:         common.Big0,
		EIP158Block:         common.Big0,
		ByzantiumBlock:      common.Big0,
		ConstantinopleBlock: common.Big0,
		PetersburgBlock:     common.Big0,
		IstanbulBlock:       common.Big0,
		MuirGlacierBlock:    common.Big0,
		BerlinBlock:         common.Big0,
		LondonBlock:         common.Big0,
		RamanujanBlock:      common.Big0,
		NielsBlock:          common.Big0,
		MirrorSyncBlock:     common.Big0,
		BrunoBlock:          common.Big0,
		Parlia:              distParliaConfig,
	}
	if mask&distForkDragon8 != 0 {
		v := d8
		cfg.Dragon8Time = &v
	}
	if mask&distForkDragon8Fix != 0 {
		v := d8fix
		cfg.Dragon8FixTime = &v
	}
	if mask&distForkPepper8 != 0 {
		v := p8
		cfg.Pepper8Time = &v
	}
	if mask&distForkPipe8 != 0 {
		v := pi8
		cfg.Pipe8Time = &v
	}
	return cfg
}

// distAccountName labels the accounts a block's balance can legitimately reach,
// so a conservation failure says "the coinbase kept the mint" rather than
// printing a hex address the reader then has to look up.
func distAccountName(addr, coinbase common.Address) string {
	switch addr {
	case coinbase:
		return "coinbase"
	case consensus.SystemAddress:
		return "SystemAddress (gas fees)"
	case distSystemReward:
		return "SystemReward pool"
	case distValidatorSet:
		return "ValidatorSet contract"
	case systemcontract.TokenomicsContractAddress:
		return "Tokenomics contract"
	case distMintRecipient:
		return "Pepper8/Pipe8 recipient"
	// Reached only if a production constant has moved off the golden address, in
	// which case the case above no longer matches it. Duplicate non-constant
	// cases are legal in Go and the golden one wins while they agree.
	case pepper8.Pepper8RecipientAddress:
		return "production Pepper8 recipient (diverged from the golden address)"
	case pipe8.Pipe8RecipientAddress:
		return "production Pipe8 recipient (diverged from the golden address)"
	default:
		return "unnamed account"
	}
}

// distTime turns a fuzzed int32 offset into a timestamp, clamped at 0.
func distTime(offset int32) uint64 {
	t := int64(distBase) + int64(offset)
	if t < 0 {
		return 0
	}
	return uint64(t)
}

// distSignerFn returns the SignerTxFn the mining path uses. Signing for real
// (rather than running in systemTxPacking mode, which leaves the transactions
// unsigned) is what lets property 7 put the emitted transactions through
// IsSystemTransaction, which recovers the sender.
func distSignerFn(key *ecdsa.PrivateKey, signer types.Signer) SignerTxFn {
	return func(_ accounts.Account, tx *types.Transaction, _ *big.Int) (*types.Transaction, error) {
		return types.SignTx(tx, signer, key)
	}
}

// distSupply maps the fuzzed (mode, bytes) pair to the value the Tokenomics
// contract reports. It is COR-211's helper: same biasing towards the realistic
// post-fork supply, towards zero and towards the top of the 256-bit range.
func distSupply(mode uint8, raw []byte) *big.Int {
	return inflationSupplyFromFuzz(mode, raw)
}

// distExpectation is the independent restatement of what distributeIncoming must
// do for one block. It is written from the fork times and the fuzzed inputs, not
// from the production branch structure, so a branch that moves shows up here as
// a mismatch.
type distExpectation struct {
	pepper8Fires bool
	pipe8Fires   bool
	dragon8Fix   bool // the Dragon8Fix schedule runs
	dragon8Slow  bool // the legacy Dragon8 schedule runs (and reads the supply)
	sysReward    bool // BSC's system-reward split runs

	// fixYear is the schedule row the Dragon8Fix branch resolves to; legacyPct
	// and legacySupply are the percentage and the pre-mint supply the legacy
	// branch works from. They are what the deposit calldata has to report.
	fixYear      uint64
	legacyPct    *big.Int
	legacySupply *big.Int

	mint       *big.Int // wei the block creates
	perAddress map[common.Address]*big.Int
}

func (e *distExpectation) dragon8() bool { return e.dragon8Fix || e.dragon8Slow }

// ---------------------------------------------------------------------------
// The target
// ---------------------------------------------------------------------------

func FuzzDistributeIncoming(f *testing.F) {
	// Seed arguments, in order:
	//   forkMask,
	//   d8Off, d8fixOff, p8Off, pi8Off, headerOff, parentGap,
	//   supplyMode, supplyBytes, supplyFails,
	//   gasFees, sysRewardBalance, valCount, valIdx, coinbaseSel
	const (
		// Untyped so the int32() conversions at the seed call sites below are
		// conversions of an untyped constant rather than redundant ones
		// (golangci-lint's unconvert check fails CI on the latter).
		yearSecs   = 31536000
		someFees   = uint64(3_000_000_000_000_000_000) // 3 CHZ of gas fees
		realSupply = uint8(1)                          // COR-211 mode 1: 8888888888 CHZ
	)
	maxSupply := make([]byte, 32)
	for i := range maxSupply {
		maxSupply[i] = 0xff
	}

	// --- one seed per fork combination -------------------------------------
	// No Chiliz mint fork scheduled: pure BSC behaviour (system reward + validator).
	f.Add(uint8(0), int32(0), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Dragon8 legacy active, one hour in: the only branch that reads the supply.
	f.Add(distForkDragon8, int32(-3600), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Dragon8Fix active (year 0), Dragon8 also scheduled earlier: Fix must win.
	f.Add(distForkDragon8|distForkDragon8Fix, int32(-yearSecs), int32(-3600), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Dragon8Fix in schedule year 5 -- a COR-219 known-exception row.
	f.Add(distForkDragon8Fix, int32(0), int32(-5*yearSecs), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Dragon8Fix in schedule year 6 and 7 -- the other two COR-219 rows.
	f.Add(distForkDragon8Fix, int32(0), int32(-6*yearSecs), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	f.Add(distForkDragon8Fix, int32(0), int32(-7*yearSecs), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Dragon8Fix on the last row of the 14-year table. Rows are numbered 0..13,
	// so an elapsed time of 13 years and 1 second still resolves to a real row
	// (13) -- it is the boundary, not the clamp.
	f.Add(distForkDragon8Fix, int32(0), int32(-13*yearSecs-1), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Dragon8Fix genuinely past the end of the table: 14 years elapsed is row 14,
	// which does not exist, so the schedule must clamp to row 13 forever. This is
	// the seed that separates the clamp from an out-of-range read.
	f.Add(distForkDragon8Fix, int32(0), int32(-14*yearSecs), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Dragon8Fix scheduled but not yet reached, Dragon8 already live: legacy branch.
	f.Add(distForkDragon8|distForkDragon8Fix, int32(-3600), int32(3600), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))

	// --- the one-time mints, at the boundary and either side of it ----------
	// Pepper8 transition block: parent below the fork, header at it.
	f.Add(distForkPepper8, int32(0), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// One block earlier: fork not reached, nothing minted.
	f.Add(distForkPepper8, int32(0), int32(0), int32(3), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// One block later: parent is already past the fork, so the mint already
	// happened. This is the seed that separates "fires once" from "fires forever".
	f.Add(distForkPepper8, int32(0), int32(0), int32(-3), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Long after the Pepper8 fork -- the shape a "mint every block" regression
	// produces on every block of a live chain.
	f.Add(distForkPepper8, int32(0), int32(0), int32(-yearSecs), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Pipe8 transition block, and the blocks either side of it: one before the
	// fork is reached, one after the parent has already crossed it.
	f.Add(distForkPipe8, int32(0), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	f.Add(distForkPipe8, int32(0), int32(0), int32(0), int32(3), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	f.Add(distForkPipe8, int32(0), int32(0), int32(0), int32(-3), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Both one-time mints on the same block, plus the legacy Dragon8 schedule:
	// three mints in one block, each with its own system transaction.
	f.Add(distForkDragon8|distForkPepper8|distForkPipe8, int32(-3600), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Pepper8 transition on a Dragon8Fix block.
	f.Add(distForkDragon8Fix|distForkPepper8, int32(0), int32(-3600), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))

	// --- the supply read ----------------------------------------------------
	// It errors while the legacy schedule is live: must be fatal (COR-184).
	f.Add(distForkDragon8, int32(-3600), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, true, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// It errors while Dragon8Fix is live: must not even be attempted, so the
	// block is unaffected. This is the seed that separates a branch-confined
	// read from a hoisted one.
	f.Add(distForkDragon8|distForkDragon8Fix, int32(-yearSecs), int32(-3600), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, true, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// It errors on a block with no Dragon8 fork at all -- mainnet's shape, where
	// dragon8Time is absent: likewise never attempted.
	f.Add(uint8(0), int32(0), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, true, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// Zero supply under the legacy schedule: a zero mint, and a deposit that
	// still has to report it coherently.
	f.Add(distForkDragon8, int32(-3600), int32(0), int32(0), int32(0), int32(0), uint16(3),
		uint8(0), []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	// A 256-bit-maximum supply: the arithmetic ceiling of the legacy schedule.
	f.Add(distForkDragon8, int32(-3600), int32(0), int32(0), int32(0), int32(0), uint16(3),
		uint8(2), maxSupply, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))

	// --- gas fees -----------------------------------------------------------
	// No gas fees at all: distributeIncoming returns before the validator deposit.
	f.Add(uint8(0), int32(0), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, uint64(0), uint8(0), uint8(3), uint8(1), uint8(0))
	f.Add(distForkDragon8Fix, int32(0), int32(-3600), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, uint64(0), uint8(0), uint8(3), uint8(1), uint8(0))
	// Fees too small to split (gasFees/5 == 0): no system-reward transaction.
	f.Add(uint8(0), int32(0), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, uint64(4), uint8(0), uint8(3), uint8(1), uint8(0))
	// The system-reward pool already at its 100-CHZ cap: the split is skipped
	// even without Dragon8. The two neighbouring seeds straddle the cap by one
	// whole CHZ, so a production cap that moves in *either* direction flips the
	// split on one of them while the golden expectation stays put. Without them,
	// only a cap raised above 100 CHZ would be caught.
	f.Add(uint8(0), int32(0), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(100), uint8(3), uint8(1), uint8(0))
	f.Add(uint8(0), int32(0), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(99), uint8(3), uint8(1), uint8(0))
	f.Add(uint8(0), int32(0), int32(0), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(101), uint8(3), uint8(1), uint8(0))
	// A single-validator set, and a 21-validator set with a high index.
	f.Add(distForkDragon8Fix, int32(0), int32(-3600), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(1), uint8(0), uint8(3))
	f.Add(distForkDragon8Fix, int32(0), int32(-3600), int32(0), int32(0), int32(0), uint16(3),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(21), uint8(20), uint8(7))
	// Parent and header on the same second (a zero-length gap is legal input to
	// the boundary detectors) and a very long gap.
	f.Add(distForkPepper8, int32(0), int32(0), int32(0), int32(0), int32(0), uint16(0),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))
	f.Add(distForkPepper8|distForkPipe8, int32(0), int32(0), int32(-1), int32(-1), int32(0), uint16(65535),
		realSupply, []byte{}, false, someFees, uint8(0), uint8(3), uint8(1), uint8(0))

	h := newDistHarness(f)

	f.Fuzz(func(t *testing.T,
		forkMask uint8,
		d8Off, d8fixOff, p8Off, pi8Off, headerOff int32, parentGap uint16,
		supplyMode uint8, supplyBytes []byte, supplyFails bool,
		gasFees uint64, sysRewardCHZ uint8,
		valCount, valIdx, coinbaseSel uint8,
	) {
		// --- inputs -> a block -------------------------------------------
		cfg := distChainConfig(forkMask, distTime(d8Off), distTime(d8fixOff), distTime(p8Off), distTime(pi8Off))
		headerTime := distTime(headerOff)
		parentTime := uint64(0)
		if uint64(parentGap) < headerTime {
			parentTime = headerTime - uint64(parentGap)
		}

		coinbaseIdx := int(coinbaseSel) % distKeys
		coinbase := h.addrs[coinbaseIdx]
		// The validator credited by the deposit calls is not the coinbase: on a
		// real chain val comes from the snapshot/header, and pinning them apart
		// is what catches a resolution that swaps one for the other.
		count := int(valCount)%21 + 1
		val := fuzzAddrFromSeed(uint64(int(valIdx) % count))

		h.engine.chainConfig = cfg
		h.backend.config = cfg
		h.backend.supply = distSupply(supplyMode, supplyBytes)
		h.backend.fail = supplyFails
		h.backend.queries = nil
		h.engine.Authorize(coinbase, nil, distSignerFn(h.keys[coinbaseIdx], h.engine.signer))

		parent := &types.Header{
			Number:     big.NewInt(distBlockNumber - 1),
			Time:       parentTime,
			Difficulty: big.NewInt(2),
			GasLimit:   distGasLimit,
			Extra:      make([]byte, extraVanity),
		}
		header := &types.Header{
			Number:     big.NewInt(distBlockNumber),
			ParentHash: parent.Hash(),
			Time:       headerTime,
			Coinbase:   coinbase,
			Difficulty: big.NewInt(2),
			GasLimit:   distGasLimit,
			BaseFee:    big.NewInt(distBaseFee),
			Extra:      make([]byte, extraVanity),
		}
		h.backend.header = parent
		reader := &prepareTestChainReader{
			config:  cfg,
			genesis: parent,
			headers: map[common.Hash]*types.Header{parent.Hash(): parent},
		}
		cx := chainContext{ChainHeaderReader: reader, parlia: h.engine}

		// --- pre-state ----------------------------------------------------
		// Gas fees arrive in the SystemAddress, which is where BSC's fee
		// collection parks them; the system-reward pool may already hold a
		// balance (its 100-CHZ cap is one of the two conditions that decide
		// whether the split runs at all).
		sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		if err != nil {
			t.Fatalf("state.New: %v", err)
		}
		// The pool balance is fuzzed in whole CHZ: maxSystemBalance is 100 CHZ
		// (1e20 wei), which does not fit in a uint64, so a wei-denominated input
		// could never reach the cap that decides whether the split runs.
		sysRewardBalance := new(uint256.Int).Mul(uint256.NewInt(uint64(sysRewardCHZ)), uint256.NewInt(params.Ether))
		sdb.SetBalance(consensus.SystemAddress, uint256.NewInt(gasFees), tracing.BalanceChangeUnspecified)
		sdb.SetBalance(distSystemReward, sysRewardBalance, tracing.BalanceChangeUnspecified)
		// Every system-transaction destination gets gas-consuming bytecode, so
		// that the receipts-sum-to-usedGas check further down compares real
		// numbers instead of 0 against 0 (see distSystemDestCode).
		for addr, cost := range distSystemDestGas {
			sdb.SetCode(addr, distSystemDestCode(cost), tracing.CodeChangeUnspecified)
		}
		ledger := &mintLedger{StateDB: sdb}

		// Balances before the call, for the ledger/state cross-check below.
		// distMintRecipient is the golden address the expectation credits; the two
		// production constants are watched alongside it so that a recipient which
		// moved fails from both sides -- as a mint that never arrived at the golden
		// account, and as a credit to an account no branch names.
		watched := []common.Address{
			coinbase, consensus.SystemAddress, distSystemReward, distValidatorSet,
			systemcontract.TokenomicsContractAddress, distMintRecipient,
			pepper8.Pepper8RecipientAddress, pipe8.Pipe8RecipientAddress,
		}
		before := make(map[common.Address]*big.Int, len(watched))
		for _, addr := range watched {
			before[addr] = sdb.GetBalance(addr).ToBig()
		}

		// --- the expectation, written independently of the production code --
		want := distExpect(cfg, headerTime, parentTime, h.backend.supply, gasFees, sysRewardBalance.ToBig(), coinbase)

		// --- run -----------------------------------------------------------
		var (
			txs      []*types.Transaction
			receipts []*types.Receipt
			usedGas  uint64
		)
		err = h.engine.distributeIncoming(val, ledger, header, cx, &txs, &receipts, nil, &usedGas, systemTxMining, nil)

		// ================================================================
		// Property 4: the supply read is hard-fail, branch-confined and
		// parent-hash pinned (COR-184).
		// ================================================================
		//
		// Confinement first: the read exists only to feed the *legacy* Dragon8
		// schedule. getNewSupplyForBlockDragon8Fix takes (forkTime, currentTime)
		// and nothing else, so on a Dragon8Fix block -- or on a block with no
		// Dragon8 fork at all, which is mainnet's shape today -- the call must
		// not happen. Hoisting it above the branch is the regression that
		// condemned a valid mainnet block on one RPC node for three weeks,
		// because an error out of Finalize is a bad-block verdict and this read
		// touches node-local indexes.
		wantQueries := 0
		if want.dragon8Slow {
			wantQueries = 1
		}
		if len(h.backend.queries) != wantQueries {
			t.Fatalf("Tokenomics supply read ran %d time(s), want %d (dragon8Fix=%v dragon8Legacy=%v headerTime=%d): the read must stay confined to the legacy Dragon8 branch (COR-184)",
				len(h.backend.queries), wantQueries, want.dragon8Fix, want.dragon8Slow, headerTime)
		}
		for i, q := range h.backend.queries {
			// Hash-pinned, never by number: by-number resolution goes through
			// the canonical number->hash index and fails locally.
			if q.BlockHash == nil || *q.BlockHash != header.ParentHash {
				t.Fatalf("supply read %d resolved state by %v, want the parent hash %s (COR-184)", i, q, header.ParentHash)
			}
			if q.BlockNumber != nil {
				t.Fatalf("supply read %d carries a block number (%v): it must resolve by hash only (COR-184)", i, q.BlockNumber)
			}
			// RequireCanonical must stay false so re-execution off the canonical
			// chain (tracing, reorgs) still resolves.
			if q.RequireCanonical {
				t.Fatalf("supply read %d set RequireCanonical: re-execution off the canonical chain must still resolve (COR-184)", i)
			}
		}
		// Hard-fail: a failed read must abort the block, never mint a default.
		if supplyFails && want.dragon8Slow {
			if err == nil {
				t.Fatalf("a failing Tokenomics supply read must abort the block, got nil error (the value feeds the mint, so failing open would diverge consensus -- COR-184)")
			}
			// Nothing beyond the one-time mints, which run before the branch,
			// may have been assembled.
			for _, tx := range txs {
				if to := tx.To(); to != nil && *to == systemcontract.TokenomicsContractAddress {
					t.Fatalf("a Tokenomics deposit was assembled although the supply read failed")
				}
			}
			if got := ledger.net(systemcontract.TokenomicsContractAddress); got.Sign() != 0 {
				t.Fatalf("Tokenomics balance moved by %s although the supply read failed", got)
			}
			return
		}
		if err != nil {
			t.Fatalf("distributeIncoming: %v (forkMask=%d headerTime=%d parentTime=%d supply=%s gasFees=%d)",
				err, forkMask, headerTime, parentTime, h.backend.supply, gasFees)
		}

		// ================================================================
		// Property 1 + 3: conservation, and no wrap.
		// ================================================================
		//
		// created() is the signed sum of every balance movement the block made,
		// so transfers cancel and only newly created wei survive. It must equal
		// the sum of exactly the mints the branch prescribes.
		if got := ledger.created(); got.Cmp(want.mint) != 0 {
			t.Fatalf("block created %s wei, schedule prescribes %s (pepper8=%v pipe8=%v dragon8Fix=%v dragon8Legacy=%v)",
				got, want.mint, want.pepper8Fires, want.pipe8Fires, want.dragon8Fix, want.dragon8Slow)
		}
		// How each movement was recorded, not just how big it was: the mints must
		// be consensus-level balance changes and the system transactions must move
		// value without creating any.
		distCheckMintReasons(t, ledger, want)
		// Per-address: nothing may be credited to an account the schedule does
		// not name, and every named account must receive exactly its share.
		for _, addr := range watched {
			expected := want.perAddress[addr]
			if expected == nil {
				expected = new(big.Int)
			}
			if got := ledger.net(addr); got.Cmp(expected) != 0 {
				t.Fatalf("net balance change of %s (%s) is %s, want %s (pepper8=%v pipe8=%v dragon8Fix=%v dragon8Legacy=%v sysReward=%v gasFees=%d)",
					distAccountName(addr, coinbase), addr, got, expected, want.pepper8Fires, want.pipe8Fires, want.dragon8Fix, want.dragon8Slow, want.sysReward, gasFees)
			}
		}
		for _, addr := range ledger.touched() {
			if _, ok := want.perAddress[addr]; !ok && ledger.net(addr).Sign() != 0 {
				t.Fatalf("balance of %s (%s) changed by %s, an account no mint branch names",
					distAccountName(addr, coinbase), addr, ledger.net(addr))
			}
		}
		// The ledger is complete, nothing wrapped, and no account went negative.
		// State balances are uint256, so neither failure mode is directly
		// observable there: a credit past 2^256-1 wraps to a small number and a
		// debit below zero wraps to a huge one. The ledger keeps the same
		// movements in signed arbitrary precision, so both show up as a
		// disagreement between the two -- and a negative running total is
		// itself the "no negative balance" property.
		for _, addr := range watched {
			sum := new(big.Int).Add(before[addr], ledger.net(addr))
			if sum.Sign() < 0 {
				t.Fatalf("%s (%s) was debited below zero: held %s, net movement %s",
					distAccountName(addr, coinbase), addr, before[addr], ledger.net(addr))
			}
			if got := sdb.GetBalance(addr).ToBig(); sum.Cmp(got) != 0 {
				t.Fatalf("balance of %s (%s) is %s but before+ledger is %s: a 256-bit wrap or an unrecorded movement",
					distAccountName(addr, coinbase), addr, got, sum)
			}
		}

		// ================================================================
		// Property 2: exactly one mint branch, and the one-time mints fire
		// only on their transition block.
		// ================================================================
		//
		// Mutual exclusion of the two Dragon8 schedules, and "Tokenomics is
		// credited only under a Dragon8 schedule", are both already decided by the
		// per-address loop above: the expectation credits Tokenomics on exactly one
		// branch and the loop demands an exact match on every watched account,
		// including a zero. Restating them here would make the property count look
		// larger than the set of things that can actually fail.
		//
		// What is *not* subsumed is the split between the two one-time mints. They
		// forward to the SAME account (goldenMintRecipient, held in production as
		// two constants in two packages), so a block on which both forks transition
		// credits it twice and the balance alone cannot say which mint ran. The sum
		// is checked here against the golden amounts, naming the fork times in the
		// failure; the two mints are separated one level down, by the
		// per-transaction value and ordering checks in distCheckSystemTxs.
		wantRecipient := new(big.Int)
		if want.pepper8Fires {
			wantRecipient.Add(wantRecipient, cmath.MustParseBig256(goldenPepper8Mint))
		}
		if want.pipe8Fires {
			wantRecipient.Add(wantRecipient, cmath.MustParseBig256(goldenPipe8Mint))
		}
		if got := ledger.net(distMintRecipient); got.Cmp(wantRecipient) != 0 {
			t.Fatalf("one-time-mint recipient moved by %s, want %s (pepper8=%v pipe8=%v headerTime=%d parentTime=%d pepper8Fork=%v pipe8Fork=%v)",
				got, wantRecipient, want.pepper8Fires, want.pipe8Fires, headerTime, parentTime, cfg.Pepper8Time, cfg.Pipe8Time)
		}
		// Pepper8 also installs the deterministic deployment proxy, exactly once,
		// only on the transition block -- and byte for byte. 0x4e59...956C is a
		// fixed protocol address that external tooling calls blind, and every
		// CREATE2 address ever computed against it assumes these exact bytes, so
		// "the account has some code" would let any valid-but-wrong bytecode
		// through. Both the address and the code are compared against the golden
		// literals, never against the production constants.
		gotProxyCode := sdb.GetCode(distProxyAddress)
		var wantProxyCode []byte
		if want.pepper8Fires {
			wantProxyCode = common.FromHex(goldenProxyRuntimeCode)
		}
		if !bytes.Equal(gotProxyCode, wantProxyCode) {
			t.Fatalf("deterministic deployment proxy at %s holds %d bytes of code (%x), want %d (%x) (pepper8Fires=%v)",
				distProxyAddress, len(gotProxyCode), gotProxyCode, len(wantProxyCode), wantProxyCode, want.pepper8Fires)
		}

		// ================================================================
		// Property 5: under Dragon8 the BSC system-reward split is skipped.
		// ================================================================
		// want.sysReward is false whenever Dragon8 is active (distExpect gates the
		// split on !dragon8()), so this one check covers both halves of property 5:
		// the split ran exactly when the golden divisor and the golden 100-CHZ cap
		// say it should, and never under Dragon8. The separate "received X under
		// Dragon8" restatement it replaces could not fail on its own.
		sysRewardPaid := ledger.net(distSystemReward).Sign() != 0
		if sysRewardPaid != want.sysReward {
			t.Fatalf("system-reward split ran=%v, want %v (dragon8=%v poolBalance=%d gasFees=%d)",
				sysRewardPaid, want.sysReward, want.dragon8(), sysRewardBalance, gasFees)
		}
		// Kept although the per-address loop above already pins the coinbase to a
		// net zero: "the coinbase kept N wei" names the single most likely
		// regression in this function directly, where the per-address failure would
		// only say that one of eight watched accounts is off by N.
		if got := ledger.net(coinbase); got.Sign() != 0 {
			t.Fatalf("coinbase kept %s wei: every mint and every fee must be forwarded within the block", got)
		}

		// ================================================================
		// Properties 6 + 7: the emitted system transactions.
		// ================================================================
		distCheckSystemTxs(t, h.engine, header, val, txs, receipts, usedGas, want)
	})
}

// distExpect restates, from the fork schedule alone, what the block must do.
//
// The one-time mints are restated from the raw timestamps rather than by calling
// IsPepper8Block/IsPipe8Block (COR-216 pins those); the Dragon8 amounts reuse
// COR-211's golden table and its already-pinned legacy arithmetic, because this
// target is about the accounting on top of them, not about re-deriving them.
func distExpect(cfg *params.ChainConfig, headerTime, parentTime uint64, supply *big.Int, gasFees uint64, sysRewardBalance *big.Int, coinbase common.Address) *distExpectation {
	e := &distExpectation{
		mint:       new(big.Int),
		perAddress: make(map[common.Address]*big.Int),
	}
	credit := func(addr common.Address, amount *big.Int) {
		if cur, ok := e.perAddress[addr]; ok {
			cur.Add(cur, amount)
			return
		}
		e.perAddress[addr] = new(big.Int).Set(amount)
	}

	// A one-time mint fires on the first block whose timestamp reaches the fork
	// while its parent's did not.
	e.pepper8Fires = cfg.Pepper8Time != nil && parentTime < *cfg.Pepper8Time && headerTime >= *cfg.Pepper8Time
	e.pipe8Fires = cfg.Pipe8Time != nil && parentTime < *cfg.Pipe8Time && headerTime >= *cfg.Pipe8Time
	// Dragon8Fix supersedes Dragon8 from its own timestamp on; before it (or
	// without it) the legacy schedule runs and reads the supply.
	e.dragon8Fix = cfg.Dragon8FixTime != nil && headerTime >= *cfg.Dragon8FixTime
	e.dragon8Slow = !e.dragon8Fix && cfg.Dragon8Time != nil && headerTime >= *cfg.Dragon8Time

	// The one-time mint amounts AND the account they land in come from the golden
	// literals, NOT from pepper8MintAmount/pipe8MintAmount/Pepper8RecipientAddress:
	// reading the production constants would move this expectation in lockstep
	// with any change to them, so the target would claim to pin how much is minted
	// and where it goes while pinning neither.
	if e.pepper8Fires {
		amount := cmath.MustParseBig256(goldenPepper8Mint)
		e.mint.Add(e.mint, amount)
		credit(distMintRecipient, amount)
	}
	if e.pipe8Fires {
		amount := cmath.MustParseBig256(goldenPipe8Mint)
		e.mint.Add(e.mint, amount)
		credit(distMintRecipient, amount)
	}
	switch {
	case e.dragon8Fix:
		e.fixYear = (headerTime - *cfg.Dragon8FixTime) / inflationYearSecs
		if e.fixYear > 13 {
			e.fixYear = 13
		}
		_, _, amount := dragon8FixRow(e.fixYear)
		e.mint.Add(e.mint, amount)
		credit(systemcontract.TokenomicsContractAddress, amount)
	case e.dragon8Slow:
		amount, pct := getNewSupplyForBlock(*cfg.Dragon8Time, headerTime, supply)
		e.legacyPct, e.legacySupply = pct, supply
		e.mint.Add(e.mint, amount)
		credit(systemcontract.TokenomicsContractAddress, amount)
	}

	// Gas fees: moved, never created. Below the SystemAddress threshold nothing
	// happens at all -- not even a validator deposit.
	if gasFees > 0 {
		fees := new(big.Int).SetUint64(gasFees)
		credit(consensus.SystemAddress, new(big.Int).Neg(fees))
		rewards := new(big.Int)
		// Both policy numbers are golden literals, not systemRewardPercent and
		// maxSystemBalance. Aliasing them is what would let an upstream merge
		// restore BSC's divisor of 4 (CLAUDE.md §2 names Chiliz's 5 as an
		// invariant) or move the 100-CHZ cap while every fuzz case kept passing:
		// distributeIncoming and this expectation would simply change together.
		if !e.dragon8() && sysRewardBalance.Cmp(cmath.MustParseBig256(goldenMaxSystemBalance)) < 0 {
			rewards.Div(fees, big.NewInt(goldenSystemRewardDivisor))
		}
		if rewards.Sign() > 0 {
			e.sysReward = true
			credit(distSystemReward, rewards)
		}
		credit(distValidatorSet, new(big.Int).Sub(fees, rewards))
	}
	// The coinbase is only ever a way station: naming it with a zero expectation
	// makes "the coinbase kept something" a per-address failure rather than an
	// unnamed-account one.
	credit(coinbase, new(big.Int))
	return e
}

// distCheckSystemTxs pins properties 6 and 7 over the transactions the block
// emitted: their number, order, destinations and values, the Tokenomics deposit
// calldata, and the fact that every one of them is a transaction
// IsSystemTransaction accepts.
func distCheckSystemTxs(t *testing.T, p *Parlia, header *types.Header, val common.Address, txs []*types.Transaction, receipts []*types.Receipt, usedGas uint64, want *distExpectation) {
	t.Helper()

	// The expected transaction sequence, in the order distributeIncoming
	// assembles it. Order is consensus-visible: importing nodes replay the block
	// body positionally (applyTransaction compares each expected transaction
	// against the next received one), so a reordering is a chain split.
	type expectedTx struct {
		name  string
		to    common.Address
		value *big.Int
	}
	var expected []expectedTx
	// Golden literals again: the per-transaction value is the only place the two
	// one-time mints can be told apart (they share a recipient), so it is the last
	// check that would notice a changed mint amount -- it must not read the
	// production constant either.
	if want.pepper8Fires {
		expected = append(expected, expectedTx{"pepper8", distMintRecipient, cmath.MustParseBig256(goldenPepper8Mint)})
	}
	if want.pipe8Fires {
		expected = append(expected, expectedTx{"pipe8", distMintRecipient, cmath.MustParseBig256(goldenPipe8Mint)})
	}
	if want.dragon8() {
		expected = append(expected, expectedTx{"tokenomics", systemcontract.TokenomicsContractAddress, want.perAddress[systemcontract.TokenomicsContractAddress]})
	}
	if want.sysReward {
		expected = append(expected, expectedTx{"systemReward", distSystemReward, want.perAddress[distSystemReward]})
	}
	if v, ok := want.perAddress[distValidatorSet]; ok {
		expected = append(expected, expectedTx{"validator", distValidatorSet, v})
	}

	if len(txs) != len(expected) {
		var got []string
		for _, tx := range txs {
			// %v on the pointer, as the destination check further down does.
			// A contract-creation system transaction (To == nil) is precisely
			// the unexpected shape this branch exists to describe, so a
			// dereference here would panic in place of the diagnostic. Today
			// applyTransaction dereferences msg.To itself when it builds the
			// transaction, so nothing in txs can carry a nil To and this cannot
			// fire -- which is the whole reason it is easy to leave as a
			// dereference and why it is worth not leaving one in the failure
			// path of the check that would report the regression.
			got = append(got, fmt.Sprintf("%v", tx.To()))
		}
		var names []string
		for _, e := range expected {
			names = append(names, e.name)
		}
		t.Fatalf("block emitted %d system transactions (to %v), want %d (%v)", len(txs), got, len(expected), names)
	}
	if len(receipts) != len(txs) {
		t.Fatalf("%d receipts for %d system transactions", len(receipts), len(txs))
	}

	var gasSum, wantGasSum uint64
	for i, tx := range txs {
		exp := expected[i]
		if tx.To() == nil || *tx.To() != exp.to {
			t.Fatalf("system tx %d (%s) goes to %v, want %s", i, exp.name, tx.To(), exp.to)
		}
		if tx.Value().Cmp(exp.value) != 0 {
			t.Fatalf("system tx %d (%s) carries %s wei, but %s was credited: the transaction and the mint must agree",
				i, exp.name, tx.Value(), exp.value)
		}
		// Property 7: the shape every mirror of this logic keys on. A system
		// transaction is zero-priced, from the coinbase, to a registered system
		// destination -- exactly the three conditions COR-214 pinned, and what
		// eth/tracers/api.go and eth/state_accessor.go re-derive when they
		// re-execute the block outside consensus.
		isSystem, err := p.IsSystemTransaction(tx, header)
		if err != nil {
			t.Fatalf("system tx %d (%s): IsSystemTransaction failed: %v", i, exp.name, err)
		}
		if !isSystem {
			t.Fatalf("system tx %d (%s) to %s is not accepted by IsSystemTransaction: the tracers and eth/state_accessor would not mirror it",
				i, exp.name, exp.to)
		}
		if tx.GasPrice().Sign() != 0 {
			t.Fatalf("system tx %d (%s) is priced at %s: system transactions must be zero-gas-price", i, exp.name, tx.GasPrice())
		}
		if receipts[i].TxHash != tx.Hash() {
			t.Fatalf("receipt %d belongs to %s, transaction is %s", i, receipts[i].TxHash, tx.Hash())
		}
		gasSum += receipts[i].GasUsed
		// The gas each system transaction costs is exactly the bytecode installed
		// at its destination: n JUMPDESTs at 1 gas each, and a system transaction
		// is charged no intrinsic gas (measured: the four destinations cost
		// 11/7/5/3 and every receipt reports precisely that, whatever the
		// calldata length). Checking it per receipt is what makes
		// distSystemDestGas's distinct costs mean something -- without this the
		// only gas assertion is the sum against usedGas, which is blind to which
		// destinations ran.
		wantGas, known := distSystemDestGas[exp.to]
		if !known {
			t.Fatalf("system tx %d (%s) goes to %s, which distSystemDestGas does not cover: "+
				"the harness installed no bytecode there, so its gas cost is 0 and the gas assertions below cannot see it",
				i, exp.name, exp.to)
		}
		if receipts[i].GasUsed != uint64(wantGas) {
			t.Fatalf("system tx %d (%s) to %s used %d gas, want %d (the JUMPDEST bytecode distSystemDestGas installs there): "+
				"either the transaction went somewhere else or applyTransaction is charging gas the harness cannot account for",
				i, exp.name, exp.to, receipts[i].GasUsed, wantGas)
		}
		wantGasSum += uint64(wantGas)
	}
	// The summed form of the same contract, stated because it is the one
	// distSystemDestGas is written in: the block's gas is the sum of the costs of
	// the destinations that were actually visited.
	if gasSum != wantGasSum {
		t.Fatalf("system transactions burned %d gas, want %d (the sum of distSystemDestGas over %d expected destinations)",
			gasSum, wantGasSum, len(expected))
	}
	// Each destination carries gas-consuming bytecode (distSystemDestGas), so this
	// is a comparison of real numbers: without it every destination is codeless,
	// every gasUsed is 0, and the check holds whatever applyTransaction does with
	// the counter.
	if gasSum != usedGas {
		t.Fatalf("usedGas is %d, receipts sum to %d", usedGas, gasSum)
	}

	// --- property 6a: the ValidatorSet deposit names the block's validator ---
	//
	// distributeIncoming ends with distributeToValidator(balance, val, ...), which
	// packs deposit(address) with `val`: the account credited with the whole
	// block's gas fees. Every check above looks at that transaction's destination
	// and value only, and on an ordinary Chiliz block `val` and header.Coinbase are
	// the same account -- so passing the coinbase instead would still send the same
	// wei to the same contract and leave the target green. The argument is the only
	// place the difference is recorded, and the bid-block path is exactly where the
	// two come apart, which is when a swapped argument misroutes every block's
	// fees. The Tokenomics deposit's validator argument was already checked; this
	// closes the asymmetry.
	if _, ok := want.perAddress[distValidatorSet]; ok {
		distCheckValidatorDeposit(t, p, txs, val)
	}

	// --- property 6b: the Tokenomics deposit calldata describes the mint -----
	if !want.dragon8() {
		return
	}
	var deposit *types.Transaction
	for _, tx := range txs {
		if to := tx.To(); to != nil && *to == systemcontract.TokenomicsContractAddress {
			deposit = tx
		}
	}
	if deposit == nil {
		t.Fatal("Dragon8 is active but no Tokenomics deposit was emitted")
	}
	if !p.IsTokenomicsDeposit(deposit.To(), deposit.Data()) {
		t.Fatalf("the Tokenomics transaction is not recognised as a deposit (selector %x): eth/tracers/api.go credits the coinbase on exactly this predicate",
			deposit.Data()[:min(4, len(deposit.Data()))])
	}
	args, err := p.tokenomicsABI.Methods["deposit"].Inputs.Unpack(deposit.Data()[4:])
	if err != nil {
		t.Fatalf("deposit calldata does not decode: %v", err)
	}
	// Guarded the way distCheckValidatorDeposit guards its single argument.
	//
	// Belt and braces, and worth being precise about why: the two routes that
	// would make args short -- the ABI losing an argument, or the "deposit" key
	// disappearing so that Methods["deposit"] is a zero-value abi.Method with nil
	// Inputs -- cannot actually reach this line today, because
	// distributeToTokenomics packs through the *same* p.tokenomicsABI. Renaming
	// the method in abi.go was tried: distributeIncoming fails first with
	// "method 'deposit' not found" and the target reports that, no panic. The
	// guard is here for the shape that would not be caught first -- a future
	// caller packing by hardcoded selector, or a decode reached from data this
	// helper did not produce -- and so that the two deposit checks read the same.
	if len(args) != 3 {
		t.Fatalf("Tokenomics deposit calldata decoded to %d arguments, want 3 "+
			"(validator, newTotalSupply, inflationPct): the tokenomics ABI has drifted from what distributeToTokenomics packs",
			len(args))
	}
	gotVal, ok := args[0].(common.Address)
	if !ok {
		t.Fatalf("deposit arg 0 is %T, want an address", args[0])
	}
	newTotalSupply, ok := args[1].(*big.Int)
	if !ok {
		t.Fatalf("deposit arg 1 is %T, want *big.Int", args[1])
	}
	inflationPct, ok := args[2].(*big.Int)
	if !ok {
		t.Fatalf("deposit arg 2 is %T, want *big.Int", args[2])
	}
	// The deposit credits the block's validator, not the coinbase. On a
	// Chiliz block they coincide, but distributeIncoming takes `val`
	// separately and the bid-block path can drive it apart.
	if gotVal != val {
		t.Fatalf("deposit credits %s, want the block's validator %s", gotVal, val)
	}
	if want.dragon8Fix {
		distCheckFixDeposit(t, deposit, newTotalSupply, inflationPct, want)
		return
	}
	distCheckLegacyDeposit(t, deposit, newTotalSupply, inflationPct, want)
}

// distCheckMintReasons pins *how* each movement was recorded, not only how large
// it was.
//
// distributeIncoming makes two kinds of movement, and a tracer can tell them
// apart (CLAUDE.md §5 -- the mints have to be mirrored wherever state is
// re-executed outside consensus). The mints and the SystemAddress fee sweep are
// made by distributeIncoming itself under tracing.BalanceChangeUnspecified; the
// value each system transaction then carries is moved by core.Transfer inside
// evm.Call under tracing.BalanceChangeTransfer. So every wei the block creates
// must appear under the former, and the latter must net to exactly zero: a
// transfer moves value, it can never create it.
func distCheckMintReasons(t *testing.T, ledger *mintLedger, want *distExpectation) {
	t.Helper()
	for i, e := range ledger.entries {
		switch e.reason {
		case tracing.BalanceChangeUnspecified, tracing.BalanceChangeTransfer:
		default:
			t.Fatalf("ledger entry %d (%s %s) is recorded under balance-change reason %v: distributeIncoming only mints and sweeps fees (Unspecified) or moves value through a system transaction (Transfer)",
				i, e.addr, e.delta, e.reason)
		}
	}
	if got := ledger.createdUnder(tracing.BalanceChangeTransfer); got.Sign() != 0 {
		t.Fatalf("the system transactions' value transfers net to %s rather than zero: a transfer moves balance, it must never create it", got)
	}
	if got := ledger.createdUnder(tracing.BalanceChangeUnspecified); got.Cmp(want.mint) != 0 {
		t.Fatalf("%s wei was created under BalanceChangeUnspecified, the schedule mints %s: every mint must be a consensus-level balance change, never recorded as a transfer",
			got, want.mint)
	}
}

// distCheckValidatorDeposit unpacks the ValidatorSet deposit(address) calldata
// and asserts it credits the block's validator rather than, say, the coinbase.
func distCheckValidatorDeposit(t *testing.T, p *Parlia, txs []*types.Transaction, val common.Address) {
	t.Helper()
	var deposit *types.Transaction
	for _, tx := range txs {
		if to := tx.To(); to != nil && *to == distValidatorSet {
			deposit = tx
		}
	}
	if deposit == nil {
		t.Fatal("gas fees were distributed but no ValidatorSet deposit was emitted")
	}
	method := p.validatorSetABI.Methods["deposit"]
	data := deposit.Data()
	if len(data) < 4 || !bytes.Equal(data[:4], method.ID) {
		t.Fatalf("the ValidatorSet transaction calls selector %x, deposit(address) is %x", data[:min(4, len(data))], method.ID)
	}
	args, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		t.Fatalf("ValidatorSet deposit calldata does not decode: %v", err)
	}
	if len(args) != 1 {
		t.Fatalf("ValidatorSet deposit decoded to %d arguments, want 1 (the validator)", len(args))
	}
	gotVal, ok := args[0].(common.Address)
	if !ok {
		t.Fatalf("ValidatorSet deposit arg 0 is %T, want an address", args[0])
	}
	if gotVal != val {
		t.Fatalf("ValidatorSet deposit credits %s, want the block's validator %s: this argument routes the whole block's gas fees, and val and header.Coinbase only coincide off the bid-block path",
			gotVal, val)
	}
}

// distCheckFixDeposit pins the Dragon8Fix deposit against the golden schedule
// row, and the row against itself.
func distCheckFixDeposit(t *testing.T, deposit *types.Transaction, newTotalSupply, inflationPct *big.Int, want *distExpectation) {
	t.Helper()
	year := want.fixYear
	wantPct, wantSupply, wantAmount := dragon8FixRow(year)
	if newTotalSupply.Cmp(wantSupply) != 0 {
		t.Fatalf("Dragon8Fix year %d: deposit reports total supply %s, schedule row says %s", year, newTotalSupply, wantSupply)
	}
	if inflationPct.Cmp(wantPct) != 0 {
		t.Fatalf("Dragon8Fix year %d: deposit reports inflation %s, schedule row says %s", year, inflationPct, wantPct)
	}
	if deposit.Value().Cmp(wantAmount) != 0 {
		t.Fatalf("Dragon8Fix year %d: deposit carries %s wei, schedule row mints %s", year, deposit.Value(), wantAmount)
	}
	// The relation itself: the per-block amount actually minted must re-derive
	// the percentage reported in the same calldata (amount*blocksPerYear ==
	// supply*pct/1e18). Three rows of the shipped table violate it; COR-219
	// records them and checkDragon8FixRowConsistency (COR-211) pins each
	// discrepancy to the wei, so a change to those rows is a visible diff
	// rather than a silently tolerated drift.
	checkDragon8FixRowConsistency(t, year)
}

// distCheckLegacyDeposit pins the legacy Dragon8 deposit: the percentage is the
// one the schedule computed for this block, and the reported new total supply is
// the old supply plus exactly what was minted.
func distCheckLegacyDeposit(t *testing.T, deposit *types.Transaction, newTotalSupply, inflationPct *big.Int, want *distExpectation) {
	t.Helper()
	if inflationPct.Cmp(want.legacyPct) != 0 {
		t.Fatalf("legacy Dragon8: deposit reports inflation %s, the schedule used %s", inflationPct, want.legacyPct)
	}
	wantSupply := new(big.Int).Add(want.legacySupply, deposit.Value())
	if wantSupply.BitLen() > 256 {
		// PINNED, NOT ENDORSED (COR-245, found by this target): lastSupply is
		// whatever the Tokenomics contract's getTotalSupply() returns, and
		// lastSupply + blockAmount can exceed 2^256 while both fit in 256 bits
		// individually. abi.Pack does not reject the overflow -- packNum reduces
		// the value modulo 2^256 and packs the low word -- so the deposit reports
		// a supply that is smaller than the one before the mint, while the mint
		// itself is credited in full. The wrap is unreachable on a live chain
		// (CHZ supply is ~2^93 wei) and is pinned rather than tolerated: the
		// reduction is asserted exactly, so any change -- a rejection, a clamp,
		// or a different wrap -- is a visible diff here.
		wantSupply.And(wantSupply, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)))
	}
	if newTotalSupply.Cmp(wantSupply) != 0 {
		t.Fatalf("legacy Dragon8: deposit reports total supply %s, want lastSupply+minted = %s", newTotalSupply, wantSupply)
	}
	// The mint must be the floor of the annual inflation over a year of blocks:
	// amount*10512000 lands in (annual - 10512000, annual], where annual is
	// lastSupply*pct/1e20 (pct is percent*1e18 on this branch, a scale the fixed
	// schedule does not share).
	annual := new(big.Int).Mul(want.legacySupply, inflationPct)
	annual.Div(annual, new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
	minted := new(big.Int).Mul(deposit.Value(), big.NewInt(inflationBlocksPerYear))
	if minted.Cmp(annual) > 0 {
		t.Fatalf("legacy Dragon8: minted %s over a year, more than the reported inflation allows (%s)", minted, annual)
	}
	if new(big.Int).Sub(annual, minted).Cmp(big.NewInt(inflationBlocksPerYear)) >= 0 {
		t.Fatalf("legacy Dragon8: minted %s over a year, short of the reported inflation %s by more than one block's rounding", minted, annual)
	}
}

// TestDistributeIncomingRequiresParent covers the one guard in distributeIncoming
// the fuzz target cannot reach (its chain reader always has the parent): both
// one-time mints are gated on the *parent's* timestamp, so a missing parent must
// abort the block rather than be treated as a zero timestamp -- which would make
// every block look like the transition block and mint forever.
func TestDistributeIncomingRequiresParent(t *testing.T) {
	h := newDistHarness(t)
	pepper8Time := distBase
	cfg := distChainConfig(distForkPepper8, 0, 0, pepper8Time, 0)
	h.engine.chainConfig = cfg
	h.backend.config = cfg
	h.engine.Authorize(h.addrs[0], nil, distSignerFn(h.keys[0], h.engine.signer))

	header := &types.Header{
		Number:     big.NewInt(distBlockNumber),
		ParentHash: common.Hash{0xde, 0xad},
		Time:       distBase + 3,
		Coinbase:   h.addrs[0],
		Difficulty: big.NewInt(2),
		GasLimit:   distGasLimit,
		BaseFee:    big.NewInt(distBaseFee),
		Extra:      make([]byte, extraVanity),
	}
	reader := &prepareTestChainReader{config: cfg, headers: map[common.Hash]*types.Header{}}
	cx := chainContext{ChainHeaderReader: reader, parlia: h.engine}

	sdb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	ledger := &mintLedger{StateDB: sdb}
	var (
		txs      []*types.Transaction
		receipts []*types.Receipt
		usedGas  uint64
	)
	if err := h.engine.distributeIncoming(h.addrs[0], ledger, header, cx, &txs, &receipts, nil, &usedGas, systemTxMining, nil); err == nil {
		t.Fatal("distributeIncoming accepted a block whose parent is unknown")
	}
	if len(ledger.entries) != 0 {
		t.Fatalf("balances moved (%d entries) before the missing parent was noticed", len(ledger.entries))
	}
}

// TestMintRecipientsAreOneAddress pins both the aliasing FuzzDistributeIncoming
// works around and the address itself.
//
// The Pepper8 and Pipe8 one-time mints are forwarded to the same account,
// declared as two constants in two packages. Every balance-level check therefore
// sees one recipient, and the two mints are only separable by their
// transactions. Should the addresses ever diverge (a second recipient for a
// future one-time mint), the recipient check in the fuzz target must be split
// back into two -- this test is what says so.
//
// Comparing the two production constants to *each other* is not enough, and used
// to be all this test did: editing both packages kept them equal, and because
// the fuzz expectation read one of them, ~600M CHZ could be redirected to a new
// account with the whole target still green. Both are therefore compared against
// goldenMintRecipient, the address CLAUDE.md §2 names.
func TestMintRecipientsAreOneAddress(t *testing.T) {
	// Checked first although the golden comparison below subsumes it: when the two
	// have drifted apart from each other, "split the recipient assertions" is the
	// instruction the reader needs, and it would be hidden behind the other failure.
	if pepper8.Pepper8RecipientAddress != pipe8.Pipe8RecipientAddress {
		t.Fatalf("the one-time-mint recipients have diverged (pepper8 %s, pipe8 %s): split the recipient assertions in FuzzDistributeIncoming",
			pepper8.Pepper8RecipientAddress, pipe8.Pipe8RecipientAddress)
	}
	for _, c := range []struct {
		name string
		addr common.Address
	}{
		{"pepper8.Pepper8RecipientAddress", pepper8.Pepper8RecipientAddress},
		{"pipe8.Pipe8RecipientAddress", pipe8.Pipe8RecipientAddress},
		{"pepper8.Pepper8Recipient", common.HexToAddress(pepper8.Pepper8Recipient)},
		{"pipe8.Pipe8Recipient", common.HexToAddress(pipe8.Pipe8Recipient)},
	} {
		if c.addr != distMintRecipient {
			t.Fatalf("%s is %s, the one-time-mint recipient is %s: a mint recipient is a business decision, not a refactor",
				c.name, c.addr, goldenMintRecipient)
		}
	}
	// The literal is checksummed, so round-tripping it is also a spot check that
	// it is not a truncated or mistyped address.
	if got := distMintRecipient.Hex(); got != goldenMintRecipient {
		t.Fatalf("golden mint recipient %s does not round-trip (got %s)", goldenMintRecipient, got)
	}
}

// TestDeterministicDeploymentProxyGolden anchors the golden proxy literals
// outside this repository, so that "golden" means something more than "a second
// copy of whatever production says today".
//
// FuzzDistributeIncoming compares the code Pepper8 installs against
// goldenProxyRuntimeCode rather than against the production constant, which
// makes the golden itself the thing a reader has to trust. It is checkable:
// these are the deterministic deployment proxy's published bytes, and their
// keccak256 is the codehash the account has on every chain the proxy exists on.
// If this test fails, the golden literal was mistyped -- not the production
// constant, which this test does not look at.
func TestDeterministicDeploymentProxyGolden(t *testing.T) {
	code := common.FromHex(goldenProxyRuntimeCode)
	if len(code) != 69 {
		t.Fatalf("golden proxy runtime code is %d bytes, the deterministic deployment proxy is 69", len(code))
	}
	if got := crypto.Keccak256Hash(code); got != common.HexToHash(goldenProxyCodeHash) {
		t.Fatalf("golden proxy runtime code hashes to %s, the published deterministic deployment proxy codehash is %s",
			got, goldenProxyCodeHash)
	}
	// The address is checksummed, so HexToAddress round-tripping it is also a
	// spot check that the literal is not a truncated or mistyped address.
	if got := distProxyAddress.Hex(); got != goldenProxyAddress {
		t.Fatalf("golden proxy address %s does not round-trip (got %s)", goldenProxyAddress, got)
	}
}

// TestSystemRewardContractAddressesAgree pins a seam distributeIncoming straddles:
// it reads the pool's balance through core/systemcontracts and pays it through
// common/systemcontract. The two constants are independent string literals in
// two packages; were they ever to drift, the cap check would look at one account
// and the reward land in another.
func TestSystemRewardContractAddressesAgree(t *testing.T) {
	if systemcontract.SystemRewardContract != systemcontracts.SystemRewardContract {
		t.Fatalf("system reward address mismatch: common/systemcontract %s vs core/systemcontracts %s",
			systemcontract.SystemRewardContract, systemcontracts.SystemRewardContract)
	}
	if systemcontract.ValidatorContract != systemcontracts.ValidatorContract {
		t.Fatalf("validator set address mismatch: common/systemcontract %s vs core/systemcontracts %s",
			systemcontract.ValidatorContract, systemcontracts.ValidatorContract)
	}
}
