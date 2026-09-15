package paths

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/storiface"
)

// Only the index and filesystem are replaced. Selection and reservation use
// Local's production methods; index order represents its space/weight ranking.
type selectionIndex struct {
	SectorIndex
	candidates []storiface.StorageInfo
	existing   []storiface.SectorStorageInfo
}

func (s *selectionIndex) StorageFindSector(context.Context, abi.SectorID, storiface.SectorFileType, abi.SectorSize, bool) ([]storiface.SectorStorageInfo, error) {
	return append([]storiface.SectorStorageInfo(nil), s.existing...), nil
}

func (s *selectionIndex) StorageBestAlloc(context.Context, storiface.SectorFileType, abi.SectorSize, storiface.PathType, abi.ActorID) ([]storiface.StorageInfo, error) {
	return append([]storiface.StorageInfo(nil), s.candidates...), nil
}

func newSelectionLocal(t *testing.T, ids ...storiface.ID) (*Local, *selectionIndex) {
	t.Helper()
	idx := &selectionIndex{}
	st := &Local{index: idx, localStorage: &sdrReservationStorage{usage: map[string]int64{}}, paths: map[storiface.ID]*path{}}
	root := t.TempDir()
	for _, id := range ids {
		st.paths[id] = &path{Local: filepath.Join(root, string(id)), Reservations: map[string]int64{}}
		idx.candidates = append(idx.candidates, storiface.StorageInfo{ID: id, CanSeal: true, CanStore: true})
	}
	return st, idx
}

func reserveSelection(t *testing.T, st *Local, id storiface.ID, number abi.SectorNumber, ft storiface.SectorFileType, sdr bool) func() {
	t.Helper()
	sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: number}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
	var ids storiface.SectorPaths
	storiface.SetPathByType(&ids, ft, string(id))
	var release func()
	var err error
	if sdr {
		r := NewSDRReservation(st.paths[id].sectorPath(sr.ID, ft))
		release, err = st.ReserveSDR(context.Background(), sr, ft, ids, storiface.FSOverheadSeal, 0, r)
	} else {
		release, err = st.Reserve(context.Background(), sr, ft, ids, storiface.FSOverheadSeal, 0)
	}
	require.NoError(t, err)
	t.Cleanup(release)
	return release
}

func selectStorage(t *testing.T, st *Local, ft storiface.SectorFileType, pathType storiface.PathType) storiface.ID {
	t.Helper()
	sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 999}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
	out, ids, err := st.AcquireSector(context.Background(), sr, storiface.FTNone, ft, pathType, storiface.AcquireMove)
	require.NoError(t, err)
	id := storiface.ID(storiface.PathByType(ids, ft))
	require.NotEmpty(t, id)
	require.Equal(t, st.paths[id].sectorPath(sr.ID, ft), storiface.PathByType(out, ft))
	return id
}

func TestLocalAcquireSectorCountsSDRReservations(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(ft.String(), func(t *testing.T) {
			st, _ := newSelectionLocal(t, "A", "B")
			release := reserveSelection(t, st, "A", 1, ft, true)
			require.Empty(t, st.paths["A"].Reservations, "SDR must not be duplicated in the ordinary credit map")
			require.Len(t, st.paths["A"].SDRReservations, 1)
			require.Equal(t, storiface.ID("B"), selectStorage(t, st, ft, storiface.PathSealing), "idle B must precede A's active SDR reservation")
			release()
			require.Equal(t, storiface.ID("A"), selectStorage(t, st, ft, storiface.PathSealing), "release restores the index's tie order")
		})
	}
}

func TestLocalAcquireSectorMixedReservationsAndRelease(t *testing.T) {
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		t.Run(ft.String(), func(t *testing.T) {
			st, _ := newSelectionLocal(t, "A", "B")
			a := reserveSelection(t, st, "A", 1, ft, false)
			sdr := reserveSelection(t, st, "A", 2, ft, true)
			b := reserveSelection(t, st, "B", 3, ft, false)
			require.Equal(t, storiface.ID("B"), selectStorage(t, st, ft, storiface.PathSealing), "A has two claims and B one")
			sdr()
			sdr()
			require.Len(t, st.paths["A"].Reservations, 1, "repeated SDR release preserves the ordinary claim")
			require.Empty(t, st.paths["A"].SDRReservations)
			require.Equal(t, storiface.ID("A"), selectStorage(t, st, ft, storiface.PathSealing))
			b()
			require.Equal(t, storiface.ID("B"), selectStorage(t, st, ft, storiface.PathSealing))
			a()
			require.Equal(t, storiface.ID("A"), selectStorage(t, st, ft, storiface.PathSealing))
		})
	}
}

func TestLocalAcquireSectorReservationOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts []int
		order  []storiface.ID
	}{
		{"reverse", []int{3, 2, 1, 0}, []storiface.ID{"D", "C", "B", "A"}},
		{"mixed", []int{1, 3, 0}, []storiface.ID{"C", "A", "B"}},
		{"mixed-four", []int{2, 4, 3, 0}, []storiface.ID{"D", "A", "C", "B"}},
		{"ties", []int{1, 0, 0, 1}, []storiface.ID{"B", "C", "A", "D"}},
		{"all-tied", []int{1, 1, 1}, []storiface.ID{"A", "B", "C"}},
	} {
		for _, sdr := range []bool{false, true} {
			kind := "ordinary"
			if sdr {
				kind = "SDR"
			}
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				ids := []storiface.ID{"A", "B", "C", "D"}[:len(tc.counts)]
				st, idx := newSelectionLocal(t, ids...)
				for i, n := range tc.counts {
					for j := 0; j < n; j++ {
						reserveSelection(t, st, ids[i], abi.SectorNumber(i*10+j), storiface.FTCache, sdr)
					}
				}
				for _, want := range tc.order {
					got := selectStorage(t, st, storiface.FTCache, storiface.PathSealing)
					require.Equal(t, want, got, "reservation count must stay attached to its storage ID")
					// Remove the chosen candidate, then exercise the next production selection.
					for i, si := range idx.candidates {
						if si.ID == got {
							idx.candidates = append(idx.candidates[:i], idx.candidates[i+1:]...)
							break
						}
					}
				}
			})
		}
	}
}

func TestLocalAcquireSectorReservationEligibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pathType storiface.PathType
		restrict func(*storiface.StorageInfo)
	}{
		{"cannot-seal", storiface.PathSealing, func(si *storiface.StorageInfo) { si.CanSeal = false }},
		{"cannot-store", storiface.PathStorage, func(si *storiface.StorageInfo) { si.CanStore = false }},
		{"allow-types", storiface.PathSealing, func(si *storiface.StorageInfo) { si.AllowTypes = []string{"sealed"} }},
		{"deny-types", storiface.PathSealing, func(si *storiface.StorageInfo) { si.DenyTypes = []string{"cache"} }},
		{"allow-miners", storiface.PathSealing, func(si *storiface.StorageInfo) { si.AllowMiners = []string{"f01001"} }},
		{"deny-miners", storiface.PathSealing, func(si *storiface.StorageInfo) { si.DenyMiners = []string{"f01000"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, idx := newSelectionLocal(t, "A", "B")
			tc.restrict(&idx.candidates[0])
			reserveSelection(t, st, "B", 1, storiface.FTCache, true)
			require.Equal(t, storiface.ID("B"), selectStorage(t, st, storiface.FTCache, tc.pathType), "idle but ineligible A is not selected")
		})
	}
	t.Run("single-path", func(t *testing.T) {
		st, _ := newSelectionLocal(t, "A")
		reserveSelection(t, st, "A", 1, storiface.FTCache, true)
		require.Equal(t, storiface.ID("A"), selectStorage(t, st, storiface.FTCache, storiface.PathSealing))
	})
	t.Run("existing-sector-preference", func(t *testing.T) {
		st, idx := newSelectionLocal(t, "A", "B")
		reserveSelection(t, st, "A", 1, storiface.FTCache, true)
		idx.existing = []storiface.SectorStorageInfo{{ID: "A", CanSeal: true, CanStore: true}}
		require.Equal(t, storiface.ID("A"), selectStorage(t, st, storiface.FTCache, storiface.PathSealing), "existing location takes precedence over workload")
	})
}
