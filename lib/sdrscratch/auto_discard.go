package sdrscratch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

var canonicalSector = regexp.MustCompile(`^s-t0[0-9]+-[0-9]+$`)

// AutoStage is supplied inside a locked, live pipeline read by the storage
// layer. It is not native termination evidence. Receipt/unknown layout checks
// remain independent. No pipeline or task is removed by this package.
type AutoStage struct {
	Allowed    bool
	Reason     string
	LayerNames []string
	LayerBytes int64
}

type AutoTarget struct {
	Base, Relative, Sector string
	Canonical              bool
	StorageID              string
	observationError       error
}
type AutoState func(AutoTarget, func(AutoStage) error) error

// AutoDiscard performs bounded namespace discovery, never recursive glob/rm.
// Private old attempts and demonstrably incomplete canonical cache are checked
// per sector. A rejected target does not become a root-wide admission veto.
func AutoDiscard(base string, state AutoState) ([]Result, error) {
	on, err := PersonalCleanupEnabled()
	if err != nil || !on {
		return nil, err
	}
	s := personalSessionPtr.Load()
	if s == nil {
		return nil, fmt.Errorf("managed session absent")
	}
	c, err := s.load(filepath.Dir(base))
	if err != nil {
		return nil, err
	}
	return autoDiscard(c, base, state, autoIO{autoParticipants, trustedDir, c.spaceStart, c.checkSpace, unix.Unlinkat, platformBoundary})
}

type autoIO struct {
	participants func(*ManagedConfig) error
	openState    func(string) (*os.File, error)
	spaceStart   func(string, uint64, uint64, string, []openIdentity) (string, error)
	checkSpace   func(string) error
	unlink       func(int, string, int) error
	boundary     func(*ManagedConfig) (Boundary, error)
}

func autoTargets(base string, f *os.File) ([]AutoTarget, error) {
	es, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []AutoTarget
	for _, e := range es {
		n := e.Name()
		switch {
		case canonicalSector.MatchString(n) && filepath.Base(base) == "cache":
			out = append(out, AutoTarget{Base: base, Relative: n, Sector: n, Canonical: true})
		case legacyRoot.MatchString(n):
			out = append(out, AutoTarget{Base: base, Relative: n, Sector: strings.TrimSuffix(n, ".tmp")})
		case sectorRoot.MatchString(n):
			d, err := pinRelative(f, n)
			if err != nil {
				out = append(out, AutoTarget{Base: base, Relative: n, Sector: strings.TrimSuffix(n, ".sdr.tmp"), observationError: err})
				continue
			}
			children, err := d.ReadDir(-1)
			_ = d.Close()
			if err != nil {
				out = append(out, AutoTarget{Base: base, Relative: n, Sector: strings.TrimSuffix(n, ".sdr.tmp"), observationError: err})
				continue
			}
			for _, child := range children {
				if validName(child.Name()) || oldAttempt.MatchString(child.Name()) {
					out = append(out, AutoTarget{Base: base, Relative: filepath.Join(n, child.Name()), Sector: strings.TrimSuffix(n, ".sdr.tmp")})
				}
			}
		}
	}
	return out, nil
}

func autoDiscard(c *ManagedConfig, base string, state AutoState, io autoIO) ([]Result, error) {
	if state == nil {
		return nil, fmt.Errorf("pipeline state provider absent")
	}
	f, err := c.pinStorageBase(base)
	if errors.Is(err, errRegisteredBaseMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	targets, err := autoTargets(base, f)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, t := range targets {
		t.StorageID = c.Storage[0].ID
		r := Result{Path: filepath.Join(base, t.Relative), Status: "deferred"}
		if t.observationError != nil {
			r.Reason = t.observationError.Error()
			out = append(out, r)
			continue
		}
		gate, e := sectorGate(c, t.Sector, true, io.openState)
		if e != nil {
			r.Reason = e.Error()
			out = append(out, r)
			continue
		}
		e = func() error {
			defer func() { _ = gate.Close() }()
			return state(t, func(stage AutoStage) error {
				if !stage.Allowed {
					r.Status = "protected"
					r.Reason = stage.Reason
					return nil
				}
				if e := io.participants(c); e != nil {
					return e
				}
				d, e := pinRelative(f, t.Relative)
				if e != nil {
					return e
				}
				defer func() { _ = d.Close() }()
				if e = lock(d); e != nil {
					return e
				}
				dev, ino, e := identity(d)
				if e != nil {
					return e
				}
				if raw, e := get(d, attribute); e == nil {
					var rec record
					if e = json.Unmarshal(raw, &rec); e != nil {
						return fmt.Errorf("unknown writer metadata")
					}
					if (rec.Version != 1 && rec.Version != 2) || (rec.Version == 2 && rec.Run == nil) || rec.Device != dev || rec.Inode != ino || rec.Name != filepath.Base(t.Relative) || rec.Root != t.Sector+".sdr.tmp" || (rec.State != "active" && rec.State != "returned" && rec.State != "reclaimed") {
						return fmt.Errorf("writer metadata identity mismatch")
					}
					if rec.Run != nil && rec.State != "returned" && rec.State != "reclaimed" {
						boundary, e := io.boundary(c)
						if e != nil {
							return e
						}
						stopped, e := boundary.Stopped(base, *rec.Run)
						if e != nil {
							return e
						}
						if !stopped {
							return fmt.Errorf("recorded writer subtree is still alive")
						}
					}
				} else if !missingAttribute(e) {
					return e
				}
				files, e := autoFiles(d, t, stage)
				if e != nil {
					return e
				}
				if len(files) == 0 {
					if e = io.checkSpace(base); e != nil {
						return e
					}
					r.Status = "empty"
					return removeAutoDirectory(f, t.Relative, dev, ino)
				}
				// Repin immediately before removal, while the sector gate and
				// pipeline row are still held. No pathname-only recursive removal.
				if e = io.participants(c); e != nil {
					return e
				}
				current, e := pinRelative(f, t.Relative)
				if e != nil {
					return e
				}
				cd, ci, e := identity(current)
				_ = current.Close()
				if e != nil || cd != dev || ci != ino {
					return fmt.Errorf("target identity changed")
				}
				var identities []openIdentity
				for _, file := range files {
					identities = append(identities, openIdentity{file.Device, file.Inode})
				}
				if _, e = io.spaceStart(base, dev, ino, t.Relative, identities); e != nil {
					return e
				}
				r.FreeBefore, e = freeBytes(d)
				if e != nil {
					return e
				}
				for _, file := range files {
					st, e := privateFile(d, file.Name)
					if e != nil {
						return e
					}
					if uint64(st.Dev) != file.Device || st.Ino != file.Inode || st.Size != file.Size {
						return fmt.Errorf("file identity changed")
					}
					if e = io.unlink(int(d.Fd()), file.Name, 0); e != nil {
						return e
					}
					r.FilesRemoved++
					r.AllocatedBytes += uint64(file.Blocks) * 512
				}
				if e = d.Sync(); e != nil {
					return e
				}
				if e = io.checkSpace(base); e != nil {
					return e
				}
				r.FreeAfter, e = freeBytes(d)
				if e != nil {
					return e
				}
				r.Status = "reclaimed"
				r.Reason = stage.Reason
				return removeAutoDirectory(f, t.Relative, dev, ino)
			})
		}()
		if e != nil {
			r.Reason = e.Error()
			r.Status = "deferred"
			if r.FilesRemoved > 0 {
				r.Status = "partial_or_space_unconfirmed"
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func removeAutoDirectory(base *os.File, relative string, device, inode uint64) error {
	d, err := pinRelative(base, relative)
	if err != nil {
		return err
	}
	dev, ino, err := identity(d)
	_ = d.Close()
	if err != nil || dev != device || ino != inode {
		return fmt.Errorf("empty directory identity changed")
	}
	parent := base
	if filepath.Dir(relative) != "." {
		parent, err = pinRelative(base, filepath.Dir(relative))
		if err != nil {
			return err
		}
		defer func() { _ = parent.Close() }()
	}
	if err = unix.Unlinkat(int(parent.Fd()), filepath.Base(relative), unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.Sync()
}

func autoFiles(d *os.File, t AutoTarget, s AutoStage) ([]LegacyFile, error) {
	// A marker is a reason to preserve, never merely an xattr-presence proof of
	// successful SDR. Normal reuse still performs full receipt validation in FFI.
	b, e := get(d, completionAttribute)
	if e == nil && string(b) != "incomplete" && t.Canonical {
		return nil, fmt.Errorf("completion receipt or unknown completion metadata preserved")
	}
	if e != nil && !missingAttribute(e) {
		return nil, fmt.Errorf("completion evidence unreadable: %w", e)
	}
	es, e := d.ReadDir(-1)
	if e != nil {
		return nil, e
	}
	allowed := map[string]bool{}
	for _, n := range s.LayerNames {
		allowed[n] = true
	}
	if len(allowed) == 0 || s.LayerBytes <= 0 {
		return nil, fmt.Errorf("SDR layout unknown")
	}
	var files []LegacyFile
	complete := len(es) == len(allowed)
	for _, entry := range es {
		if !allowed[entry.Name()] {
			return nil, fmt.Errorf("non-SDR layer preserved: %s", entry.Name())
		}
		st, e := privateFile(d, entry.Name())
		if e != nil {
			return nil, e
		}
		if st.Size < 0 || st.Size > s.LayerBytes {
			return nil, fmt.Errorf("unexpected layer size")
		}
		complete = complete && st.Size == s.LayerBytes
		files = append(files, LegacyFile{entry.Name(), uint64(st.Dev), st.Ino, st.Size, st.Blocks})
	}
	if t.Canonical && complete {
		return nil, fmt.Errorf("possible successful legacy SDR: full layer layout preserved")
	}
	return files, nil
}
