package storage_market

import (
	"errors"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/lotus/chain/proofs"
)

type fakeMK20OffsetStore struct {
	locked          bool
	lockErr         error
	initial         []mk20SectorPiece
	initialErr      error
	retained        mk20RetainedLayout
	retainedOK      bool
	retainedErr     error
	persistOK       bool
	persistErr      error
	lockCalls       int
	initialCalls    int
	retainedCalls   int
	persistCalls    int
	persistedTarget mk20OffsetTarget
	persistedOffset abi.PaddedPieceSize
	persistedGuard  *mk20OffsetMetadataGuard
}

func (f *fakeMK20OffsetStore) lockTarget(target mk20OffsetTarget) (bool, error) {
	f.lockCalls++
	f.persistedTarget = target
	return f.locked, f.lockErr
}

func (f *fakeMK20OffsetStore) loadInitialPieces(_, _ int64) ([]mk20SectorPiece, error) {
	f.initialCalls++
	return f.initial, f.initialErr
}

func (f *fakeMK20OffsetStore) loadRetainedLayout(_, _ int64) (mk20RetainedLayout, bool, error) {
	f.retainedCalls++
	return f.retained, f.retainedOK, f.retainedErr
}

func (f *fakeMK20OffsetStore) persistOffset(target mk20OffsetTarget, offset abi.PaddedPieceSize, guard *mk20OffsetMetadataGuard) (bool, error) {
	f.persistCalls++
	f.persistedTarget = target
	f.persistedOffset = offset
	if guard != nil {
		copied := *guard
		f.persistedGuard = &copied
	}
	return f.persistOK, f.persistErr
}

func newFakeMK20OffsetStore() *fakeMK20OffsetStore {
	return &fakeMK20OffsetStore{locked: true, retainedOK: true, persistOK: true}
}

func TestResolveMK20PieceOffsetPrefersInitialPieces(t *testing.T) {
	target, _, _ := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
	store := newFakeMK20OffsetStore()
	store.initial = []mk20SectorPiece{
		{CID: syntheticPieceCID(t, 21), Size: 128, Index: 0},
		{CID: target.PieceCID, Size: abi.PaddedPieceSize(target.PieceSize), Index: 1},
	}
	store.retained = mk20RetainedLayout{UnsealedCID: "deliberately invalid"}

	updated, err := resolveMK20PieceOffset(store, target)
	require.NoError(t, err)
	require.True(t, updated)
	require.Equal(t, 1, store.initialCalls)
	require.Zero(t, store.retainedCalls, "nonempty initial pieces must prevent metadata fallback")
	require.Equal(t, abi.PaddedPieceSize(256), store.persistedOffset)
	require.Nil(t, store.persistedGuard)
}

func TestResolveMK20PieceOffsetDoesNotFallbackWhenInitialTargetMissing(t *testing.T) {
	target, retained, _ := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
	store := newFakeMK20OffsetStore()
	store.initial = []mk20SectorPiece{{CID: syntheticPieceCID(t, 22), Size: 128, Index: 0}}
	store.retained = retained

	updated, err := resolveMK20PieceOffset(store, target)
	require.ErrorContains(t, err, "failed to find deal offset")
	require.False(t, updated)
	require.Zero(t, store.retainedCalls)
	require.Zero(t, store.persistCalls)
}

func TestResolveMK20PieceOffsetPreservesInitialFirstMatch(t *testing.T) {
	proof := abi.RegisteredSealProof_StackedDrg2KiBV1_1
	sharedCID := syntheticPieceCID(t, 27)
	target := defaultMK20OffsetTarget(proof, sharedCID, 128)
	store := newFakeMK20OffsetStore()
	store.initial = []mk20SectorPiece{
		{CID: sharedCID, Size: 128, Index: 0},
		{CID: sharedCID, Size: 128, Index: 1},
	}

	updated, err := resolveMK20PieceOffset(store, target)
	require.NoError(t, err)
	require.True(t, updated)
	require.Zero(t, store.persistedOffset, "the live initial-piece path must retain its existing first-match behavior")
	require.Zero(t, store.retainedCalls)
}

func TestResolveMK20PieceOffsetUsesValidatedRetainedLayout(t *testing.T) {
	tests := []struct {
		name  string
		proof abi.RegisteredSealProof
	}{
		{name: "finalized SDR metadata", proof: abi.RegisteredSealProof_StackedDrg2KiBV1_1},
		{name: "finalized Snap metadata", proof: abi.RegisteredSealProof_StackedDrg2KiBV1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, retained, expectedOffset := retainedOffsetFixture(t, test.proof)
			originalPieces := append([]mk20RetainedPiece(nil), retained.Pieces...)
			store := newFakeMK20OffsetStore()
			store.retained = retained

			updated, err := resolveMK20PieceOffset(store, target)
			require.NoError(t, err)
			require.True(t, updated)
			require.Equal(t, 1, store.retainedCalls)
			require.Equal(t, expectedOffset, store.persistedOffset)
			require.Equal(t, &mk20OffsetMetadataGuard{
				UnsealedCID: retained.UnsealedCID,
				PieceNums:   []int64{0, 1, 2},
				PieceCIDs:   []string{retained.Pieces[0].CID, retained.Pieces[1].CID, retained.Pieces[2].CID},
				PieceSizes:  []int64{512, 256, 128},
			}, store.persistedGuard)
			require.Equal(t, originalPieces, retained.Pieces, "validation must not mutate retained piece order or data")
		})
	}
}

func TestResolveMK20PieceOffsetAllowsValidatedZeroOffset(t *testing.T) {
	proof := abi.RegisteredSealProof_StackedDrg2KiBV1_1
	cid := syntheticPieceCID(t, 23)
	target := defaultMK20OffsetTarget(proof, cid, 512)
	retained := retainedLayout(t, proof, []mk20RetainedPiece{{Index: 0, CID: cid, Size: 512}})
	store := newFakeMK20OffsetStore()
	store.retained = retained

	updated, err := resolveMK20PieceOffset(store, target)
	require.NoError(t, err)
	require.True(t, updated)
	require.Zero(t, store.persistedOffset)
}

func TestResolveMK20PieceOffsetLeavesUnavailableFallbackUnchanged(t *testing.T) {
	target, _, _ := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)

	for _, unavailableState := range []string{
		"missing retained metadata",
		"open sector still establishes layout",
		"SDR pipeline still establishes layout",
		"Snap pipeline still establishes layout",
	} {
		t.Run(unavailableState, func(t *testing.T) {
			store := newFakeMK20OffsetStore()
			// The production query represents each state above as no eligible
			// retained layout. SQL-shape coverage below verifies those guards.
			store.retainedOK = false

			updated, err := resolveMK20PieceOffset(store, target)
			require.NoError(t, err)
			require.False(t, updated)
			require.Zero(t, store.persistCalls)
		})
	}
}

func TestValidateMK20RetainedLayoutRejectsUnsafeLayouts(t *testing.T) {
	target, valid, _ := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)

	tests := []struct {
		name       string
		mutate     func(*mk20RetainedLayout, *mk20OffsetTarget)
		errorMatch string
	}{
		{
			name:       "missing pieces",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.Pieces = nil },
			errorMatch: "no pieces",
		},
		{
			name:       "proof changed",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.RegSealProof++ },
			errorMatch: "proof changed",
		},
		{
			name:       "invalid unsealed CID",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.UnsealedCID = "not-a-cid" },
			errorMatch: "parsing retained unsealed CID",
		},
		{
			name:       "noncontiguous piece index",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.Pieces[1].Index = 3 },
			errorMatch: "invalid piece order",
		},
		{
			name:       "nonpositive size",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.Pieces[0].Size = 0 },
			errorMatch: "invalid piece size",
		},
		{
			name:       "invalid padded size",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.Pieces[0].Size = 192 },
			errorMatch: "validating piece size",
		},
		{
			name:       "invalid normal-path size order",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.Pieces[2].Size = 512 },
			errorMatch: "invalid piece size order",
		},
		{
			name:       "out of bounds before arithmetic overflow",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.Pieces[0].Size = 1 << 62 },
			errorMatch: "exceeds sector size",
		},
		{
			name:       "invalid piece CID",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.Pieces[0].CID = "not-a-cid" },
			errorMatch: "parsing piece CID",
		},
		{
			name:       "missing target",
			mutate:     func(_ *mk20RetainedLayout, target *mk20OffsetTarget) { target.PieceCID = syntheticPieceCID(t, 24) },
			errorMatch: "expected one retained target occurrence, found 0",
		},
		{
			name: "ambiguous repeated target",
			mutate: func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) {
				layout.Pieces[0].CID = layout.Pieces[1].CID
				layout.Pieces[0].Size = layout.Pieces[1].Size
			},
			errorMatch: "expected one retained target occurrence, found 2",
		},
		{
			name:       "CommD mismatch",
			mutate:     func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) { layout.UnsealedCID = syntheticPieceCID(t, 25) },
			errorMatch: "does not match current unsealed CID",
		},
		{
			name: "truncated layout",
			mutate: func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) {
				layout.Pieces = layout.Pieces[:2]
			},
			errorMatch: "does not match current unsealed CID",
		},
		{
			name: "mixed layout",
			mutate: func(layout *mk20RetainedLayout, _ *mk20OffsetTarget) {
				layout.Pieces[2].CID = syntheticPieceCID(t, 26)
			},
			errorMatch: "does not match current unsealed CID",
		},
		{
			name: "unsupported proof",
			mutate: func(layout *mk20RetainedLayout, target *mk20OffsetTarget) {
				layout.RegSealProof = 10_000
				target.RegSealProof = 10_000
			},
			errorMatch: "getting sector size",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := valid
			layout.Pieces = append([]mk20RetainedPiece(nil), valid.Pieces...)
			caseTarget := target
			test.mutate(&layout, &caseTarget)

			_, err := validateMK20RetainedLayout(layout, caseTarget)
			require.ErrorContains(t, err, test.errorMatch)
		})
	}
}

func TestResolveMK20PieceOffsetHandlesStaleTargetAndErrors(t *testing.T) {
	target, retained, _ := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
	testError := errors.New("test failure")

	tests := []struct {
		name        string
		configure   func(*fakeMK20OffsetStore)
		wantError   string
		wantPersist int
	}{
		{
			name:      "target changed or offset already populated",
			configure: func(store *fakeMK20OffsetStore) { store.locked = false },
		},
		{
			name:      "target read error",
			configure: func(store *fakeMK20OffsetStore) { store.lockErr = testError },
			wantError: "locking MK20 offset target",
		},
		{
			name:      "initial source error",
			configure: func(store *fakeMK20OffsetStore) { store.initialErr = testError },
			wantError: "getting pieces for sector",
		},
		{
			name:      "metadata source error",
			configure: func(store *fakeMK20OffsetStore) { store.retainedErr = testError },
			wantError: "getting retained sector metadata",
		},
		{
			name:        "conditional persistence lost race",
			configure:   func(store *fakeMK20OffsetStore) { store.persistOK = false },
			wantPersist: 1,
		},
		{
			name:        "persistence error",
			configure:   func(store *fakeMK20OffsetStore) { store.persistErr = testError },
			wantError:   "test failure",
			wantPersist: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newFakeMK20OffsetStore()
			store.retained = retained
			test.configure(store)

			updated, err := resolveMK20PieceOffset(store, target)
			if test.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.wantError)
			}
			require.False(t, updated)
			require.Equal(t, test.wantPersist, store.persistCalls)
		})
	}
}

func TestResolveMK20PieceOffsetDoesNotWriteStaleOrPopulatedTargets(t *testing.T) {
	target, retained, _ := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)

	for _, stateChange := range []string{
		"provider changed",
		"sector changed",
		"proof changed",
		"MK20 aggregate assignment changed",
		"offset already populated",
	} {
		t.Run(stateChange, func(t *testing.T) {
			store := newFakeMK20OffsetStore()
			store.locked = false
			store.retained = retained

			updated, err := resolveMK20PieceOffset(store, target)
			require.NoError(t, err)
			require.False(t, updated)
			require.Zero(t, store.initialCalls)
			require.Zero(t, store.retainedCalls)
			require.Zero(t, store.persistCalls)
		})
	}
}

func TestMK20OffsetSQLScopesFallbackAndConditionalPersistence(t *testing.T) {
	for _, predicate := range []string{
		"id = $1", "aggr_index = $2", "sp_id = $3", "sector = $4",
		"reg_seal_proof = $5", "piece_cid = $6", "piece_size = $7",
		"sector_offset IS NULL", "FOR UPDATE",
	} {
		require.Contains(t, mk20OffsetLockTargetSQL, predicate)
	}

	require.Contains(t, mk20OffsetInitialPiecesSQL, "ORDER BY piece_index ASC")
	for _, relation := range []string{
		"open_sector_pieces", "sectors_sdr_pipeline", "sectors_snap_pipeline",
		"sectors_sdr_initial_pieces", "sectors_snap_initial_pieces",
	} {
		require.Contains(t, mk20OffsetRetainedLayoutSQL, "NOT EXISTS")
		require.Contains(t, mk20OffsetRetainedLayoutSQL, relation)
		require.Contains(t, mk20OffsetUpdateFromMetadataSQL, relation)
	}
	require.Contains(t, mk20OffsetRetainedLayoutSQL, "ORDER BY smp.piece_num ASC")
	require.Contains(t, mk20OffsetRetainedLayoutSQL, "FOR UPDATE OF sm, smp")
	require.Contains(t, mk20OffsetUpdateFromMetadataSQL, "sm.reg_seal_proof = $6")
	require.Contains(t, mk20OffsetUpdateFromMetadataSQL, "sm.cur_unsealed_cid = $9")
	require.Contains(t, mk20OffsetUpdateFromMetadataSQL, "= $10::bigint[]")
	require.Contains(t, mk20OffsetUpdateFromMetadataSQL, "= $11::text[]")
	require.Contains(t, mk20OffsetUpdateFromMetadataSQL, "= $12::bigint[]")
	for _, predicate := range []string{
		"id = $2", "aggr_index = $3", "sp_id = $4", "sector = $5",
		"reg_seal_proof = $6", "piece_cid = $7", "piece_size = $8",
		"sector_offset IS NULL",
	} {
		require.Contains(t, mk20OffsetUpdateFromMetadataSQL, predicate)
	}

	upperSQL := strings.ToUpper(mk20OffsetRetainedLayoutSQL + mk20OffsetUpdateFromMetadataSQL)
	require.NotContains(t, upperSQL, " COALESCE(")
}

func retainedOffsetFixture(t *testing.T, proof abi.RegisteredSealProof) (mk20OffsetTarget, mk20RetainedLayout, abi.PaddedPieceSize) {
	t.Helper()
	pieces := []mk20RetainedPiece{
		{Index: 0, CID: syntheticPieceCID(t, 31), Size: 512},
		{Index: 1, CID: syntheticPieceCID(t, 32), Size: 256},
		{Index: 2, CID: syntheticPieceCID(t, 33), Size: 128},
	}
	// Golden CommD for the ordered 512/256/128 layout above in a 2 KiB
	// sector, including the data path's alignment and trailing zero-padding.
	layout := mk20RetainedLayout{
		RegSealProof: int64(proof),
		UnsealedCID:  "baga6ea4seaqcngjkivln4kahgoewk3j7qjexin45mi4cddjfozo2ukqtbgxqsja",
		Pieces:       pieces,
	}
	target := defaultMK20OffsetTarget(proof, pieces[1].CID, pieces[1].Size)
	return target, layout, abi.PaddedPieceSize(512)
}

func defaultMK20OffsetTarget(proof abi.RegisteredSealProof, pieceCID string, pieceSize int64) mk20OffsetTarget {
	return mk20OffsetTarget{
		ID:               "synthetic-deal",
		SPID:             1000,
		AggregationIndex: 2,
		Sector:           101,
		RegSealProof:     int64(proof),
		PieceCID:         pieceCID,
		PieceSize:        pieceSize,
	}
}

func retainedLayout(t *testing.T, proof abi.RegisteredSealProof, pieces []mk20RetainedPiece) mk20RetainedLayout {
	t.Helper()
	infos := make([]abi.PieceInfo, len(pieces))
	for index, piece := range pieces {
		parsed, parseErr := cid.Parse(piece.CID)
		require.NoError(t, parseErr)
		infos[index] = abi.PieceInfo{PieceCID: parsed, Size: abi.PaddedPieceSize(piece.Size)}
	}
	unsealedCID, err := proofs.GenerateUnsealedCID(proof, infos)
	require.NoError(t, err)
	return mk20RetainedLayout{
		RegSealProof: int64(proof),
		UnsealedCID:  unsealedCID.String(),
		Pieces:       pieces,
	}
}
