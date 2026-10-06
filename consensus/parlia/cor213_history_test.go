package parlia

// Replays every epoch header since Snake8, dumped from the public RPCs by
// testdata/cor213/dump-epochs.sh, through the pre-COR-213 parser and the fixed
// one. Skipped unless COR213_MAINNET / COR213_SPICY point at a dump.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func legacyGetValidatorBytesPreLuban(header *types.Header, epochLength uint64) []byte {
	if len(header.Extra) <= extraVanity+extraSeal {
		return nil
	}
	start := extraVanity
	end := len(header.Extra) - extraSeal
	if bytes.HasPrefix(header.Extra[start:end], validatorFrequencyDataPrefix) {
		return nil
	}
	for i := 0; i <= end-3; i++ {
		if bytes.Equal(header.Extra[i:i+3], validatorFrequencyDataPrefix) {
			end = i
			break
		}
	}
	if end <= start {
		return nil
	}
	if header.Number.Uint64()%epochLength == 0 && (end-start)%validatorBytesLengthBeforeLuban != 0 {
		return nil
	}
	return header.Extra[start:end]
}

func TestCOR213History(t *testing.T) {
	type net struct {
		file   string
		epoch  uint64
		snake8 uint64
	}
	nets := map[string]net{
		"mainnet": {os.Getenv("COR213_MAINNET"), 28800, 1760432400},
		"spicy":   {os.Getenv("COR213_SPICY"), 7200, 1754991000},
	}
	for name, n := range nets {
		// One subtest per network. t.Skip in a plain loop body abandons the whole
		// test (runtime.Goexit), so with only one variable set the other network
		// was replayed or silently dropped depending on map iteration order.
		t.Run(name, func(t *testing.T) {
			if n.file == "" {
				t.Skipf("set COR213_%s to a dump from testdata/cor213/dump-epochs.sh", strings.ToUpper(name))
			}
			replayCOR213Dump(t, name, n.file, n.epoch, n.snake8)
		})
	}
}

// maxSnake8BoundarySlack is one block period on the Chiliz networks (3s), the
// widest gap there can be between a header's time and its parent's. A header
// whose time is Snake8 but whose time minus this is not may have a pre-fork
// parent, and Snake8 gates on parent.Time.
const maxSnake8BoundarySlack = 3

func replayCOR213Dump(t *testing.T, name, file string, epochLength, snake8Time uint64) {
	fh, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()

	st := snake8Time
	cfg := &params.ChainConfig{ChainID: big.NewInt(1), Parlia: &params.ParliaConfig{Epoch: epochLength, Period: 3}, Snake8Time: &st}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var total, snake8Headers, snake8Boundary, emptyTails, vfqInVanity, vfqInAddrs int
	var prevNumber, firstNumber, firstTime uint64
	var lastValidators []common.Address
	seen := map[common.Address]bool{}
	for sc.Scan() {
		var rec struct{ Number, Time, Extra string }
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		num, ok := new(big.Int).SetString(rec.Number, 0)
		if !ok {
			t.Fatalf("%s: unparsable block number %q", name, rec.Number)
		}
		tm, ok := new(big.Int).SetString(rec.Time, 0)
		if !ok {
			t.Fatalf("%s: block %s: unparsable timestamp %q", name, num, rec.Time)
		}
		h := &types.Header{Number: num, Time: tm.Uint64(), Extra: common.FromHex(rec.Extra)}
		total++
		if total == 1 {
			firstNumber, firstTime = num.Uint64(), h.Time
		}
		// The dump must be one contiguous run of epoch blocks; a gap means the
		// dump script lost a batch and the compatibility claim would be partial.
		if num.Uint64()%epochLength != 0 {
			t.Fatalf("%s: block %d is not an epoch block (epoch %d)", name, num.Uint64(), epochLength)
		}
		if total > 1 && num.Uint64() != prevNumber+epochLength {
			t.Fatalf("%s: epoch headers not contiguous: %d follows %d (epoch %d)", name, num.Uint64(), prevNumber, epochLength)
		}
		prevNumber = num.Uint64()
		legacy := legacyGetValidatorBytesPreLuban(h, epochLength)
		fixed := getValidatorBytesFromHeader(h, cfg, epochLength)
		if !bytes.Equal(legacy, fixed) {
			t.Errorf("%s block %d: legacy=%x fixed=%x", name, num, legacy, fixed)
		}
		if len(fixed) == 0 || len(fixed)%20 != 0 {
			t.Errorf("%s block %d: bad validator bytes len %d", name, num, len(fixed))
			continue
		}
		vals, _, err := parseValidators(h, cfg, epochLength)
		if err != nil {
			t.Errorf("%s block %d: %v", name, num, err)
		}
		for _, v := range vals {
			seen[v] = true
			if bytes.Contains(v.Bytes(), validatorFrequencyDataPrefix) {
				vfqInAddrs++
				t.Logf("%s block %d: validator %s contains VFQ", name, num, v)
			}
		}
		if bytes.Contains(h.Extra[:extraVanity+2], validatorFrequencyDataPrefix) {
			vfqInVanity++
		}
		lastValidators = vals
		// Snake8 gates on *parent*.Time, and the dump holds only epoch headers, so
		// the parent of any header here is 28800 blocks away and not available.
		// Classifying by h.Time alone mislabels the single epoch header that
		// straddles the fork — h.Time past snake8Time while parent.Time is not —
		// and the replay would then demand a frequency section the chain never
		// produced, reporting a regression that is really an off-by-one-block in
		// the test. Only assert the section for headers a full block period clear
		// of the boundary; the straddling header is counted and skipped.
		switch {
		case !cfg.IsSnake8(h.Time):
		case !cfg.IsSnake8(h.Time - maxSnake8BoundarySlack):
			snake8Boundary++
			t.Logf("%s block %d: time %d is within one block period of snake8Time; parent may be pre-fork, skipping the frequency-section assertions", name, num, h.Time)
		default:
			snake8Headers++
			ts, ok := extractSnake8ParentTimestamp(h, cfg)
			if !ok {
				t.Errorf("%s block %d: no parent timestamp", name, num)
			} else if ts+3 != h.Time {
				t.Logf("%s block %d: parent ts %d vs time %d", name, num, ts, h.Time)
			}
			tail, err := parseValidatorFrequencies(h, cfg)
			if err != nil {
				t.Errorf("%s block %d: %v", name, num, err)
			}
			if len(tail) == 0 {
				emptyTails++
			} else if !decodesAsFrequencyList(tail) {
				t.Errorf("%s block %d: tail does not decode", name, num)
			}
		}
	}
	// A truncated read (an over-long line, an I/O error) ends Scan silently and
	// would otherwise look like a short but clean dump.
	if err := sc.Err(); err != nil {
		t.Fatalf("%s: reading %s: %v", name, file, err)
	}
	// Without these the test passes vacuously on an empty or near-empty dump,
	// which is exactly what a broken dump script produces. The claim being made
	// is "every epoch header since Snake8", so the run has to start before the
	// fork (the script deliberately backs up two epochs) and carry Snake8 blocks.
	if total == 0 {
		t.Fatalf("%s: %s is empty — the dump script produced nothing to replay", name, file)
	}
	if firstTime >= snake8Time {
		t.Fatalf("%s: dump starts at block %d (time %d), at or after snake8Time %d — it misses the fork boundary and cannot show pre-Snake8 headers are unaffected",
			name, firstNumber, firstTime, snake8Time)
	}
	if snake8Headers == 0 {
		t.Fatalf("%s: no Snake8 epoch header in the dump — nothing exercised the frequency block", name)
	}
	t.Logf("%s: %d epoch headers %d..%d (%d Snake8, %d on the fork boundary), %d empty tails, %d VFQ-in-vanity, %d VFQ-in-address, %d distinct validators ever, current set %d: %v",
		name, total, firstNumber, prevNumber, snake8Headers, snake8Boundary, emptyTails, vfqInVanity, vfqInAddrs, len(seen), len(lastValidators), lastValidators)
}
