package sdrscratch

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// PersonalPolicyEnv is a config FILE, not a capacity profile or deletion flag.
// Empty preserves the certificate-only behavior. The launcher and all local
// storage accessors must participate before this personal mode is enabled.
const PersonalPolicyEnv = "CURIO_SDR_DISCARD_INTERRUPTED"

type ManagedRun struct {
	Domain, Host, Boot, Cgroup string
	Device, Inode              uint64
}

// Boundary implementations must establish termination of the entire native
// process subtree, not just the Go parent. The injected boundary is for tests;
// production uses the Linux cgroup-v2 implementation and enrolled config.
type Boundary interface {
	Current(base string) (*ManagedRun, error)
	Stopped(base string, run ManagedRun) (bool, error)
}

type Options struct {
	RecordIO RecordIO
	Boundary Boundary
}

type ManagedStorage struct {
	Root, ID      string
	Device, Inode uint64
}

type ManagedUnit struct{ Name, Cgroup string }

type ManagedConfig struct {
	Version      int
	Domain, Host string
	StateDir     string
	Storage      []ManagedStorage
	Units        []ManagedUnit
	// Automatic membership is observation, not a complete maintenance inventory.
	Automatic bool `json:",omitempty"`
}

func decodeStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func readPrivateJSON(path string, v any) error {
	parent, err := openDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	fd, err := unix.Openat(int(parent.Fd()), filepath.Base(path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != 0 || st.Mode&0022 != 0 || st.Nlink != 1 {
		return fmt.Errorf("config must be a root-owned, non-writable private regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return err
	}
	if len(b) > 65536 {
		return fmt.Errorf("config too large")
	}
	return decodeStrict(b, v)
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9_.@-]+\.service$`)
var errRegisteredBaseMissing = errors.New("registered cache/key directory absent")

func LoadManagedConfig(path string) (*ManagedConfig, error) {
	var c ManagedConfig
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("absolute clean config path required")
	}
	d, err := trustedDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	_ = d.Close()
	if err := readPrivateJSON(path, &c); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func HostBoot() (string, string, error) { return platformHostBoot() }

func (c *ManagedConfig) validate() error {
	u, err := uuid.Parse(c.Domain)
	if err != nil || u.String() != c.Domain || c.Version != 1 || len(c.Host) != 32 || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(c.Host) {
		return fmt.Errorf("invalid domain/host/version")
	}
	if !filepath.IsAbs(c.StateDir) || filepath.Clean(c.StateDir) != c.StateDir || c.StateDir == "/" {
		return fmt.Errorf("invalid state directory")
	}
	if len(c.Storage) == 0 || len(c.Units) == 0 {
		return fmt.Errorf("explicit storage and all accessor units required")
	}
	seen := map[string]bool{}
	for _, s := range c.Storage {
		if !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) != s.Root || s.Root == "/" || s.Inode == 0 || s.ID == "" || seen[s.Root] || c.StateDir == s.Root || strings.HasPrefix(c.StateDir, s.Root+"/") {
			return fmt.Errorf("invalid or duplicate storage identity; state must be outside storage")
		}
		seen[s.Root] = true
	}
	seen = map[string]bool{}
	for _, u := range c.Units {
		if !unitName.MatchString(u.Name) || !strings.HasPrefix(u.Cgroup, "/") || filepath.Clean(u.Cgroup) != u.Cgroup || filepath.Base(u.Cgroup) != u.Name || seen[u.Name] {
			return fmt.Errorf("invalid/duplicate accessor unit")
		}
		seen[u.Name] = true
	}
	return nil
}

func (c *ManagedConfig) checkStorage(base string) error {
	f, err := c.pinStorageBase(base)
	if err == nil {
		_ = f.Close()
	}
	return err
}

func (c *ManagedConfig) pinStorageBase(base string) (*os.File, error) {
	return c.pinStorageBaseWith(base, sameDevice)
}

// The returned FD is the verified child of the enrolled root, not a second
// pathname lookup. Keep this FD through scanning/creation/legacy traversal.
func (c *ManagedConfig) pinStorageBaseWith(base string, edge func(*os.File, *os.File) error) (*os.File, error) {
	for _, s := range c.Storage {
		if base != filepath.Join(s.Root, "cache") && base != filepath.Join(s.Root, "key") {
			continue
		}
		root, err := openDir(s.Root)
		if err != nil {
			return nil, err
		}
		defer func() { _ = root.Close() }()
		dev, ino, err := identity(root)
		if err != nil {
			return nil, err
		}
		if dev != s.Device || ino != s.Inode {
			return nil, fmt.Errorf("storage root identity changed")
		}
		if err := supportedFS(root); err != nil {
			return nil, err
		}
		fd, err := unix.Openat(int(root.Fd()), "sectorstore.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		f := os.NewFile(uintptr(fd), "sectorstore.json")
		defer func() { _ = f.Close() }()
		var meta struct{ ID string }
		if err := json.NewDecoder(io.LimitReader(f, 65536)).Decode(&meta); err != nil {
			return nil, err
		}
		if meta.ID != s.ID {
			return nil, fmt.Errorf("storage ID changed")
		}
		child, err := openDirAt(int(root.Fd()), filepath.Base(base))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: %v", errRegisteredBaseMissing, err)
			}
			return nil, err
		}
		if err = edge(root, child); err != nil {
			_ = child.Close()
			return nil, err
		}
		return child, nil
	}
	return nil, fmt.Errorf("storage not enrolled in personal discard domain")
}

func pinBoundaryBase(boundary Boundary, base string) (*os.File, error) {
	if b, ok := boundary.(interface {
		pinBase(string) (*os.File, error)
	}); ok {
		return b.pinBase(base)
	}
	return openDir(base)
}

// Traverse each component from an already pinned base, validating mount IDs
// as well as devices. Neither a bind mount nor a mount-back can be skipped.
func pinRelative(base *os.File, relative string) (*os.File, error) {
	if relative == "." || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || strings.HasPrefix(relative, "..") {
		return nil, fmt.Errorf("invalid relative scratch path")
	}
	parent := base
	for _, name := range strings.Split(relative, string(os.PathSeparator)) {
		child, err := openDirAt(int(parent.Fd()), name)
		if err == nil {
			err = sameDevice(parent, child)
		}
		if parent != base {
			_ = parent.Close()
		}
		if err != nil {
			if child != nil {
				_ = child.Close()
			}
			return nil, err
		}
		parent = child
	}
	return parent, nil
}

func configuredBoundary(injected Boundary, base string) (Boundary, error) {
	if injected != nil {
		return injected, nil
	}
	on, err := PersonalCleanupEnabled()
	if err != nil {
		return nil, err
	}
	if on {
		return personalBoundary(base)
	}
	p := os.Getenv(PersonalPolicyEnv)
	if p == "" {
		return nil, nil
	}
	c, err := LoadManagedConfig(p)
	if err != nil {
		return nil, err
	}
	return platformBoundary(c)
}

func (w *Writer) DiscardInterrupted() bool { return w.r.Run != nil }

// DiscardOwn is only for a directly observed synchronous return under the
// personal policy. A completed receipt IN SCRATCH is not a published output.
// Returned has already revalidated the original pathname and pinned inode.
func (w *Writer) DiscardOwn() (int, error) {
	if !w.returnedHere || w.r.Run == nil {
		return 0, fmt.Errorf("no managed local return authority")
	}
	if err := w.checkPath(); err != nil {
		return 0, err
	}
	return w.reclaimFilesMode(unix.Unlinkat, true)
}

// State lives outside scratch so ENOSPC on scratch does not also consume the
// cleanup accounting journal. Its ancestry and lease cannot be user-replaced.
func trustedDir(path string) (*os.File, error) {
	f, err := openDir(path)
	if err != nil {
		return nil, err
	}
	for p := path; ; p = filepath.Dir(p) {
		d, e := openDir(p)
		if e != nil {
			_ = f.Close()
			return nil, e
		}
		var st unix.Stat_t
		e = unix.Fstat(int(d.Fd()), &st)
		_ = d.Close()
		if e != nil || st.Uid != 0 || st.Mode&0022 != 0 {
			_ = f.Close()
			return nil, fmt.Errorf("untrusted writable ancestor: %s", p)
		}
		if p == "/" {
			break
		}
	}
	return f, nil
}

func openLease(c *ManagedConfig) (*os.File, error) {
	d, err := trustedDir(c.StateDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	fd, err := unix.Openat(int(d.Fd()), "domain-"+c.Domain+".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "domain lease")
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != 0 || st.Nlink != 1 || st.Mode&0077 != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("unsafe domain lease")
	}
	return f, nil
}

func AcquireDomain(c *ManagedConfig, exclusive bool) (*os.File, error) {
	f, err := openLease(c)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	if err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("domain busy: %w", err)
	}
	return f, nil
}

type spaceWitness struct {
	Base string
	// MinimumFree is accepted only for reading old witnesses. It is not an
	// admission floor: concurrent writers make that old measurement obsolete.
	Device, MinimumFree uint64
	Files               []openIdentity
	Target              string `json:",omitempty"`
}
type openIdentity struct{ Device, Inode uint64 }

func freeBytes(f *os.File) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func spacePrefix(base string) string { return fmt.Sprintf("space-%x-", sha256.Sum256([]byte(base))) }

func (c *ManagedConfig) spaceStart(base string, dev, ino uint64, target string, files []openIdentity) (string, error) {
	d, err := trustedDir(c.StateDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = d.Close() }()
	return publishSpace(d, c.StateDir, base, dev, ino, target, files, readPrivateJSON)
}

func publishSpace(d *os.File, state, base string, dev, ino uint64, target string, files []openIdentity, read func(string, any) error) (string, error) {
	name := fmt.Sprintf("%s%d-%d.json", spacePrefix(base), dev, ino)
	if !validRelative(filepath.Join(filepath.Base(base), target)) {
		return "", fmt.Errorf("invalid witness target")
	}
	w := spaceWitness{Base: base, Device: dev, Files: files, Target: target}
	b, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	// Pending files cannot authorize unlink. A crash before the atomic rename
	// leaves scratch intact; a subsequent scanner publishes a fresh complete
	// witness. Orphan pending files are diagnostics, never parsed as witnesses.
	temp := ".space-pending-" + uuid.NewString()
	fd, err := unix.Openat(int(d.Fd()), temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return "", fmt.Errorf("unresolved cleanup accounting: %w", err)
	}
	defer func() { _ = unix.Unlinkat(int(d.Fd()), temp, 0) }()
	f := os.NewFile(uintptr(fd), temp)
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	_ = f.Close()
	if err == nil {
		err = publishNoReplace(d, temp, name)
	}
	if errors.Is(err, unix.EEXIST) {
		var existing spaceWitness
		if e := read(filepath.Join(state, name), &existing); e != nil {
			return "", e
		}
		if existing.Base != base || existing.Device != dev || (existing.Target != "" && existing.Target != target) {
			return "", fmt.Errorf("space witness mismatch")
		}
		// On partial cleanup retain ALL original inodes, including already
		// unlinked files which may still have open descriptors. No new files.
		for _, key := range files {
			found := false
			for _, old := range existing.Files {
				if old == key {
					found = true
					break
				}
			}
			if !found {
				return "", fmt.Errorf("space witness file identity changed")
			}
		}
		err = nil
	}
	if err == nil {
		err = d.Sync()
	}
	return name, err
}

func (c *ManagedConfig) checkSpace(base string) error {
	return c.checkSpaceWith(base, spaceAccess{trustedDir, readPrivateJSON, platformOpenFiles, freeBytes})
}

// Only OS evidence boundaries are injectable; production always uses the
// root-owned state directory and the host-wide Linux descriptor inspection.
type spaceAccess struct {
	dir  func(string) (*os.File, error)
	read func(string, any) error
	open func([]openIdentity) (bool, error)
	free func(*os.File) (uint64, error)
}

func (c *ManagedConfig) checkSpaceWith(base string, access spaceAccess) error {
	d, err := access.dir(c.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	es, err := d.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range es {
		if !strings.HasPrefix(e.Name(), spacePrefix(base)) {
			continue
		}
		var w spaceWitness
		if err = access.read(filepath.Join(c.StateDir, e.Name()), &w); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			} // another scanner completed it
			return err
		}
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(e.Name(), spacePrefix(base)), ".json"), "-")
		if len(parts) != 2 || !strings.HasSuffix(e.Name(), ".json") {
			return fmt.Errorf("invalid witness name")
		}
		dev, de := strconv.ParseUint(parts[0], 10, 64)
		ino, ie := strconv.ParseUint(parts[1], 10, 64)
		if w.Base != base || de != nil || ie != nil || w.Device != dev || ino == 0 {
			return fmt.Errorf("space witness identity mismatch")
		}
		open, openErr := access.open(w.Files)
		if openErr != nil {
			return openErr
		}
		if open {
			return fmt.Errorf("space_unconfirmed: deleted scratch still has an open descriptor; no further SDR admission")
		}
		f, err := c.pinStorageBase(base)
		if err != nil {
			return err
		}
		err = witnessEmpty(f, w, ino)
		_, statErr := access.free(f) // observation health only, not historical admission policy
		_ = f.Close()
		if err != nil || statErr != nil {
			return errors.Join(err, statErr)
		}
		if err = unix.Unlinkat(int(d.Fd()), e.Name(), 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		if err = d.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// Empty tombstones are retained by both cleanup paths. Their inode is encoded
// in the witness name. Old witnesses lacked a target path; locate that exact
// inode only in allowed scratch namespaces, never canonical outputs.
func witnessEmpty(base *os.File, w spaceWitness, ino uint64) error {
	var targets []string
	if w.Target != "" {
		if !validRelative(filepath.Join(filepath.Base(w.Base), w.Target)) {
			return fmt.Errorf("invalid witness target")
		}
		targets = []string{w.Target}
	} else {
		es, err := base.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, e := range es {
			if legacyRoot.MatchString(e.Name()) {
				targets = append(targets, e.Name())
			}
			if sectorRoot.MatchString(e.Name()) {
				r, err := pinRelative(base, e.Name())
				if err != nil {
					return err
				}
				children, err := r.ReadDir(-1)
				_ = r.Close()
				if err != nil {
					return err
				}
				for _, child := range children {
					if validName(child.Name()) || oldAttempt.MatchString(child.Name()) {
						targets = append(targets, filepath.Join(e.Name(), child.Name()))
					}
				}
			}
		}
	}
	for _, target := range targets {
		f, err := pinRelative(base, target)
		if err != nil {
			return err
		}
		dev, id, err := identity(f)
		if err == nil && dev == w.Device && id == ino {
			es, e := f.ReadDir(-1)
			_ = f.Close()
			if e != nil {
				return e
			}
			if len(es) != 0 {
				return fmt.Errorf("space_unconfirmed: scratch files remain")
			}
			return nil
		}
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("space_unconfirmed: original scratch tombstone missing or replaced")
}
