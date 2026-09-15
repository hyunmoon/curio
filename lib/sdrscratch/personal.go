package sdrscratch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const PersonalCleanupEnv = "CURIO_PERSONAL_SDR_CLEANUP"
const personalStateDir = "/var/lib/curio/sdr-cleanup"

// This is process-local wiring, not a discovery claim about other accessors.
// Only Local.openPath/PrepareSDRScratch enroll registered CanSeal roots.
type personalSession struct {
	state, host, domain string
	unit                ManagedUnit
	lease               *os.File
	mu                  sync.RWMutex
	roots               map[string]string
	access              registrationAccess
}

type registrationAccess struct {
	dir  func(string) (*os.File, error)
	read func(string, any) error
	fs   func(*os.File) error
}

var personalSessionPtr atomic.Pointer[personalSession]
var personalStartMu sync.Mutex

func PersonalCleanupEnabled() (bool, error) {
	switch os.Getenv(PersonalCleanupEnv) {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		if os.Getenv(PersonalPolicyEnv) != "" || os.Getenv("CURIO_SDR_DOMAIN_LEASE_FD") != "" {
			return false, fmt.Errorf("%s conflicts with manual managed mode", PersonalCleanupEnv)
		}
		return true, nil
	default:
		return false, fmt.Errorf("%s must be unset, 0, false, 1 or true", PersonalCleanupEnv)
	}
}

// StartPersonal is called once, before worker/DB/API/native initialization.
// The current process enters its own cgroup; no helper or re-exec is used.
func StartPersonal(registered []string) error {
	personalStartMu.Lock()
	defer personalStartMu.Unlock()
	on, err := PersonalCleanupEnabled()
	if err != nil || !on {
		return err
	}
	if personalSessionPtr.Load() != nil {
		return fmt.Errorf("personal SDR lifetime already initialized")
	}
	s, err := startPersonal(registered)
	if err != nil {
		return err
	}
	personalSessionPtr.Store(s)
	return nil
}

// RegisterPersonalStorage does not enumerate disks or infer CanSeal from a
// pathname. The caller supplies the actual local registration and metadata ID.
// Failed registration is remembered as a blocked root, never legacy fallback.
func RegisterPersonalStorage(root, id string) error {
	on, err := PersonalCleanupEnabled()
	if err != nil || !on {
		return err
	}
	s := personalSessionPtr.Load()
	if s == nil {
		return fmt.Errorf("personal SDR cleanup requires the Curio run lifetime boundary")
	}
	root = filepath.Clean(root)
	s.mu.Lock()
	s.roots[root] = id
	s.mu.Unlock()
	_, err = s.register(root, id)
	return err
}

func stateOutside(state string, roots []string) error {
	for _, root := range roots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return fmt.Errorf("absolute clean registered root required")
		}
		rel, err := filepath.Rel(root, state)
		if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return fmt.Errorf("persistent state must be outside every registered storage root")
		}
	}
	return nil
}

func (s *personalSession) configPath(root string) (string, ManagedStorage, error) {
	f, err := openDir(root)
	if err != nil {
		return "", ManagedStorage{}, err
	}
	defer func() { _ = f.Close() }()
	dev, ino, err := identity(f)
	if err != nil {
		return "", ManagedStorage{}, err
	}
	return filepath.Join(s.state, fmt.Sprintf("auto-storage-%d-%d.json", dev, ino)), ManagedStorage{Root: root, Device: dev, Inode: ino}, nil
}

func (s *personalSession) register(root, id string) (*ManagedConfig, error) {
	if id == "" {
		return nil, fmt.Errorf("registered storage ID required")
	}
	if err := stateOutside(s.state, []string{root}); err != nil {
		return nil, err
	}
	name, storage, err := s.configPath(root)
	if err != nil {
		return nil, err
	}
	storage.ID = id
	f, err := openDir(root)
	if err != nil {
		return nil, err
	}
	err = s.access.fs(f)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	d, err := s.access.dir(s.state)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	// One short registry publication critical section, never held for native IO.
	if err = lock(d); err != nil {
		return nil, fmt.Errorf("registry busy; retry this root: %w", err)
	}
	c := new(ManagedConfig)
	err = s.access.read(name, c)
	if errors.Is(err, os.ErrNotExist) {
		c = &ManagedConfig{Version: 1, Domain: s.domain, Host: s.host, StateDir: s.state, Storage: []ManagedStorage{storage}, Automatic: true}
	} else if err != nil {
		return nil, err
	}
	if c.Domain != s.domain || c.Host != s.host || c.StateDir != s.state || !c.Automatic || len(c.Storage) != 1 || c.Storage[0] != storage {
		return nil, fmt.Errorf("existing storage identity/alias/domain mismatch; no registry replacement")
	}
	found := false
	for _, u := range c.Units {
		if u.Name == s.unit.Name {
			if u != s.unit {
				return nil, fmt.Errorf("accessor cgroup changed; explicit review required")
			}
			found = true
		}
	}
	if found {
		return c, c.validate()
	}
	c.Units = append(c.Units, s.unit)
	sort.Slice(c.Units, func(i, j int) bool { return c.Units[i].Name < c.Units[j].Name })
	if err = c.validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	// Existing valid metadata is extended under the shared directory lock, not
	// recreated. Pending files never authorize reclamation after a crash.
	temp := ".registry-pending-" + uuid.NewString()
	fd, err := unix.Openat(int(d.Fd()), temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Unlinkat(int(d.Fd()), temp, 0) }()
	out := os.NewFile(uintptr(fd), temp)
	_, err = out.Write(raw)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return nil, err
	}
	if err = unix.Renameat(int(d.Fd()), temp, int(d.Fd()), filepath.Base(name)); err != nil {
		return nil, err
	}
	return c, d.Sync()
}

func (s *personalSession) load(root string) (*ManagedConfig, error) {
	s.mu.RLock()
	id, ok := s.roots[root]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("not a registered local sealing root")
	}
	f, err := openDir(root)
	if err != nil {
		return nil, err
	}
	err = s.access.fs(f)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	name, storage, err := s.configPath(root)
	if err != nil {
		return nil, err
	}
	storage.ID = id
	var c ManagedConfig
	if err = s.access.read(name, &c); err != nil {
		return nil, err
	}
	if c.Domain != s.domain || c.Host != s.host || c.StateDir != s.state || !c.Automatic || len(c.Storage) != 1 || c.Storage[0] != storage {
		return nil, fmt.Errorf("registered root identity changed")
	}
	return &c, c.validate()
}

func personalBoundary(base string) (Boundary, error) {
	s := personalSessionPtr.Load()
	if s == nil {
		return nil, fmt.Errorf("personal SDR cleanup has no established process lifetime")
	}
	c, err := s.load(filepath.Dir(base))
	if err != nil {
		return nil, err
	}
	return personalPlatformBoundary(c, s.lease)
}
