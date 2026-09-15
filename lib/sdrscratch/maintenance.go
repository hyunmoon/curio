package sdrscratch

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var legacyRoot = regexp.MustCompile(`^s-t0[0-9]+-[0-9]+\.tmp$`)
var sectorRoot = regexp.MustCompile(`^s-t0[0-9]+-[0-9]+\.sdr\.tmp$`)
var oldAttempt = regexp.MustCompile(`^attempt-[a-f0-9-]{36}$`)

const maintenanceAttribute = "user.curio.sdr-maintenance-empty-v1"

func maintenanceEmpty(f *os.File) bool {
	b, err := get(f, maintenanceAttribute)
	if err != nil {
		return false
	}
	d, i, err := identity(f)
	if err != nil || string(b) != fmt.Sprintf("%d:%d", d, i) {
		return false
	}
	if _, err = f.Seek(0, 0); err != nil {
		return false
	}
	es, err := f.ReadDir(-1)
	_, _ = f.Seek(0, 0)
	return err == nil && len(es) == 0
}

type LegacyFile struct {
	Name          string
	Device, Inode uint64
	Size, Blocks  int64
}
type LegacyTarget struct {
	Root, Relative string
	Device, Inode  uint64
	Files          []LegacyFile
}
type LegacyPlan struct {
	Version                         int
	ConfigHash, Host, Boot, Created string
	Targets                         []LegacyTarget
}
type LegacyResult struct {
	Path, Status, Error                   string
	FilesRemoved                          int
	AllocatedBytes, FreeBefore, FreeAfter uint64
}

func configHash(c *ManagedConfig) string {
	b, _ := json.Marshal(c)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func validRelative(path string) bool {
	if filepath.Clean(path) != path || filepath.IsAbs(path) {
		return false
	}
	p := strings.Split(path, "/")
	if len(p) < 2 || (p[0] != "cache" && p[0] != "key") {
		return false
	}
	if len(p) == 2 {
		return legacyRoot.MatchString(p[1])
	}
	return len(p) == 3 && sectorRoot.MatchString(p[1]) && (validName(p[2]) || oldAttempt.MatchString(p[2]))
}

func pinLegacy(c *ManagedConfig, root, relative string) (*os.File, LegacyTarget, error) {
	t := LegacyTarget{Root: root, Relative: relative}
	if !validRelative(relative) {
		return nil, t, fmt.Errorf("not an exact private SDR temporary namespace: %s", relative)
	}
	base := filepath.Join(root, strings.Split(relative, "/")[0])
	if err := c.checkStorage(base); err != nil {
		return nil, t, err
	}
	f, err := openDir(filepath.Join(root, relative))
	if err != nil {
		return nil, t, err
	}
	fail := func(e error) (*os.File, LegacyTarget, error) { _ = f.Close(); return nil, t, e }
	b, err := openDir(base)
	if err != nil {
		return fail(err)
	}
	err = sameDevice(b, f)
	_ = b.Close()
	if err != nil {
		return fail(err)
	}
	if err = lock(f); err != nil {
		return fail(fmt.Errorf("termination_required: directory busy: %w", err))
	}
	t.Device, t.Inode, err = identity(f)
	if err != nil {
		return fail(err)
	}
	es, err := f.ReadDir(-1)
	if err != nil {
		return fail(err)
	}
	for _, e := range es {
		s, e2 := privateFile(f, e.Name())
		if e2 != nil {
			err = e2
			return fail(err)
		}
		if s.Mode&unix.S_IFMT != unix.S_IFREG || s.Nlink != 1 || uint64(s.Dev) != t.Device {
			return fail(fmt.Errorf("protected non-private file: %s", e.Name()))
		}
		t.Files = append(t.Files, LegacyFile{e.Name(), uint64(s.Dev), s.Ino, s.Size, s.Blocks})
	}
	return f, t, nil
}

// BuildLegacyPlan is read-only. Each root/path is explicit; canonical names,
// glob expansion, recursive discovery and sector GC are deliberately absent.
func BuildLegacyPlan(c *ManagedConfig, paths []LegacyTarget, host, boot string) (*LegacyPlan, error) {
	p := &LegacyPlan{Version: 1, ConfigHash: configHash(c), Host: host, Boot: boot, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	if host != c.Host || len(paths) == 0 || len(paths) > 1024 {
		return nil, fmt.Errorf("wrong host or invalid finite target count")
	}
	seen := map[string]bool{}
	for _, path := range paths {
		key := filepath.Join(path.Root, path.Relative)
		if seen[key] {
			return nil, fmt.Errorf("duplicate target: %s", key)
		}
		seen[key] = true
		f, t, err := pinLegacy(c, path.Root, path.Relative)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
		p.Targets = append(p.Targets, t)
	}
	return p, nil
}

// ExecuteLegacy holds the exclusive launcher lease, rechecks all accessor
// subtrees and every approved inode, then unlinks only flat private files.
// Guard injection is unexported and used only in offline tests.
func ExecuteLegacy(c *ManagedConfig, p *LegacyPlan, journal string, in io.Reader, out io.Writer) ([]LegacyResult, error) {
	h, b, err := platformHostBoot()
	if err != nil {
		return nil, err
	}
	lease, err := AcquireDomain(c, true)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Close() }()
	return executeLegacy(c, p, journal, in, out, h, b, func() error { return MaintenanceGuard(c) }, unix.Unlinkat, true)
}

func executeLegacy(c *ManagedConfig, p *LegacyPlan, journal string, in io.Reader, out io.Writer, host, boot string, guard func() error, unlink func(int, string, int) error, account bool) ([]LegacyResult, error) {
	if p.Version != 1 || p.ConfigHash != configHash(c) || p.Host != host || p.Boot != boot || host != c.Host || len(p.Targets) == 0 || len(p.Targets) > 1024 {
		return nil, fmt.Errorf("plan config/host/boot mismatch or invalid size")
	}
	if err := guard(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, t := range p.Targets {
		key := filepath.Join(t.Root, t.Relative)
		if seen[key] {
			return nil, fmt.Errorf("duplicate plan target")
		}
		seen[key] = true
		f, current, err := pinLegacy(c, t.Root, t.Relative)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
		if !reflect.DeepEqual(current, t) {
			return nil, fmt.Errorf("plan changed: %s", t.Relative)
		}
		if _, err := fmt.Fprintf(out, "DISCARD %s files=%d allocated_bytes=%d\n", key, len(t.Files), allocatedFiles(t.Files)); err != nil {
			return nil, err
		}
	}
	if _, err := fmt.Fprintf(out, "All listed storage accessors must remain stopped and restart-excluded. No unmanaged/remote accessor or process migration is permitted. Canonical outputs and task/DB state are NOT targets.\nType DISCARD LEGACY %d: ", len(p.Targets)); err != nil {
		return nil, err
	}
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil || strings.TrimSuffix(strings.TrimSuffix(answer, "\n"), "\r") != fmt.Sprintf("DISCARD LEGACY %d", len(p.Targets)) {
		return nil, fmt.Errorf("cancelled; zero file removals")
	}
	if !filepath.IsAbs(journal) || filepath.Clean(journal) != journal {
		return nil, fmt.Errorf("absolute journal path required")
	}
	for _, s := range c.Storage {
		if journal == s.Root || strings.HasPrefix(journal, s.Root+"/") {
			return nil, fmt.Errorf("journal must be outside storage")
		}
	}
	j, err := os.OpenFile(journal, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = j.Close() }()
	parent, err := openDir(filepath.Dir(journal))
	if err != nil {
		return nil, err
	}
	err = parent.Sync()
	_ = parent.Close()
	if err != nil {
		return nil, err
	}
	write := func(v any) error {
		if err := json.NewEncoder(j).Encode(v); err != nil {
			return err
		}
		return j.Sync()
	}
	if err = write(p); err != nil {
		return nil, err
	}
	results := make([]LegacyResult, len(p.Targets))
	for i, t := range p.Targets {
		results[i] = LegacyResult{Path: filepath.Join(t.Root, t.Relative), Status: "unattempted"}
	}
	for i, t := range p.Targets {
		if err = guard(); err != nil {
			return results, err
		}
		f, current, e := pinLegacy(c, t.Root, t.Relative)
		if e != nil {
			return results, e
		}
		if !reflect.DeepEqual(current, t) {
			_ = f.Close()
			return results, fmt.Errorf("plan changed: %s", t.Relative)
		}
		r := &results[i]
		r.AllocatedBytes = allocatedFiles(t.Files)
		r.FreeBefore, err = freeBytes(f)
		if err == nil && account && r.AllocatedBytes > 0 {
			base := filepath.Join(t.Root, strings.Split(t.Relative, "/")[0])
			var keys []openIdentity
			for _, file := range t.Files {
				keys = append(keys, openIdentity{file.Device, file.Inode})
			}
			_, err = c.spaceStart(base, t.Device, t.Inode, r.FreeBefore+r.AllocatedBytes, keys)
		}
		if err != nil {
			_ = f.Close()
			return results, err
		}
		r.Status = "attempt"
		if err = write(*r); err != nil {
			_ = f.Close()
			return results, err
		}
		for _, file := range t.Files {
			s, e2 := privateFile(f, file.Name)
			err = e2
			if err == nil && (uint64(s.Dev) != file.Device || s.Ino != file.Inode || s.Mode&unix.S_IFMT != unix.S_IFREG || s.Nlink != 1 || s.Size != file.Size) {
				err = fmt.Errorf("file identity changed: %s", file.Name)
			}
			if err == nil {
				err = unlink(int(f.Fd()), file.Name, 0)
			}
			if err != nil {
				break
			}
			r.FilesRemoved++
		}
		if err == nil {
			err = unix.Fsetxattr(int(f.Fd()), maintenanceAttribute, []byte(fmt.Sprintf("%d:%d", t.Device, t.Inode)), 0)
		}
		if err == nil {
			err = f.Sync()
		}
		var statErr error
		r.FreeAfter, statErr = freeBytes(f)
		_ = f.Close()
		err = errors.Join(err, statErr)
		if err == nil && account {
			err = c.checkSpace(filepath.Join(t.Root, strings.Split(t.Relative, "/")[0]))
		}
		r.Status = "private_files_unlinked"
		if err != nil {
			r.Status = "partial_or_space_unconfirmed"
			r.Error = err.Error()
		}
		logErr := write(*r)
		_, displayErr := fmt.Fprintf(out, "%s %s removed=%d allocated=%d free_before=%d free_after=%d\n", r.Status, r.Path, r.FilesRemoved, r.AllocatedBytes, r.FreeBefore, r.FreeAfter)
		logErr = errors.Join(logErr, displayErr)
		if err != nil || logErr != nil {
			return results, errors.Join(err, logErr)
		}
	}
	return results, nil
}

func allocatedFiles(files []LegacyFile) uint64 {
	var n uint64
	for _, f := range files {
		n += uint64(f.Blocks) * 512
	}
	return n
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) > 8*1024*1024 {
		return fmt.Errorf("oversized input")
	}
	return decodeStrict(b, v)
}

func WriteNewJSON(path string, v any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	e := json.NewEncoder(f)
	e.SetIndent("", "  ")
	if err = e.Encode(v); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	d, err := openDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
