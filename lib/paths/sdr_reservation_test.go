package paths

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/storiface"

	"github.com/filecoin-project/lotus/storage/sealer/fsutil"
)

type sdrReservationStorage struct {
	LocalStorage
	usage map[string]int64
}

func (s *sdrReservationStorage) DiskUsage(p string) (int64, error) {
	if n, ok := s.usage[p]; ok {
		return n, nil
	}
	return 0, os.ErrNotExist
}
func (s *sdrReservationStorage) Stat(string) (fsutil.FsStat, error) {
	return fsutil.FsStat{Available: 3_200_000_000_000, Capacity: 3_200_000_000_000}, nil
}

func TestSDRScratchReservationFourSlots(t *testing.T) {
	profile, err := parsePersonalCapacityProfile("sdisk-4slot")
	require.NoError(t, err)
	const labelBytes = int64(352 << 30)
	for _, fileType := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		overhead := int64(storiface.FSOverheadSeal[fileType]) * int64(32<<30) / storiface.FSOverheadDen
		p := &path{Local: profile.root, Reservations: map[string]int64{}, personalCapacity: profile}
		s := &sdrReservationStorage{usage: map[string]int64{profile.root: 4 * labelBytes}}
		st := &Local{localStorage: s, paths: map[storiface.ID]*path{"test": p}}
		ids := storiface.SectorPaths{}
		storiface.SetPathByType(&ids, fileType, "test")
		for i := 0; i < 4; i++ {
			id := abi.SectorID{Miner: 1000, Number: abi.SectorNumber(i)}
			r := NewSDRReservation(p.sectorPath(id, fileType))
			release, err := st.ReserveSDR(context.Background(), storiface.SectorRef{ID: id, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}, fileType, ids, storiface.FSOverheadSeal, 0, r)
			require.NoError(t, err)
			defer release()
			s.usage[r.Scratch] = labelBytes
		}
		stat, _, err := p.stat(s)
		require.NoError(t, err)
		require.Equal(t, 4*(overhead-labelBytes), stat.Reserved)
		require.Equal(t, int64(5_600_000_000_000)-4*overhead, stat.Available)
	}
}

func TestSDRReservationOwnershipAndExistingCache(t *testing.T) {
	profile, err := parsePersonalCapacityProfile("sdisk-4slot")
	require.NoError(t, err)
	const old = int64(352 << 30)
	sr := storiface.SectorRef{ID: abi.SectorID{Miner: 1000, Number: 42}, ProofType: abi.RegisteredSealProof_StackedDrg32GiBV1_1}
	oh := int64(storiface.FSOverheadSeal[storiface.FTCache]) * int64(32<<30) / storiface.FSOverheadDen
	p := &path{Local: profile.root, personalCapacity: profile, Reservations: map[string]int64{}}
	s := &sdrReservationStorage{usage: map[string]int64{profile.root: old}}
	st := &Local{localStorage: s, paths: map[storiface.ID]*path{"test": p}}
	ids := storiface.SectorPaths{Cache: "test"}
	dest := p.sectorPath(sr.ID, storiface.FTCache)
	a, b := NewSDRReservation(dest), NewSDRReservation(dest)
	require.NotEqual(t, a.Scratch, b.Scratch)
	s.usage[storiface.SDRTempRoot(dest)] = old // Foreign process/crash remains in total usage only.
	ra, err := st.ReserveSDR(context.Background(), sr, storiface.FTCache, ids, storiface.FSOverheadSeal, 0, a)
	require.NoError(t, err)
	rb, err := st.ReserveSDR(context.Background(), sr, storiface.FTCache, ids, storiface.FSOverheadSeal, 0, b)
	require.NoError(t, err)
	defer ra()
	defer rb()
	s.usage[a.Scratch] = 100
	s.usage[b.Scratch] = 200
	s.usage[profile.root] = old + 300
	stat, _, err := p.stat(s)
	require.NoError(t, err)
	require.Equal(t, 2*oh-300, stat.Reserved)
	require.Equal(t, profile.capacity-old-2*oh, stat.Available)
	ra()
	ra() // Repeated release cannot release the other claim.
	stat, _, err = p.stat(s)
	require.NoError(t, err)
	require.Equal(t, oh-200, stat.Reserved)
	rb()
	s.usage[dest] = old
	release, err := st.Reserve(context.Background(), sr, storiface.FTCache, ids, storiface.FSOverheadSeal, 0)
	require.NoError(t, err)
	defer release()
	stat, _, err = p.stat(s)
	require.NoError(t, err)
	require.Equal(t, oh-old, stat.Reserved, "TreeD/TreeRC existing cache credit remains")
}

func TestSDRScratchExcludedFromDeclaration(t *testing.T) {
	root := t.TempDir()
	info, err := os.Stat(root)
	require.NoError(t, err)
	name := storiface.SectorName(abi.SectorID{Miner: 1000, Number: 42})
	for _, n := range []string{name + storiface.TempSuffix, storiface.SDRTempRoot(name)} {
		_, ok := declareableSectorEntry(n, storiface.FTCache, info)
		require.False(t, ok)
	}
	_, ok := declareableSectorEntry(name, storiface.FTCache, info)
	require.True(t, ok)
}
