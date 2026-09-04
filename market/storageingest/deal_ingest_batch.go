package storageingest

import (
	"context"
	"errors"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/tasks/seal"
)

// sealBatchSize bounds both the transaction lifetime and write-intent set,
// while keeping the normal path set-based and large enough to drain backlogs.
const sealBatchSize = 64

type sealBatchParams struct {
	spID                 int64
	proof                int64
	sectorSize           int64
	isSnap               bool
	maxWait              time.Duration
	sealBeforeChainEpoch abi.ChainEpoch
}

type sealBatchResult struct {
	candidates []abi.SectorNumber
	committed  int
}

type sealBatchFunc func(failed map[abi.SectorNumber]struct{}, limit int) (sealBatchResult, error)
type sealSectorFunc func(sector abi.SectorNumber) (bool, error)

func newSealWake() chan struct{} {
	wake := make(chan struct{}, 1)
	wakeSealLoop(wake)
	return wake
}

func wakeSealLoop(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func runSealLoop(ctx context.Context, ticks <-chan time.Time, wake <-chan struct{}, sealBatch func() error, onError func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		case <-wake:
		}

		if err := sealBatch(); err != nil {
			onError(err)
		}
	}
}

func drainSealBatches(ctx context.Context, db *harmonydb.DB, params sealBatchParams) error {
	return drainSealBatchesWith(ctx, params,
		func(failed map[abi.SectorNumber]struct{}, limit int) (sealBatchResult, error) {
			return sealReadyBatch(ctx, db, params, failed, limit)
		},
		func(sector abi.SectorNumber) (bool, error) {
			return sealReadySector(ctx, db, params, sector)
		},
	)
}

func drainSealBatchesWith(ctx context.Context, params sealBatchParams, sealBatch sealBatchFunc, sealSector sealSectorFunc) error {
	failed := make(map[abi.SectorNumber]struct{})
	var sectorErrors []error

	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(sectorErrors, err)...)
		}

		started := time.Now()
		result, err := sealBatch(failed, sealBatchSize)
		if err == nil {
			if len(result.candidates) == 0 {
				return errors.Join(sectorErrors...)
			}

			anotherBatch := len(result.candidates) == sealBatchSize
			log.Infow("committed storage ingest seal batch",
				"sp_id", params.spID,
				"candidate_count", len(result.candidates),
				"committed_count", result.committed,
				"failed_count", 0,
				"elapsed", time.Since(started),
				"another_batch", anotherBatch,
			)
			if !anotherBatch {
				return errors.Join(sectorErrors...)
			}
			continue
		}

		if len(result.candidates) == 0 {
			return errors.Join(append(sectorErrors, err)...)
		}

		log.Warnw("storage ingest seal batch failed; isolating candidates",
			"sp_id", params.spID,
			"candidate_count", len(result.candidates),
			"error", err,
		)

		committed := 0
		failedCount := 0
		for _, sector := range result.candidates {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(sectorErrors, err)...)
			}

			moved, moveErr := sealSector(sector)
			if moveErr != nil {
				failed[sector] = struct{}{}
				failedCount++
				wrapped := xerrors.Errorf("sealing provider %d sector %d: %w", params.spID, sector, moveErr)
				sectorErrors = append(sectorErrors, wrapped)
				log.Errorw("failed to move open sector to sealing pipeline",
					"sp_id", params.spID,
					"sector_number", sector,
					"error", moveErr,
				)
				continue
			}
			if moved {
				committed++
			}
		}

		anotherBatch := len(result.candidates) == sealBatchSize
		log.Infow("committed isolated storage ingest seal batch",
			"sp_id", params.spID,
			"candidate_count", len(result.candidates),
			"committed_count", committed,
			"failed_count", failedCount,
			"elapsed", time.Since(started),
			"another_batch", anotherBatch,
		)
		if !anotherBatch {
			return errors.Join(sectorErrors...)
		}
	}
}

func sealReadyBatch(ctx context.Context, db *harmonydb.DB, params sealBatchParams, failed map[abi.SectorNumber]struct{}, limit int) (sealBatchResult, error) {
	var result sealBatchResult
	committed, err := db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		result = sealBatchResult{}
		if err := seal.LockSectorState(tx, params.spID); err != nil {
			return false, err
		}

		candidates, err := selectSealCandidates(tx, params, failed, limit)
		if err != nil {
			return false, err
		}
		result.candidates = candidates
		if len(candidates) == 0 {
			return true, nil
		}

		result.committed, err = transferSealCandidates(tx, params, candidates)
		if err != nil {
			return false, err
		}
		return true, nil
	}, harmonydb.OptionRetry())
	if err != nil {
		return result, err
	}
	if !committed {
		return result, xerrors.Errorf("seal batch transaction did not commit")
	}
	return result, nil
}

func selectSealCandidates(tx *harmonydb.Tx, params sealBatchParams, failed map[abi.SectorNumber]struct{}, limit int) ([]abi.SectorNumber, error) {
	failedNumbers := make([]int64, 0, len(failed))
	for sector := range failed {
		failedNumbers = append(failedNumbers, int64(sector))
	}

	var rows []struct {
		Sector abi.SectorNumber `db:"sector_number"`
	}
	err := tx.Select(&rows, `SELECT sector_number
		FROM open_sector_pieces
		WHERE sp_id = $1
		  AND is_snap = $2
		  AND NOT (sector_number = ANY($3::bigint[]))
		GROUP BY sector_number
		HAVING SUM(piece_size) = $4
		    OR MIN(created_at) < $5
		    OR MIN(COALESCE(direct_start_epoch, f05_deal_start_epoch, 0)) < $6
		ORDER BY sector_number
		LIMIT $7`,
		params.spID,
		params.isSnap,
		failedNumbers,
		params.sectorSize,
		time.Now().Add(-params.maxWait),
		params.sealBeforeChainEpoch,
		limit,
	)
	if err != nil {
		return nil, xerrors.Errorf("selecting seal candidates for provider %d: %w", params.spID, err)
	}

	candidates := make([]abi.SectorNumber, 0, len(rows))
	for _, row := range rows {
		candidates = append(candidates, row.Sector)
	}
	return candidates, nil
}

func transferSealCandidates(tx *harmonydb.Tx, params sealBatchParams, candidates []abi.SectorNumber) (int, error) {
	sectorNumbers := make([]int64, 0, len(candidates))
	for _, sector := range candidates {
		sectorNumbers = append(sectorNumbers, int64(sector))
	}

	var insertedRows *harmonydb.Query
	var err error
	if params.isSnap {
		insertedRows, err = tx.Query(`INSERT INTO sectors_snap_pipeline (sp_id, sector_number, upgrade_proof)
			SELECT $2, sector_number, $3
			FROM unnest($1::bigint[]) AS candidate(sector_number)
			ON CONFLICT (sp_id, sector_number) DO NOTHING
			RETURNING sector_number`, sectorNumbers, params.spID, params.proof)
	} else {
		insertedRows, err = tx.Query(`INSERT INTO sectors_sdr_pipeline (sp_id, sector_number, reg_seal_proof)
			SELECT $2, sector_number, $3
			FROM unnest($1::bigint[]) AS candidate(sector_number)
			ON CONFLICT (sp_id, sector_number) DO NOTHING
			RETURNING sector_number`, sectorNumbers, params.spID, params.proof)
	}
	if err != nil {
		return 0, xerrors.Errorf("adding seal candidates for provider %d: %w", params.spID, err)
	}

	inserted := make([]int64, 0, len(candidates))
	for insertedRows.Next() {
		var sector int64
		if err := insertedRows.Scan(&sector); err != nil {
			insertedRows.Close()
			return 0, xerrors.Errorf("reading inserted seal candidate for provider %d: %w", params.spID, err)
		}
		inserted = append(inserted, sector)
	}
	if err := insertedRows.Err(); err != nil {
		insertedRows.Close()
		return 0, xerrors.Errorf("inserting seal candidates for provider %d: %w", params.spID, err)
	}
	insertedRows.Close()
	if len(inserted) != len(candidates) {
		return 0, xerrors.Errorf("adding seal candidates for provider %d: inserted %d of %d candidates", params.spID, len(inserted), len(candidates))
	}

	var transferRows *harmonydb.Query
	if params.isSnap {
		transferRows, err = tx.Query(`SELECT transfer_and_delete_sorted_open_piece_snap($2, sector_number)
			FROM unnest($1::bigint[]) AS inserted(sector_number)
			ORDER BY sector_number`, inserted, params.spID)
	} else {
		transferRows, err = tx.Query(`SELECT transfer_and_delete_sorted_open_piece($2, sector_number)
			FROM unnest($1::bigint[]) AS inserted(sector_number)
			ORDER BY sector_number`, inserted, params.spID)
	}
	if err != nil {
		return 0, xerrors.Errorf("starting transfer for provider %d: %w", params.spID, err)
	}
	defer transferRows.Close()

	moved := 0
	for transferRows.Next() {
		moved++
	}
	if err := transferRows.Err(); err != nil {
		return 0, xerrors.Errorf("transferring open sectors for provider %d: %w", params.spID, err)
	}
	if moved != len(candidates) {
		return moved, xerrors.Errorf("transferring open sectors for provider %d: moved %d of %d candidates", params.spID, moved, len(candidates))
	}
	return moved, nil
}

func sealReadySector(ctx context.Context, db *harmonydb.DB, params sealBatchParams, sector abi.SectorNumber) (bool, error) {
	moved := false
	committed, err := db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		moved = false
		if err := seal.LockSectorState(tx, params.spID); err != nil {
			return false, err
		}

		var stillOpen bool
		err := tx.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM open_sector_pieces
			WHERE sp_id = $1 AND sector_number = $2 AND is_snap = $3
		)`, params.spID, sector, params.isSnap).Scan(&stillOpen)
		if err != nil {
			return false, xerrors.Errorf("revalidating provider %d sector %d: %w", params.spID, sector, err)
		}
		if !stillOpen {
			return true, nil
		}

		var inserted int
		if params.isSnap {
			inserted, err = tx.Exec(`INSERT INTO sectors_snap_pipeline (sp_id, sector_number, upgrade_proof)
				VALUES ($1, $2, $3)
				ON CONFLICT (sp_id, sector_number) DO NOTHING`, params.spID, sector, params.proof)
		} else {
			inserted, err = tx.Exec(`INSERT INTO sectors_sdr_pipeline (sp_id, sector_number, reg_seal_proof)
				VALUES ($1, $2, $3)
				ON CONFLICT (sp_id, sector_number) DO NOTHING`, params.spID, sector, params.proof)
		}
		if err != nil {
			return false, xerrors.Errorf("adding provider %d sector %d to pipeline: %w", params.spID, sector, err)
		}
		if inserted != 1 {
			return false, xerrors.Errorf("provider %d sector %d already has a pipeline row while open pieces remain", params.spID, sector)
		}

		if params.isSnap {
			_, err = tx.Exec(`SELECT transfer_and_delete_sorted_open_piece_snap($1, $2)`, params.spID, sector)
		} else {
			_, err = tx.Exec(`SELECT transfer_and_delete_sorted_open_piece($1, $2)`, params.spID, sector)
		}
		if err != nil {
			return false, xerrors.Errorf("transferring provider %d sector %d: %w", params.spID, sector, err)
		}
		moved = true
		return true, nil
	}, harmonydb.OptionRetry())
	if err != nil {
		return false, err
	}
	if !committed {
		return false, xerrors.Errorf("provider %d sector %d transaction did not commit", params.spID, sector)
	}
	return moved, nil
}
