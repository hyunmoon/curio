// Package sdrscratch separates a current writer's observed synchronous return
// from the durable return certificate required by a later scanner. A free lock
// is NOT evidence that an interrupted native call (or a child process) has ended.
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
	Run       *ManagedRun `json:",omitempty"`
}

type Result struct {
	Path           string
	Status         string
	Reason         string
	FilesRemoved   int
	AllocatedBytes uint64
	FreeBefore     uint64
	FreeAfter      uint64
}

// Writer owns a locked directory inode, not a replaceable lock file. Close
// releases the lock; it does not assert that native execution has ended.
type Writer struct {
	dir  *os.File
	r    record
	path string
	// Process-local authority, never reconstructed from a free lock or metadata.
	returnedHere bool
	recordIO     RecordIO
	boundary     Boundary
	basePath     string
	measurement  Result
}

// RecordIO is the narrow persistence boundary for writer diagnostics. Nil uses
// real xattr writes and directory Sync. It does not control reclamation checks
// or file removal. Callers must not use it to manufacture return certificates.
type RecordIO interface {
	SetXattr(int, string, []byte, int) error
	Sync(*os.File) error
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
	if w.recordIO != nil {
		return w.saveWith(state, w.recordIO.SetXattr, func() error { return w.recordIO.Sync(w.dir) })
	}
	return w.saveWith(state, unix.Fsetxattr, w.dir.Sync)
}

func (w *Writer) saveWith(state string, set func(int, string, []byte, int) error, sync func() error) error {
	r := w.r
	r.State = state
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := set(int(w.dir.Fd()), attribute, b, 0); err != nil {
		return err
	}
	if err := sync(); err != nil {
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
	return BeginWithRecordIO(path, nil)
}

// BeginWithRecordIO has the same ownership contract as Begin, with per-writer
// persistence operations for deterministic I/O failure testing.
func BeginWithRecordIO(path string, recordIO RecordIO) (*Writer, error) {
	return BeginWithOptions(path, Options{RecordIO: recordIO})
}

func BeginWithOptions(path string, options Options) (*Writer, error) {
	if !validName(filepath.Base(path)) || !strings.HasSuffix(filepath.Dir(path), ".sdr.tmp") {
		return nil, fmt.Errorf("invalid SDR scratch name")
	}
	basePath := filepath.Dir(filepath.Dir(path))
	boundary, err := configuredBoundary(options.Boundary, basePath)
	if err != nil {
		return nil, err
	}
	var run *ManagedRun
	if boundary != nil {
		run, err = boundary.Current(basePath)
		if err != nil {
			return nil, fmt.Errorf("managed SDR entry: %w", err)
		}
		if run == nil {
			return nil, fmt.Errorf("managed SDR entry missing identity")
		}
	}
	base, err := pinBoundaryBase(boundary, basePath)
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
	if _, err := sweepLockedWithBoundary(base, basePath, true, boundary); err != nil {
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
	w := &Writer{dir: dir, path: path, boundary: boundary, basePath: basePath, recordIO: options.RecordIO, r: record{Version: 1, Name: name, Root: rootName, State: "active", Run: run}}
	if run != nil {
		w.r.Version = 2
	}
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

func privateFile(dir *os.File, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return st, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	if err = unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return st, fmt.Errorf("not a private regular file: %s", name)
	}
	if err = sameDevice(dir, f); err != nil {
		return st, err
	}
	return st, nil
}

func (w *Writer) Close() error {
	w.returnedHere = false
	return w.dir.Close()
}

// Returned is called after a synchronous native ERROR return/pre-native failure
// or key publication. Managed discard also calls it after a successful native
// return followed by publication failure, while the inode is still in scratch.
// Never call it merely because context/ownership/heartbeat was lost.
// A persistence error does not erase this writer's directly observed return.
// Its lock and pinned FD must remain continuously held until cleanup finishes.
func (w *Writer) Returned() error {
	if err := w.checkPath(); err != nil {
		return err
	}
	w.returnedHere = true
	return w.save("returned")
}

func (w *Writer) checkPath() error {
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
	dev, ino, err = identity(w.dir)
	if err != nil {
		return err
	}
	if dev != w.r.Device || ino != w.r.Inode {
		return fmt.Errorf("writer descriptor identity changed; preserve scratch")
	}
	return nil
}

// ReclaimOwn uses only this writer's observed return, while its original pinned
// directory FD and lock remain held. It can run even when the return certificate
// could not be persisted. Scanners never acquire this process-local authority.
func (w *Writer) ReclaimOwn() (int, error) {
	if !w.returnedHere {
		return 0, fmt.Errorf("no return observed by this writer")
	}
	return w.reclaimFiles(unix.Unlinkat)
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
	return w.reclaimFiles(unlink)
}

func (w *Writer) reclaimFiles(unlink func(int, string, int) error) (int, error) {
	return w.reclaimFilesMode(unlink, false)
}

func (w *Writer) reclaimFilesMode(unlink func(int, string, int) error, discard bool) (int, error) {
	if !discard {
		b, err := get(w.dir, completionAttribute)
		if err != nil {
			return 0, fmt.Errorf("unknown completion marker: %w", err)
		}
		if string(b) != "incomplete" {
			return 0, fmt.Errorf("completed or unknown output preserved")
		}
	}
	if _, err := w.dir.Seek(0, 0); err != nil {
		return 0, err
	}
	entries, err := w.dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	// Validate the entire flat directory before deleting any file.
	var allocated uint64
	var files []openIdentity
	for _, e := range entries {
		st, err := privateFile(w.dir, e.Name())
		if err != nil {
			return 0, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG || uint64(st.Dev) != w.r.Device || st.Nlink != 1 {
			return 0, fmt.Errorf("non-private regular scratch file: %s", e.Name())
		}
		allocated += uint64(st.Blocks) * 512
		files = append(files, openIdentity{uint64(st.Dev), st.Ino})
	}
	w.measurement.AllocatedBytes = allocated
	w.measurement.FreeBefore, err = freeBytes(w.dir)
	if err != nil {
		return 0, err
	}
	var accounting *ManagedConfig
	if b, ok := w.boundary.(interface{ accounting() *ManagedConfig }); discard && ok && len(files) > 0 {
		accounting = b.accounting()
		if _, err = accounting.spaceStart(w.basePath, w.r.Device, w.r.Inode, filepath.Join(w.r.Root, w.r.Name), files); err != nil {
			return 0, err
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
	w.measurement.FreeAfter, err = freeBytes(w.dir)
	if err != nil {
		return n, err
	}
	if accounting != nil {
		if err = accounting.checkSpace(w.basePath); err != nil {
			return n, err
		}
	}
	return n, w.save("reclaimed")
}

func (w *Writer) Measurement() Result { return w.measurement }

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
	boundary, err := configuredBoundary(nil, basePath)
	if err != nil {
		return nil, err
	}
	return scanWithBoundary(basePath, reclaim, boundary)
}

// SweepWithBoundary exposes the same scanner to deterministic lifetime tests.
func SweepWithBoundary(basePath string, reclaim bool, boundary Boundary) ([]Result, error) {
	return scanWithBoundary(basePath, reclaim, boundary)
}

func scanWithBoundary(basePath string, reclaim bool, boundary Boundary) ([]Result, error) {
	if b, ok := boundary.(interface{ checkBase(string) error }); ok {
		if err := b.checkBase(basePath); err != nil {
			if errors.Is(err, errRegisteredBaseMissing) {
				if accounting, ok := boundary.(interface{ checkSpace(string) error }); ok {
					return nil, accounting.checkSpace(basePath)
				}
				return nil, nil
			}
			return nil, err
		}
	}
	base, err := pinBoundaryBase(boundary, basePath)
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
	r, e := sweepLockedWithBoundary(base, basePath, reclaim, boundary)
	if b, ok := boundary.(interface{ checkSpace(string) error }); ok {
		e = errors.Join(e, b.checkSpace(basePath))
	}
	return r, e
}

func sweepLockedWithBoundary(base *os.File, basePath string, reclaim bool, boundary Boundary) ([]Result, error) {
	entries, err := base.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var results []Result
	var failures error
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sdr.tmp") {
			if boundary != nil && legacyRoot.MatchString(e.Name()) {
				f, e2 := openDirAt(int(base.Fd()), e.Name())
				if e2 != nil {
					return results, e2
				}
				safeEmpty := maintenanceEmpty(f)
				_, e2 = f.ReadDir(-1)
				_ = f.Close()
				if e2 != nil {
					return results, e2
				}
				if !safeEmpty {
					failures = errors.Join(failures, fmt.Errorf("legacy_requires_maintenance: %s", filepath.Join(basePath, e.Name())))
				}
			}
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
			r, err := inspectWithBoundary(root, c.Name(), reclaim, basePath, boundary)
			if boundary != nil && r.Status == "needs_review" {
				err = errors.Join(err, fmt.Errorf("legacy_requires_maintenance: %s", r.Reason))
			}
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

func inspectWithBoundary(root *os.File, name string, reclaim bool, basePath string, boundary Boundary) (Result, error) {
	r := Result{Status: "needs_review"}
	if !validName(name) {
		if boundary != nil && oldAttempt.MatchString(name) {
			f, e := openDirAt(int(root.Fd()), name)
			if e != nil {
				return r, e
			}
			defer func() { _ = f.Close() }()
			if e = sameDevice(root, f); e != nil {
				return r, e
			}
			if e = lock(f); e != nil {
				return r, e
			}
			if maintenanceEmpty(f) {
				r.Status = "empty_legacy_tombstone"
				return r, nil
			}
		}
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
	if boundary != nil && maintenanceEmpty(dir) {
		r.Status = "empty_legacy_tombstone"
		return r, nil
	}
	w := &Writer{dir: dir, boundary: boundary, basePath: basePath}
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
	if (w.r.Version != 1 && w.r.Version != 2) || (w.r.Version == 2) != (w.r.Run != nil) || w.r.Name != name || w.r.Root != root.Name() || w.r.RootInode != rootInode || w.r.Device != dev || w.r.Inode != ino {
		r.Reason = "writer identity mismatch"
		return r, nil
	}
	if w.r.Run != nil && w.r.State != "reclaimed" {
		if w.r.State != "active" && w.r.State != "returned" {
			return r, fmt.Errorf("unknown managed state")
		}
		if boundary == nil {
			return r, fmt.Errorf("managed scratch requires its discard domain")
		}
		// Even returned certificates are validated against the enrolled domain.
		stopped, err := boundary.Stopped(basePath, *w.r.Run)
		if err != nil {
			r.Reason = err.Error()
			return r, err
		}
		if !stopped && w.r.State != "returned" {
			r.Status = "termination_required"
			r.Reason = "native process subtree is still populated"
			return r, fmt.Errorf("%s", r.Reason)
		}
		if !reclaim {
			r.Status = "cleanup_pending"
			return r, fmt.Errorf("managed SDR discard pending")
		}
		r.FilesRemoved, err = w.reclaimFilesMode(unix.Unlinkat, true)
		r.AllocatedBytes = w.measurement.AllocatedBytes
		r.FreeBefore = w.measurement.FreeBefore
		r.FreeAfter = w.measurement.FreeAfter
		if err != nil {
			r.Status = "cleanup_failed"
			r.Reason = err.Error()
			return r, err
		}
		r.Status = "reclaimed"
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
