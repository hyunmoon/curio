package sdrscratch

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAutoDiscardCancellationBetweenTargets(t *testing.T) {
	c, base, io, state := autoFixture(t)
	a := autoWrite(t, base, "s-t01000-41.tmp", 1)
	b := autoWrite(t, base, "s-t01000-42.tmp", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	results, err := autoDiscardContext(ctx, c, base, func(target AutoTarget, apply func(AutoStage) error) error {
		calls++
		err := state(target, apply)
		cancel() // completed journal; do not start a second target
		return err
	}, io)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
	require.Len(t, results, 1)
	require.Equal(t, "reclaimed", results[0].Status)
	remaining := 0
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); err == nil {
			remaining++
		}
	}
	require.Equal(t, 1, remaining)
	results, err = autoDiscard(c, base, state, io)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "reclaimed", results[0].Status)
}
