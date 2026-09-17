//go:build integration && !skiff

package harmonytask

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/harmony/taskhelp"
)

// Frozen from b80e09094ebf9fd64a695a0d657e68bce9f8a4b3. These are the
// pre-session registration and attempt preparation queries, not new-worker
// emulations that silently populate the new process provenance.
const preLifetimePrepare = `UPDATE harmony_task
SET attempt_id=$1, attempt_started_at=NULL, attempt_start_source='prepared'
WHERE id=$2 AND owner_id=$3 AND owner_generation=$4
  AND (attempt_id IS NULL OR attempt_id=$1)
  AND attempt_started_at IS NULL AND attempt_start_source IN ('claimed', 'prepared')`

const preLifetimeRegister = `
			WITH upsert AS (
				UPDATE harmony_machines
				SET cpu = $2, ram = $3, gpu = $4, last_contact = CURRENT_TIMESTAMP
				WHERE host_and_port = $1
				RETURNING id
			),
			inserted AS (
				INSERT INTO harmony_machines (host_and_port, cpu, ram, gpu, last_contact)
				SELECT $1, $2, $3, $4, CURRENT_TIMESTAMP
				WHERE NOT EXISTS (SELECT id FROM upsert)
				RETURNING id
			)
			SELECT id FROM upsert
			UNION ALL
			SELECT id FROM inserted;
		`

func TestTaskLifetimeSQLOldWorkerCompatibility(t *testing.T) {
	ctx, db, _, _ := attemptSQLFixture(t)
	source, err := os.ReadFile("../resources/resources.go")
	require.NoError(t, err)
	require.Contains(t, string(source), "`"+preLifetimeRegister+"`", "registration query still matches production")
	_, err = db.Exec(ctx, `UPDATE harmony_machines SET process_session='new-worker-session' WHERE id=101`)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE harmony_machines SET last_contact=CURRENT_TIMESTAMP WHERE id=101`)
	require.NoError(t, err)
	var session *string
	require.NoError(t, db.QueryRow(ctx, `SELECT process_session FROM harmony_machines WHERE id=101`).Scan(&session))
	require.Equal(t, "new-worker-session", *session, "heartbeat is not registration")
	var owner int
	require.NoError(t, db.QueryRow(ctx, preLifetimeRegister, "worker-a.example:12300", 8, 1024, 0).Scan(&owner))
	require.Equal(t, 101, owner)
	require.NoError(t, db.QueryRow(ctx, `SELECT process_session FROM harmony_machines WHERE id=101`).Scan(&session))
	require.Nil(t, session, "old registration invalidates provenance; do not show old worker as current session")

	e := &TaskEngine{cfg: taskEngineConfig{ctx: ctx, db: db, ownerID: 101}, taskMap: map[string]*taskTypeHandler{}}
	var id TaskID
	e.AddTaskByName("SDR", func(i TaskID, tx *harmonydb.Tx) (bool, error) {
		id = i
		_, err := tx.Exec(`INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof,task_id_sdr) VALUES(1000,70,8,$1)`, id)
		return err == nil, err
	})
	require.NotZero(t, id)
	h := &taskTypeHandler{TaskEngine: e, TaskTypeDetails: TaskTypeDetails{Name: "SDR", Max: taskhelp.Max(1), MaxFailures: 3}}
	// claimTaskOwnership and recordCompletion are byte-identical to the
	// pre-lifetime parent; run their real SQL on the additive schema.
	for attempt, done := range []bool{false, true} {
		var observed task
		require.NoError(t, db.QueryRow(ctx, `SELECT id,posted_time,update_time,retries FROM harmony_task WHERE id=$1`, id).Scan(&observed.ID, &observed.PostedTime, &observed.UpdateTime, &observed.Retries))
		gens := map[TaskID]int64{}
		ids, err := h.claimTaskOwnership([]TaskID{id}, 1, gens, observed)
		require.NoError(t, err)
		require.Equal(t, []TaskID{id}, ids)
		n, err := db.Exec(ctx, preLifetimePrepare, "old-binary-attempt", id, 101, gens[id])
		require.NoError(t, err)
		require.Equal(t, 1, n)
		n, err = db.Exec(ctx, RECORD_TASK_ATTEMPT_START, time.Now(), id, 101, "old-binary-attempt")
		require.NoError(t, err)
		require.Equal(t, 1, n)
		var unknown bool
		require.NoError(t, db.QueryRow(ctx, `SELECT attempt_session IS NULL AND sdr_execution IS NULL FROM harmony_task WHERE id=$1`, id).Scan(&unknown))
		require.True(t, unknown, "old execution is functional but deliberately Unknown in the new UI")
		var failure error
		if done {
			_, err = db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET after_sdr=true,task_id_sdr=NULL WHERE task_id_sdr=$1`, id)
			require.NoError(t, err)
		} else {
			failure = errors.New("fixture retry")
		}
		result := h.recordCompletion(id, nil, time.Now(), done, failure, false, completionIdentity{101, gens[id], "old-binary-attempt", true})
		require.True(t, result.applied)
		var histories int
		require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM harmony_task_history WHERE task_id=$1`, id).Scan(&histories))
		require.Equal(t, attempt+1, histories)
	}
	var remaining int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM harmony_task WHERE id=$1`, id).Scan(&remaining))
	require.Zero(t, remaining)
	t.Log("old registration -> real AddTask/link -> old prepare -> real retry -> re-claim -> SDR result -> real success/history; sequential SQL compatibility, not native mixed-worker operation")
}
