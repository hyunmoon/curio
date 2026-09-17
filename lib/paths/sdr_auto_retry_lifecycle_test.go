//go:build sdr_auto_itest

package paths

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"

	"github.com/filecoin-project/lotus/storage/sealer/fsutil"
)

type retryStateMode struct {
	mode   atomic.Int32 // 0=transient DB error, 1=blocked connection, 2=eligible, 3=completed
	calls  atomic.Int32
	active atomic.Int32
}
type retryStateIndex struct {
	SectorIndex
	roots map[string]*retryStateMode
}

func (*retryStateIndex) StorageAttach(context.Context, storiface.StorageInfo, fsutil.FsStat) error {
	return nil
}
func (*retryStateIndex) StorageList(context.Context, storiface.ID) ([]storiface.Decl, error) {
	return nil, nil
}
func (*retryStateIndex) BatchStorageDeclareSectors(context.Context, []SectorDeclaration) error {
	return nil
}
func (*retryStateIndex) StorageReportHealth(context.Context, storiface.ID, storiface.HealthReport) error {
	return nil
}
func (*retryStateIndex) sdrExternallyReady(context.Context, abi.SectorID, storiface.SectorFileType) (bool, error) {
	return true, nil // completed canonical fixture; no chain/native assertion
}
func (i *retryStateIndex) withSDRDiscardState(ctx context.Context, target sdrscratch.AutoTarget, apply func(sdrscratch.AutoStage) error) error {
	m := i.roots[filepath.Dir(target.Base)]
	m.calls.Add(1)
	m.active.Add(1)
	defer m.active.Add(-1)
	switch m.mode.Load() {
	case 0:
		return errors.New("fixture transient DB failure")
	case 1:
		<-ctx.Done()
		return ctx.Err()
	case 3:
		return apply(sdrscratch.AutoStage{Reason: "SDR/later stage completed"})
	default:
		return apply(sdrscratch.AutoStage{Allowed: true, LayerBytes: 2048, LayerNames: []string{"sc-02-data-layer-1.dat", "sc-02-data-layer-2.dat"}})
	}
}

func TestSDRAutoRetryLifecycle(t *testing.T) {
	if !sdrscratch.AutoFixtureActive {
		t.Skip("explicit OS-evidence overlay required; SQL exercised separately")
	}
	for _, mode := range []string{"transient", "partial", "root-isolation-cancel", "completed-denial-cache"} {
		t.Run(mode, func(t *testing.T) {
			roots := []string{t.TempDir(), t.TempDir()}
			index := &retryStateIndex{roots: map[string]*retryStateMode{}}
			ls := &TestingLocalStorage{}
			var targets []string
			for _, root := range roots {
				id := storiface.ID(uuid.NewString())
				meta, err := json.Marshal(storiface.LocalStorageMeta{ID: id, CanSeal: true})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(root, MetaFile), meta, 0600))
				ls.c.StoragePaths = append(ls.c.StoragePaths, storiface.LocalPath{Path: root})
				index.roots[root] = &retryStateMode{}
				target := filepath.Join(root, "cache", "s-t01000-42.tmp")
				if mode == "completed-denial-cache" {
					target = filepath.Join(root, "cache", "s-t01000-42")
				}
				require.NoError(t, os.MkdirAll(target, 0700))
				for _, name := range []string{"sc-02-data-layer-1.dat", "sc-02-data-layer-2.dat"} {
					require.NoError(t, os.WriteFile(filepath.Join(target, name), make([]byte, 2048), 0600))
				}
				targets = append(targets, target)
			}
			sdrscratch.AutoFixtureSetup(t, roots)
			var fault *atomic.Bool
			if mode == "partial" {
				fault = sdrscratch.AutoFixtureFailUnlinkAfter(t, 1)
				index.roots[roots[0]].mode.Store(2)
			}
			if mode == "completed-denial-cache" {
				index.roots[roots[0]].mode.Store(3)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			local, err := NewLocal(ctx, ls, index, "")
			require.NoError(t, err)
			if mode == "partial" {
				require.True(t, fault.Load())
				entries, e := os.ReadDir(targets[0])
				require.NoError(t, e)
				require.Len(t, entries, 1, "one actual unlink succeeded before injected error")
			} else {
				require.DirExists(t, targets[0])
			}
			if mode == "completed-denial-cache" {
				time.Sleep(130 * time.Millisecond)
				require.Equal(t, int32(1), index.roots[roots[0]].calls.Load(), "unchanged completed data must not query DB every tick")
				require.DirExists(t, targets[0])
			} else {
				if mode == "root-isolation-cancel" {
					index.roots[roots[0]].mode.Store(1)
				} else {
					index.roots[roots[0]].mode.Store(2)
				}
				index.roots[roots[1]].mode.Store(2)
				require.Eventually(t, func() bool { _, e := os.Stat(targets[1]); return os.IsNotExist(e) }, time.Second, 5*time.Millisecond)
				if mode == "root-isolation-cancel" {
					require.Eventually(t, func() bool { return index.roots[roots[0]].active.Load() == 1 }, time.Second, 5*time.Millisecond)
					var overlap sync.WaitGroup
					for n := 0; n < 20; n++ {
						overlap.Add(1)
						go func() { defer overlap.Done(); local.PrepareSDRScratch() }()
					}
					overlap.Wait()
					require.Equal(t, int32(1), index.roots[roots[0]].active.Load())
					require.DirExists(t, targets[0])
				} else {
					require.Eventually(t, func() bool { _, e := os.Stat(targets[0]); return os.IsNotExist(e) }, time.Second, 5*time.Millisecond)
				}
			}
			cancel()
			require.Eventually(t, func() bool {
				return index.roots[roots[0]].active.Load() == 0 && index.roots[roots[1]].active.Load() == 0
			}, time.Second, 5*time.Millisecond)
			// Drain the actual root pass, then observe no new work after cancel.
			for _, root := range roots {
				r := local.cleanupRoot(root)
				r.mu.Lock()
				nextLog := r.nextLog
				r.mu.Unlock()
				require.False(t, nextLog.IsZero(), "root pass drained after lifecycle entry")
			}
			calls := index.roots[roots[0]].calls.Load() + index.roots[roots[1]].calls.Load()
			time.Sleep(60 * time.Millisecond)
			require.Equal(t, calls, index.roots[roots[0]].calls.Load()+index.roots[roots[1]].calls.Load())
		})
	}
}
