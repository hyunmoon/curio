// Package sdrscratch reclaims only scratch whose synchronous writer durably
// recorded that it no longer uses the directory. A free lock is NOT evidence
// that an interrupted native call (or a child process) has ended.
package sdrscratch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const Prefix = "owned-v1-"
const attribute = "user.curio.sdr-scratch-v1"
const completionAttribute = "user.curio.sdr-completion-v1"

type record struct {
	Version   int
	Name      string
	Root      string
	RootInode uint64
	Device    uint64
	Inode     uint64
	State     string
}

type Result struct {
	Path         string
	Status       string
	Reason       string
	FilesRemoved int
}

// Writer owns a locked directory inode, not a replaceable lock file. Close
// releases the lock; it does not assert that native execution has ended.
type Writer struct {
	dir  *os.File
	r    record
	path string
}

func openDirAt(parent int, name string) (*os.File, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Pin every component without following symlinks, including storage ancestors.
func openDir(path string) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := openDirAt(unix.AT_FDCWD, string(os.PathSeparator))
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(abs, string(os.PathSeparator)), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		next, e := openDirAt(int(f.Fd()), part)
		_ = f.Close()
		if e != nil {
			return nil, e
		}
		f = next
	}
	return f, nil
}

func lock(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }

func identity(f *os.File) (uint64, uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Dev), st.Ino, nil
}

func get(f *os.File, key string) ([]byte, error) {
	b := make([]byte, 8192)
	n, err := unix.Fgetxattr(int(f.Fd()), key, b)
	if err != nil {
		return nil, err
	}
	return b[:n], nil
}

func (w *Writer) save(state string) error {
	return w.saveWith(state, unix.Fsetxattr)
}

func (w *Writer) saveWith(state string, set func(int, string, []byte, int) error) error {
	r := w.r
	r.State = state
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := set(int(w.dir.Fd()), attribute, b, 0); err != nil {
		return err
	}
	if err := w.dir.Sync(); err != nil {
		return err
	}
	w.r = r
	return nil
}

func validName(name string) bool {
	if !strings.HasPrefix(name, Prefix) {
		return false
	}
	id := strings.TrimPrefix(name, Prefix)
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

// Begin serializes new scratch creation with startup/admission scans of the
// same cache/key directory. Contention returns an error, never a sleeping
// scheduler or a lock held throughout native execution.
func Begin(path string) (*Writer, error) {
	if !validName(filepath.Base(path)) || !strings.HasSuffix(filepath.Dir(path), ".sdr.tmp") {
		return nil, fmt.Errorf("invalid SDR scratch name")
	}
	basePath := filepath.Dir(filepath.Dir(path))
	base, err := openDir(basePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = base.Close() }() // releases the directory lock
	if err := supportedFS(base); err != nil {
		return nil, err
	}
	if err := lock(base); err != nil {
		return nil, fmt.Errorf("SDR scratch scan/creation busy: %w", err)
	}
	if _, err := sweepLocked(base, basePath, true); err != nil {
		return nil, err
	}
	rootName := filepath.Base(filepath.Dir(path))
	if err := unix.Mkdirat(int(base.Fd()), rootName, 0755); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	root, err := openDirAt(int(base.Fd()), rootName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := sameDevice(base, root); err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if err := unix.Mkdirat(int(root.Fd()), name, 0755); err != nil {
		return nil, err
	}
	dir, err := openDirAt(int(root.Fd()), name)
	if err != nil {
		return nil, err
	}
	w := &Writer{dir: dir, path: path, r: record{Version: 1, Name: name, Root: rootName, State: "active"}}
	if err = lock(dir); err == nil {
		w.r.Device, w.r.Inode, err = identity(dir)
	}
	if err == nil {
		_, w.r.RootInode, err = identity(root)
	}
	if err == nil {
		err = sameDevice(root, dir)
	}
	if err == nil {
		err = w.save("active")
	}
	if err == nil {
		err = unix.Fsetxattr(int(dir.Fd()), completionAttribute, []byte("incomplete"), 0)
	}
	if err == nil {
		err = dir.Sync()
	}
	if err == nil {
		err = root.Sync()
	}
	if err == nil {
		err = base.Sync()
	}
	if err != nil {
		_ = dir.Close()
		return nil, err
	} // unknown remnants stay protected
	return w, nil
}

func sameDevice(a, b *os.File) error {
	da, _, err := identity(a)
	if err != nil {
		return err
	}
	db, _, err := identity(b)
	if err != nil {
		return err
	}
	if da != db {
		return fmt.Errorf("cross-device SDR scratch")
	}
	if err := supportedFS(b); err != nil {
		return err
	}
	return sameMount(a, b)
}

func (w *Writer) Close() error { return w.dir.Close() }

// Returned must be called only after a synchronous native ERROR return (or a
// failure before entering native), or after a key was successfully published.
// Never call it merely because context/ownership/heartbeat was lost.
func (w *Writer) Returned() error {
	current, err := openDir(w.path)
	if err != nil {
		return err
	}
	defer func() { _ = current.Close() }()
	dev, ino, err := identity(current)
	if err != nil {
		return err
	}
	if dev != w.r.Device || ino != w.r.Inode {
		return fmt.Errorf("writer pathname replaced; preserve scratch")
	}
	return w.save("returned")
}

// Reclaim never traverses directories, symlinks or mount points, and never
// removes the directory by pathname. The empty inode-bound tombstone is kept:
// POSIX has no conditional unlink-directory-by-inode operation. Thus a renamed
// or replaced pathname cannot cause deletion of a newer attempt's directory.
func (w *Writer) Reclaim() (int, error) { return w.reclaim(unix.Unlinkat) }

func (w *Writer) reclaim(unlink func(int, string, int) error) (int, error) {
	if w.r.State != "returned" {
		return 0, fmt.Errorf("no synchronous return certificate")
	}
	b, err := get(w.dir, completionAttribute)
	if err != nil {
		return 0, fmt.Errorf("unknown completion marker: %w", err)
	}
	if string(b) != "incomplete" {
		return 0, fmt.Errorf("completed or unknown output preserved")
	}
	if _, err := w.dir.Seek(0, 0); err != nil {
		return 0, err
	}
	entries, err := w.dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	// Validate the entire flat directory before deleting any file.
	for _, e := range entries {
		var st unix.Stat_t
		if err := unix.Fstatat(int(w.dir.Fd()), e.Name(), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return 0, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG || uint64(st.Dev) != w.r.Device || st.Nlink != 1 {
			return 0, fmt.Errorf("non-private regular scratch file: %s", e.Name())
		}
	}
	n := 0
	for _, e := range entries {
		if err := unlink(int(w.dir.Fd()), e.Name(), 0); err != nil {
			return n, err
		}
		n++
	}
	if err := w.dir.Sync(); err != nil {
		return n, err
	}
	return n, w.save("reclaimed")
}

// Sweep scans one local cache/key directory. Unknown/active/completed entries
// remain needs_review/live. Errors reclaiming certified remnants are returned
// to the caller so admission cannot silently treat them as reclaimed space.
func Sweep(basePath string) ([]Result, error) {
	return scan(basePath, true)
}

// Check never unlinks. It may be called inside an existing storage reservation
// critical section, after an out-of-lock Sweep. Pending work denies admission.
func Check(basePath string) error {
	_, err := scan(basePath, false)
	return err
}

func scan(basePath string, reclaim bool) ([]Result, error) {
	base, err := openDir(basePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = base.Close() }()
	if err := supportedFS(base); err != nil {
		return nil, err
	}
	if err := lock(base); err != nil {
		return nil, fmt.Errorf("SDR scratch scan/creation busy: %w", err)
	}
	return sweepLocked(base, basePath, reclaim)
}

func sweepLocked(base *os.File, basePath string, reclaim bool) ([]Result, error) {
	entries, err := base.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var results []Result
	var failures error
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sdr.tmp") {
			continue
		}
		root, err := openDirAt(int(base.Fd()), e.Name())
		if err != nil {
			return results, err
		}
		if err := sameDevice(base, root); err != nil {
			_ = root.Close()
			return results, err
		}
		children, err := root.ReadDir(-1)
		if err != nil {
			_ = root.Close()
			return results, err
		}
		for _, c := range children {
			path := filepath.Join(basePath, e.Name(), c.Name())
			r, err := inspect(root, c.Name(), reclaim)
			r.Path = path
			results = append(results, r)
			if err != nil {
				failures = errors.Join(failures, fmt.Errorf("%s: %w", path, err))
			}
		}
		_ = root.Close()
	}
	return results, failures
}

func inspect(root *os.File, name string, reclaim bool) (Result, error) {
	r := Result{Status: "needs_review"}
	if !validName(name) {
		r.Reason = "nonparticipating/legacy format"
		return r, nil
	}
	dir, err := openDirAt(int(root.Fd()), name)
	if err != nil {
		r.Reason = err.Error()
		return r, err
	}
	defer func() { _ = dir.Close() }()
	if err := sameDevice(root, dir); err != nil {
		r.Reason = err.Error()
		return r, err
	}
	if err := lock(dir); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			r.Status = "live"
			r.Reason = "writer holds directory lock"
			return r, nil
		}
		return r, err
	}
	w := &Writer{dir: dir}
	b, err := get(dir, attribute)
	if err != nil {
		r.Reason = "missing writer metadata"
		return r, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&w.r); err != nil {
		r.Reason = "unknown writer metadata"
		return r, nil
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		r.Reason = "trailing writer metadata"
		return r, nil
	}
	dev, ino, err := identity(dir)
	if err != nil {
		return r, err
	}
	_, rootInode, err := identity(root)
	if err != nil {
		return r, err
	}
	if w.r.Version != 1 || w.r.Name != name || w.r.Root != root.Name() || w.r.RootInode != rootInode || w.r.Device != dev || w.r.Inode != ino {
		r.Reason = "writer identity mismatch"
		return r, nil
	}
	switch w.r.State {
	case "reclaimed":
		es, err := dir.ReadDir(-1)
		if err != nil {
			return r, err
		}
		if len(es) != 0 {
			r.Reason = "reclaimed tombstone changed"
			return r, nil
		}
		r.Status = "already_reclaimed"
	case "returned":
		if !reclaim {
			r.Status = "cleanup_pending"
			return r, fmt.Errorf("SDR scratch reclamation pending")
		}
		r.FilesRemoved, err = w.Reclaim()
		if err != nil {
			r.Status = "cleanup_failed"
			r.Reason = err.Error()
			return r, err
		}
		r.Status = "reclaimed"
	default:
		r.Reason = "no synchronous return certificate; free lock is not native termination"
	}
	return r, nil
}
