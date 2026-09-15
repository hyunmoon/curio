//go:build linux

package sdrscratch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func reviewLinux(t *testing.T) string {
	t.Helper()
	if os.Getenv("CURIO_SDR_REVIEW_LINUX") != "1" {
		t.Skip("owned disposable Linux root/systemd/ext4 or XFS fixture opt-in required")
	}
	require.Equal(t, "disposable", os.Getenv("CURIO_SDR_DISCARD_TEST_TARGET"))
	require.Zero(t, os.Geteuid())
	root := os.Getenv("CURIO_SDR_REVIEW_ROOT")
	require.True(t, strings.HasPrefix(filepath.Base(root), "curio-sdr-review-"))
	f, e := trustedDir(root)
	require.NoError(t, e)
	require.NoError(t, supportedFS(f))
	require.NoError(t, f.Close())
	return root
}

func linuxState(t *testing.T, c *ManagedConfig) {
	t.Helper()
	root := reviewLinux(t)
	state, e := os.MkdirTemp(root, "state-")
	require.NoError(t, e)
	c.StateDir = state
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(state)) }) // exact owned test directory
}

func TestManagedLinuxReviewAccounting(t *testing.T) {
	reviewLinux(t)
	c, ts := legacyFixture(t)
	linuxState(t, c)
	base := filepath.Join(ts[0].Root, "cache")
	key := filepath.Join(ts[0].Root, "key")
	require.NoError(t, os.Mkdir(key, 0755))
	target := filepath.Join(ts[0].Root, ts[0].Relative)
	d, e := openDir(target)
	require.NoError(t, e)
	dev, ino, e := identity(d)
	require.NoError(t, e)
	require.NoError(t, d.Close())
	file, e := os.Open(filepath.Join(target, "layer"))
	require.NoError(t, e)
	fd, fi, e := identity(file)
	require.NoError(t, e)
	name, e := c.spaceStart(base, dev, ino, filepath.Base(target), []openIdentity{{fd, fi}})
	require.NoError(t, e)
	// Actual host /proc, root-owned journal, statfs and atomic publication.
	require.Error(t, c.checkSpace(base), "residual/open file is protected")
	require.NoError(t, os.Remove(filepath.Join(target, "layer")))
	require.ErrorContains(t, c.checkSpace(base), "open descriptor")
	require.NoError(t, file.Close())
	// Deterministic old absolute threshold plus an actual independent writer.
	var w spaceWitness
	require.NoError(t, readPrivateJSON(filepath.Join(c.StateDir, name), &w))
	w.MinimumFree = ^uint64(0)
	raw, e := json.Marshal(w)
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(filepath.Join(c.StateDir, name), raw, 0600))
	cmd := exec.Command("dd", "if=/dev/zero", "of="+filepath.Join(base, "normal-output"), "bs=4096", "count=2", "conv=fsync")
	output, e := cmd.CombinedOutput()
	require.NoError(t, e, string(output))
	require.NoError(t, c.checkSpace(base))
	require.FileExists(t, filepath.Join(base, "normal-output"))
	// A corrupt own-base record must not block a different registered base.
	p := filepath.Join(c.StateDir, spacePrefix(base)+"1-1.json")
	require.NoError(t, os.WriteFile(p, []byte("{"), 0600))
	require.NoError(t, c.checkSpace(key))
	require.Error(t, c.checkSpace(base))
	require.FileExists(t, p)
}

func TestManagedLinuxReviewMounts(t *testing.T) {
	reviewLinux(t)
	for _, kind := range []string{"bind", "different"} {
		t.Run(kind, func(t *testing.T) {
			c, ts := legacyFixture(t)
			linuxState(t, c)
			base := filepath.Join(ts[0].Root, "cache")
			plan, e := BuildLegacyPlan(c, ts, c.Host, "boot")
			require.NoError(t, e)
			other := t.TempDir()
			mounted := false
			defer func() {
				if mounted {
					require.NoError(t, unix.Unmount(base, 0))
				}
			}()
			if kind == "bind" {
				require.NoError(t, unix.Mount(other, base, "", unix.MS_BIND, ""))
			} else {
				require.NoError(t, unix.Mount("tmpfs", base, "tmpfs", 0, "size=4m"))
			}
			mounted = true
			// Populate exact same relative names on the substituted mount. Preview
			// itself must reject enrollment, not merely the old plan's inode mismatch.
			for _, v := range ts {
				p := filepath.Join(v.Root, v.Relative)
				require.NoError(t, os.Mkdir(p, 0755))
				require.NoError(t, os.WriteFile(filepath.Join(p, "layer"), []byte("keep"), 0600))
			}
			_, e = BuildLegacyPlan(c, ts, c.Host, "boot")
			require.Error(t, e)
			calls := 0
			_, e = executeLegacy(c, plan, filepath.Join(c.StateDir, "never-journal"), strings.NewReader("DISCARD LEGACY 2\n"), &bytes.Buffer{}, c.Host, "boot", func() error { return nil }, func(int, string, int) error { calls++; return nil }, true)
			require.Error(t, e)
			require.Zero(t, calls)
			for _, v := range ts {
				require.FileExists(t, filepath.Join(v.Root, v.Relative, "layer"))
			}
			require.NoError(t, unix.Unmount(base, 0))
			mounted = false
			// Same mount positive path uses the real accounting, but this unit test's
			// injected maintenance guard. Connected test below uses the real guard.
			_, e = executeLegacy(c, plan, filepath.Join(c.StateDir, "same-mount.jsonl"), strings.NewReader("DISCARD LEGACY 2\n"), &bytes.Buffer{}, c.Host, "boot", func() error { return nil }, unix.Unlinkat, true)
			require.NoError(t, e)
			require.FileExists(t, filepath.Join(ts[0].Root, "cache/s-t03199233-160988/keep"))
		})
	}
}

type launcherNote struct {
	PID  int
	Path string
}

// Tiny replacement worker/native child; production launcher/config/lease,
// scanner/accounting are not mocked. No real sealing/CUDA is performed.
func TestManagedLinuxLauncherWorker(t *testing.T) {
	mode := os.Getenv("CURIO_SDR_REVIEW_WORKER")
	if mode == "" {
		return
	}
	control := os.Getenv("CURIO_SDR_REVIEW_CONTROL")
	base := os.Getenv("CURIO_SDR_REVIEW_BASE")
	if mode == "child" {
		f, e := os.OpenFile(filepath.Join(base, "layer-child"), os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, e)
		defer func() { _ = f.Close() }()
		_, e = f.Write(make([]byte, 8192))
		require.NoError(t, e)
		require.NoError(t, f.Sync())
		require.NoError(t, os.WriteFile(filepath.Join(control, "child-ready"), []byte("ready"), 0600))
		require.Eventually(t, func() bool { _, e := os.Stat(filepath.Join(control, "child-exit")); return e == nil }, 35*time.Second, 20*time.Millisecond)
		return
	}
	if mode == "blocked" {
		_, e := Sweep(base)
		require.ErrorContains(t, e, "subtree")
		p := filepath.Join(base, "s-t01000-99.sdr.tmp", Prefix+uuid.NewString())
		_, e = Begin(p)
		require.Error(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(control, "blocked"), []byte("PASS"), 0600))
		return
	}
	if mode == "recover" {
		r, e := Sweep(base)
		require.NoError(t, e)
		removed := 0
		for _, v := range r {
			removed += v.FilesRemoved
		}
		require.Equal(t, 2, removed)
		p := filepath.Join(base, "s-t01000-99.sdr.tmp", Prefix+uuid.NewString())
		w, e := Begin(p)
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(p, "new-layer"), make([]byte, 8192), 0600))
		require.NoError(t, w.Returned())
		n, e := w.DiscardOwn()
		require.NoError(t, e)
		require.Equal(t, 1, n)
		require.NoError(t, w.Close())
		require.NoError(t, os.WriteFile(filepath.Join(control, "recovered"), []byte("PASS"), 0600))
		return
	}
	require.Equal(t, "hold", mode)
	require.Equal(t, "sdisk-4slot", os.Getenv("CURIO_PERSONAL_STORAGE_PROFILE"))
	require.Equal(t, "4", os.Getenv("CURIO_SDR_REVIEW_SLOTS"))
	require.Equal(t, "43m45s", os.Getenv("CURIO_SDR_REVIEW_INTERVAL"))
	p := filepath.Join(base, "s-t01000-42.sdr.tmp", Prefix+uuid.NewString())
	w, e := Begin(p)
	require.NoError(t, e)
	defer func() { _ = w.Close() }()
	require.NoError(t, os.WriteFile(filepath.Join(p, "layer-parent"), make([]byte, 8192), 0600))
	child := exec.Command(os.Args[0], "-test.run=^TestManagedLinuxLauncherWorker$", "-test.timeout=40s")
	child.Env = append(os.Environ(), "CURIO_SDR_REVIEW_WORKER=child", "CURIO_SDR_REVIEW_BASE="+p)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	require.NoError(t, child.Start())
	require.Eventually(t, func() bool { _, e := os.Stat(filepath.Join(control, "child-ready")); return e == nil }, 10*time.Second, 20*time.Millisecond)
	raw, e := json.Marshal(launcherNote{os.Getpid(), p})
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(filepath.Join(control, "parent-ready"), raw, 0600))
	_ = child.Wait()
}

func reviewCommand(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	require.NoError(t, e, "%v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func TestManagedLinuxReviewConnected(t *testing.T) {
	root := reviewLinux(t)
	helper := os.Getenv("CURIO_SDR_REVIEW_HELPER")
	require.True(t, filepath.IsAbs(helper))
	require.FileExists(t, helper)
	// Explicitly disposable host only: two new UUID-named units are the sole
	// services created/started/stopped/masked. Never use an existing unit name.
	c, ts := legacyFixture(t)
	linuxState(t, c)
	base := filepath.Join(ts[0].Root, "cache")
	// Fixture legacy directories would intentionally block entry; keep them
	// outside this enrolled storage until the later maintenance phase.
	for _, v := range ts {
		require.NoError(t, os.Rename(filepath.Join(v.Root, v.Relative), filepath.Join(root, filepath.Base(v.Relative)+"-"+c.Domain)))
	}
	control := c.StateDir
	config := filepath.Join(control, "domain.json")
	names := []string{"curio-sdr-review-a-" + c.Domain + ".service", "curio-sdr-review-b-" + c.Domain + ".service"}
	unitPath := func(name string) string { return filepath.Join("/etc/systemd/system", name) }
	for _, name := range names {
		_, e := os.Lstat(unitPath(name))
		require.True(t, os.IsNotExist(e))
	}
	defer func() {
		for _, name := range names {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_ = exec.CommandContext(ctx, "systemctl", "stop", name).Run()
			cancel()
			_ = exec.Command("systemctl", "unmask", "--runtime", name).Run()
			_ = os.Remove(unitPath(name))
		}
		_ = exec.Command("systemctl", "daemon-reload").Run()
	}()
	writeUnit := func(name, mode string) {
		start := "/usr/bin/sleep 90"
		if mode != "bootstrap" {
			start = fmt.Sprintf("%s run --config %s -- %s -test.run=^TestManagedLinuxLauncherWorker$ -test.timeout=45s", helper, config, os.Args[0])
		}
		// The one-shot driver rejects whitespace/% in fixture executable paths.
		data := fmt.Sprintf("[Unit]\nDescription=Owned disposable SDR review fixture\n[Service]\nType=simple\nDelegate=yes\nKillMode=control-group\nSendSIGKILL=yes\nTimeoutStopSec=5\nEnvironment=CURIO_SDR_REVIEW_WORKER=%s CURIO_SDR_REVIEW_CONTROL=%s CURIO_SDR_REVIEW_BASE=%s CURIO_PERSONAL_STORAGE_PROFILE=sdisk-4slot CURIO_SDR_REVIEW_SLOTS=4 CURIO_SDR_REVIEW_INTERVAL=43m45s\nExecStart=%s\n", mode, control, base, start)
		require.NoError(t, os.WriteFile(unitPath(name), []byte(data), 0600))
	}
	for _, n := range names {
		writeUnit(n, "bootstrap")
	}
	reviewCommand(t, "systemctl", "daemon-reload")
	reviewCommand(t, append([]string{"systemctl", "start"}, names...)...)
	captured, e := CaptureManagedConfig(control, []string{ts[0].Root}, names)
	require.NoError(t, e)
	c = captured
	require.NoError(t, WriteNewJSON(config, c))
	reviewCommand(t, append([]string{"systemctl", "stop"}, names...)...)
	writeUnit(names[0], "hold")
	writeUnit(names[1], "blocked")
	reviewCommand(t, "systemctl", "daemon-reload")
	reviewCommand(t, "systemctl", "start", names[0])
	waitFile := func(n string) {
		require.Eventually(t, func() bool { _, e := os.Stat(filepath.Join(control, n)); return e == nil }, 12*time.Second, 25*time.Millisecond)
	}
	waitFile("parent-ready")
	var note launcherNote
	raw, e := os.ReadFile(filepath.Join(control, "parent-ready"))
	require.NoError(t, e)
	require.NoError(t, json.Unmarshal(raw, &note))
	pid, e := strconv.Atoi(reviewCommand(t, "systemctl", "show", "--value", "--property=MainPID", names[0]))
	require.NoError(t, e)
	require.Greater(t, pid, 1)
	require.NoError(t, unix.Kill(pid, unix.SIGSTOP))
	defer func() { _ = unix.Kill(pid, unix.SIGCONT) }()
	require.NoError(t, unix.Kill(note.PID, unix.SIGKILL))
	reviewCommand(t, "systemctl", "start", names[1])
	waitFile("blocked")
	require.FileExists(t, filepath.Join(note.Path, "layer-child"))
	require.NoError(t, os.WriteFile(filepath.Join(control, "child-exit"), []byte("exit"), 0600))
	// Read the actual persisted run, then require whole-subtree termination.
	f, e := openDir(note.Path)
	require.NoError(t, e)
	data, e := get(f, attribute)
	require.NoError(t, e)
	require.NoError(t, f.Close())
	var record record
	require.NoError(t, json.Unmarshal(data, &record))
	boundary := &linuxBoundary{c}
	require.NotNil(t, record.Run)
	require.Eventually(t, func() bool { stopped, e := boundary.Stopped(base, *record.Run); return e == nil && stopped }, 12*time.Second, 25*time.Millisecond)
	require.NoError(t, unix.Kill(pid, unix.SIGCONT))
	reviewCommand(t, "systemctl", "stop", names[0], names[1])
	writeUnit(names[1], "recover")
	reviewCommand(t, "systemctl", "daemon-reload")
	reviewCommand(t, "systemctl", "start", names[1])
	waitFile("recovered")
	reviewCommand(t, "systemctl", "stop", names[1])
	require.NoFileExists(t, filepath.Join(note.Path, "layer-parent"))
	require.NoFileExists(t, filepath.Join(note.Path, "layer-child"))
	require.FileExists(t, filepath.Join(ts[0].Root, "cache/s-t03199233-160988/keep"))
	// Real MaintenanceGuard and exclusive lease, not an injected boolean.
	reviewCommand(t, "systemctl", "mask", "--runtime", names[0], names[1])
	p := filepath.Join(base, "s-t01000-100.tmp")
	require.NoError(t, os.Mkdir(p, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(p, "legacy"), make([]byte, 8192), 0600))
	h, boot, e := HostBoot()
	require.NoError(t, e)
	plan, e := BuildLegacyPlan(c, []LegacyTarget{{Root: ts[0].Root, Relative: "cache/s-t01000-100.tmp"}}, h, boot)
	require.NoError(t, e)
	_, e = ExecuteLegacy(c, plan, filepath.Join(control, "cancel.jsonl"), strings.NewReader("no\n"), &bytes.Buffer{})
	require.ErrorContains(t, e, "cancelled")
	require.FileExists(t, filepath.Join(p, "legacy"))
	r, e := ExecuteLegacy(c, plan, filepath.Join(control, "apply.jsonl"), strings.NewReader("DISCARD LEGACY 1\n"), &bytes.Buffer{})
	require.NoError(t, e)
	require.Equal(t, 1, r[0].FilesRemoved)
	require.FileExists(t, filepath.Join(ts[0].Root, "cache/s-t03199233-160988/keep"))
}
