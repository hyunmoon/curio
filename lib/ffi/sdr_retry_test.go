package ffi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/storiface"
)

func TestSDRPublishedRetry(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(ft.String(), func(t *testing.T) {
			f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
			calls := 0
			native := func(p abi.RegisteredSealProof, d string, r [32]byte) error {
				calls++
				return writeSDRTestLayers(p, d, r)
			}
			f.store.err = errors.New("temporary post-publication error")
			require.ErrorContains(t, f.run(context.Background(), native, nil), "temporary post-publication error")
			f.store.err = nil
			require.NoError(t, f.run(context.Background(), native, nil), "known published output must finish on retry")
			require.Equal(t, 1, calls, "retry must not repeat native generation")
		})
	}
}

func TestSDRPublishedInputMismatch(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		for _, which := range []string{"ticket", "commD", "proof", "sector", "missing-layer"} {
			t.Run(ft.String()+"/"+which, func(t *testing.T) {
				f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
				require.NoError(t, f.run(context.Background(), writeSDRTestLayers, nil))
				sector, d, ticket := f.sector, f.commD, make([]byte, 32)
				switch which {
				case "ticket":
					ticket[0] = 1
				case "commD":
					b := make([]byte, 32)
					b[0] = 1
					var err error
					d, err = commcid.DataCommitmentV1ToCID(b)
					require.NoError(t, err)
				case "proof":
					sector.ProofType = abi.RegisteredSealProof_StackedDrg64GiBV1_1
				case "sector":
					r, err := readSDRReceipt(f.dest)
					require.NoError(t, err)
					r.Sector.Number++
					require.NoError(t, writeSDRReceipt(f.dest, *r))
				case "missing-layer":
					p := f.dest
					if ft == storiface.FTCache {
						p = filepath.Join(p, proofpaths.LayerFileName(1))
					}
					require.NoError(t, os.Truncate(p, 0))
				}
				calls := 0
				err := f.sb.generateSDR(context.Background(), 1, ft, sector, ticket, d, func(abi.RegisteredSealProof, string, [32]byte) error { calls++; return nil }, nil, nil)
				require.Error(t, err)
				require.Zero(t, calls)
			})
		}
	}
}

func TestSDRLatePublicationConflict(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(ft.String(), func(t *testing.T) {
			f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
			calls := 0
			err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
				calls++
				if err := writeSDRTestLayers(p, d, r); err != nil {
					return err
				}
				return os.WriteFile(f.dest, []byte("concurrent destination"), 0600)
			}, nil)
			require.Error(t, err)
			require.Equal(t, 1, calls)
			b, err := os.ReadFile(f.dest)
			require.NoError(t, err)
			require.Equal(t, "concurrent destination", string(b))
			require.Zero(t, f.index.declares.Load())
		})
	}
}

func TestSDRPublishedAcrossProcess(t *testing.T) {
	if dest := os.Getenv("CURIO_SDR_RECEIPT_TEST_CHILD"); dest != "" {
		f := newSDRCleanupFixture(t, storiface.FTCache, dest)
		f.store.err = errors.New("post-publish failure before process exit")
		require.ErrorContains(t, f.run(context.Background(), writeSDRTestLayers, nil), "post-publish failure")
		return
	}
	dest := filepath.Join(t.TempDir(), "cache")
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(exe, "-test.run=^TestSDRPublishedAcrossProcess$", "-test.timeout=20s")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CURIO_SDR_RECEIPT_TEST_CHILD=" + dest}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	f := newSDRCleanupFixture(t, storiface.FTCache, dest)
	require.NoError(t, f.run(context.Background(), func(abi.RegisteredSealProof, string, [32]byte) error {
		t.Error("native repeated after process restart")
		return nil
	}, nil))
}

func TestSDRExistingConflictIsEarly(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(ft.String(), func(t *testing.T) {
			f := newSDRCleanupFixture(t, ft, filepath.Join(t.TempDir(), "output"))
			require.NoError(t, os.Mkdir(f.dest, 0700))
			calls := 0
			err := f.run(context.Background(), func(p abi.RegisteredSealProof, d string, r [32]byte) error {
				calls++
				return writeSDRTestLayers(p, d, r)
			}, nil)
			require.Error(t, err)
			require.Zero(t, calls, "known conflict must reject before native entry")
		})
	}
}
