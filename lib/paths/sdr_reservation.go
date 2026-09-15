package paths

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"

	"github.com/filecoin-project/curio/lib/storiface"
)

// Each claim owns a fresh name, even when task/sector IDs are reused. The name
// is allocated before reserving space; the directory is created at Do entry.
// It is not a distributed lease: other processes' reservations remain local.
type SDRReservation struct {
	Scratch      string
	mu           sync.Mutex
	materialized string
}

func NewSDRReservation(destination string) *SDRReservation {
	p := filepath.Join(storiface.SDRTempRoot(destination), "attempt-"+uuid.NewString())
	return &SDRReservation{Scratch: p, materialized: p}
}

// UsePublished must only be called after validating the completion provenance,
// or after this claim atomically published its own successful native output.
func (r *SDRReservation) UsePublished(destination string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.materialized = destination
}

func (r *SDRReservation) creditPath() string { r.mu.Lock(); defer r.mu.Unlock(); return r.materialized }
func (r *SDRReservation) Published() bool    { return r.creditPath() != r.Scratch }

func sdrCredit(ls LocalStorage, p string, oh int64) (int64, error) {
	n, err := ls.DiskUsage(p)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return min(max(n, 0), oh), nil
}

func (st *Local) ReserveSDR(ctx context.Context, sid storiface.SectorRef, ft storiface.SectorFileType, ids storiface.SectorPaths, overheads map[storiface.SectorFileType]int, minFree float64, r *SDRReservation) (func(), error) {
	if r == nil || (ft != storiface.FTCache && ft != storiface.FTKey) {
		return nil, fmt.Errorf("invalid SDR reservation intent")
	}
	return st.reserve(ctx, sid, ft, ids, overheads, minFree, r)
}
