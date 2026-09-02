// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package replay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReplayFixtures re-executes every committed fixture and requires it to reproduce the
// gas the chain charged. Each fixture's comment says which invariant it stands for; the
// COR-193 ones are the tripwire for the DeployerProxy hook-dispatch warming in
// core/vm/chiliz.go, and fail by exactly +2500 if it is dropped.
//
// To add a block, point cmd/replaycheck at it with --save-fixture internal/replay/testdata;
// no code change is needed here.
func TestReplayFixtures(t *testing.T) {
	paths, err := filepath.Glob("testdata/*.json")
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no fixtures found")

	for _, path := range paths {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			fixture, err := UnmarshalFixture(raw)
			require.NoError(t, err)

			result, err := fixture.Replay(nil)
			require.NoError(t, err, "replay must run; %s", fixture.Comment)
			require.Equal(t, fixture.ExpectedFailed, result.VMErr != nil,
				"replay must succeed or fail exactly as the chain did (vm error: %v)", result.VMErr)
			require.Equal(t, fixture.ExpectedGasUsed, result.GasUsed,
				"block %d tx %d must cost what the chain charged, off by %+d\n%s",
				fixture.Block.Number, fixture.Tx.Index, fixture.Delta(result), fixture.Comment)
		})
	}
}
