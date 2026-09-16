//go:build linux

package sdrscratch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

func platformHostBoot() (string, string, error) { return hostBoot() }

// CaptureManagedConfig only reads identities. Capture while the inventoried
// units have their normal cgroups; enabling the mode requires maintenance.
func CaptureManagedConfig(state string, roots, units []string) (*ManagedConfig, error) {
	h, _, err := hostBoot()
	if err != nil {
		return nil, err
	}
	c := &ManagedConfig{Version: 1, Domain: uuid.NewString(), Host: h, StateDir: state}
	d, err := trustedDir(state)
	if err != nil {
		return nil, err
	}
	_ = d.Close()
	for _, root := range roots {
		f, err := openDir(root)
		if err != nil {
			return nil, err
		}
		dev, ino, e := identity(f)
		fs := supportedFS(f)
		_ = f.Close()
		if e != nil {
			return nil, e
		}
		if fs != nil {
			return nil, fs
		}
		var meta struct{ ID string }
		data, err := os.ReadFile(filepath.Join(root, "sectorstore.json"))
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &meta); err != nil {
			return nil, err
		}
		c.Storage = append(c.Storage, ManagedStorage{root, meta.ID, dev, ino})
	}
	for _, name := range units {
		if !unitName.MatchString(name) {
			return nil, fmt.Errorf("invalid unit")
		}
		p, err := unitProperties(name)
		if err != nil {
			return nil, err
		}
		c.Units = append(c.Units, ManagedUnit{name, p["ControlGroup"]})
	}
	if err = c.validate(); err != nil {
		return nil, err
	}
	for _, s := range c.Storage {
		if err = c.checkStorage(filepath.Join(s.Root, "cache")); err != nil {
			return nil, err
		}
	}
	return c, nil
}

type linuxBoundary struct {
	c       *ManagedConfig
	leaseFD int
}

func platformBoundary(c *ManagedConfig) (Boundary, error) { return &linuxBoundary{c: c}, nil }

func hostBoot() (string, string, error) {
	h, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return "", "", err
	}
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(h)), strings.TrimSpace(string(b)), nil
}

func selfCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "0::/") {
			return strings.TrimPrefix(line, "0::"), nil
		}
	}
	return "", fmt.Errorf("unified cgroup v2 required")
}

func openCgroup(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, fmt.Errorf("invalid cgroup path")
	}
	f, err := openDir(cgroupRoot + path)
	if err != nil {
		return nil, err
	}
	var st unix.Statfs_t
	if err = unix.Fstatfs(int(f.Fd()), &st); err == nil && st.Type != unix.CGROUP2_SUPER_MAGIC {
		err = fmt.Errorf("not cgroup v2")
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func readAt(dir *os.File, name string) ([]byte, error) {
	path := filepath.Join(dir.Name(), name)
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open kernel record %q: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	b, err := readKernelRecord(f)
	if err != nil {
		return nil, fmt.Errorf("read kernel record %q: %w", path, err)
	}
	return b, nil
}

// EOF terminates a record, including an empty direct-PID list. It is not
// execution-termination evidence; callers must still validate the contents
// and participating child cgroups. Bound the whole read, not just one Read.
func readKernelRecord(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, 4097))
	if err != nil {
		return nil, err
	}
	if len(b) > 4096 {
		return nil, fmt.Errorf("oversized kernel record")
	}
	return b, nil
}

func populated(f *os.File) (bool, error) {
	b, err := readAt(f, "cgroup.events")
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(string(b), "\n") {
		if l == "populated 0" {
			return false, nil
		}
		if l == "populated 1" {
			return true, nil
		}
	}
	return false, fmt.Errorf("missing populated kernel evidence in %q", filepath.Join(f.Name(), "cgroup.events"))
}

func (b *linuxBoundary) validateRun(base string, r ManagedRun) error {
	if err := b.c.checkStorage(base); err != nil {
		return err
	}
	h, _, err := hostBoot()
	if err != nil {
		return err
	}
	if h != b.c.Host || r.Host != h || r.Domain != b.c.Domain || r.Inode == 0 {
		return fmt.Errorf("managed host/domain/identity mismatch")
	}
	if _, err := uuid.Parse(r.Boot); err != nil {
		return err
	}
	name := filepath.Base(r.Cgroup)
	prefix := "curio-sdr-" + b.c.Domain + "-"
	if !strings.HasPrefix(name, prefix) {
		return fmt.Errorf("unmanaged process group")
	}
	id := strings.TrimPrefix(name, prefix)
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id {
		return fmt.Errorf("invalid process run identity")
	}
	for _, unit := range b.c.Units {
		if filepath.Dir(r.Cgroup) == unit.Cgroup {
			return nil
		}
	}
	return fmt.Errorf("process group not in enrolled service")
}

func (b *linuxBoundary) Current(base string) (*ManagedRun, error) {
	if err := b.c.checkSpace(base); err != nil {
		return nil, err
	}
	h, boot, err := hostBoot()
	if err != nil {
		return nil, err
	}
	p, err := selfCgroup()
	if err != nil {
		return nil, err
	}
	f, err := openCgroup(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	d, i, err := identity(f)
	if err != nil {
		return nil, err
	}
	r := &ManagedRun{Domain: b.c.Domain, Host: h, Boot: boot, Cgroup: p, Device: d, Inode: i}
	if err = b.validateRun(base, *r); err != nil {
		return nil, err
	}
	// The launcher passes an already-locked domain lease into the worker. It
	// remains open for the entire process. This is exclusion, NOT termination.
	fd, err := strconv.Atoi(os.Getenv("CURIO_SDR_DOMAIN_LEASE_FD"))
	if b.leaseFD >= 3 {
		fd, err = b.leaseFD, nil
	}
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("managed launcher lease missing")
	}
	lease, err := openLease(b.c)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lease.Close() }()
	var a, z unix.Stat_t
	if err = unix.Fstat(fd, &a); err != nil {
		return nil, err
	}
	if err = unix.Fstat(int(lease.Fd()), &z); err != nil {
		return nil, err
	}
	if a.Dev != z.Dev || a.Ino != z.Ino {
		return nil, fmt.Errorf("domain lease identity mismatch")
	}
	return r, nil
}

func (b *linuxBoundary) checkBase(base string) error           { return b.c.checkStorage(base) }
func (b *linuxBoundary) pinBase(base string) (*os.File, error) { return b.c.pinStorageBase(base) }
func (b *linuxBoundary) checkSpace(base string) error          { return b.c.checkSpace(base) }
func (b *linuxBoundary) accounting() *ManagedConfig            { return b.c }

func (b *linuxBoundary) Stopped(base string, r ManagedRun) (bool, error) {
	if err := b.validateRun(base, r); err != nil {
		return false, err
	}
	_, boot, err := hostBoot()
	if err != nil {
		return false, err
	}
	if boot != r.Boot {
		return true, nil
	} // same host, different kernel boot
	f, err := openCgroup(r.Cgroup)
	// Kernel removal requires an empty group. Run UUIDs are never reused or
	// re-entered; migrating processes out is forbidden by the managed contract.
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	d, i, err := identity(f)
	if err != nil {
		return false, err
	}
	if d != r.Device || i != r.Inode {
		return false, fmt.Errorf("cgroup identity replaced")
	}
	live, err := populated(f)
	return !live, err
}

func unitProperties(unit string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=ActiveState,SubState,UnitFileState,ControlGroup,Delegate,KillMode,SendSIGKILL,Type").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if ok {
			m[k] = v
		}
	}
	return m, nil
}

// MaintenanceGuard checks every explicitly inventoried accessor. The inventory
// must be complete and storage local; no PID, task or SQL inference is used.
func MaintenanceGuard(c *ManagedConfig) error {
	if c.Automatic {
		return fmt.Errorf("explicit complete accessor inventory required for legacy maintenance")
	}
	h, _, err := hostBoot()
	if err != nil {
		return err
	}
	if h != c.Host {
		return fmt.Errorf("wrong host")
	}
	for _, u := range c.Units {
		p, err := unitProperties(u.Name)
		if err != nil {
			return err
		}
		if (p["UnitFileState"] != "masked" && p["UnitFileState"] != "masked-runtime") || p["ActiveState"] != "inactive" {
			return fmt.Errorf("termination_required: %s must be masked and inactive", u.Name)
		}
		if p["ControlGroup"] != "" && p["ControlGroup"] != u.Cgroup {
			return fmt.Errorf("accessor cgroup changed: %s", u.Name)
		}
		f, err := openCgroup(u.Cgroup)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		live, e := populated(f)
		_ = f.Close()
		if e != nil {
			return e
		}
		if live {
			return fmt.Errorf("termination_required: %s subtree populated", u.Name)
		}
	}
	return nil
}

// RunManaged never kills by age and never changes the requested worker args,
// capacity profile, FFI backend or task parallelism. systemd owns service stop.
func RunManaged(config string, args []string) error {
	c, err := LoadManagedConfig(config)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("worker command required")
	}
	h, _, err := hostBoot()
	if err != nil {
		return err
	}
	if h != c.Host {
		return fmt.Errorf("wrong host")
	}
	parent, err := selfCgroup()
	if err != nil {
		return err
	}
	found := false
	for _, u := range c.Units {
		if u.Cgroup == parent {
			p, e := unitProperties(u.Name)
			if e != nil {
				return e
			}
			if p["ControlGroup"] != parent || p["Delegate"] != "yes" || p["KillMode"] != "control-group" || p["SendSIGKILL"] != "yes" {
				return fmt.Errorf("service requires Delegate=yes KillMode=control-group SendSIGKILL=yes")
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("launcher is not inside an enrolled service")
	}
	lease, err := AcquireDomain(c, false)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Close() }()
	pg, err := openCgroup(parent)
	if err != nil {
		return err
	}
	defer func() { _ = pg.Close() }()
	name := "curio-sdr-" + c.Domain + "-" + uuid.NewString()
	if err = unix.Mkdirat(int(pg.Fd()), name, 0755); err != nil {
		return err
	}
	group, err := openCgroup(parent + "/" + name)
	if err != nil {
		return err
	}
	defer func() { _ = group.Close() }()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = withoutEnv(os.Environ(), PersonalPolicyEnv, "CURIO_SDR_DOMAIN_LEASE_FD")
	cmd.Env = append(cmd.Env, PersonalPolicyEnv+"="+config, "CURIO_SDR_DOMAIN_LEASE_FD=3")
	cmd.ExtraFiles = []*os.File{lease}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(group.Fd())}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("atomic cgroup entry failed (no uncontained fallback): %w", err)
	}
	fmt.Fprintf(os.Stderr, "SDR discard domain=%s group=%s profile=%s\n", c.Domain, parent+"/"+name, os.Getenv("CURIO_PERSONAL_STORAGE_PROFILE"))
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-sig:
			_ = cmd.Process.Signal(s)
		case err := <-done:
			live, e := populated(group)
			if e != nil {
				return e
			}
			if live {
				return fmt.Errorf("termination_required: worker exited but descendants remain in %s; use enrolled service stop, no cleanup", name)
			}
			return err
		}
	}
}

func withoutEnv(env []string, keys ...string) []string {
	var out []string
	for _, v := range env {
		key, _, _ := strings.Cut(v, "=")
		skip := false
		for _, k := range keys {
			if key == k {
				skip = true
			}
		}
		if !skip {
			out = append(out, v)
		}
	}
	return out
}

// Supplementary open-inode check AFTER unlink. This is not termination proof.
// Run only in the host PID namespace with root-visible procfs; inaccessible
// processes fail closed. The managed contract forbids external FD transfer.
func platformOpenFiles(files []openIdentity) (bool, error) {
	keys := map[openIdentity]bool{}
	for _, f := range files {
		keys[f] = true
	}
	if len(keys) == 0 {
		return false, nil
	}
	self, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return false, err
	}
	init, err := os.Readlink("/proc/1/ns/pid")
	if err != nil {
		return false, err
	}
	if self != init {
		return false, fmt.Errorf("host PID namespace required")
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, p := range processes {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		dir := "/proc/" + p.Name() + "/fd"
		fds, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("cannot rule out open scratch handles: %w", err)
		}
		for _, fd := range fds {
			var st unix.Stat_t
			err := unix.Stat(dir+"/"+fd.Name(), &st)
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return false, err
			}
			if keys[openIdentity{uint64(st.Dev), st.Ino}] {
				return true, nil
			}
		}
	}
	return false, nil
}
