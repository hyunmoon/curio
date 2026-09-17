//go:build integration

package harmonydb

import (
	"embed"
	"testing"

	"github.com/stretchr/testify/require"
)

// All startup inputs from the reviewed pre-lifetime parent, unchanged bytes.
// No manually inserted ledger rows and no direct execution of migration SQL.
//
//go:embed sql/202[3-5]*.sql sql/20260[1-8]*.sql sql/20260906*.sql sql/20260909*.sql sql/20260910*.sql sql/20260912*.sql
var preLifetimeStartupFS embed.FS

func TestTaskLifetimeRunnerPreservesLegacyAndRestarts(t *testing.T) {
	f := newTaskMigrationFixture(t)
	f.startup(t, &preLifetimeStartupFS)
	f.seed(t, true, true)
	tasks, ledger := f.snapshot(t, "harmony_task"), f.snapshot(t, "base")
	var machineBefore string
	require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT to_jsonb(m)::text FROM harmony_machines m WHERE id=101`).Scan(&machineBefore))
	f.startup(t, nil)
	f.assertPreserved(t, tasks, ledger)
	for _, row := range f.snapshot(t, "harmony_task") {
		for _, field := range []string{"created_at", "queued_at", "attempt_session", "sdr_execution"} {
			require.Nil(t, row[field], "no fabricated legacy %s", field)
		}
	}
	var machineAfter string
	require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT (to_jsonb(m)-'process_session')::text FROM harmony_machines m WHERE id=101`).Scan(&machineAfter))
	require.JSONEq(t, machineBefore, machineAfter)
	var unknown bool
	require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT process_session IS NULL FROM harmony_machines WHERE id=101`).Scan(&unknown))
	require.True(t, unknown)
	var applied int
	require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT count(*) FROM base WHERE entry='20260917'`).Scan(&applied))
	require.Equal(t, 1, applied)

	// Repeated new startup and return to the old migration set leave additive
	// schema and retirement tombstones in place, not a destructive downgrade.
	_, err := f.conn.Exec(f.ctx, `INSERT INTO harmony_sdr_task_retirements(task_id,reason,original_task) VALUES(900,'fixture retained tombstone','{}')`)
	require.NoError(t, err)
	beforeRestart := f.snapshot(t, "harmony_task")
	ledgerRestart := f.snapshot(t, "base")
	f.startup(t, nil)
	f.startup(t, &preLifetimeStartupFS)
	require.Equal(t, beforeRestart, f.snapshot(t, "harmony_task"))
	require.Equal(t, ledgerRestart, f.snapshot(t, "base"))
	require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT count(*) FROM harmony_sdr_task_retirements WHERE task_id=900`).Scan(&applied))
	require.Equal(t, 1, applied)
	t.Log("real runner: old -> new -> new restart -> old startup; legacy rows and ledger unchanged, provenance NULL, tombstone retained")
}
