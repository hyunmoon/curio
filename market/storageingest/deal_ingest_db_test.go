package storageingest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	storageingestitest "github.com/filecoin-project/curio/market/storageingest/itest"
	"github.com/filecoin-project/curio/tasks/seal"

	"github.com/filecoin-project/lotus/chain/types"
)

const testSectorSize2KiB = int64(2 << 10)

func TestStorageIngestDBBoundedDrainIsolatesPoison(t *testing.T) {
	db := storageIngestTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const spID = int64(1000)
	const total = 2*sealBatchSize + 1
	poison := int64(sealBatchSize + 6)

	insertTestOpenSectors(t, db, spID, 1, total, false)
	_, err := db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline (sp_id, sector_number, reg_seal_proof)
		VALUES ($1, $2, $3)`, spID, poison, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
	if err != nil {
		t.Fatal(err)
	}

	err = drainSealBatches(ctx, db, sealBatchParams{
		spID:                 spID,
		proof:                int64(abi.RegisteredSealProof_StackedDrg2KiBV1_1),
		sectorSize:           testSectorSize2KiB,
		maxWait:              24 * time.Hour,
		sealBeforeChainEpoch: 0,
	})
	if err == nil {
		t.Fatal("expected poison-sector error")
	}

	assertDBCount(t, db, countOpenSectors, 1, spID)
	assertDBCount(t, db, countOpenSector, 1, spID, poison)
	assertDBCount(t, db, countInitialSectors, total-1, spID)
	assertDBCount(t, db, countPipelineSectors, total, spID)
}

func TestStorageIngestDBConcurrentDrainersMoveEachSectorOnce(t *testing.T) {
	db := storageIngestTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const spID = int64(1001)
	const total = sealBatchSize + 1
	insertTestOpenSectors(t, db, spID, 1, total, false)

	params := sealBatchParams{
		spID:                 spID,
		proof:                int64(abi.RegisteredSealProof_StackedDrg2KiBV1_1),
		sectorSize:           testSectorSize2KiB,
		maxWait:              24 * time.Hour,
		sealBeforeChainEpoch: 0,
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- drainSealBatches(ctx, db, params)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	assertDBCount(t, db, countOpenSectors, 0, spID)
	assertDBCount(t, db, countPipelineSectors, total, spID)
	assertDBCount(t, db, countInitialSectors, total, spID)
}

func TestStorageIngestDBConcurrentSectorNumberAllocation(t *testing.T) {
	db := storageIngestTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	maddr, err := address.NewIDAddress(1002)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan abi.SectorNumber, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var allocated []abi.SectorNumber
			committed, err := db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
				var allocErr error
				allocated, allocErr = seal.AllocateSectorNumbers(ctx, emptyAllocatedAPI{}, tx, maddr, 1)
				return allocErr == nil, allocErr
			}, harmonydb.OptionRetry())
			if err != nil {
				errs <- err
				return
			}
			if !committed || len(allocated) != 1 {
				errs <- fmt.Errorf("allocation committed=%t count=%d", committed, len(allocated))
				return
			}
			results <- allocated[0]
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatal(err)
	}

	seen := make(map[abi.SectorNumber]struct{})
	for sector := range results {
		seen[sector] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("allocated sector numbers = %v, want two distinct numbers", seen)
	}
}

func TestStorageIngestDBSelectSealCandidatesPreservesEligibility(t *testing.T) {
	db := storageIngestTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const spID = int64(1003)

	committed, err := db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		rows := []struct {
			sector    int64
			size      int64
			createdAt time.Time
			start     *int64
			isSnap    bool
		}{
			{sector: 1, size: testSectorSize2KiB, createdAt: time.Now()},
			{sector: 2, size: testSectorSize2KiB / 2, createdAt: time.Now().Add(-2 * time.Hour), start: ptr(int64(200))},
			{sector: 3, size: testSectorSize2KiB / 2, createdAt: time.Now(), start: ptr(int64(50))},
			{sector: 4, size: testSectorSize2KiB / 2, createdAt: time.Now(), start: ptr(int64(200))},
			{sector: 5, size: testSectorSize2KiB, createdAt: time.Now(), isSnap: true},
		}
		for _, row := range rows {
			_, err := tx.Exec(`INSERT INTO open_sector_pieces (
				sp_id, sector_number, piece_index, piece_cid, piece_size,
				data_url, data_raw_size, data_delete_on_finalize, created_at,
				direct_start_epoch, is_snap
			) VALUES ($1, $2, 0, $3, $4, $5, 1016, FALSE, $6, $7, $8)`,
				spID, row.sector, fmt.Sprintf("eligibility-piece-%d", row.sector), row.size,
				fmt.Sprintf("pieceref:%d", row.sector), row.createdAt, row.start, row.isSnap)
			if err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("eligibility fixture did not commit")
	}

	var candidates []abi.SectorNumber
	committed, err = db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		if err := seal.LockSectorState(tx, spID); err != nil {
			return false, err
		}
		var selectErr error
		candidates, selectErr = selectSealCandidates(tx, sealBatchParams{
			spID:                 spID,
			sectorSize:           testSectorSize2KiB,
			maxWait:              time.Hour,
			sealBeforeChainEpoch: 100,
		}, nil, sealBatchSize)
		return selectErr == nil, selectErr
	}, harmonydb.OptionRetry())
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("candidate selection transaction did not commit")
	}
	want := []abi.SectorNumber{1, 2, 3}
	if len(candidates) != len(want) {
		t.Fatalf("seal candidates = %v, want %v", candidates, want)
	}
	for i := range want {
		if candidates[i] != want[i] {
			t.Fatalf("seal candidates = %v, want %v", candidates, want)
		}
	}
}

func TestStorageIngestDBSnapBatchTransfer(t *testing.T) {
	db := storageIngestTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const (
		spID   = int64(1004)
		sector = int64(1)
	)

	_, err := db.Exec(ctx, `INSERT INTO sectors_meta (
		sp_id, sector_num, reg_seal_proof, ticket_epoch, ticket_value,
		orig_sealed_cid, orig_unsealed_cid, cur_sealed_cid, cur_unsealed_cid,
		seed_epoch, seed_value
	) VALUES ($1, $2, $3, 0, '\x00', 'sealed', 'unsealed', 'sealed', 'unsealed', 0, '\x00')`,
		spID, sector, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
	if err != nil {
		t.Fatal(err)
	}
	insertTestOpenSectors(t, db, spID, int(sector), 1, true)

	err = drainSealBatches(ctx, db, sealBatchParams{
		spID:                 spID,
		proof:                int64(abi.RegisteredUpdateProof_StackedDrg2KiBV1),
		sectorSize:           testSectorSize2KiB,
		isSnap:               true,
		maxWait:              24 * time.Hour,
		sealBeforeChainEpoch: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	assertDBCount(t, db, countOpenSectors, 0, spID)
	assertDBCount(t, db, countSnapPipelineSectors, 1, spID)
	assertDBCount(t, db, countSnapInitialSectors, 1, spID)
}

type emptyAllocatedAPI struct{}

func (emptyAllocatedAPI) StateMinerAllocated(context.Context, address.Address, types.TipSetKey) (*bitfield.BitField, error) {
	empty := bitfield.New()
	return &empty, nil
}

func storageIngestTestDB(t *testing.T) *harmonydb.DB {
	t.Helper()
	return storageingestitest.NewYugabyteDB(t)
}

func insertTestOpenSectors(t *testing.T, db *harmonydb.DB, spID int64, first, count int, isSnap bool) {
	t.Helper()
	committed, err := db.BeginTransaction(context.Background(), func(tx *harmonydb.Tx) (bool, error) {
		for sector := first; sector < first+count; sector++ {
			_, err := tx.Exec(`INSERT INTO open_sector_pieces (
				sp_id, sector_number, piece_index, piece_cid, piece_size,
				data_url, data_raw_size, data_delete_on_finalize, is_snap
			) VALUES ($1, $2, 0, $3, $4, $5, $6, FALSE, $7)`,
				spID,
				sector,
				fmt.Sprintf("test-piece-%d", sector),
				testSectorSize2KiB,
				fmt.Sprintf("pieceref:%d", sector),
				int64(2032),
				isSnap,
			)
			if err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("test-sector insert did not commit")
	}
}

type countQuery int

const (
	countOpenSectors countQuery = iota
	countOpenSector
	countInitialSectors
	countPipelineSectors
	countSnapInitialSectors
	countSnapPipelineSectors
)

func assertDBCount(t *testing.T, db *harmonydb.DB, query countQuery, want int, args ...any) {
	t.Helper()
	var count int
	var err error
	switch query {
	case countOpenSectors:
		err = db.QueryRow(context.Background(), `SELECT COUNT(*) FROM open_sector_pieces WHERE sp_id = $1`, args...).Scan(&count)
	case countOpenSector:
		err = db.QueryRow(context.Background(), `SELECT COUNT(*) FROM open_sector_pieces WHERE sp_id = $1 AND sector_number = $2`, args...).Scan(&count)
	case countInitialSectors:
		err = db.QueryRow(context.Background(), `SELECT COUNT(*) FROM sectors_sdr_initial_pieces WHERE sp_id = $1`, args...).Scan(&count)
	case countPipelineSectors:
		err = db.QueryRow(context.Background(), `SELECT COUNT(*) FROM sectors_sdr_pipeline WHERE sp_id = $1`, args...).Scan(&count)
	case countSnapInitialSectors:
		err = db.QueryRow(context.Background(), `SELECT COUNT(*) FROM sectors_snap_initial_pieces WHERE sp_id = $1`, args...).Scan(&count)
	case countSnapPipelineSectors:
		err = db.QueryRow(context.Background(), `SELECT COUNT(*) FROM sectors_snap_pipeline WHERE sp_id = $1`, args...).Scan(&count)
	default:
		t.Fatalf("unknown count query %d", query)
	}
	if err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("count query %d = %d, want %d", query, count, want)
	}
}

func ptr[T any](value T) *T {
	return &value
}
