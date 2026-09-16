package sdrscratch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

func autoFixture(t *testing.T) (*ManagedConfig, string, autoIO, AutoState) {
	t.Helper()
	s, roots := personalFixture(t)
	c, err := s.register(roots[0], "0")
	require.NoError(t, err)
	base := filepath.Join(roots[0], "cache")
	io := autoTestIO(c)
	state := func(_ AutoTarget, apply func(AutoStage) error) error {
		return apply(AutoStage{true, "fixture pre-SDR", []string{"sc-02-data-layer-1.dat", "sc-02-data-layer-2.dat"}, 2048})
	}
	return c, base, io, state
}

func autoTestIO(c *ManagedConfig) autoIO {
	access := reviewAccess()
	io := autoIO{participants: func(*ManagedConfig) error { return nil }, openState: openDir, unlink: unix.Unlinkat, boundary: platformBoundary}
	io.spaceStart = func(base string, dev, ino uint64, target string, files []openIdentity) (string, error) {
		d, e := openDir(c.StateDir)
		if e != nil {
			return "", e
		}
		defer func() { _ = d.Close() }()
		return publishSpace(d, c.StateDir, base, dev, ino, target, files, access.read)
	}
	io.checkSpace = func(base string) error { return c.checkSpaceWith(base, access) }
	return io
}

func autoWrite(t *testing.T, base, relative string, n int) string {
	t.Helper()
	p := filepath.Join(base, relative)
	require.NoError(t, os.MkdirAll(p, 0700))
	for i := 1; i <= n; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(p, fmt.Sprintf("sc-02-data-layer-%d.dat", i)), make([]byte, 2048), 0600))
	}
	return p
}

func TestAutoDiscardFirstTransition(t *testing.T) {
	for _, relative := range []string{"s-t01000-42.tmp", "s-t01000-42.sdr.tmp/attempt-" + uuid.NewString(), "s-t01000-42.sdr.tmp/" + Prefix + uuid.NewString(), "s-t01000-42"} {
		t.Run(relative, func(t *testing.T) {
			c, base, io, state := autoFixture(t)
			target := autoWrite(t, base, relative, 1)
			r, err := autoDiscard(c, base, state, io)
			require.NoError(t, err)
			require.Len(t, r, 1)
			require.Equal(t, "reclaimed", r[0].Status)
			require.Equal(t, 1, r[0].FilesRemoved)
			require.NoDirExists(t, target)
			// A new no-replace publication is possible; no stale empty canonical
			// or legacy tombstone blocks the next calculation.
			require.NoError(t, os.Mkdir(target, 0700))
		})
	}
}

func TestAutoDiscardPreservation(t *testing.T) {
	for _, mode := range []string{"after-sdr", "tree-rc-failed", "missing-pipeline", "db-error", "receipt", "full-legacy-layout", "unknown-file", "symlink", "hardlink", "unconverted", "reader"} {
		t.Run(mode, func(t *testing.T) {
			c, base, io, state := autoFixture(t)
			relative := "s-t01000-42"
			n := 1
			if mode == "full-legacy-layout" {
				n = 2
			}
			p := autoWrite(t, base, relative, n)
			switch mode {
			case "after-sdr", "tree-rc-failed", "missing-pipeline":
				state = func(_ AutoTarget, f func(AutoStage) error) error {
					return f(AutoStage{Reason: mode, LayerNames: []string{"sc-02-data-layer-1.dat", "sc-02-data-layer-2.dat"}, LayerBytes: 2048})
				}
			case "db-error":
				state = func(AutoTarget, func(AutoStage) error) error { return errors.New("DB unavailable") }
			case "receipt":
				require.NoError(t, unix.Setxattr(p, completionAttribute, []byte(`{"Version":1}`), 0))
			case "unknown-file":
				require.NoError(t, os.WriteFile(filepath.Join(p, "sc-02-data-tree-d.dat"), []byte("keep"), 0600))
			case "symlink":
				require.NoError(t, os.Symlink(filepath.Join(p, "sc-02-data-layer-1.dat"), filepath.Join(p, "sc-02-data-layer-2.dat")))
			case "hardlink":
				require.NoError(t, os.Link(filepath.Join(p, "sc-02-data-layer-1.dat"), filepath.Join(base, "keep")))
			case "unconverted":
				io.participants = func(*ManagedConfig) error { return errors.New("live pre-protocol process") }
			case "reader":
				f, e := sectorGate(c, relative, false, openDir)
				require.NoError(t, e)
				defer func() { _ = f.Close() }()
			}
			r, e := autoDiscard(c, base, state, io)
			require.NoError(t, e)
			require.Len(t, r, 1)
			require.NotEqual(t, "reclaimed", r[0].Status)
			require.Zero(t, r[0].FilesRemoved)
			require.FileExists(t, filepath.Join(p, "sc-02-data-layer-1.dat"))
		})
	}
}

func TestAutoDiscardPartialFailureRestart(t *testing.T) {
	c, base, io, state := autoFixture(t)
	p := autoWrite(t, base, "s-t01000-42.tmp", 2)
	calls := 0
	io.unlink = func(fd int, name string, flags int) error {
		calls++
		if calls == 2 {
			return unix.EIO
		}
		return unix.Unlinkat(fd, name, flags)
	}
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "partial_or_space_unconfirmed", r[0].Status)
	require.Equal(t, 1, r[0].FilesRemoved)
	io.unlink = unix.Unlinkat
	r, e = autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "reclaimed", r[0].Status)
	require.Equal(t, 1, r[0].FilesRemoved)
	require.NoDirExists(t, p)
}

func TestAutoDiscardOpenInodeIsNotReclaimedSpace(t *testing.T) {
	c, base, io, state := autoFixture(t)
	p := autoWrite(t, base, "s-t01000-42.tmp", 1)
	f, e := os.Open(filepath.Join(p, "sc-02-data-layer-1.dat"))
	require.NoError(t, e)
	defer func() { _ = f.Close() }()
	a := reviewAccess()
	a.open = func([]openIdentity) (bool, error) { return true, nil }
	io.checkSpace = func(base string) error { return c.checkSpaceWith(base, a) }
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "partial_or_space_unconfirmed", r[0].Status)
	b := make([]byte, 1)
	_, e = f.Read(b)
	require.NoError(t, e, "real open unlinked inode remains readable")
	require.NoError(t, f.Close())
	a.open = func([]openIdentity) (bool, error) { return false, nil }
	r, e = autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "empty", r[0].Status)
	require.NoDirExists(t, p)
}

func TestAutoDiscardIsolationAndReentry(t *testing.T) {
	c, base, io, state := autoFixture(t)
	busy := autoWrite(t, base, "s-t01000-42.tmp", 1)
	ready := autoWrite(t, base, "s-t02000-42.tmp", 1)
	reader, e := sectorGate(c, "s-t01000-42", false, openDir)
	require.NoError(t, e)
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Len(t, r, 2)
	require.DirExists(t, busy)
	require.NoDirExists(t, ready)
	require.NoError(t, reader.Close())
	r, e = autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Len(t, r, 1)
	require.Equal(t, "reclaimed", r[0].Status)
	require.NoDirExists(t, busy)
}

func TestAutoDiscardGateBlocksClaimUntilReturn(t *testing.T) {
	c, base, io, _ := autoFixture(t)
	p := autoWrite(t, base, "s-t01000-42.tmp", 1)
	state := func(target AutoTarget, f func(AutoStage) error) error {
		_, err := sectorGate(c, target.Sector, false, openDir)
		require.Error(t, err)
		return f(AutoStage{true, "fixture", []string{"sc-02-data-layer-1.dat", "sc-02-data-layer-2.dat"}, 2048})
	}
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "reclaimed", r[0].Status)
	require.NoDirExists(t, p)
	f, e := sectorGate(c, "s-t01000-42", false, openDir)
	require.NoError(t, e)
	require.NoError(t, f.Close())
}

func TestAutoDiscardGateProcess(t *testing.T) {
	raw := os.Getenv("CURIO_AUTO_GATE_FIXTURE")
	if raw == "" {
		return
	}
	var c ManagedConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &c))
	if os.Getenv("CURIO_AUTO_CRASH_FIXTURE") == "1" {
		io := autoTestIO(&c)
		io.unlink = func(fd int, name string, flags int) error {
			e := unix.Unlinkat(fd, name, flags)
			if e == nil {
				os.Exit(23)
			}
			return e
		}
		_, e := autoDiscard(&c, filepath.Join(c.Storage[0].Root, "cache"), func(_ AutoTarget, f func(AutoStage) error) error {
			return f(AutoStage{true, "fixture", []string{"sc-02-data-layer-1.dat", "sc-02-data-layer-2.dat"}, 2048})
		}, io)
		require.NoError(t, e)
		t.Fatal("crash injection did not execute")
	}
	f, e := sectorGate(&c, "s-t01000-42", false, openDir)
	require.NoError(t, e)
	defer func() { _ = f.Close() }()
	fmt.Println("gate-held")
	_, e = bufio.NewReader(os.Stdin).ReadString('\n')
	require.NoError(t, e)
}

func TestAutoDiscardProcessCrashResume(t *testing.T) {
	c, base, io, state := autoFixture(t)
	p := autoWrite(t, base, "s-t01000-42.tmp", 2)
	raw, e := json.Marshal(c)
	require.NoError(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAutoDiscardGateProcess$", "-test.timeout=12s")
	cmd.Env = []string{"CURIO_AUTO_GATE_FIXTURE=" + string(raw), "CURIO_AUTO_CRASH_FIXTURE=1", "TMPDIR=" + os.Getenv("TMPDIR")}
	output, e := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, e, &exit, string(output))
	require.Equal(t, 23, exit.ExitCode())
	remaining, e := os.ReadDir(p)
	require.NoError(t, e)
	require.Len(t, remaining, 1)
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "reclaimed", r[0].Status)
	require.Equal(t, 1, r[0].FilesRemoved)
	require.NoDirExists(t, p)
}

func TestAutoDiscardIndependentReader(t *testing.T) {
	c, base, io, state := autoFixture(t)
	p := autoWrite(t, base, "s-t01000-42.tmp", 1)
	other := autoWrite(t, base, "s-t02000-42.tmp", 1)
	raw, e := json.Marshal(c)
	require.NoError(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAutoDiscardGateProcess$", "-test.timeout=12s")
	cmd.Env = []string{"CURIO_AUTO_GATE_FIXTURE=" + string(raw), "TMPDIR=" + os.Getenv("TMPDIR")}
	input, e := cmd.StdinPipe()
	require.NoError(t, e)
	output, e := cmd.StdoutPipe()
	require.NoError(t, e)
	require.NoError(t, cmd.Start())
	defer func() { _ = input.Close(); _ = cmd.Wait() }()
	line, e := bufio.NewReader(output).ReadString('\n')
	require.NoError(t, e)
	require.Equal(t, "gate-held\n", line)
	_, e = autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.DirExists(t, p)
	require.NoDirExists(t, other)
	_, e = input.Write([]byte("return\n"))
	require.NoError(t, e)
	require.NoError(t, input.Close())
	require.NoError(t, cmd.Wait())
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "reclaimed", r[0].Status)
	require.NoDirExists(t, p)
}

func TestAutoDiscardRecordedLiveRun(t *testing.T) {
	c, base, io, state := autoFixture(t)
	rel := filepath.Join("s-t01000-42.sdr.tmp", Prefix+uuid.NewString())
	p := autoWrite(t, base, rel, 1)
	require.NoError(t, os.WriteFile(filepath.Join(p, "native-interrupted-work"), []byte("private"), 0600))
	d, e := openDir(p)
	require.NoError(t, e)
	defer func() { _ = d.Close() }()
	dev, ino, e := identity(d)
	require.NoError(t, e)
	b := new(testBoundary)
	run, e := b.Current(base)
	require.NoError(t, e)
	raw, e := json.Marshal(record{Version: 2, Name: filepath.Base(rel), Root: "s-t01000-42.sdr.tmp", Device: dev, Inode: ino, State: "active", Run: run})
	require.NoError(t, e)
	require.NoError(t, unix.Fsetxattr(int(d.Fd()), attribute, raw, 0))
	io.boundary = func(*ManagedConfig) (Boundary, error) { return b, nil }
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Zero(t, r[0].FilesRemoved)
	require.DirExists(t, p)
	b.ended.Store(true)
	r, e = autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "reclaimed", r[0].Status)
	require.NoDirExists(t, p)
}

func TestAutoDiscardENOSPCAndOtherRoot(t *testing.T) {
	c, base, io, state := autoFixture(t)
	p := autoWrite(t, base, "s-t01000-42.tmp", 1)
	goodStart := io.spaceStart
	io.spaceStart = func(string, uint64, uint64, string, []openIdentity) (string, error) { return "", unix.ENOSPC }
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Zero(t, r[0].FilesRemoved)
	require.DirExists(t, p)
	c2, base2, io2, state2 := autoFixture(t)
	p2 := autoWrite(t, base2, "s-t01000-42.tmp", 1)
	r, e = autoDiscard(c2, base2, state2, io2)
	require.NoError(t, e)
	require.Equal(t, "reclaimed", r[0].Status)
	require.NoDirExists(t, p2)
	// Scratch xattr writes are never needed for legacy adoption. If the
	// separate witness filesystem is healthy, retry recovers the same target.
	io.spaceStart = goodStart
	r, e = autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "reclaimed", r[0].Status)
	require.NoDirExists(t, p)
}

func TestAutoDiscardReplacementAfterState(t *testing.T) {
	c, base, io, state := autoFixture(t)
	p := autoWrite(t, base, "s-t01000-42.tmp", 1)
	require.NoError(t, os.WriteFile(filepath.Join(p, "native-interrupted-work"), []byte("private"), 0600))
	calls := 0
	io.participants = func(*ManagedConfig) error {
		calls++
		if calls == 2 {
			require.NoError(t, os.Rename(p, p+".preserved"))
			autoWrite(t, base, "s-t01000-42.tmp", 1)
		}
		return nil
	}
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Zero(t, r[0].FilesRemoved)
	require.FileExists(t, filepath.Join(p, "sc-02-data-layer-1.dat"))
	require.FileExists(t, filepath.Join(p+".preserved", "sc-02-data-layer-1.dat"))
	require.FileExists(t, filepath.Join(p+".preserved", "native-interrupted-work"))
}
