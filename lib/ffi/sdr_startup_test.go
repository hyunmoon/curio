package ffi

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

func TestSDRStartupAfterCleanupFailure(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(ft.String(), func(t *testing.T) {
			f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
			var scratch string
			err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
				scratch = d
				if err := writeSDRTestLayers(p, d, r); err != nil {
					return err
				}
				return syscall.ENOSPC
			}, func(string) error { return syscall.EACCES })
			require.ErrorIs(t, err, syscall.ENOSPC)
			require.ErrorIs(t, err, syscall.EACCES)
			r, err := sdrscratch.Sweep(filepath.Dir(f.dest))
			require.NoError(t, err)
			require.Len(t, r, 1)
			require.Equal(t, "reclaimed", r[0].Status)
			require.Equal(t, 11, r[0].FilesRemoved)
			assertSDREmptyTombstone(t, scratch)
			require.NoError(t, f.run(context.Background(), writeSDRTestLayers, nil), "next generation and publication still work")
		})
	}
}

func TestSDRStartupPreservesCompletedUnpublished(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(ft.String(), func(t *testing.T) {
			f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
			var scratch string
			err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
				scratch = d
				if err := writeSDRTestLayers(p, d, r); err != nil {
					return err
				}
				return os.WriteFile(f.dest, []byte("unrelated destination"), 0600)
			}, nil)
			require.Error(t, err)
			r, err := sdrscratch.Sweep(filepath.Dir(f.dest))
			require.NoError(t, err)
			require.Len(t, r, 1)
			require.Equal(t, "needs_review", r[0].Status)
			entries, err := os.ReadDir(scratch)
			require.NoError(t, err)
			require.Len(t, entries, 11)
			b, err := os.ReadFile(f.dest)
			require.NoError(t, err)
			require.Equal(t, "unrelated destination", string(b))
		})
	}
}

func TestSDRStartupPanicIsNotReturn(t *testing.T) {
	f := newSDRCleanupFixture(t, storiface.FTCache, filepath.Join(t.TempDir(), "output"))
	var scratch string
	require.Panics(t, func() {
		_ = f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
			scratch = d
			require.NoError(t, writeSDRTestLayers(p, d, r))
			panic("simulated interruption before native return")
		}, nil)
	})
	r, err := sdrscratch.Sweep(filepath.Dir(f.dest))
	require.NoError(t, err)
	require.Len(t, r, 1)
	require.Equal(t, "needs_review", r[0].Status)
	entries, err := os.ReadDir(scratch)
	require.NoError(t, err)
	require.Len(t, entries, 11)
}
