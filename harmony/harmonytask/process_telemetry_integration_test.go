//go:build integration && !skiff

package harmonytask

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/harmony/resources"
)

// Consume the frozen deployed UI's actual SQL, not a reimplementation of its
// classification. The full original file is retained for byte comparison.
func deployedTookSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/3631-task_took.go.txt")
	require.NoError(t, err)
	_, tail, ok := strings.Cut(string(b), "const clusterTaskTookStateSQL = `")
	require.True(t, ok)
	sql, _, ok := strings.Cut(tail, "`")
	require.True(t, ok)
	return sql
}

func TestProcessTelemetryDoEntryAndRestart(t *testing.T) {
	ctx, db, _, conn := retrySQLFixture(t)
	register := func() *resources.Reg {
		r, err := resources.RegisterWithResources(db, "telemetry.example:12300", resources.Resources{Cpu: 4, Ram: 1024})
		require.NoError(t, err)
		t.Cleanup(r.Shutdown)
		require.NotEmpty(t, r.ProcessSession)
		return r
	}
	r := register()
	_, err := db.Exec(ctx, `INSERT INTO harmony_task(id,name,owner_id,added_by,posted_time) VALUES(1,'SDR',$1,$1,CURRENT_TIMESTAMP-INTERVAL '3 hours'),(2,'Synthetic',$1,$1,CURRENT_TIMESTAMP)`, r.MachineID)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof,task_id_sdr) VALUES(1000,1,8,1)`)
	require.NoError(t, err)
	state := func(id TaskID) string {
		t.Helper()
		var s string
		query := `WITH sl AS (SELECT task_id_sdr,count(*) AS sdr_references,bool_and(NOT after_sdr AND NOT failed) AS sdr_ready FROM sectors_sdr_pipeline GROUP BY task_id_sdr),
		 rows AS (SELECT t.*,m.process_session,COALESCE(sl.sdr_references,0) AS sdr_references,COALESCE(sl.sdr_ready,false) AS sdr_ready,CURRENT_TIMESTAMP AS observed_at
		 FROM harmony_task t LEFT JOIN harmony_machines m ON m.id=t.owner_id LEFT JOIN sl ON sl.task_id_sdr=t.id WHERE t.id=$1)
		 SELECT ` + deployedTookSQL(t) + ` FROM rows`
		require.NoError(t, conn.QueryRow(ctx, query, id).Scan(&s))
		return s
	}
	store := func(reg *resources.Reg, id TaskID) harmonyTaskAttemptStore {
		t.Helper()
		var generation int64
		require.NoError(t, db.QueryRow(ctx, `SELECT owner_generation FROM harmony_task WHERE id=$1`, id).Scan(&generation))
		return harmonyTaskAttemptStore{db: db, owner: reg.MachineID, session: reg.ProcessSession, generations: map[TaskID]int64{id: generation}}
	}
	require.Equal(t, "unknown", state(1), "claim alone is not current-process provenance")
	s := store(r, 1)
	require.NoError(t, s.prepare(ctx, 1, "first"))
	require.Equal(t, "awaiting-start", state(1))
	var posted time.Time
	require.NoError(t, db.QueryRow(ctx, `SELECT posted_time FROM harmony_task WHERE id=1`).Scan(&posted))
	entry := time.Now().UTC().Add(-time.Second)
	history := time.Time{}
	ok, err := runWithAttemptStart(ctx, s, 1, "first", nil, func() time.Time { return entry }, func(start time.Time) { history = start }, func() (bool, error) {
		// Substitute Do, not native execution: wait for the real asynchronous
		// DB writer so its successful result can be asserted deterministically.
		require.Eventually(t, func() bool { return state(1) == "running" }, time.Second, time.Millisecond)
		return true, nil
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, entry, history)
	require.Equal(t, "running", state(1))
	_, err = db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET after_sdr=true WHERE task_id_sdr=1`)
	require.NoError(t, err)
	require.Equal(t, "invalid-reference", state(1), "session restoration must not relabel completed SDR references")
	_, err = db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET after_sdr=false,failed=true WHERE task_id_sdr=1`)
	require.NoError(t, err)
	require.Equal(t, "invalid-reference", state(1))
	_, err = db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET failed=false WHERE task_id_sdr=1`)
	require.NoError(t, err)
	require.Equal(t, "running", state(1))
	var afterPosted time.Time
	require.NoError(t, db.QueryRow(ctx, `SELECT posted_time FROM harmony_task WHERE id=1`).Scan(&afterPosted))
	require.Equal(t, posted, afterPosted)
	// Same owner, different process: registration replaces only machine
	// provenance, never backfills current tasks or repeats engine registration.
	next := register()
	require.Equal(t, r.MachineID, next.MachineID)
	require.NotEqual(t, r.ProcessSession, next.ProcessSession)
	require.Equal(t, "previous-process", state(1))
	old := store(r, 2)
	require.Error(t, old.prepare(ctx, 2, "delayed-old-process"))
	_, err = db.Exec(ctx, `UPDATE harmony_machines SET last_contact=CURRENT_TIMESTAMP WHERE id=$1`, next.MachineID)
	require.NoError(t, err)
	var session string
	require.NoError(t, db.QueryRow(ctx, `SELECT process_session FROM harmony_machines WHERE id=$1`, next.MachineID).Scan(&session))
	require.Equal(t, next.ProcessSession, session)
	_, err = db.Exec(ctx, `UPDATE harmony_task SET owner_generation=owner_generation+1 WHERE id=1`)
	require.NoError(t, err)
	require.Equal(t, "unknown", state(1))
	current := store(next, 1)
	require.NoError(t, current.prepare(ctx, 1, "next"))
	ok, err = s.record(ctx, 1, "first", entry)
	require.NoError(t, err)
	require.False(t, ok, "late previous attempt cannot mark the new attempt running")
	require.Equal(t, "awaiting-start", state(1))
	current.token = "next"
	require.NoError(t, current.releaseUnstarted(ctx, 1))
	require.Equal(t, "pending", state(1))
	_, err = db.Exec(ctx, `UPDATE harmony_task SET owner_id=$1 WHERE id=1`, next.MachineID)
	require.NoError(t, err)
	current = store(next, 1)
	require.NoError(t, current.prepare(ctx, 1, "retry"))
	ok, err = current.record(ctx, 1, "retry", entry)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "running", state(1))
	// Non-SDR tasks use exactly the same session/Do-entry contract.
	other := store(next, 2)
	require.NoError(t, other.prepare(ctx, 2, "other"))
	ok, err = other.record(ctx, 2, "other", entry)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "running", state(2))
	// Real old registration shape clears process provenance; its attempts
	// remain executable but the new UI must conservatively show Unknown.
	_, err = db.Exec(ctx, `UPDATE harmony_machines SET cpu=4,ram=1024,gpu=0 WHERE id=$1`, next.MachineID)
	require.NoError(t, err)
	require.Equal(t, "unknown", state(1))
	require.Equal(t, "unknown", state(2))
	_, err = db.Exec(ctx, `UPDATE harmony_task SET owner_generation=owner_generation+1 WHERE id=2`)
	require.NoError(t, err)
	legacy := store(next, 2)
	legacy.session = ""
	require.NoError(t, legacy.prepare(ctx, 2, "legacy"))
	ok, err = legacy.record(ctx, 2, "legacy", entry)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "unknown", state(2))
}

// A cancelled recording must not invent a timestamp/session or release the
// current task. The existing bounded writer/lifecycle tests cover dispatch.
func TestProcessTelemetryCancelledRecord(t *testing.T) {
	ctx, db, _, _ := retrySQLFixture(t)
	_, err := db.Exec(ctx, `UPDATE harmony_machines SET process_session='p' WHERE id=101;
	 INSERT INTO harmony_task(id,name,owner_id,added_by,posted_time) VALUES(1,'Synthetic',101,101,CURRENT_TIMESTAMP)`)
	require.NoError(t, err)
	s := harmonyTaskAttemptStore{db: db, owner: 101, session: "p", generations: map[TaskID]int64{1: 0}}
	require.NoError(t, s.prepare(ctx, 1, "token"))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	ok, err := s.record(cancelled, 1, "token", time.Now())
	require.Error(t, err)
	require.False(t, ok)
	var preserved bool
	require.NoError(t, db.QueryRow(ctx, `SELECT attempt_started_at IS NULL AND attempt_start_source='prepared' AND attempt_session='p' AND owner_id=101 FROM harmony_task WHERE id=1`).Scan(&preserved))
	require.True(t, preserved)
}

// Optional red control: execute the exact baseline preparation SQL instead of
// the restored writer against the SAME new UI predicate and schema. This is a
// missing-provenance reproduction, not a native scheduler/fleet reproduction.
func TestProcessTelemetryConsumerRegression(t *testing.T) {
	ctx, db, _, conn := retrySQLFixture(t)
	_, err := db.Exec(ctx, `UPDATE harmony_machines SET process_session='p' WHERE id=101;
	 INSERT INTO harmony_task(id,name,owner_id,added_by,posted_time) VALUES(1,'Synthetic',101,101,CURRENT_TIMESTAMP)`)
	require.NoError(t, err)
	s := harmonyTaskAttemptStore{db: db, owner: 101, session: "p", generations: map[TaskID]int64{1: 0}}
	if os.Getenv("CURIO_TELEMETRY_BASELINE_RED") == "1" {
		b, err := os.ReadFile("testdata/4ae-attempt_start.go.txt")
		require.NoError(t, err)
		_, tail, ok := strings.Cut(string(b), "const PREPARE_TASK_ATTEMPT = `")
		require.True(t, ok)
		query, _, ok := strings.Cut(tail, "`")
		require.True(t, ok)
		_, err = conn.Exec(ctx, query, "token", 1, 101, 0)
		require.NoError(t, err)
	} else {
		require.NoError(t, s.prepare(ctx, 1, "token"))
	}
	ok, err := s.record(ctx, 1, "token", time.Now().Add(-time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	var state string
	err = conn.QueryRow(ctx, `WITH rows AS (SELECT t.*,m.process_session,0::bigint AS sdr_references,false AS sdr_ready,CURRENT_TIMESTAMP AS observed_at
	 FROM harmony_task t JOIN harmony_machines m ON m.id=t.owner_id WHERE t.id=1) SELECT `+deployedTookSQL(t)+` FROM rows`).Scan(&state)
	require.NoError(t, err)
	require.Equal(t, "running", state, "current Do entry must be recognized by the deployed UI")
}
