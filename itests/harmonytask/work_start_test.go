package harmonytask

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/harmony/resources"
)

func waitForTaskReleased(t *testing.T, db *harmonydb.DB, taskID harmonytask.TaskID, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var ownerID sql.NullInt64
		var workStart sql.NullTime
		err := db.QueryRow(context.Background(), `SELECT owner_id, work_start FROM harmony_task WHERE id=$1`, taskID).Scan(&ownerID, &workStart)
		if err == nil && !ownerID.Valid && !workStart.Valid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for task %d release: owner=%v work_start=%v err=%v", taskID, ownerID, workStart, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestWorkStartOwnershipTrigger(t *testing.T) {
	db := getDB(t)
	ctx := context.Background()

	reg, err := resources.RegisterWithResources(db, "work-start-trigger:1000", resources.Resources{Cpu: 1, Ram: 1 << 30})
	require.NoError(t, err)
	t.Cleanup(reg.Shutdown)

	var taskID harmonytask.TaskID
	err = db.QueryRow(ctx, `
		INSERT INTO harmony_task (name, added_by, posted_time)
		VALUES ('WorkStartTrig', $1, CURRENT_TIMESTAMP)
		RETURNING id`, reg.MachineID).Scan(&taskID)
	require.NoError(t, err)

	var workStart sql.NullTime
	require.NoError(t, db.QueryRow(ctx, `SELECT work_start FROM harmony_task WHERE id=$1`, taskID).Scan(&workStart))
	require.False(t, workStart.Valid)

	_, err = db.Exec(ctx, `UPDATE harmony_task SET owner_id=$1 WHERE id=$2`, reg.MachineID, taskID)
	require.NoError(t, err)

	var ownerID sql.NullInt64
	require.NoError(t, db.QueryRow(ctx, `SELECT owner_id, work_start FROM harmony_task WHERE id=$1`, taskID).Scan(&ownerID, &workStart))
	require.Equal(t, int64(reg.MachineID), ownerID.Int64)
	require.True(t, workStart.Valid, "legacy owner-only claim must set work_start")

	_, err = db.Exec(ctx, `UPDATE harmony_task SET owner_id=NULL WHERE id=$1`, taskID)
	require.NoError(t, err)
	waitForTaskReleased(t, db, taskID, taskTimeout)
}

func TestWorkStartRecoveryResetsCurrentOwnershipInterval(t *testing.T) {
	db := getDB(t)
	ctx := context.Background()

	task := newTestTask("RecoverStart", 1)
	started := make(chan harmonytask.TaskID, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTask := func() { releaseOnce.Do(func() { close(release) }) }
	task.doFunc = func(_ context.Context, taskID harmonytask.TaskID, _ func() bool) (bool, error) {
		started <- taskID
		<-release
		task.doneCh <- taskID
		return true, nil
	}
	t.Cleanup(cleanupTasks(task))

	reg, err := resources.RegisterWithResources(db, "recover-start:1000", resources.Resources{Cpu: 1, Ram: 1 << 30})
	require.NoError(t, err)

	var taskID harmonytask.TaskID
	err = db.QueryRow(ctx, `
		INSERT INTO harmony_task (name, added_by, posted_time, owner_id)
		VALUES ($1, $2, CURRENT_TIMESTAMP - INTERVAL '2 hours', $2)
		RETURNING id`, task.name, reg.MachineID).Scan(&taskID)
	require.NoError(t, err)

	oldStart := time.Now().Add(-time.Hour).UTC()
	_, err = db.Exec(ctx, `UPDATE harmony_task SET work_start=$1 WHERE id=$2`, oldStart, taskID)
	require.NoError(t, err)

	recoveryBegan := time.Now().UTC()
	engine, err := harmonytask.NewWithReg(db, []harmonytask.TaskInterface{task}, "recover-start:1000", &deadPeerConnector{}, reg)
	require.NoError(t, err)
	t.Cleanup(engine.GracefullyTerminate)
	t.Cleanup(releaseTask)

	require.Equal(t, taskID, waitForTask(t, started, taskTimeout))

	var recoveredStart time.Time
	require.NoError(t, db.QueryRow(ctx, `SELECT work_start FROM harmony_task WHERE id=$1`, taskID).Scan(&recoveredStart))
	require.False(t, recoveredStart.Before(recoveryBegan.Add(-time.Second)), "recovery must start a new ownership interval")

	releaseTask()
	require.Equal(t, taskID, waitForTask(t, task.doneCh, taskTimeout))
	waitForHistory(t, db, taskID, taskTimeout)
}

func TestWorkStartClearedWhenRecoveredTaskTypeIsUnavailable(t *testing.T) {
	db := getDB(t)
	ctx := context.Background()

	reg, err := resources.RegisterWithResources(db, "recover-missing:1000", resources.Resources{Cpu: 1, Ram: 1 << 30})
	require.NoError(t, err)

	var taskID harmonytask.TaskID
	err = db.QueryRow(ctx, `
		INSERT INTO harmony_task (name, added_by, posted_time, owner_id)
		VALUES ('MissingRecover', $1, CURRENT_TIMESTAMP, $1)
		RETURNING id`, reg.MachineID).Scan(&taskID)
	require.NoError(t, err)

	engine, err := harmonytask.NewWithReg(db, nil, "recover-missing:1000", &deadPeerConnector{}, reg)
	require.NoError(t, err)
	t.Cleanup(engine.GracefullyTerminate)

	waitForTaskReleased(t, db, taskID, taskTimeout)
}

func TestWorkStartClaimAndRetry(t *testing.T) {
	db := getDB(t)
	ctx := context.Background()

	task := newTestTask("WorkStartRetry", 1)
	task.maxFail = 2
	task.retryWait = func(int) time.Duration { return time.Minute }
	started := make(chan harmonytask.TaskID, 1)
	fail := make(chan struct{})
	var failOnce sync.Once
	failTask := func() { failOnce.Do(func() { close(fail) }) }
	task.doFunc = func(_ context.Context, taskID harmonytask.TaskID, _ func() bool) (bool, error) {
		started <- taskID
		<-fail
		return false, errors.New("intentional retry")
	}
	t.Cleanup(cleanupTasks(task))

	engine := makeEngine(t, db, []harmonytask.TaskInterface{task}, "work-start-retry:1000")
	t.Cleanup(failTask)
	engine.AddTaskByName(task.name, func(harmonytask.TaskID, *harmonydb.Tx) (bool, error) { return true, nil })

	taskID := waitForTask(t, started, taskTimeout)
	var ownerID sql.NullInt64
	var workStart sql.NullTime
	require.NoError(t, db.QueryRow(ctx, `SELECT owner_id, work_start FROM harmony_task WHERE id=$1`, taskID).Scan(&ownerID, &workStart))
	require.True(t, ownerID.Valid)
	require.True(t, workStart.Valid, "claim must set work_start")

	failTask()
	waitForTaskReleased(t, db, taskID, taskTimeout)

	var retries int
	require.NoError(t, db.QueryRow(ctx, `SELECT retries FROM harmony_task WHERE id=$1`, taskID).Scan(&retries))
	require.Equal(t, 1, retries)
}

type rejectingStorage struct {
	claimed chan int
}

func (s *rejectingStorage) HasCapacity() bool { return true }

func (s *rejectingStorage) Claim(taskID int) (func() error, error) {
	s.claimed <- taskID
	return nil, errors.New("intentional storage claim failure")
}

func TestWorkStartClearedOnStorageClaimFailure(t *testing.T) {
	db := getDB(t)

	storage := &rejectingStorage{claimed: make(chan int, 1)}
	task := newTestTask("WorkStartStore", 1)
	task.storage = storage
	t.Cleanup(cleanupTasks(task))

	engine := makeEngine(t, db, []harmonytask.TaskInterface{task}, "work-start-store:1000")
	engine.AddTaskByName(task.name, func(harmonytask.TaskID, *harmonydb.Tx) (bool, error) { return true, nil })

	var taskID harmonytask.TaskID
	select {
	case claimed := <-storage.claimed:
		taskID = harmonytask.TaskID(claimed)
	case <-time.After(taskTimeout):
		t.Fatal("timed out waiting for storage claim")
	}

	waitForTaskReleased(t, db, taskID, taskTimeout)
	require.Equal(t, int32(0), atomic.LoadInt32(&task.attempts), "task must not run after storage claim failure")
}
