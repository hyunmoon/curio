package paths

import (
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
	const overhead = int64((32 << 30) * 141 / 10)
	for _, fileType := range []storiface.SectorFileType{storiface.FTCache, storiface.FTKey} {
		p := &path{Local: profile.root, Reserved: 4 * overhead, Reservations: map[string]int64{}, personalCapacity: profile}
		s := &sdrReservationStorage{usage: map[string]int64{profile.root: 4 * labelBytes}}
		for i := 0; i < 4; i++ {
			id := abi.SectorID{Miner: 1000, Number: abi.SectorNumber(i)}
			p.Reservations[(&sectorFile{id, fileType}).String()] = overhead
			s.usage[storiface.SDRTempRoot(p.sectorPath(id, fileType))] = labelBytes
		}
		stat, _, err := p.stat(s)
		require.NoError(t, err)
		require.Equal(t, 4*(overhead-labelBytes), stat.Reserved)
		require.Equal(t, int64(5_600_000_000_000)-4*overhead, stat.Available)
		// Identical credit for a newly requested reservation, before registration.
		id := abi.SectorID{Miner: 1000, Number: 5}
		sp := p.sectorPath(id, fileType)
		s.usage[storiface.SDRTempRoot(sp)] = labelBytes
		_, onDisk, err := p.stat(s, statExistingSectorForReservation{id, fileType, overhead})
		require.NoError(t, err)
		require.Equal(t, labelBytes, onDisk)
		// Legacy/published data and independently owned scratch can coexist.
		s.usage[sp] = labelBytes
		_, onDisk, err = p.stat(s, statExistingSectorForReservation{id, fileType, overhead})
		require.NoError(t, err)
		require.Equal(t, overhead, onDisk, "credit must remain capped")
		delete(s.usage, sp)
		s.usage[sp+storiface.TempSuffix] = 1
		_, onDisk, err = p.stat(s, statExistingSectorForReservation{id, fileType, overhead})
		require.NoError(t, err)
		require.Equal(t, labelBytes+1, onDisk)
	}
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
