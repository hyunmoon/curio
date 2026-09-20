package harmonytask

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/harmony/taskhelp"
)

type preparedCapacityStorage struct {
	started  chan struct{}
	finish   chan struct{}
	err      error
	released atomic.Int32
	legacy   atomic.Int32
}

func (*preparedCapacityStorage) HasCapacity() bool { return true }
func (s *preparedCapacityStorage) Claim(int) (func() error, error) {
	s.legacy.Add(1)
	return nil, errors.New("unexpected legacy claim")
}
func (s *preparedCapacityStorage) PrepareStorageClaim(context.Context, int) (func() error, bool, error) {
	close(s.started)
	<-s.finish
	if s.err != nil {
		return nil, true, s.err
	}
	return func() error { s.released.Add(1); return nil }, true, nil
}

func TestCapacityPreparationDoesNotBlockSchedulerOrChargeFailure(t *testing.T) {
	for _, mode := range []string{"success", "denial", "cancel-late-success"} {
		t.Run(mode, func(t *testing.T) {
			e, cancel := newAdmissionEngine(t, 2)
			defer cancel()
			finish := make(chan struct{})
			store := newMemoryAttemptStore()
			h, task := addAdmissionHandler(e, "capacity", taskhelp.Max(1), store, finish)
			s := &preparedCapacityStorage{started: make(chan struct{}), finish: make(chan struct{})}
			if mode == "denial" {
				s.err = errors.New("WAIT_CAPACITY")
			}
			h.Cost.Storage = s
			var completions atomic.Int32
			h.completionRecorder = func(TaskID, bool, error) { completions.Add(1) }
			h.admissions = make(map[TaskID]*taskAdmission)
			h.beginAdmission("test", 1, nil, store, nil, eventEmitter{ctx: e.cfg.ctx, schedulerChannel: e.schedulerChannel}, nil)
			a := h.admissions[1]
			receiveAdmission(t, s.started)
			// This is the real scheduler drain call. A disk operation is held
			// at a deterministic barrier in the preparation worker.
			drained := make(chan struct{})
			go func() { h.drainAdmissions(); close(drained) }()
			receiveAdmission(t, drained) // must return while storage is still held
			select {
			case <-task.entered:
				t.Fatal("Do before storage admission")
			default:
			}
			if mode == "cancel-late-success" {
				a.handle.CancelPending()
			}
			close(s.finish)
			if mode == "success" {
				require.Eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.ready }, time.Second, time.Millisecond)
				h.drainAdmissions()
				require.Equal(t, TaskID(1), receiveAdmission(t, task.entered))
				close(finish)
				receiveAdmission(t, a.localDone)
				require.Eventually(t, func() bool { return completions.Load() == 1 }, time.Second, time.Millisecond)
			} else {
				receiveAdmission(t, a.workerDone)
				h.drainAdmissions()
				require.Zero(t, completions.Load(), "pre-entry failure must not consume task retry/failure budget")
				select {
				case <-task.entered:
					t.Fatal("Do on rejected claim")
				default:
				}
			}
			require.Zero(t, s.legacy.Load())
			if mode != "denial" {
				require.EqualValues(t, 1, s.released.Load())
			}
			require.EqualValues(t, 0, h.Max.Active())
		})
	}
}
