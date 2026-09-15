package paths

import (
	"path/filepath"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

// PrepareSDRScratch must precede ReservationCtxLock and sector/resource locks.
// A failing path is logged and will fail its own ReserveSDR check; healthy
// paths and unrelated task types are not globally disabled.
func (st *Local) PrepareSDRScratch() {
	st.localLk.RLock()
	var locals []string
	for _, p := range st.paths {
		if p.CanSeal {
			locals = append(locals, p.Local)
		}
	}
	st.localLk.RUnlock()
	for _, local := range locals {
		st.sweepSDRScratch(local)
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
