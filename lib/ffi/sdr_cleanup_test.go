package ffi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/puzpuzpuz/xsync/v2"
	"github.com/stretchr/testify/require"

	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

type sdrCleanupIndex struct {
	paths.SectorIndex
	declares atomic.Int32
}

func (s *sdrCleanupIndex) StorageDeclareSector(context.Context, storiface.ID, abi.SectorID, storiface.SectorFileType, bool) error {
	s.declares.Add(1)
	return nil
}

func (s *sdrCleanupIndex) StorageFindSector(context.Context, abi.SectorID, storiface.SectorFileType, abi.SectorSize, bool) ([]storiface.SectorStorageInfo, error) {
	return nil, nil
}

type sdrCleanupStore struct {
	paths.Store
	err error
}

func (s *sdrCleanupStore) Remove(context.Context, abi.SectorID, storiface.SectorFileType, bool, []storiface.ID) error {
	return s.err // Only ensureOneCopy; no filesystem/remote deletion in this fixture.
}

type sdrCleanupFixture struct {
	sb        *SealCalls
	index     *sdrCleanupIndex
	store     *sdrCleanupStore
	sector    storiface.SectorRef
	commD     cid.Cid
	into      storiface.SectorFileType
	dest      string
	releases  atomic.Int32
	onRelease func()
	recordIO  sdrscratch.RecordIO
	boundary  sdrscratch.Boundary
}

func newSDRCleanupFixture(t *testing.T, into storiface.SectorFileType, dest string) *sdrCleanupFixture {
	t.Helper()
	f := &sdrCleanupFixture{index: &sdrCleanupIndex{}, store: &sdrCleanupStore{}, into: into, dest: dest}
	f.sector = storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 42}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
	var err error
	f.commD, err = commcid.DataCommitmentV1ToCID(make([]byte, 32))
	require.NoError(t, err)
	remote, err := paths.NewRemote(f.store, f.index, nil, 1, nil)
	require.NoError(t, err)
	f.sb = &SealCalls{Sectors: &storageProvider{storage: remote, sindex: f.index, storageReservations: xsync.NewIntegerMapOf[harmonytask.TaskID, []*StorageReservation]()}}
	return f
}

func (f *sdrCleanupFixture) run(ctx context.Context, generate func(abi.RegisteredSealProof, string, [32]byte) error, cleanup func(string) error) error {
	var once sync.Once
	pp, ids := storiface.SectorPaths{}, storiface.SectorPaths{}
	storiface.SetPathByType(&pp, f.into, f.dest)
	storiface.SetPathByType(&ids, f.into, "test-storage")
	f.sb.Sectors.storageReservations.Store(1, []*StorageReservation{{SDR: paths.NewSDRReservation(f.dest), SectorRef: SectorRef{SpID: 1000, SectorNumber: 42, RegSealProof: f.sector.ProofType}, Alloc: f.into, Paths: pp, PathIDs: ids, Release: func() {
		once.Do(func() {
			if f.onRelease != nil {
				f.onRelease()
			}
			f.releases.Add(1)
		})
	}}})
	return f.sb.generateSDR(ctx, 1, f.into, f.sector, make([]byte, 32), f.commD, generate, cleanup, sdrscratch.Options{RecordIO: f.recordIO, Boundary: f.boundary})
}

func writeSDRTestLayers(p abi.RegisteredSealProof, dir string, _ [32]byte) error {
	n, err := proofpaths.SDRLayers(p)
	if err != nil {
		return err
	}
	for i := 1; i <= n; i++ {
		if err := os.WriteFile(filepath.Join(dir, proofpaths.LayerFileName(i)), make([]byte, 8192), 0600); err != nil {
			return err
		}
	}
	return nil
}

// This exact test is also run against the incident source with only the native
// call adapted. The old path leaks real, allocated files and declares a cache.
func TestSDRCleanupFailureRegression(t *testing.T) {
	f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "cache"))
	var scratch string
	err := f.run(context.Background(), func(p abi.RegisteredSealProof, dir string, r [32]byte) error {
		scratch = dir
		if err := writeSDRTestLayers(p, dir, r); err != nil {
			return err
		}
		return syscall.ENOSPC
	}, nil)
	require.ErrorIs(t, err, syscall.ENOSPC)
	assertSDREmptyTombstone(t, scratch)
	if f.index.declares.Load() != 0 {
		t.Errorf("failed SDR declared a cache: %d", f.index.declares.Load())
	}
	require.EqualValues(t, 1, f.releases.Load())
}

func TestSDRCleanupReturnAndCrossAttempt(t *testing.T) {
	for _, newerPublished := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-native-running", true: "new-cache-published"}[newerPublished], func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "cache")
			old, next := newSDRCleanupFixture(t, storiface.FTCache, dest), newSDRCleanupFixture(t, storiface.FTCache, dest)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			enteredOld, enteredNext := make(chan string, 1), make(chan string, 1)
			releaseOld, releaseNext := make(chan struct{}), make(chan struct{})
			doneOld, doneNext := make(chan error, 1), make(chan error, 1)
			body := func(entered chan string, release chan struct{}, result error) func(abi.RegisteredSealProof, string, [32]byte) error {
				return func(p abi.RegisteredSealProof, dir string, r [32]byte) error {
					if err := writeSDRTestLayers(p, dir, r); err != nil {
						return err
					}
					entered <- dir
					<-release
					return result
				}
			}
			go func() { doneOld <- old.run(ctx, body(enteredOld, releaseOld, syscall.ENOSPC), nil) }()
			oldDir := <-enteredOld
			cancel()
			require.DirExists(t, oldDir)
			require.Zero(t, old.releases.Load())
			go func() { doneNext <- next.run(context.Background(), body(enteredNext, releaseNext, nil), nil) }()
			nextDir := <-enteredNext
			require.NotEqual(t, oldDir, nextDir)
			if newerPublished {
				close(releaseNext)
				require.NoError(t, <-doneNext)
				require.DirExists(t, dest)
			}
			close(releaseOld)
			require.ErrorIs(t, <-doneOld, syscall.ENOSPC)
			assertSDREmptyTombstone(t, oldDir)
			if !newerPublished {
				require.DirExists(t, nextDir)
				close(releaseNext)
				require.NoError(t, <-doneNext)
			}
			require.FileExists(t, filepath.Join(dest, proofpaths.LayerFileName(11)))
			require.Zero(t, old.index.declares.Load())
			require.EqualValues(t, 1, next.index.declares.Load())
		})
	}
}

func TestSDRCleanupSuccessAndBoundaries(t *testing.T) {
	for _, into := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(into.String(), func(t *testing.T) {
			f := newSDRCleanupFixture(t, into, filepath.Join(t.TempDir(), "output"))
			require.NoError(t, f.run(context.Background(), writeSDRTestLayers, nil))
			entries, err := os.ReadDir(storiface.SDRTempRoot(f.dest))
			require.NoError(t, err)
			for _, e := range entries {
				assertSDREmptyTombstone(t, filepath.Join(storiface.SDRTempRoot(f.dest), e.Name()))
			}
			require.EqualValues(t, 1, f.index.declares.Load())
			// Known completion may be reused, but must not be regenerated.
			require.NoError(t, f.run(context.Background(), func(abi.RegisteredSealProof, string, [32]byte) error { t.Error("native repeated"); return nil }, nil))
			require.EqualValues(t, 2, f.index.declares.Load())
			_, err = os.Stat(f.dest)
			require.NoError(t, err)
		})
	}
	t.Run("existing-empty-directory", func(t *testing.T) {
		f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "output"))
		require.NoError(t, os.Mkdir(f.dest, 0755))
		require.Error(t, f.run(context.Background(), writeSDRTestLayers, nil))
		entries, err := os.ReadDir(f.dest)
		require.NoError(t, err)
		require.Empty(t, entries)
	})
	t.Run("pre-generation", func(t *testing.T) {
		root := t.TempDir()
		f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(root, "output"))
		for _, p := range []string{f.dest, f.dest + storiface.TempSuffix, filepath.Join(root, "sealed"), filepath.Join(root, "unsealed"), filepath.Join(root, "other-sector")} {
			require.NoError(t, os.WriteFile(p, []byte("preserve"), 0600))
		}
		f.commD = cid.Undef
		require.Error(t, f.run(context.Background(), func(abi.RegisteredSealProof, string, [32]byte) error { t.Error("native called"); return nil }, nil))
		for _, p := range []string{f.dest, f.dest + storiface.TempSuffix, filepath.Join(root, "sealed"), filepath.Join(root, "unsealed"), filepath.Join(root, "other-sector")} {
			b, err := os.ReadFile(p)
			require.NoError(t, err)
			require.Equal(t, "preserve", string(b))
		}
		require.Zero(t, f.index.declares.Load())
	})
	t.Run("post-publication-api-error", func(t *testing.T) {
		f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "output"))
		f.store.err = errors.New("index unavailable")
		require.ErrorIs(t, f.run(context.Background(), writeSDRTestLayers, nil), f.store.err)
		require.FileExists(t, filepath.Join(f.dest, proofpaths.LayerFileName(11)))
		require.EqualValues(t, 1, f.index.declares.Load())
	})
	t.Run("cleanup-error", func(t *testing.T) {
		f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "output"))
		var dir string
		err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
			dir = d
			if err := writeSDRTestLayers(p, d, r); err != nil {
				return err
			}
			return syscall.ENOSPC
		}, func(string) error { return syscall.EACCES })
		require.ErrorIs(t, err, syscall.ENOSPC)
		require.ErrorIs(t, err, syscall.EACCES)
		require.DirExists(t, dir)
		require.EqualValues(t, 1, f.releases.Load())
		require.Zero(t, f.index.declares.Load())
	})
}

func TestSDRCleanupRepeatedFailuresReleaseAfterCleanup(t *testing.T) {
	f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "output"))
	var cleanupFinished bool
	f.onRelease = func() { require.True(t, cleanupFinished, "reservation returned before scratch cleanup") }
	for i := 0; i < 20; i++ {
		cleanupFinished = false
		err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
			if err := writeSDRTestLayers(p, d, r); err != nil {
				return err
			}
			st, err := os.Stat(filepath.Join(d, proofpaths.LayerFileName(1)))
			require.NoError(t, err)
			require.Positive(t, st.Sys().(*syscall.Stat_t).Blocks, "fixture must allocate blocks, not just sparse apparent bytes")
			return syscall.ENOSPC
		}, func(string) error { cleanupFinished = true; return nil })
		require.ErrorIs(t, err, syscall.ENOSPC)
		entries, err := os.ReadDir(storiface.SDRTempRoot(f.dest))
		require.NoError(t, err)
		for _, e := range entries {
			assertSDREmptyTombstone(t, filepath.Join(storiface.SDRTempRoot(f.dest), e.Name()))
		}
	}
	require.EqualValues(t, 20, f.releases.Load())
	require.Zero(t, f.index.declares.Load())
}

func TestSDRCleanupDelayedReleaseFourSlots(t *testing.T) {
	// Four independent production GenerateSDR bodies/reservations. This models
	// a caller's capacity accounting, not native throughput or the scheduler.
	entered := make(chan struct{}, 4)
	released := make(chan struct{}, 4)
	allowCleanup := make(chan struct{})
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "output"))
		f.onRelease = func() { released <- struct{}{} }
		go func() {
			done <- f.run(context.Background(), func(p abi.RegisteredSealProof, dir string, r [32]byte) error {
				if err := writeSDRTestLayers(p, dir, r); err != nil {
					return err
				}
				return syscall.EIO
			}, func(string) error { entered <- struct{}{}; <-allowCleanup; return nil })
		}()
	}
	for i := 0; i < 4; i++ {
		<-entered
	}
	select {
	case <-released:
		t.Fatal("capacity returned before cleanup")
	default:
	}
	close(allowCleanup)
	for i := 0; i < 4; i++ {
		require.ErrorIs(t, <-done, syscall.EIO)
		<-released
	}
	next := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "next"))
	require.NoError(t, next.run(context.Background(), writeSDRTestLayers, nil))
	require.EqualValues(t, 1, next.releases.Load())
}

func TestSDRCleanupMissingLastLayerAndLegacyPreservation(t *testing.T) {
	f := newSDRCleanupFixture(t, storiface.FTKey, filepath.Join(t.TempDir(), "key"))
	legacy := f.dest + storiface.TempSuffix
	require.NoError(t, os.Mkdir(legacy, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "old-attempt"), []byte("untouched"), 0600))
	err := f.run(context.Background(), func(_ abi.RegisteredSealProof, dir string, _ [32]byte) error {
		return os.WriteFile(filepath.Join(dir, "partial"), []byte("partial"), 0600)
	}, nil)
	require.Error(t, err)
	entries, err := os.ReadDir(storiface.SDRTempRoot(f.dest))
	require.NoError(t, err)
	require.Len(t, entries, 1, "successful native return with invalid output is preserved for review")
	require.FileExists(t, filepath.Join(storiface.SDRTempRoot(f.dest), entries[0].Name(), "partial"))
	require.FileExists(t, filepath.Join(legacy, "old-attempt"))
	require.Zero(t, f.index.declares.Load())
}

func waitSDRBarrier(name string) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(name); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-deadline.C:
			return errors.New("SDR process barrier timed out")
		case <-tick.C:
		}
	}
}

func TestSDRCleanupProcessHelper(t *testing.T) {
	root := os.Getenv("CURIO_SDR_CLEANUP_TEST_CHILD")
	if root == "" {
		return
	}
	f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(root, "output"))
	err := f.run(context.Background(), func(p abi.RegisteredSealProof, dir string, r [32]byte) error {
		if err := writeSDRTestLayers(p, dir, r); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, "ready"), []byte(dir), 0600); err != nil {
			return err
		}
		if err := waitSDRBarrier(filepath.Join(root, "finish")); err != nil {
			return err
		}
		return syscall.EIO
	}, nil)
	require.ErrorIs(t, err, syscall.EIO)
	require.Zero(t, f.index.declares.Load())
	require.EqualValues(t, 1, f.releases.Load())
}

func TestSDRCleanupSeparateProcess(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSDRCleanupProcessHelper$", "-test.timeout=8s")
	cmd.Env = append(os.Environ(), "CURIO_SDR_CLEANUP_TEST_CHILD="+root)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	waited := false
	defer func() {
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	}()
	require.NoError(t, waitSDRBarrier(filepath.Join(root, "ready")))
	old, err := os.ReadFile(filepath.Join(root, "ready"))
	require.NoError(t, err)
	next := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(root, "output"))
	require.NoError(t, next.run(context.Background(), writeSDRTestLayers, nil))
	require.DirExists(t, string(old), "new attempt must not delete a different process's scratch")
	require.NoError(t, os.WriteFile(filepath.Join(root, "finish"), nil, 0600))
	err = cmd.Wait()
	waited = true
	require.NoError(t, err)
	assertSDREmptyTombstone(t, string(old))
	require.FileExists(t, filepath.Join(next.dest, proofpaths.LayerFileName(11)))
}

func assertSDREmptyTombstone(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "all failed scratch files must be reclaimed; the inode-bound empty directory is retained")
}
