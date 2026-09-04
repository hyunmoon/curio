package storageingest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
)

func TestDrainSealBatchesCommitsMoreThanOneBatch(t *testing.T) {
	const total = 2*sealBatchSize + 1
	open := make(map[abi.SectorNumber]bool, total)
	for sector := 1; sector <= total; sector++ {
		open[abi.SectorNumber(sector)] = true
	}

	var batchSizes []int
	err := drainSealBatchesWith(context.Background(), sealBatchParams{spID: 1000},
		func(failed map[abi.SectorNumber]struct{}, limit int) (sealBatchResult, error) {
			candidates := nextTestCandidates(open, failed, total, limit)
			for _, sector := range candidates {
				delete(open, sector)
			}
			if len(candidates) > 0 {
				batchSizes = append(batchSizes, len(candidates))
			}
			return sealBatchResult{candidates: candidates, committed: len(candidates)}, nil
		},
		func(abi.SectorNumber) (bool, error) {
			t.Fatal("unexpected individual fallback")
			return false, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("%d sectors remain open", len(open))
	}
	want := []int{sealBatchSize, sealBatchSize, 1}
	if len(batchSizes) != len(want) {
		t.Fatalf("batch sizes = %v, want %v", batchSizes, want)
	}
	for i := range want {
		if batchSizes[i] != want[i] {
			t.Fatalf("batch sizes = %v, want %v", batchSizes, want)
		}
	}

	err = drainSealBatchesWith(context.Background(), sealBatchParams{spID: 1000},
		func(failed map[abi.SectorNumber]struct{}, limit int) (sealBatchResult, error) {
			return sealBatchResult{candidates: nextTestCandidates(open, failed, total, limit)}, nil
		},
		func(abi.SectorNumber) (bool, error) {
			t.Fatal("completed sectors must not be moved again")
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("idempotent second drain: %v", err)
	}
}

func TestDrainSealBatchesIsolatesPoisonAndPreservesEarlierCommit(t *testing.T) {
	const total = 2*sealBatchSize + 1
	poison := abi.SectorNumber(sealBatchSize + 6)
	open := make(map[abi.SectorNumber]bool, total)
	for sector := 1; sector <= total; sector++ {
		open[abi.SectorNumber(sector)] = true
	}

	batchCalls := 0
	err := drainSealBatchesWith(context.Background(), sealBatchParams{spID: 1000},
		func(failed map[abi.SectorNumber]struct{}, limit int) (sealBatchResult, error) {
			batchCalls++
			candidates := nextTestCandidates(open, failed, total, limit)
			if batchCalls == 2 {
				return sealBatchResult{candidates: candidates}, errors.New("batch contains poison sector")
			}
			for _, sector := range candidates {
				delete(open, sector)
			}
			return sealBatchResult{candidates: candidates, committed: len(candidates)}, nil
		},
		func(sector abi.SectorNumber) (bool, error) {
			if sector == poison {
				return false, errors.New("poison sector")
			}
			if !open[sector] {
				return false, nil
			}
			delete(open, sector)
			return true, nil
		},
	)
	if err == nil {
		t.Fatal("expected poison-sector error")
	}
	if len(open) != 1 || !open[poison] {
		t.Fatalf("open sectors = %v, want only poison sector %d", open, poison)
	}
	for sector := 1; sector <= sealBatchSize; sector++ {
		if open[abi.SectorNumber(sector)] {
			t.Fatalf("sector %d from the first committed batch was rolled back", sector)
		}
	}
	if batchCalls != 3 {
		t.Fatalf("batch calls = %d, want 3", batchCalls)
	}
}

func TestWakeSealLoopCoalescesAndNeverOverlaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticks := make(chan time.Time)
	wake := make(chan struct{}, 1)
	wakeSealLoop(wake)

	started := make(chan struct{})
	release := make(chan struct{})
	secondDone := make(chan struct{})
	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32

	go runSealLoop(ctx, ticks, wake, func() error {
		call := calls.Add(1)
		current := active.Add(1)
		for {
			previous := maxActive.Load()
			if current <= previous || maxActive.CompareAndSwap(previous, current) {
				break
			}
		}
		defer active.Add(-1)

		if call == 1 {
			close(started)
			<-release
		}
		if call == 2 {
			close(secondDone)
		}
		return nil
	}, func(err error) {
		t.Errorf("unexpected seal error: %v", err)
	})

	waitTestSignal(t, started, "startup seal")
	for range 100 {
		wakeSealLoop(wake)
	}
	close(release)
	waitTestSignal(t, secondDone, "coalesced wake")
	cancel()

	if calls.Load() != 2 {
		t.Fatalf("seal calls = %d, want 2", calls.Load())
	}
	if maxActive.Load() != 1 {
		t.Fatalf("maximum concurrent seals = %d, want 1", maxActive.Load())
	}
}

func TestNewSealWakeRequestsStartupDrain(t *testing.T) {
	wake := newSealWake()
	select {
	case <-wake:
	default:
		t.Fatal("new seal wake did not request an immediate startup drain")
	}
	select {
	case <-wake:
		t.Fatal("new seal wake queued more than one startup drain")
	default:
	}
}

func TestDrainSealBatchesStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	err := drainSealBatchesWith(ctx, sealBatchParams{spID: 1000},
		func(map[abi.SectorNumber]struct{}, int) (sealBatchResult, error) {
			called = true
			return sealBatchResult{}, nil
		},
		func(abi.SectorNumber) (bool, error) {
			called = true
			return false, nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("drain error = %v, want context cancellation", err)
	}
	if called {
		t.Fatal("drain invoked database callbacks after cancellation")
	}
}

func TestSealLoopTickerFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticks := make(chan time.Time, 1)
	wake := make(chan struct{}, 1)
	called := make(chan struct{})
	go runSealLoop(ctx, ticks, wake, func() error {
		close(called)
		return nil
	}, func(err error) {
		t.Errorf("unexpected seal error: %v", err)
	})

	ticks <- time.Now()
	waitTestSignal(t, called, "ticker fallback")
}

func nextTestCandidates(open map[abi.SectorNumber]bool, failed map[abi.SectorNumber]struct{}, total, limit int) []abi.SectorNumber {
	candidates := make([]abi.SectorNumber, 0, limit)
	for sector := 1; sector <= total && len(candidates) < limit; sector++ {
		number := abi.SectorNumber(sector)
		if !open[number] {
			continue
		}
		if _, excluded := failed[number]; excluded {
			continue
		}
		candidates = append(candidates, number)
	}
	return candidates
}

func waitTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
