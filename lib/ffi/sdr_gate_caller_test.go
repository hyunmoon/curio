//go:build sdr_auto_itest && sdr_retry_itest

package ffi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

func TestSDRCleanupGateCallerDB(t *testing.T) {
	for _, mode := range []string{"fresh", "receipt", "claim", "cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			db := SDRRetryTestDB(t)
			root := t.TempDir()
			id := storiface.ID(uuid.NewString())
			meta, e := json.Marshal(storiface.LocalStorageMeta{ID: id, CanSeal: true, Weight: 1})
			require.NoError(t, e)
			require.NoError(t, os.WriteFile(filepath.Join(root, paths.MetaFile), meta, 0600))
			sdrscratch.AutoFixtureSetup(t, []string{root})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, e = db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof) VALUES(1000,42,5)`)
			require.NoError(t, e)
			index := paths.NewDBIndex(nil, db)
			local, e := paths.NewLocal(ctx, &autoCallerStorage{storiface.StorageConfig{StoragePaths: []storiface.LocalPath{{Path: root}}}}, index, "")
			require.NoError(t, e)
			remote, e := paths.NewRemote(local, index, nil, 1, nil)
			require.NoError(t, e)
			sb := NewSealCalls(remote, local, index)
			sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 42}, ProofType: abi.RegisteredSealProof_StackedDrg2KiBV1_1}
			dest := filepath.Join(root, "cache", storiface.SectorName(sr.ID))
			commD, e := commcid.DataCommitmentV1ToCID(make([]byte, 32))
			require.NoError(t, e)
			var nativeCalls atomic.Int32
			var released atomic.Int32
			native := func(_ abi.RegisteredSealProof, p string, _ [32]byte) error {
				nativeCalls.Add(1)
				for n := 1; n <= 2; n++ {
					if err := os.WriteFile(filepath.Join(p, proofpaths.LayerFileName(n)), make([]byte, 2048), 0600); err != nil {
						return err
					}
				}
				return nil
			}
			accessCtx := ctx
			generate := func() error {
				return sb.generateSDR(accessCtx, 1, storiface.FTCache, sr, make([]byte, 32), commD, native, nil, sdrscratch.Options{})
			}
			prepare := func() {
				pp, ids, e := local.AcquireSector(ctx, sr, storiface.FTNone, storiface.FTCache, storiface.PathSealing, storiface.AcquireMove)
				require.NoError(t, e)
				r := paths.NewSDRReservation(pp.Cache)
				release, e := local.ReserveSDR(ctx, sr, storiface.FTCache, ids, storiface.FSOverheadSeal, 0, r)
				require.NoError(t, e)
				t.Cleanup(release)
				sb.Sectors.storageReservations.Store(1, []*StorageReservation{{SectorRef: SectorRef{SpID: 1000, SectorNumber: 42, RegSealProof: sr.ProofType}, Alloc: storiface.FTCache, Paths: pp, PathIDs: ids, SDR: r, Release: func() { released.Add(1); release() }}})
			}
			if mode != "claim" {
				prepare()
			}
			if mode == "receipt" {
				require.NoError(t, generate())
			}
			if mode != "receipt" {
				require.NoError(t, os.MkdirAll(dest+".tmp", 0700))
				require.NoError(t, os.WriteFile(filepath.Join(dest+".tmp", proofpaths.LayerFileName(1)), make([]byte, 2048), 0600))
			}
			entered, unblock := sdrscratch.AutoFixtureHoldNextCleanup(t, root)
			defer unblock()
			cleanupDone := make(chan struct{})
			go func() { local.PrepareSDRScratch(); close(cleanupDone) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("cleanup did not reach exclusive-gate barrier")
			}
			if mode == "claim" {
				storage := sb.Storage(func(harmonytask.TaskID) (SectorRef, error) {
					return SectorRef{SpID: 1000, SectorNumber: 42, RegSealProof: sr.ProofType}, nil
				}, storiface.FTCache, storiface.FTNone, 2048, storiface.PathSealing, 0).ForSDR()
				_, e = storage.Claim(1)
				require.ErrorIs(t, e, unix.EWOULDBLOCK)
				_, ok := sb.Sectors.storageReservations.Load(1)
				require.False(t, ok)
				unblock()
				<-cleanupDone
				release, e := storage.Claim(1)
				require.NoError(t, e)
				// The prepared reservation prevents cleanup from taking an exclusive
				// gate between Claim and Do. No pacing/failure budget has been committed.
				require.ErrorIs(t, sdrscratch.AutoFixtureExclusiveAccess(root, storiface.SectorName(sr.ID)), unix.EWOULDBLOCK)
				// Pre-entry cancellation frees this reservation's gate.
				require.NoError(t, release())
				require.NoError(t, sdrscratch.AutoFixtureExclusiveAccess(root, storiface.SectorName(sr.ID)))
				release, e = storage.Claim(1)
				require.NoError(t, e)
				require.NoError(t, generate())
				require.Equal(t, int32(1), nativeCalls.Load())
				require.NoError(t, release())
				require.NoError(t, sdrscratch.AutoFixtureExclusiveAccess(root, storiface.SectorName(sr.ID)))
				local.PrepareSDRScratch()
				return
			}
			if mode == "receipt" {
				prepare()
			}
			if mode == "cancel" || mode == "deadline" {
				var stop context.CancelFunc
				accessCtx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
				defer stop()
				done := make(chan error, 1)
				go func() { done <- generate() }()
				if mode == "cancel" {
					time.Sleep(40 * time.Millisecond)
					stop()
				}
				e = <-done
				if mode == "cancel" {
					require.ErrorIs(t, e, context.Canceled)
				} else {
					require.ErrorIs(t, e, context.DeadlineExceeded)
				}
				require.Zero(t, nativeCalls.Load())
				require.Equal(t, int32(1), released.Load(), "access failure releases the storage reservation")
				unblock()
				<-cleanupDone
				// The same sector can be claimed and released after the failed accessor.
				storage := sb.Storage(func(harmonytask.TaskID) (SectorRef, error) {
					return SectorRef{SpID: 1000, SectorNumber: 42, RegSealProof: sr.ProofType}, nil
				}, storiface.FTCache, storiface.FTNone, 2048, storiface.PathSealing, 0).ForSDR()
				release, e := storage.Claim(2)
				require.NoError(t, e)
				require.NoError(t, release())
				return
			}
			before := nativeCalls.Load()
			done := make(chan error, 1)
			go func() { done <- generate() }()
			select {
			case err := <-done:
				t.Fatalf("F1: real SDR caller must wait through cleanup, returned %v", err)
			case <-time.After(75 * time.Millisecond):
			}
			require.Equal(t, before, nativeCalls.Load(), "native must not start under cleanup gate")
			unblock()
			<-cleanupDone
			select {
			case e = <-done:
				require.NoError(t, e)
			case <-time.After(2 * time.Second):
				t.Fatal("access did not resume")
			}
			if mode == "receipt" {
				require.Equal(t, int32(1), nativeCalls.Load(), "published output must be reused")
			} else {
				require.Equal(t, int32(1), nativeCalls.Load())
			}
		})
	}
}

func TestSDRCleanupGateClaimFailureDB(t *testing.T) {
	for _, mode := range []string{"preparation", "reservation"} {
		t.Run(mode, func(t *testing.T) {
			db := SDRRetryTestDB(t)
			root := t.TempDir()
			id := storiface.ID(uuid.NewString())
			meta, err := json.Marshal(storiface.LocalStorageMeta{ID: id, CanSeal: true, Weight: 1})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, paths.MetaFile), meta, 0600))
			sdrscratch.AutoFixtureSetup(t, []string{root})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			index := paths.NewDBIndex(nil, db)
			local, err := paths.NewLocal(ctx, &autoCallerStorage{storiface.StorageConfig{StoragePaths: []storiface.LocalPath{{Path: root}}}}, index, "")
			require.NoError(t, err)
			remote, err := paths.NewRemote(local, index, nil, 1, nil)
			require.NoError(t, err)
			sb := NewSealCalls(remote, local, index)
			blocked := filepath.Join(root, "cache", "s-t01000-43")
			if mode == "reservation" {
				blocked += ".tmp"
			}
			require.NoError(t, os.MkdirAll(blocked, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(blocked, "unknown-protected-file"), []byte("preserve"), 0600))
			storage := sb.StorageMulti(func(harmonytask.TaskID) ([]SectorRef, error) {
				return []SectorRef{{SpID: 1000, SectorNumber: 42, RegSealProof: 5}, {SpID: 1000, SectorNumber: 43, RegSealProof: 5}}, nil
			}, storiface.FTCache, storiface.FTNone, 2048, storiface.PathSealing, 0, storiface.FSOverheadSeal).ForSDR()
			_, err = storage.Claim(1)
			require.Error(t, err)
			if mode == "reservation" {
				require.ErrorContains(t, err, "SDR scratch cleanup before reservation")
			} else {
				require.ErrorContains(t, err, "receipt")
			}
			_, ok := sb.Sectors.storageReservations.Load(1)
			require.False(t, ok)
			for _, sector := range []string{"s-t01000-42", "s-t01000-43"} {
				require.NoError(t, sdrscratch.AutoFixtureExclusiveAccess(root, sector), "multi-sector claim must release all gates")
			}
			stat, err := local.FsStat(ctx, id)
			require.NoError(t, err)
			require.Zero(t, stat.Reserved)
			b, err := os.ReadFile(filepath.Join(blocked, "unknown-protected-file"))
			require.NoError(t, err)
			require.Equal(t, "preserve", string(b))
			// StorageTryLock releases asynchronously on the Claim timer context.
			// Keep the fixture DB open until those actual goroutines finish;
			// dropping the schema sooner only creates teardown errors.
			require.Eventually(t, func() bool {
				buf := make([]byte, 1<<20)
				n := runtime.Stack(buf, true)
				return n < len(buf) && !bytes.Contains(buf[:n], []byte("paths.(*DBIndex).StorageTryLock.func1"))
			}, 2*time.Second, 10*time.Millisecond)
		})
	}
}
