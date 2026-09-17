//go:build integration && !skiff

package harmonytask

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/harmony/harmonytask/internal/runregistry"
	"github.com/filecoin-project/curio/harmony/resources"
)

type retirementEngineFixture struct {
	retirementBoundaryFixture
	entries atomic.Int32
	allow   bool
	db      *harmonydb.DB
}

func (f *retirementEngineFixture) TypeDetails() TaskTypeDetails { return TaskTypeDetails{Name: "SDR"} }
func (f *retirementEngineFixture) Adder(AddTaskFunc)            {}
func (f *retirementEngineFixture) CanAccept(ids []TaskID, _ *TaskEngine) ([]TaskID, error) {
	return ids, nil
}
func (f *retirementEngineFixture) ReserveTaskStart(TaskID) (func(context.Context) error, func(), bool) {
	return nil, nil, f.allow // Simulates a pacing gate; never sleep or reserve.
}
func (f *retirementEngineFixture) Do(ctx context.Context, id TaskID, _ func() bool) (bool, error) {
	f.entries.Add(1)
	if !f.allow {
		return false, errors.New("fixture must never execute")
	}
	// Native computation is replaced; the SDR result and scheduler completion
	// still use their real database paths.
	_, err := f.db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET after_sdr=true,task_id_sdr=NULL WHERE task_id_sdr=$1`, id)
	return err == nil, err
}

func TestSDRRetirementSQLStartupDespiteCordonAndPacing(t *testing.T) {
	ctx, db, _, _ := attemptSQLFixture(t)
	_, orphan := newRetirementTask(t, ctx, db)
	_, valid := newRetirementTask(t, ctx, db)
	_, err := db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET after_sdr=true,task_id_sdr=NULL WHERE task_id_sdr=$1`, orphan.ID)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE harmony_machines SET unschedulable=true WHERE id=101`)
	require.NoError(t, err)
	impl := &retirementEngineFixture{retirementBoundaryFixture: retirementBoundaryFixture{stopped: true}}
	old := Registry["SDR"]
	Registry["SDR"] = impl
	defer func() {
		if old == nil {
			delete(Registry, "SDR")
		} else {
			Registry["SDR"] = old
		}
	}()
	e, err := NewWithReg(db, []TaskInterface{impl}, "worker-a.example:12300", nil, &resources.Reg{Resources: resources.Resources{MachineID: 101, Cpu: 8, Ram: 1024}})
	require.NoError(t, err)
	defer e.GracefullyTerminate()
	require.Eventually(t, func() bool {
		var n int
		err := db.QueryRow(ctx, `SELECT count(*) FROM harmony_task WHERE id=$1`, orphan.ID).Scan(&n)
		return err == nil && n == 0
	}, 3*time.Second, 10*time.Millisecond, "startup retirement must not wait for admission, pacing or uncordon")
	var token, session string
	require.NoError(t, db.QueryRow(ctx, `SELECT attempt_id,attempt_session FROM harmony_task WHERE id=$1`, valid.ID).Scan(&token, &session))
	require.Equal(t, valid.Token, token)
	require.Equal(t, valid.Session, session)
	var current string
	require.NoError(t, db.QueryRow(ctx, `SELECT process_session FROM harmony_machines WHERE id=101`).Scan(&current))
	require.Equal(t, e.cfg.session, current)
	require.NotEqual(t, valid.Session, current)
	require.Zero(t, impl.entries.Load())
	e.GracefullyTerminate()
	// A subsequent uncordoned restart resumes the valid task normally. Its old
	// reference is retained above; retirement never turns it into success.
	_, err = db.Exec(ctx, `UPDATE harmony_machines SET unschedulable=false WHERE id=101`)
	require.NoError(t, err)
	resumed := &retirementEngineFixture{retirementBoundaryFixture: retirementBoundaryFixture{stopped: true}, allow: true, db: db}
	Registry["SDR"] = resumed
	next, err := NewWithReg(db, []TaskInterface{resumed}, "worker-a.example:12300", nil, &resources.Reg{Resources: resources.Resources{MachineID: 101, Cpu: 8, Ram: 1024}})
	require.NoError(t, err)
	defer next.GracefullyTerminate()
	require.Eventually(t, func() bool {
		var n int
		err := db.QueryRow(ctx, `SELECT count(*) FROM harmony_task_history WHERE task_id=$1 AND result=true`, valid.ID).Scan(&n)
		return err == nil && n == 1
	}, 3*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, resumed.entries.Load())
}

type retirementBoundaryFixture struct {
	stopped bool
	err     error
}

func (b retirementBoundaryFixture) TaskExecutionIdentity() (string, error) {
	return "fixture-kernel-subtree", nil
}
func (b retirementBoundaryFixture) TaskExecutionStopped(string) (bool, error) {
	return b.stopped, b.err
}

func newRetirementTask(t *testing.T, ctx context.Context, db *harmonydb.DB) (*taskTypeHandler, sdrRetirement) {
	t.Helper()
	e := &TaskEngine{cfg: taskEngineConfig{ctx: ctx, db: db, ownerID: 101, session: "old-process"}, taskMap: map[string]*taskTypeHandler{}}
	var sector int64
	require.NoError(t, db.QueryRow(ctx, `SELECT COALESCE(max(sector_number),0)+1 FROM sectors_sdr_pipeline`).Scan(&sector))
	_, err := db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof) VALUES(1000,$1,8)`, sector)
	require.NoError(t, err)
	var id TaskID
	// The real AddTask creates and links in one transaction, same UPDATE as pollStartSDR.
	e.AddTaskByName("SDR", func(i TaskID, tx *harmonydb.Tx) (bool, error) {
		id = i
		n, err := tx.Exec(`UPDATE sectors_sdr_pipeline SET task_id_sdr=$1 WHERE sp_id=1000 AND sector_number=$2 AND task_id_sdr IS NULL`, i, sector)
		return n == 1, err
	})
	require.NotZero(t, id)
	h := &taskTypeHandler{TaskEngine: e, TaskTypeDetails: TaskTypeDetails{Name: "SDR"}, running: runregistry.New()}
	gens := map[TaskID]int64{}
	var snapshot task
	require.NoError(t, db.QueryRow(ctx, `SELECT id,posted_time,update_time,retries FROM harmony_task WHERE id=$1`, id).Scan(&snapshot.ID, &snapshot.PostedTime, &snapshot.UpdateTime, &snapshot.Retries))
	ids, err := h.claimTaskOwnership([]TaskID{id}, 1, gens, snapshot)
	require.NoError(t, err)
	require.Equal(t, []TaskID{id}, ids)
	store := harmonyTaskAttemptStore{db: db, owner: 101, generations: gens, session: "old-process", execution: retirementBoundaryFixture{}.TaskExecutionIdentity}
	require.NoError(t, store.prepare(ctx, id, "old-attempt"))
	ok, err := store.record(ctx, id, "old-attempt", time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	e.cfg.session = "new-process"         // same machine ID, separate in-memory registry
	e.atomics.yieldBackground.Store(true) // cleanup must not depend on uncordon
	return h, sdrRetirement{id, 101, gens[id], "old-attempt", "old-process", "fixture-kernel-subtree"}
}

func TestSDRRetirementSQLLifecycle(t *testing.T) {
	ctx, db, _, conn := attemptSQLFixture(t)
	h, r := newRetirementTask(t, ctx, db)
	ended := retirementBoundaryFixture{stopped: true}
	ok, err := h.retireStoppedSDR(ctx, r, ended)
	require.NoError(t, err)
	require.False(t, ok, "valid recovery protected")
	for _, column := range []string{"failed", "after_sdr"} {
		_, err = conn.Exec(ctx, `UPDATE sectors_sdr_pipeline SET `+column+`=true WHERE task_id_sdr=$1`, r.ID)
		require.NoError(t, err)
		ok, err = h.retireStoppedSDR(ctx, r, ended)
		require.NoError(t, err)
		require.False(t, ok, "even failed/completed links are not severed")
	}
	// The actual SDR success write clears task_id_sdr before scheduler completion.
	_, err = db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET after_sdr=true,task_id_sdr=NULL WHERE task_id_sdr=$1`, r.ID)
	require.NoError(t, err)
	ok, err = h.retireStoppedSDR(ctx, r, retirementBoundaryFixture{})
	require.NoError(t, err)
	require.False(t, ok, "no native termination proof")
	_, err = h.retireStoppedSDR(ctx, r, retirementBoundaryFixture{err: errors.New("kernel inspection denied")})
	require.Error(t, err)
	handle := h.running.Start(int64(r.ID), func() {})
	ok, err = h.retireStoppedSDR(ctx, r, ended)
	require.NoError(t, err)
	require.False(t, ok, "current run registry wins")
	h.running.FinishHandle(handle)
	ok, err = h.retireStoppedSDR(ctx, r, ended)
	require.NoError(t, err)
	require.True(t, ok)
	var count int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM harmony_task WHERE id=$1`, r.ID).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM harmony_task_history WHERE task_id=$1`, r.ID).Scan(&count))
	require.Zero(t, count, "not a fabricated success")
	var original string
	require.NoError(t, db.QueryRow(ctx, `SELECT original_task->>'attempt_id' FROM harmony_sdr_task_retirements WHERE task_id=$1`, r.ID).Scan(&original))
	require.Equal(t, r.Token, original)
	_, err = db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET task_id_sdr=$1 WHERE sp_id=1000`, r.ID)
	require.ErrorContains(t, err, "retired SDR")
}

func TestSDRRetirementSQLAcquisitionAndRollback(t *testing.T) {
	ctx, db, _, conn := attemptSQLFixture(t)
	for _, change := range []string{"owner", "generation", "token", "session", "execution", "name", "db-error"} {
		t.Run(change, func(t *testing.T) {
			h, r := newRetirementTask(t, ctx, db)
			_, err := db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET task_id_sdr=NULL WHERE task_id_sdr=$1`, r.ID)
			require.NoError(t, err)
			q := map[string]string{"owner": "owner_id=102", "generation": "owner_generation=owner_generation+1", "token": "attempt_id='new'", "session": "attempt_session='new'", "execution": "sdr_execution='other'", "name": "name='SDRKeyRegen'"}[change]
			if change != "db-error" {
				_, err = conn.Exec(ctx, `UPDATE harmony_task SET `+q+` WHERE id=$1`, r.ID)
				require.NoError(t, err)
			}
			c := ctx
			if change == "db-error" {
				var cancel context.CancelFunc
				c, cancel = context.WithCancel(ctx)
				cancel()
			}
			ok, err := h.retireStoppedSDR(c, r, retirementBoundaryFixture{stopped: true})
			require.False(t, ok)
			if change == "db-error" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			var count int
			require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM harmony_task WHERE id=$1`, r.ID).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
	h, r := newRetirementTask(t, ctx, db)
	_, err := db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET task_id_sdr=NULL WHERE task_id_sdr=$1`, r.ID)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `CREATE FUNCTION reject_retirement_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture delete failure'; END $$;
CREATE TRIGGER reject_retirement_fixture BEFORE DELETE ON harmony_task FOR EACH ROW EXECUTE FUNCTION reject_retirement_fixture()`)
	require.NoError(t, err)
	ok, err := h.retireStoppedSDR(ctx, r, retirementBoundaryFixture{stopped: true})
	require.ErrorContains(t, err, "fixture delete failure")
	require.False(t, ok)
	var count int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM harmony_sdr_task_retirements WHERE task_id=$1`, r.ID).Scan(&count))
	require.Zero(t, count, "audit insertion rolls back with deletion")
}

func TestSDRRetirementSQLReconnectAndClaimRace(t *testing.T) {
	ctx, db, observer, conn := attemptSQLFixture(t)
	for _, action := range []string{"reconnect", "reclaim"} {
		t.Run(action, func(t *testing.T) {
			h, r := newRetirementTask(t, ctx, db)
			var sector int64
			require.NoError(t, db.QueryRow(ctx, `SELECT sector_number FROM sectors_sdr_pipeline WHERE task_id_sdr=$1`, r.ID).Scan(&sector))
			_, err := db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET task_id_sdr=NULL WHERE task_id_sdr=$1`, r.ID)
			require.NoError(t, err)
			tx, err := conn.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(context.Background()) }()
			if action == "reconnect" {
				_, err = tx.Exec(ctx, `UPDATE sectors_sdr_pipeline SET task_id_sdr=$1 WHERE sp_id=1000 AND sector_number=$2`, r.ID, sector)
			} else {
				_, err = tx.Exec(ctx, `UPDATE harmony_task SET owner_generation=owner_generation+1 WHERE id=$1`, r.ID)
			}
			require.NoError(t, err)
			type result struct {
				ok  bool
				err error
			}
			done := make(chan result, 1)
			go func() {
				ok, err := h.retireStoppedSDR(ctx, r, retirementBoundaryFixture{stopped: true})
				done <- result{ok, err}
			}()
			require.Eventually(t, func() bool {
				var linked bool
				err := observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock' AND query LIKE 'SELECT to_jsonb(t)%')`, conn.PgConn().PID()).Scan(&linked)
				return err == nil && linked
			}, time.Second, 5*time.Millisecond, "actual retirement lock wait not observed")
			require.NoError(t, tx.Commit(ctx))
			select {
			case result := <-done:
				require.NoError(t, result.err)
				require.False(t, result.ok)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var count int
			require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM harmony_task WHERE id=$1`, r.ID).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}

func TestSDRRetirementSQLWinsReconnect(t *testing.T) {
	ctx, db, other, conn := attemptSQLFixture(t)
	h, r := newRetirementTask(t, ctx, db)
	_, err := db.Exec(ctx, `UPDATE sectors_sdr_pipeline SET task_id_sdr=NULL WHERE task_id_sdr=$1`, r.ID)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `CREATE FUNCTION retirement_barrier_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(716019); RETURN NEW; END $$;
CREATE TRIGGER retirement_barrier_fixture BEFORE INSERT ON harmony_sdr_task_retirements FOR EACH ROW EXECUTE FUNCTION retirement_barrier_fixture()`)
	require.NoError(t, err)
	lock, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = lock.Rollback(context.Background()) }()
	_, err = lock.Exec(ctx, `SELECT pg_advisory_xact_lock(716019)`)
	require.NoError(t, err)
	retired := make(chan error, 1)
	go func() {
		ok, e := h.retireStoppedSDR(ctx, r, retirementBoundaryFixture{stopped: true})
		if e == nil && !ok {
			e = errors.New("expected retirement")
		}
		retired <- e
	}()
	var retirementPID int
	require.Eventually(t, func() bool {
		return other.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE 'INSERT INTO harmony_sdr_task_retirements%'`, conn.PgConn().PID()).Scan(&retirementPID) == nil
	}, time.Second, 5*time.Millisecond)
	linked := make(chan error, 1)
	go func() {
		_, e := other.Exec(ctx, `UPDATE sectors_sdr_pipeline SET task_id_sdr=$1 WHERE sp_id=1000`, r.ID)
		linked <- e
	}()
	require.Eventually(t, func() bool {
		var blocked bool
		e := lock.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, retirementPID).Scan(&blocked)
		return e == nil && blocked
	}, time.Second, 5*time.Millisecond, "reconnection trigger must actually wait for retirement")
	require.NoError(t, lock.Commit(ctx))
	require.NoError(t, <-retired)
	require.ErrorContains(t, <-linked, "retired SDR")
}

func TestSDRRetirementSQLProducerTransaction(t *testing.T) {
	ctx, db, other, _ := attemptSQLFixture(t)
	e := &TaskEngine{cfg: taskEngineConfig{ctx: ctx, db: db, ownerID: 101}, taskMap: map[string]*taskTypeHandler{}}
	inserted := make(chan TaskID, 1)
	release := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		e.AddTaskByName("SDR", func(id TaskID, tx *harmonydb.Tx) (bool, error) {
			inserted <- id
			<-release
			_, err := tx.Exec(`INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof,task_id_sdr) VALUES(1000,1,8,$1)`, id)
			return err == nil, err
		})
	}()
	defer func() { close(release); <-finished }()
	id := <-inserted
	var count int
	require.NoError(t, other.QueryRow(ctx, `SELECT count(*) FROM harmony_task WHERE id=$1`, id).Scan(&count))
	require.Zero(t, count, "uncommitted AddTask is not a visible orphan")
	h := &taskTypeHandler{TaskEngine: &TaskEngine{cfg: taskEngineConfig{ctx: ctx, db: other, ownerID: 101, session: "new"}}, TaskTypeDetails: TaskTypeDetails{Name: "SDR"}, running: runregistry.New()}
	ok, err := h.retireStoppedSDR(ctx, sdrRetirement{id, 101, 0, "old", "old", "fixture"}, retirementBoundaryFixture{stopped: true})
	require.NoError(t, err)
	require.False(t, ok)
}

func TestTaskLifetimeSQLQueueProvenance(t *testing.T) {
	ctx, db, _, _ := attemptSQLFixture(t)
	_, err := db.Exec(ctx, `INSERT INTO harmony_task(id,name,posted_time,added_by) VALUES(91,'SDR','2020-01-01',101)`)
	require.NoError(t, err)
	var created, queued, posted time.Time
	require.NoError(t, db.QueryRow(ctx, `SELECT created_at,queued_at,posted_time FROM harmony_task WHERE id=91`).Scan(&created, &queued, &posted))
	require.True(t, created.Equal(queued))
	require.Greater(t, created.Year(), posted.Year())
	_, err = db.Exec(ctx, `UPDATE harmony_task SET posted_time='2019-01-01',created_at='2018-01-01',queued_at='2018-01-01' WHERE id=91`)
	require.NoError(t, err)
	var c, q time.Time
	require.NoError(t, db.QueryRow(ctx, `SELECT created_at,queued_at FROM harmony_task WHERE id=91`).Scan(&c, &q))
	require.True(t, c.Equal(created))
	require.True(t, q.Equal(queued))
	_, err = db.Exec(ctx, `UPDATE harmony_task SET owner_id=101 WHERE id=91`)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE harmony_task SET owner_id=NULL,retries=1 WHERE id=91`)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(ctx, `SELECT created_at,queued_at,posted_time FROM harmony_task WHERE id=91`).Scan(&c, &q, &posted))
	require.True(t, c.Equal(created))
	require.True(t, q.After(queued))
	require.Equal(t, 2019, posted.Year(), "retry queue stamp must not rewrite FIFO key")
}
