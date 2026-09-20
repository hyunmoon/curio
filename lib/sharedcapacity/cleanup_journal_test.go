package sharedcapacity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCleanupJournalUnknownRecoveryAndRestart(t *testing.T) {
	for _, op := range []string{"cancel", "returned"} {
		t.Run(op, func(t *testing.T) {
			a, _ := fixture(t, 4*modelEnvelope)
			ctx := context.Background()
			r := Request{Client: "writer", Sequence: 1, Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: modelEnvelope}
			out, err := a.Apply(ctx, r, nil)
			require.NoError(t, err)
			if op == "returned" {
				r.Sequence++
				r.Operation = "start"
				r.Token = out.Entry.Token
				_, err = a.Apply(ctx, r, nil)
				require.NoError(t, err)
			}
			var fail atomic.Bool
			fail.Store(true)
			var writes atomic.Int32
			a.fault = func(at string) error {
				if at == "after-rename" && writes.Add(1) >= 2 && fail.Load() {
					return errors.New("unknown publication")
				}
				return nil
			}
			dir := t.TempDir()
			j, err := OpenCleanupJournal(a, dir, "cleanup", true)
			require.NoError(t, err)
			_, err = OpenCleanupJournal(a, dir, "cleanup", false)
			require.Error(t, err, "duplicate issuer")
			result, err := j.Submit(ctx, op, "sector", out.Entry.Token)
			require.NoError(t, err)
			require.True(t, result.Durable)
			require.Error(t, j.Recover(ctx))
			j.mu.Lock()
			require.NoError(t, j.read())
			require.Len(t, j.state.Jobs, 1)
			request := j.state.Jobs[0].Request
			j.mu.Unlock()
			require.EqualValues(t, 1, request.Sequence)
			// The mutation itself was published; its acknowledgement is what
			// failed. This is not merely a failed pre-Apply Resolve.
			require.NoError(t, a.lock(ctx, func() error {
				s, e := a.read()
				if e != nil {
					return e
				}
				if op == "cancel" {
					require.Empty(t, s.Entries)
				} else {
					require.Equal(t, "resident", s.Entries["sector"].State)
				}
				return nil
			}))
			require.NoError(t, j.Close())
			fail.Store(false)
			j, err = OpenCleanupJournal(a, dir, "cleanup", false)
			require.NoError(t, err)
			defer func() { require.NoError(t, j.Close()) }()
			require.Eventually(t, func() bool { j.mu.Lock(); defer j.mu.Unlock(); return len(j.state.Jobs) == 0 }, 3*time.Second, 10*time.Millisecond)
			s, _, err := a.Inspect(ctx)
			require.NoError(t, err)
			if op == "cancel" {
				require.Empty(t, s.Entries)
			} else {
				require.Equal(t, "resident", s.Entries["sector"].State)
			}
			require.EqualValues(t, 1, s.Receipts["cleanup"].Sequence, "same request resolved, no replacement client/sequence")
			result, err = j.Submit(ctx, op, "sector", out.Entry.Token)
			require.NoError(t, err)
			require.True(t, result.Durable)
		})
	}
}

func TestCleanupJournalWriteFailureIsNotHandoff(t *testing.T) {
	for _, at := range []string{"before-rename", "after-rename"} {
		t.Run(at, func(t *testing.T) {
			a, _ := fixture(t, 4*modelEnvelope)
			ctx := context.Background()
			out, err := a.Apply(ctx, Request{Client: "writer", Sequence: 1, Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: modelEnvelope}, nil)
			require.NoError(t, err)
			j, err := OpenCleanupJournal(a, t.TempDir(), "cleanup", true)
			require.NoError(t, err)
			defer func() { require.NoError(t, j.Close()) }()
			var fail atomic.Bool
			fail.Store(true)
			j.mu.Lock()
			j.fault = func(point string) error {
				if fail.Load() && point == at {
					return errors.New("journal write failed")
				}
				return nil
			}
			j.mu.Unlock()
			result, err := j.Submit(ctx, "cancel", "sector", out.Entry.Token)
			require.Error(t, err)
			require.False(t, result.Durable)
			fail.Store(false)
			result, err = j.Submit(ctx, "cancel", "sector", out.Entry.Token)
			require.NoError(t, err)
			require.True(t, result.Durable)
			require.NoError(t, j.Recover(ctx))
			s, _, err := a.Inspect(ctx)
			require.NoError(t, err)
			require.Empty(t, s.Entries)
		})
	}
}

func TestCleanupJournalCancellationCannotEndRunningWriter(t *testing.T) {
	a, _ := fixture(t, 4*modelEnvelope)
	ctx := context.Background()
	out, err := a.Apply(ctx, Request{Client: "writer", Sequence: 1, Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: modelEnvelope}, nil)
	require.NoError(t, err)
	_, err = a.Apply(ctx, Request{Client: "writer", Sequence: 2, Operation: "start", Sector: "sector", Token: out.Entry.Token}, nil)
	require.NoError(t, err)
	j, err := OpenCleanupJournal(a, t.TempDir(), "cleanup", true)
	require.NoError(t, err)
	defer func() { require.NoError(t, j.Close()) }()
	_, err = j.Submit(ctx, "cancel", "sector", out.Entry.Token)
	require.NoError(t, err)
	require.NoError(t, j.Recover(ctx))
	s, _, err := a.Inspect(ctx)
	require.NoError(t, err)
	require.Equal(t, "running", s.Entries["sector"].State)
	_, err = j.Submit(ctx, "start", "sector", out.Entry.Token)
	require.Error(t, err, "recovery cannot launch native")
}
