// Copyright 2026 The go-ethereum Authors
// SPDX-License-Identifier: LGPL-3.0
//
// deployer-proxy-sunset-check exercises DeployerProxySunset fork behavior via JSON-RPC:
// it verifies that the deployment hook (registerDeployedContract) and invocation hook
// (checkContractActive) are present in traces before the fork and absent after it.
//
// Usage: deployer-proxy-sunset-check <rpc_url> <fork_timestamp_unix> <hex_private_key>

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
)

// Creation bytecode for a minimal storage contract (DummySunsetCheck):
//
//	contract Dummy { uint256 public x; function set(uint256 v) external { x = v; } }
//
// Compiled with solc 0.8.17, optimizer disabled.
var dummyCreationBytecode = common.Hex2Bytes("608060405234801561001057600080fd5b5060b18061001f6000396000f3fe6080604052348015600f57600080fd5b506004361060325760003560e01c80630c55699c14603757806360fe47b1146051575b600080fd5b603f60005481565b60405190815260200160405180910390f35b6061605c3660046063565b600055565b005b600060208284031215607457600080fd5b503591905056fea26469706673582212208d6bf60bdd14199f61adb170ca6377f362ab12e6d7942090290aab407778c51a64736f6c63430008110033")

var deployerProxy = systemcontract.DeployerProxyContractAddress

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "usage: %s <rpc_url> <fork_timestamp_unix> <hex_private_key>\n", os.Args[0])
		return errors.New("invalid arguments")
	}
	rpcURL := os.Args[1]
	forkTime, err := strconv.ParseUint(os.Args[2], 10, 64)
	if err != nil {
		return fmt.Errorf("fork timestamp: %w", err)
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(os.Args[3], "0x"))
	if err != nil {
		return fmt.Errorf("private key: %w", err)
	}
	from := crypto.PubkeyToAddress(key.PublicKey)

	ctx := context.Background()
	ec, err := ethclient.Dial(rpcURL)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer ec.Close()
	gc := gethclient.New(ec.Client())

	chainID, err := ec.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("chain id: %w", err)
	}

	ts, err := latestBlockTime(ctx, ec)
	if err != nil {
		return err
	}
	if ts >= forkTime {
		return fmt.Errorf("latest block time %d already >= fork %d (need to start before fork)", ts, forkTime)
	}

	// Use ABI-derived selectors to avoid hardcoding and stay in sync with the ABI definition.
	regSel := systemcontract.EvmHooksAbi.Methods["registerDeployedContract"].ID
	checkSel := systemcontract.EvmHooksAbi.Methods["checkContractActive"].ID

	callData, err := encodeSetCall(big.NewInt(42))
	if err != nil {
		return err
	}

	// --- Pre-fork: deploy and call ---

	hDeploy1, receipt1, err := sendContractCreation(ctx, ec, chainID, key, from, dummyCreationBytecode)
	if err != nil {
		return fmt.Errorf("deploy #1 (pre-fork): %w", err)
	}
	addr1 := receipt1.ContractAddress
	fmt.Printf("DeployTx1 %s  contract %s  gasUsed=%d\n", hDeploy1.Hex(), addr1.Hex(), receipt1.GasUsed)

	trace1, err := traceCallTracer(ctx, gc, hDeploy1)
	if err != nil {
		return err
	}
	if !traceHasProxyCall(trace1, deployerProxy, regSel) {
		return fmt.Errorf("DeployTx1 trace: expected registerDeployedContract call to %s (pre-fork)", deployerProxy.Hex())
	}
	fmt.Println("DeployTx1 trace: registerDeployedContract present (ok)")

	hCallA, receiptCallA, err := sendContractCall(ctx, ec, chainID, key, from, addr1, callData)
	if err != nil {
		return fmt.Errorf("call #A (pre-fork): %w", err)
	}
	fmt.Printf("CallTxA   %s  gasUsed=%d\n", hCallA.Hex(), receiptCallA.GasUsed)

	traceCallA, err := traceCallTracer(ctx, gc, hCallA)
	if err != nil {
		return err
	}
	if !traceHasProxyCall(traceCallA, deployerProxy, checkSel) {
		return fmt.Errorf("CallTxA trace: expected checkContractActive call to %s (pre-fork)", deployerProxy.Hex())
	}
	fmt.Println("CallTxA   trace: checkContractActive present (ok)")

	// --- Wait for fork ---

	fmt.Printf("\nWaiting until block time >= %d ...\n", forkTime)
	for {
		ts, err := latestBlockTime(ctx, ec)
		if err != nil {
			return err
		}
		if ts >= forkTime {
			fmt.Printf("Fork active (latest block time %d)\n\n", ts)
			break
		}
		time.Sleep(2 * time.Second)
	}

	// --- Re-trace historical pre-fork transactions ---

	trace1b, err := traceCallTracer(ctx, gc, hDeploy1)
	if err != nil {
		return err
	}
	if !traceHasProxyCall(trace1b, deployerProxy, regSel) {
		return fmt.Errorf("re-trace DeployTx1: registerDeployedContract must still be present (backward compat)")
	}
	fmt.Println("Re-trace DeployTx1: registerDeployedContract present (ok)")

	traceCallAb, err := traceCallTracer(ctx, gc, hCallA)
	if err != nil {
		return err
	}
	if !traceHasProxyCall(traceCallAb, deployerProxy, checkSel) {
		return fmt.Errorf("re-trace CallTxA: checkContractActive must still be present (backward compat)")
	}
	fmt.Println("Re-trace CallTxA:   checkContractActive present (ok)")

	// --- Post-fork: deploy and call ---

	hDeploy2, receipt2, err := sendContractCreation(ctx, ec, chainID, key, from, dummyCreationBytecode)
	if err != nil {
		return fmt.Errorf("deploy #2 (post-fork): %w", err)
	}
	addr2 := receipt2.ContractAddress
	fmt.Printf("\nDeployTx2 %s  contract %s  gasUsed=%d\n", hDeploy2.Hex(), addr2.Hex(), receipt2.GasUsed)

	trace2, err := traceCallTracer(ctx, gc, hDeploy2)
	if err != nil {
		return err
	}
	if traceHasProxyCall(trace2, deployerProxy, regSel) {
		return fmt.Errorf("DeployTx2 trace: registerDeployedContract must be absent (post-fork)")
	}
	fmt.Println("DeployTx2 trace: registerDeployedContract absent (ok)")

	hCallB, receiptCallB, err := sendContractCall(ctx, ec, chainID, key, from, addr1, callData)
	if err != nil {
		return fmt.Errorf("call #B (post-fork): %w", err)
	}
	fmt.Printf("CallTxB   %s  gasUsed=%d\n", hCallB.Hex(), receiptCallB.GasUsed)

	traceCallB, err := traceCallTracer(ctx, gc, hCallB)
	if err != nil {
		return err
	}
	if traceHasProxyCall(traceCallB, deployerProxy, checkSel) {
		return fmt.Errorf("CallTxB trace: checkContractActive must be absent (post-fork)")
	}
	fmt.Println("CallTxB   trace: checkContractActive absent (ok)")

	// --- Gas summary ---

	fmt.Println("\n--- Gas comparison ---")
	fmt.Printf("Deployments:  pre=%d  post=%d  saved=%d\n",
		receipt1.GasUsed, receipt2.GasUsed, int64(receipt1.GasUsed)-int64(receipt2.GasUsed))
	fmt.Printf("Calls:        pre=%d  post=%d  saved=%d\n",
		receiptCallA.GasUsed, receiptCallB.GasUsed, int64(receiptCallA.GasUsed)-int64(receiptCallB.GasUsed))

	fmt.Println("\nAll checks passed.")
	return nil
}

// encodeSetCall ABI-encodes a call to Dummy.set(v).
func encodeSetCall(v *big.Int) ([]byte, error) {
	// selector for set(uint256): 0x60fe47b1
	sel := []byte{0x60, 0xfe, 0x47, 0xb1}
	padded := common.LeftPadBytes(v.Bytes(), 32)
	return append(sel, padded...), nil
}

func latestBlockTime(ctx context.Context, ec *ethclient.Client) (uint64, error) {
	h, err := ec.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, err
	}
	return h.Time, nil
}

func traceCallTracer(ctx context.Context, gc *gethclient.Client, h common.Hash) (*callFrameJSON, error) {
	name := "callTracer"
	cfg := &tracers.TraceConfig{Tracer: &name}
	raw, err := gc.TraceTransaction(ctx, h, cfg)
	if err != nil {
		return nil, fmt.Errorf("trace %s: %w", h.Hex(), err)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var frame callFrameJSON
	if err := json.Unmarshal(b, &frame); err != nil {
		return nil, err
	}
	return &frame, nil
}

// callFrameJSON matches the geth native callTracer JSON output (subset used here).
type callFrameJSON struct {
	Type  string          `json:"type"`
	To    string          `json:"to"`
	Input hexutil.Bytes   `json:"input"`
	Calls []callFrameJSON `json:"calls"`
}

// traceHasProxyCall recursively searches the call tree for a call to proxy with the given 4-byte selector.
func traceHasProxyCall(f *callFrameJSON, proxy common.Address, selector []byte) bool {
	if f == nil {
		return false
	}
	if common.IsHexAddress(f.To) {
		to := common.HexToAddress(f.To)
		if bytes.Equal(to.Bytes(), proxy.Bytes()) && len(f.Input) >= 4 && bytes.Equal(f.Input[:4], selector) {
			return true
		}
	}
	for i := range f.Calls {
		if traceHasProxyCall(&f.Calls[i], proxy, selector) {
			return true
		}
	}
	return false
}

func sendContractCreation(ctx context.Context, ec *ethclient.Client, chainID *big.Int, key *ecdsa.PrivateKey, from common.Address, code []byte) (common.Hash, *types.Receipt, error) {
	nonce, err := ec.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, nil, err
	}
	gasLimit, err := ec.EstimateGas(ctx, ethereum.CallMsg{From: from, Data: code})
	if err != nil {
		return common.Hash{}, nil, err
	}
	tx, err := signDynamicFeeTx(ctx, ec, chainID, nonce, gasLimit, nil, code, key)
	if err != nil {
		return common.Hash{}, nil, err
	}
	return sendAndWait(ctx, ec, tx)
}

func sendContractCall(ctx context.Context, ec *ethclient.Client, chainID *big.Int, key *ecdsa.PrivateKey, from, to common.Address, data []byte) (common.Hash, *types.Receipt, error) {
	nonce, err := ec.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, nil, err
	}
	gasLimit, err := ec.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Data: data})
	if err != nil {
		return common.Hash{}, nil, err
	}
	tx, err := signDynamicFeeTx(ctx, ec, chainID, nonce, gasLimit, &to, data, key)
	if err != nil {
		return common.Hash{}, nil, err
	}
	return sendAndWait(ctx, ec, tx)
}

func signDynamicFeeTx(ctx context.Context, ec *ethclient.Client, chainID *big.Int, nonce, gasLimit uint64, to *common.Address, data []byte, key *ecdsa.PrivateKey) (*types.Transaction, error) {
	tip, err := ec.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, err
	}
	head, err := ec.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, err
	}
	if head.BaseFee == nil {
		gasPrice, err := ec.SuggestGasPrice(ctx)
		if err != nil {
			return nil, err
		}
		return types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    nonce,
			GasPrice: gasPrice,
			Gas:      gasLimit,
			To:       to,
			Value:    big.NewInt(0),
			Data:     data,
		}), types.LatestSignerForChainID(chainID), key)
	}
	gasFeeCap := new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))
	return types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: tip,
		GasFeeCap: gasFeeCap,
		Gas:       gasLimit,
		To:        to,
		Value:     big.NewInt(0),
		Data:      data,
	}), types.LatestSignerForChainID(chainID), key)
}

func sendAndWait(ctx context.Context, ec *ethclient.Client, tx *types.Transaction) (common.Hash, *types.Receipt, error) {
	if err := ec.SendTransaction(ctx, tx); err != nil {
		return common.Hash{}, nil, err
	}
	h := tx.Hash()
	receipt, err := bind.WaitMined(ctx, ec, tx)
	if err != nil {
		return h, nil, err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return h, receipt, fmt.Errorf("tx %s failed (status %d)", h.Hex(), receipt.Status)
	}
	return h, receipt, nil
}
