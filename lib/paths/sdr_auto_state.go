package paths

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

type sdrDiscardState interface {
	withSDRDiscardState(context.Context, sdrscratch.AutoTarget, func(sdrscratch.AutoStage) error) error
}

// The row lock serializes completed-stage persistence, not native execution.
// Filesystem access gates and managed-process evidence are checked separately.
func (dbi *DBIndex) withSDRDiscardState(ctx context.Context, t sdrscratch.AutoTarget, apply func(sdrscratch.AutoStage) error) error {
	id, err := storiface.ParseSectorID(t.Sector)
	if err != nil {
		return err
	}
	_, err = dbi.harmonyDB.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		var rows []struct {
			Proof int64 `db:"reg_seal_proof"`
			Done  bool  `db:"done"`
		}
		var readErr error
		switch filepath.Base(t.Base) {
		case "cache":
			readErr = tx.Select(&rows, `SELECT reg_seal_proof, after_sdr OR after_tree_d OR after_tree_c OR after_tree_r OR after_porep OR after_precommit_msg OR after_precommit_msg_success OR after_finalize OR after_move_storage OR after_commit_msg OR after_commit_msg_success AS done FROM sectors_sdr_pipeline WHERE sp_id=$1 AND sector_number=$2 FOR UPDATE`, id.Miner, id.Number)
		case "key":
			readErr = tx.Select(&rows, `SELECT reg_seal_proof, after_unseal_sdr OR after_decode_sector AS done FROM sectors_unseal_pipeline WHERE sp_id=$1 AND sector_number=$2 FOR UPDATE`, id.Miner, id.Number)
		default:
			return false, fmt.Errorf("unknown SDR file type")
		}
		if readErr != nil {
			return false, readErr
		}
		s := sdrscratch.AutoStage{Reason: "pipeline absent or ambiguous"}
		if len(rows) != 1 {
			return false, apply(s)
		}
		if rows[0].Done {
			s.Reason = "SDR/later stage completed"
			return false, apply(s)
		}
		if filepath.Base(t.Base) == "cache" {
			var protected bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sectors_meta WHERE sp_id=$1 AND sector_num=$2) OR EXISTS(SELECT 1 FROM sectors_snap_pipeline WHERE sp_id=$1 AND sector_number=$2) OR EXISTS(SELECT 1 FROM sectors_unseal_pipeline WHERE sp_id=$1 AND sector_number=$2)`, id.Miner, id.Number).Scan(&protected); err != nil {
				return false, err
			}
			if protected {
				s.Reason = "metadata/Snap/unseal lifecycle present"
				return false, apply(s)
			}
		}
		proof := abi.RegisteredSealProof(rows[0].Proof)
		n, err := proofpaths.SDRLayers(proof)
		if err != nil {
			return false, err
		}
		size, err := proof.SectorSize()
		if err != nil {
			return false, err
		}
		for i := 1; i <= n; i++ {
			s.LayerNames = append(s.LayerNames, proofpaths.LayerFileName(i))
		}
		s.LayerBytes = int64(size)
		s.Allowed = true
		s.Reason = "pre-SDR pipeline, exclusive local access and terminated pre-protocol runs"
		// No transaction retry: a filesystem side effect cannot be replayed by
		// the DB retry loop. Filesystem journals handle a later startup retry.
		if err := apply(s); err != nil {
			return false, err
		}
		if t.Canonical && t.StorageID != "" {
			if _, err := os.Lstat(filepath.Join(t.Base, t.Relative)); os.IsNotExist(err) {
				// The exclusive filesystem gate is still held. Do not touch any
				// replica or task. A DB error leaves a stale declaration, not a
				// false success; startup drop-missing will reconcile it.
				_, err = tx.Exec(`DELETE FROM sector_location WHERE miner_id=$1 AND sector_num=$2 AND sector_filetype=$3 AND storage_id=$4`, id.Miner, id.Number, int(storiface.FTCache), t.StorageID)
				return err == nil, err
			}
		}
		return false, nil
	})
	return err
}

func (st *Local) autoDiscardSDR(local string) {
	on, err := sdrscratch.PersonalCleanupEnabled()
	if err != nil || !on {
		return
	}
	db, ok := st.index.(sdrDiscardState)
	if !ok {
		log.Errorw("SDR automatic adoption unavailable", "reason", "pipeline state provider absent")
		return
	}
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		base := filepath.Join(local, ft.String())
		results, err := sdrscratch.AutoDiscard(base, func(t sdrscratch.AutoTarget, fn func(sdrscratch.AutoStage) error) error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return db.withSDRDiscardState(ctx, t, fn)
		})
		for _, r := range results {
			log.Infow("SDR automatic adoption", "path", r.Path, "status", r.Status, "reason", r.Reason, "filesRemoved", r.FilesRemoved, "allocatedBytes", r.AllocatedBytes, "freeBefore", r.FreeBefore, "freeAfter", r.FreeAfter)
		}
		if err != nil {
			log.Errorw("SDR root automatic adoption deferred", "type", ft, "error", err)
		}
	}
}
