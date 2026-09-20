package ffi

import (
	"context"
	"errors"
	"sync"

	"github.com/filecoin-project/curio/lib/storiface"
)

// capacityAdmission is an internal integration boundary, not a selectable
// production backend. Startup must not install it until root-wide writer,
// sampler, quota and recovery contracts are closed. Keeping it private prevents
// configuring only TaskStorage as if it protected an entire filesystem.
type capacityAdmission interface {
	Reserve(context.Context, SectorRef, storiface.SectorPaths, storiface.SectorPaths, bool) (capacityClaim, error)
}

type capacityClaim interface {
	// Begin persists writer responsibility. Only a successful invocation gets
	// the capability to certify its own synchronous return. Unknown Start must
	// retain responsibility and yield no execution capability.
	Begin(context.Context) (capacityExecution, error)
	// Cancel is only for claims that have not entered a writer body.
	Cancel(context.Context) error
}

type capacityExecution interface {
	// Returned is called only after the synchronous body and its cleanup return.
	// It retains residency; task completion is not physical space reclamation.
	Returned(context.Context) error
}

type capacityInvocationKey struct{}

// Private to one synchronous caller, not a property of a shared claim. It also
// pins the reservation used for Acquire and SDR receipt/scratch bookkeeping.
type capacityInvocation struct {
	mu          sync.Mutex
	reservation *StorageReservation
	execution   capacityExecution
	closed      bool
}

func (i *capacityInvocation) acquire(ctx context.Context, res *StorageReservation) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return errors.New("capacity invocation already returned")
	}
	if i.reservation != res {
		return errors.New("capacity reservation replaced before writer entry")
	}
	if i.execution != nil {
		return nil
	} // repeated access by this invocation
	execution, err := res.capacity.Begin(ctx)
	if err != nil {
		return err
	}
	if execution == nil {
		return errors.New("capacity Begin returned no execution capability")
	}
	i.execution = execution
	return nil
}

func (i *capacityInvocation) returned(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	if i.execution == nil {
		return nil
	}
	err := i.execution.Returned(ctx)
	if err != nil {
		i.reservation.capacityMu.Lock()
		i.reservation.pendingReturn = i.execution
		i.reservation.capacityMu.Unlock()
	}
	return err
}
