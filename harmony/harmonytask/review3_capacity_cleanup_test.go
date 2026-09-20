package harmonytask

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/harmony/taskhelp"
)

// Independently reconstructed from Review3; these run real admissions and
// runregistry with a fake DB store/storage, not a native or filesystem test.
type review3CleanupStorage struct{ release func() error }

func (*review3CleanupStorage) HasCapacity() bool { return true }
func (*review3CleanupStorage) Claim(int) (func() error, error) {
	return nil, errors.New("unexpected legacy claim")
}
func (s *review3CleanupStorage) PrepareStorageClaim(context.Context, int) (func() error, bool, error) {
	return s.release, true, nil
}

func TestReview3PreparedCancellationMustNotBlockScheduler(t *testing.T) {
	e, cancel := newAdmissionEngine(t, 2)
	defer cancel()
	finish := make(chan struct{})
	defer close(finish)
	h, _ := addAdmissionHandler(e, "slow-release", taskhelp.Max(1), newMemoryAttemptStore(), finish)
	blocked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.Cost.Storage = &review3CleanupStorage{release: func() error { once.Do(func() { close(blocked) }); <-release; return nil }}
	require.True(t, h.considerWork(workSourcePoller, []task{{ID: 1}}, eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}))
	a := h.admissions[1]
	require.Eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ready }, 2*time.Second, time.Millisecond)
	e.atomics.draining.Store(true)
	drained := make(chan struct{})
	go func() { h.drainAdmissions(); close(drained) }()
	defer func() { close(release); receiveAdmission(t, drained); receiveAdmission(t, a.workerDone) }()
	receiveAdmission(t, blocked)
	select {
	case <-drained:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("scheduler waiting for prepared storage cleanup I/O")
	}
	require.Zero(t, h.Max.Active())
	require.Empty(t, h.running.Snapshot())
	e.atomics.draining.Store(false)
	other, fast := addAdmissionHandler(e, "fast", taskhelp.Max(1), newMemoryAttemptStore(), finish)
	require.True(t, other.considerWork(workSourcePoller, []task{{ID: 2}}, eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}))
	settleAdmissions(t, other)
	require.Equal(t, TaskID(2), receiveAdmission(t, fast.entered), "another actual dispatch must enter with cleanup still blocked")
}

func TestReview3UnknownPreparedCleanupMustRemainTracked(t *testing.T) {
	e, cancel := newAdmissionEngine(t, 2)
	defer cancel()
	finish := make(chan struct{})
	defer close(finish)
	h, _ := addAdmissionHandler(e, "unknown-release", taskhelp.Max(1), newMemoryAttemptStore(), finish)
	h.Cost.Storage = &review3CleanupStorage{release: func() error { return errors.New("Cancel publication outcome unknown") }}
	require.True(t, h.considerWork(workSourcePoller, []task{{ID: 1}}, eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}))
	a := h.admissions[1]
	require.Eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ready }, 2*time.Second, time.Millisecond)
	a.handle.CancelPending()
	receiveAdmission(t, a.workerDone)
	h.drainAdmissions()
	a.mu.Lock()
	quarantined, storageErr := a.quarantined, a.storageError
	a.mu.Unlock()
	require.True(t, quarantined, "cleanup outcome lost after successful DB release")
	require.Error(t, storageErr)
	require.Same(t, a, h.admissions[1])
	require.Zero(t, h.Max.Active(), "storage uncertainty must not retain execution slots")
}

func TestReview3CleanupRecoveryReleasesQuarantineWithoutHoldingResources(t *testing.T) {
	e, cancel := newAdmissionEngine(t, 2)
	defer cancel()
	finish := make(chan struct{})
	defer close(finish)
	h, _ := addAdmissionHandler(e, "recover-cleanup", taskhelp.Max(1), newMemoryAttemptStore(), finish)
	var unavailable atomic.Bool
	unavailable.Store(true)
	var calls atomic.Int32
	h.Cost.Storage = &review3CleanupStorage{release: func() error {
		calls.Add(1)
		if unavailable.Load() {
			return errors.New("unacknowledged cleanup")
		}
		return nil
	}}
	require.True(t, h.considerWork(workSourcePoller, []task{{ID: 1}}, eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}))
	a := h.admissions[1]
	require.Eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ready }, time.Second, time.Millisecond)
	a.handle.CancelPending()
	receiveAdmission(t, a.workerDone)
	require.Zero(t, h.Max.Active())
	require.Empty(t, h.running.Snapshot())
	unavailable.Store(false)
	require.Eventually(t, func() bool { h.drainAdmissions(); return len(h.admissions) == 0 }, 3*time.Second, time.Millisecond)
	require.EqualValues(t, 2, calls.Load(), "one initial cleanup and one bounded retry")
}

func TestReview3CancellationEntryPointsLeaveSlowCleanupOffScheduler(t *testing.T) {
	for _, mode := range []string{"preempt", "shutdown", "other-worker-first", "legacy-storage"} {
		t.Run(mode, func(t *testing.T) {
			e, cancel := newAdmissionEngine(t, 2)
			defer cancel()
			finish := make(chan struct{})
			defer close(finish)
			h, _ := addAdmissionHandler(e, "blocked-cleanup", taskhelp.Max(1), newMemoryAttemptStore(), finish)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			var calls atomic.Int32
			cleanup := func() error { calls.Add(1); once.Do(func() { close(entered) }); <-release; return nil }
			if mode == "legacy-storage" {
				h.Cost.Storage = &admissionFixtureStorage{claim: func(int) (func() error, error) { return cleanup, nil }}
			} else {
				h.Cost.Storage = &review3CleanupStorage{release: cleanup}
			}
			require.True(t, h.considerWork(workSourcePoller, []task{{ID: 1}}, eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}))
			a := h.admissions[1]
			require.Eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ready }, time.Second, time.Millisecond)
			if mode == "other-worker-first" {
				a.cancel()
				receiveAdmission(t, entered)
			}
			done := make(chan struct{})
			go func() {
				switch mode {
				case "preempt":
					a.handle.Preempt()
				case "shutdown":
					cancel()
					e.cancelPendingAdmissions()
				default:
					a.handle.CancelPending()
				}
				close(done)
			}()
			defer func() { close(release); receiveAdmission(t, a.workerDone) }()
			receiveAdmission(t, done)
			receiveAdmission(t, entered)
			require.Zero(t, h.Max.Active())
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestReview3EnteredCleanupFailureRemainsRecoverable(t *testing.T) {
	e, cancel := newAdmissionEngine(t, 2)
	defer cancel()
	finish := make(chan struct{})
	h, impl := addAdmissionHandler(e, "entered-release", taskhelp.Max(1), newMemoryAttemptStore(), finish)
	var failed atomic.Bool
	failed.Store(true)
	var calls atomic.Int32
	h.Cost.Storage = &review3CleanupStorage{release: func() error {
		calls.Add(1)
		if failed.Load() {
			return errors.New("journal handoff failed")
		}
		return nil
	}}
	require.True(t, h.considerWork(workSourcePoller, []task{{ID: 1}}, eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}))
	a := h.admissions[1]
	settleAdmissions(t, h)
	receiveAdmission(t, impl.entered)
	require.Zero(t, h.pendingAdmissionCount(), "running records must not consume bounded pending slots")
	close(finish)
	require.Eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.finished }, time.Second, time.Millisecond)
	h.drainAdmissions()
	require.Same(t, a, h.admissions[1])
	require.Zero(t, h.Max.Active())
	failed.Store(false)
	require.Eventually(t, func() bool { h.drainAdmissions(); return len(h.admissions) == 0 }, 3*time.Second, time.Millisecond)
	require.EqualValues(t, 2, calls.Load())
}

func TestReview3RecoveryWorkersAreBounded(t *testing.T) {
	e, cancel := newAdmissionEngine(t, 1)
	defer cancel()
	h, _ := addAdmissionHandler(e, "bounded-cleanup", taskhelp.Max(1), newMemoryAttemptStore(), nil)
	h.admissions = map[TaskID]*taskAdmission{}
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	for n := range 8 {
		id := TaskID(n + 1)
		h.admissions[id] = &taskAdmission{h: h, id: id, finished: true, quarantined: true, storageUnresolved: true, storage: func() error { entered <- struct{}{}; <-release; return nil }}
	}
	h.drainAdmissions()
	for range 4 {
		receiveAdmission(t, entered)
	}
	h.drainAdmissions() // never waits for a recovery worker or creates a fifth
	require.Empty(t, entered)
	require.Len(t, h.cleanupSlots, 4)
	close(release)
	require.Eventually(t, func() bool { h.drainAdmissions(); return len(h.admissions) == 0 }, time.Second, time.Millisecond)
}
