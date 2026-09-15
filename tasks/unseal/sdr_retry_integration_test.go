//go:build sdr_retry_itest

package unseal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-commp-utils/v2/zerocomm"
	"github.com/filecoin-project/go-state-types/abi"

	ffi2 "github.com/filecoin-project/curio/lib/ffi"
	"github.com/filecoin-project/curio/lib/storiface"
)

func TestSDRKeyCallerPublishedDBRetry(t *testing.T) {
	db := ffi2.SDRRetryTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 42}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
	_, err := db.Exec(ctx, `INSERT INTO sectors_unseal_pipeline(sp_id,sector_number,reg_seal_proof,task_id_unseal_sdr) VALUES(1000,42,$1,100)`, sr.ProofType)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO sectors_meta(sp_id,sector_num,reg_seal_proof,ticket_epoch,ticket_value,orig_unsealed_cid,orig_sealed_cid,cur_unsealed_cid,cur_sealed_cid,seed_epoch,seed_value) VALUES(1000,42,$1,0,$2,$3,$3,$3,$3,0,$2)`, sr.ProofType, make([]byte, 32), zerocomm.ZeroPieceCommitment(abi.PaddedPieceSize(32<<30).Unpadded()).String())
	require.NoError(t, err)
	_, err = db.Exec(ctx, `CREATE FUNCTION reject_key_success() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.after_unseal_sdr THEN RAISE EXCEPTION 'injected key completion failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_key_success BEFORE UPDATE ON sectors_unseal_pipeline FOR EACH ROW EXECUTE FUNCTION reject_key_success()`)
	require.NoError(t, err)
	dest := filepath.Join(t.TempDir(), "key")
	sc, calls := ffi2.SDRRetryTestCalls(t, storiface.FTKey, sr, 100, dest)
	task := &TaskUnsealSdr{db: db, sc: sc}
	done, err := task.Do(ctx, 100, func() bool { return true })
	require.False(t, done)
	require.ErrorContains(t, err, "injected key completion failure")
	require.EqualValues(t, 1, calls.Load())
	_, err = db.Exec(ctx, `DROP TRIGGER reject_key_success ON sectors_unseal_pipeline`)
	require.NoError(t, err)
	retry, calls2 := ffi2.SDRRetryTestCalls(t, storiface.FTKey, sr, 100, dest)
	task.sc = retry
	done, err = task.Do(ctx, 100, func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.Zero(t, calls2.Load())
	var after, noTask bool
	require.NoError(t, db.QueryRow(ctx, `SELECT after_unseal_sdr,task_id_unseal_sdr IS NULL FROM sectors_unseal_pipeline WHERE sp_id=1000 AND sector_number=42`).Scan(&after, &noTask))
	require.True(t, after)
	require.True(t, noTask)
}
