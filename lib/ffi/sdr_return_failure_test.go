package ffi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

// Fail the real record serialization/write/Sync path, not Returned as a whole.
// Each generateSDR invocation has its own operations; no global fault switch.
type sdrReturnRecordFault struct {
	syncFailure, failReclaimed bool
	state                      string
	returnWrites, returnSyncs  int
}

func (f *sdrReturnRecordFault) SetXattr(fd int, key string, value []byte, flags int) error {
	var r struct{ State string }
	if err := json.Unmarshal(value, &r); err != nil {
		return err
	}
	f.state = r.State
	if r.State == "returned" {
		f.returnWrites++
	}
	if !f.syncFailure && (r.State == "returned" || f.failReclaimed && r.State == "reclaimed") {
		return unix.ENOSPC
	}
	return unix.Fsetxattr(fd, key, value, flags)
}

func (f *sdrReturnRecordFault) Sync(dir *os.File) error {
	if f.state == "returned" {
		f.returnSyncs++
	}
	if f.syncFailure && (f.state == "returned" || f.failReclaimed && f.state == "reclaimed") {
		return unix.EIO
	}
	return dir.Sync()
}

func TestSDRReturnRecordFailureCleansOwn(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		for _, syncFailure := range []bool{false, true} {
			t.Run(ft.String()+"/"+map[bool]string{false: "write", true: "sync"}[syncFailure], func(t *testing.T) {
				f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
				fault := &sdrReturnRecordFault{syncFailure: syncFailure}
				f.recordIO = fault
				nativeErr := errors.New("synchronous native failure")
				var scratch, other, completed, legacy string
				f.onRelease = func() { assertSDREmptyTombstone(t, scratch) }
				err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
					scratch = d
					require.NoError(t, writeSDRTestLayers(p, d, r))
					other = filepath.Join(filepath.Dir(d), sdrscratch.Prefix+uuid.NewString())
					w, err := sdrscratch.Begin(other)
					require.NoError(t, err)
					t.Cleanup(func() { _ = w.Close() })
					require.NoError(t, writeSDRTestLayers(p, other, r))
					completed = filepath.Join(filepath.Dir(d), sdrscratch.Prefix+uuid.NewString())
					cw, err := sdrscratch.Begin(completed)
					require.NoError(t, err)
					t.Cleanup(func() { _ = cw.Close() })
					require.NoError(t, writeSDRTestLayers(p, completed, r))
					receipt, err := newSDRReceipt(storiface.FTCache, f.sector, make([]byte, 32), f.commD, nil)
					require.NoError(t, err)
					require.NoError(t, writeSDRReceipt(completed, receipt))
					require.NoError(t, cw.Close())
					legacy = f.dest + ".tmp"
					require.NoError(t, os.Mkdir(legacy, 0700))
					require.NoError(t, os.WriteFile(filepath.Join(legacy, "layer-legacy"), []byte("preserved"), 0600))
					return nativeErr
				}, nil)
				require.ErrorIs(t, err, nativeErr)
				if syncFailure {
					require.ErrorIs(t, err, unix.EIO)
					require.Equal(t, 1, fault.returnSyncs)
				} else {
					require.ErrorIs(t, err, unix.ENOSPC)
					require.Zero(t, fault.returnSyncs, "write failure prevents Sync, not own cleanup")
				}
				require.Equal(t, 1, fault.returnWrites)
				require.ErrorContains(t, err, "recording SDR attempt return")
				require.NotContains(t, err.Error(), "cleaning SDR attempt", "record failure is not a cleanup failure")
				assertSDREmptyTombstone(t, scratch)
				require.EqualValues(t, 1, f.releases.Load())
				require.Zero(t, f.index.declares.Load())
				results, err := sdrscratch.Sweep(filepath.Dir(f.dest))
				require.NoError(t, err)
				states := map[string]string{}
				for _, result := range results {
					states[result.Path] = result.Status
				}
				// Paths returned by Sweep may have resolved the temporary root.
				other, err = filepath.EvalSymlinks(other)
				require.NoError(t, err)
				completed, err = filepath.EvalSymlinks(completed)
				require.NoError(t, err)
				require.Equal(t, "live", states[other])
				require.Equal(t, "needs_review", states[completed])
				for _, d := range []string{other, completed} {
					entries, err := os.ReadDir(d)
					require.NoError(t, err)
					require.Len(t, entries, 11)
					require.FileExists(t, filepath.Join(d, proofpaths.LayerFileName(11)))
				}
				require.FileExists(t, filepath.Join(legacy, "layer-legacy"))
			})
		}
	}
}

func TestSDRReturnRecordAndCleanupErrorsRemainDistinct(t *testing.T) {
	for _, cleanupBlocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "final-record-failure", true: "removal-blocked"}[cleanupBlocked], func(t *testing.T) {
			f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "output"))
			f.recordIO = &sdrReturnRecordFault{failReclaimed: true}
			nativeErr := errors.New("native failure")
			var scratch string
			var hook func(string) error
			if cleanupBlocked {
				hook = func(string) error { return unix.EACCES }
			}
			err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
				scratch = d
				require.NoError(t, writeSDRTestLayers(p, d, r))
				return nativeErr
			}, hook)
			require.ErrorIs(t, err, nativeErr)
			require.ErrorIs(t, err, unix.ENOSPC)
			require.ErrorContains(t, err, "recording SDR attempt return")
			require.ErrorContains(t, err, "cleaning SDR attempt")
			if cleanupBlocked {
				require.ErrorIs(t, err, unix.EACCES)
				require.ErrorContains(t, err, "0 files removed")
				r, scanErr := sdrscratch.Sweep(filepath.Dir(f.dest))
				require.NoError(t, scanErr)
				require.Equal(t, "needs_review", r[0].Status)
				entries, readErr := os.ReadDir(scratch)
				require.NoError(t, readErr)
				require.Len(t, entries, 11, "no certificate: restart cannot inherit current writer authority")
			} else {
				require.ErrorContains(t, err, "11 files removed")
				assertSDREmptyTombstone(t, scratch)
			}
		})
	}
}

func TestSDRReturnRecordFailurePreservesPublishedReuse(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		for _, syncFailure := range []bool{false, true} {
			t.Run(ft.String()+"/"+map[bool]string{false: "write", true: "sync"}[syncFailure], func(t *testing.T) {
				f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
				fault := &sdrReturnRecordFault{syncFailure: syncFailure}
				f.recordIO = fault
				var scratch string
				err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
					scratch = d
					return writeSDRTestLayers(p, d, r)
				}, nil)
				if ft == storiface.FTKey {
					require.ErrorContains(t, err, "recording SDR attempt return")
					require.Equal(t, 1, fault.returnWrites)
					assertSDREmptyTombstone(t, scratch)
				} else {
					require.NoError(t, err)
					require.Zero(t, fault.returnWrites, "published cache never enters reclamation")
				}
				receipt, err := readSDRReceipt(f.dest)
				require.NoError(t, err)
				require.NotNil(t, receipt)
				calls := 0
				err = f.run(context.Background(), func(abi.RegisteredSealProof, string, [32]byte) error {
					calls++
					return errors.New("published output must be reused")
				}, nil)
				require.NoError(t, err)
				require.Zero(t, calls)
				require.EqualValues(t, 2, f.releases.Load())
			})
		}
	}
}
