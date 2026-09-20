package sharedcapacity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCleanupJournalProcessHelper(t *testing.T) {
	mode := os.Getenv("CURIO_CLEANUP_JOURNAL_TEST_MODE")
	if mode == "" {
		return
	}
	a, err := Open(os.Getenv("CURIO_CLEANUP_JOURNAL_TEST_LEDGER"), fixtureID, fixturePolicy, func(context.Context, State) (Sample, error) {
		return Sample{Identity: fixtureID, Free: 4 * modelEnvelope}, nil
	})
	require.NoError(t, err)
	defer func() { require.NoError(t, a.Close()) }()
	if mode == "crash" {
		var writes atomic.Int32
		a.fault = func(point string) error {
			if point == "after-rename" && writes.Add(1) == 2 {
				os.Exit(74)
			}
			return nil
		}
	}
	j, err := OpenCleanupJournal(a, os.Getenv("CURIO_CLEANUP_JOURNAL_TEST_DIR"), "restartable-cleanup", mode == "crash")
	require.NoError(t, err)
	defer func() { require.NoError(t, j.Close()) }()
	if mode == "crash" {
		_, err = j.Submit(context.Background(), os.Getenv("CURIO_CLEANUP_JOURNAL_TEST_OP"), "sector", os.Getenv("CURIO_CLEANUP_JOURNAL_TEST_TOKEN"))
		require.NoError(t, err)
	}
	require.NoError(t, j.Recover(context.Background()))
	if mode == "crash" {
		t.Fatal("did not crash after mutation rename")
	}
	remaining, err := j.Pending()
	require.NoError(t, err)
	require.Empty(t, remaining)
}

func TestCleanupJournalAcrossRealProcessExit(t *testing.T) {
	for _, op := range []string{"cancel", "returned"} {
		t.Run(op, func(t *testing.T) {
			a, ledger := fixture(t, 4*modelEnvelope)
			ctx := context.Background()
			out, err := a.Apply(ctx, Request{Client: "writer", Sequence: 1, Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: modelEnvelope}, nil)
			require.NoError(t, err)
			if op == "returned" {
				_, err = a.Apply(ctx, Request{Client: "writer", Sequence: 2, Operation: "start", Sector: "sector", Token: out.Entry.Token}, nil)
				require.NoError(t, err)
			}
			dir := t.TempDir()
			exe, err := os.Executable()
			require.NoError(t, err)
			for _, mode := range []string{"crash", "recover"} {
				childCtx, stop := context.WithTimeout(ctx, 30*time.Second)
				cmd := exec.CommandContext(childCtx, exe, "-test.run=^TestCleanupJournalProcessHelper$", "-test.timeout=25s")
				cmd.Env = append(os.Environ(), "CURIO_CLEANUP_JOURNAL_TEST_MODE="+mode, "CURIO_CLEANUP_JOURNAL_TEST_LEDGER="+ledger, "CURIO_CLEANUP_JOURNAL_TEST_DIR="+dir, "CURIO_CLEANUP_JOURNAL_TEST_OP="+op, "CURIO_CLEANUP_JOURNAL_TEST_TOKEN="+out.Entry.Token)
				output, runErr := cmd.CombinedOutput()
				stop()
				if mode == "crash" {
					var exit *exec.ExitError
					require.True(t, errors.As(runErr, &exit), "%s", output)
					require.Equal(t, 74, exit.ExitCode(), "%s", output)
				} else {
					require.NoError(t, runErr, "%s", output)
				}
			}
			s, _, err := a.Inspect(ctx)
			require.NoError(t, err)
			if op == "cancel" {
				require.Empty(t, s.Entries)
			} else {
				require.Equal(t, "resident", s.Entries["sector"].State)
			}
			require.EqualValues(t, 1, s.Receipts["restartable-cleanup"].Sequence)
		})
	}
}
