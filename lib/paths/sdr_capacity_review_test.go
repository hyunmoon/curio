package paths

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/storiface"
)

func TestSDRReclaimedStillRequiresCurrentCapacity(t *testing.T) {
	profile, err := parsePersonalCapacityProfile("sdisk-4slot")
	require.NoError(t, err)
	for _, ft := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 42}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
		p := &path{Local: profile.root, Reservations: map[string]int64{}, personalCapacity: profile}
		storage := &sdrReservationStorage{usage: map[string]int64{profile.root: profile.capacity - 1}}
		local := &Local{localStorage: storage, paths: map[storiface.ID]*path{"A": p}}
		ids := storiface.SectorPaths{}
		storiface.SetPathByType(&ids, ft, "A")
		release, err := local.ReserveSDR(context.Background(), sr, ft, ids, storiface.FSOverheadSeal, 0, NewSDRReservation(p.sectorPath(sr.ID, ft)))
		require.ErrorContains(t, err, "can't reserve")
		require.Nil(t, release)
		require.Empty(t, p.SDRReservations)
	}
}
