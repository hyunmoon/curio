package harmonytask

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/filecoin-project/curio/harmony/harmonytask/internal/runregistry"
	"github.com/filecoin-project/curio/harmony/resources"
)

const maxPendingAdmissions = 100

// admissions is scheduler-owned. Workers only mutate their own locked result.
// Quarantine consumes a bounded slot until process recovery; it never busy-retries.
type taskAdmission struct {
	h                 *taskTypeHandler
	id                TaskID
	from              string
	tasks             []task
	store             taskAttemptStore
	token             string
	reservation       *taskStartReservation
	ee                eventEmitter
	ctx               context.Context
	cancel            context.CancelFunc
	handle            *runregistry.Handle
	entered           chan struct{}
	workerDone        chan struct{}
	localDone         chan struct{}
	release           func([]TaskID, map[TaskID]string) error
	stopEngine        func() bool
	localOnce         sync.Once
	mu                sync.Mutex
	ready             bool
	enteredExecution  bool
	executionReturned bool
	taken             bool
	finished          bool
	quarantined       bool
	storage           func() error
	storagePrepared   bool
	storageError      error
	dbCleanupError    error
	storageUnresolved bool
	recovering        bool
	retryCleanupAt    time.Time
}

func (h *taskTypeHandler) wakeAdmission() {
	select {
	case h.TaskEngine.admissionWake <- struct{}{}:
	default:
	}
}

func (h *taskTypeHandler) beginAdmission(from string, id TaskID, tasks []task, store taskAttemptStore,
	reservation *taskStartReservation, ee eventEmitter, release func([]TaskID, map[TaskID]string) error) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &taskAdmission{h: h, id: id, from: from, tasks: tasks, store: store,
		token: uuid.NewString(), reservation: reservation, ee: ee, ctx: ctx, cancel: cancel,
		entered: make(chan struct{}), workerDone: make(chan struct{}), localDone: make(chan struct{}), release: release}
	a.store = bindAttemptToken(store, a.token)
	h.Max.Add(1) // Same counters as running: no pending resource double-spend.
	h.running.StartPending(int64(id), func() {
		cancel()
		if a.handle.IsPending() {
			a.releaseLocal()
		}
	}, func(handle *runregistry.Handle) { a.handle = handle })
	h.admissions[id] = a
	a.stopEngine = context.AfterFunc(h.TaskEngine.cfg.ctx, func() { a.handle.CancelPending() })
	go a.prepare()
}

func admissionIO(fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("admission I/O panic: %v", p)
		}
	}()
	return fn()
}

func (a *taskAdmission) prepare() {
	defer close(a.workerDone)
	ctx, stop := context.WithTimeout(a.ctx, 5*time.Second)
	err := admissionIO(func() error { return a.store.prepare(ctx, a.id, a.token) })
	if err == nil {
		err = ctx.Err()
	}
	stop()
	prepared := err == nil
	if prepared && a.ctx.Err() == nil && a.h.Cost.Storage != nil {
		var release func() error
		var handled bool
		if storage, ok := a.h.Cost.Storage.(resources.PreparedStorage); ok {
			err = admissionIO(func() error {
				var claimErr error
				release, handled, claimErr = storage.PrepareStorageClaim(a.ctx, int(a.id))
				return claimErr
			})
			if err == nil && ((handled && release == nil) || (!handled && release != nil)) {
				err = fmt.Errorf("invalid prepared storage cleanup contract")
			}
		}
		if err == nil && !handled && a.ctx.Err() == nil {
			h := a.h
			err = admissionIO(func() error { var e error; release, e = h.Cost.Claim(int(a.id)); return e })
		}
		if release != nil {
			a.attachStorage(release)
		}
		a.mu.Lock()
		a.storagePrepared, a.storageError = true, err
		a.mu.Unlock()
	}
	if prepared && err == nil && a.ctx.Err() == nil {
		a.mu.Lock()
		a.ready = true
		a.mu.Unlock()
		a.h.wakeAdmission()
		select {
		case <-a.entered:
			a.h.wakeAdmission()
			return
		case <-a.ctx.Done():
		}
	}
	// Do-entry wins against cancellation. A running task owns completion now.
	select {
	case <-a.entered:
		a.h.wakeAdmission()
		return
	default:
	}
	if err != nil {
		log.Errorw("Task admission preparation failed", "id", a.id, "error", err)
	}
	a.handle.CancelPending()
	a.stopEngine()
	a.releaseLocal()
	storageErr := a.releaseStorage()
	cleanupCtx, stopCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	err = admissionIO(func() error {
		if prepared && a.release != nil {
			return a.release([]TaskID{a.id}, map[TaskID]string{a.id: a.token})
		}
		return a.store.releaseUnstarted(cleanupCtx, a.id)
	})
	stopCleanup()
	if err != nil {
		log.Errorw("Task admission quarantined until recovery", "id", a.id, "error", err)
	}
	a.mu.Lock()
	a.finished = true
	a.quarantined = err != nil || storageErr != nil
	a.dbCleanupError = err
	a.storageUnresolved = storageErr != nil
	a.retryCleanupAt = time.Now().Add(time.Second)
	a.mu.Unlock()
	a.h.wakeAdmission()
}

func (a *taskAdmission) releaseLocal() {
	a.localOnce.Do(func() {
		var cancelReservation func()
		if a.reservation != nil {
			cancelReservation = a.reservation.cancel
		}
		for _, cleanup := range []func(){cancelReservation} {
			if cleanup == nil {
				continue
			}
			if err := admissionIO(func() error { cleanup(); return nil }); err != nil {
				log.Errorw("Admission local cleanup failed", "id", a.id, "error", err)
			}
		}
		a.h.Max.Add(-1)
		a.h.running.FinishHandle(a.handle)
		close(a.localDone)
		a.h.wakeAdmission()
	})
}

// All storage I/O belongs to the existing bounded preparation worker (pending
// cancellation) or actual task worker (entered completion), never CancelPending
// or the scheduler. Ownership passes only through entered; there is one caller.
func (a *taskAdmission) releaseStorage() error {
	a.mu.Lock()
	release := a.storage
	a.mu.Unlock()
	if release == nil {
		return nil
	}
	err := admissionIO(release)
	a.mu.Lock()
	if err == nil {
		a.storage = nil
	} else {
		a.storageError = err
	}
	a.mu.Unlock()
	return err
}

func (a *taskAdmission) attachStorage(release func() error) {
	a.mu.Lock()
	a.storage = release
	a.mu.Unlock()
}

// At most one retry per quarantined admission and four per task type.
// No filesystem or recovery I/O runs in the scheduler. A hung syscall consumes
// only its already-reserved quarantine slot, not a CPU/RAM/pacing permit.
func (a *taskAdmission) recoverStorage() {
	a.mu.Lock()
	if !a.storageUnresolved || a.recovering || time.Now().Before(a.retryCleanupAt) {
		a.mu.Unlock()
		return
	}
	if a.h.cleanupSlots == nil {
		a.h.cleanupSlots = make(chan struct{}, 4)
	}
	select {
	case a.h.cleanupSlots <- struct{}{}:
	default:
		a.mu.Unlock()
		return
	}
	a.recovering = true
	a.mu.Unlock()
	slots := a.h.cleanupSlots
	go func() {
		defer func() { <-slots }()
		err := a.releaseStorage()
		a.mu.Lock()
		a.recovering = false
		a.storageUnresolved = err != nil
		a.quarantined = err != nil || a.dbCleanupError != nil
		a.retryCleanupAt = time.Now().Add(time.Second)
		a.mu.Unlock()
		a.h.wakeAdmission()
	}()
}

func (a *taskAdmission) enter(ctx context.Context) error {
	return a.handle.Enter(func() error {
		e := a.h.TaskEngine
		if ctx.Err() != nil || e.cfg.ctx.Err() != nil || e.atomics.draining.Load() || (e.atomics.yieldBackground.Load() && a.from != workSourceOverride) {
			return context.Canceled
		}
		if a.reservation != nil {
			if err := a.reservation.start(ctx); err != nil {
				return err
			}
		}
		a.stopEngine()
		a.mu.Lock()
		a.enteredExecution = true
		a.mu.Unlock()
		close(a.entered)
		return nil
	})
}

func (a *taskAdmission) finishedStorage(err error) {
	a.mu.Lock()
	a.finished = true
	a.quarantined = err != nil
	a.storageUnresolved = err != nil
	a.retryCleanupAt = time.Now().Add(time.Second)
	a.mu.Unlock()
	a.h.wakeAdmission()
}

// Transfer to the cleanup liability budget BEFORE returning execution permits.
// The scheduler can then reuse CPU/RAM without losing track of a blocked first
// cleanup. In addition to maxPendingAdmissions, already-entered Do workers may
// return concurrently (bounded by their existing resource/Max limits).
func (a *taskAdmission) returnedExecution() {
	a.mu.Lock()
	a.executionReturned = true
	a.mu.Unlock()
}

func (h *taskTypeHandler) pendingAdmissionCount() int {
	count := 0
	for _, a := range h.admissions {
		a.mu.Lock()
		if !a.enteredExecution || a.executionReturned || a.finished {
			count++
		}
		a.mu.Unlock()
	}
	return count
}

// Only the scheduler calls this: storageFailures, dispatch and maps stay local.
func (h *taskTypeHandler) drainAdmissions() {
	for id, a := range h.admissions {
		a.mu.Lock()
		finished, quarantined := a.finished, a.quarantined
		storageError := a.storageError
		ready := a.ready && !a.taken
		if ready {
			a.taken = true
		}
		a.mu.Unlock()
		if finished {
			if quarantined {
				a.recoverStorage()
			}
			if storageError != nil {
				if _, recorded := h.storageFailures[id]; !recorded {
					h.storageFailures[id] = time.Now()
				}
			}
			if !quarantined {
				delete(h.admissions, id)
			}
			continue
		}
		if !ready {
			continue
		}
		if a.ctx.Err() != nil || h.TaskEngine.cfg.ctx.Err() != nil || h.TaskEngine.atomics.draining.Load() || (h.TaskEngine.atomics.yieldBackground.Load() && a.from != workSourceOverride) {
			a.handle.CancelPending()
			continue
		}
		h.dispatchAdmission(a)
	}
}

func (e *TaskEngine) cancelPendingAdmissions() {
	for _, h := range e.handlers {
		for _, entry := range h.running.Snapshot() {
			if handle, ok := h.running.Get(entry.ID); ok {
				handle.CancelPending()
			}
		}
	}
}

func (h *taskTypeHandler) recoverTaskOwnership(tasks []task, ids []TaskID, limit int, generations map[TaskID]int64) ([]TaskID, error) {
	var accepted []TaskID
	for _, t := range tasks {
		if len(accepted) >= limit {
			break
		}
		wanted := false
		for _, id := range ids {
			if id == t.ID {
				wanted = true
				break
			}
		}
		if !wanted {
			continue
		}
		var rows []int64
		err := h.TaskEngine.cfg.db.Select(h.TaskEngine.cfg.ctx, &rows, RECOVER_TASK_ACQUISITION, t.ID, h.TaskEngine.cfg.ownerID, t.OwnerGeneration)
		if err != nil {
			return accepted, err
		}
		if len(rows) == 1 {
			accepted = append(accepted, t.ID)
			generations[t.ID] = rows[0]
		}
	}
	return accepted, nil
}

const RECOVER_TASK_ACQUISITION = `UPDATE harmony_task
SET owner_generation=owner_generation+1
WHERE id=$1 AND owner_id=$2 AND owner_generation=$3
RETURNING owner_generation`

// Recovery is fed by the real event loop, never by a constructor waiting for it.
// Capacity/refusal retains the observed generation for later conditional retry.
func (e *TaskEngine) drainRecovery(ee eventEmitter) {
	if e.cfg.ctx.Err() != nil || e.atomics.draining.Load() || e.atomics.yieldBackground.Load() {
		return
	}
	for name, tasks := range e.recovery {
		h := e.taskMap[name]
		var deferred []task
		for _, t := range tasks {
			if !h.considerWork(workSourceRecover, []task{t}, ee) {
				deferred = append(deferred, t)
			}
		}
		e.recovery[name] = deferred
	}
}
