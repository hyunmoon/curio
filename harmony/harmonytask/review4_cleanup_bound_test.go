package harmonytask

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/harmony/taskhelp"
)

// Reconstructed from Review4 (original ZIP unavailable). Actual considerWork,
// dispatchAdmission and completion callback; substituted DB/storage/Do bodies.
func TestReview4ActualDispatchBoundsInitialCleanup(t *testing.T) {
	e, cancel := newAdmissionEngine(t, 2)
	defer cancel()
	go func() {
		for {
			select {
			case <-e.cfg.ctx.Done():
				return
			case <-e.schedulerChannel:
			}
		}
	}() // consume notifications as the scheduler normally does
	finish := make(chan struct{})
	close(finish)
	h, impl := addAdmissionHandler(e, "slow-first-cleanup", taskhelp.Max(1), newMemoryAttemptStore(), finish)
	blocked := make(chan struct{}, maxPendingAdmissions+5)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var completed atomic.Int32
	h.completionRecorder = func(TaskID, bool, error) { completed.Add(1) }
	h.Cost.Storage = &review3CleanupStorage{release: func() error { blocked <- struct{}{}; <-release; return nil }}
	ee := eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}
	accepted := 0
	for n := 1; n <= maxPendingAdmissions+5; n++ {
		if !h.considerWork(workSourcePoller, []task{{ID: TaskID(n)}}, ee) {
			break
		}
		accepted++
		settleAdmissions(t, h)
		receiveAdmission(t, impl.entered)
		receiveAdmission(t, blocked)
		require.Zero(t, h.Max.Active())
	}
	t.Logf("accepted=%d pending=%d active=%d completed=%d", accepted, h.pendingAdmissionCount(), h.Max.Active(), completed.Load())
	require.Equal(t, maxPendingAdmissions, accepted, "Do-returned cleanup bypasses the admission bound")
	require.Equal(t, accepted, h.pendingAdmissionCount())
	require.Zero(t, completed.Load(), "storage barrier precedes completion persistence")
	other, fast := addAdmissionHandler(e, "unrelated", taskhelp.Max(1), newMemoryAttemptStore(), finish)
	require.True(t, other.considerWork(workSourcePoller, []task{{ID: 1000}}, ee))
	settleAdmissions(t, other)
	receiveAdmission(t, fast.entered)
	once.Do(func() { close(release) })
	require.Eventually(t, func() bool { h.drainAdmissions(); return len(h.admissions) == 0 && int(completed.Load()) == accepted }, 3*time.Second, time.Millisecond)
	require.Zero(t, h.pendingAdmissionCount())
	require.True(t, h.considerWork(workSourcePoller, []task{{ID: 1001}}, ee), "released liabilities allow refill")
	settleAdmissions(t, h)
	receiveAdmission(t, impl.entered)
	require.Eventually(t, func() bool { h.drainAdmissions(); return len(h.admissions) == 0 }, time.Second, time.Millisecond)
}

func TestReview4ActiveDoAndReturnedLiabilitiesAreDistinct(t *testing.T) {
	for _, slots := range []int{4, 12} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			e, cancel := newAdmissionEngine(t, slots)
			defer cancel()
			finish, release := make(chan struct{}), make(chan struct{})
			var doOnce, releaseOnce sync.Once
			defer doOnce.Do(func() { close(finish) })
			defer releaseOnce.Do(func() { close(release) })
			h, impl := addAdmissionHandler(e, "active", taskhelp.Max(slots), newMemoryAttemptStore(), finish)
			h.Cost.Storage = &review3CleanupStorage{release: func() error { <-release; return nil }}
			ee := eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}
			for n := 1; n <= slots; n++ {
				require.True(t, h.considerWork(workSourcePoller, []task{{ID: TaskID(n)}}, ee))
				settleAdmissions(t, h)
				receiveAdmission(t, impl.entered)
			}
			require.Equal(t, slots, h.Max.Active())
			require.Zero(t, h.pendingAdmissionCount())
			e.atomics.draining.Store(true)
			doOnce.Do(func() { close(finish) })
			require.Eventually(t, func() bool { return h.Max.Active() == 0 }, time.Second, time.Millisecond)
			require.Equal(t, slots, h.pendingAdmissionCount())
			e.cancelPendingAdmissions() // no storage I/O, no removal of liabilities
			require.Equal(t, slots, h.pendingAdmissionCount())
			releaseOnce.Do(func() { close(release) })
			require.Eventually(t, func() bool { h.drainAdmissions(); return len(h.admissions) == 0 }, time.Second, time.Millisecond)
		})
	}
}

func TestReview4ReturnDuringCanAcceptMustNotClaimNegativeHeadroom(t *testing.T) {
	e, cancel := newAdmissionEngine(t, 3)
	defer cancel()
	finish, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	defer close(release)
	h, impl := addAdmissionHandler(e, "return-during-filter", taskhelp.Max(3), newMemoryAttemptStore(), finish)
	h.Cost.Storage = &review3CleanupStorage{release: func() error { <-release; return nil }}
	ee := eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}
	require.True(t, h.considerWork(workSourcePoller, []task{{ID: 1}, {ID: 2}}, ee))
	settleAdmissions(t, h)
	receiveAdmission(t, impl.entered)
	receiveAdmission(t, impl.entered)
	// Boundary setup only: existing quarantines, not additional native workers.
	for n := 100; n < 100+maxPendingAdmissions-1; n++ {
		h.admissions[TaskID(n)] = &taskAdmission{enteredExecution: true, finished: true, quarantined: true}
	}
	impl.accept = func(ids []TaskID) ([]TaskID, error) {
		once.Do(func() { close(finish) })
		require.Eventually(t, func() bool { return h.Max.Active() == 0 }, time.Second, time.Millisecond)
		return ids, nil
	}
	called := false
	claim := func([]TaskID, int) ([]TaskID, error) { called = true; return nil, nil }
	require.False(t, h.considerWorkWithOwnership(workSourcePoller, []task{{ID: 50}}, ee, claim, nil, newMemoryAttemptStore()))
	require.False(t, called, "Do-return liability grew while filtering: reject before claim/reservation")
}
