package ffi

import (
	"context"

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
	// Begin must persist writer responsibility before fetch or other writes.
	Begin(context.Context) error
	// Returned is called only after the synchronous body and its cleanup return.
	// It retains residency; task completion is not physical space reclamation.
	Returned(context.Context) error
	// Cancel is only for claims that have not entered a writer body.
	Cancel(context.Context) error
}
