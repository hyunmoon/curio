//go:build integration

package harmonydb

import (
	"embed"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/harmony/harmonydb/testdata/lifetime"
)

// The real pre-restore startup, including the acquisition fence.
//
//go:embed sql/202[3-5]*.sql sql/20260[1-8]*.sql sql/20260906*.sql sql/20260909*.sql sql/20260910*.sql sql/20260912*.sql
var preProcessStartupFS embed.FS

func TestProcessTelemetryRunnerCompatibility(t *testing.T) {
	for _, scenario := range []string{"fresh", "baseline", "lifetime"} {
		t.Run(scenario, func(t *testing.T) {
			f := newTaskMigrationFixture(t)
			if scenario == "fresh" {
				f.startup(t, nil)
			} else {
				f.startup(t, &preProcessStartupFS)
				if scenario == "lifetime" {
					f.startup(t, &lifetime.SQL)
				}
			}
			f.seed(t, true, true)
			if scenario == "lifetime" {
				_, err := f.conn.Exec(f.ctx, `UPDATE harmony_machines SET process_session='existing-process' WHERE id=101;
					UPDATE harmony_task SET attempt_session='existing-process',sdr_execution='existing-boundary' WHERE id=1;
					INSERT INTO harmony_sdr_task_retirements(task_id,reason,original_task) VALUES(900,'test historical retirement','{}')`)
				require.NoError(t, err)
			}
			tasks, ledger := f.snapshot(t, "harmony_task"), f.snapshot(t, "base")
			f.startup(t, nil)
			f.assertPreserved(t, tasks, ledger)
			var missing int
			require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT count(*) FROM harmony_task WHERE attempt_session IS NULL`).Scan(&missing))
			if scenario == "lifetime" {
				require.Equal(t, 2, missing)
				var session string
				require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT process_session FROM harmony_machines WHERE id=101`).Scan(&session))
				require.Equal(t, "existing-process", session)
				var retained int
				require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT count(*) FROM harmony_sdr_task_retirements WHERE task_id=900 AND reason='test historical retirement'`).Scan(&retained))
				require.Equal(t, 1, retained)
				require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT count(*) FROM pg_trigger WHERE tgname IN ('harmony_sdr_reference_guard','harmony_task_lifetime') AND tgrelid IN ('harmony_task'::regclass,'sectors_sdr_pipeline'::regclass)`).Scan(&retained))
				require.Equal(t, 2, retained)
			} else {
				require.Equal(t, 3, missing, "never fabricate provenance for pre-existing rows")
				var absent bool
				require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT to_regclass('harmony_sdr_task_retirements') IS NULL`).Scan(&absent))
				require.True(t, absent, "telemetry must not install retirement policy")
			}
			stable, applied := f.snapshot(t, "harmony_task"), f.snapshot(t, "base")
			f.startup(t, nil)
			require.Equal(t, stable, f.snapshot(t, "harmony_task"))
			require.Equal(t, applied, f.snapshot(t, "base"))
			// The pinned runner commits SQL before its ledger INSERT. An exact
			// SQL replay must also preserve rows if that INSERT was interrupted.
			sql, err := upgradeFS.ReadFile("sql/20260925-task-process-telemetry.sql")
			require.NoError(t, err)
			_, err = f.conn.Exec(f.ctx, string(sql))
			require.NoError(t, err)
			require.Equal(t, stable, f.snapshot(t, "harmony_task"))
			_, err = f.conn.Exec(f.ctx, `UPDATE harmony_task SET owner_generation=owner_generation+1 WHERE id=1`)
			require.NoError(t, err)
			var cleared bool
			require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT attempt_session IS NULL AND attempt_id IS NULL AND attempt_started_at IS NULL FROM harmony_task WHERE id=1`).Scan(&cleared))
			require.True(t, cleared)
		})
	}
}

func TestProcessTelemetryRunnerRejectsIncompatibleSchema(t *testing.T) {
	f := newTaskMigrationFixture(t)
	f.startup(t, &preProcessStartupFS)
	_, err := f.conn.Exec(f.ctx, `ALTER TABLE harmony_task ADD COLUMN attempt_session BIGINT`)
	require.NoError(t, err)
	_, err = NewFromConfig(f.cfg)
	require.ErrorContains(t, err, "incompatible task process telemetry")
	var applied int
	require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT count(*) FROM base WHERE entry='20260925'`).Scan(&applied))
	require.Zero(t, applied)
	// Repair only this deliberately malformed disposable fixture, not a ledger.
	_, err = f.conn.Exec(f.ctx, `ALTER TABLE harmony_task ALTER COLUMN attempt_session TYPE TEXT USING attempt_session::text`)
	require.NoError(t, err)
	f.startup(t, nil)
	require.NoError(t, f.conn.QueryRow(f.ctx, `SELECT count(*) FROM base WHERE entry='20260925'`).Scan(&applied))
	require.Equal(t, 1, applied)
}
