package seal

import (
	"testing"
	"time"
)

func TestNextSDRJitterPhase(t *testing.T) {
	interval := time.Minute
	offset := 10 * time.Second

	now := time.Unix(100, 0)
	next := nextSDRJitterPhase(now, interval, offset)

	delay := next.Sub(now)
	if delay < 0 || delay >= interval {
		t.Fatalf("next phase delay must be in [0, interval): delay=%s interval=%s", delay, interval)
	}

	if want := time.Unix(130, 0); !next.Equal(want) {
		t.Fatalf("next phase mismatch: got %s want %s", next, want)
	}

	if rem := (next.UnixNano() - int64(offset)) % int64(interval); rem != 0 {
		t.Fatalf("next phase is not aligned to offset: next=%s offset=%s rem=%d", next, offset, rem)
	}
}

func TestNextSDRJitterPhaseReturnsNowWhenAlreadyOnPhase(t *testing.T) {
	interval := time.Minute
	offset := 10 * time.Second

	now := time.Unix(130, 0)
	next := nextSDRJitterPhase(now, interval, offset)

	if !next.Equal(now) {
		t.Fatalf("expected next phase to be now when already aligned: got %s want %s", next, now)
	}
}

func TestSDRStartReadyUsesMinInterval(t *testing.T) {
	s := &SDRTask{
		minStartInterval: time.Minute,
		lastSDRStart:     time.Unix(100, 0),
	}

	now := time.Unix(120, 0)
	ready, next, remaining, reason := s.sdrStartReady(now)

	if ready {
		t.Fatal("expected SDR start to be delayed")
	}
	if reason != "min start interval" {
		t.Fatalf("reason mismatch: got %q want %q", reason, "min start interval")
	}
	if want := time.Unix(160, 0); !next.Equal(want) {
		t.Fatalf("next start mismatch: got %s want %s", next, want)
	}
	if want := 40 * time.Second; remaining != want {
		t.Fatalf("remaining mismatch: got %s want %s", remaining, want)
	}
}

func TestSDRStartReadyAllowsFirstStartWithoutJitter(t *testing.T) {
	s := &SDRTask{
		minStartInterval: time.Minute,
	}

	ready, next, remaining, reason := s.sdrStartReady(time.Unix(100, 0))

	if !ready {
		t.Fatalf("expected first SDR start to be ready: next=%s remaining=%s reason=%q", next, remaining, reason)
	}
	if !next.IsZero() {
		t.Fatalf("expected zero next start when ready, got %s", next)
	}
	if remaining != 0 {
		t.Fatalf("expected zero remaining when ready, got %s", remaining)
	}
	if reason != "" {
		t.Fatalf("expected empty reason when ready, got %q", reason)
	}
}

func TestReserveSDRStartSlotRecordsStart(t *testing.T) {
	s := &SDRTask{
		minStartInterval: time.Minute,
	}

	if !s.reserveSDRStartSlot(1) {
		t.Fatal("expected first SDR start slot reservation to succeed")
	}
	if s.lastSDRStart.IsZero() {
		t.Fatal("expected lastSDRStart to be recorded")
	}
	if s.reserveSDRStartSlot(1) {
		t.Fatal("expected immediate second SDR start slot reservation to be delayed")
	}
}
