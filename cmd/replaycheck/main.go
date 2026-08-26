// Copyright 2026 The go-ethereum Authors
// SPDX-License-Identifier: LGPL-3.0
//
// replaycheck answers one question: does *this* build reproduce the gas the chain charged?
//
// Give it a block number. It pulls the block, the receipts, each transaction in signed form
// and the pre-state each transaction saw from an RPC endpoint, then re-executes every
// transaction locally, with the consensus rules compiled into this binary, and compares the
// gas against the canonical receipts. It also recomputes the parlia fee pool from the gas it
// measured and checks it against the value the block's system transactions actually paid
// out, which is what an importing node compares — so a mismatch there is the same
// "########## BAD BLOCK #########" a resync would hit.
//
// The endpoint is only a source of history: it is never asked to judge anything. That is the
// difference from debug_traceTransaction, which re-executes on the remote node under the
// remote node's rules. Use --remote-trace to see that number too, which is how you tell
// "our build is fixed" from "the endpoint is fixed".
//
//	replaycheck --block 31384697                     # one block against mainnet
//	replaycheck --block 31000000-31000010            # a range
//	replaycheck --upgrades                           # every runtime upgrade on the network
//	replaycheck --governance                         # every executed governance proposal
//	replaycheck --rpc https://spicy-rpc.chiliz.com --upgrades
//	replaycheck --block 31384697 --save-fixture internal/replay/testdata
//
// The endpoint needs the debug namespace and enough history to trace the blocks asked for.
// No chain state and no node of our own is required.
//
// It exits non-zero on any delta, so it works in CI and as a bisect predicate.
//
// Parlia's own system transactions are replayed too, through the execution path parlia uses
// for them rather than through core.ApplyMessage. One caveat if one of those ever diverges:
// their pre-state is captured by the endpoint's tracer, which drains the fee pool to the
// coinbase before the block's first system transaction, whereas consensus drains it after
// the Tokenomics deposit. Rule out that discrepancy before believing such a delta.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/replay"
	"github.com/ethereum/go-ethereum/rpc"
)

// depositSelector is validatorSet.deposit(address), the parlia system transaction that
// drains the block's fee pool into the validator contract.
var depositSelector = []byte{0xf3, 0x40, 0xfa, 0x01}

var (
	validatorContract    = common.HexToAddress(systemcontract.ValidatorContract)
	systemRewardContract = common.HexToAddress(systemcontract.SystemRewardContract)
)

var (
	// runtimeUpgradeTopic is the event the RuntimeUpgrade contract (0x...7004) emits when
	// it replaces the bytecode of a system contract. Its first data word is the target.
	runtimeUpgradeTopic = common.HexToHash("0x294c52758d41df5421795a058ea4837ce9d9714c75091eb30fe6925d1231db4a")
	// governanceExecutedTopic is the Governance contract (0x...7002) proposal-execution
	// event, a wider net than the upgrade event alone.
	governanceExecutedTopic = common.HexToHash("0x712ae1383f79ac853f8d882153778e0260ef8f03b504e2866e0593e04d2b291f")
)

type blockList []uint64

func (b *blockList) String() string { return fmt.Sprint(*b) }

// Set accepts either a single block number or an inclusive "from-to" range.
func (b *blockList) Set(v string) error {
	from, to, isRange := strings.Cut(v, "-")
	lo, err := strconv.ParseUint(strings.TrimSpace(from), 10, 64)
	if err != nil {
		return fmt.Errorf("bad block %q: %w", v, err)
	}
	hi := lo
	if isRange {
		if hi, err = strconv.ParseUint(strings.TrimSpace(to), 10, 64); err != nil {
			return fmt.Errorf("bad block range %q: %w", v, err)
		}
	}
	if hi < lo {
		return fmt.Errorf("bad block range %q: end before start", v)
	}
	if hi-lo > 10_000 {
		return fmt.Errorf("bad block range %q: too wide, each block costs several RPC calls", v)
	}
	for n := lo; n <= hi; n++ {
		*b = append(*b, n)
	}
	return nil
}

// options is what the command line asks for.
type options struct {
	rpcURL      string
	blocks      blockList
	upgrades    bool
	governance  bool
	from, to    uint64
	chunk       uint64
	genesis     string
	saveFixture string
	remoteTrace bool
	verbose     bool
}

func main() {
	var opts options
	flag.StringVar(&opts.rpcURL, "rpc", "https://rpc.chiliz.com", "JSON-RPC endpoint to pull history from (needs the debug namespace)")
	flag.Var(&opts.blocks, "block", "block to check; a number or an inclusive N-M range (repeatable)")
	flag.BoolVar(&opts.upgrades, "upgrades", false, "check every system-contract runtime upgrade on the network")
	flag.BoolVar(&opts.governance, "governance", false, "check every executed governance proposal on the network")
	flag.Uint64Var(&opts.from, "from", 0, "first block of the sweep range")
	flag.Uint64Var(&opts.to, "to", 0, "last block of the sweep range (0 = chain head)")
	flag.Uint64Var(&opts.chunk, "chunk", 4_000_000, "eth_getLogs chunk size for sweeps")
	flag.StringVar(&opts.genesis, "genesis", "", "embedded genesis to take the fork schedule from (default: from the endpoint's chain id)")
	flag.StringVar(&opts.saveFixture, "save-fixture", "", "directory to write a replay fixture per transaction, for committing as a regression test")
	flag.BoolVar(&opts.remoteTrace, "remote-trace", false, "also report the gas the endpoint itself charges (debug_traceTransaction)")
	flag.BoolVar(&opts.verbose, "v", false, "print every transaction, not just the diverging ones")
	flag.Parse()

	if err := run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if len(opts.blocks) == 0 && !opts.upgrades && !opts.governance {
		flag.Usage()
		return errors.New("nothing to check: pass --block, --upgrades or --governance")
	}
	ctx := context.Background()
	client, err := rpc.DialContext(ctx, opts.rpcURL)
	if err != nil {
		return fmt.Errorf("dial %s: %w", opts.rpcURL, err)
	}
	defer client.Close()

	var clientVersion string
	if err := call(ctx, client, &clientVersion, "web3_clientVersion"); err != nil {
		return fmt.Errorf("web3_clientVersion: %w", err)
	}
	var chainID hexutil.Big
	if err := call(ctx, client, &chainID, "eth_chainId"); err != nil {
		return fmt.Errorf("eth_chainId: %w", err)
	}
	genesis := opts.genesis
	if genesis == "" {
		if genesis, err = replay.GenesisForChainID(chainID.ToInt().Uint64()); err != nil {
			return fmt.Errorf("%w; pass --genesis to override", err)
		}
	}
	head, err := blockNumber(ctx, client)
	if err != nil {
		return err
	}
	fmt.Printf("history from %s (%s), chain id %s, head %d\nreplaying with the rules in this build, %s fork schedule\n\n",
		opts.rpcURL, clientVersion, chainID.ToInt(), head, genesis)

	targets := map[uint64]string{}
	for _, n := range opts.blocks {
		targets[n] = "requested"
	}
	if opts.upgrades {
		found, err := sweep(ctx, client, systemcontract.RuntimeUpgradeContractAddress, runtimeUpgradeTopic, opts, head, true)
		if err != nil {
			return fmt.Errorf("enumerate runtime upgrades: %w", err)
		}
		fmt.Printf("runtime upgrades found: %d\n", len(found))
		for n, label := range found {
			targets[n] = label
		}
	}
	if opts.governance {
		found, err := sweep(ctx, client, systemcontract.GovernanceContractAddress, governanceExecutedTopic, opts, head, false)
		if err != nil {
			return fmt.Errorf("enumerate governance executions: %w", err)
		}
		fmt.Printf("governance executions found: %d\n", len(found))
		for n, label := range found {
			if _, dup := targets[n]; !dup {
				targets[n] = label
			}
		}
	}

	ordered := make([]uint64, 0, len(targets))
	for n := range targets {
		ordered = append(ordered, n)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })

	fmt.Printf("\nchecking %d block(s)\n\n", len(ordered))
	var diverged, failed []uint64
	for _, n := range ordered {
		ok, err := checkBlock(ctx, client, n, targets[n], genesis, opts)
		switch {
		case err != nil:
			// An endpoint that will not answer for one block says nothing about that
			// block; keep sweeping and report it as unchecked at the end.
			fmt.Printf("block %d (%s): could not check: %v\n", n, targets[n], err)
			failed = append(failed, n)
		case !ok:
			diverged = append(diverged, n)
		}
	}

	fmt.Println()
	if len(failed) > 0 {
		fmt.Printf("could not check %d of %d block(s): %v\n", len(failed), len(ordered), failed)
	}
	if len(diverged) == 0 && len(failed) == 0 {
		fmt.Printf("OK: this build replays all %d block(s) exactly\n", len(ordered))
		return nil
	}
	if len(diverged) == 0 {
		return fmt.Errorf("%d of %d block(s) unchecked", len(failed), len(ordered))
	}
	return fmt.Errorf("this build diverges on %d of %d block(s): %v", len(diverged), len(ordered), diverged)
}

// checkBlock re-executes every transaction of a block with this build's rules and reports
// whether the canonical gas and the canonical fee-pool payout both survive.
func checkBlock(ctx context.Context, client *rpc.Client, number uint64, label, genesis string, opts options) (bool, error) {
	block, err := blockByNumber(ctx, client, number)
	if err != nil {
		return false, err
	}
	receipts, err := blockReceipts(ctx, client, number, block.Transactions)
	if err != nil {
		return false, err
	}
	if len(receipts) != len(block.Transactions) {
		return false, fmt.Errorf("got %d receipts for %d transactions", len(receipts), len(block.Transactions))
	}
	baseFee := block.BaseFeePerGas.ToInt()
	getHash := blockHashResolver(ctx, client)

	var (
		canonicalPool = new(big.Int)
		replayedPool  = new(big.Int)
		payout        = new(big.Int)
		sawDeposit    bool
		deltas        []string
	)
	for i, tx := range block.Transactions {
		receipt := receipts[i]
		canonical := uint64(receipt.GasUsed)
		system := isSystemTransaction(&tx, block.Miner)

		fixture, err := buildFixture(ctx, client, block, &tx, i, &receipt, genesis, system)
		if err != nil {
			return false, err
		}
		if opts.saveFixture != "" {
			if err := saveFixture(opts.saveFixture, genesis, number, i, fixture); err != nil {
				return false, err
			}
		}
		result, err := fixture.Replay(getHash)
		if err != nil {
			return false, fmt.Errorf("replay tx %d %s: %w", i, tx.Hash.Hex(), err)
		}
		replayed := result.GasUsed

		tip := effectiveTip(&tx, &receipt, baseFee)
		canonicalPool.Add(canonicalPool, feeFor(canonical, tip, &receipt))
		replayedPool.Add(replayedPool, feeFor(replayed, tip, &receipt))
		if drains, deposit := drainsFeePool(&tx, tip, block.Miner); drains {
			payout.Add(payout, tx.Value.ToInt())
			sawDeposit = sawDeposit || deposit
		}

		var remote string
		if opts.remoteTrace {
			gas, err := tracedGasUsed(ctx, client, tx.Hash)
			if err != nil {
				return false, err
			}
			remote = fmt.Sprintf(", endpoint %d", gas)
		}
		switch {
		case !fixture.Matches(result):
			delta := fmt.Sprintf("  tx %d %s: %s%s", i, tx.Hash.Hex(), describe(fixture, result), remote)
			if system {
				delta += "\n    note: system transaction — check the fee-pool ordering caveat in this file's header before believing it"
			}
			deltas = append(deltas, delta)
		case opts.verbose:
			fmt.Printf("  tx %d %s to %s: gasUsed %d, tip %s (match)%s\n",
				i, tx.Hash.Hex(), addrString(tx.To), canonical, tip, remote)
		}
	}

	ok := len(deltas) == 0
	fmt.Printf("block %d (%s), %d tx, baseFee %s\n", number, label, len(block.Transactions), baseFee)
	for _, d := range deltas {
		fmt.Println(d)
	}
	// The system transactions that drain the fee pool carry exactly what the block paid
	// out. Recomputing that from canonical gas must reproduce it; recomputing it from the
	// gas this build charges is what an importing node compares against the canonical
	// system transaction, so a mismatch there is the bad-block rejection seen on a resync.
	switch {
	case !sawDeposit:
		fmt.Printf("  no deposit() system transaction in this block; fee pool from canonical gas %s\n", canonicalPool)
	default:
		if canonicalPool.Cmp(payout) != 0 {
			fmt.Printf("  fee pool from canonical gas %s != system-transaction payout %s (%+d)\n",
				canonicalPool, payout, new(big.Int).Sub(canonicalPool, payout))
			ok = false
		}
		if replayedPool.Cmp(payout) != 0 {
			fmt.Printf("  fee pool from this build's gas %s != system-transaction payout %s (%+d) -> this build cannot import this block\n",
				replayedPool, payout, new(big.Int).Sub(replayedPool, payout))
			ok = false
		} else if opts.verbose {
			fmt.Printf("  system-transaction payout %s reproduced\n", payout)
		}
	}
	if ok {
		fmt.Println("  match")
	}
	return ok, nil
}

// buildFixture assembles everything needed to re-execute one transaction offline: the
// transaction in canonical signed form and the pre-state it read, captured with the
// prestateTracer.
func buildFixture(ctx context.Context, client *rpc.Client, block *rpcBlock, tx *rpcTransaction, index int, receipt *rpcReceipt, genesis string, system bool) (*replay.Fixture, error) {
	var raw hexutil.Bytes
	if err := call(ctx, client, &raw, "eth_getRawTransactionByHash", tx.Hash); err != nil {
		return nil, fmt.Errorf("eth_getRawTransactionByHash %s: %w", tx.Hash.Hex(), err)
	}
	var pre types.GenesisAlloc
	cfg := map[string]interface{}{"tracer": "prestateTracer"}
	if err := call(ctx, client, &pre, "debug_traceTransaction", tx.Hash, cfg); err != nil {
		return nil, fmt.Errorf("prestateTracer %s: %w", tx.Hash.Hex(), err)
	}
	return &replay.Fixture{
		Genesis: genesis,
		Block: replay.BlockInfo{
			Number:        uint64(block.Number),
			Hash:          block.Hash,
			ParentHash:    block.ParentHash,
			Time:          uint64(block.Timestamp),
			Coinbase:      block.Miner,
			Difficulty:    block.Difficulty,
			GasLimit:      block.GasLimit,
			BaseFeePerGas: block.BaseFeePerGas,
			ExcessBlobGas: block.ExcessBlobGas,
			BlobGasUsed:   block.BlobGasUsed,
			MixHash:       block.MixHash,
		},
		Tx:              replay.TxInfo{Hash: tx.Hash, Index: index, Raw: raw, System: system},
		ExpectedGasUsed: uint64(receipt.GasUsed),
		ExpectedFailed:  receipt.Status == 0,
		Pre:             pre,
	}, nil
}

// describe says how a replay differs from the canonical receipt.
func describe(fixture *replay.Fixture, result *replay.Result) string {
	out := fmt.Sprintf("canonical %d, this build %d (%+d)",
		fixture.ExpectedGasUsed, result.GasUsed, fixture.Delta(result))
	switch {
	case result.VMErr != nil && !fixture.ExpectedFailed:
		out += fmt.Sprintf("; canonical succeeded but this build failed: %v", result.VMErr)
	case result.VMErr == nil && fixture.ExpectedFailed:
		out += "; canonical failed but this build succeeded"
	}
	return out
}

func saveFixture(dir, genesis string, number uint64, index int, fixture *replay.Fixture) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := replay.MarshalFixture(fixture)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s_%d_tx%d.json", genesis, number, index))
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("  wrote fixture %s\n", path)
	return nil
}

// blockHashResolver answers the BLOCKHASH opcode from the endpoint, so a transaction that
// reads an old block hash replays on the real value rather than on zero.
func blockHashResolver(ctx context.Context, client *rpc.Client) func(uint64) common.Hash {
	cache := map[uint64]common.Hash{}
	return func(n uint64) common.Hash {
		if hash, ok := cache[n]; ok {
			return hash
		}
		var header struct {
			Hash common.Hash `json:"hash"`
		}
		if err := call(ctx, client, &header, "eth_getBlockByNumber", hexutil.Uint64(n), false); err != nil {
			return common.Hash{}
		}
		cache[n] = header.Hash
		return header.Hash
	}
}

// sweep enumerates the blocks carrying a given event, in chunks so a public endpoint's
// log-range limit is respected. When withTarget is set the event's first data word is
// reported as the upgraded contract, which is what distinguishes a DeployerProxy upgrade
// from any other one.
func sweep(ctx context.Context, client *rpc.Client, address common.Address, topic common.Hash, opts options, head uint64, withTarget bool) (map[uint64]string, error) {
	to, chunk := opts.to, opts.chunk
	if to == 0 || to > head {
		to = head
	}
	if chunk == 0 {
		chunk = 4_000_000
	}
	out := map[uint64]string{}
	for start := opts.from; start <= to; start += chunk {
		end := start + chunk - 1
		if end > to {
			end = to
		}
		var logs []struct {
			BlockNumber hexutil.Uint64 `json:"blockNumber"`
			Data        hexutil.Bytes  `json:"data"`
		}
		arg := map[string]interface{}{
			"address":   address,
			"topics":    []interface{}{topic},
			"fromBlock": hexutil.Uint64(start),
			"toBlock":   hexutil.Uint64(end),
		}
		if err := call(ctx, client, &logs, "eth_getLogs", arg); err != nil {
			return nil, fmt.Errorf("eth_getLogs [%d,%d]: %w", start, end, err)
		}
		for _, l := range logs {
			label := "event"
			if withTarget && len(l.Data) >= 32 {
				label = "upgrade of " + common.BytesToAddress(l.Data[12:32]).Hex()
			}
			out[uint64(l.BlockNumber)] = label
		}
	}
	return out, nil
}

// isSystemTransaction reports whether tx is one of the transactions parlia injects during
// Finalize: sent by the block's coinbase to a system contract at a zero gas price. Mirrors
// Parlia.IsSystemTransaction closely enough to classify a mined block.
func isSystemTransaction(tx *rpcTransaction, coinbase common.Address) bool {
	if tx.To == nil || tx.From != coinbase {
		return false
	}
	if price := tx.GasPrice.ToInt(); price != nil && price.Sign() != 0 {
		return false
	}
	return true
}

// drainsFeePool reports whether tx is one of the parlia system transactions that pay out
// the block's fee pool, and whether it is the deposit() to the validator contract. The
// pool leaves the block in up to two pieces: deposit() to 0x...1000 and, when Dragon8 is
// not yet active and the system reward pool is below its cap, a bare value transfer of
// balance/systemRewardPercent to 0x...1002. Minted amounts — the Tokenomics deposit and
// the Pepper8/Pipe8 one-time mints — are not part of the pool and are ignored here.
func drainsFeePool(tx *rpcTransaction, tip *big.Int, coinbase common.Address) (drains, deposit bool) {
	if !isSystemTransaction(tx, coinbase) {
		return false, false
	}
	switch {
	case *tx.To == validatorContract && hasSelector(tx.Input, depositSelector):
		return true, true
	case *tx.To == systemRewardContract && len(tx.Input) == 0:
		return true, false
	}
	return false, false
}

// effectiveTip is the per-gas amount credited to the fee pool: the transaction's effective
// gas price minus the burned base fee, clamped at zero so zero-gas-price system
// transactions and Pepper8/Pipe8 deposits contribute nothing. Mirrors the tip computation
// in core/state_transition.go.
func effectiveTip(tx *rpcTransaction, receipt *rpcReceipt, baseFee *big.Int) *big.Int {
	gasPrice := receipt.EffectiveGasPrice.ToInt()
	if gasPrice == nil || gasPrice.Sign() == 0 {
		// Pre-London receipts and endpoints that omit the field: fall back to the
		// transaction's own pricing.
		gasPrice = tx.GasPrice.ToInt()
		if tx.MaxFeePerGas != nil && baseFee != nil {
			gasPrice = new(big.Int).Add(baseFee, tx.MaxPriorityFeePerGas.ToInt())
			if capped := tx.MaxFeePerGas.ToInt(); gasPrice.Cmp(capped) > 0 {
				gasPrice = capped
			}
		}
	}
	if gasPrice == nil {
		return new(big.Int)
	}
	tip := gasPrice
	if baseFee != nil {
		tip = new(big.Int).Sub(gasPrice, baseFee)
	}
	if tip.Sign() < 0 {
		return new(big.Int)
	}
	return tip
}

// feeFor is the contribution of one transaction to the block's fee pool, including the
// Cancun blob fee, which parlia credits to the system address as well.
func feeFor(gasUsed uint64, tip *big.Int, receipt *rpcReceipt) *big.Int {
	fee := new(big.Int).Mul(new(big.Int).SetUint64(gasUsed), tip)
	if receipt.BlobGasUsed != nil && receipt.BlobGasPrice != nil {
		fee.Add(fee, new(big.Int).Mul(new(big.Int).SetUint64(uint64(*receipt.BlobGasUsed)), receipt.BlobGasPrice.ToInt()))
	}
	return fee
}

// tracedGasUsed asks the endpoint to re-execute a transaction under its own rules. Only
// used for --remote-trace: the verdict comes from the local replay.
func tracedGasUsed(ctx context.Context, client *rpc.Client, hash common.Hash) (uint64, error) {
	var frame struct {
		GasUsed hexutil.Uint64 `json:"gasUsed"`
	}
	cfg := map[string]interface{}{"tracer": "callTracer"}
	if err := call(ctx, client, &frame, "debug_traceTransaction", hash, cfg); err != nil {
		return 0, fmt.Errorf("debug_traceTransaction %s: %w", hash.Hex(), err)
	}
	return uint64(frame.GasUsed), nil
}

type rpcTransaction struct {
	Hash                 common.Hash     `json:"hash"`
	From                 common.Address  `json:"from"`
	To                   *common.Address `json:"to"`
	Input                hexutil.Bytes   `json:"input"`
	Value                *hexutil.Big    `json:"value"`
	GasPrice             *hexutil.Big    `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big    `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big    `json:"maxPriorityFeePerGas"`
}

type rpcReceipt struct {
	TransactionHash   common.Hash     `json:"transactionHash"`
	GasUsed           hexutil.Uint64  `json:"gasUsed"`
	Status            hexutil.Uint64  `json:"status"`
	EffectiveGasPrice *hexutil.Big    `json:"effectiveGasPrice"`
	BlobGasUsed       *hexutil.Uint64 `json:"blobGasUsed"`
	BlobGasPrice      *hexutil.Big    `json:"blobGasPrice"`
}

type rpcBlock struct {
	Number        hexutil.Uint64   `json:"number"`
	Hash          common.Hash      `json:"hash"`
	ParentHash    common.Hash      `json:"parentHash"`
	Timestamp     hexutil.Uint64   `json:"timestamp"`
	Miner         common.Address   `json:"miner"`
	Difficulty    *hexutil.Big     `json:"difficulty"`
	GasLimit      hexutil.Uint64   `json:"gasLimit"`
	BaseFeePerGas *hexutil.Big     `json:"baseFeePerGas"`
	ExcessBlobGas *hexutil.Uint64  `json:"excessBlobGas"`
	BlobGasUsed   *hexutil.Uint64  `json:"blobGasUsed"`
	MixHash       common.Hash      `json:"mixHash"`
	Transactions  []rpcTransaction `json:"transactions"`
}

// call is CallContext with a few retries. Public endpoints drop the occasional connection
// and a sweep is long enough that one transient failure should not throw away the run.
func call(ctx context.Context, client *rpc.Client, result interface{}, method string, args ...interface{}) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err = client.CallContext(ctx, result, method, args...); err == nil {
			return nil
		}
		// A JSON-RPC error is the endpoint answering, not failing: do not retry it.
		if _, isRPCError := err.(rpc.Error); isRPCError {
			return err
		}
	}
	return err
}

func blockNumber(ctx context.Context, client *rpc.Client) (uint64, error) {
	var n hexutil.Uint64
	if err := call(ctx, client, &n, "eth_blockNumber"); err != nil {
		return 0, fmt.Errorf("eth_blockNumber: %w", err)
	}
	return uint64(n), nil
}

func blockByNumber(ctx context.Context, client *rpc.Client, number uint64) (*rpcBlock, error) {
	var block *rpcBlock
	if err := call(ctx, client, &block, "eth_getBlockByNumber", hexutil.Uint64(number), true); err != nil {
		return nil, fmt.Errorf("eth_getBlockByNumber: %w", err)
	}
	if block == nil {
		return nil, errors.New("block not found")
	}
	return block, nil
}

// blockReceipts prefers the single-call eth_getBlockReceipts and falls back to per-
// transaction receipts for endpoints that do not serve it. The result is ordered like the
// block's transaction list.
func blockReceipts(ctx context.Context, client *rpc.Client, number uint64, txs []rpcTransaction) ([]rpcReceipt, error) {
	var batched []rpcReceipt
	err := call(ctx, client, &batched, "eth_getBlockReceipts", hexutil.Uint64(number))
	if err == nil && len(batched) == len(txs) {
		byHash := make(map[common.Hash]rpcReceipt, len(batched))
		for _, r := range batched {
			byHash[r.TransactionHash] = r
		}
		ordered := make([]rpcReceipt, 0, len(txs))
		for _, tx := range txs {
			r, ok := byHash[tx.Hash]
			if !ok {
				return nil, fmt.Errorf("eth_getBlockReceipts: no receipt for %s", tx.Hash.Hex())
			}
			ordered = append(ordered, r)
		}
		return ordered, nil
	}
	ordered := make([]rpcReceipt, 0, len(txs))
	for _, tx := range txs {
		var r *rpcReceipt
		if err := call(ctx, client, &r, "eth_getTransactionReceipt", tx.Hash); err != nil {
			return nil, fmt.Errorf("eth_getTransactionReceipt %s: %w", tx.Hash.Hex(), err)
		}
		if r == nil {
			return nil, fmt.Errorf("no receipt for %s", tx.Hash.Hex())
		}
		ordered = append(ordered, *r)
	}
	return ordered, nil
}

func hasSelector(input []byte, selector []byte) bool {
	return len(input) >= 4 && string(input[:4]) == string(selector)
}

func addrString(a *common.Address) string {
	if a == nil {
		return "(creation)"
	}
	return a.Hex()
}
