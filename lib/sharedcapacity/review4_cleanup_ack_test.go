package sharedcapacity

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Reconstructed from the Review4 narrative, not the missing original drop-in.
func TestReview4LostHandoffAck(t *testing.T) {
	for _, followup := range []bool{false, true} {
		t.Run(map[bool]string{false: "without-followup", true: "with-followup"}[followup], func(t *testing.T) {
			ctx := context.Background()
			a, _ := fixture(t, 4*modelEnvelope)
			r := Request{Client: "writer", Sequence: 1, Operation: "reserve", Sector: "A", Boot: "boot", Envelope: modelEnvelope}
			out, err := a.Apply(ctx, r, nil)
			require.NoError(t, err)
			token := out.Entry.Token
			r.Operation, r.Sequence, r.Token = "start", 2, token
			_, err = a.Apply(ctx, r, nil)
			require.NoError(t, err)
			j, err := OpenCleanupJournal(a, t.TempDir(), "cleanup", true)
			require.NoError(t, err)
			defer func() { require.NoError(t, j.Close()) }()
			j.cancel()
			<-j.done // deterministic manual recovery, same production Recover
			j.fault = func(at string) error {
				if at == "after-rename" {
					return errors.New("lost acknowledgement")
				}
				return nil
			}
			ack, err := j.Submit(ctx, "returned", "A", token)
			require.Error(t, err)
			require.False(t, ack.Durable)
			j.fault = nil
			require.NoError(t, j.Recover(ctx))
			if followup {
				r.Operation, r.Sequence, r.Token = "reserve", 3, ""
				out, err = a.Apply(ctx, r, nil)
				require.NoError(t, err)
				require.NotEqual(t, token, out.Entry.Token)
			}
			before, _, err := a.Inspect(ctx)
			require.NoError(t, err)
			ack, err = j.Submit(ctx, "returned", "A", token)
			require.NoError(t, err, "the exact completed handoff must remain confirmable after token change")
			require.True(t, ack.Durable)
			after, _, err := a.Inspect(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after, "old acknowledgement must not mutate the new token or receipt")
		})
	}
}
