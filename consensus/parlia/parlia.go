package parlia

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/holiman/uint256"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls"
	"github.com/willf/bitset"
	"golang.org/x/crypto/sha3"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/gopool"
	"github.com/ethereum/go-ethereum/common/hexutil"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/common/systemcontract"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	inMemorySnapshots  = 256   // Number of recent snapshots to keep in memory
	inMemorySignatures = 4096  // Number of recent block signatures to keep in memory
	inMemoryHeaders    = 86400 // Number of recent headers to keep in memory for double sign detection,

	checkpointInterval = 1024        // Number of blocks after which to save the snapshot to the database
	defaultEpochLength = uint64(200) // Default number of blocks of checkpoint to update validatorSet from contract
	defaultTurnLength  = uint8(1)    // Default consecutive number of blocks a validator receives priority for block production

	extraVanity      = 32 // Fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal        = 65 // Fixed number of extra-data suffix bytes reserved for signer seal
	nextForkHashSize = 4  // Fixed number of extra-data suffix bytes reserved for nextForkHash.
	turnLengthSize   = 1  // Fixed number of extra-data suffix bytes reserved for turnLength

	validatorBytesLengthBeforeLuban = common.AddressLength
	validatorBytesLength            = common.AddressLength + types.BLSPublicKeyLength
	validatorNumberSize             = 1 // Fixed number of extra prefix bytes reserved for validator number after Luban

	wiggleTime         = uint64(1) // second, Random delay (per signer) to allow concurrent signers
	initialBackOffTime = uint64(1) // second
	processBackOffTime = uint64(1) // second

	systemRewardPercent = 5 // it means 1/3  percentage of gas fee incoming will be distributed to system

	collectAdditionalVotesRewardRatio = 100 // ratio of additional reward for collecting more votes than needed, the denominator is 100
)

var (
	uncleHash  = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.
	diffInTurn = big.NewInt(2)            // Block difficulty for in-turn signatures
	diffNoTurn = big.NewInt(1)            // Block difficulty for out-of-turn signatures
	// 100 native token
	maxSystemBalance                  = new(uint256.Int).Mul(uint256.NewInt(100), uint256.NewInt(params.Ether))
	verifyVoteAttestationErrorCounter = metrics.NewRegisteredCounter("parlia/verifyVoteAttestation/error", nil)
	updateAttestationErrorCounter     = metrics.NewRegisteredCounter("parlia/updateAttestation/error", nil)
	validVotesfromSelfCounter         = metrics.NewRegisteredCounter("parlia/VerifyVote/self", nil)
	doubleSignCounter                 = metrics.NewRegisteredCounter("parlia/doublesign", nil)
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	// errUnknownBlock is returned when the list of validators is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errMissingVanity is returned if a block's extra-data section is shorter than
	// 32 bytes, which is required to store the signer vanity.
	errMissingVanity = errors.New("extra-data 32 byte vanity prefix missing")

	// errMissingSignature is returned if a block's extra-data section doesn't seem
	// to contain a 65 byte secp256k1 signature.
	errMissingSignature = errors.New("extra-data 65 byte signature suffix missing")

	// errExtraValidators is returned if non-sprint-end block contain validator data in
	// their extra-data fields.
	errExtraValidators = errors.New("non-sprint-end block contains extra validator list")

	// errInvalidSpanValidators is returned if a block contains an
	// invalid list of validators (i.e. non divisible by 20 bytes).
	errInvalidSpanValidators = errors.New("invalid validator list on sprint end block")

	// errInvalidTurnLength is returned if a block contains an
	// invalid length of turn (i.e. no data left after parsing validators).
	errInvalidTurnLength = errors.New("invalid turnLength")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")

	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	// errMismatchingEpochValidators is returned if a sprint block contains a
	// list of validators different than the one the local node calculated.
	errMismatchingEpochValidators = errors.New("mismatching validator list on epoch block")

	// errMismatchingEpochTurnLength is returned if a sprint block contains a
	// turn length different than the one the local node calculated.
	errMismatchingEpochTurnLength = errors.New("mismatching turn length on epoch block")

	// errInvalidDifficulty is returned if the difficulty of a block is missing.
	errInvalidDifficulty = errors.New("invalid difficulty")

	// errWrongDifficulty is returned if the difficulty of a block doesn't match the
	// turn of the signer.
	errWrongDifficulty = errors.New("wrong difficulty")

	// errOutOfRangeChain is returned if an authorization list is attempted to
	// be modified via out-of-range or non-contiguous headers.
	errOutOfRangeChain = errors.New("out of range or non-contiguous chain")

	// errBlockHashInconsistent is returned if an authorization list is attempted to
	// insert an inconsistent block.
	errBlockHashInconsistent = errors.New("the block hash is inconsistent")

	// errUnauthorizedValidator is returned if a header is signed by a non-authorized entity.
	errUnauthorizedValidator = func(val string) error {
		return errors.New("unauthorized validator: " + val)
	}

	// errCoinBaseMisMatch is returned if a header's coinbase do not match with signature
	errCoinBaseMisMatch = errors.New("coinbase do not match with signature")

	// errRecentlySigned is returned if a header is signed by an authorized entity
	// that already signed a header recently, thus is temporarily not allowed to.
	errRecentlySigned = errors.New("recently signed")
)

// SignerFn is a signer callback function to request a header to be signed by a
// backing account.
type SignerFn func(accounts.Account, string, []byte) ([]byte, error)
type SignerTxFn func(accounts.Account, *types.Transaction, *big.Int) (*types.Transaction, error)

func isToSystemContract(to common.Address) bool {
	return systemcontract.IsSystemContract(to) || to == getPepper8RecipientAddress()
}

// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header, sigCache *lru.ARCCache, chainId *big.Int) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()
	if address, known := sigCache.Get(hash); known {
		return address.(common.Address), nil
	}
	// Retrieve the signature from the header extra-data
	if len(header.Extra) < extraSeal {
		return common.Address{}, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-extraSeal:]

	// Recover the public key and the Ethereum address
	pubkey, err := crypto.Ecrecover(types.SealHash(header, chainId).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigCache.Add(hash, signer)
	return signer, nil
}

// ParliaRLP returns the rlp bytes which needs to be signed for the parlia
// sealing. The RLP to sign consists of the entire header apart from the 65 byte signature
// contained at the end of the extra data.
//
// Note, the method requires the extra data to be at least 65 bytes, otherwise it
// panics. This is done to avoid accidentally using both forms (signature present
// or not), which could be abused to produce different hashes for the same header.
func ParliaRLP(header *types.Header, chainId *big.Int) []byte {
	b := new(bytes.Buffer)
	types.EncodeSigHeader(b, header, chainId)
	return b.Bytes()
}

// Parlia is the consensus engine of BSC
type Parlia struct {
	chainConfig *params.ChainConfig  // Chain config
	config      *params.ParliaConfig // Consensus engine configuration parameters for parlia consensus
	genesisHash common.Hash
	db          ethdb.Database // Database to store and retrieve snapshot checkpoints

	recentSnaps   *lru.ARCCache // Snapshots for recent block to speed up
	signatures    *lru.ARCCache // Signatures of recent blocks to speed up mining
	recentHeaders *lru.ARCCache //
	// Recent headers to check for double signing: key includes block number and miner. value is the block header
	// If same key's value already exists for different block header roots then double sign is detected

	signer types.Signer

	val      common.Address // Ethereum address of the signing key
	signFn   SignerFn       // Signer function to authorize hashes with
	signTxFn SignerTxFn

	lock sync.RWMutex // Protects the signer fields

	ethAPI                     *ethapi.BlockChainAPI
	VotePool                   consensus.VotePool
	validatorSetABIBeforeLuban abi.ABI
	validatorSetABI            abi.ABI
	slashABI                   abi.ABI
	tokenomicsABI              abi.ABI
	stakeHubABI                abi.ABI

	// The fields below are for testing only
	fakeDiff bool // Skip difficulty verifications
}

// New creates a Parlia consensus engine.
func New(
	chainConfig *params.ChainConfig,
	db ethdb.Database,
	ethAPI *ethapi.BlockChainAPI,
	genesisHash common.Hash,
) *Parlia {
	// get parlia config
	parliaConfig := chainConfig.Parlia
	log.Info("Parlia", "chainConfig", chainConfig)

	// Set any missing consensus parameters to their defaults
	if parliaConfig != nil && parliaConfig.Epoch == 0 {
		parliaConfig.Epoch = defaultEpochLength
	}

	// Allocate the snapshot caches and create the engine
	recentSnaps, err := lru.NewARC(inMemorySnapshots)
	if err != nil {
		panic(err)
	}
	signatures, err := lru.NewARC(inMemorySignatures)
	if err != nil {
		panic(err)
	}
	recentHeaders, err := lru.NewARC(inMemoryHeaders)
	if err != nil {
		panic(err)
	}
	vABIBeforeLuban, err := abi.JSON(strings.NewReader(validatorSetABIBeforeLuban))
	if err != nil {
		panic(err)
	}
	vABI, err := abi.JSON(strings.NewReader(validatorSetABI))
	if err != nil {
		panic(err)
	}
	sABI, err := abi.JSON(strings.NewReader(slashABI))
	if err != nil {
		panic(err)
	}
	tABI, err := abi.JSON(strings.NewReader(tokenomicsABI))
	if err != nil {
		panic(err)
	}
	stABI, err := abi.JSON(strings.NewReader(stakeABI))
	if err != nil {
		panic(err)
	}
	c := &Parlia{
		chainConfig:                chainConfig,
		config:                     parliaConfig,
		genesisHash:                genesisHash,
		db:                         db,
		ethAPI:                     ethAPI,
		recentSnaps:                recentSnaps,
		recentHeaders:              recentHeaders,
		signatures:                 signatures,
		validatorSetABIBeforeLuban: vABIBeforeLuban,
		validatorSetABI:            vABI,
		slashABI:                   sABI,
		tokenomicsABI:              tABI,
		stakeHubABI:                stABI,
		signer:                     types.LatestSigner(chainConfig),
	}

	return c
}

func (p *Parlia) Period() uint64 {
	return p.config.Period
}

func (p *Parlia) IsSystemTransaction(tx *types.Transaction, header *types.Header) (bool, error) {
	// deploy a contract
	if tx.To() == nil {
		return false, nil
	}
	sender, err := types.Sender(p.signer, tx)
	if err != nil {
		return false, errors.New("UnAuthorized transaction")
	}
	if sender == header.Coinbase && isToSystemContract(*tx.To()) && tx.GasPrice().Cmp(big.NewInt(0)) == 0 {
		return true, nil
	}
	return false, nil
}

// IsTokenomicsDeposit returns true if to address is the tokenomics contract and tx data
// starts with Tokenomics.deposit() method signature
func (p *Parlia) IsTokenomicsDeposit(to *common.Address, data []byte) bool {
	isDestinationTokenomics := bytes.Equal(to.Bytes(), systemcontract.TokenomicsContractAddress.Bytes())
	inputStr := hex.EncodeToString(data)
	isDeposit := false
	if len(inputStr) >= 8 {
		isDeposit = hex.EncodeToString(data)[:8] == "0efe6a8b"
	}
	return isDestinationTokenomics && isDeposit
}

func (p *Parlia) IsSystemContract(to *common.Address) bool {
	if to == nil {
		return false
	}
	return isToSystemContract(*to)
}

// Author implements consensus.Engine, returning the SystemAddress
func (p *Parlia) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

// VerifyHeader checks whether a header conforms to the consensus rules.
func (p *Parlia) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	return p.verifyHeader(chain, header, nil)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers. The
// method returns a quit channel to abort the operations and a results channel to
// retrieve the async verifications (the order is that of the input slice).
func (p *Parlia) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	gopool.Submit(func() {
		for i, header := range headers {
			err := p.verifyHeader(chain, header, headers[:i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	})
	return abort, results
}

// getValidatorBytesFromHeader returns the validators bytes extracted from the header's extra field if exists.
// The validators bytes would be contained only in the epoch block's header, and its each validator bytes length is fixed.
// On luban fork, we introduce vote attestation into the header's extra field, so extra format is different from before.
// Before luban fork: |---Extra Vanity---|---Validators Bytes (or Empty)---|---Extra Seal---|
// After luban fork:  |---Extra Vanity---|---Validators Number and Validators Bytes (or Empty)---|---Vote Attestation (or Empty)---|---Extra Seal---|
// After bohr fork:   |---Extra Vanity---|---Validators Number and Validators Bytes (or Empty)---|---Turn Length (or Empty)---|---Vote Attestation (or Empty)---|---Extra Seal---|
func getValidatorBytesFromHeader(header *types.Header, chainConfig *params.ChainConfig, parliaConfig *params.ParliaConfig) []byte {
	if len(header.Extra) <= extraVanity+extraSeal {
		return nil
	}

	if !chainConfig.IsLuban(header.Number) {
		if header.Number.Uint64()%parliaConfig.Epoch == 0 && (len(header.Extra)-extraSeal-extraVanity)%validatorBytesLengthBeforeLuban != 0 {
			return nil
		}
		return header.Extra[extraVanity : len(header.Extra)-extraSeal]
	}

	if header.Number.Uint64()%parliaConfig.Epoch != 0 {
		return nil
	}
	num := int(header.Extra[extraVanity])
	start := extraVanity + validatorNumberSize
	end := start + num*validatorBytesLength
	extraMinLen := end + extraSeal
	if chainConfig.IsBohr(header.Number, header.Time) {
		extraMinLen += turnLengthSize
	}
	if num == 0 || len(header.Extra) < extraMinLen {
		return nil
	}
	return header.Extra[start:end]
}

// getVoteAttestationFromHeader returns the vote attestation extracted from the header's extra field if exists.
func getVoteAttestationFromHeader(header *types.Header, chainConfig *params.ChainConfig, parliaConfig *params.ParliaConfig) (*types.VoteAttestation, error) {
	if len(header.Extra) <= extraVanity+extraSeal {
		return nil, nil
	}

	if !chainConfig.IsLuban(header.Number) {
		return nil, nil
	}

	var attestationBytes []byte
	if header.Number.Uint64()%parliaConfig.Epoch != 0 {
		attestationBytes = header.Extra[extraVanity : len(header.Extra)-extraSeal]
	} else {
		num := int(header.Extra[extraVanity])
		start := extraVanity + validatorNumberSize + num*validatorBytesLength
		if chainConfig.IsBohr(header.Number, header.Time) {
			start += turnLengthSize
		}
		end := len(header.Extra) - extraSeal
		if end <= start {
			return nil, nil
		}
		attestationBytes = header.Extra[start:end]
	}

	var attestation types.VoteAttestation
	if err := rlp.Decode(bytes.NewReader(attestationBytes), &attestation); err != nil {
		return nil, fmt.Errorf("block %d has vote attestation info, decode err: %s", header.Number.Uint64(), err)
	}
	return &attestation, nil
}

// getParent returns the parent of a given block.
func (p *Parlia) getParent(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) (*types.Header, error) {
	var parent *types.Header
	number := header.Number.Uint64()
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}

	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return nil, consensus.ErrUnknownAncestor
	}
	return parent, nil
}

// verifyVoteAttestation checks whether the vote attestation in the header is valid.
func (p *Parlia) verifyVoteAttestation(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	attestation, err := getVoteAttestationFromHeader(header, p.chainConfig, p.config)
	if err != nil {
		return err
	}
	if attestation == nil {
		return nil
	}
	if attestation.Data == nil {
		return errors.New("invalid attestation, vote data is nil")
	}
	if len(attestation.Extra) > types.MaxAttestationExtraLength {
		return fmt.Errorf("invalid attestation, too large extra length: %d", len(attestation.Extra))
	}

	// Get parent block
	parent, err := p.getParent(chain, header, parents)
	if err != nil {
		return err
	}

	// The target block should be direct parent.
	targetNumber := attestation.Data.TargetNumber
	targetHash := attestation.Data.TargetHash
	if targetNumber != parent.Number.Uint64() || targetHash != parent.Hash() {
		return fmt.Errorf("invalid attestation, target mismatch, expected block: %d, hash: %s; real block: %d, hash: %s",
			parent.Number.Uint64(), parent.Hash(), targetNumber, targetHash)
	}

	// The source block should be the highest justified block.
	sourceNumber := attestation.Data.SourceNumber
	sourceHash := attestation.Data.SourceHash
	headers := []*types.Header{parent}
	if len(parents) > 0 {
		headers = parents
	}
	justifiedBlockNumber, justifiedBlockHash, err := p.GetJustifiedNumberAndHash(chain, headers)
	if err != nil {
		return errors.New("unexpected error when getting the highest justified number and hash")
	}
	if sourceNumber != justifiedBlockNumber || sourceHash != justifiedBlockHash {
		return fmt.Errorf("invalid attestation, source mismatch, expected block: %d, hash: %s; real block: %d, hash: %s",
			justifiedBlockNumber, justifiedBlockHash, sourceNumber, sourceHash)
	}

	// The snapshot should be the targetNumber-1 block's snapshot.
	if len(parents) > 1 {
		parents = parents[:len(parents)-1]
	} else {
		parents = nil
	}
	snap, err := p.snapshot(chain, parent.Number.Uint64()-1, parent.ParentHash, parents)
	if err != nil {
		return err
	}

	// Filter out valid validator from attestation.
	validators := snap.validators()
	validatorsBitSet := bitset.From([]uint64{uint64(attestation.VoteAddressSet)})
	if validatorsBitSet.Count() > uint(len(validators)) {
		return errors.New("invalid attestation, vote number larger than validators number")
	}
	votedAddrs := make([]bls.PublicKey, 0, validatorsBitSet.Count())
	for index, val := range validators {
		if !validatorsBitSet.Test(uint(index)) {
			continue
		}

		voteAddr, err := bls.PublicKeyFromBytes(snap.Validators[val].VoteAddress[:])
		if err != nil {
			return fmt.Errorf("BLS public key converts failed: %v", err)
		}
		votedAddrs = append(votedAddrs, voteAddr)
	}

	// The valid voted validators should be no less than 2/3 validators.
	if len(votedAddrs) < cmath.CeilDiv(len(snap.Validators)*2, 3) {
		return errors.New("invalid attestation, not enough validators voted")
	}

	// Verify the aggregated signature.
	aggSig, err := bls.SignatureFromBytes(attestation.AggSignature[:])
	if err != nil {
		return fmt.Errorf("BLS signature converts failed: %v", err)
	}
	if !aggSig.FastAggregateVerify(votedAddrs, attestation.Data.Hash()) {
		return errors.New("invalid attestation, signature verify failed")
	}

	return nil
}

// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
func (p *Parlia) verifyHeader(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}

	// Don't waste time checking blocks from the future
	if header.Time > uint64(time.Now().Unix()+time.Second.Milliseconds()/1000) {
		return consensus.ErrFutureBlock
	}
	// Check that the extra-data contains the vanity, validators and signature.
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}

	// check extra data
	number := header.Number.Uint64()
	isEpoch := number%p.config.Epoch == 0

	// Ensure that the extra-data contains a signer list on checkpoint, but none otherwise
	signersBytes := getValidatorBytesFromHeader(header, p.chainConfig, p.config)
	if !isEpoch && len(signersBytes) != 0 {
		return errExtraValidators
	}
	if isEpoch && len(signersBytes) == 0 {
		return errInvalidSpanValidators
	}

	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}
	// Ensure that the block doesn't contain any uncles which are meaningless in PoA
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}
	// Ensure that the block's difficulty is meaningful (may not be correct at this point)
	if number > 0 {
		if header.Difficulty == nil {
			return errInvalidDifficulty
		}
	}

	parent, err := p.getParent(chain, header, parents)
	if err != nil {
		return err
	}

	// Verify the block's gas usage and (if applicable) verify the base fee.
	if !chain.Config().IsLondon(header.Number) {
		// Verify BaseFee not present before EIP-1559 fork.
		if header.BaseFee != nil {
			return fmt.Errorf("invalid baseFee before fork: have %d, expected 'nil'", header.BaseFee)
		}
	} else if err := eip1559.VerifyEIP1559Header(chain.Config(), parent, header); err != nil {
		// Verify the header's EIP-1559 attributes.
		return err
	}

	cancun := chain.Config().IsCancun(header.Number, header.Time)
	if !cancun {
		switch {
		case header.ExcessBlobGas != nil:
			return fmt.Errorf("invalid excessBlobGas: have %d, expected nil", header.ExcessBlobGas)
		case header.BlobGasUsed != nil:
			return fmt.Errorf("invalid blobGasUsed: have %d, expected nil", header.BlobGasUsed)
		case header.WithdrawalsHash != nil:
			return fmt.Errorf("invalid WithdrawalsHash, have %#x, expected nil", header.WithdrawalsHash)
		}
	} else {
		switch {
		case !header.EmptyWithdrawalsHash():
			return errors.New("header has wrong WithdrawalsHash")
		}
		if err := eip4844.VerifyEIP4844Header(parent, header); err != nil {
			return err
		}
	}

	bohr := chain.Config().IsBohr(header.Number, header.Time)
	if !bohr {
		if header.ParentBeaconRoot != nil {
			return fmt.Errorf("invalid parentBeaconRoot, have %#x, expected nil", header.ParentBeaconRoot)
		}
	} else {
		if header.ParentBeaconRoot == nil || *header.ParentBeaconRoot != (common.Hash{}) {
			return fmt.Errorf("invalid parentBeaconRoot, have %#x, expected zero hash", header.ParentBeaconRoot)
		}
	}

	// All basic checks passed, verify cascading fields
	return p.verifyCascadingFields(chain, header, parents)
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers. The caller may optionally pass
// in a batch of parents (ascending order) to avoid looking those up from the
// database. This is useful for concurrently verifying a batch of new headers.
func (p *Parlia) verifyCascadingFields(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}

	parent, err := p.getParent(chain, header, parents)
	if err != nil {
		return err
	}

	snap, err := p.snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}

	err = p.blockTimeVerifyForRamanujanFork(snap, header, parent)
	if err != nil {
		return err
	}

	// Verify that the gas limit is <= 2^63-1
	capacity := uint64(0x7fffffffffffffff)
	if header.GasLimit > capacity {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, capacity)
	}
	// Verify that the gasUsed is <= gasLimit
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}

	// Verify that the gas limit remains within allowed bounds
	diff := int64(parent.GasLimit) - int64(header.GasLimit)
	if diff < 0 {
		diff *= -1
	}
	limit := parent.GasLimit / params.GasLimitBoundDivisor

	if uint64(diff) >= limit || header.GasLimit < params.MinGasLimit {
		return fmt.Errorf("invalid gas limit: have %d, want %d += %d", header.GasLimit, parent.GasLimit, limit-1)
	}

	// Verify vote attestation for fast finality.
	if err := p.verifyVoteAttestation(chain, header, parents); err != nil {
		log.Warn("Verify vote attestation failed", "error", err, "hash", header.Hash(), "number", header.Number,
			"parent", header.ParentHash, "coinbase", header.Coinbase, "extra", common.Bytes2Hex(header.Extra))
		verifyVoteAttestationErrorCounter.Inc(1)
		if chain.Config().IsPlato(header.Number) {
			return err
		}
	}

	// All basic checks passed, verify the seal and return
	return p.verifySeal(chain, header, parents)
}

// snapshot retrieves the authorization snapshot at a given point in time.
// !!! be careful
// the block with `number` and `hash` is just the last element of `parents`,
// unlike other interfaces such as verifyCascadingFields, `parents` are real parents
func (p *Parlia) snapshot(chain consensus.ChainHeaderReader, number uint64, hash common.Hash, parents []*types.Header) (*Snapshot, error) {
	// Search for a snapshot in memory or on disk for checkpoints
	var (
		headers []*types.Header
		snap    *Snapshot
	)

	for snap == nil {
		// If an in-memory snapshot was found, use that
		if s, ok := p.recentSnaps.Get(hash); ok {
			snap = s.(*Snapshot)
			break
		}

		// If an on-disk checkpoint snapshot can be found, use that
		if number%checkpointInterval == 0 {
			if s, err := loadSnapshot(p.config, p.signatures, p.db, hash, p.ethAPI); err == nil {
				log.Trace("Loaded snapshot from disk", "number", number, "hash", hash)
				snap = s
				break
			}
		}

		// If we're at the genesis, snapshot the initial state. Alternatively if we have
		// piled up more headers than allowed to be reorged (chain reinit from a freezer),
		// consider the checkpoint trusted and snapshot it.
		// An offset `p.config.Epoch - 1` can ensure getting the right validators.
		if number == 0 || ((number+1)%p.config.Epoch == 0 && (len(headers) > int(params.FullImmutabilityThreshold))) {
			var (
				checkpoint *types.Header
				blockHash  common.Hash
			)
			if number == 0 {
				checkpoint = chain.GetHeaderByNumber(0)
				if checkpoint != nil {
					blockHash = checkpoint.Hash()
				}
			} else {
				checkpoint = chain.GetHeaderByNumber(number + 1 - p.config.Epoch)
				blockHeader := chain.GetHeaderByNumber(number)
				if blockHeader != nil {
					blockHash = blockHeader.Hash()
				}
			}
			if checkpoint != nil && blockHash != (common.Hash{}) {
				// get validators from headers
				validators, voteAddrs, err := parseValidators(checkpoint, p.chainConfig, p.config)
				if err != nil {
					return nil, err
				}

				// new snapshot
				snap = newSnapshot(p.config, p.signatures, number, blockHash, validators, voteAddrs, p.ethAPI)

				// get turnLength from headers and use that for new turnLength
				turnLength, err := parseTurnLength(checkpoint, p.chainConfig, p.config)
				if err != nil {
					return nil, err
				}
				if turnLength != nil {
					snap.TurnLength = *turnLength
				}

				// snap.Recents is currently empty, which affects the following:
				// a. The function SignRecently - This is acceptable since an empty snap.Recents results in a more lenient check.
				// b. The function blockTimeVerifyForRamanujanFork - This is also acceptable as it won't be invoked during `snap.apply`.
				// c. This may cause a mismatch in the slash systemtx, but the transaction list is not verified during `snap.apply`.

				// snap.Attestation is nil, but Snapshot.updateAttestation will handle it correctly.
				if err := snap.store(p.db); err != nil {
					return nil, err
				}
				log.Info("Stored checkpoint snapshot to disk", "number", number, "hash", blockHash)
				break
			}
		}

		// No snapshot for this header, gather the header and move backward
		var header *types.Header
		if len(parents) > 0 {
			// If we have explicit parents, pick from there (enforced)
			header = parents[len(parents)-1]
			if header.Hash() != hash || header.Number.Uint64() != number {
				return nil, consensus.ErrUnknownAncestor
			}
			parents = parents[:len(parents)-1]
		} else {
			// No explicit parents (or no more left), reach out to the database
			header = chain.GetHeader(hash, number)
			if header == nil {
				return nil, consensus.ErrUnknownAncestor
			}
		}
		headers = append(headers, header)
		number, hash = number-1, header.ParentHash
	}

	// check if snapshot is nil
	if snap == nil {
		return nil, fmt.Errorf("unknown error while retrieving snapshot at block number %v", number)
	}

	// Previous snapshot found, apply any pending headers on top of it
	for i := 0; i < len(headers)/2; i++ {
		headers[i], headers[len(headers)-1-i] = headers[len(headers)-1-i], headers[i]
	}

	snap, err := snap.apply(headers, chain, parents, p.chainConfig)
	if err != nil {
		return nil, err
	}
	p.recentSnaps.Add(snap.Hash, snap)

	// If we've generated a new checkpoint snapshot, save to disk
	if snap.Number%checkpointInterval == 0 && len(headers) > 0 {
		if err = snap.store(p.db); err != nil {
			return nil, err
		}
		log.Trace("Stored snapshot to disk", "number", snap.Number, "hash", snap.Hash)
	}

	var validators []string
	for v := range snap.Validators {
		validators = append(validators, v.Hex())
	}
	log.Trace("loaded snapshot", "number", snap.Number, "hash", snap.Hash, "validators", strings.Join(validators, ","), "len", len(snap.Validators))

	return snap, err
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (p *Parlia) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errors.New("uncles not allowed")
	}
	return nil
}

// VerifySeal implements consensus.Engine, checking whether the signature contained
// in the header satisfies the consensus protocol requirements.
func (p *Parlia) VerifySeal(chain consensus.ChainReader, header *types.Header) error {
	return p.verifySeal(chain, header, nil)
}

// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements. The method accepts an optional list of parent
// headers that aren't yet part of the local blockchain to generate the snapshots
// from.
func (p *Parlia) verifySeal(chain consensus.ChainHeaderReader, header *types.Header, parents []*types.Header) error {
	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}
	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := p.snapshot(chain, number-1, header.ParentHash, parents)
	if err != nil {
		return err
	}

	// Resolve the authorization key and check against validators
	signer, err := ecrecover(header, p.signatures, p.chainConfig.ChainID)
	if err != nil {
		return err
	}

	if signer != header.Coinbase {
		return errCoinBaseMisMatch
	}

	// check for double sign & add to cache
	key := proposalKey(*header)
	preHash, ok := p.recentHeaders.Get(key)
	if ok && preHash != header.Hash() {
		doubleSignCounter.Inc(1)
		log.Warn("DoubleSign detected", " block", header.Number, " miner", header.Coinbase,
			"hash1", preHash.(common.Hash), "hash2", header.Hash())
	} else {
		p.recentHeaders.Add(key, header.Hash())
	}

	if _, ok := snap.Validators[signer]; !ok {
		return errUnauthorizedValidator(signer.String())
	}

	if snap.SignRecently(signer) {
		return errRecentlySigned
	}

	// Ensure that the difficulty corresponds to the turn-ness of the signer
	if !p.fakeDiff {
		inturn := snap.inturn(signer)
		if inturn && header.Difficulty.Cmp(diffInTurn) != 0 {
			return errWrongDifficulty
		}
		if !inturn && header.Difficulty.Cmp(diffNoTurn) != 0 {
			return errWrongDifficulty
		}
	}

	return nil
}

func (p *Parlia) prepareValidators(header *types.Header) error {
	if header.Number.Uint64()%p.config.Epoch != 0 {
		return nil
	}

	newValidators, voteAddressMap, err := p.getCurrentValidators(header.ParentHash, new(big.Int).Sub(header.Number, big.NewInt(1)))
	if err != nil {
		return err
	}
	// sort validator by address
	sort.Sort(validatorsAscending(newValidators))
	if !p.chainConfig.IsLuban(header.Number) {
		for _, validator := range newValidators {
			header.Extra = append(header.Extra, validator.Bytes()...)
		}
	} else {
		header.Extra = append(header.Extra, byte(len(newValidators)))
		if p.chainConfig.IsOnLuban(header.Number) {
			voteAddressMap = make(map[common.Address]*types.BLSPublicKey, len(newValidators))
			var zeroBlsKey types.BLSPublicKey
			for _, validator := range newValidators {
				voteAddressMap[validator] = &zeroBlsKey
			}
		}
		for _, validator := range newValidators {
			header.Extra = append(header.Extra, validator.Bytes()...)
			header.Extra = append(header.Extra, voteAddressMap[validator].Bytes()...)
		}
	}
	return nil
}

func (p *Parlia) prepareTurnLength(chain consensus.ChainHeaderReader, header *types.Header) error {
	if header.Number.Uint64()%p.config.Epoch != 0 ||
		!p.chainConfig.IsBohr(header.Number, header.Time) {
		return nil
	}

	turnLength, err := p.getTurnLength(chain, header)
	if err != nil {
		return err
	}

	if turnLength != nil {
		header.Extra = append(header.Extra, *turnLength)
	}

	return nil
}

func (p *Parlia) assembleVoteAttestation(chain consensus.ChainHeaderReader, header *types.Header) error {
	if !p.chainConfig.IsLuban(header.Number) || header.Number.Uint64() < 2 {
		return nil
	}

	if p.VotePool == nil {
		return nil
	}

	// Fetch direct parent's votes
	parent := chain.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		return errors.New("parent not found")
	}
	snap, err := p.snapshot(chain, parent.Number.Uint64()-1, parent.ParentHash, nil)
	if err != nil {
		return err
	}
	votes := p.VotePool.FetchVoteByBlockHash(parent.Hash())
	if len(votes) < cmath.CeilDiv(len(snap.Validators)*2, 3) {
		return nil
	}

	// Prepare vote attestation
	// Prepare vote data
	justifiedBlockNumber, justifiedBlockHash, err := p.GetJustifiedNumberAndHash(chain, []*types.Header{parent})
	if err != nil {
		return errors.New("unexpected error when getting the highest justified number and hash")
	}
	attestation := &types.VoteAttestation{
		Data: &types.VoteData{
			SourceNumber: justifiedBlockNumber,
			SourceHash:   justifiedBlockHash,
			TargetNumber: parent.Number.Uint64(),
			TargetHash:   parent.Hash(),
		},
	}
	// Check vote data from votes
	for _, vote := range votes {
		if vote.Data.Hash() != attestation.Data.Hash() {
			return fmt.Errorf("vote check error, expected: %v, real: %v", attestation.Data, vote)
		}
	}
	// Prepare aggregated vote signature
	voteAddrSet := make(map[types.BLSPublicKey]struct{}, len(votes))
	signatures := make([][]byte, 0, len(votes))
	for _, vote := range votes {
		voteAddrSet[vote.VoteAddress] = struct{}{}
		signatures = append(signatures, vote.Signature[:])
	}
	sigs, err := bls.MultipleSignaturesFromBytes(signatures)
	if err != nil {
		return err
	}
	copy(attestation.AggSignature[:], bls.AggregateSignatures(sigs).Marshal())
	// Prepare vote address bitset.
	for _, valInfo := range snap.Validators {
		if _, ok := voteAddrSet[valInfo.VoteAddress]; ok {
			attestation.VoteAddressSet |= 1 << (valInfo.Index - 1) // Index is offset by 1
		}
	}
	validatorsBitSet := bitset.From([]uint64{uint64(attestation.VoteAddressSet)})
	if validatorsBitSet.Count() < uint(len(signatures)) {
		log.Warn(fmt.Sprintf("assembleVoteAttestation, check VoteAddress Set failed, expected:%d, real:%d", len(signatures), validatorsBitSet.Count()))
		return errors.New("invalid attestation, check VoteAddress Set failed")
	}

	// Append attestation to header extra field.
	buf := new(bytes.Buffer)
	err = rlp.Encode(buf, attestation)
	if err != nil {
		return err
	}

	// Insert vote attestation into header extra ahead extra seal.
	extraSealStart := len(header.Extra) - extraSeal
	extraSealBytes := header.Extra[extraSealStart:]
	header.Extra = append(header.Extra[0:extraSealStart], buf.Bytes()...)
	header.Extra = append(header.Extra, extraSealBytes...)

	return nil
}

// NextInTurnValidator return the next in-turn validator for header
func (p *Parlia) NextInTurnValidator(chain consensus.ChainHeaderReader, header *types.Header) (common.Address, error) {
	snap, err := p.snapshot(chain, header.Number.Uint64(), header.Hash(), nil)
	if err != nil {
		return common.Address{}, err
	}

	return snap.inturnValidator(), nil
}

// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
func (p *Parlia) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	header.Coinbase = p.val
	header.Nonce = types.BlockNonce{}

	number := header.Number.Uint64()
	snap, err := p.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}

	// Set the correct difficulty
	header.Difficulty = CalcDifficulty(snap, p.val)
	if header.Difficulty.Cmp(diffInTurn) != 0 && header.Number.Uint64() == 1 {
		return fmt.Errorf("not your turn for block producing")
	}

	// Ensure the extra data has all it's components
	if len(header.Extra) < extraVanity-nextForkHashSize {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-nextForkHashSize-len(header.Extra))...)
	}

	// Ensure the timestamp has the correct delay
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Time = p.blockTimeForRamanujanFork(snap, header, parent)
	if header.Time < uint64(time.Now().Unix()) {
		header.Time = uint64(time.Now().Unix())
	}

	header.Extra = header.Extra[:extraVanity-nextForkHashSize]
	nextForkHash := forkid.NextForkHash(p.chainConfig, p.genesisHash, chain.GenesisHeader().Time, number, header.Time)
	header.Extra = append(header.Extra, nextForkHash[:]...)

	if err := p.prepareValidators(header); err != nil {
		return err
	}

	if err := p.prepareTurnLength(chain, header); err != nil {
		return err
	}
	// add extra seal space
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	// Mix digest is reserved for now, set to empty
	header.MixDigest = common.Hash{}
	return nil
}

func (p *Parlia) verifyValidators(header *types.Header) error {
	if header.Number.Uint64()%p.config.Epoch != 0 {
		return nil
	}

	newValidators, voteAddressMap, err := p.getCurrentValidators(header.ParentHash, new(big.Int).Sub(header.Number, big.NewInt(1)))
	if err != nil {
		return err
	}
	// sort validator by address
	sort.Sort(validatorsAscending(newValidators))
	var validatorsBytes []byte
	validatorsNumber := len(newValidators)
	if !p.chainConfig.IsLuban(header.Number) {
		validatorsBytes = make([]byte, validatorsNumber*validatorBytesLengthBeforeLuban)
		for i, validator := range newValidators {
			copy(validatorsBytes[i*validatorBytesLengthBeforeLuban:], validator.Bytes())
		}
	} else {
		if uint8(validatorsNumber) != header.Extra[extraVanity] {
			return errMismatchingEpochValidators
		}
		validatorsBytes = make([]byte, validatorsNumber*validatorBytesLength)
		if p.chainConfig.IsOnLuban(header.Number) {
			voteAddressMap = make(map[common.Address]*types.BLSPublicKey, len(newValidators))
			var zeroBlsKey types.BLSPublicKey
			for _, validator := range newValidators {
				voteAddressMap[validator] = &zeroBlsKey
			}
		}
		for i, validator := range newValidators {
			copy(validatorsBytes[i*validatorBytesLength:], validator.Bytes())
			copy(validatorsBytes[i*validatorBytesLength+common.AddressLength:], voteAddressMap[validator].Bytes())
		}
	}
	if !bytes.Equal(getValidatorBytesFromHeader(header, p.chainConfig, p.config), validatorsBytes) {
		return errMismatchingEpochValidators
	}
	return nil
}

func (p *Parlia) verifyTurnLength(chain consensus.ChainHeaderReader, header *types.Header) error {
	if header.Number.Uint64()%p.config.Epoch != 0 ||
		!p.chainConfig.IsBohr(header.Number, header.Time) {
		return nil
	}

	turnLengthFromHeader, err := parseTurnLength(header, p.chainConfig, p.config)
	if err != nil {
		return err
	}
	if turnLengthFromHeader != nil {
		turnLength, err := p.getTurnLength(chain, header)
		if err != nil {
			return err
		}
		if turnLength != nil && *turnLength == *turnLengthFromHeader {
			log.Debug("verifyTurnLength", "turnLength", *turnLength)
			return nil
		}
	}

	return errMismatchingEpochTurnLength
}

func (p *Parlia) distributeFinalityReward(chain consensus.ChainHeaderReader, state *state.StateDB, header *types.Header,
	cx core.ChainContext, txs *[]*types.Transaction, receipts *[]*types.Receipt, systemTxs *[]*types.Transaction,
	usedGas *uint64, mining bool) error {
	currentHeight := header.Number.Uint64()
	epoch := p.config.Epoch
	chainConfig := chain.Config()
	if currentHeight%epoch != 0 {
		return nil
	}

	head := header
	accumulatedWeights := make(map[common.Address]uint64)
	for height := currentHeight - 1; height+epoch >= currentHeight && height >= 1; height-- {
		head = chain.GetHeaderByHash(head.ParentHash)
		if head == nil {
			return fmt.Errorf("header is nil at height %d", height)
		}
		voteAttestation, err := getVoteAttestationFromHeader(head, chainConfig, p.config)
		if err != nil {
			return err
		}
		if voteAttestation == nil {
			continue
		}
		justifiedBlock := chain.GetHeaderByHash(voteAttestation.Data.TargetHash)
		if justifiedBlock == nil {
			log.Warn("justifiedBlock is nil at height %d", voteAttestation.Data.TargetNumber)
			continue
		}

		snap, err := p.snapshot(chain, justifiedBlock.Number.Uint64()-1, justifiedBlock.ParentHash, nil)
		if err != nil {
			return err
		}
		validators := snap.validators()
		validatorsBitSet := bitset.From([]uint64{uint64(voteAttestation.VoteAddressSet)})
		if validatorsBitSet.Count() > uint(len(validators)) {
			log.Error("invalid attestation, vote number larger than validators number")
			continue
		}
		validVoteCount := 0
		for index, val := range validators {
			if validatorsBitSet.Test(uint(index)) {
				accumulatedWeights[val] += 1
				validVoteCount += 1
			}
		}
		quorum := cmath.CeilDiv(len(snap.Validators)*2, 3)
		if validVoteCount > quorum {
			accumulatedWeights[head.Coinbase] += uint64((validVoteCount - quorum) * collectAdditionalVotesRewardRatio / 100)
		}
	}

	validators := make([]common.Address, 0, len(accumulatedWeights))
	weights := make([]*big.Int, 0, len(accumulatedWeights))
	for val := range accumulatedWeights {
		validators = append(validators, val)
	}
	sort.Sort(validatorsAscending(validators))
	for _, val := range validators {
		weights = append(weights, big.NewInt(int64(accumulatedWeights[val])))
	}

	// generate system transaction
	method := "distributeFinalityReward"
	data, err := p.validatorSetABI.Pack(method, validators, weights)
	if err != nil {
		log.Error("Unable to pack tx for distributeFinalityReward", "error", err)
		return err
	}
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.ValidatorContract), data, common.Big0)
	return p.applyTransaction(msg, state, header, cx, txs, receipts, systemTxs, usedGas, mining)
}

// Finalize implements consensus.Engine, ensuring no uncles are set, nor block
// rewards given.
func (p *Parlia) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs *[]*types.Transaction,
	uncles []*types.Header, _ []*types.Withdrawal, receipts *[]*types.Receipt, systemTxs *[]*types.Transaction, usedGas *uint64) error {
	// warn if not in majority fork
	number := header.Number.Uint64()
	snap, err := p.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}
	nextForkHash := forkid.NextForkHash(p.chainConfig, p.genesisHash, chain.GenesisHeader().Time, number, header.Time)
	if !snap.isMajorityFork(hex.EncodeToString(nextForkHash[:])) {
		log.Debug("there is a possible fork, and your client is not the majority. Please check...", "nextForkHash", hex.EncodeToString(nextForkHash[:]))
	}
	// If the block is an epoch end block, verify the validator list
	// The verification can only be done when the state is ready, it can't be done in VerifyHeader.
	if err := p.verifyValidators(header); err != nil {
		return err
	}

	if err := p.verifyTurnLength(chain, header); err != nil {
		return err
	}

	cx := chainContext{Chain: chain, parlia: p}

	parent := chain.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		return errors.New("parent not found")
	}

	if p.chainConfig.IsFeynman(header.Number, header.Time) {
		systemcontracts.UpgradeBuildInSystemContract(p.chainConfig, header.Number, parent.Time, header.Time, state)
	}

	if p.chainConfig.IsOnFeynman(header.Number, parent.Time, header.Time) {
		err := p.initializeFeynmanContract(state, header, cx, txs, receipts, systemTxs, usedGas, false)
		if err != nil {
			log.Error("init feynman contract failed", "error", err)
		}
	}

	// No block rewards in PoA, so the state remains as is and uncles are dropped
	if header.Number.Cmp(common.Big1) == 0 {
		err := p.initContract(state, header, cx, txs, receipts, systemTxs, usedGas, false)
		if err != nil {
			log.Error("init contract failed", "error", err)
			return err
		}
	}
	if header.Difficulty.Cmp(diffInTurn) != 0 {
		spoiledVal := snap.inturnValidator()
		signedRecently := false
		if p.chainConfig.IsPlato(header.Number) {
			signedRecently = snap.SignRecently(spoiledVal)
		} else {
			for _, recent := range snap.Recents {
				if recent == spoiledVal {
					signedRecently = true
					break
				}
			}
		}

		if !signedRecently {
			log.Trace("slash validator", "block hash", header.Hash(), "address", spoiledVal)
			err = p.slash(spoiledVal, state, header, cx, txs, receipts, systemTxs, usedGas, false)
			if err != nil {
				// it is possible that slash validator failed because of the slash channel is disabled.
				log.Error("slash validator failed", "block hash", header.Hash(), "address", spoiledVal)
			}
		}
	}
	val := header.Coinbase
	err = p.distributeIncoming(val, state, header, cx, txs, receipts, systemTxs, usedGas, false)
	if err != nil {
		return err
	}

	if p.chainConfig.IsPlato(header.Number) {
		if err := p.distributeFinalityReward(chain, state, header, cx, txs, receipts, systemTxs, usedGas, false); err != nil {
			return err
		}
	}

	// update validators every day
	if p.chainConfig.IsFeynman(header.Number, header.Time) && isBreatheBlock(parent.Time, header.Time) {
		// we should avoid update validators in the Feynman upgrade block
		if !p.chainConfig.IsOnFeynman(header.Number, parent.Time, header.Time) {
			if err := p.updateValidatorSetV2(state, header, cx, txs, receipts, systemTxs, usedGas, false); err != nil {
				return err
			}
		}
	}

	if len(*systemTxs) > 0 {
		return errors.New("the length of systemTxs do not match")
	}
	return nil
}

// FinalizeAndAssemble implements consensus.Engine, ensuring no uncles are set,
// nor block rewards given, and returns the final block.
func (p *Parlia) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB,
	txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt, _ []*types.Withdrawal) (*types.Block, []*types.Receipt, error) {
	// No block rewards in PoA, so the state remains as is and uncles are dropped
	cx := chainContext{Chain: chain, parlia: p}
	if txs == nil {
		txs = make([]*types.Transaction, 0)
	}
	if receipts == nil {
		receipts = make([]*types.Receipt, 0)
	}

	parent := chain.GetHeaderByHash(header.ParentHash)
	if parent == nil {
		return nil, nil, errors.New("parent not found")
	}

	if p.chainConfig.IsFeynman(header.Number, header.Time) {
		systemcontracts.UpgradeBuildInSystemContract(p.chainConfig, header.Number, parent.Time, header.Time, state)
	}

	if p.chainConfig.IsOnFeynman(header.Number, parent.Time, header.Time) {
		err := p.initializeFeynmanContract(state, header, cx, &txs, &receipts, nil, &header.GasUsed, true)
		if err != nil {
			log.Error("init feynman contract failed", "error", err)
		}
	}

	if header.Number.Cmp(common.Big1) == 0 {
		err := p.initContract(state, header, cx, &txs, &receipts, nil, &header.GasUsed, true)
		if err != nil {
			log.Error("init contract failed", "error", err)
			return nil, nil, err
		}
	}
	if header.Difficulty.Cmp(diffInTurn) != 0 {
		number := header.Number.Uint64()
		snap, err := p.snapshot(chain, number-1, header.ParentHash, nil)
		if err != nil {
			return nil, nil, err
		}
		spoiledVal := snap.inturnValidator()
		signedRecently := false
		if p.chainConfig.IsPlato(header.Number) {
			signedRecently = snap.SignRecently(spoiledVal)
		} else {
			for _, recent := range snap.Recents {
				if recent == spoiledVal {
					signedRecently = true
					break
				}
			}
		}
		if !signedRecently {
			err = p.slash(spoiledVal, state, header, cx, &txs, &receipts, nil, &header.GasUsed, true)
			if err != nil {
				// it is possible that slash validator failed because of the slash channel is disabled.
				log.Error("slash validator failed", "block hash", header.Hash(), "address", spoiledVal)
			}
		}
	}

	err := p.distributeIncoming(p.val, state, header, cx, &txs, &receipts, nil, &header.GasUsed, true)
	if err != nil {
		return nil, nil, err
	}

	if p.chainConfig.IsPlato(header.Number) {
		if err := p.distributeFinalityReward(chain, state, header, cx, &txs, &receipts, nil, &header.GasUsed, true); err != nil {
			return nil, nil, err
		}
	}

	// update validators every day
	if p.chainConfig.IsFeynman(header.Number, header.Time) && isBreatheBlock(parent.Time, header.Time) {
		// we should avoid update validators in the Feynman upgrade block
		if !p.chainConfig.IsOnFeynman(header.Number, parent.Time, header.Time) {
			if err := p.updateValidatorSetV2(state, header, cx, &txs, &receipts, nil, &header.GasUsed, true); err != nil {
				return nil, nil, err
			}
		}
	}

	// should not happen. Once happen, stop the node is better than broadcast the block
	if header.GasLimit < header.GasUsed {
		return nil, nil, errors.New("gas consumption of system txs exceed the gas limit")
	}
	header.UncleHash = types.CalcUncleHash(nil)
	var blk *types.Block
	var rootHash common.Hash
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		rootHash = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
		wg.Done()
	}()
	go func() {
		blk = types.NewBlock(header, txs, nil, receipts, trie.NewStackTrie(nil))
		wg.Done()
	}()
	wg.Wait()
	blk.SetRoot(rootHash)
	// Assemble and return the final block for sealing
	return blk, receipts, nil
}

func (p *Parlia) IsActiveValidatorAt(chain consensus.ChainHeaderReader, header *types.Header, checkVoteKeyFn func(bLSPublicKey *types.BLSPublicKey) bool) bool {
	number := header.Number.Uint64()
	snap, err := p.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		log.Error("failed to get the snapshot from consensus", "error", err)
		return false
	}
	validators := snap.Validators
	validatorInfo, ok := validators[p.val]

	return ok && (checkVoteKeyFn == nil || (validatorInfo != nil && checkVoteKeyFn(&validatorInfo.VoteAddress)))
}

// VerifyVote will verify: 1. If the vote comes from valid validators 2. If the vote's sourceNumber and sourceHash are correct
func (p *Parlia) VerifyVote(chain consensus.ChainHeaderReader, vote *types.VoteEnvelope) error {
	targetNumber := vote.Data.TargetNumber
	targetHash := vote.Data.TargetHash
	header := chain.GetVerifiedBlockByHash(targetHash)
	if header == nil {
		log.Warn("BlockHeader at current voteBlockNumber is nil", "targetNumber", targetNumber, "targetHash", targetHash)
		return errors.New("BlockHeader at current voteBlockNumber is nil")
	}
	if header.Number.Uint64() != targetNumber {
		log.Warn("unexpected target number", "expect", header.Number.Uint64(), "real", targetNumber)
		return errors.New("target number mismatch")
	}

	justifiedBlockNumber, justifiedBlockHash, err := p.GetJustifiedNumberAndHash(chain, []*types.Header{header})
	if err != nil {
		log.Error("failed to get the highest justified number and hash", "headerNumber", header.Number, "headerHash", header.Hash())
		return errors.New("unexpected error when getting the highest justified number and hash")
	}
	if vote.Data.SourceNumber != justifiedBlockNumber || vote.Data.SourceHash != justifiedBlockHash {
		return errors.New("vote source block mismatch")
	}

	number := header.Number.Uint64()
	snap, err := p.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		log.Error("failed to get the snapshot from consensus", "error", err)
		return errors.New("failed to get the snapshot from consensus")
	}

	validators := snap.Validators
	voteAddress := vote.VoteAddress
	for addr, validator := range validators {
		if validator.VoteAddress == voteAddress {
			if addr == p.val {
				validVotesfromSelfCounter.Inc(1)
			}
			metrics.GetOrRegisterCounter(fmt.Sprintf("parlia/VerifyVote/%s", addr.String()), nil).Inc(1)
			return nil
		}
	}

	return errors.New("vote verification failed")
}

// Authorize injects a private key into the consensus engine to mint new blocks
// with.
func (p *Parlia) Authorize(val common.Address, signFn SignerFn, signTxFn SignerTxFn) {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.val = val
	p.signFn = signFn
	p.signTxFn = signTxFn
}

// Argument leftOver is the time reserved for block finalize(calculate root, distribute income...)
func (p *Parlia) Delay(chain consensus.ChainReader, header *types.Header, leftOver *time.Duration) *time.Duration {
	number := header.Number.Uint64()
	snap, err := p.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return nil
	}
	delay := p.delayForRamanujanFork(snap, header)

	if *leftOver >= time.Duration(p.config.Period)*time.Second {
		// ignore invalid leftOver
		log.Error("Delay invalid argument", "leftOver", leftOver.String(), "Period", p.config.Period)
	} else if *leftOver >= delay {
		delay = time.Duration(0)
		return &delay
	} else {
		delay = delay - *leftOver
	}

	// The blocking time should be no more than half of period when snap.TurnLength == 1
	timeForMining := time.Duration(p.config.Period) * time.Second / 2
	if !snap.lastBlockInOneTurn(header.Number.Uint64()) {
		timeForMining = time.Duration(p.config.Period) * time.Second * 2 / 3
	}
	if delay > timeForMining {
		delay = timeForMining
	}
	return &delay
}

// Seal implements consensus.Engine, attempting to create a sealed block using
// the local signing credentials.
func (p *Parlia) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	header := block.Header()

	// Sealing the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}
	// For 0-period chains, refuse to seal empty blocks (no reward but would spin sealing)
	if p.config.Period == 0 && len(block.Transactions()) == 0 {
		log.Info("Sealing paused, waiting for transactions")
		return nil
	}
	// Don't hold the val fields for the entire sealing procedure
	p.lock.RLock()
	val, signFn := p.val, p.signFn
	p.lock.RUnlock()

	snap, err := p.snapshot(chain, number-1, header.ParentHash, nil)
	if err != nil {
		return err
	}

	// Bail out if we're unauthorized to sign a block
	if _, authorized := snap.Validators[val]; !authorized {
		return errUnauthorizedValidator(val.String())
	}

	// If we're amongst the recent signers, wait for the next block
	if snap.SignRecently(val) {
		log.Info("Signed recently, must wait for others")
		return nil
	}

	// Sweet, the protocol permits us to sign the block, wait for our time
	delay := p.delayForRamanujanFork(snap, header)

	log.Info("Sealing block with", "number", number, "delay", delay, "headerDifficulty", header.Difficulty, "val", val.Hex())

	// Wait until sealing is terminated or delay timeout.
	log.Info("Waiting for slot to sign and propagate", "delay", common.PrettyDuration(delay))
	go func() {
		select {
		case <-stop:
			return
		case <-time.After(delay):
		}

		err := p.assembleVoteAttestation(chain, header)
		if err != nil {
			/* If the vote attestation can't be assembled successfully, the blockchain won't get
			   fast finalized, but it can be tolerated, so just report this error here. */
			log.Error("Assemble vote attestation failed when sealing", "err", err)
		}

		// Sign all the things!
		sig, err := signFn(accounts.Account{Address: val}, accounts.MimetypeParlia, ParliaRLP(header, p.chainConfig.ChainID))
		if err != nil {
			log.Error("Sign for the block header failed when sealing", "err", err)
			return
		}
		copy(header.Extra[len(header.Extra)-extraSeal:], sig)

		if p.shouldWaitForCurrentBlockProcess(chain, header, snap) {
			log.Info("Waiting for received in turn block to process")
			select {
			case <-stop:
				log.Info("Received block process finished, abort block seal")
				return
			case <-time.After(time.Duration(processBackOffTime) * time.Second):
				if chain.CurrentHeader().Number.Uint64() >= header.Number.Uint64() {
					log.Info("Process backoff time exhausted, and current header has updated to abort this seal")
					return
				}
				log.Info("Process backoff time exhausted, start to seal block")
			}
		}

		select {
		case results <- block.WithSeal(header):
		default:
			log.Warn("Sealing result is not read by miner", "sealhash", types.SealHash(header, p.chainConfig.ChainID))
		}
	}()

	return nil
}

func (p *Parlia) shouldWaitForCurrentBlockProcess(chain consensus.ChainHeaderReader, header *types.Header, snap *Snapshot) bool {
	if header.Difficulty.Cmp(diffInTurn) == 0 {
		return false
	}

	highestVerifiedHeader := chain.GetHighestVerifiedHeader()
	if highestVerifiedHeader == nil {
		log.Warn("Shouldn't wait for block process, because there is no highest verified header")
		return false
	}

	if header.ParentHash == highestVerifiedHeader.ParentHash {
		return true
	}
	return false
}

func (p *Parlia) EnoughDistance(chain consensus.ChainReader, header *types.Header) bool {
	snap, err := p.snapshot(chain, header.Number.Uint64()-1, header.ParentHash, nil)
	if err != nil {
		return true
	}
	return snap.enoughDistance(p.val, header)
}

func (p *Parlia) IsLocalBlock(header *types.Header) bool {
	return p.val == header.Coinbase
}

func (p *Parlia) SignRecently(chain consensus.ChainReader, parent *types.Block) (bool, error) {
	snap, err := p.snapshot(chain, parent.NumberU64(), parent.Hash(), nil)
	if err != nil {
		return true, err
	}

	// Bail out if we're unauthorized to sign a block
	if _, authorized := snap.Validators[p.val]; !authorized {
		return true, errUnauthorizedValidator(p.val.String())
	}

	return snap.SignRecently(p.val), nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have based on the previous blocks in the chain and the
// current signer.
func (p *Parlia) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	snap, err := p.snapshot(chain, parent.Number.Uint64(), parent.Hash(), nil)
	if err != nil {
		return nil
	}
	return CalcDifficulty(snap, p.val)
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have based on the previous blocks in the chain and the
// current signer.
func CalcDifficulty(snap *Snapshot, signer common.Address) *big.Int {
	if snap.inturn(signer) {
		return new(big.Int).Set(diffInTurn)
	}
	return new(big.Int).Set(diffNoTurn)
}

func encodeSigHeaderWithoutVoteAttestation(w io.Writer, header *types.Header, chainId *big.Int) {
	err := rlp.Encode(w, []interface{}{
		chainId,
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:extraVanity], // this will panic if extra is too short, should check before calling encodeSigHeaderWithoutVoteAttestation
		header.MixDigest,
		header.Nonce,
	})
	if err != nil {
		panic("can't encode: " + err.Error())
	}
}

// SealHash returns the hash of a block without vote attestation prior to it being sealed.
// So it's not the real hash of a block, just used as unique id to distinguish task
func (p *Parlia) SealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	encodeSigHeaderWithoutVoteAttestation(hasher, header, p.chainConfig.ChainID)
	hasher.Sum(hash[:0])
	return hash
}

// APIs implements consensus.Engine, returning the user facing RPC API to query snapshot.
func (p *Parlia) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{{
		Namespace: "parlia",
		Version:   "1.0",
		Service:   &API{chain: chain, parlia: p},
		Public:    false,
	}}
}

// Close implements consensus.Engine. It's a noop for parlia as there are no background threads.
func (p *Parlia) Close() error {
	return nil
}

// ==========================  interaction with contract/account =========

// Returns the inflation % for years passed
// years are estimated based on the seconds passed since the fork block's timestamp
// If year > 13: pct = 1.88, Otherwise  pct = 9.24e^(-0.250x) + 1.60
// Note: result is rounded to 1e18 decimal places and returned as int (percent * 1e18)
func getInflationPct(secondsPassed uint64) *big.Int {
	// convert seconds to years
	year := big.NewFloat(0)
	year.Mul(big.NewFloat(0).SetUint64(secondsPassed), big.NewFloat(1.0/31536000))
	year.Add(year, big.NewFloat(1))
	yearF, _ := year.Float64()

	log.Trace("inflation year", "year", yearF)

	var inflationPct float64
	if year.Cmp(big.NewFloat(13)) > 0 {
		inflationPct = 1.88
	} else {
		initDecayMag := 9.24
		decayRate := -0.25
		offset := 1.6

		inflationPct = initDecayMag*math.Pow(math.E, decayRate*yearF) + offset
	}
	inflationPct = math.Round(inflationPct*1e18) / 1e18
	return big.NewInt(int64(inflationPct * 1e18))
}

// Returns amount of chz & inflation pct for specific block (part of dragon8)
func getNewSupplyForBlock(forkTs uint64, currentTs uint64, lastSupply *big.Int) (*big.Int, *big.Int) {
	// Calculate the new supply
	secondsPassed := currentTs - forkTs
	newIntroducedSupply := big.NewInt(0)
	inflationPct := getInflationPct(secondsPassed)
	newIntroducedSupply.Mul(lastSupply, inflationPct)
	newIntroducedSupply.Div(newIntroducedSupply, big.NewInt(1e18))
	newIntroducedSupply.Div(newIntroducedSupply, big.NewInt(100)) // inflPct is percent*1e18

	// Calculate amount for 1 block
	blockAmount := big.NewInt(0)
	blockAmount.Div(newIntroducedSupply, big.NewInt(10512000)) // 10512000 blocks = ~1y

	log.Trace("inflation details", "secondsSinceFork", secondsPassed, "newIntroducedSupply", newIntroducedSupply, "blockAmount", blockAmount)

	return blockAmount, inflationPct
}

// Returns inflation %, supply, amount for the block (part of dragon8Fix)
func getNewSupplyForBlockDragon8Fix(forkTime uint64, currentTime uint64) (*big.Int, *big.Int, *big.Int) {
	var (
		// inflation %, supply, amount per block
		inflationData = [][]*big.Int{
			{big.NewInt(87961192355797800), cmath.MustParseBig256("8888888888000000000000000000"), cmath.MustParseBig256("74379496319128800000")},
			{big.NewInt(72043432957447300), cmath.MustParseBig256("9670766153000000000000000000"), cmath.MustParseBig256("66278081527102500000")},
			// one time inflation of 148600000 CHZ during year 1 + og schedule
			// new supply after year1 = 10367481346 + 148600000 = 10516081346
			// year 3 to year 8 have changed
			{big.NewInt(55304937824698300), cmath.MustParseBig256("10516081346000000000000000000"), cmath.MustParseBig256("55326410292998500000")},
			{big.NewInt(46543801590000000), cmath.MustParseBig256("11097672571000000000000000000"), cmath.MustParseBig256("49136973934551000000")},
			{big.NewInt(39673708820000000), cmath.MustParseBig256("11614200441000000000000000000"), cmath.MustParseBig256("43833562214611900000")},
			{big.NewInt(39673708820000000), cmath.MustParseBig256("12074978847000000000000000000"), cmath.MustParseBig256("39395232686453600000")},
			{big.NewInt(34295934680000000), cmath.MustParseBig256("12489101533000000000000000000"), cmath.MustParseBig256("35751611491628600000")},
			{big.NewInt(30091911660000000), cmath.MustParseBig256("12864922473000000000000000000"), cmath.MustParseBig256("34885308219178100000")},
			//
			{big.NewInt(25738888349516300), cmath.MustParseBig256("13231636833000000000000000000"), cmath.MustParseBig256("32397985455982600000")},
			{big.NewInt(23584653872848300), cmath.MustParseBig256("13572204456000000000000000000"), cmath.MustParseBig256("30450508407284500000")},
			{big.NewInt(21906934375499800), cmath.MustParseBig256("13892300200000000000000000000"), cmath.MustParseBig256("28951456317173600000")},
			{big.NewInt(20600325117190600), cmath.MustParseBig256("14196637909000000000000000000"), cmath.MustParseBig256("27821095556737700000")},
			{big.NewInt(19582736803651100), cmath.MustParseBig256("14489093265000000000000000000"), cmath.MustParseBig256("26991638121944800000")},
			{big.NewInt(18800000000000000), cmath.MustParseBig256("14772829365000000000000000000"), cmath.MustParseBig256("26420204724736900000")},
		}
		yearInSecs = uint64(31536000)
		year       = uint64(0)
	)

	// calculate current inflation year from block number
	year = (currentTime - forkTime) / yearInSecs
	log.Debug("inflation year", "year", year)

	if year >= 13 {
		return inflationData[13][0], inflationData[13][1], inflationData[13][2]
	}

	return inflationData[year][0], inflationData[year][1], inflationData[year][2]
}

func (p *Parlia) getLastSupplyFromTokenomics(header *types.Header) (*big.Int, error) {
	method := "getTotalSupply"
	data, err := p.tokenomicsABI.Pack(method)
	if err != nil {
		return nil, err
	}
	msgData := (hexutil.Bytes)(data)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	args := ethapi.TransactionArgs{
		From: &header.Coinbase,
		To:   &systemcontract.TokenomicsContractAddress,
		Gas:  &gas,
		Data: &msgData,
	}
	blockNum := (rpc.BlockNumber)(big.NewInt(0).Sub(header.Number, big.NewInt(1)).Int64())
	blockNr := rpc.BlockNumberOrHashWithNumber(blockNum)
	res, err := p.ethAPI.Call(context.Background(), args, &blockNr, nil, nil)
	if err != nil {
		return nil, err
	}

	initialTotalSupply := big.NewInt(0)
	if err := p.tokenomicsABI.UnpackIntoInterface(&initialTotalSupply, method, res); err != nil {
		return nil, err
	}
	return initialTotalSupply, nil
}

func (p *Parlia) distributeToTokenomics(amount *big.Int, inflationPct *big.Int, validator common.Address, newTotalSupply *big.Int,
	state *state.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool) error {
	// method
	method := "deposit"

	// get packed data
	data, err := p.tokenomicsABI.Pack(method, validator, newTotalSupply, inflationPct)
	if err != nil {
		log.Error("Unable to pack tx for tokenomics deposit", "error", err)
		return err
	}
	// get system message
	msg := p.getSystemMessage(header.Coinbase, systemcontract.TokenomicsContractAddress, data, amount)
	// apply message
	return p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining)
}

func (p *Parlia) distributePepper8(state *state.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool) error {

	amount := p.GetPepper8MintAmount()
	recipient := p.getPepper8RecipientAddress()

	// get system message
	msg := p.getSystemMessage(header.Coinbase, recipient, nil, amount)
	// apply message
	return p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining)
}

// getCurrentValidators get current validators
func (p *Parlia) getCurrentValidators(blockHash common.Hash, blockNum *big.Int) ([]common.Address, map[common.Address]*types.BLSPublicKey, error) {
	// block
	blockNr := rpc.BlockNumberOrHashWithHash(blockHash, false)

	if !p.chainConfig.IsLuban(blockNum) {
		validators, err := p.getCurrentValidatorsBeforeLuban(blockHash, blockNum)
		return validators, nil, err
	}

	// method
	method := "getMiningValidators"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // cancel when we are finished consuming integers

	data, err := p.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getMiningValidators", "error", err)
		return nil, nil, err
	}
	// call
	msgData := (hexutil.Bytes)(data)
	toAddress := common.HexToAddress(systemcontract.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, nil, err
	}

	var valSet []common.Address
	var voteAddrSet []types.BLSPublicKey
	if err := p.validatorSetABI.UnpackIntoInterface(&[]interface{}{&valSet, &voteAddrSet}, method, result); err != nil {
		return nil, nil, err
	}

	voteAddrMap := make(map[common.Address]*types.BLSPublicKey, len(valSet))
	for i := 0; i < len(valSet); i++ {
		voteAddrMap[valSet[i]] = &(voteAddrSet)[i]
	}
	return valSet, voteAddrMap, nil
}

// distributeIncoming distributes system incoming of the block
func (p *Parlia) distributeIncoming(val common.Address, state *state.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool) error {
	var (
		coinbase  = header.Coinbase
		isDragon8 = p.chainConfig.IsDragon8(header.Time) || p.chainConfig.IsDragon8Fix(header.Time)
	)

	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return errors.New("parent not found")
	}

	if p.IsPepper8Block(header.Time, parent.Time) {
		// set the bytecode of the deterministic deployment proxy
		bytecode, err := hex.DecodeString(p.getPepper8DeterministicDeploymentProxyBytecode())
		if err != nil {
			return err
		}
		state.SetCode(p.getPepper8DeterministicDeploymentProxyAddress(), bytecode)

		// distribute Pepper8
		log.Trace("distributePRB", "block hash", header.Number.Uint64())
		state.AddBalance(coinbase, uint256.MustFromBig(p.GetPepper8MintAmount()))
		if err := p.distributePepper8(state, header, chain, txs, receipts, receivedTxs, usedGas, mining); err != nil {
			return err
		}
	}

	if isDragon8 {
		var (
			blockAmount    *big.Int
			inflationPct   *big.Int
			lastSupply     *big.Int
			newTotalSupply *big.Int
		)

		lastSupply, err := p.getLastSupplyFromTokenomics(header)
		if err != nil {
			return err
		}

		if p.chainConfig.IsDragon8Fix(header.Time) {
			inflationPct, newTotalSupply, blockAmount = getNewSupplyForBlockDragon8Fix(*p.chainConfig.Dragon8FixTime, header.Time)
		} else if p.chainConfig.IsDragon8(header.Time) {
			blockAmount, inflationPct = getNewSupplyForBlock(*p.chainConfig.Dragon8Time, header.Time, lastSupply)
			newTotalSupply = big.NewInt(0).Add(lastSupply, blockAmount)
		}
		state.AddBalance(coinbase, uint256.MustFromBig(blockAmount))

		// DEPOSIT to tokenomics
		log.Trace("distribute to tokenomics", "block hash", header.Hash(), "amount", blockAmount, "inflation", inflationPct, "lastSupply", lastSupply, "newTotalSupply", newTotalSupply)
		if err := p.distributeToTokenomics(blockAmount, inflationPct, val, newTotalSupply, state, header, chain, txs, receipts, receivedTxs, usedGas, mining); err != nil {
			return err
		}
	}

	balance := state.GetBalance(consensus.SystemAddress)
	if balance.Cmp(common.U2560) <= 0 {
		return nil
	}
	state.SetBalance(consensus.SystemAddress, common.U2560)
	state.AddBalance(coinbase, balance)

	doDistributeSysReward := !isDragon8 && state.GetBalance(common.HexToAddress(systemcontracts.SystemRewardContract)).Cmp(maxSystemBalance) < 0
	if doDistributeSysReward {
		rewards := new(big.Int)
		rewards = rewards.Div(balance.ToBig(), big.NewInt(systemRewardPercent))
		if rewards.Cmp(common.Big0) > 0 {
			err := p.distributeToSystem(rewards, state, header, chain, txs, receipts, receivedTxs, usedGas, mining)
			if err != nil {
				return err
			}
			log.Trace("distribute to system reward pool", "block hash", header.Hash(), "amount", rewards)
			balance = balance.Sub(balance, uint256.MustFromBig(rewards))
		}
	}
	log.Trace("distribute to validator contract", "block hash", header.Hash(), "amount", balance)
	return p.distributeToValidator(balance.ToBig(), val, state, header, chain, txs, receipts, receivedTxs, usedGas, mining)
}

// slash spoiled validators
func (p *Parlia) slash(spoiledVal common.Address, state *state.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool) error {
	// method
	method := "slash"

	// get packed data
	data, err := p.slashABI.Pack(method,
		spoiledVal,
	)
	if err != nil {
		log.Error("Unable to pack tx for slash", "error", err)
		return err
	}
	// get system message
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontract.SlashContract), data, common.Big0)
	// apply message
	return p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining)
}

// init contract
func (p *Parlia) initContract(state *state.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool) error {
	// method
	method := "init"
	// get packed data
	data, err := p.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for init validator set", "error", err)
		return err
	}
	contracts := []common.Address{
		common.HexToAddress(systemcontract.ValidatorContract),
		common.HexToAddress(systemcontract.SlashContract),
		common.HexToAddress(systemcontract.SystemRewardContract),
		common.HexToAddress(systemcontract.StakingPoolContract),
		common.HexToAddress(systemcontract.GovernanceContract),
		common.HexToAddress(systemcontract.ChainConfigContract),
		common.HexToAddress(systemcontract.RuntimeUpgradeContract),
		common.HexToAddress(systemcontract.DeployerProxyContract),
	}
	if p.chainConfig.IsDragon8(header.Time) || p.chainConfig.IsDragon8Fix(header.Time) {
		contracts = append(contracts, common.HexToAddress(systemcontract.TokenomicsContract))
	}
	for _, c := range contracts {
		msg := p.getSystemMessage(header.Coinbase, c, data, common.Big0)
		// apply message
		log.Info("init contract", "block hash", header.Hash(), "contract", c)
		err = p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Parlia) distributeToSystem(amount *big.Int, state *state.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool) error {
	// get system message
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontract.SystemRewardContract), nil, amount)
	// apply message
	return p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining)
}

// distributeToValidator deposits validator reward to validator contract
func (p *Parlia) distributeToValidator(amount *big.Int, validator common.Address,
	state *state.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool) error {
	// method
	method := "deposit"

	// get packed data
	data, err := p.validatorSetABI.Pack(method,
		validator,
	)
	if err != nil {
		log.Error("Unable to pack tx for deposit", "error", err)
		return err
	}
	// get system message
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontract.ValidatorContract), data, amount)
	// apply message
	return p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining)
}

// get system message
func (p *Parlia) getSystemMessage(from, toAddress common.Address, data []byte, value *big.Int) callmsg {
	return callmsg{
		ethereum.CallMsg{
			From:     from,
			Gas:      math.MaxUint64 / 2,
			GasPrice: big.NewInt(0),
			Value:    value,
			To:       &toAddress,
			Data:     data,
		},
	}
}

func (p *Parlia) applyTransaction(
	msg callmsg,
	state *state.StateDB,
	header *types.Header,
	chainContext core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt,
	receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool,
) (err error) {
	nonce := state.GetNonce(msg.From())
	expectedTx := types.NewTransaction(nonce, *msg.To(), msg.Value(), msg.Gas(), msg.GasPrice(), msg.Data())
	expectedHash := p.signer.Hash(expectedTx)

	if msg.From() == p.val && mining {
		expectedTx, err = p.signTxFn(accounts.Account{Address: msg.From()}, expectedTx, p.chainConfig.ChainID)
		if err != nil {
			return err
		}
	} else {
		if receivedTxs == nil || len(*receivedTxs) == 0 || (*receivedTxs)[0] == nil {
			return errors.New("supposed to get a actual transaction, but get none")
		}
		actualTx := (*receivedTxs)[0]
		if !bytes.Equal(p.signer.Hash(actualTx).Bytes(), expectedHash.Bytes()) {
			return fmt.Errorf("expected tx hash %v, nonce %d, to %s, value %s, gas %d, gasPrice %s, data %s\ngot tx hash %v, nonce %d, to %s, value %s, gas %d, gasPrice %s, data %s",
				expectedHash.String(),
				expectedTx.Nonce(),
				expectedTx.To().String(),
				expectedTx.Value().String(),
				expectedTx.Gas(),
				expectedTx.GasPrice().String(),
				hex.EncodeToString(expectedTx.Data()),
				actualTx.Hash().Hex(),
				actualTx.Nonce(),
				actualTx.To().String(),
				actualTx.Value().String(),
				actualTx.Gas(),
				actualTx.GasPrice().String(),
				hex.EncodeToString(actualTx.Data()),
			)
		}
		expectedTx = actualTx
		// move to next
		*receivedTxs = (*receivedTxs)[1:]
	}
	state.SetTxContext(expectedTx.Hash(), len(*txs))
	gasUsed, err := applyMessage(msg, state, header, p.chainConfig, chainContext)
	if err != nil {
		return err
	}
	*txs = append(*txs, expectedTx)
	var root []byte
	if p.chainConfig.IsByzantium(header.Number) {
		state.Finalise(true)
	} else {
		root = state.IntermediateRoot(p.chainConfig.IsEIP158(header.Number)).Bytes()
	}
	*usedGas += gasUsed
	receipt := types.NewReceipt(root, false, *usedGas)
	receipt.TxHash = expectedTx.Hash()
	receipt.GasUsed = gasUsed

	// Set the receipt logs and create a bloom for filtering
	receipt.Logs = state.GetLogs(expectedTx.Hash(), header.Number.Uint64(), header.Hash())
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	receipt.BlockHash = header.Hash()
	receipt.BlockNumber = header.Number
	receipt.TransactionIndex = uint(state.TxIndex())
	*receipts = append(*receipts, receipt)
	return nil
}

// GetJustifiedNumberAndHash retrieves the number and hash of the highest justified block
// within the branch including `headers` and utilizing the latest element as the head.
func (p *Parlia) GetJustifiedNumberAndHash(chain consensus.ChainHeaderReader, headers []*types.Header) (uint64, common.Hash, error) {
	if chain == nil || len(headers) == 0 || headers[len(headers)-1] == nil {
		return 0, common.Hash{}, errors.New("illegal chain or header")
	}
	head := headers[len(headers)-1]
	snap, err := p.snapshot(chain, head.Number.Uint64(), head.Hash(), headers)
	if err != nil {
		log.Error("Unexpected error when getting snapshot",
			"error", err, "blockNumber", head.Number.Uint64(), "blockHash", head.Hash())
		return 0, common.Hash{}, err
	}

	if snap.Attestation == nil {
		if p.chainConfig.IsLuban(head.Number) {
			log.Debug("once one attestation generated, attestation of snap would not be nil forever basically")
		}
		return 0, chain.GetHeaderByNumber(0).Hash(), nil
	}
	return snap.Attestation.TargetNumber, snap.Attestation.TargetHash, nil
}

// GetFinalizedHeader returns highest finalized block header.
func (p *Parlia) GetFinalizedHeader(chain consensus.ChainHeaderReader, header *types.Header) *types.Header {
	if chain == nil || header == nil {
		return nil
	}
	if !chain.Config().IsPlato(header.Number) {
		return chain.GetHeaderByNumber(0)
	}

	snap, err := p.snapshot(chain, header.Number.Uint64(), header.Hash(), nil)
	if err != nil {
		log.Error("Unexpected error when getting snapshot",
			"error", err, "blockNumber", header.Number.Uint64(), "blockHash", header.Hash())
		return nil
	}

	if snap.Attestation == nil {
		return chain.GetHeaderByNumber(0) // keep consistent with GetJustifiedNumberAndHash
	}

	return chain.GetHeader(snap.Attestation.SourceHash, snap.Attestation.SourceNumber)
}

// ===========================     utility function        ==========================
func (p *Parlia) backOffTime(snap *Snapshot, header *types.Header, val common.Address) uint64 {
	if snap.inturn(val) {
		log.Debug("backOffTime", "blockNumber", header.Number, "in turn validator", val)
		return 0
	} else {
		delay := initialBackOffTime
		validators := snap.validators()
		if p.chainConfig.IsPlanck(header.Number) {
			counts := snap.countRecents()
			for addr, seenTimes := range counts {
				log.Debug("backOffTime", "blockNumber", header.Number, "validator", addr, "seenTimes", seenTimes)
			}

			// The backOffTime does not matter when a validator has signed recently.
			if snap.signRecentlyByCounts(val, counts) {
				return 0
			}

			inTurnAddr := snap.inturnValidator()
			if snap.signRecentlyByCounts(inTurnAddr, counts) {
				log.Debug("in turn validator has recently signed, skip initialBackOffTime",
					"inTurnAddr", inTurnAddr)
				delay = 0
			}

			// Exclude the recently signed validators and the in turn validator
			temp := make([]common.Address, 0, len(validators))
			for _, addr := range validators {
				if snap.signRecentlyByCounts(addr, counts) {
					continue
				}
				if p.chainConfig.IsBohr(header.Number, header.Time) {
					if addr == inTurnAddr {
						continue
					}
				}
				temp = append(temp, addr)
			}
			validators = temp
		}

		// get the index of current validator and its shuffled backoff time.
		idx := -1
		for index, itemAddr := range validators {
			if val == itemAddr {
				idx = index
			}
		}
		if idx < 0 {
			log.Debug("The validator is not authorized", "addr", val)
			return 0
		}

		randSeed := snap.Number
		if p.chainConfig.IsBohr(header.Number, header.Time) {
			randSeed = header.Number.Uint64() / uint64(snap.TurnLength)
		}
		s := rand.NewSource(int64(randSeed))
		r := rand.New(s)
		n := len(validators)
		backOffSteps := make([]uint64, 0, n)

		for i := uint64(0); i < uint64(n); i++ {
			backOffSteps = append(backOffSteps, i)
		}

		r.Shuffle(n, func(i, j int) {
			backOffSteps[i], backOffSteps[j] = backOffSteps[j], backOffSteps[i]
		})

		delay += backOffSteps[idx] * wiggleTime
		return delay
	}
}

// chain context
type chainContext struct {
	Chain  consensus.ChainHeaderReader
	parlia consensus.Engine
}

func (c chainContext) Engine() consensus.Engine {
	return c.parlia
}

func (c chainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	return c.Chain.GetHeader(hash, number)
}

// callmsg implements core.Message to allow passing it as a transaction simulator.
type callmsg struct {
	ethereum.CallMsg
}

func (m callmsg) From() common.Address { return m.CallMsg.From }
func (m callmsg) Nonce() uint64        { return 0 }
func (m callmsg) CheckNonce() bool     { return false }
func (m callmsg) To() *common.Address  { return m.CallMsg.To }
func (m callmsg) GasPrice() *big.Int   { return m.CallMsg.GasPrice }
func (m callmsg) Gas() uint64          { return m.CallMsg.Gas }
func (m callmsg) Value() *big.Int      { return m.CallMsg.Value }
func (m callmsg) Data() []byte         { return m.CallMsg.Data }

// apply message
func applyMessage(
	msg callmsg,
	state *state.StateDB,
	header *types.Header,
	chainConfig *params.ChainConfig,
	chainContext core.ChainContext,
) (uint64, error) {
	// Create a new context to be used in the EVM environment
	context := core.NewEVMBlockContext(header, chainContext, nil)
	// Create a new environment which holds all relevant information
	// about the transaction and calling mechanisms.
	vmenv := vm.NewEVM(context, vm.TxContext{Origin: msg.From(), GasPrice: big.NewInt(0)}, state, chainConfig, vm.Config{})
	// Apply the transaction to the current state (included in the env)
	if chainConfig.IsCancun(header.Number, header.Time) {
		rules := vmenv.ChainConfig().Rules(vmenv.Context.BlockNumber, vmenv.Context.Random != nil, vmenv.Context.Time)
		state.Prepare(rules, msg.From(), vmenv.Context.Coinbase, msg.To(), vm.ActivePrecompiles(rules), msg.AccessList)
	}
	// Increment the nonce for the next transaction
	state.SetNonce(msg.From(), state.GetNonce(msg.From())+1)

	ret, returnGas, err := vmenv.Call(
		vm.AccountRef(msg.From()),
		*msg.To(),
		msg.Data(),
		msg.Gas(),
		uint256.MustFromBig(msg.Value()),
	)
	if err != nil && len(ret) > 64+4 {
		log.Error("apply message failed", "msg", string(ret[64+4:]), "err", err)
	}
	return msg.Gas() - returnGas, err
}

// proposalKey build a key which is a combination of the block number and the proposer address.
func proposalKey(header types.Header) string {
	return header.ParentHash.String() + header.Coinbase.String()
}
