package sdrscratch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type testBoundary struct {
	ended atomic.Bool
	fail  atomic.Bool
}

type failedDisplay struct{}

func (failedDisplay) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestLegacyDisplayFailureNoRemoval(t *testing.T) {
	c, ts := legacyFixture(t)
	p, err := BuildLegacyPlan(c, ts, c.Host, "boot")
	require.NoError(t, err)
	calls := 0
	_, err = executeLegacy(c, p, filepath.Join(t.TempDir(), "journal"), strings.NewReader("DISCARD LEGACY 2\n"), failedDisplay{}, c.Host, "boot", func() error { return nil }, func(int, string, int) error { calls++; return nil }, false)
	require.ErrorIs(t, err, io.ErrClosedPipe)
	require.Zero(t, calls)
}

func (b *testBoundary) Current(string) (*ManagedRun, error) {
	return &ManagedRun{Domain: "test-domain", Host: "test-host", Boot: "test-boot", Cgroup: "test-subtree", Device: 1, Inode: 2}, nil
}
func (b *testBoundary) Stopped(string, ManagedRun) (bool, error) {
	if b.fail.Load() {
		return false, errors.New("identity changed")
	}
	return b.ended.Load(), nil
}

func managedFixture(t *testing.T) (string, string, *Writer, *testBoundary) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	p := filepath.Join(base, "s-t01000-42.sdr.tmp", Prefix+uuid.NewString())
	b := new(testBoundary)
	w, err := BeginWithOptions(p, Options{Boundary: b})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	require.NoError(t, os.WriteFile(filepath.Join(p, "layer-1"), make([]byte, 8192), 0600))
	return base, p, w, b
}

func TestManagedDiscardTerminationNotFreeLock(t *testing.T) {
	base, p, w, b := managedFixture(t)
	r, err := SweepWithBoundary(base, true, b)
	require.NoError(t, err)
	require.Equal(t, "live", r[0].Status)
	require.NoError(t, w.Close()) // parent FD closes; native child may still live
	r, err = SweepWithBoundary(base, true, b)
	require.ErrorContains(t, err, "subtree")
	require.Equal(t, "termination_required", r[0].Status)
	require.FileExists(t, filepath.Join(p, "layer-1"))
	b.fail.Store(true)
	_, err = SweepWithBoundary(base, true, b)
	require.ErrorContains(t, err, "identity changed")
	b.fail.Store(false)
	b.ended.Store(true)
	_, err = SweepWithBoundary(base, false, b)
	require.ErrorContains(t, err, "pending")
	r, err = SweepWithBoundary(base, true, b)
	require.NoError(t, err)
	require.Equal(t, "reclaimed", r[0].Status)
	require.Equal(t, 1, r[0].FilesRemoved)
	require.Positive(t, r[0].AllocatedBytes)
	r, err = SweepWithBoundary(base, true, b)
	require.NoError(t, err)
	require.Equal(t, "already_reclaimed", r[0].Status)
}

func TestManagedReceiptPublicationAndReplacement(t *testing.T) {
	for _, action := range []string{"unpublished", "published", "replacement"} {
		t.Run(action, func(t *testing.T) {
			base, p, w, b := managedFixture(t)
			require.NoError(t, unix.Fsetxattr(int(w.dir.Fd()), completionAttribute, []byte(`{"complete":true}`), 0))
			if action != "unpublished" {
				require.NoError(t, os.Rename(p, filepath.Join(base, "s-t01000-42")))
				if action == "replacement" {
					require.NoError(t, os.Mkdir(p, 0755))
					require.NoError(t, os.WriteFile(filepath.Join(p, "new-owner"), []byte("keep"), 0600))
				}
				require.Error(t, w.Returned())
				_, err := w.DiscardOwn()
				require.Error(t, err)
			}
			require.NoError(t, w.Close())
			b.ended.Store(true)
			_, err := SweepWithBoundary(base, true, b)
			if action == "replacement" {
				require.Error(t, err)
				require.FileExists(t, filepath.Join(p, "new-owner"))
			} else {
				require.NoError(t, err)
			}
			if action == "unpublished" {
				require.NoFileExists(t, filepath.Join(p, "layer-1"))
			} else {
				require.FileExists(t, filepath.Join(base, "s-t01000-42", "layer-1"))
			}
		})
	}
}

func TestManagedPartialFailureAndHealthyPath(t *testing.T) {
	base, p, w, b := managedFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(p, "layer-2"), make([]byte, 8192), 0600))
	require.NoError(t, w.Returned())
	n, err := w.reclaimFilesMode(func(fd int, name string, flags int) error {
		if name == "layer-2" {
			return unix.EACCES
		}
		return unix.Unlinkat(fd, name, flags)
	}, true)
	require.Equal(t, 1, n)
	require.ErrorIs(t, err, unix.EACCES)
	require.NoError(t, w.Close())
	_, err = SweepWithBoundary(base, false, b)
	require.Error(t, err)
	healthy, _, live, hb := managedFixture(t)
	_, err = SweepWithBoundary(healthy, false, hb)
	require.NoError(t, err)
	require.NoError(t, live.Close())
	_, err = SweepWithBoundary(base, true, b)
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(p, "layer-2"))
}

func TestManagedFourSlotsRepeatedCrashRecovery(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	b := new(testBoundary)
	for cycle := 0; cycle < 3; cycle++ {
		b.ended.Store(false)
		var writers []*Writer
		for slot := 0; slot < 4; slot++ {
			p := filepath.Join(base, fmt.Sprintf("s-t01000-%d.sdr.tmp", slot), Prefix+uuid.NewString())
			w, e := BeginWithOptions(p, Options{Boundary: b})
			require.NoError(t, e)
			writers = append(writers, w)
			require.NoError(t, os.WriteFile(filepath.Join(p, "layer"), make([]byte, 8192), 0600))
		}
		for _, w := range writers {
			require.NoError(t, w.Close())
		}
		_, err = SweepWithBoundary(base, false, b)
		require.Error(t, err)
		b.ended.Store(true)
		r, e := SweepWithBoundary(base, true, b)
		require.NoError(t, e)
		n := 0
		for _, v := range r {
			n += v.FilesRemoved
		}
		require.Equal(t, 4, n)
	}
}

func TestManagedUnknownEmptyIsNotTermination(t *testing.T) {
	base, p, w, b := managedFixture(t)
	require.NoError(t, os.Remove(filepath.Join(p, "layer-1")))
	require.NoError(t, w.Close())
	r, err := SweepWithBoundary(base, true, b)
	require.Error(t, err)
	require.Equal(t, "termination_required", r[0].Status)
}

func TestLegacyMaintenanceTombstoneUnblocksOnlyItsPath(t *testing.T) {
	c, ts := legacyFixture(t)
	base := filepath.Join(ts[0].Root, "cache")
	b := new(testBoundary)
	_, err := SweepWithBoundary(base, false, b)
	require.ErrorContains(t, err, "legacy_requires_maintenance")
	p, err := BuildLegacyPlan(c, ts, c.Host, "boot")
	require.NoError(t, err)
	_, err = executeLegacy(c, p, filepath.Join(t.TempDir(), "journal"), strings.NewReader("DISCARD LEGACY 2\n"), &bytes.Buffer{}, c.Host, "boot", func() error { return nil }, unix.Unlinkat, false)
	require.NoError(t, err)
	_, err = SweepWithBoundary(base, false, b)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(ts[0].Root, ts[0].Relative, "reused"), []byte("unexpected"), 0600))
	_, err = SweepWithBoundary(base, false, b)
	require.Error(t, err)
}

func legacyFixture(t *testing.T) (*ManagedConfig, []LegacyTarget) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(root, "cache"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sectorstore.json"), []byte(`{"ID":"test-storage"}`), 0600))
	f, err := openDir(root)
	require.NoError(t, err)
	dev, ino, err := identity(f)
	require.NoError(t, err)
	_ = f.Close()
	c := &ManagedConfig{Version: 1, Domain: uuid.NewString(), Host: strings.Repeat("a", 32), Storage: []ManagedStorage{{Root: root, ID: "test-storage", Device: dev, Inode: ino}}}
	var ts []LegacyTarget
	for _, n := range []int{158667, 158918} {
		rel := fmt.Sprintf("cache/s-t03199233-%d.tmp", n)
		require.NoError(t, os.Mkdir(filepath.Join(root, rel), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(root, rel, "layer"), make([]byte, 8192), 0600))
		ts = append(ts, LegacyTarget{Root: root, Relative: rel})
	}
	require.NoError(t, os.Mkdir(filepath.Join(root, "cache/s-t03199233-160988"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cache/s-t03199233-160988/keep"), []byte("canonical"), 0600))
	return c, ts
}

func TestLegacyExplicitPlanGuardsAndConfirmation(t *testing.T) {
	for _, input := range []string{"", "yes\n", "DISCARD LEGACY 1\n", "DISCARD LEGACY 2"} {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			c, ts := legacyFixture(t)
			p, err := BuildLegacyPlan(c, ts, c.Host, "boot")
			require.NoError(t, err)
			var out bytes.Buffer
			calls := 0
			_, err = executeLegacy(c, p, filepath.Join(t.TempDir(), "journal"), strings.NewReader(input), &out, c.Host, "boot", func() error { return nil }, func(int, string, int) error { calls++; return nil }, false)
			require.ErrorContains(t, err, "cancelled")
			require.Zero(t, calls)
			for _, v := range ts {
				require.Less(t, strings.Index(out.String(), v.Relative), strings.Index(out.String(), "Type DISCARD"))
			}
		})
	}
	c, ts := legacyFixture(t)
	p, err := BuildLegacyPlan(c, ts, c.Host, "boot")
	require.NoError(t, err)
	_, err = executeLegacy(c, p, "unused", strings.NewReader("DISCARD LEGACY 2\n"), &bytes.Buffer{}, c.Host, "boot", func() error { return fmt.Errorf("accessor live") }, unix.Unlinkat, false)
	require.ErrorContains(t, err, "accessor live")
	ts[0].Relative = "cache/s-t03199233-160988"
	_, err = BuildLegacyPlan(c, ts, c.Host, "boot")
	require.Error(t, err)
}

func TestLegacyExactDeletionPartialFailureReplan(t *testing.T) {
	c, ts := legacyFixture(t)
	p, err := BuildLegacyPlan(c, ts, c.Host, "boot")
	require.NoError(t, err)
	journal := filepath.Join(t.TempDir(), "journal")
	n := 0
	r, err := executeLegacy(c, p, journal, strings.NewReader("DISCARD LEGACY 2\n"), &bytes.Buffer{}, c.Host, "boot", func() error { return nil }, func(fd int, name string, flags int) error {
		n++
		if n == 2 {
			return unix.EIO
		}
		return unix.Unlinkat(fd, name, flags)
	}, false)
	require.ErrorIs(t, err, unix.EIO)
	require.Equal(t, "private_files_unlinked", r[0].Status)
	require.Equal(t, "partial_or_space_unconfirmed", r[1].Status)
	require.FileExists(t, filepath.Join(ts[0].Root, "cache/s-t03199233-160988/keep"))
	require.NoFileExists(t, filepath.Join(ts[0].Root, ts[0].Relative, "layer"))
	_, err = executeLegacy(c, p, journal, strings.NewReader("DISCARD LEGACY 2\n"), &bytes.Buffer{}, c.Host, "boot", func() error { return nil }, unix.Unlinkat, false)
	require.ErrorContains(t, err, "changed")
	p, err = BuildLegacyPlan(c, ts[1:], c.Host, "boot")
	require.NoError(t, err)
	r, err = executeLegacy(c, p, filepath.Join(t.TempDir(), "journal2"), strings.NewReader("DISCARD LEGACY 1\n"), &bytes.Buffer{}, c.Host, "boot", func() error { return nil }, unix.Unlinkat, false)
	require.NoError(t, err)
	require.Equal(t, 1, r[0].FilesRemoved)
}

func TestLegacyReplacementLinksAndSecondGuard(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "replace", "reentry"} {
		t.Run(mode, func(t *testing.T) {
			c, ts := legacyFixture(t)
			p, err := BuildLegacyPlan(c, ts, c.Host, "boot")
			require.NoError(t, err)
			path := filepath.Join(ts[0].Root, ts[0].Relative)
			switch mode {
			case "symlink":
				require.NoError(t, os.Symlink("layer", filepath.Join(path, "link")))
			case "hardlink":
				require.NoError(t, os.Link(filepath.Join(path, "layer"), filepath.Join(path, "link")))
			case "replace":
				require.NoError(t, os.Rename(path, path+"-preserved"))
				require.NoError(t, os.Mkdir(path, 0755))
			}
			checks := 0
			_, err = executeLegacy(c, p, filepath.Join(t.TempDir(), "journal"), strings.NewReader("DISCARD LEGACY 2\n"), &bytes.Buffer{}, c.Host, "boot", func() error {
				checks++
				if mode == "reentry" && checks > 1 {
					return fmt.Errorf("accessor restarted")
				}
				return nil
			}, unix.Unlinkat, false)
			require.Error(t, err)
			require.FileExists(t, filepath.Join(ts[1].Root, ts[1].Relative, "layer"))
		})
	}
}
