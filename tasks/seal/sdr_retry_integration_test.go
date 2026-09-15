//go:build sdr_retry_itest

package seal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/crypto"

	ffi2 "github.com/filecoin-project/curio/lib/ffi"
	"github.com/filecoin-project/curio/lib/storiface"

	"github.com/filecoin-project/lotus/chain/types"
)

type retryTicketAPI struct {
	ts     *types.TipSet
	epochs []abi.ChainEpoch
}

func (a *retryTicketAPI) ChainHead(context.Context) (*types.TipSet, error) { return a.ts, nil }
func (a *retryTicketAPI) StateGetRandomnessFromTickets(_ context.Context, _ crypto.DomainSeparationTag, e abi.ChainEpoch, _ []byte, _ types.TipSetKey) (abi.Randomness, error) {
	a.epochs = append(a.epochs, e)
	b := make([]byte, 32)
	b[0] = byte(e)
	return b, nil
}

func TestSDRCallerPublishedDBRetry(t *testing.T) {
	db := ffi2.SDRRetryTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 42}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
	_, err := db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof,task_id_sdr) VALUES(1000,42,$1,100)`, sr.ProofType)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `CREATE FUNCTION reject_sdr_success() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.after_sdr THEN RAISE EXCEPTION 'injected completion write failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_sdr_success BEFORE UPDATE ON sectors_sdr_pipeline FOR EACH ROW EXECUTE FUNCTION reject_sdr_success()`)
	require.NoError(t, err)
	api := &retryTicketAPI{ts: porepLifecycleTipSet(t)}
	dest := filepath.Join(t.TempDir(), "cache")
	sc, calls := ffi2.SDRRetryTestCalls(t, storiface.FTCache, sr, 100, dest)
	task := &SDRTask{db: db, api: api, sc: sc}
	done, err := task.Do(ctx, 100, func() bool { return true })
	require.False(t, done)
	require.ErrorContains(t, err, "injected completion write failure")
	require.EqualValues(t, 1, calls.Load())
	var after bool
	var linked int
	require.NoError(t, db.QueryRow(ctx, `SELECT after_sdr,task_id_sdr FROM sectors_sdr_pipeline WHERE sp_id=1000 AND sector_number=42`).Scan(&after, &linked))
	require.False(t, after)
	require.Equal(t, 100, linked)
	_, err = db.Exec(ctx, `DROP TRIGGER reject_sdr_success ON sectors_sdr_pipeline`)
	require.NoError(t, err)
	// New handle/claim models a process restart. A newer chain head must not
	// relabel the published labels with a freshly selected ticket.
	blocks := api.ts.Blocks()
	blocks[0].Height += 100
	api.ts, err = types.NewTipSet(blocks)
	require.NoError(t, err)
	retry, calls2 := ffi2.SDRRetryTestCalls(t, storiface.FTCache, sr, 100, dest)
	task.sc = retry
	done, err = task.Do(ctx, 100, func() bool { return true })
	require.NoError(t, err)
	require.True(t, done)
	require.Zero(t, calls2.Load())
	var epoch abi.ChainEpoch
	var ticket []byte
	var noTask bool
	require.NoError(t, db.QueryRow(ctx, `SELECT after_sdr,ticket_epoch,ticket_value,task_id_sdr IS NULL FROM sectors_sdr_pipeline WHERE sp_id=1000 AND sector_number=42`).Scan(&after, &epoch, &ticket, &noTask))
	require.True(t, after)
	require.True(t, noTask)
	require.Equal(t, api.epochs[0], epoch)
	require.Equal(t, api.epochs[0], api.epochs[1])
	require.Equal(t, byte(epoch), ticket[0])
}
