//go:build linux

package sdrscratch

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func personalPlatformBoundary(c *ManagedConfig, lease *os.File) (Boundary, error) {
	return &linuxBoundary{c: c, leaseFD: int(lease.Fd())}, nil
}

func personalService(parent string, p map[string]string) error {
	if !unitName.MatchString(filepath.Base(parent)) || p["ControlGroup"] != parent || p["Delegate"] != "yes" || p["KillMode"] != "control-group" || p["SendSIGKILL"] != "yes" || p["Type"] != "simple" {
		return fmt.Errorf("personal cleanup requires Type=simple Delegate=yes KillMode=control-group SendSIGKILL=yes; no service settings were changed")
	}
	return nil
}

func startPersonal(registered []string) (*personalSession, error) {
	return startPersonalAt(registered, personalStateDir)
}

// The state argument is only used by package-local disposable Linux fixtures.
func startPersonalAt(registered []string, state string) (*personalSession, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("personal cleanup requires root and host-wide procfs visibility")
	}
	if err := stateOutside(state, registered); err != nil {
		return nil, err
	}
	parent, err := selfCgroup()
	if err != nil {
		return nil, err
	}
	unit := filepath.Base(parent)
	p, err := unitProperties(unit)
	if err != nil {
		return nil, err
	}
	if err = personalService(parent, p); err != nil {
		return nil, err
	}
	pg, err := openCgroup(parent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pg.Close() }()
	// No existing child execution may be left outside the new lifetime. This
	// reads kernel membership, not process names, heartbeats or owner rows.
	err = filepath.WalkDir(cgroupRoot+parent, func(path string, e os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if e.Name() != "cgroup.procs" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, pid := range strings.Fields(string(b)) {
			if pid != strconv.Itoa(os.Getpid()) {
				return fmt.Errorf("pre-existing process outside new managed lifetime")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	h, _, err := hostBoot()
	if err != nil {
		return nil, err
	}
	// Constant management location, never inferred from a disk name/profile.
	if err = os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	d, err := trustedDir(state)
	if err != nil {
		return nil, err
	}
	stateDevice, _, err := identity(d)
	_ = d.Close()
	if err != nil {
		return nil, err
	}
	domain := uuid.NewSHA1(uuid.NameSpaceOID, []byte(h+"/curio-personal-sdr-cleanup-v1")).String()
	c := &ManagedConfig{Domain: domain, StateDir: state}
	lease, err := AcquireDomain(c, false)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = lease.Close()
		}
	}()
	name := "curio-sdr-" + domain + "-" + uuid.NewString()
	if err = unix.Mkdirat(int(pg.Fd()), name, 0755); err != nil {
		return nil, err
	}
	group, err := openCgroup(parent + "/" + name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = group.Close() }()
	// cgroup.procs moves the entire current thread group before any worker,
	// DB, port or SDR entry. MainPID/argv/env/cwd/UID/limits remain unchanged.
	fd, err := unix.Openat(int(group.Fd()), "cgroup.procs", unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	_, err = unix.Write(fd, []byte(strconv.Itoa(os.Getpid())))
	closeErr := unix.Close(fd)
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	current, err := selfCgroup()
	if err != nil || current != parent+"/"+name {
		return nil, fmt.Errorf("managed self entry not confirmed: %v", err)
	}
	s := &personalSession{state: state, host: h, domain: domain, unit: ManagedUnit{unit, parent}, lease: lease, roots: map[string]string{}}
	s.access = registrationAccess{dir: trustedDir, read: readPrivateJSON, fs: func(f *os.File) error {
		if err := supportedFS(f); err != nil {
			return err
		}
		dev, _, err := identity(f)
		if err != nil {
			return err
		}
		if dev == stateDevice {
			return fmt.Errorf("cleanup state must be on a filesystem separate from scratch; this root's SDR entry is blocked")
		}
		return nil
	}}
	ok = true
	fmt.Fprintf(os.Stderr, "Personal SDR cleanup: same-process managed lifetime=%s state=%s; local CanSeal roots will be registered independently; observed accessor membership is not a complete inventory\n", current, state)
	return s, nil
}
