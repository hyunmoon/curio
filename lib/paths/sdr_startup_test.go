package paths

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"

	"github.com/filecoin-project/lotus/storage/sealer/fsutil"
)

type startupIndex struct{ SectorIndex }

func (*startupIndex) StorageAttach(context.Context, storiface.StorageInfo, fsutil.FsStat) error {
	return nil
}

func TestSDRStartupOpenPath(t *testing.T) {
	local := t.TempDir()
	dest := filepath.Join(local, "cache", "s-t01000-42")
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0700))
	r := NewSDRReservation(dest)
	w, err := sdrscratch.Begin(r.Scratch)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(r.Scratch, "layer"), []byte("failed output"), 0600))
	require.NoError(t, w.Returned())
	require.NoError(t, w.Close())
	meta, err := json.Marshal(storiface.LocalStorageMeta{ID: "A", CanSeal: true})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(local, MetaFile), meta, 0600))
	st := &Local{paths: map[storiface.ID]*path{}, index: &startupIndex{}, localStorage: &sdrReservationStorage{usage: map[string]int64{}}}
	id, _, err := st.openPath(context.Background(), local, false)
	require.NoError(t, err)
	require.Equal(t, storiface.ID("A"), id)
	require.NoFileExists(t, filepath.Join(r.Scratch, "layer"))
	require.DirExists(t, r.Scratch)
}

func TestSDRStartupAdmissionFailureAndRetry(t *testing.T) {
	st, _ := newSelectionLocal(t, "A", "B")
	st.paths["A"].CanSeal = true
	local := st.paths["A"].Local
	base := filepath.Join(local, "cache")
	require.NoError(t, os.MkdirAll(base, 0700))
	sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 42}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
	r := NewSDRReservation(st.paths["A"].sectorPath(sr.ID, storiface.FTCache))
	w, err := sdrscratch.Begin(r.Scratch)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(r.Scratch, "layer"), []byte("owned"), 0600))
	require.NoError(t, w.Returned())
	require.NoError(t, w.Close())
	// An unexpected nested entry makes reclamation fail closed before any unlink.
	require.NoError(t, os.Mkdir(filepath.Join(r.Scratch, "unknown"), 0700))
	// Startup uses the same scan; it must not hold the global localLk during I/O.
	st.localLk.Lock()
	st.sweepSDRScratch(local)
	st.localLk.Unlock()
	_, err = st.ReserveSDR(context.Background(), sr, storiface.FTCache, storiface.SectorPaths{Cache: "A"}, storiface.FSOverheadSeal, 0, NewSDRReservation(st.paths["A"].sectorPath(sr.ID, storiface.FTCache)))
	require.ErrorContains(t, err, "scratch cleanup before reservation")
	require.Zero(t, st.paths["A"].Reserved)
	require.Empty(t, st.paths["A"].SDRReservations)
	require.FileExists(t, filepath.Join(r.Scratch, "layer"))
	// Other storage is usable; no change to virtual capacity or selection counters.
	releaseB := reserveSelection(t, st, "B", 43, storiface.FTCache, true)
	releaseB()
	// Fixture-only removal of the injected obstruction. Retry does not need a
	// process restart or an in-memory permanent error flag reset.
	require.NoError(t, os.Remove(filepath.Join(r.Scratch, "unknown")))
	st.PrepareSDRScratch()
	release := reserveSelection(t, st, "A", 44, storiface.FTCache, true)
	require.NoFileExists(t, filepath.Join(r.Scratch, "layer"))
	release()
	require.Empty(t, st.paths["A"].SDRReservations)
}
