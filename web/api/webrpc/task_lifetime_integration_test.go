//go:build integration && !skiff

package webrpc

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClusterTaskLifetimeSQL(t *testing.T) {
	db, conn, ctx := clusterTaskSQLFixture(t)
	_, err := conn.Exec(ctx, `INSERT INTO harmony_task(id,name,posted_time,added_by,owner_id)
SELECT n,'SDR','2020-01-01',101,CASE WHEN n<6 THEN 101 END FROM generate_series(1,7) n;
UPDATE harmony_task SET attempt_id='a-'||id,attempt_started_at=NOW()-INTERVAL '43 hours',attempt_start_source='do_entry' WHERE id<6;
UPDATE harmony_task SET attempt_session='previous' WHERE id=1;
UPDATE harmony_task SET attempt_session=NULL WHERE id=2;
UPDATE harmony_task SET attempt_started_at=NOW()-INTERVAL '2 minutes' WHERE id=4;
UPDATE harmony_task SET attempt_started_at=NULL,attempt_start_source='prepared' WHERE id=5;
INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof,task_id_sdr,create_time)
SELECT 1000,id,8,id,NOW()-INTERVAL '1 day' FROM harmony_task WHERE id>=4;
ALTER TABLE harmony_task DISABLE TRIGGER harmony_task_lifetime;
UPDATE harmony_task SET created_at=NULL,queued_at=NULL WHERE id=6;
ALTER TABLE harmony_task ENABLE TRIGGER harmony_task_lifetime;`)
	require.NoError(t, err)
	source := harmonyClusterTaskSummarySource{db: db}
	snap, err := source.LoadSnapshot(ctx, ClusterTaskSummaryApplied{MaxTasks: 7, MaxPending: 2})
	require.NoError(t, err)
	require.Equal(t, ClusterTaskSectionTotals{Running: 1, AwaitingStart: 1, Unknown: 3, Pending: 2}, snap.SectionTotals)
	require.EqualValues(t, 5, snap.RunningTotal, "legacy owned count retained")
	wants := map[int64]string{1: "previous-process", 2: "unknown", 3: "unreferenced", 4: "running", 5: "awaiting-start", 6: "pending", 7: "pending"}
	for _, r := range snap.Rows {
		item := buildLimitedTaskSummary(r, snap.ObservedAt, nil)
		require.Equal(t, wants[r.ID], item.TookState)
		if r.ID == 6 {
			require.Nil(t, item.WaitingSeconds)
			require.Equal(t, "priority-before-sector", item.WaitingState)
		}
		if r.ID == 7 {
			require.NotNil(t, item.WaitingSeconds)
			require.Equal(t, "queue-entry", item.WaitingState)
		}
	}
	if out := os.Getenv("CURIO_CLUSTER_SNAPSHOT_OUTPUT"); out != "" {
		response, err := buildClusterTaskSummaryLimited(ctx, ClusterTaskSummaryLimitedRequest{}, source, db, nil)
		require.NoError(t, err)
		encoded, err := json.Marshal(response)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(out, encoded, 0600))
	}
	limited, err := source.LoadSnapshot(ctx, ClusterTaskSummaryApplied{MaxTasks: 1, MaxPending: 0})
	require.NoError(t, err)
	require.Equal(t, snap.SectionTotals, limited.SectionTotals)
	require.EqualValues(t, 4, limited.Rows[0].ID)
	// An old binary's resource registration also invalidates the process token.
	_, err = conn.Exec(ctx, `UPDATE harmony_machines SET cpu=cpu WHERE id=101`)
	require.NoError(t, err)
	snap, err = source.LoadSnapshot(ctx, ClusterTaskSummaryApplied{MaxTasks: 7, MaxPending: 2})
	require.NoError(t, err)
	require.Zero(t, snap.SectionTotals.Running)
	require.EqualValues(t, 5, snap.SectionTotals.Unknown)
	// Both SQL and enrichment failure remain errors/partial data, not orphan proof.
	_, err = conn.Exec(ctx, `ALTER TABLE sectors_sdr_pipeline RENAME TO fixture_pipeline_unavailable`)
	require.NoError(t, err)
	_, err = source.LoadSnapshot(ctx, ClusterTaskSummaryApplied{MaxTasks: 7, MaxPending: 2})
	require.Error(t, err)
}
