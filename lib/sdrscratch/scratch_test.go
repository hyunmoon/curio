package sdrscratch

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func fixture(t *testing.T) (string, string, *Writer) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	p := filepath.Join(base, "s-t01000-42.sdr.tmp", Prefix+uuid.NewString())
	w, err := Begin(p)
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	require.NoError(t, unix.Fsetxattr(int(w.dir.Fd()), completionAttribute, []byte("incomplete"), 0))
	require.NoError(t, os.WriteFile(filepath.Join(p, "layer-1"), make([]byte, 8192), 0600))
	return base, p, w
}

func TestStartupReturnCertificate(t *testing.T) {
	for _, state := range []string{"active", "returned", "unknown"} {
		t.Run(state, func(t *testing.T) {
			base, p, w := fixture(t)
			if state == "returned" {
				require.NoError(t, w.Returned())
			}
			if state == "unknown" {
				require.NoError(t, unix.Fsetxattr(int(w.dir.Fd()), attribute, []byte(`{"Version":999,"pid":1}`), 0))
			}
			require.NoError(t, w.Close())
			r, err := Sweep(base)
			require.NoError(t, err)
			require.Len(t, r, 1)
			if state == "returned" {
				require.Equal(t, "reclaimed", r[0].Status)
				require.Equal(t, 1, r[0].FilesRemoved)
				require.NoFileExists(t, filepath.Join(p, "layer-1"))
				r, err = Sweep(base)
				require.NoError(t, err)
				require.Equal(t, "already_reclaimed", r[0].Status)
			} else {
				require.Equal(t, "needs_review", r[0].Status)
				require.FileExists(t, filepath.Join(p, "layer-1"))
			}
		})
	}
}

func TestStartupProcessHelper(t *testing.T) {
	p := os.Getenv("SDR_STARTUP_TEST_PATH")
	if p == "" {
		return
	}
	w, err := Begin(p)
	require.NoError(t, err)
	defer func() { _ = w.Close() }()
	require.NoError(t, unix.Fsetxattr(int(w.dir.Fd()), completionAttribute, []byte("incomplete"), 0))
	require.NoError(t, os.WriteFile(filepath.Join(p, "layer-1"), make([]byte, 8192), 0600))
	if os.Getenv("SDR_STARTUP_TEST_RETURNED") == "1" {
		require.NoError(t, w.Returned())
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestStartupSeparateProcessAndCrashBoundary(t *testing.T) {
	for _, returned := range []bool{false, true} {
		t.Run(fmt.Sprint(returned), func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			p := filepath.Join(base, "s-t01000-42.sdr.tmp", Prefix+uuid.NewString())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartupProcessHelper$", "-test.timeout=8s")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SDR_STARTUP_TEST_PATH=" + p}
			if returned {
				cmd.Env = append(cmd.Env, "SDR_STARTUP_TEST_RETURNED=1")
			}
			stdin, err := cmd.StdinPipe()
			require.NoError(t, err)
			defer func() { _ = stdin.Close() }()
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			cmd.Stderr = os.Stderr
			require.NoError(t, cmd.Start())
			defer func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "ready\n", line)
			r, err := Sweep(base)
			require.NoError(t, err)
			require.Equal(t, "live", r[0].Status)
			require.FileExists(t, filepath.Join(p, "layer-1"))
			// A separate process can begin without waiting for the live native body.
			next, err := Begin(filepath.Join(base, "s-t01000-43.sdr.tmp", Prefix+uuid.NewString()))
			require.NoError(t, err)
			defer func() { _ = next.Close() }()
			require.NoError(t, cmd.Process.Kill())
			require.Error(t, cmd.Wait())
			_, err = Sweep(base)
			require.NoError(t, err)
			if returned {
				require.NoFileExists(t, filepath.Join(p, "layer-1"))
			} else {
				require.FileExists(t, filepath.Join(p, "layer-1"), "parent death/free flock cannot establish native-child termination")
			}
		})
	}
}

func TestStartupCompletedLegacyAndIdentityProtection(t *testing.T) {
	for _, kind := range []string{"completed", "legacy", "copied-identity", "symlink", "directory", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			base, p, w := fixture(t)
			require.NoError(t, w.Returned())
			switch kind {
			case "completed":
				require.NoError(t, unix.Fsetxattr(int(w.dir.Fd()), completionAttribute, []byte(`{"Version":1}`), 0))
			case "legacy":
				q := filepath.Join(filepath.Dir(p), "attempt-"+uuid.NewString())
				require.NoError(t, os.Rename(p, q))
				p = q
			case "copied-identity":
				w.r.Inode++
				require.NoError(t, w.save("returned"))
			case "symlink":
				require.NoError(t, os.Symlink(filepath.Join(p, "layer-1"), filepath.Join(p, "link")))
			case "directory":
				require.NoError(t, os.Mkdir(filepath.Join(p, "nested"), 0700))
			case "hardlink":
				require.NoError(t, os.Link(filepath.Join(p, "layer-1"), filepath.Join(base, "shared")))
			}
			require.NoError(t, w.Close())
			r, err := Sweep(base)
			if kind == "legacy" || kind == "copied-identity" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NotEqual(t, "reclaimed", r[0].Status)
			require.FileExists(t, filepath.Join(p, "layer-1"))
		})
	}
}

func TestStartupReplacementAndPartialFailure(t *testing.T) {
	t.Run("replacement-before-return", func(t *testing.T) {
		_, p, w := fixture(t)
		require.NoError(t, os.Rename(p, p+"-old"))
		require.NoError(t, os.Mkdir(p, 0700))
		require.ErrorContains(t, w.Returned(), "pathname replaced")
		require.FileExists(t, filepath.Join(p+"-old", "layer-1"))
	})
	t.Run("replacement-during-unlink", func(t *testing.T) {
		_, p, w := fixture(t)
		require.NoError(t, w.Returned())
		n, err := w.reclaim(func(fd int, name string, flags int) error {
			require.NoError(t, os.Rename(p, p+"-old"))
			require.NoError(t, os.Mkdir(p, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(p, name), []byte("new attempt"), 0600))
			return unix.Unlinkat(fd, name, flags)
		})
		require.NoError(t, err)
		require.Equal(t, 1, n)
		b, err := os.ReadFile(filepath.Join(p, "layer-1"))
		require.NoError(t, err)
		require.Equal(t, "new attempt", string(b))
	})
	t.Run("partial-failure-restart", func(t *testing.T) {
		base, p, w := fixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(p, "layer-2"), []byte("second"), 0600))
		require.NoError(t, w.Returned())
		calls := 0
		n, err := w.reclaim(func(fd int, name string, flags int) error {
			calls++
			if calls == 2 {
				return unix.EACCES
			}
			return unix.Unlinkat(fd, name, flags)
		})
		require.ErrorIs(t, err, unix.EACCES)
		require.Equal(t, 1, n)
		require.NoError(t, w.Close())
		r, err := Sweep(base)
		require.NoError(t, err)
		require.Equal(t, 1, r[0].FilesRemoved)
	})
	t.Run("diagnostic-write-failure", func(t *testing.T) {
		base, p, w := fixture(t)
		// Inject ENOSPC at the persistent write boundary; do not fill a host disk.
		require.ErrorIs(t, w.saveWith("returned", func(int, string, []byte, int) error { return unix.ENOSPC }), unix.ENOSPC)
		require.Equal(t, "active", w.r.State)
		_, err := w.Reclaim()
		require.Error(t, err)
		require.NoError(t, w.Close())
		require.Error(t, w.Returned())
		require.Error(t, w.save("returned"))
		r, err := Sweep(base)
		require.NoError(t, err)
		require.Equal(t, "needs_review", r[0].Status)
		require.FileExists(t, filepath.Join(p, "layer-1"))
	})
}

func TestStartupScanCreationExclusion(t *testing.T) {
	base, _, w := fixture(t)
	require.NoError(t, w.Close())
	b, err := openDir(base)
	require.NoError(t, err)
	require.NoError(t, lock(b))
	_, err = Sweep(base)
	require.ErrorIs(t, err, unix.EWOULDBLOCK)
	_, err = Begin(filepath.Join(base, "s-t01000-43.sdr.tmp", Prefix+uuid.NewString()))
	require.ErrorIs(t, err, unix.EWOULDBLOCK)
	require.NoError(t, b.Close())
	next, err := Begin(filepath.Join(base, "s-t01000-43.sdr.tmp", Prefix+uuid.NewString()))
	require.NoError(t, err)
	require.NoError(t, next.Close())
}

func TestStartupUnknownNonparticipantCannotOptIn(t *testing.T) {
	base, p, w := fixture(t)
	// Metadata from an old writer cannot acquire authority by a newly created
	// lock or a PID match. Neither PID nor wall clock appears in the contract.
	require.NoError(t, unix.Fsetxattr(int(w.dir.Fd()), attribute, []byte(`{"pid":123,"state":"dead"}`), 0))
	require.NoError(t, w.Close())
	r, err := Sweep(base)
	require.NoError(t, err)
	require.Equal(t, "needs_review", r[0].Status)
	b, err := os.ReadFile(filepath.Join(p, "layer-1"))
	require.NoError(t, err)
	require.Len(t, b, 8192)
}
