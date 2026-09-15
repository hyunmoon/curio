package paths

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/storiface"
)

// The red version used Local.Reserve, the old production SDR claim path.
func reserveFreshSDR(st *Local, sr storiface.SectorRef, ft storiface.SectorFileType, ids storiface.SectorPaths) (func(), error) {
	p := st.paths[storiface.ID(storiface.PathByType(ids, ft))]
	return st.ReserveSDR(context.Background(), sr, ft, ids, storiface.FSOverheadSeal, 0, NewSDRReservation(p.sectorPath(sr.ID, ft)))
}

func TestSDRFreshReservationDoesNotCreditOthers(t *testing.T) {
	profile, err := parsePersonalCapacityProfile("sdisk-4slot")
	require.NoError(t, err)
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		for _, legacy := range []bool{true, false} {
			t.Run(ft.String()+map[bool]string{true: "/legacy", false: "/other-attempt"}[legacy], func(t *testing.T) {
				const old = int64(352 << 30)
				sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 42}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
				p := &path{Local: profile.root, Reservations: map[string]int64{}, personalCapacity: profile}
				sp := p.sectorPath(sr.ID, ft)
				other := storiface.SDRTempRoot(sp)
				if legacy {
					other = sp + storiface.TempSuffix
				}
				ls := &sdrReservationStorage{usage: map[string]int64{profile.root: old, other: old}}
				st := &Local{localStorage: ls, paths: map[storiface.ID]*path{"test": p}}
				ids := storiface.SectorPaths{}
				storiface.SetPathByType(&ids, ft, "test")
				release, err := reserveFreshSDR(st, sr, ft, ids)
				require.NoError(t, err)
				defer release()
				stat, _, err := p.stat(ls)
				require.NoError(t, err)
				oh := int64(storiface.FSOverheadSeal[ft]) * int64(32<<30) / storiface.FSOverheadDen
				require.Equal(t, oh, stat.Reserved, "another attempt is not this reservation's materialized bytes")
				require.Equal(t, profile.capacity-old-oh, stat.Available)
				require.Equal(t, old, ls.usage[other], "preserved bytes must remain present")
			})
		}
	}
}

func TestSDRIndependentProcessAccountingModel(t *testing.T) {
	profile, err := parsePersonalCapacityProfile("sdisk-4slot")
	require.NoError(t, err)
	sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 42}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
	s := &sdrReservationStorage{usage: map[string]int64{profile.root: 300}}
	p1 := &path{Local: profile.root, personalCapacity: profile, Reservations: map[string]int64{}}
	p2 := &path{Local: profile.root, personalCapacity: profile, Reservations: map[string]int64{}}
	a, b := NewSDRReservation(p1.sectorPath(sr.ID, storiface.FTKey)), NewSDRReservation(p2.sectorPath(sr.ID, storiface.FTKey))
	s.usage[a.Scratch] = 100
	s.usage[b.Scratch] = 200
	ids := storiface.SectorPaths{Key: "test"}
	oh := int64(storiface.FSOverheadSeal[storiface.FTKey]) * int64(32<<30) / storiface.FSOverheadDen
	for i, p := range []*path{p1, p2} {
		st := &Local{localStorage: s, paths: map[storiface.ID]*path{"test": p}}
		r := []*SDRReservation{a, b}[i]
		release, err := st.ReserveSDR(context.Background(), sr, storiface.FTKey, ids, storiface.FSOverheadSeal, 0, r)
		require.NoError(t, err)
		defer release()
		stat, _, err := p.stat(s)
		require.NoError(t, err)
		own := s.usage[r.Scratch]
		require.Equal(t, oh-own, stat.Reserved)
		require.Equal(t, profile.capacity-300-oh+own, stat.Available)
	}
	// Separate Local handles model separate reservation registries. Each sees
	// the other's actual disk bytes, not its future reservation: no new global
	// cross-process quota/lease guarantee is asserted.
}
