//go:build sdr_auto_itest

package sdrscratch

// This adapter is not compiled into Curio. The documented Go overlay replaces
// only host/cgroup/root-ownership evidence, never SQL, unlink, locks or FFI's
// receipt checks. Native bodies are separately replaced by the caller fixture.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

var AutoFixtureActive = false

type autoFixturePause struct {
	root             string
	entered, release chan struct{}
}

var autoFixturePauseNext atomic.Pointer[autoFixturePause]

// Probe the same exclusive lock used by automatic cleanup without creating
// conflicting files that independently prohibit a new SDR attempt.
func AutoFixtureExclusiveAccess(root, sector string) error {
	c, err := personalSessionPtr.Load().load(root)
	if err != nil {
		return err
	}
	f, err := sectorGate(c, sector, true, openDir)
	if err != nil {
		return err
	}
	return f.Close()
}

// Hold an actual cleanup after its exclusive sector gate and live DB state
// have been acquired. Tests release it explicitly, not by hoping a timer races.
func AutoFixtureHoldNextCleanup(t *testing.T, root string) (<-chan struct{}, func()) {
	p := &autoFixturePause{root: root, entered: make(chan struct{}), release: make(chan struct{})}
	old := autoFixturePauseNext.Swap(p)
	var once sync.Once
	release := func() { once.Do(func() { close(p.release) }) }
	t.Cleanup(func() { release(); autoFixturePauseNext.Store(old) })
	return p.entered, release
}

func autoFixtureParticipants(c *ManagedConfig) error {
	if p := autoFixturePauseNext.Load(); p != nil && c.Storage[0].Root == p.root && autoFixturePauseNext.CompareAndSwap(p, nil) {
		close(p.entered)
		<-p.release
	}
	return nil
}

type autoFixtureUnlinkFailure struct {
	remaining atomic.Int32
	hit       atomic.Bool
}

var autoFixtureFailure atomic.Pointer[autoFixtureUnlinkFailure]

// Test-only one-shot filesystem fault; actual successful unlink calls remain real.
func AutoFixtureFailUnlinkAfter(t *testing.T, successes int32) *atomic.Bool {
	f := &autoFixtureUnlinkFailure{}
	f.remaining.Store(successes + 1)
	old := autoFixtureFailure.Swap(f)
	t.Cleanup(func() { autoFixtureFailure.Store(old) })
	return &f.hit
}

func autoFixtureUnlink(fd int, name string, flags int) error {
	if f := autoFixtureFailure.Load(); f != nil && f.remaining.Add(-1) == 0 {
		f.hit.Store(true)
		return unix.EIO
	}
	return unix.Unlinkat(fd, name, flags)
}

// Referenced by the Go overlay, not normal builds. Keeping a reference here
// lets static analysis check the adapter without enabling it at runtime.
var _ = autoFixtureRuntimeIO

func AutoFixtureSetup(t *testing.T, roots []string) {
	t.Helper()
	require.True(t, AutoFixtureActive, "fixture requires the explicit OS-evidence overlay")
	t.Setenv(PersonalCleanupEnv, "1")
	t.Setenv(PersonalPolicyEnv, "")
	t.Setenv("CURIO_SDR_DOMAIN_LEASE_FD", "")
	s := &personalSession{state: t.TempDir(), host: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", domain: uuid.NewString(), unit: ManagedUnit{"fixture.service", "/system.slice/fixture.service"}, roots: map[string]string{}}
	s.access = registrationAccess{dir: openDir, read: func(p string, v any) error {
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		return decodeStrict(b, v)
	}, fs: func(*os.File) error { return nil }}
	old := personalSessionPtr.Swap(s)
	t.Cleanup(func() { personalSessionPtr.Store(old) })
	for _, r := range roots {
		var meta struct{ ID string }
		b, e := os.ReadFile(filepath.Join(r, "sectorstore.json"))
		require.NoError(t, e)
		require.NoError(t, json.Unmarshal(b, &meta))
		require.NoError(t, RegisterPersonalStorage(r, meta.ID))
	}
}

type autoFixtureBoundary struct{ c *ManagedConfig }

func (b autoFixtureBoundary) Current(string) (*ManagedRun, error) {
	return &ManagedRun{Domain: b.c.Domain, Host: b.c.Host, Boot: "fixture", Cgroup: "/fixture", Device: 1, Inode: 2}, nil
}
func (b autoFixtureBoundary) Stopped(string, ManagedRun) (bool, error) { return false, nil }
func (b autoFixtureBoundary) pinBase(base string) (*os.File, error)    { return b.c.pinStorageBase(base) }
func (b autoFixtureBoundary) checkBase(base string) error {
	f, e := b.c.pinStorageBase(base)
	if f != nil {
		_ = f.Close()
	}
	return e
}
func autoFixtureRead(p string, v any) error {
	b, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	return decodeStrict(b, v)
}
func autoFixtureRuntimeIO(c *ManagedConfig) autoIO {
	a := spaceAccess{openDir, autoFixtureRead, func([]openIdentity) (bool, error) { return false, nil }, freeBytes}
	return autoIO{autoFixtureParticipants, openDir,
		func(base string, dev, ino uint64, target string, files []openIdentity) (string, error) {
			d, e := openDir(c.StateDir)
			if e != nil {
				return "", e
			}
			defer func() { _ = d.Close() }()
			return publishSpace(d, c.StateDir, base, dev, ino, target, files, autoFixtureRead)
		},
		func(base string) error { return c.checkSpaceWith(base, a) }, autoFixtureUnlink,
		func(c *ManagedConfig) (Boundary, error) { return autoFixtureBoundary{c}, nil }}
}
