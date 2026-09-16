package sdrscratch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func accessFixture(t *testing.T) (*ManagedConfig, string) {
	s, roots := personalFixture(t)
	c, err := s.register(roots[0], "0")
	require.NoError(t, err)
	t.Setenv(PersonalCleanupEnv, "1")
	t.Setenv(PersonalPolicyEnv, "")
	old := personalSessionPtr.Swap(s)
	t.Cleanup(func() { personalSessionPtr.Store(old) })
	return c, filepath.Join(roots[0], "cache", "s-t01000-42")
}

func TestSectorAccessWaitAndLifetime(t *testing.T) {
	c, p := accessFixture(t)
	gate, err := sectorGate(c, filepath.Base(p), true, openDir)
	require.NoError(t, err)
	defer func() { _ = gate.Close() }()
	_, err = AccessPaths(p)
	require.ErrorIs(t, err, ErrAccessBusy)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var release func()
	go func() { var err error; release, err = AccessPathsContext(ctx, p); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("access did not wait: %v", err)
	case <-time.After(60 * time.Millisecond):
	}
	require.NoError(t, gate.Close())
	require.NoError(t, <-done)
	cancel()
	_, err = sectorGate(c, filepath.Base(p), true, openDir)
	require.ErrorIs(t, err, ErrAccessBusy, "cancellation cannot unlock live I/O")
	release()
	release()
	gate, err = sectorGate(c, filepath.Base(p), true, openDir)
	require.NoError(t, err)
	require.NoError(t, gate.Close())
}

func TestSectorAccessDisabled(t *testing.T) {
	t.Setenv(PersonalCleanupEnv, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release, err := AccessPathsContext(ctx, "not-a-sector")
	require.NoError(t, err)
	release()
}

func TestSectorAccessCancellationTimeoutAndPartialRelease(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			c, p := accessFixture(t)
			first := filepath.Join(filepath.Dir(p), "s-t01000-41")
			gate, err := sectorGate(c, filepath.Base(p), true, openDir)
			require.NoError(t, err)
			defer func() { _ = gate.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := AccessPathsContext(ctx, first, p); done <- err }()
			if mode == "cancel" {
				time.Sleep(30 * time.Millisecond)
				cancel()
			}
			err = <-done
			require.ErrorIs(t, err, ErrAccessBusy)
			if mode == "cancel" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, context.DeadlineExceeded)
			}
			free, err := sectorGate(c, filepath.Base(first), true, openDir)
			require.NoError(t, err, "earlier path not leaked on later busy")
			require.NoError(t, free.Close())
		})
	}
}

func TestSectorAccessHardErrorsAreNotBusy(t *testing.T) {
	c, p := accessFixture(t)
	gate, err := sectorGate(c, filepath.Base(p), true, openDir)
	require.NoError(t, err)
	name := gate.Name()
	require.NoError(t, gate.Close())
	gatePath := filepath.Join(c.StateDir, name)
	require.NoError(t, os.Chmod(gatePath, 0644))
	_, err = AccessPathsContext(context.Background(), p)
	require.ErrorContains(t, err, "unsafe sector gate")
	require.False(t, errors.Is(err, ErrAccessBusy))
	require.NoError(t, os.Chmod(gatePath, 0600))
	_, err = sectorGate(c, filepath.Base(p), false, func(string) (*os.File, error) { return nil, os.ErrPermission })
	require.ErrorIs(t, err, os.ErrPermission)
	require.False(t, errors.Is(err, ErrAccessBusy))
	_, err = AccessPathsContext(context.Background(), filepath.Join(filepath.Dir(p), "not-a-sector"))
	require.ErrorContains(t, err, "invalid sector access path")
	require.False(t, errors.Is(err, ErrAccessBusy))
}
