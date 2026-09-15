package ffi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

type discardBoundary struct{}

func (discardBoundary) Current(string) (*sdrscratch.ManagedRun, error) {
	return &sdrscratch.ManagedRun{Domain: "fixture"}, nil
}
func (discardBoundary) Stopped(string, sdrscratch.ManagedRun) (bool, error) { return true, nil }

// The production generateSDR/receipt/no-replace/cleanup path runs here. Native
// proofs are replaced by small allocated files; this is not a CUDA execution.
func TestSDRDiscardUnpublishedSuccess(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(ft.String(), func(t *testing.T) {
			f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
			f.boundary = discardBoundary{}
			var scratch string
			err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
				scratch = d
				if err := writeSDRTestLayers(p, d, r); err != nil {
					return err
				}
				return os.WriteFile(f.dest, []byte("other canonical"), 0600)
			}, nil)
			require.ErrorContains(t, err, "renaming")
			assertSDREmptyTombstone(t, scratch)
			b, err := os.ReadFile(f.dest)
			require.NoError(t, err)
			require.Equal(t, "other canonical", string(b))
			require.EqualValues(t, 1, f.releases.Load())
		})
	}
}

func TestSDRDiscardPublishedRetry(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(ft.String(), func(t *testing.T) {
			f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
			f.boundary = discardBoundary{}
			calls := 0
			native := func(p abi.RegisteredSealProof, d string, r [32]byte) error {
				calls++
				return writeSDRTestLayers(p, d, r)
			}
			f.store.err = errors.New("post-publication unavailable")
			require.ErrorContains(t, f.run(context.Background(), native, nil), "unavailable")
			f.store.err = nil
			require.NoError(t, f.run(context.Background(), native, nil))
			require.Equal(t, 1, calls)
		})
	}
}

func TestSDRDiscardErrorAndNextAttempt(t *testing.T) {
	f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "output"))
	f.boundary = discardBoundary{}
	for i := 0; i < 4; i++ {
		var scratch string
		err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
			scratch = d
			if err := writeSDRTestLayers(p, d, r); err != nil {
				return err
			}
			return syscall.ENOSPC
		}, nil)
		require.ErrorIs(t, err, syscall.ENOSPC)
		assertSDREmptyTombstone(t, scratch)
	}
	require.NoError(t, f.run(context.Background(), writeSDRTestLayers, nil))
	require.EqualValues(t, 5, f.releases.Load())
}

func TestSDRDiscardDiagnosticFailure(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		for _, syncFail := range []bool{false, true} {
			t.Run(ft.String()+map[bool]string{false: "/write", true: "/sync"}[syncFail], func(t *testing.T) {
				f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
				f.boundary = discardBoundary{}
				f.recordIO = &sdrReturnRecordFault{syncFailure: syncFail}
				var scratch string
				err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
					scratch = d
					if err := writeSDRTestLayers(p, d, r); err != nil {
						return err
					}
					return unix.ENOSPC
				}, nil)
				require.ErrorIs(t, err, unix.ENOSPC)
				if syncFail {
					require.ErrorIs(t, err, unix.EIO)
				}
				assertSDREmptyTombstone(t, scratch)
			})
		}
	}
}

func TestSDRDiscardLatePublicationPreserved(t *testing.T) {
	f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "output"))
	f.boundary = discardBoundary{}
	err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
		if err := writeSDRTestLayers(p, d, r); err != nil {
			return err
		}
		return unix.EIO
	}, func(d string) error { return os.Rename(d, f.dest) })
	require.ErrorContains(t, err, "cleaning SDR attempt")
	require.FileExists(t, filepath.Join(f.dest, "sc-02-data-layer-11.dat"))
}
