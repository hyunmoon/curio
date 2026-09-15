package sdrscratch

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Real files/statfs and production accounting algorithm; only root trust and
// Linux /proc evidence are substituted for unprivileged Darwin execution.
func reviewAccess() spaceAccess {
	return spaceAccess{openDir, func(p string, v any) error {
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		return decodeStrict(b, v)
	}, func([]openIdentity) (bool, error) { return false, nil }, freeBytes}
}
func TestAccountingResidualOpenAndReplacement(t *testing.T) {
	for _, mode := range []string{"residual", "open", "unknown-open", "replacement", "wrong-base", "missing"} {
		t.Run(mode, func(t *testing.T) {
			c, ts := legacyFixture(t)
			c.StateDir = t.TempDir()
			base := filepath.Join(ts[0].Root, "cache")
			target := filepath.Join(ts[0].Root, ts[0].Relative)
			d, e := openDir(target)
			require.NoError(t, e)
			dev, ino, e := identity(d)
			require.NoError(t, e)
			require.NoError(t, d.Close())
			w := spaceWitness{Base: base, Device: dev, Target: filepath.Base(target)}
			access := reviewAccess()
			switch mode {
			case "open":
				access.open = func([]openIdentity) (bool, error) { return true, nil }
			case "unknown-open":
				access.open = func([]openIdentity) (bool, error) { return false, fmt.Errorf("proc unavailable") }
			case "replacement":
				require.NoError(t, os.Rename(target, target+"-keep"))
				require.NoError(t, os.Mkdir(target, 0755))
			case "missing":
				require.NoError(t, os.Rename(target, target+"-keep"))
			case "wrong-base":
				w.Base = base + "-wrong"
			}
			p := reviewWitness(t, c, base, ino, w)
			require.Error(t, c.checkSpaceWith(base, access))
			require.FileExists(t, p)
			require.FileExists(t, filepath.Join(ts[0].Root, "cache/s-t03199233-160988/keep"))
		})
	}
}

func TestAccountingAtomicPublicationAndPartialRetry(t *testing.T) {
	c, ts := legacyFixture(t)
	c.StateDir = t.TempDir()
	base := filepath.Join(ts[0].Root, "cache")
	d, e := openDir(c.StateDir)
	require.NoError(t, e)
	defer func() { _ = d.Close() }()
	access := reviewAccess()
	target := filepath.Base(ts[0].Relative)
	first := []openIdentity{{c.Storage[0].Device, 1}, {c.Storage[0].Device, 2}}
	name, e := publishSpace(d, c.StateDir, base, c.Storage[0].Device, 3, target, first, access.read)
	require.NoError(t, e)
	_, e = publishSpace(d, c.StateDir, base, c.Storage[0].Device, 3, target, first[1:], access.read)
	require.NoError(t, e)
	var w spaceWitness
	require.NoError(t, access.read(filepath.Join(c.StateDir, name), &w))
	require.Equal(t, first, w.Files)
	_, e = publishSpace(d, c.StateDir, base, c.Storage[0].Device, 3, target, []openIdentity{{1, 99}}, access.read)
	require.ErrorContains(t, e, "identity changed")
	require.NoError(t, os.WriteFile(filepath.Join(c.StateDir, name), []byte("{"), 0600))
	_, e = publishSpace(d, c.StateDir, base, c.Storage[0].Device, 3, target, first, access.read)
	require.Error(t, e)
	// A pre-publication crash leaves diagnostic metadata only, never an
	// actionable witness. No scratch deletion precedes complete publication.
	require.NoError(t, os.WriteFile(filepath.Join(c.StateDir, ".space-pending-crash-fixture"), []byte("{"), 0600))
	healthy := filepath.Join(ts[0].Root, "key")
	require.NoError(t, os.Mkdir(healthy, 0755))
	require.NoError(t, c.checkSpaceWith(healthy, access))
	es, e := os.ReadDir(c.StateDir)
	require.NoError(t, e)
	for _, v := range es {
		require.False(t, strings.HasPrefix(v.Name(), ".space-pending-") && v.Name() != ".space-pending-crash-fixture", "handled publication errors must clean their own temporary metadata")
	}
}

func TestAccountingConcurrentScanners(t *testing.T) {
	c, ts := legacyFixture(t)
	c.StateDir = t.TempDir()
	base := filepath.Join(ts[0].Root, "cache")
	target := filepath.Join(ts[0].Root, ts[0].Relative)
	f, e := openDir(target)
	require.NoError(t, e)
	dev, ino, e := identity(f)
	require.NoError(t, e)
	require.NoError(t, f.Close())
	require.NoError(t, c.checkStorage(base))
	require.NoError(t, os.Remove(filepath.Join(target, "layer")))
	p := reviewWitness(t, c, base, ino, spaceWitness{Base: base, Device: dev, Target: filepath.Base(target)})
	access := reviewAccess()
	read := access.read
	var ready sync.WaitGroup
	ready.Add(2)
	access.read = func(p string, v any) error { e := read(p, v); ready.Done(); ready.Wait(); return e }
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errs <- c.checkSpaceWith(base, access) }()
	}
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.NoFileExists(t, p)
}

func TestStorageRootBaseMountEdge(t *testing.T) {
	c, ts := legacyFixture(t)
	base := filepath.Join(ts[0].Root, "cache")
	calls := 0
	f, e := c.pinStorageBaseWith(base, func(root, child *os.File) error {
		calls++
		require.Equal(t, filepath.Base(ts[0].Root), root.Name())
		require.Equal(t, "cache", child.Name())
		return fmt.Errorf("injected cross-mount")
	})
	if f != nil {
		_ = f.Close()
	}
	require.ErrorContains(t, e, "cross-mount")
	require.Equal(t, 1, calls)
	require.FileExists(t, filepath.Join(ts[0].Root, ts[0].Relative, "layer"))
	f, e = c.pinStorageBase(base)
	require.NoError(t, e)
	require.NoError(t, f.Close())
}
func reviewWitness(t *testing.T, c *ManagedConfig, base string, ino uint64, w spaceWitness) string {
	t.Helper()
	p := filepath.Join(c.StateDir, fmt.Sprintf("space-%x-%d-%d.json", sha256.Sum256([]byte(base)), w.Device, ino))
	b, e := json.Marshal(w)
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(p, b, 0600))
	return p
}
func TestAccountingCorruptOtherStorage(t *testing.T) {
	c, ts := legacyFixture(t)
	c.StateDir = t.TempDir()
	a := filepath.Join(ts[0].Root, "cache")
	b := filepath.Join(ts[0].Root, "key")
	require.NoError(t, os.Mkdir(b, 0755))
	p := reviewWitness(t, c, a, 1, spaceWitness{Base: a, Device: c.Storage[0].Device})
	require.NoError(t, os.WriteFile(p, []byte(`{"Base":`), 0600))
	require.NoError(t, c.checkSpaceWith(b, reviewAccess()), "healthy B must not parse A's corrupt witness")
	require.Error(t, c.checkSpaceWith(a, reviewAccess()), "own corruption remains protected")
	require.FileExists(t, p)
}
func TestAccountingConcurrentWriter(t *testing.T) {
	c, ts := legacyFixture(t)
	c.StateDir = t.TempDir()
	base := filepath.Join(ts[0].Root, "cache")
	target := filepath.Join(ts[0].Root, ts[0].Relative)
	d, e := openDir(target)
	require.NoError(t, e)
	defer func() { _ = d.Close() }()
	dev, ino, e := identity(d)
	require.NoError(t, e)
	// Existing version-1 witness is deliberately supported through its inode.
	reviewWitness(t, c, base, ino, spaceWitness{Base: base, Device: dev, MinimumFree: 110})
	require.NoError(t, os.Remove(filepath.Join(target, "layer")))
	written := make(chan struct{})
	go func() {
		defer close(written)
		e := os.WriteFile(filepath.Join(base, "unrelated-normal-output"), make([]byte, 8192), 0600)
		if e != nil {
			panic(e)
		}
	}()
	<-written
	access := reviewAccess()
	access.free = func(*os.File) (uint64, error) { return 109, nil }
	require.NoError(t, c.checkSpaceWith(base, access), "unlink complete, no open reference; unrelated write must not impose a historical 110-byte floor")
	require.FileExists(t, filepath.Join(base, "unrelated-normal-output"))
}
