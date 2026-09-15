//go:build integration && !skiff

package webrpc

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

func clusterTaskSQLFixture(t *testing.T) (*harmonydb.DB, *pgx.Conn, context.Context) {
	t.Helper()
	if os.Getenv("CURIO_TASK_ATTEMPT_ITEST") != "1" {
		t.Skip("requires explicit CURIO_TASK_ATTEMPT_ITEST=1 and a disposable loopback target")
	}
	const prefix = "CURIO_TASK_ATTEMPT_ITEST_"
	host, port := os.Getenv(prefix+"HOST"), os.Getenv(prefix+"PORT")
	address := net.ParseIP(host)
	require.True(t, address != nil && host == "127.0.0.1", "HOST must be literal 127.0.0.1")
	n, err := strconv.ParseUint(port, 10, 16)
	require.NoError(t, err)
	require.NotZero(t, n)
	database, user := os.Getenv(prefix+"DATABASE"), os.Getenv(prefix+"USER")
	require.NotEmpty(t, strings.TrimSpace(database))
	require.NotEmpty(t, strings.TrimSpace(user))
	opts := harmonydb.ItestOptions{Hosts: []string{host}, Port: port, Database: database, Username: user,
		Password: os.Getenv(prefix + "PASSWORD"), ITestID: harmonydb.ITestNewID()}
	cfg := opts.HarmonyConfig()
	cfg.ReadOnly, cfg.LoadBalance = true, false
	db, err := harmonydb.NewFromConfig(cfg)
	require.NoError(t, err)
	t.Cleanup(db.ITestDeleteAll)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pcfg, err := pgx.ParseConfig("postgresql://placeholder@127.0.0.1/placeholder?sslmode=disable&load_balance=false")
	require.NoError(t, err)
	pcfg.Host, pcfg.Port, pcfg.Database, pcfg.User, pcfg.Password = host, uint16(n), database, user, opts.Password
	pcfg.Fallbacks, pcfg.ConnectTimeout = nil, 5*time.Second
	pcfg.RuntimeParams = map[string]string{"search_path": "itest_" + string(opts.ITestID), "statement_timeout": "5000", "lock_timeout": "2000"}
	conn, err := pgx.ConnectConfig(ctx, pcfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		require.NoError(t, conn.Close(closeCtx))
	})
	_, path, _, ok := runtime.Caller(0)
	require.True(t, ok)
	for _, name := range []string{"20230719-harmony.sql", "20260909-task-ownership-age.sql", "20260909-task-attempt-start.sql"} {
		contents, err := os.ReadFile(filepath.Join(filepath.Dir(path), "..", "..", "..", "harmony", "harmonydb", "sql", name))
		require.NoError(t, err)
		_, err = conn.Exec(ctx, string(contents))
		require.NoError(t, err)
	}
	_, err = db.Exec(ctx, `INSERT INTO harmony_machines (id,host_and_port,cpu,ram,gpu) VALUES (101,'worker.example:12300',8,1024,0)`)
	require.NoError(t, err)
	var schema string
	require.NoError(t, conn.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema))
	require.Equal(t, "itest_"+string(opts.ITestID), schema)
	var schemas []struct {
		Schema string `db:"schema"`
	}
	require.NoError(t, db.Select(ctx, &schemas, `SELECT current_schema() AS schema`))
	require.Equal(t, schema, schemas[0].Schema)
	return db, conn, ctx
}

func TestClusterTaskSnapshotSQL(t *testing.T) {
	db, conn, ctx := clusterTaskSQLFixture(t)
	_, err := db.Exec(ctx, `INSERT INTO harmony_task (id,posted_time,owner_id,added_by,name)
SELECT n, CURRENT_TIMESTAMP-INTERVAL '3 hours', CASE WHEN n<=3 THEN 101 ELSE NULL END,101,'Synthetic' FROM generate_series(1,503) n`)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE harmony_task SET attempt_id='current',attempt_started_at=CURRENT_TIMESTAMP-INTERVAL '10 minutes',attempt_start_source='do_entry' WHERE id=1`)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE harmony_task SET attempt_start_source=NULL WHERE id=3`)
	require.NoError(t, err)
	source := harmonyClusterTaskSummarySource{db: db}
	applied := ClusterTaskSummaryApplied{MaxTasks: 5, MaxPending: 2}
	started := time.Now()
	snapshot, err := source.LoadSnapshot(ctx, applied)
	require.NoError(t, err)
	t.Logf("bounded snapshot wall=%s returned=%d running_total=%d pending_total=%d; row limit is not a scanned-row bound", time.Since(started), len(snapshot.Rows), snapshot.RunningTotal, snapshot.PendingTotal)
	require.Len(t, snapshot.Rows, 5)
	require.Equal(t, int64(3), snapshot.RunningTotal)
	require.Equal(t, int64(500), snapshot.PendingTotal)
	for i, expected := range []string{"running", "awaiting-start", "unknown", "pending", "pending"} {
		row := buildLimitedTaskSummary(snapshot.Rows[i], snapshot.ObservedAt, nil)
		require.Equal(t, expected, row.TookState)
		if i == 0 {
			require.NotNil(t, row.TookSeconds)
			require.InDelta(t, 600, *row.TookSeconds, 3)
		} else {
			require.Nil(t, row.TookSeconds)
		}
	}
	types, err := source.LoadTaskTypes(ctx, false)
	require.NoError(t, err)
	require.Len(t, types, 1)
	rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+clusterTaskSummaryLimitedQuery, clusterTaskSummaryLimitedQueryArgs(applied)...)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		t.Log(line)
	}
	require.NoError(t, rows.Err())
}

func TestClusterTaskOrderSQL(t *testing.T) {
	db, conn, ctx := clusterTaskSQLFixture(t)
	// Real task rows, not a frontend page: 510 owned + 32,108 connected queue
	// rows. Most ownership times deliberately disagree with execution age.
	_, err := conn.Exec(ctx, `INSERT INTO harmony_machines (id,host_and_port,cpu,ram,gpu)
 VALUES (102,'other.example:12300',8,1024,0);
 INSERT INTO harmony_task (id,posted_time,owner_id,added_by,name)
 SELECT n, NOW()-INTERVAL '1 day', CASE WHEN n<=510 THEN 101 ELSE NULL END,101,
 CASE WHEN n=510 THEN 'Indexing' WHEN n=509 THEN 'bg:test' ELSE 'SDR' END
 FROM generate_series(1,32618) n;
 UPDATE harmony_task SET work_start=NOW()-INTERVAL '4 hours',work_start_source='claim',
 attempt_id='a-'||id,attempt_started_at=NOW()-INTERVAL '15 minutes',attempt_start_source='do_entry' WHERE id<=510;
 UPDATE harmony_task SET owner_id=102 WHERE id=510;
 UPDATE harmony_task SET work_start=NOW()-INTERVAL '3 hours',attempt_id='long',
 attempt_started_at=NOW()-INTERVAL '3 hours',attempt_start_source='do_entry' WHERE id=510;
 UPDATE harmony_task SET attempt_start_source='prepared',attempt_started_at=NULL WHERE id=507;
 UPDATE harmony_task SET attempt_start_source=NULL WHERE id=508;`)
	require.NoError(t, err)
	source := harmonyClusterTaskSummarySource{db: db}
	applied := ClusterTaskSummaryApplied{MaxTasks: 500, MaxPending: 30}
	started := time.Now()
	snap, err := source.LoadSnapshot(ctx, applied)
	require.NoError(t, err)
	snapshotWall := time.Since(started)
	require.Len(t, snap.Rows, 500)
	require.Equal(t, int64(510), snap.Rows[0].ID, "LIMIT must select longest Took even across task type/owner/younger ownership")
	require.Equal(t, ClusterTaskSectionTotals{Running: 507, AwaitingStart: 1, Unknown: 1, Pending: 32108}, snap.SectionTotals)
	require.Equal(t, int64(509), snap.RunningTotal, "legacy owned total")
	for i, row := range snap.Rows {
		_, state := taskTookAge(row, snap.ObservedAt)
		require.Equal(t, "running", state)
		if i > 0 {
			require.Equal(t, int64(i), row.ID)
		}
	}
	response, err := buildClusterTaskSummaryLimited(ctx, ClusterTaskSummaryLimitedRequest{}, source, db, nil)
	require.NoError(t, err)
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	t.Logf("snapshot_wall=%s snapshot_plus_second_response_wall=%s rows=%d json_bytes=%d totals=%+v (bounded returned rows, not scanned rows)", snapshotWall, time.Since(started), len(snap.Rows), len(encoded), snap.SectionTotals)
	plan, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+clusterTaskSummaryLimitedQuery, clusterTaskSummaryLimitedQueryArgs(applied)...)
	require.NoError(t, err)
	for plan.Next() {
		var line string
		require.NoError(t, plan.Scan(&line))
		t.Log(line)
	}
	require.NoError(t, plan.Err())
	plan.Close()
	// Type/background filters apply before counts AND selection.
	name := "Indexing"
	applied.TaskName = &name
	snap, err = source.LoadSnapshot(ctx, applied)
	require.NoError(t, err)
	require.Len(t, snap.Rows, 1)
	require.Equal(t, int64(1), snap.SectionTotals.Running)
	require.Zero(t, snap.PendingTotal)
	applied.TaskName = nil
	applied.IncludeBackground = true
	snap, err = source.LoadSnapshot(ctx, applied)
	require.NoError(t, err)
	require.Equal(t, int64(508), snap.SectionTotals.Running)
	// Release the selected running subset inside the owned fixture only. The
	// remaining sections get space, with the same total display cap.
	_, err = conn.Exec(ctx, `DELETE FROM harmony_task WHERE id<=506`)
	require.NoError(t, err)
	applied.IncludeBackground = false
	applied.MaxTasks = 5
	applied.MaxPending = 30
	snap, err = source.LoadSnapshot(ctx, applied)
	require.NoError(t, err)
	require.Len(t, snap.Rows, 5)
	for i, state := range []string{"running", "awaiting-start", "unknown", "pending", "pending"} {
		_, got := taskTookAge(snap.Rows[i], snap.ObservedAt)
		require.Equal(t, state, got)
	}
	require.Equal(t, ClusterTaskSectionTotals{Running: 1, AwaitingStart: 1, Unknown: 1, Pending: 32108}, snap.SectionTotals)
	applied.MaxPending = 0
	snap, err = source.LoadSnapshot(ctx, applied)
	require.NoError(t, err)
	require.Len(t, snap.Rows, 3)
	require.Equal(t, int64(32108), snap.PendingTotal)
	// Each SQL classification must match the actual response helper, including
	// null token at claim, malformed provenance, future start, and zero seconds.
	_, err = conn.Exec(ctx, `DELETE FROM harmony_task;
 INSERT INTO harmony_task (id,posted_time,owner_id,added_by,name)
 SELECT n,NOW(),101,101,'SDR' FROM generate_series(1,12) n;
 UPDATE harmony_task SET attempt_id='a-'||id,attempt_started_at=NOW()-INTERVAL '1 day',attempt_start_source='do_entry';
 UPDATE harmony_task SET attempt_started_at=NULL,attempt_start_source='claimed',attempt_id=NULL WHERE id=2;
 UPDATE harmony_task SET attempt_started_at=NULL,attempt_start_source='prepared' WHERE id=3;
 UPDATE harmony_task SET attempt_id='' WHERE id=4;
 UPDATE harmony_task SET attempt_started_at=NULL WHERE id=5;
 UPDATE harmony_task SET attempt_start_source='backfill' WHERE id=6;
 UPDATE harmony_task SET attempt_started_at=NOW()+INTERVAL '1 day' WHERE id=7;
 UPDATE harmony_task SET attempt_start_source='claimed' WHERE id=8;
 UPDATE harmony_task SET owner_id=NULL WHERE id=9;
 UPDATE harmony_task SET attempt_start_source=NULL WHERE id=10;
 UPDATE harmony_task SET attempt_id=NULL WHERE id=11;
 UPDATE harmony_task SET attempt_started_at=NOW() WHERE id=12;`)
	require.NoError(t, err)
	applied.MaxTasks = 50
	applied.MaxPending = 30
	snap, err = source.LoadSnapshot(ctx, applied)
	require.NoError(t, err)
	require.Equal(t, ClusterTaskSectionTotals{Running: 2, AwaitingStart: 2, Unknown: 7, Pending: 1}, snap.SectionTotals)
	var sqlRows []struct {
		ID         int64     `db:"id"`
		TookState  string    `db:"took_state"`
		Seconds    *int64    `db:"seconds"`
		ObservedAt time.Time `db:"observed_at"`
	}
	require.NoError(t, db.Select(ctx, &sqlRows, `WITH classified AS (SELECT t.*,statement_timestamp() AS observed_at FROM harmony_task t), states AS (SELECT *, `+clusterTaskTookStateSQL+` AS took_state FROM classified)
 SELECT id,took_state,observed_at,CASE WHEN took_state='running' THEN FLOOR(EXTRACT(EPOCH FROM observed_at-attempt_started_at))::BIGINT END AS seconds FROM states`))
	for _, r := range sqlRows {
		for _, row := range snap.Rows {
			if row.ID == r.ID {
				seconds, state := taskTookAge(row, r.ObservedAt)
				require.Equal(t, r.TookState, state)
				require.Equal(t, r.Seconds, seconds)
			}
		}
	}
	// Retry on the same owner changes the ordering on the next snapshot.
	_, err = conn.Exec(ctx, `UPDATE harmony_task SET attempt_id='retry-'||id,attempt_started_at=NOW() WHERE id IN (1,12)`)
	require.NoError(t, err)
	snap, err = source.LoadSnapshot(ctx, applied)
	require.NoError(t, err)
	require.Equal(t, int64(1), snap.Rows[0].ID, "equal whole seconds break ties by numeric task ID")
	rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) "+clusterTaskSummaryLimitedQuery, clusterTaskSummaryLimitedQueryArgs(applied)...)
	require.NoError(t, err)
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		t.Log(line)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	_, err = conn.Exec(ctx, `DELETE FROM harmony_task`)
	require.NoError(t, err)
	snap, err = source.LoadSnapshot(ctx, applied)
	require.NoError(t, err)
	require.Empty(t, snap.Rows)
	require.Equal(t, ClusterTaskSectionTotals{}, snap.SectionTotals)
}
