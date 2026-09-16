//go:build sdr_auto_itest && sdr_retry_itest

package ffi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/storiface"
)

// Exercise the actual Claim timer goroutine and reservation lock boundary.
func TestExistingClaimContextRaceDB(t *testing.T) {
	db := SDRRetryTestDB(t)
	root := t.TempDir()
	meta, e := json.Marshal(storiface.LocalStorageMeta{ID: storiface.ID(uuid.NewString()), CanSeal: true, Weight: 1})
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(filepath.Join(root, paths.MetaFile), meta, 0600))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	index := paths.NewDBIndex(nil, db)
	local, e := paths.NewLocal(ctx, &autoCallerStorage{storiface.StorageConfig{StoragePaths: []storiface.LocalPath{{Path: root}}}}, index, "")
	require.NoError(t, e)
	remote, e := paths.NewRemote(local, index, nil, 1, nil)
	require.NoError(t, e)
	sb := NewSealCalls(remote, local, index)
	storage := sb.Storage(func(id harmonytask.TaskID) (SectorRef, error) {
		return SectorRef{SpID: 1000, SectorNumber: int64(id), RegSealProof: 5}, nil
	}, storiface.FTCache, storiface.FTNone, 2048, storiface.PathSealing, 0)
	for n := 0; n < 20; n++ {
		release, e := storage.Claim(9000 + n)
		require.NoError(t, e)
		require.NoError(t, release())
	}
}
