package storage_market

import (
	"fmt"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"

	"github.com/filecoin-project/lotus/chain/proofs"
)

const mk20OffsetLockTargetSQL = `
	SELECT 1 AS present
	FROM market_mk20_pipeline
	WHERE id = $1
	  AND aggr_index = $2
	  AND sp_id = $3
	  AND sector = $4
	  AND reg_seal_proof = $5
	  AND piece_cid = $6
	  AND piece_size = $7
	  AND sector_offset IS NULL
	FOR UPDATE`

const mk20OffsetInitialPiecesSQL = `
	SELECT piece_cid, piece_size, piece_index
	FROM sectors_sdr_initial_pieces
	WHERE sp_id = $1 AND sector_number = $2

	UNION ALL

	SELECT piece_cid, piece_size, piece_index
	FROM sectors_snap_initial_pieces
	WHERE sp_id = $1 AND sector_number = $2

	ORDER BY piece_index ASC`

// Retained metadata is eligible only after the mutable open and pipeline
// sources have gone away. The subsequent commitment check establishes local
// layout consistency; it does not establish chain state or data availability.
const mk20OffsetRetainedLayoutSQL = `
	SELECT
		sm.reg_seal_proof,
		sm.cur_unsealed_cid,
		smp.piece_num,
		smp.piece_cid,
		smp.piece_size
	FROM sectors_meta sm
	JOIN sectors_meta_pieces smp
	  ON smp.sp_id = sm.sp_id
	 AND smp.sector_num = sm.sector_num
	WHERE sm.sp_id = $1
	  AND sm.sector_num = $2
	  AND NOT EXISTS (
		SELECT 1 FROM open_sector_pieces osp
		WHERE osp.sp_id = sm.sp_id AND osp.sector_number = sm.sector_num
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM sectors_sdr_pipeline sdr
		WHERE sdr.sp_id = sm.sp_id AND sdr.sector_number = sm.sector_num
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM sectors_snap_pipeline snap
		WHERE snap.sp_id = sm.sp_id AND snap.sector_number = sm.sector_num
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM sectors_sdr_initial_pieces sip
		WHERE sip.sp_id = sm.sp_id AND sip.sector_number = sm.sector_num
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM sectors_snap_initial_pieces sip
		WHERE sip.sp_id = sm.sp_id AND sip.sector_number = sm.sector_num
	  )
	ORDER BY smp.piece_num ASC
	FOR UPDATE OF sm, smp`

const mk20OffsetUpdateSQL = `
	UPDATE market_mk20_pipeline
	SET sector_offset = $1
	WHERE id = $2
	  AND aggr_index = $3
	  AND sp_id = $4
	  AND sector = $5
	  AND reg_seal_proof = $6
	  AND piece_cid = $7
	  AND piece_size = $8
	  AND sector_offset IS NULL`

const mk20OffsetUpdateFromMetadataSQL = `
	UPDATE market_mk20_pipeline
	SET sector_offset = $1
	WHERE id = $2
	  AND aggr_index = $3
	  AND sp_id = $4
	  AND sector = $5
	  AND reg_seal_proof = $6
	  AND piece_cid = $7
	  AND piece_size = $8
	  AND sector_offset IS NULL
	  AND EXISTS (
		SELECT 1
		FROM sectors_meta sm
		WHERE sm.sp_id = $4
		  AND sm.sector_num = $5
		  AND sm.reg_seal_proof = $6
		  AND sm.cur_unsealed_cid = $9
	  )
	  AND ARRAY(
		SELECT smp.piece_num
		FROM sectors_meta_pieces smp
		WHERE smp.sp_id = $4 AND smp.sector_num = $5
		ORDER BY smp.piece_num
	  ) = $10::bigint[]
	  AND ARRAY(
		SELECT smp.piece_cid
		FROM sectors_meta_pieces smp
		WHERE smp.sp_id = $4 AND smp.sector_num = $5
		ORDER BY smp.piece_num
	  ) = $11::text[]
	  AND ARRAY(
		SELECT smp.piece_size
		FROM sectors_meta_pieces smp
		WHERE smp.sp_id = $4 AND smp.sector_num = $5
		ORDER BY smp.piece_num
	  ) = $12::bigint[]
	  AND NOT EXISTS (
		SELECT 1 FROM open_sector_pieces osp
		WHERE osp.sp_id = $4 AND osp.sector_number = $5
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM sectors_sdr_pipeline sdr
		WHERE sdr.sp_id = $4 AND sdr.sector_number = $5
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM sectors_snap_pipeline snap
		WHERE snap.sp_id = $4 AND snap.sector_number = $5
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM sectors_sdr_initial_pieces sip
		WHERE sip.sp_id = $4 AND sip.sector_number = $5
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM sectors_snap_initial_pieces sip
		WHERE sip.sp_id = $4 AND sip.sector_number = $5
	  )`

type mk20OffsetTarget struct {
	ID               string
	SPID             int64
	AggregationIndex int64
	Sector           int64
	RegSealProof     int64
	PieceCID         string
	PieceSize        int64
}

type mk20RetainedPiece struct {
	Index int64  `db:"piece_num"`
	CID   string `db:"piece_cid"`
	Size  int64  `db:"piece_size"`
}

type mk20RetainedLayout struct {
	RegSealProof int64
	UnsealedCID  string
	Pieces       []mk20RetainedPiece
}

type mk20OffsetMetadataGuard struct {
	UnsealedCID string
	PieceNums   []int64
	PieceCIDs   []string
	PieceSizes  []int64
}

type mk20OffsetStore interface {
	lockTarget(mk20OffsetTarget) (bool, error)
	loadInitialPieces(spID, sector int64) ([]mk20SectorPiece, error)
	loadRetainedLayout(spID, sector int64) (mk20RetainedLayout, bool, error)
	persistOffset(mk20OffsetTarget, abi.PaddedPieceSize, *mk20OffsetMetadataGuard) (bool, error)
}

func resolveMK20PieceOffset(store mk20OffsetStore, target mk20OffsetTarget) (bool, error) {
	// Stabilize the exact MK20 assignment before selecting either layout source.
	// The transaction adapter also locks retained rows and rechecks their exact
	// ordered contents and lifecycle guards in the conditional offset update.
	locked, err := store.lockTarget(target)
	if err != nil {
		return false, xerrors.Errorf("locking MK20 offset target: %w", err)
	}
	if !locked {
		return false, nil
	}

	pieces, err := store.loadInitialPieces(target.SPID, target.Sector)
	if err != nil {
		return false, xerrors.Errorf("getting pieces for sector: %w", err)
	}
	if len(pieces) > 0 {
		offset, found := findMK20PieceOffset(pieces, target.PieceCID, abi.PaddedPieceSize(target.PieceSize))
		if !found {
			return false, xerrors.Errorf("failed to find deal offset for piece %s", target.PieceCID)
		}
		updated, err := store.persistOffset(target, offset, nil)
		if err != nil {
			return false, err
		}
		return updated, nil
	}
	layout, found, err := store.loadRetainedLayout(target.SPID, target.Sector)
	if err != nil {
		return false, xerrors.Errorf("getting retained sector metadata: %w", err)
	}
	if !found {
		return false, nil
	}

	offset, err := validateMK20RetainedLayout(layout, target)
	if err != nil {
		return false, xerrors.Errorf("validating retained sector metadata: %w", err)
	}

	guard := &mk20OffsetMetadataGuard{
		UnsealedCID: layout.UnsealedCID,
		PieceNums:   make([]int64, len(layout.Pieces)),
		PieceCIDs:   make([]string, len(layout.Pieces)),
		PieceSizes:  make([]int64, len(layout.Pieces)),
	}
	for index, piece := range layout.Pieces {
		guard.PieceNums[index] = piece.Index
		guard.PieceCIDs[index] = piece.CID
		guard.PieceSizes[index] = piece.Size
	}

	updated, err := store.persistOffset(target, offset, guard)
	if err != nil {
		return false, err
	}
	return updated, nil
}

func validateMK20RetainedLayout(layout mk20RetainedLayout, target mk20OffsetTarget) (abi.PaddedPieceSize, error) {
	if len(layout.Pieces) == 0 {
		return 0, xerrors.Errorf("sector metadata has no pieces")
	}
	if layout.RegSealProof != target.RegSealProof {
		return 0, xerrors.Errorf("sector metadata proof changed: expected %d, got %d", target.RegSealProof, layout.RegSealProof)
	}

	proofType := abi.RegisteredSealProof(layout.RegSealProof)
	sectorSize, err := proofType.SectorSize()
	if err != nil {
		return 0, xerrors.Errorf("getting sector size for proof %d: %w", layout.RegSealProof, err)
	}
	sectorUnpadded := abi.PaddedPieceSize(sectorSize).Unpadded()

	expectedUnsealedCID, err := cid.Parse(layout.UnsealedCID)
	if err != nil {
		return 0, xerrors.Errorf("parsing retained unsealed CID: %w", err)
	}
	if _, err := commcid.CIDToDataCommitmentV1(expectedUnsealedCID); err != nil {
		return 0, xerrors.Errorf("validating retained unsealed CID: %w", err)
	}

	pieceInfos := make([]abi.PieceInfo, 0, len(layout.Pieces))
	sectorPieces := make([]mk20SectorPiece, 0, len(layout.Pieces))
	var offset abi.UnpaddedPieceSize
	var targetMatches int

	for position, piece := range layout.Pieces {
		if piece.Index != int64(position) {
			return 0, xerrors.Errorf("invalid piece order at position %d: piece_num=%d", position, piece.Index)
		}
		if piece.Size <= 0 {
			return 0, xerrors.Errorf("invalid piece size at piece_num %d: %d", piece.Index, piece.Size)
		}
		if position > 0 && piece.Size > layout.Pieces[position-1].Size {
			return 0, xerrors.Errorf("invalid piece size order at piece_num %d: %d follows %d", piece.Index, piece.Size, layout.Pieces[position-1].Size)
		}

		pieceSize := abi.PaddedPieceSize(piece.Size)
		if err := pieceSize.Validate(); err != nil {
			return 0, xerrors.Errorf("validating piece size at piece_num %d: %w", piece.Index, err)
		}
		pieceCID, err := cid.Parse(piece.CID)
		if err != nil {
			return 0, xerrors.Errorf("parsing piece CID at piece_num %d: %w", piece.Index, err)
		}
		if _, err := commcid.CIDToPieceCommitmentV1(pieceCID); err != nil {
			return 0, xerrors.Errorf("validating piece CID at piece_num %d: %w", piece.Index, err)
		}

		_, padding := proofs.GetRequiredPadding(offset.Padded(), pieceSize)
		paddingUnpadded := padding.Unpadded()
		if paddingUnpadded > sectorUnpadded-offset {
			return 0, xerrors.Errorf("piece layout padding exceeds sector size at piece_num %d", piece.Index)
		}
		offset += paddingUnpadded

		pieceUnpadded := pieceSize.Unpadded()
		if pieceUnpadded > sectorUnpadded-offset {
			return 0, xerrors.Errorf("piece layout exceeds sector size at piece_num %d", piece.Index)
		}

		if piece.CID == target.PieceCID && piece.Size == target.PieceSize {
			targetMatches++
		}
		offset += pieceUnpadded

		pieceInfos = append(pieceInfos, abi.PieceInfo{PieceCID: pieceCID, Size: pieceSize})
		sectorPieces = append(sectorPieces, mk20SectorPiece{CID: piece.CID, Size: pieceSize, Index: piece.Index})
	}

	if targetMatches != 1 {
		return 0, xerrors.Errorf("expected one retained target occurrence, found %d", targetMatches)
	}

	// GenerateUnsealedCID uses the same inter-piece alignment and trailing
	// zero-padding as the sector data path, without reading piece payloads.
	calculatedUnsealedCID, err := proofs.GenerateUnsealedCID(proofType, pieceInfos)
	if err != nil {
		return 0, xerrors.Errorf("reconstructing unsealed CID: %w", err)
	}
	if !calculatedUnsealedCID.Equals(expectedUnsealedCID) {
		return 0, xerrors.Errorf("retained piece layout does not match current unsealed CID")
	}

	pieceSize := abi.PaddedPieceSize(target.PieceSize)
	offsetPadded, found := findMK20PieceOffset(sectorPieces, target.PieceCID, pieceSize)
	if !found {
		return 0, xerrors.Errorf("retained target disappeared during validation")
	}
	return offsetPadded, nil
}

type harmonyMK20OffsetStore struct {
	tx *harmonydb.Tx
}

func (s *harmonyMK20OffsetStore) lockTarget(target mk20OffsetTarget) (bool, error) {
	var matches []struct {
		Present int `db:"present"`
	}
	if err := s.tx.Select(&matches, mk20OffsetLockTargetSQL,
		target.ID, target.AggregationIndex, target.SPID, target.Sector,
		target.RegSealProof, target.PieceCID, target.PieceSize); err != nil {
		return false, err
	}
	if len(matches) > 1 {
		return false, xerrors.Errorf("expected at most one MK20 offset target, got %d", len(matches))
	}
	return len(matches) == 1, nil
}

func (s *harmonyMK20OffsetStore) loadInitialPieces(spID, sector int64) ([]mk20SectorPiece, error) {
	var pieces []mk20SectorPiece
	if err := s.tx.Select(&pieces, mk20OffsetInitialPiecesSQL, spID, sector); err != nil {
		return nil, err
	}
	return pieces, nil
}

func (s *harmonyMK20OffsetStore) loadRetainedLayout(spID, sector int64) (mk20RetainedLayout, bool, error) {
	var rows []struct {
		RegSealProof int64  `db:"reg_seal_proof"`
		UnsealedCID  string `db:"cur_unsealed_cid"`
		PieceNum     int64  `db:"piece_num"`
		PieceCID     string `db:"piece_cid"`
		PieceSize    int64  `db:"piece_size"`
	}
	if err := s.tx.Select(&rows, mk20OffsetRetainedLayoutSQL, spID, sector); err != nil {
		return mk20RetainedLayout{}, false, err
	}
	if len(rows) == 0 {
		return mk20RetainedLayout{}, false, nil
	}

	layout := mk20RetainedLayout{
		RegSealProof: rows[0].RegSealProof,
		UnsealedCID:  rows[0].UnsealedCID,
		Pieces:       make([]mk20RetainedPiece, 0, len(rows)),
	}
	for _, row := range rows {
		if row.RegSealProof != layout.RegSealProof || row.UnsealedCID != layout.UnsealedCID {
			return mk20RetainedLayout{}, false, xerrors.Errorf("inconsistent retained sector metadata header")
		}
		layout.Pieces = append(layout.Pieces, mk20RetainedPiece{
			Index: row.PieceNum,
			CID:   row.PieceCID,
			Size:  row.PieceSize,
		})
	}
	return layout, true, nil
}

func (s *harmonyMK20OffsetStore) persistOffset(target mk20OffsetTarget, offset abi.PaddedPieceSize, metadata *mk20OffsetMetadataGuard) (bool, error) {
	var updated int
	var err error
	if metadata != nil {
		updated, err = s.tx.Exec(mk20OffsetUpdateFromMetadataSQL,
			offset, target.ID, target.AggregationIndex, target.SPID, target.Sector,
			target.RegSealProof, target.PieceCID, target.PieceSize,
			metadata.UnsealedCID, metadata.PieceNums, metadata.PieceCIDs, metadata.PieceSizes)
	} else {
		updated, err = s.tx.Exec(mk20OffsetUpdateSQL,
			offset, target.ID, target.AggregationIndex, target.SPID, target.Sector,
			target.RegSealProof, target.PieceCID, target.PieceSize)
	}
	if err != nil {
		return false, xerrors.Errorf("updating deal offset: %w", err)
	}
	if updated > 1 {
		return false, fmt.Errorf("expected to update at most 1 deal, updated %d", updated)
	}
	return updated == 1, nil
}
