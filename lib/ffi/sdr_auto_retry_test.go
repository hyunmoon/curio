//go:build sdr_auto_itest && sdr_retry_itest

package ffi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"

	"github.com/filecoin-project/lotus/storage/sealer/fsutil"
)

// Actual filesystem free space, plus the existing MaxStorage path quota. The
// small quota induces capacity rejection without filling the host filesystem.
type retryUsageStorage struct{ autoCallerStorage }

func (*retryUsageStorage) Stat(p string) (fsutil.FsStat, error) { return fsutil.Statfs(p) }

func TestSDRAutoRetryWithoutAdmissionDB(t *testing.T) {
	db := SDRRetryTestDB(t)
	root := t.TempDir()
	id := storiface.ID(uuid.NewString())
	need, e := storiface.FTCache.SealSpaceUse(2048)
	require.NoError(t, e)
	meta, e := json.Marshal(storiface.LocalStorageMeta{ID: id, Weight: 1, CanSeal: true, MaxStorage: need + 3*2048})
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(filepath.Join(root, paths.MetaFile), meta, 0600))
	write := func(n abi.SectorNumber, complete bool) string {
		sector := abi.SectorID{Miner: 1000, Number: n}
		target := filepath.Join(root, "cache", storiface.SectorName(sector)+".tmp")
		require.NoError(t, os.MkdirAll(target, 0700))
		// No canonical layer: recovery must remove the actual native temporary
		// name through the timer and republish capacity without a new admission.
		require.NoError(t, os.WriteFile(filepath.Join(target, "sc-02-data-layer-1..tmp"), make([]byte, 2048), 0600))
		_, err := db.Exec(context.Background(), `INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof,after_sdr) VALUES(1000,$1,5,$2)`, n, complete)
		require.NoError(t, err)
		return target
	}
	old := write(100, false)
	live := write(101, false)
	complete := write(102, true)
	lockDir := func(p string) *os.File {
		f, err := os.Open(p)
		require.NoError(t, err)
		require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB))
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	oldLock := lockDir(old) // first startup and repeated timer passes defer
	_ = lockDir(live)       // a live writer's actual pinned-directory lock remains held
	sdrscratch.AutoFixtureSetup(t, []string{root})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ls := &retryUsageStorage{autoCallerStorage{storiface.StorageConfig{StoragePaths: []storiface.LocalPath{{Path: root}}}}}
	usedBefore, e := ls.DiskUsage(root)
	require.NoError(t, e)
	index := paths.NewDBIndex(nil, db)
	local, e := paths.NewLocal(ctx, ls, index, "")
	require.NoError(t, e)
	remote, e := paths.NewRemote(local, index, nil, 1, nil)
	require.NoError(t, e)
	sb := NewSealCalls(remote, local, index)
	var claims atomic.Int32
	storage := sb.Storage(func(harmonytask.TaskID) (SectorRef, error) {
		claims.Add(1)
		return SectorRef{SpID: 1000, SectorNumber: 103, RegSealProof: abi.RegisteredSealProof_StackedDrg2KiBV1_1}, nil
	}, storiface.FTCache, storiface.FTNone, 2048, storiface.PathSealing, 0).ForSDR()
	require.False(t, storage.HasCapacity(), "real files must exhaust configured path quota")
	before, e := local.FsStat(ctx, id)
	require.NoError(t, e)
	require.Less(t, before.Available, int64(need))
	// No tasks are created and no scheduler admission is invoked. Thus full
	// slots/cordon/pacing cannot be the driver of the recovery assertion.
	time.Sleep(100 * time.Millisecond)
	require.DirExists(t, old)
	require.False(t, storage.HasCapacity())
	require.Zero(t, claims.Load())
	require.NoError(t, oldLock.Close())
	require.Eventually(t, func() bool {
		_, err := os.Stat(old)
		return os.IsNotExist(err) && storage.HasCapacity()
	}, 3*time.Second, 10*time.Millisecond, "R1: same Local must retry without Claim and publish reclaimed capacity")
	require.Zero(t, claims.Load(), "cleanup must not manufacture task admission")
	require.DirExists(t, live)
	require.DirExists(t, complete)
	usedAfter, e := ls.DiskUsage(root)
	require.NoError(t, e)
	require.Equal(t, int64(2048), usedBefore-usedAfter, "actual layer bytes removed, not fixed available-space stub")
	after, e := local.FsStat(ctx, id)
	require.NoError(t, e)
	require.GreaterOrEqual(t, after.Available, int64(need))
	t.Logf("real directory usage %d -> %d; available quota %d -> %d; required=%d", usedBefore, usedAfter, before.Available, after.Available, need)
	t.Run("post-recovery-existing-Claim", func(t *testing.T) {
		release, e := storage.Claim(9001)
		require.NoError(t, e, "normal allocation/reservation follows the fresh health report")
		require.Equal(t, int32(1), claims.Load())
		require.NoError(t, release())
	})
	var pipelines, tasks int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM sectors_sdr_pipeline`).Scan(&pipelines))
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM harmony_task`).Scan(&tasks))
	require.Equal(t, 3, pipelines)
	require.Zero(t, tasks)
}
