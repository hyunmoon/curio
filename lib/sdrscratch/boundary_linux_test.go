//go:build linux

package sdrscratch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type kernelTestEntry struct{ run ManagedRun }

func (b kernelTestEntry) Current(string) (*ManagedRun, error)      { return &b.run, nil }
func (b kernelTestEntry) Stopped(string, ManagedRun) (bool, error) { return false, nil }

func TestManagedKernelHelper(t *testing.T) {
	p := os.Getenv("CURIO_DISCARD_KERNEL_HELPER")
	if p == "" {
		return
	}
	if os.Getenv("CURIO_DISCARD_KERNEL_CHILD") == "1" {
		f, err := os.OpenFile(filepath.Join(p, "layer-child"), os.O_CREATE|os.O_RDWR, 0600)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		_, err = f.Write(make([]byte, 8192))
		require.NoError(t, err)
		fmt.Println("child-ready")
		time.Sleep(8 * time.Second)
		return
	}
	var r ManagedRun
	require.NoError(t, json.Unmarshal([]byte(os.Getenv("CURIO_DISCARD_KERNEL_RUN")), &r))
	w, err := BeginWithOptions(p, Options{Boundary: kernelTestEntry{r}})
	require.NoError(t, err)
	defer func() { _ = w.Close() }()
	require.NoError(t, os.WriteFile(filepath.Join(p, "layer-parent"), make([]byte, 8192), 0600))
	child := exec.Command(os.Args[0], "-test.run=^TestManagedKernelHelper$", "-test.timeout=12s")
	child.Env = append(os.Environ(), "CURIO_DISCARD_KERNEL_CHILD=1")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	require.NoError(t, child.Start())
	// Child inherits the actual cgroup, not the parent's CLOEXEC directory lock.
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	_ = child.Wait()
}

func TestManagedLinuxKernelCrashAndChild(t *testing.T) {
	if os.Getenv("CURIO_SDR_DISCARD_CGROUP_ITEST") != "1" {
		t.Skip("explicit disposable Linux cgroup test opt-in missing")
	}
	require.Equal(t, "disposable", os.Getenv("CURIO_SDR_DISCARD_TEST_TARGET"))
	parent, err := selfCgroup()
	require.NoError(t, err)
	require.Equal(t, "curio-sdr-discard-test.service", filepath.Base(parent), "run only in the dedicated disposable systemd test unit")
	// The fixture itself owns all files/processes; no enrolled production config.
	c, targets := legacyFixture(t)
	base := filepath.Join(targets[0].Root, "key")
	require.NoError(t, os.Mkdir(base, 0755))
	h, boot, err := hostBoot()
	require.NoError(t, err)
	c.Host = h
	c.Units = []ManagedUnit{{filepath.Base(parent), parent}}
	name := "curio-sdr-" + c.Domain + "-" + uuid.NewString()
	pg, err := openCgroup(parent)
	require.NoError(t, err)
	defer func() { _ = pg.Close() }()
	require.NoError(t, unix.Mkdirat(int(pg.Fd()), name, 0755))
	g, err := openCgroup(parent + "/" + name)
	require.NoError(t, err)
	defer func() { _ = g.Close() }()
	dev, ino, err := identity(g)
	require.NoError(t, err)
	r := ManagedRun{c.Domain, h, boot, parent + "/" + name, dev, ino}
	b := &linuxBoundary{c: c}
	p := filepath.Join(base, "s-t01000-42.sdr.tmp", Prefix+uuid.NewString())
	encoded, err := json.Marshal(r)
	require.NoError(t, err)
	cmd := exec.Command(os.Args[0], "-test.run=^TestManagedKernelHelper$", "-test.timeout=15s")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "CURIO_DISCARD_KERNEL_HELPER=" + p, "CURIO_DISCARD_KERNEL_RUN=" + string(encoded)}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(g.Fd())}
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
	require.Equal(t, "child-ready", strings.TrimSpace(line))
	require.NoError(t, cmd.Process.Signal(syscall.SIGSTOP))
	stopped, err := b.Stopped(base, r)
	require.NoError(t, err)
	require.False(t, stopped)
	require.NoError(t, cmd.Process.Kill())
	require.Error(t, cmd.Wait())
	stopped, err = b.Stopped(base, r)
	require.NoError(t, err)
	require.False(t, stopped, "real descendant still executing after parent SIGKILL")
	result, err := sweepKernelTest(base, b)
	require.Error(t, err)
	require.Equal(t, "termination_required", result[0].Status)
	require.FileExists(t, filepath.Join(p, "layer-child"))
	require.Eventually(t, func() bool { ended, e := b.Stopped(base, r); return e == nil && ended }, 12*time.Second, 20*time.Millisecond)
	result, err = sweepKernelTest(base, b)
	require.NoError(t, err)
	require.Equal(t, 2, result[0].FilesRemoved)
	// Different boot only accepted for the same host/enrolled run.
	r.Boot = uuid.NewString()
	stopped, err = b.Stopped(base, r)
	require.NoError(t, err)
	require.True(t, stopped)
	r.Host = "wrong"
	_, err = b.Stopped(base, r)
	require.Error(t, err)
}

// Kernel lifetime test uses the actual scanner, but excludes accounting state
// directory provisioning. The separate operator launcher test covers that.
func sweepKernelTest(base string, b *linuxBoundary) ([]Result, error) {
	f, err := openDir(base)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if err = lock(f); err != nil {
		return nil, err
	}
	return sweepLockedWithBoundary(f, base, true, kernelScanner{b})
}

type kernelScanner struct{ b *linuxBoundary }

func (b kernelScanner) Current(s string) (*ManagedRun, error)        { return b.b.Current(s) }
func (b kernelScanner) Stopped(s string, r ManagedRun) (bool, error) { return b.b.Stopped(s, r) }

func TestManagedLinuxOpenInodeAccounting(t *testing.T) {
	if os.Getenv("CURIO_SDR_DISCARD_CGROUP_ITEST") != "1" {
		t.Skip("explicit disposable Linux opt-in missing")
	}
	require.Equal(t, "disposable", os.Getenv("CURIO_SDR_DISCARD_TEST_TARGET"))
	stateParent := os.Getenv("CURIO_SDR_DISCARD_TEST_STATE")
	require.True(t, strings.HasPrefix(filepath.Base(stateParent), "curio-sdr-discard-test-"))
	d, err := trustedDir(stateParent)
	require.NoError(t, err)
	_ = d.Close()
	state, err := os.MkdirTemp(stateParent, "accounting-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(state)) }) // this test's exact owned directory
	c, targets := legacyFixture(t)
	c.StateDir = state
	base := filepath.Join(targets[0].Root, "cache")
	p := filepath.Join(targets[0].Root, targets[0].Relative, "layer")
	f, err := os.Open(p)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	dev, ino, err := identity(f)
	require.NoError(t, err)
	dir, err := openDir(filepath.Dir(p))
	require.NoError(t, err)
	_, directoryInode, err := identity(dir)
	require.NoError(t, err)
	require.NoError(t, dir.Close())
	_, err = c.spaceStart(base, dev, directoryInode, filepath.Base(filepath.Dir(p)), []openIdentity{{dev, ino}})
	require.NoError(t, err)
	require.NoError(t, os.Remove(p))
	require.ErrorContains(t, c.checkSpace(base), "open descriptor")
	require.NoError(t, f.Close())
	require.NoError(t, c.checkSpace(base))
}
