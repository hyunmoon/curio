package storage_market

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestFullMK20PollRunsGlobalStagesInOrder(t *testing.T) {
	ctx := context.Background()
	var stages []string

	stage := func(name string) mk20PollStage {
		return func(context.Context) {
			stages = append(stages, name)
		}
	}

	runMK20PollStages(ctx, stage("pieces"), stage("aggregation"), stage("ingestion"))

	want := []string{"pieces", "aggregation", "ingestion"}
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
}

func TestFullMK20PiecePassMarksDownloadedOnce(t *testing.T) {
	ctx := context.Background()
	stored := []MK20PipelinePiece{
		{ID: "deal-1", PieceCID: "piece-1"},
		{ID: "deal-2", PieceCID: "piece-2"},
		{ID: "deal-3", PieceCID: "piece-3"},
	}
	var snapshot []MK20PipelinePiece

	markCalls := 0
	loadCalls := 0
	err := markAndLoadMK20Pieces(ctx, func(context.Context) error {
		markCalls++
		return nil
	}, func(context.Context) error {
		loadCalls++
		snapshot = append([]MK20PipelinePiece(nil), stored...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	processCalls := 0
	stage := func(context.Context, MK20PipelinePiece) error {
		processCalls++
		return nil
	}
	for _, piece := range snapshot {
		if err := processMk20PieceStages(ctx, piece, stage, stage, stage); err != nil {
			t.Fatal(err)
		}
	}

	if markCalls != 1 {
		t.Fatalf("mark calls = %d, want 1", markCalls)
	}
	if loadCalls != 1 {
		t.Fatalf("load calls = %d, want 1", loadCalls)
	}
	wantProcessCalls := len(stored) * 3
	if processCalls != wantProcessCalls {
		t.Fatalf("stage calls = %d, want %d", processCalls, wantProcessCalls)
	}
}

func TestMK20PiecePassProcessesFreshDownloadedState(t *testing.T) {
	ctx := context.Background()
	stored := []MK20PipelinePiece{
		{ID: "deal-1", Downloaded: false},
		{ID: "deal-2", Downloaded: false},
	}
	var snapshot []MK20PipelinePiece

	err := markAndLoadMK20Pieces(ctx, func(context.Context) error {
		for i := range stored {
			stored[i].Downloaded = true
		}
		return nil
	}, func(context.Context) error {
		snapshot = append([]MK20PipelinePiece(nil), stored...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(snapshot) != len(stored) {
		t.Fatalf("loaded %d pieces, want %d", len(snapshot), len(stored))
	}
	processed := 0
	checkDownloaded := func(_ context.Context, piece MK20PipelinePiece) error {
		processed++
		if !piece.Downloaded {
			t.Fatalf("piece %s processed with stale downloaded=false", piece.ID)
		}
		return nil
	}
	noOp := func(context.Context, MK20PipelinePiece) error { return nil }
	for _, piece := range snapshot {
		if err := processMk20PieceStages(ctx, piece, checkDownloaded, noOp, noOp); err != nil {
			t.Fatal(err)
		}
	}
	if processed != len(stored) {
		t.Fatalf("processed %d pieces, want %d", processed, len(stored))
	}
}

func TestSignalNextMK20LoadsOnlyRequestedDealWithoutMarkingDownloaded(t *testing.T) {
	ctx := context.Background()
	requestedID := "requested-deal"
	stored := []MK20PipelinePiece{
		{ID: requestedID, PieceCID: "requested-piece", Downloaded: true, AfterCommp: true},
		{ID: "other-deal", PieceCID: "other-piece", Downloaded: false},
	}
	var operations []string
	var processed []MK20PipelinePiece
	wakeCalls := 0

	err := runSignalNextMK20(ctx, requestedID, func(_ context.Context, id string) ([]MK20PipelinePiece, error) {
		operations = append(operations, "load:"+id)
		var pieces []MK20PipelinePiece
		for _, piece := range stored {
			if piece.ID == id {
				pieces = append(pieces, piece)
			}
		}
		return pieces, nil
	}, func(_ context.Context, piece MK20PipelinePiece) error {
		operations = append(operations, "process:"+piece.ID)
		processed = append(processed, piece)
		return nil
	}, func(piece MK20PipelinePiece, err error) {
		t.Fatalf("unexpected processing error for %s: %v", piece.ID, err)
	}, func() {
		operations = append(operations, "wake")
		wakeCalls++
	})
	if err != nil {
		t.Fatal(err)
	}

	wantOperations := []string{"load:" + requestedID, "process:" + requestedID, "wake"}
	if !reflect.DeepEqual(operations, wantOperations) {
		t.Fatalf("operations = %v, want %v", operations, wantOperations)
	}
	if len(processed) != 1 {
		t.Fatalf("processed %d pieces, want 1", len(processed))
	}
	if processed[0].ID != requestedID {
		t.Fatalf("processed deal %q, want %q", processed[0].ID, requestedID)
	}
	if !processed[0].Downloaded {
		t.Fatal("requested deal processed with stale downloaded=false")
	}
	if !processed[0].AfterCommp {
		t.Fatal("requested deal processed with stale after_commp=false")
	}
	if wakeCalls != 1 {
		t.Fatalf("wake calls = %d, want 1", wakeCalls)
	}
}

func TestSignalNextMK20ContinuesAfterPieceErrorAndWakesOnce(t *testing.T) {
	ctx := context.Background()
	pieces := []MK20PipelinePiece{{ID: "deal", PieceCID: "piece-1"}, {ID: "deal", PieceCID: "piece-2"}}
	var processed []string
	var failed []string
	wakeCalls := 0

	err := runSignalNextMK20(ctx, "deal", func(context.Context, string) ([]MK20PipelinePiece, error) {
		return pieces, nil
	}, func(_ context.Context, piece MK20PipelinePiece) error {
		processed = append(processed, piece.PieceCID)
		if piece.PieceCID == "piece-1" {
			return errors.New("test error")
		}
		return nil
	}, func(piece MK20PipelinePiece, _ error) {
		failed = append(failed, piece.PieceCID)
	}, func() {
		wakeCalls++
	})
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(processed, []string{"piece-1", "piece-2"}) {
		t.Fatalf("processed = %v", processed)
	}
	if !reflect.DeepEqual(failed, []string{"piece-1"}) {
		t.Fatalf("failed = %v", failed)
	}
	if wakeCalls != 1 {
		t.Fatalf("wake calls = %d, want 1", wakeCalls)
	}
}

func TestWakeDealPollerCoalescesPendingWake(t *testing.T) {
	d := &CurioStorageDealMarket{
		wakePollReq:        make(chan struct{}),
		wakePollLoopCtx:    context.Background(),
		lastWakeDrivenPoll: time.Now().Add(time.Hour),
	}

	d.WakeDealPoller()
	d.wakeMu.Lock()
	firstTimer := d.wakePollTimer
	d.wakeMu.Unlock()
	if firstTimer == nil {
		t.Fatal("first wake did not schedule a timer")
	}

	d.WakeDealPoller()
	d.wakeMu.Lock()
	secondTimer := d.wakePollTimer
	if secondTimer != nil {
		secondTimer.Stop()
		d.wakePollTimer = nil
	}
	d.wakeMu.Unlock()

	if secondTimer != firstTimer {
		t.Fatal("second wake scheduled a different timer")
	}
}

func TestProcessMK20PieceStagesPreservesOrdering(t *testing.T) {
	ctx := context.Background()
	piece := MK20PipelinePiece{ID: "deal-1"}
	var stages []string

	stage := func(name string) mk20PieceStage {
		return func(_ context.Context, got MK20PipelinePiece) error {
			if got.ID != piece.ID {
				t.Fatalf("stage %s received deal %q, want %q", name, got.ID, piece.ID)
			}
			stages = append(stages, name)
			return nil
		}
	}

	err := processMk20PieceStages(ctx, piece, stage("offline"), stage("commp"), stage("offset"))
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"offline", "commp", "offset"}
	if !reflect.DeepEqual(stages, want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
}
