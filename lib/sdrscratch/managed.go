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
	for _, s := range c.Storage {
		if base != filepath.Join(s.Root, "cache") && base != filepath.Join(s.Root, "key") {
			continue
		}
		root, err := openDir(s.Root)
		if err != nil {
			return err
		}
		defer func() { _ = root.Close() }()
		dev, ino, err := identity(root)
		if err != nil {
			return err
		}
		if dev != s.Device || ino != s.Inode {
			return fmt.Errorf("storage root identity changed")
		}
		if err := supportedFS(root); err != nil {
			return err
		}
		fd, err := unix.Openat(int(root.Fd()), "sectorstore.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), "sectorstore.json")
		defer func() { _ = f.Close() }()
		var meta struct{ ID string }
		if err := json.NewDecoder(io.LimitReader(f, 65536)).Decode(&meta); err != nil {
			return err
		}
		if meta.ID != s.ID {
			return fmt.Errorf("storage ID changed")
		}
		return nil
	}
	return fmt.Errorf("storage not enrolled in personal discard domain")
}

func configuredBoundary(injected Boundary) (Boundary, error) {
	if injected != nil {
		return injected, nil
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
	Base                string
	Device, MinimumFree uint64
	Files               []openIdentity
}
type openIdentity struct{ Device, Inode uint64 }

func freeBytes(f *os.File) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// The watermark is a conservative admission guard, not attributable disk
// recovery accounting: unrelated writers may change filesystem free space.
func (c *ManagedConfig) spaceStart(base string, dev, ino, minimum uint64, files []openIdentity) (string, error) {
	d, err := trustedDir(c.StateDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = d.Close() }()
	name := fmt.Sprintf("space-%x-%d-%d.json", sha256.Sum256([]byte(base)), dev, ino)
	b, err := json.Marshal(spaceWitness{base, dev, minimum, files})
	if err != nil {
		return "", err
	}
	fd, err := unix.Openat(int(d.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if errors.Is(err, unix.EEXIST) {
		var w spaceWitness
		if e := readPrivateJSON(filepath.Join(c.StateDir, name), &w); e != nil {
			return "", e
		}
		if w.Base != base || w.Device != dev {
			return "", fmt.Errorf("space witness mismatch")
		}
		return name, nil
	}
	if err != nil {
		return "", fmt.Errorf("unresolved cleanup accounting: %w", err)
	}
	f := os.NewFile(uintptr(fd), name)
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	_ = f.Close()
	if err == nil {
		err = d.Sync()
	}
	return name, err
}

func (c *ManagedConfig) checkSpace(base string) error {
	d, err := trustedDir(c.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	es, err := d.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range es {
		if !strings.HasPrefix(e.Name(), "space-") {
			continue
		}
		var w spaceWitness
		if err = readPrivateJSON(filepath.Join(c.StateDir, e.Name()), &w); err != nil {
			return err
		}
		if w.Base != base {
			continue
		}
		open, openErr := platformOpenFiles(w.Files)
		if openErr != nil {
			return openErr
		}
		if open {
			return fmt.Errorf("space_unconfirmed: deleted scratch still has an open descriptor; no further SDR admission")
		}
		f, err := openDir(base)
		if err != nil {
			return err
		}
		dev, _, err := identity(f)
		free, se := freeBytes(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if se != nil {
			return se
		}
		if dev != w.Device {
			return fmt.Errorf("space witness device changed")
		}
		if free < w.MinimumFree {
			return fmt.Errorf("space_unconfirmed: %s free=%d required=%d; no further SDR admission", base, free, w.MinimumFree)
		}
		if err = unix.Unlinkat(int(d.Fd()), e.Name(), 0); err != nil {
			return err
		}
		if err = d.Sync(); err != nil {
			return err
		}
	}
	return nil
}
