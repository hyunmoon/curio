package paths

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

const sdrCleanupInterval = 30 * time.Second
const sdrCleanupProtectedTTL = 5 * time.Minute
const sdrCleanupLogInterval = 15 * time.Minute

type sdrCleanupRoot struct {
	mu             sync.Mutex
	protectedUntil map[string]time.Time // denial cache only; never authorizes removal
	nextLog        time.Time
}

func (st *Local) sdrRoots() map[string]storiface.ID {
	st.localLk.RLock()
	defer st.localLk.RUnlock()
	locals := map[string]storiface.ID{}
	for id, p := range st.paths {
		if p.CanSeal {
			locals[p.Local] = id
		}
	}
	return locals
}

// Each root has at most one in-flight pass across startup, Claim and timer.
// A slow/unavailable root does not serialize the timers for other roots.
func (st *Local) retrySDRCleanup(ctx context.Context) {
	ticker := time.NewTicker(sdrCleanupInterval)
	defer ticker.Stop()
	var running sync.WaitGroup
	defer running.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			for local, id := range st.sdrRoots() {
				st.startSDRRoot(ctx, local, id, &running)
			}
		}
	}
}

func (st *Local) cleanupRoot(local string) *sdrCleanupRoot {
	// Registration resolves aliases too; use the same root key for overlap.
	key, err := filepath.EvalSymlinks(local)
	if err != nil {
		key = filepath.Clean(local)
	}
	r, _ := st.sdrCleanupRoots.LoadOrStore(key, &sdrCleanupRoot{})
	return r.(*sdrCleanupRoot)
}

func (st *Local) startSDRRoot(ctx context.Context, local string, id storiface.ID, running *sync.WaitGroup) {
	if ctx.Err() != nil {
		return
	}
	r := st.cleanupRoot(local)
	if !r.mu.TryLock() {
		return
	}
	running.Add(1)
	go func() {
		defer running.Done()
		defer r.mu.Unlock()
		st.cleanSDRRoot(ctx, local, id, r, true)
	}()
}

func (st *Local) prepareSDRRoot(ctx context.Context, local string, id storiface.ID, periodic bool) {
	r := st.cleanupRoot(local)
	if !r.mu.TryLock() {
		return
	}
	defer r.mu.Unlock()
	st.cleanSDRRoot(ctx, local, id, r, periodic)
}

func (st *Local) cleanSDRRoot(ctx context.Context, local string, id storiface.ID, r *sdrCleanupRoot, periodic bool) {
	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	now := time.Now()
	verbose := !now.Before(r.nextLog)
	if verbose {
		r.nextLog = now.Add(sdrCleanupLogInterval)
	}
	if err := sdrscratch.RegisterPersonalStorage(local, string(id)); err != nil {
		if verbose {
			log.Errorw("Personal SDR root registration retry failed", "path", local, "error", err)
		}
		return
	}
	changed := st.autoDiscardSDR(ctx, local, r, verbose)
	// Keep the existing synchronous returned-writer sweep. The automatic
	// discard timer uses the stricter live DB + process boundary for each target.
	if !periodic && ctx.Err() == nil {
		st.sweepSDRScratch(local)
	}
	if changed {
		if cached, ok := st.localStorage.(*cachedLocalStorage); ok {
			cached.invalidate(local)
		}
		// StorageBestAlloc consults index health, not Local.Stat. Publish only
		// this root, outside localLk, and retain normal heartbeat retry on error.
		stat, err := st.FsStat(ctx, id)
		if err == nil && ctx.Err() == nil {
			err = st.index.StorageReportHealth(ctx, id, storiface.HealthReport{Stat: stat})
		}
		if err != nil && verbose {
			log.Warnw("SDR reclaimed capacity report deferred", "path", local, "error", err)
		}
	}
}

// PrepareSDRScratch must precede ReservationCtxLock and sector/resource locks.
// A failing path is logged and will fail its own ReserveSDR check; healthy
// paths and unrelated task types are not globally disabled.
func (st *Local) PrepareSDRScratch() {
	ctx := st.sdrCleanupCtx
	if ctx == nil {
		ctx = context.Background()
	}
	for local, id := range st.sdrRoots() {
		st.prepareSDRRoot(ctx, local, id, false)
	}
}

func (st *Local) sweepSDRScratch(local string) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		results, err := sdrscratch.Sweep(filepath.Join(local, ft.String()))
		for _, r := range results {
			if r.Status == "already_reclaimed" || r.Status == "live" {
				continue
			}
			log.Infow("SDR startup scratch inspection", "path", r.Path, "status", r.Status, "reason", r.Reason, "filesRemoved", r.FilesRemoved)
		}
		if err != nil {
			log.Errorw("SDR scratch cleanup incomplete; reservation will recheck", "path", local, "type", ft, "error", err)
		}
	}
}
