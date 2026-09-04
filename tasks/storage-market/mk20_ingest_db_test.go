package storage_market

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	storageingestitest "github.com/filecoin-project/curio/market/storageingest/itest"
)

func TestStorageMarketDBConcurrentMK20IngestionClaimsOnce(t *testing.T) {
	db := storageingestitest.NewYugabyteDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const spID = int64(2000)
	const dealID = "01JTESTMK20CONCURRENTCLAIM000"
	_, err := db.Exec(ctx, `INSERT INTO market_mk20_pipeline (
		id, sp_id, contract, client, piece_cid_v2, piece_cid,
		piece_size, raw_size, offline, indexing, announce, duration,
		piece_aggregation, aggregated
	) VALUES ($1, $2, '', 't0100', 'piece-v2', 'piece-v1', 2048, 2032,
		FALSE, FALSE, FALSE, 1000, 0, TRUE)`, dealID, spID)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			committed, err := db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
				return runMK20IngestTransaction(
					func() (int64, bool, error) {
						return claimMK20IngestRow(tx, spID, dealID)
					},
					func() (mk20SectorAssignment, error) {
						_, err := tx.Exec(`INSERT INTO open_sector_pieces (
							sp_id, sector_number, piece_index, piece_cid, piece_size,
							data_url, data_raw_size, data_delete_on_finalize, is_snap
						) VALUES ($1, 1, 0, 'piece-v1', 2048, 'pieceref:1', 2032, FALSE, FALSE)`, spID)
						if err != nil {
							return mk20SectorAssignment{}, err
						}
						return mk20SectorAssignment{
							sector: 1,
							proof:  abi.RegisteredSealProof_StackedDrg2KiBV1_1,
						}, nil
					},
					func(assignment mk20SectorAssignment, aggrIndex int64) (bool, error) {
						return persistMK20IngestAssignment(tx, spID, dealID, assignment, aggrIndex)
					},
				)
			}, harmonydb.OptionRetry())
			if err != nil {
				errs <- err
				return
			}
			results <- committed
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		t.Fatal(err)
	}

	commits := 0
	for committed := range results {
		if committed {
			commits++
		}
	}
	if commits != 1 {
		t.Fatalf("committed ingestions = %d, want 1", commits)
	}

	var openPieces int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM open_sector_pieces WHERE sp_id = $1`, spID).Scan(&openPieces); err != nil {
		t.Fatal(err)
	}
	if openPieces != 1 {
		t.Fatalf("open-sector pieces = %d, want 1", openPieces)
	}

	var sector int64
	if err := db.QueryRow(ctx, `SELECT sector FROM market_mk20_pipeline WHERE id = $1 AND aggr_index = 0`, dealID).Scan(&sector); err != nil {
		t.Fatal(err)
	}
	if sector != 1 {
		t.Fatalf("pipeline sector = %d, want 1", sector)
	}
}
