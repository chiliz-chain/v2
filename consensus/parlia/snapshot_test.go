package parlia

import (
	"bytes"
	"fmt"
	"math/big"
	"sort"
	"testing"
	"text/tabwriter"
	"os"
	"encoding/binary"

	"github.com/golang/snappy"
	"github.com/stretchr/testify/assert"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestValidatorSetSort(t *testing.T) {
	size := 100
	validators := make([]common.Address, size)
	for i := 0; i < size; i++ {
		validators[i] = randomAddress()
	}
	sort.Sort(validatorsAscending(validators))
	for i := 0; i < size-1; i++ {
		assert.True(t, bytes.Compare(validators[i][:], validators[i+1][:]) < 0)
	}
}

func TestValidatorSelectionAlgorithm(t *testing.T) {
	var (
		valCount = 10
		validators = make([]common.Address, valCount)
		initialStakes = map[common.Address]*big.Int{}
		s = []int64{400,300,50,50,50,50,40,34,18,8}
		d = big.NewInt(1e18)
	)
	for i := 0; i < valCount; i++ {
		validators[i] = common.HexToAddress(fmt.Sprintf("0x%040x", i+1))
		initialStakes[validators[i]] = big.NewInt(s[i])
		initialStakes[validators[i]].Mul(initialStakes[validators[i]], d)
	}

	var(
		lastSnap *Snapshot
		blocks = 28800 * 365
		blocksPerValidator = map[common.Address]int{}
	)
	for i := 0; i < blocks; i++ {
		lastSnap = newSnapshot(
			nil,
			nil,
			uint64(i),
			common.Hash{},
			validators,
			[]types.BLSPublicKey{},
			nil,
			true,
		)

		// generate frequency data RLP
		freqRLP, err := lastSnap.calcFrequencyRLP(initialStakes)
		if err != nil {
			t.Fatalf("error calculating frequency hash: %v", err)
		}
		lastSnap.FrequencyRLP = freqRLP

		// update block distribution
		blocksPerValidator[lastSnap.inturnValidator()]++
	}

	sort.Slice(validators, func(i, j int) bool {
		return blocksPerValidator[validators[i]] > blocksPerValidator[validators[j]]
	})

	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	fmt.Fprintf(w, "Validator\t\tCHZ Staked\t\tBlocks Produced out of 28800\t\n")
	for _, addr := range validators {
		fmt.Fprintf(w, "%s\t\t%d\t\t%s\t\n", addr,  initialStakes[addr], fmt.Sprintf("%v (%v%%)", blocksPerValidator[addr], float64(blocksPerValidator[addr]*100)/float64(blocks)))
	}
	w.Flush()
}

func TestMeasureOverhead(t *testing.T) {
	extraVanity        := 32 // Fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal          := 65 // Fixed number of extra-data suffix bytes reserved for signer seal
	nextForkHashSize   := 4  // Fixed number of extra-data suffix bytes reserved for nextForkHash.
	freqDataPrefixSize := 3 // Fixed number of extra-data bytes reserved for frequency data prefix
	parentTsSize       := 8 // Size of parent block timestamp in bytes
	calcFreqRlp := func(count int) []byte {
		validators := make([]common.Address, count)
		initialStakes := map[common.Address]*big.Int{}
		for i := 0; i < count; i++ {
			validators[i] = common.HexToAddress(fmt.Sprintf("0x%040x", i+1))
			initialStakes[validators[i]] = big.NewInt(1e18)
		}

		snap := newSnapshot(
			nil,
			nil,
			0,
			common.Hash{},
			validators,
			[]types.BLSPublicKey{},
			nil,
			true,
		)
		freqRLP, err := snap.calcFrequencyRLP(initialStakes)
		if err != nil {
			t.Fatalf("error calculating frequency hash: %v", err)
		}

		return freqRLP
	}

	calcValBytes := func(count int) []byte {
		validators := make([]common.Address, count)
		for i := 0; i < count; i++ {
			validators[i] = common.HexToAddress(fmt.Sprintf("0x%040x", i+1))
		}

		var v []byte

		for _, addr := range validators {
			v = append(v, addr.Bytes()...)
		}
		return v
	}

	ts := make([]byte, 8)
	binary.LittleEndian.PutUint64(ts, 1750854147)
	ev := make([]byte, extraVanity)
	es := make([]byte, extraSeal)
	nf := make([]byte, nextForkHashSize)
	var newData []byte
	newData = append(newData, ev...)
	newData = append(newData, nf...)
	newData = append(newData, calcValBytes(20)...)
	newData = append(newData, []byte("VFQ")...)
	newData = append(newData, ts...)
	newData = append(newData, calcFreqRlp(20)...)
	newData = append(newData, es...)

	compressed := snappy.Encode(nil, newData)
	fmt.Printf("Frequency data length: %v bytes\n", len(newData))
	fmt.Printf("Frequency data length (compressed): %v bytes\n", len(compressed))

	// counts := []int{13,14,21,35,45}
	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	fmt.Printf("\nSize of RLP encoded frequency data for 1 validator: %v bytes\n", len(calcFreqRlp(1)))
	fmt.Fprintf(w, "Number of Validators (2k+1)\t\tCurrent Size [epoch block | normal block] (bytes)\t\tNew Size [epoch block | normal block] (bytes)\t\tAdded overhead per Block(bytes)\t\tAdded storage overhead per year for 1 full archive node (gigabytes)\n")
	i := 6
	for j := 20; j < 100; j = 2*i+1 {
		currNormalSize := extraVanity + nextForkHashSize + extraSeal
		newNormalSize := extraVanity + nextForkHashSize + freqDataPrefixSize + parentTsSize + len(calcFreqRlp(j)) + extraSeal

		currEpochSize := extraVanity + nextForkHashSize + len(calcValBytes(j)) + extraSeal
		newEpochSize := extraVanity + nextForkHashSize + len(calcValBytes(j)) + freqDataPrefixSize + parentTsSize + len(calcFreqRlp(j)) + extraSeal

		overhead := freqDataPrefixSize + parentTsSize + len(calcFreqRlp(j))
		fmt.Fprintf(w, "%d\t\t%s\t\t%s\t\t%d\t\t%.3f\n",
			j,
			fmt.Sprintf("%d | %d", currEpochSize, currNormalSize),
			fmt.Sprintf("%d | %d", newEpochSize, newNormalSize),
			overhead,
			float64(overhead)*10518975.0/1e9,
		)
		i++
	}
	w.Flush()
}
