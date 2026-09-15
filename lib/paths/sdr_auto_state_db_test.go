//go:build sdr_auto_itest

package paths

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

func TestSDRAutoStateRowFence(t *testing.T) {
	if os.Getenv("CURIO_SDR_RETRY_ITEST") != "1" {
		t.Skip("owned disposable DB opt-in required")
	}
	require.Equal(t, "127.0.0.1", os.Getenv("CURIO_SDR_RETRY_ITEST_HOST"))
	require.Equal(t, "curio_test_sdr_retry", os.Getenv("CURIO_SDR_RETRY_ITEST_DATABASE"))
	require.Equal(t, "curio_sdr_retry", os.Getenv("CURIO_SDR_RETRY_ITEST_USER"))
	id := harmonydb.ITestNewID()
	db, e := harmonydb.NewFromConfig(harmonydb.Config{Hosts: []string{"127.0.0.1"}, Port: os.Getenv("CURIO_SDR_RETRY_ITEST_PORT"), Database: "curio_test_sdr_retry", Username: "curio_sdr_retry", ITestID: id, UseTemplate: false, LoadBalance: false, ApplicationName: "auto-discard-fixture"})
	require.NoError(t, e)
	t.Cleanup(db.ITestDeleteAll)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var addr, schema, version string
	require.NoError(t, db.QueryRow(ctx, `SELECT host(inet_server_addr()),current_schema(),version()`).Scan(&addr, &schema, &version))
	require.Equal(t, "127.0.0.1", addr)
	require.Equal(t, "itest_"+string(id), schema)
	t.Log(version)
	_, e = db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof) VALUES(1000,42,5)`)
	require.NoError(t, e)
	index := NewDBIndex(nil, db)
	target := sdrscratch.AutoTarget{Base: filepath.Join(t.TempDir(), "cache"), Sector: "s-t01000-42"}
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- index.withSDRDiscardState(ctx, target, func(s sdrscratch.AutoStage) error {
			if !s.Allowed {
				t.Error("unexpected protected fixture")
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	updated := make(chan error, 1)
	go func() {
		_, e := db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET after_sdr=true WHERE sp_id=1000 AND sector_number=42 /* auto-discard-fence-writer */`)
		updated <- e
	}()
	if len(version) >= 10 && version[:10] == "PostgreSQL" {
		// Actual backend lock observation, not merely an apparent timeout.
		require.Eventually(t, func() bool {
			var waiting bool
			e := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE 'UPDATE sectors_sdr_pipeline%auto-discard-fence-writer%')`).Scan(&waiting)
			return e == nil && waiting
		}, 3*time.Second, 10*time.Millisecond)
	}
	close(release)
	require.NoError(t, <-done)
	require.NoError(t, <-updated)
	e = index.withSDRDiscardState(ctx, target, func(s sdrscratch.AutoStage) error {
		require.False(t, s.Allowed)
		require.Contains(t, s.Reason, "completed")
		return nil
	})
	require.NoError(t, e)
	ready, e := index.sdrExternallyReady(ctx, abi.SectorID{Miner: 1000, Number: 42}, storiface.FTCache)
	require.NoError(t, e)
	require.True(t, ready)
	ready, e = index.sdrExternallyReady(ctx, abi.SectorID{Miner: 2000, Number: 42}, storiface.FTCache)
	require.NoError(t, e)
	require.False(t, ready, "same sector number is not same miner")
	_, e = db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET after_sdr=false, after_commit_msg=true WHERE sp_id=1000 AND sector_number=42`)
	require.NoError(t, e)
	e = index.withSDRDiscardState(ctx, target, func(s sdrscratch.AutoStage) error {
		require.False(t, s.Allowed, "contradictory later flags cannot authorize deletion")
		return nil
	})
	require.NoError(t, e)
}
