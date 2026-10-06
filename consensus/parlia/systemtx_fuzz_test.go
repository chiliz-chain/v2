package parlia

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/pepper8"
	"github.com/ethereum/go-ethereum/common/pipe8"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// COR-214: Chiliz decides "is this a system transaction / a protocol deposit" in
// three places that must agree, and every field of a user-submitted transaction
// is attacker-controlled:
//
//   - consensus/parlia  IsSystemTransaction / isToSystemContract / IsSystemContract
//   - core              IsChilizFeeExemptMessage (the preCheck fee-cap exemption)
//   - consensus/parlia  IsTokenomicsDeposit / IsPepper8Deposit / IsPipe8Deposit
//     (mirrored by eth/tracers/api.go and eth/state_accessor.go)
//
// The test lives in consensus/parlia because parlia already imports core (for
// the engine's state/chain plumbing) while core does not import parlia, so this
// is the only side of the edge that can see both without a cycle.

// fuzzToTable is the biased destination table: the six BAS system contracts,
// the three BSC ones the registry marks true, one the registry marks false, the
// Pepper8/Pipe8 recipient (one address, two constants), the EVM hook address,
// and the two addresses that bracket the BAS range.
var fuzzToTable = []common.Address{
	systemcontract.StakingPoolContractAddress,
	systemcontract.GovernanceContractAddress,
	systemcontract.ChainConfigContractAddress,
	systemcontract.RuntimeUpgradeContractAddress,
	systemcontract.DeployerProxyContractAddress,
	systemcontract.TokenomicsContractAddress,
	pepper8.Pepper8RecipientAddress,
	pipe8.Pipe8RecipientAddress,
	common.HexToAddress(systemcontract.ValidatorContract),
	common.HexToAddress(systemcontract.SlashContract),
	common.HexToAddress(systemcontract.SystemRewardContract),
	common.HexToAddress(systemcontract.LightClientContract), // registered as false
	systemcontract.EvmHookRuntimeUpgradeAddress,
	common.HexToAddress("0x0000000000000000000000000000000000007000"),
	common.HexToAddress("0x0000000000000000000000000000000000007007"),
	{},
}

const (
	fuzzToTokenomics = 5
	fuzzToPepper8    = 6
	fuzzToPipe8      = 7
	fuzzToStaking    = 0
	fuzzToLightCl    = 11
)

// fuzzTxKind selects the transaction envelope. Every user-submittable type the
// Prague signer accepts is covered: both Chiliz networks schedule Berlin
// (AccessListTx), London (DynamicFeeTx), Cancun (BlobTx) and Prague (SetCodeTx),
// and each type dispatches through its own effectiveGasPrice / gasPrice
// implementation (core/types/tx_*.go), which is exactly what the two
// classification views under test consume.
const (
	fuzzKindLegacy uint8 = iota
	fuzzKindAccessList
	fuzzKindDynamicFee
	fuzzKindBlob
	fuzzKindSetCode
	fuzzKindCount
)

// fuzzBaseFee is Chiliz's base-fee floor (2,500 Gwei), the value a post-London
// header carries on every Chiliz network. It is InitialBaseFeeForBSC, not
// params.InitialBaseFee -- the latter is upstream Ethereum's 1 Gwei default,
// used only by non-Parlia chains (see consensus/misc/eip1559).
const fuzzBaseFee = uint64(params.InitialBaseFeeForBSC)

// fuzzSpecInSet is the specification of the system destination set, written
// independently of isToSystemContract.
func fuzzSpecInSet(to *common.Address) bool {
	if to == nil {
		return false
	}
	return systemcontract.IsSystemContract(*to) ||
		*to == pepper8.Pepper8RecipientAddress ||
		*to == pipe8.Pipe8RecipientAddress
}

// legacyIsTokenomicsDeposit is the pre-COR-214 implementation of
// IsTokenomicsDeposit (hex-encode the whole calldata, compare eight
// characters), kept verbatim as the reference for the equivalence property.
func legacyIsTokenomicsDeposit(to *common.Address, data []byte) bool {
	isDestinationTokenomics := bytes.Equal(to.Bytes(), systemcontract.TokenomicsContractAddress.Bytes())
	inputStr := hex.EncodeToString(data)
	isDeposit := false
	if len(inputStr) >= 8 {
		isDeposit = hex.EncodeToString(data)[:8] == "0efe6a8b"
	}
	return isDestinationTokenomics && isDeposit
}

func fuzzAddrFromSeed(seed uint64) common.Address {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], seed)
	return common.BytesToAddress(crypto.Keccak256(b[:]))
}

// fuzzBlobHashFromSeed derives a structurally valid (version 0x01) versioned
// blob hash. Neither the signer nor TransactionToMessage validates the
// commitment behind it, so the seed only needs to vary the bytes.
func fuzzBlobHashFromSeed(seed uint64) common.Hash {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], seed)
	h := common.BytesToHash(crypto.Keccak256(b[:]))
	h[0] = 0x01
	return h
}

// fuzzSignerConfig is ParliaTestChainConfig with Prague scheduled, so that
// types.LatestSigner yields the Prague signer and all five transaction kinds
// (SetCodeTx needs Prague; the shared config stops at Cancun) sign and recover.
// Copying the value leaves the shared package-level config untouched.
func fuzzSignerConfig() *params.ChainConfig {
	cfg := *params.ParliaTestChainConfig
	pragueTime := uint64(0)
	cfg.PragueTime = &pragueTime
	return &cfg
}

func TestTokenomicsDepositSelector(t *testing.T) {
	want := crypto.Keccak256([]byte("deposit(address,uint256,uint256)"))[:4]
	if !bytes.Equal(want, tokenomicsDepositSelector[:]) {
		t.Fatalf("tokenomicsDepositSelector = %x, keccak(deposit(address,uint256,uint256))[:4] = %x", tokenomicsDepositSelector, want)
	}
}

func FuzzSystemTxClassification(f *testing.F) {
	// Seeds. Argument order matches the fuzz function below:
	// toSel, toXor, toXorPos, nilTo,
	// senderSeed, fromIsCoinbase, coinbaseSeed,
	// txKind, gasPrice, feeCap, tip, baseFee, nilBaseFee, blobFeeCap, kindSeed,
	// depositPrefix, data, value.
	for i := range fuzzToTable {
		// A genuine zero-price legacy system tx from the coinbase to each
		// table entry; deposit calldata for the Tokenomics entry.
		f.Add(uint8(i), uint8(0), uint8(0), false,
			uint64(1), true, uint64(0),
			fuzzKindLegacy, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
			i == fuzzToTokenomics, []byte{}, uint64(0))
	}
	// Pepper8 deposit shape: coinbase -> recipient, zero price, non-zero value.
	f.Add(uint8(fuzzToPepper8), uint8(0), uint8(0), false,
		uint64(2), true, uint64(0),
		fuzzKindLegacy, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		false, []byte{}, uint64(1e18))
	// Pipe8 deposit shape, as a zero-fee dynamic tx.
	f.Add(uint8(fuzzToPipe8), uint8(0), uint8(0), false,
		uint64(3), true, uint64(0),
		fuzzKindDynamicFee, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		false, []byte{}, uint64(1e18))
	// User tx to a system contract with a non-zero price, not from coinbase.
	f.Add(uint8(fuzzToStaking), uint8(0), uint8(0), false,
		uint64(4), false, uint64(99),
		fuzzKindLegacy, fuzzBaseFee, uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		false, []byte{0x01, 0x02, 0x03, 0x04}, uint64(0))
	// Coinbase-sent dynamic-fee tx with zero tip and non-zero cap: the one
	// class where parlia (EffectiveGasPriceForBSC == 0) and core
	// (msg.GasPrice == min(0+baseFee, cap) > 0) legitimately disagree.
	f.Add(uint8(fuzzToTokenomics), uint8(0), uint8(0), false,
		uint64(5), true, uint64(0),
		fuzzKindDynamicFee, uint64(0), fuzzBaseFee, uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		true, []byte{}, uint64(0))
	// Same shape with a nil base fee (pre-London header).
	f.Add(uint8(fuzzToTokenomics), uint8(0), uint8(0), false,
		uint64(5), true, uint64(0),
		fuzzKindDynamicFee, uint64(0), fuzzBaseFee, uint64(0), uint64(0), true, uint64(0), uint64(0),
		true, []byte{}, uint64(0))
	// Dynamic tx with zero cap and non-zero tip (cap < tip is still signable).
	f.Add(uint8(fuzzToStaking), uint8(0), uint8(0), false,
		uint64(6), true, uint64(0),
		fuzzKindDynamicFee, uint64(0), uint64(0), uint64(7), fuzzBaseFee, false, uint64(0), uint64(0),
		false, []byte{}, uint64(0))
	// nil-`to` contract creation from the coinbase at zero price.
	f.Add(uint8(0), uint8(0), uint8(0), true,
		uint64(7), true, uint64(0),
		fuzzKindLegacy, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		true, []byte{0x60, 0x80, 0x60, 0x40}, uint64(0))
	// Off-by-one-byte neighbours of the Tokenomics contract and the recipient.
	f.Add(uint8(fuzzToTokenomics), uint8(1), uint8(19), false,
		uint64(8), true, uint64(0),
		fuzzKindLegacy, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		true, []byte{}, uint64(0))
	f.Add(uint8(fuzzToPepper8), uint8(0x80), uint8(0), false,
		uint64(9), true, uint64(0),
		fuzzKindLegacy, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		false, []byte{}, uint64(0))
	// Registry entry explicitly marked false (LightClient): never a system tx.
	f.Add(uint8(fuzzToLightCl), uint8(0), uint8(0), false,
		uint64(10), true, uint64(0),
		fuzzKindLegacy, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		false, []byte{}, uint64(0))
	// Deposit selector with a 3-byte calldata (must not match) and exactly 4.
	f.Add(uint8(fuzzToTokenomics), uint8(0), uint8(0), false,
		uint64(11), true, uint64(0),
		fuzzKindLegacy, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		false, []byte{0x0e, 0xfe, 0x6a}, uint64(0))
	f.Add(uint8(fuzzToTokenomics), uint8(0), uint8(0), false,
		uint64(12), true, uint64(0),
		fuzzKindLegacy, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		false, []byte{0x0e, 0xfe, 0x6a, 0x8b}, uint64(0))

	// --- the remaining envelope kinds (AccessList, Blob, SetCode) ---

	// Zero-price AccessList tx from the coinbase to a system contract: must
	// classify exactly like the legacy shape (gasPrice on both views).
	f.Add(uint8(fuzzToStaking), uint8(0), uint8(0), false,
		uint64(13), true, uint64(0),
		fuzzKindAccessList, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		false, []byte{}, uint64(0))
	// Priced AccessList tx to a system contract, not from the coinbase.
	f.Add(uint8(fuzzToTokenomics), uint8(0), uint8(0), false,
		uint64(14), false, uint64(98),
		fuzzKindAccessList, fuzzBaseFee, uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(0),
		true, []byte{}, uint64(0))
	// Zero-tip, zero-cap BlobTx from the coinbase to the Tokenomics contract
	// carrying deposit calldata: the deposit shape in a blob envelope.
	f.Add(uint8(fuzzToTokenomics), uint8(0), uint8(0), false,
		uint64(15), true, uint64(0),
		fuzzKindBlob, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(1), uint64(21),
		true, []byte{}, uint64(0))
	// BlobTx to the Tokenomics contract in the tip=0/cap>0 divergence class.
	f.Add(uint8(fuzzToTokenomics), uint8(0), uint8(0), false,
		uint64(16), true, uint64(0),
		fuzzKindBlob, uint64(0), fuzzBaseFee, uint64(0), fuzzBaseFee, false, uint64(1), uint64(22),
		true, []byte{}, uint64(0))
	// Zero-tip, zero-cap SetCodeTx from the coinbase to a system contract.
	f.Add(uint8(fuzzToStaking), uint8(0), uint8(0), false,
		uint64(17), true, uint64(0),
		fuzzKindSetCode, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(23),
		false, []byte{}, uint64(0))
	// SetCodeTx to the Pipe8 recipient in the divergence class, nil base fee.
	f.Add(uint8(fuzzToPipe8), uint8(0), uint8(0), false,
		uint64(18), true, uint64(0),
		fuzzKindSetCode, uint64(0), fuzzBaseFee, uint64(0), uint64(0), true, uint64(0), uint64(24),
		false, []byte{}, uint64(1e18))
	// Blob and SetCode kinds with a nil destination: exercises the fallback
	// to the dynamic-fee envelope (a create tx of those kinds is invalid).
	f.Add(uint8(0), uint8(0), uint8(0), true,
		uint64(19), true, uint64(0),
		fuzzKindBlob, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(1), uint64(25),
		false, []byte{0x60, 0x80}, uint64(0))
	f.Add(uint8(0), uint8(0), uint8(0), true,
		uint64(20), true, uint64(0),
		fuzzKindSetCode, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(0), uint64(26),
		false, []byte{0x60, 0x80}, uint64(0))
	// Kind selector beyond the table (reduced mod fuzzKindCount).
	f.Add(uint8(fuzzToPepper8), uint8(0), uint8(0), false,
		uint64(21), true, uint64(0),
		fuzzKindCount+fuzzKindBlob, uint64(0), uint64(0), uint64(0), fuzzBaseFee, false, uint64(1), uint64(27),
		false, []byte{}, uint64(1e18))

	// The signer parlia uses is types.LatestSigner(chainConfig). Production
	// configs schedule Prague, so the test config does too; see fuzzSignerConfig.
	cfg := fuzzSignerConfig()
	signer := types.LatestSigner(cfg)
	p := &Parlia{chainConfig: cfg, signer: signer}
	chainID256 := uint256.MustFromBig(cfg.ChainID)

	f.Fuzz(func(t *testing.T,
		toSel, toXor, toXorPos uint8, nilTo bool,
		senderSeed uint64, fromIsCoinbase bool, coinbaseSeed uint64,
		txKind uint8, gasPrice, feeCap, tip, baseFee uint64, nilBaseFee bool, blobFeeCap, kindSeed uint64,
		depositPrefix bool, data []byte, value uint64,
	) {
		// --- destination ---
		var to *common.Address
		if !nilTo {
			addr := fuzzToTable[int(toSel)%len(fuzzToTable)]
			if toXor != 0 {
				addr[int(toXorPos)%common.AddressLength] ^= toXor
			}
			to = &addr
		}

		// --- sender (deterministic key from seed) and coinbase ---
		var seedBytes [8]byte
		binary.LittleEndian.PutUint64(seedBytes[:], senderSeed)
		key, err := crypto.ToECDSA(crypto.Keccak256(seedBytes[:]))
		if err != nil {
			t.Skip("seed does not map to a valid secp256k1 scalar")
		}
		sender := crypto.PubkeyToAddress(key.PublicKey)
		coinbase := sender
		if !fromIsCoinbase {
			coinbase = fuzzAddrFromSeed(coinbaseSeed)
			if coinbase == sender {
				t.Skip("coinbase seed collided with sender")
			}
		}

		// --- calldata ---
		if depositPrefix {
			data = append(append([]byte{}, tokenomicsDepositSelector[:]...), data...)
		}

		// --- transaction ---
		kind := txKind % fuzzKindCount
		// BlobTx and SetCodeTx carry a non-pointer To: a contract creation of
		// those kinds does not exist, and building one would make the input
		// invalid rather than interesting. Fall back to the dynamic-fee
		// envelope, which shares their gas-price semantics, instead of
		// skipping the input.
		if to == nil && (kind == fuzzKindBlob || kind == fuzzKindSetCode) {
			kind = fuzzKindDynamicFee
		}
		var txdata types.TxData
		switch kind {
		case fuzzKindLegacy:
			txdata = &types.LegacyTx{
				Nonce:    0,
				GasPrice: new(big.Int).SetUint64(gasPrice),
				Gas:      params.TxGas,
				To:       to,
				Value:    new(big.Int).SetUint64(value),
				Data:     data,
			}
		case fuzzKindAccessList:
			txdata = &types.AccessListTx{
				ChainID:  cfg.ChainID,
				Nonce:    0,
				GasPrice: new(big.Int).SetUint64(gasPrice),
				Gas:      params.TxGas,
				To:       to,
				Value:    new(big.Int).SetUint64(value),
				Data:     data,
			}
		case fuzzKindDynamicFee:
			txdata = &types.DynamicFeeTx{
				ChainID:   cfg.ChainID,
				Nonce:     0,
				GasTipCap: new(big.Int).SetUint64(tip),
				GasFeeCap: new(big.Int).SetUint64(feeCap),
				Gas:       params.TxGas,
				To:        to,
				Value:     new(big.Int).SetUint64(value),
				Data:      data,
			}
		case fuzzKindBlob:
			txdata = &types.BlobTx{
				ChainID:    chainID256,
				Nonce:      0,
				GasTipCap:  uint256.NewInt(tip),
				GasFeeCap:  uint256.NewInt(feeCap),
				Gas:        params.TxGas,
				To:         *to,
				Value:      uint256.NewInt(value),
				Data:       data,
				BlobFeeCap: uint256.NewInt(blobFeeCap),
				BlobHashes: []common.Hash{fuzzBlobHashFromSeed(kindSeed)},
			}
		case fuzzKindSetCode:
			// One authorization keeps the shape protocol-valid (preCheck
			// rejects an empty list); neither the signer nor
			// TransactionToMessage validates the authorization itself.
			txdata = &types.SetCodeTx{
				ChainID:   chainID256,
				Nonce:     0,
				GasTipCap: uint256.NewInt(tip),
				GasFeeCap: uint256.NewInt(feeCap),
				Gas:       params.TxGas,
				To:        *to,
				Value:     uint256.NewInt(value),
				Data:      data,
				AuthList: []types.SetCodeAuthorization{{
					ChainID: *chainID256,
					Address: fuzzAddrFromSeed(kindSeed),
					Nonce:   kindSeed,
					V:       0,
					R:       *uint256.NewInt(1),
					S:       *uint256.NewInt(1),
				}},
			}
		default:
			t.Fatalf("unhandled tx kind %d", kind)
		}
		tx := types.MustSignNewTx(key, signer, txdata)
		// The envelope must be the one selected; a silent downgrade would
		// leave a kind untested without failing anything below.
		wantType := map[uint8]uint8{
			fuzzKindLegacy:     types.LegacyTxType,
			fuzzKindAccessList: types.AccessListTxType,
			fuzzKindDynamicFee: types.DynamicFeeTxType,
			fuzzKindBlob:       types.BlobTxType,
			fuzzKindSetCode:    types.SetCodeTxType,
		}[kind]
		if tx.Type() != wantType {
			t.Fatalf("tx.Type() = %d, want %d for kind %d", tx.Type(), wantType, kind)
		}

		var headerBaseFee *big.Int
		if !nilBaseFee {
			headerBaseFee = new(big.Int).SetUint64(baseFee)
		}
		header := &types.Header{
			Number:   big.NewInt(1),
			Coinbase: coinbase,
			BaseFee:  headerBaseFee,
		}

		// --- classify on both sides ---
		isSystem, err := p.IsSystemTransaction(tx, header)
		if err != nil {
			t.Fatalf("IsSystemTransaction returned an error for a correctly signed %d-type tx: %v", tx.Type(), err)
		}
		msg, err := core.TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			t.Fatalf("TransactionToMessage (type %d): %v", tx.Type(), err)
		}
		if msg.From != sender {
			t.Fatalf("recovered sender %s != key address %s", msg.From, sender)
		}
		exempt := core.IsChilizFeeExemptMessage(msg.From, msg.To, msg.GasPrice, header.Coinbase)

		inSet := fuzzSpecInSet(to)
		fromCoinbase := sender == coinbase

		// Pin the two gas-price views this property depends on. The five
		// envelopes fall into two families (core/types/tx_*.go):
		//   Legacy, AccessList           effectiveGasPrice = gasPrice() = GasPrice
		//   DynamicFee, Blob, SetCode    effectiveGasPrice(0) = min(cap, tip)
		//                                gasPrice() = cap, so msg.GasPrice is
		//                                cap (nil baseFee) | min(tip+baseFee, cap)
		// Blob and SetCode hold uint256 fields and convert with ToBig, which
		// must not change the arithmetic.
		dynamic := kind == fuzzKindDynamicFee || kind == fuzzKindBlob || kind == fuzzKindSetCode
		effBSC := tx.EffectiveGasPriceForBSC()
		var wantEff, wantMsgPrice *big.Int
		if dynamic {
			capB, tipB := new(big.Int).SetUint64(feeCap), new(big.Int).SetUint64(tip)
			wantEff = new(big.Int).Set(capB)
			if tipB.Cmp(capB) < 0 {
				wantEff.Set(tipB)
			}
			if headerBaseFee == nil {
				wantMsgPrice = new(big.Int).Set(capB)
			} else {
				wantMsgPrice = new(big.Int).Add(tipB, headerBaseFee)
				if wantMsgPrice.Cmp(capB) > 0 {
					wantMsgPrice.Set(capB)
				}
			}
		} else {
			wantEff = new(big.Int).SetUint64(gasPrice)
			wantMsgPrice = new(big.Int).SetUint64(gasPrice)
		}
		if effBSC.Cmp(wantEff) != 0 {
			t.Fatalf("EffectiveGasPriceForBSC = %s, want %s (kind=%d gasPrice=%d cap=%d tip=%d)", effBSC, wantEff, kind, gasPrice, feeCap, tip)
		}
		if msg.GasPrice.Cmp(wantMsgPrice) != 0 {
			t.Fatalf("msg.GasPrice = %s, want %s (kind=%d gasPrice=%d cap=%d tip=%d baseFee=%v)", msg.GasPrice, wantMsgPrice, kind, gasPrice, feeCap, tip, headerBaseFee)
		}
		if dynamic && tip == 0 && feeCap > 0 && effBSC.Sign() != 0 {
			t.Fatalf("EffectiveGasPriceForBSC must be zero for a kind-%d tx with tip=0, cap=%d; got %s", kind, feeCap, effBSC)
		}
		// The blob-only fields must ride along unchanged (BlobGasFeeCap is
		// what preCheck compares against the header's blob base fee) and must
		// be absent on every other envelope.
		if kind == fuzzKindBlob {
			if msg.BlobGasFeeCap == nil || msg.BlobGasFeeCap.Cmp(new(big.Int).SetUint64(blobFeeCap)) != 0 {
				t.Fatalf("msg.BlobGasFeeCap = %v, want %d", msg.BlobGasFeeCap, blobFeeCap)
			}
			if len(msg.BlobHashes) != 1 || msg.BlobHashes[0] != fuzzBlobHashFromSeed(kindSeed) {
				t.Fatalf("msg.BlobHashes = %v, want the single fuzzed hash", msg.BlobHashes)
			}
		} else if msg.BlobGasFeeCap != nil || len(msg.BlobHashes) != 0 {
			t.Fatalf("kind-%d tx carries blob fields: feeCap=%v hashes=%v", kind, msg.BlobGasFeeCap, msg.BlobHashes)
		}
		if (kind == fuzzKindSetCode) != (len(msg.SetCodeAuthorizations) == 1) {
			t.Fatalf("kind-%d tx carries %d authorizations", kind, len(msg.SetCodeAuthorizations))
		}

		// Property 3: a non-zero price on the respective view is never a
		// system tx / never exempt.
		if effBSC.Sign() != 0 && isSystem {
			t.Fatalf("non-zero EffectiveGasPriceForBSC %s classified as system tx", effBSC)
		}
		if msg.GasPrice.Sign() != 0 && exempt {
			t.Fatalf("non-zero msg.GasPrice %s classified as fee-exempt", msg.GasPrice)
		}

		// Property 2: a non-coinbase sender is never exempt and never a system tx.
		if !fromCoinbase && (isSystem || exempt) {
			t.Fatalf("non-coinbase sender classified as system=%v exempt=%v", isSystem, exempt)
		}

		// Pin each side against its own specification.
		if want := inSet && fromCoinbase && effBSC.Sign() == 0; isSystem != want {
			t.Fatalf("IsSystemTransaction = %v, want %v (to=%v inSet=%v fromCoinbase=%v effBSC=%s)", isSystem, want, to, inSet, fromCoinbase, effBSC)
		}
		if want := inSet && fromCoinbase && msg.GasPrice.Sign() == 0; exempt != want {
			t.Fatalf("IsChilizFeeExemptMessage = %v, want %v (to=%v inSet=%v fromCoinbase=%v msgPrice=%s)", exempt, want, to, inSet, fromCoinbase, msg.GasPrice)
		}
		if got := p.IsSystemContract(to); got != inSet {
			t.Fatalf("IsSystemContract(%v) = %v, want %v", to, got, inSet)
		}

		// Property 1: cross-module agreement. msg.GasPrice == 0 implies
		// EffectiveGasPriceForBSC == 0, so exempt always implies system. The
		// converse fails exactly for a dynamic-fee-shaped tx (DynamicFee,
		// Blob, SetCode) with tip == 0, cap > 0 and a nil or positive base
		// fee: parlia sees min(cap, 0) = 0 while core sees min(0+baseFee,
		// cap) > 0 (or cap when baseFee is nil).
		divergent := dynamic && tip == 0 && feeCap > 0 && (headerBaseFee == nil || headerBaseFee.Sign() > 0)
		if exempt && !isSystem {
			t.Fatalf("fee-exempt in core but not a system tx in parlia (to=%v kind=%d cap=%d tip=%d baseFee=%v)", to, kind, feeCap, tip, headerBaseFee)
		}
		if !divergent && exempt != isSystem {
			t.Fatalf("core exempt=%v != parlia system=%v outside the documented divergence class (to=%v kind=%d gasPrice=%d cap=%d tip=%d baseFee=%v)",
				exempt, isSystem, to, kind, gasPrice, feeCap, tip, headerBaseFee)
		}
		if divergent {
			if exempt {
				t.Fatalf("core must not exempt a kind-%d tx with tip=0 cap=%d baseFee=%v", kind, feeCap, headerBaseFee)
			}
			if isSystem != (inSet && fromCoinbase) {
				t.Fatalf("in the divergence class parlia must classify purely on to/from: got %v, inSet=%v fromCoinbase=%v", isSystem, inSet, fromCoinbase)
			}
		}

		// Properties 4 and 5: the deposit predicates. They dereference `to`,
		// and every production call site (eth/tracers/api.go behind
		// IsSystemTransaction, eth/state_accessor.go behind IsSystemContract)
		// reaches them only after a nil destination has already been rejected,
		// so they are exercised here with the same precondition.
		if to != nil {
			gotTok := p.IsTokenomicsDeposit(to, data)
			wantTok := *to == systemcontract.TokenomicsContractAddress && len(data) >= 4 &&
				bytes.Equal(data[:4], []byte{0x0e, 0xfe, 0x6a, 0x8b})
			if gotTok != wantTok {
				t.Fatalf("IsTokenomicsDeposit(%s, %x) = %v, want %v", to, data, gotTok, wantTok)
			}
			if legacy := legacyIsTokenomicsDeposit(to, data); gotTok != legacy {
				t.Fatalf("IsTokenomicsDeposit(%s, %x) = %v, legacy hex implementation = %v", to, data, gotTok, legacy)
			}
			gotP8 := p.IsPepper8Deposit(&msg.From, to, &header.Coinbase)
			if want := *to == pepper8.Pepper8RecipientAddress && fromCoinbase; gotP8 != want {
				t.Fatalf("IsPepper8Deposit = %v, want %v (to=%s fromCoinbase=%v)", gotP8, want, to, fromCoinbase)
			}
			gotPi8 := p.IsPipe8Deposit(&msg.From, to, header.Coinbase)
			if want := *to == pipe8.Pipe8RecipientAddress && fromCoinbase; gotPi8 != want {
				t.Fatalf("IsPipe8Deposit = %v, want %v (to=%s fromCoinbase=%v)", gotPi8, want, to, fromCoinbase)
			}
			// A deposit of any kind is only ever credited behind the system-tx
			// gate; a tx that is a deposit shape but not a system tx must not
			// be reachable as a credit. Pin the containment on the parlia side.
			if (gotP8 || gotPi8) && effBSC.Sign() == 0 && !isSystem {
				t.Fatalf("deposit shape at zero price not classified as system tx (to=%s)", to)
			}
		}
	})
}
