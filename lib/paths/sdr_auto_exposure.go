package paths

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

// This read guards external cache consumers, not a successful SDR retry's
// receipt reuse. The latter remains in GenerateSDR with full input validation.
func (dbi *DBIndex) sdrExternallyReady(ctx context.Context, id abi.SectorID, ft storiface.SectorFileType) (bool, error) {
	var ready bool
	var err error
	if ft == storiface.FTCache {
		err = dbi.harmonyDB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sectors_sdr_pipeline WHERE sp_id=$1 AND sector_number=$2 AND after_sdr) OR EXISTS(SELECT 1 FROM sectors_meta WHERE sp_id=$1 AND sector_num=$2)`, id.Miner, id.Number).Scan(&ready)
	} else {
		err = dbi.harmonyDB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sectors_unseal_pipeline WHERE sp_id=$1 AND sector_number=$2 AND after_unseal_sdr)`, id.Miner, id.Number).Scan(&ready)
	}
	return ready, err
}

func (st *Local) SDRExternalRead(ctx context.Context, path string, id abi.SectorID, ft storiface.SectorFileType) error {
	if ft != storiface.FTCache && ft != storiface.FTKey {
		return nil
	}
	on, err := sdrscratch.PersonalCleanupEnabled()
	if err != nil || !on {
		return err
	}
	st.localLk.RLock()
	registered := false
	for _, p := range st.paths {
		if p.CanSeal && p.Local == filepath.Dir(filepath.Dir(path)) {
			registered = true
		}
	}
	st.localLk.RUnlock()
	if !registered {
		return nil
	}
	i, ok := st.index.(interface {
		sdrExternallyReady(context.Context, abi.SectorID, storiface.SectorFileType) (bool, error)
	})
	if !ok {
		return fmt.Errorf("managed cache readiness provider absent")
	}
	ready, err := i.sdrExternallyReady(ctx, id, ft)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("managed cache has no completed-stage evidence; external read deferred")
	}
	return nil
}
