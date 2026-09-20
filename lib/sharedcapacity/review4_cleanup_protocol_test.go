package sharedcapacity

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func review4StoppedJournal(t *testing.T, a *Authority, dir string, create bool) *CleanupJournal {
	t.Helper()
	j, err := OpenCleanupJournal(a, dir, "cleanup", create)
	require.NoError(t, err)
	j.cancel()
	<-j.done
	return j
}

func review4Reserve(t *testing.T, a *Authority, key string, start bool) string {
	t.Helper()
	r := Request{Client: key, Sequence: 1, Operation: "reserve", Sector: key, Boot: "boot", Envelope: modelEnvelope}
	out, err := a.Apply(context.Background(), r, nil)
	require.NoError(t, err)
	if start {
		r.Sequence, r.Operation, r.Token = 2, "start", out.Entry.Token
		_, err = a.Apply(context.Background(), r, nil)
		require.NoError(t, err)
	}
	return out.Entry.Token
}

func TestReview4TerminalSurvivesReceiptReplacementRemovalAndRestart(t *testing.T) {
	ctx := context.Background()
	for _, op := range []string{"cancel", "returned"} {
		t.Run(op, func(t *testing.T) {
			a, _ := fixture(t, 8*modelEnvelope)
			token := review4Reserve(t, a, "A", op == "returned")
			dir := t.TempDir()
			j := review4StoppedJournal(t, a, dir, true)
			first, err := j.Submit(ctx, op, "A", token)
			require.NoError(t, err)
			require.NoError(t, j.Recover(ctx))
			secondToken := review4Reserve(t, a, "B", true)
			second, err := j.Submit(ctx, "returned", "B", secondToken)
			require.NoError(t, err)
			require.NoError(t, j.Recover(ctx))
			if op == "returned" {
				_, err = a.Apply(ctx, Request{Client: "reconciler", Sequence: 1, Operation: "reconcile", Sector: "A", Token: token}, func(Entry) error { return nil })
				require.NoError(t, err) // synthetic proof, no native or file deletion
			}
			require.NoError(t, j.Close())
			j = review4StoppedJournal(t, a, dir, false)
			defer func() { require.NoError(t, j.Close()) }()
			before, _, err := a.Inspect(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, before.Receipts["cleanup"].Sequence)
			require.NotContains(t, before.Entries, "A")
			ack, err := j.Submit(ctx, op, "A", token)
			require.NoError(t, err)
			require.True(t, ack.Completed)
			require.Equal(t, first.ID, ack.ID)
			_, err = j.Submit(ctx, op, "A", "unproven-token")
			require.ErrorIs(t, err, ErrIdentity)
			after, _, err := a.Inspect(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.NoError(t, j.Acknowledge(ctx, first.ID))
			require.NoError(t, j.Acknowledge(ctx, second.ID))
			require.NoError(t, j.Acknowledge(ctx, first.ID), "ack retry after collection")
			require.Empty(t, j.state.Completed)
		})
	}
}

func TestReview4TerminalPublicationFailureWithNewToken(t *testing.T) {
	ctx := context.Background()
	for _, boundary := range []string{"before-rename", "after-rename"} {
		t.Run(boundary, func(t *testing.T) {
			a, _ := fixture(t, 8*modelEnvelope)
			token := review4Reserve(t, a, "A", true)
			dir := t.TempDir()
			j := review4StoppedJournal(t, a, dir, true)
			_, err := j.Submit(ctx, "returned", "A", token)
			require.NoError(t, err)
			j.fault = func(at string) error {
				if at == boundary && len(j.state.Completed) != 0 {
					return errors.New("terminal publication failed")
				}
				return nil
			}
			require.Error(t, j.Recover(ctx))
			next, err := a.Apply(ctx, Request{Client: "followup", Sequence: 1, Operation: "reserve", Sector: "A", Boot: "boot", Envelope: modelEnvelope}, nil)
			require.NoError(t, err)
			require.NotEqual(t, token, next.Entry.Token)
			require.NoError(t, j.Close())
			j = review4StoppedJournal(t, a, dir, false)
			defer func() { require.NoError(t, j.Close()) }()
			require.NoError(t, j.Recover(ctx))
			before, _, err := a.Inspect(ctx)
			require.NoError(t, err)
			ack, err := j.Submit(ctx, "returned", "A", token)
			require.NoError(t, err)
			require.True(t, ack.Completed)
			after, _, err := a.Inspect(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, next.Entry, after.Entries["A"])
		})
	}
}

func TestReview4AcknowledgementFailureDoesNotLosePendingMutation(t *testing.T) {
	ctx := context.Background()
	for _, boundary := range []string{"before-rename", "after-rename"} {
		for _, terminal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/terminal=%t", boundary, terminal), func(t *testing.T) {
				a, _ := fixture(t, 8*modelEnvelope)
				token := review4Reserve(t, a, "A", true)
				j := review4StoppedJournal(t, a, t.TempDir(), true)
				defer func() { require.NoError(t, j.Close()) }()
				ack, err := j.Submit(ctx, "returned", "A", token)
				require.NoError(t, err)
				if terminal {
					require.NoError(t, j.Recover(ctx))
				}
				j.fault = func(at string) error {
					if at == boundary {
						return errors.New("ack lost")
					}
					return nil
				}
				require.Error(t, j.Acknowledge(ctx, ack.ID))
				j.fault = nil
				require.NoError(t, j.Acknowledge(ctx, ack.ID))
				require.NoError(t, j.Recover(ctx))
				s, _, err := a.Inspect(ctx)
				require.NoError(t, err)
				require.Equal(t, "resident", s.Entries["A"].State)
				require.EqualValues(t, 1, s.Receipts["cleanup"].Sequence)
				require.Empty(t, j.state.Jobs)
				require.Empty(t, j.state.Completed)
			})
		}
	}
}

func TestReview4TerminalEvidenceHasFiniteExplicitAckBudget(t *testing.T) {
	ctx := context.Background()
	a, _ := fixture(t, 8*modelEnvelope)
	j := review4StoppedJournal(t, a, t.TempDir(), true)
	defer func() { require.NoError(t, j.Close()) }()
	// Boundary fixture: synthesize historical no-op records to avoid 1,024 fsync
	// loops. Real publication/recovery/ack paths are exercised in the tests above.
	for n := range maxCleanupRecords {
		r := Request{Client: j.client, Operation: "returned", Sector: fmt.Sprint(n), Token: "old-token"}
		j.state.Completed = append(j.state.Completed, cleanupJob{ID: cleanupID(r.Operation, r.Sector, r.Token), Request: r})
	}
	require.NoError(t, j.write())
	token := review4Reserve(t, a, "new", true)
	result, err := j.Submit(ctx, "returned", "new", token)
	require.ErrorContains(t, err, "journal full")
	require.False(t, result.Durable)
	// Known old receipts remain queryable even at the limit; no time eviction.
	result, err = j.Submit(ctx, "returned", "0", "old-token")
	require.NoError(t, err)
	require.True(t, result.Completed)
	require.NoError(t, j.Acknowledge(ctx, result.ID))
	result, err = j.Submit(ctx, "returned", "new", token)
	require.NoError(t, err)
	require.True(t, result.Durable)
	require.NoError(t, j.Acknowledge(ctx, result.ID))
	require.NoError(t, j.Recover(ctx))
	require.Len(t, j.state.Completed, maxCleanupRecords-1)
}

func TestReview4VersionOnePendingForwardUpgrade(t *testing.T) {
	a, _ := fixture(t, 8*modelEnvelope)
	ctx := context.Background()
	token := review4Reserve(t, a, "A", true)
	dir := t.TempDir()
	j := review4StoppedJournal(t, a, dir, true)
	_, err := j.Submit(ctx, "returned", "A", token)
	require.NoError(t, err)
	j.state.Version = 1
	require.NoError(t, j.write())
	require.NoError(t, j.Close())
	j = review4StoppedJournal(t, a, dir, false)
	defer func() { require.NoError(t, j.Close()) }()
	require.NoError(t, j.Recover(ctx))
	require.Equal(t, 2, j.state.Version)
	require.Len(t, j.state.Completed, 1)
}
