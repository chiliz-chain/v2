package parlia

import (
	"math"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// Pepper8 and Pipe8 are one-time mints: at the first block whose timestamp is
// at or past the fork time, a fixed amount of CHZ is created and forwarded to
// the recipient (CLAUDE.md section 2). IsPepper8Block / IsPipe8Block decide
// "is this the activation block" from the (current, parent) timestamp pair.
// Firing twice along a chain is a double mint; never firing skips it.
//
// FuzzMintBoundaryFiresOnce walks a strictly increasing timestamp sequence (a
// chain segment) and asserts each detector fires for exactly one consecutive
// pair when the segment crosses the fork time, for none when it does not, and
// for none when the fork is unscheduled.

// mintBoundarySequence turns fuzz bytes into a strictly increasing timestamp
// sequence of 2..200 entries starting at start. Each byte is a gap-1, so gaps
// of exactly one second (adjacent-block boundaries) are as likely as any other.
func mintBoundarySequence(start uint64, deltas []byte) []uint64 {
	if len(deltas) == 0 {
		deltas = []byte{0}
	}
	if len(deltas) > 199 {
		deltas = deltas[:199]
	}
	seq := make([]uint64, 0, len(deltas)+1)
	seq = append(seq, start)
	for _, d := range deltas {
		gap := uint64(d) + 1
		last := seq[len(seq)-1]
		if last > math.MaxUint64-gap {
			break
		}
		seq = append(seq, last+gap)
	}
	return seq
}

// countMintBoundaries applies detect to every consecutive (current, parent)
// pair and returns how many fired and the index of the last firing pair.
func countMintBoundaries(seq []uint64, detect func(cur, parent uint64) bool) (fires int, at int) {
	at = -1
	for i := 1; i < len(seq); i++ {
		if detect(seq[i], seq[i-1]) {
			fires++
			at = i
		}
	}
	return fires, at
}

// expectedMintBoundary returns the index i at which the segment first reaches
// forkTime (seq[i-1] < forkTime <= seq[i]), or -1 if the segment does not
// cross it. Crossing requires seq[0] < forkTime: a segment that starts at or
// past the fork already minted in an earlier block.
func expectedMintBoundary(seq []uint64, forkTime *uint64) int {
	if forkTime == nil || seq[0] >= *forkTime {
		return -1
	}
	for i := 1; i < len(seq); i++ {
		if seq[i] >= *forkTime {
			return i
		}
	}
	return -1
}

func FuzzMintBoundaryFiresOnce(f *testing.F) {
	// Fork exactly on an element in the middle of the segment.
	f.Add(true, uint64(1000), true, uint64(1006), uint64(994), []byte{2, 2, 2, 2, 2, 2})
	// Fork one second past the last element (not crossed) / equal to the first (already active).
	f.Add(true, uint64(1010), true, uint64(1000), uint64(1000), []byte{2, 2, 2})
	// Fork falls in a gap between two blocks.
	f.Add(true, uint64(1001), true, uint64(1005), uint64(1000), []byte{2, 2, 2})
	// Unscheduled forks.
	f.Add(false, uint64(0), false, uint64(0), uint64(0), []byte{0, 0, 0})
	// Mainnet Pepper8 time with three-second blocks around it.
	f.Add(true, uint64(1757410200), false, uint64(0), uint64(1757410191), []byte{2, 2, 2, 2, 2, 2})
	// Saturation at the top of the range.
	f.Add(true, uint64(math.MaxUint64), true, uint64(math.MaxUint64-1), uint64(math.MaxUint64-3), []byte{0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, p8Set bool, p8 uint64, pi8Set bool, pi8 uint64, start uint64, deltas []byte) {
		seq := mintBoundarySequence(start, deltas)
		if len(seq) < 2 {
			t.Skip("sequence saturated before a second entry")
		}

		cfg := &params.ChainConfig{Parlia: &params.ParliaConfig{Period: 3, Epoch: 200}}
		if p8Set {
			cfg.Pepper8Time = &p8
		}
		if pi8Set {
			cfg.Pipe8Time = &pi8
		}
		p := &Parlia{chainConfig: cfg}

		for _, fk := range []struct {
			name   string
			time   *uint64
			detect func(cur, parent uint64) bool
		}{
			{"Pepper8", cfg.Pepper8Time, p.IsPepper8Block},
			{"Pipe8", cfg.Pipe8Time, p.IsPipe8Block},
		} {
			wantAt := expectedMintBoundary(seq, fk.time)
			wantFires := 0
			if wantAt >= 0 {
				wantFires = 1
			}
			fires, at := countMintBoundaries(seq, fk.detect)
			if fires != wantFires {
				t.Fatalf("%s: fired %d times over %d blocks (fork=%v, seq[0]=%d, seq[last]=%d), want %d",
					fk.name, fires, len(seq), derefTime(fk.time), seq[0], seq[len(seq)-1], wantFires)
			}
			if at != wantAt {
				t.Fatalf("%s: fired at pair index %d, want %d (fork=%v, seq=%v)", fk.name, at, wantAt, derefTime(fk.time), seq)
			}
		}
	})
}

func derefTime(t *uint64) any {
	if t == nil {
		return nil
	}
	return *t
}
