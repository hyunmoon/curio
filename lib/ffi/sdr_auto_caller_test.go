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

	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"

	"github.com/filecoin-project/lotus/storage/sealer/fsutil"
)

type autoCallerStorage struct{ config storiface.StorageConfig }

func (s *autoCallerStorage) GetStorage() (storiface.StorageConfig, error) { return s.config, nil }
func (s *autoCallerStorage) SetStorage(f func(*storiface.StorageConfig)) error {
	f(&s.config)
	return nil
}
func (*autoCallerStorage) DiskUsage(p string) (int64, error) {
	var total int64
	err := filepath.Walk(p, func(_ string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if !i.IsDir() {
			total += i.Size()
		}
		return nil
	})
	return total, err
}
func (*autoCallerStorage) Stat(string) (fsutil.FsStat, error) {
	return fsutil.FsStat{Capacity: 1 << 40, Available: 1 << 40, FSAvailable: 1 << 40}, nil
}

func TestSDRAutoCallerDB(t *testing.T) {
	db := SDRRetryTestDB(t) // literal loopback, dedicated disposable role/database; actual migration runner
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		for _, shape := range []string{"legacy", "attempt", "owned", "canonical", "complete", "tree-rc-failed", "missing"} {
			if ft == storiface.FTKey && shape == "canonical" {
				continue
			}
			t.Run(ft.String()+"/"+shape, func(t *testing.T) {
				root := t.TempDir()
				id := storiface.ID(uuid.NewString())
				sector := abi.SectorID{Miner: 1000, Number: 42}
				meta, e := json.Marshal(storiface.LocalStorageMeta{ID: id, Weight: 1, CanSeal: true})
				require.NoError(t, e)
				require.NoError(t, os.WriteFile(filepath.Join(root, paths.MetaFile), meta, 0600))
				base := filepath.Join(root, ft.String())
				dest := filepath.Join(base, storiface.SectorName(sector))
				target := dest + ".tmp"
				if shape == "attempt" {
					target = filepath.Join(dest+".sdr.tmp", "attempt-"+uuid.NewString())
				}
				if shape == "owned" {
					target = filepath.Join(dest+".sdr.tmp", sdrscratch.Prefix+uuid.NewString())
				}
				if shape == "canonical" || shape == "complete" || shape == "tree-rc-failed" {
					target = dest
				}
				require.NoError(t, os.MkdirAll(target, 0700))
				file := filepath.Join(target, proofpaths.LayerFileName(1))
				require.NoError(t, os.WriteFile(file, make([]byte, 2048), 0600))
				if shape == "legacy" || shape == "attempt" || shape == "owned" {
					// Real native temporary naming plus a future backend artifact:
					// disposal authority belongs to the private folder, not names.
					for _, name := range []string{"sc-02-data-layer-11..tmp", "backend-interrupted-work"} {
						require.NoError(t, os.WriteFile(filepath.Join(target, name), []byte("partial"), 0600))
					}
				}
				_, e = db.Exec(ctx, `DELETE FROM sectors_sdr_pipeline WHERE sp_id=1000 AND sector_number=42`)
				require.NoError(t, e)
				_, e = db.Exec(ctx, `DELETE FROM sectors_unseal_pipeline WHERE sp_id=1000 AND sector_number=42`)
				require.NoError(t, e)
				if shape != "missing" {
					if ft == storiface.FTCache {
						_, e = db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof,after_sdr,failed) VALUES(1000,42,5,$1,$2)`, shape == "complete" || shape == "tree-rc-failed", shape == "tree-rc-failed")
					} else {
						_, e = db.Exec(ctx, `INSERT INTO sectors_unseal_pipeline(sp_id,sector_number,reg_seal_proof,after_unseal_sdr) VALUES(1000,42,5,$1)`, shape == "complete" || shape == "tree-rc-failed")
					}
					require.NoError(t, e)
				}
				sdrscratch.AutoFixtureSetup(t, []string{root})
				ls := &autoCallerStorage{storiface.StorageConfig{StoragePaths: []storiface.LocalPath{{Path: root}}}}
				index := paths.NewDBIndex(nil, db)
				local, e := paths.NewLocal(ctx, ls, index, "")
				require.NoError(t, e)
				if shape == "complete" || shape == "tree-rc-failed" || shape == "missing" {
					require.FileExists(t, file)
					return
				}
				require.NoDirExists(t, target, "startup must really remove legacy/partial canonical, not hide the error")
				local.PrepareSDRScratch() // idempotent admission preparation, no pacing delay
				sr := storiface.SectorRef{ID: sector, ProofType: abi.RegisteredSealProof_StackedDrg2KiBV1_1}
				pp, ids, e := local.AcquireSector(ctx, sr, storiface.FTNone, ft, storiface.PathSealing, storiface.AcquireMove)
				require.NoError(t, e)
				r := paths.NewSDRReservation(storiface.PathByType(pp, ft))
				release, e := local.ReserveSDR(ctx, sr, ft, ids, storiface.FSOverheadSeal, 0, r)
				require.NoError(t, e)
				remote, e := paths.NewRemote(local, index, nil, 1, nil)
				require.NoError(t, e)
				sb := NewSealCalls(remote, local, index)
				sb.Sectors.storageReservations.Store(1, []*StorageReservation{{SectorRef: SectorRef{SpID: 1000, SectorNumber: 42, RegSealProof: sr.ProofType}, Alloc: ft, Paths: pp, PathIDs: ids, SDR: r, Release: release}})
				commD, e := commcid.DataCommitmentV1ToCID(make([]byte, 32))
				require.NoError(t, e)
				calls := 0
				native := func(_ abi.RegisteredSealProof, d string, _ [32]byte) error {
					calls++
					for n := 1; n <= 2; n++ {
						if e := os.WriteFile(filepath.Join(d, proofpaths.LayerFileName(n)), make([]byte, 2048), 0600); e != nil {
							return e
						}
					}
					return nil
				}
				e = sb.generateSDR(ctx, 1, ft, sr, make([]byte, 32), commD, native, nil, sdrscratch.Options{})
				require.NoError(t, e)
				require.Equal(t, 1, calls)
				// Simulate DB persistence failure after successful publication: do
				// NOT change after_sdr. Startup and another GenerateSDR must reuse.
				local.PrepareSDRScratch()
				if ft == storiface.FTKey {
					require.FileExists(t, dest)
				} else {
					require.FileExists(t, filepath.Join(dest, proofpaths.LayerFileName(1)))
				}
				e = sb.generateSDR(ctx, 1, ft, sr, make([]byte, 32), commD, native, nil, sdrscratch.Options{})
				require.NoError(t, e)
				require.Equal(t, 1, calls, "published output reused without native restart")
				var n int
				if ft == storiface.FTCache {
					e = db.QueryRow(ctx, `SELECT count(*) FROM sectors_sdr_pipeline WHERE sp_id=1000 AND sector_number=42`).Scan(&n)
				} else {
					e = db.QueryRow(ctx, `SELECT count(*) FROM sectors_unseal_pipeline WHERE sp_id=1000 AND sector_number=42`).Scan(&n)
				}
				require.NoError(t, e)
				require.Equal(t, 1, n, "cleanup must preserve retry pipeline")
			})
		}
	}
}
