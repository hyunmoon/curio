//go:build linux

package sdrscratch

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type autoCapability struct {
	Version int
	PID     int
	Start   string
	Run     ManagedRun
}

func processStart(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndex(string(b), ") ")
	if end < 0 {
		return "", fmt.Errorf("invalid process stat")
	}
	f := strings.Fields(string(b)[end+2:])
	if len(f) <= 19 {
		return "", fmt.Errorf("short process stat")
	}
	return f[19], nil // field 22, after pid and parenthesized comm
}

func publishAutoCapability(s *personalSession, group *os.File, current string) error {
	h, boot, err := hostBoot()
	if err != nil {
		return err
	}
	d, i, err := identity(group)
	if err != nil {
		return err
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		return err
	}
	c := autoCapability{1, os.Getpid(), start, ManagedRun{s.domain, h, boot, current, d, i}}
	return WriteNewJSON(filepath.Join(s.state, "auto-access-"+filepath.Base(current)+".json"), c)
}

// All known accessor subtrees are checked, including children surviving a Go
// parent. A live pre-protocol Curio process prevents legacy adoption. Neither
// process age nor the absence of open FDs substitutes for this check.
func autoParticipants(c *ManagedConfig) error {
	h, boot, err := hostBoot()
	if err != nil || h != c.Host {
		return fmt.Errorf("auto cleanup host evidence: %w", err)
	}
	entries, err := os.ReadDir(c.StateDir)
	if err != nil {
		return err
	}
	valid := map[string]autoCapability{}
	byPID := map[int]autoCapability{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "auto-access-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var a autoCapability
		path := filepath.Join(c.StateDir, e.Name())
		if err := readPrivateJSON(path, &a); err != nil {
			return fmt.Errorf("auto cleanup capability %q: %w", path, err)
		}
		if a.Version != 1 || a.Run.Host != h || a.Run.Domain != c.Domain {
			return fmt.Errorf("invalid automatic access capability")
		}
		if a.Run.Boot != boot {
			continue
		}
		start, err := processStart(a.PID)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if start != a.Start {
			continue
		}
		cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", a.PID))
		if err != nil {
			return err
		}
		if !strings.Contains(string(cg), "0::"+a.Run.Cgroup+"\n") {
			return fmt.Errorf("participating process migrated")
		}
		f, err := openCgroup(a.Run.Cgroup)
		if err != nil {
			return err
		}
		d, i, err := identity(f)
		_ = f.Close()
		if err != nil || d != a.Run.Device || i != a.Run.Inode {
			return fmt.Errorf("participating cgroup identity changed")
		}
		valid[a.Run.Cgroup], byPID[a.PID] = a, a
	}
	// Discover unconverted Curio parents without hard-coded unit names or paths.
	// This is not discovery of arbitrary external programs with future filesystem
	// access: such access remains outside the managed local-storage contract.
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, p := range procs {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		if _, ok := byPID[pid]; ok {
			continue
		}
		info, err := buildinfo.ReadFile(filepath.Join("/proc", p.Name(), "exe"))
		if err != nil {
			if errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EIO) {
				return fmt.Errorf("cannot inspect process %d executable: %w", pid, err)
			}
			continue
		} // kernel threads and non-Go native executables
		if info.Main.Path == "github.com/filecoin-project/curio" || strings.HasPrefix(info.Path, "github.com/filecoin-project/curio/") {
			// `curio ffi` is a child of the participating worker, not a second
			// unconverted service. Its live parent holds the sector gate; if the
			// parent dies the populated subtree below is rejected instead.
			cg, e := os.ReadFile(filepath.Join("/proc", p.Name(), "cgroup"))
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			participating := false
			for group := range valid {
				if strings.Contains(string(cg), "0::"+group+"\n") {
					participating = true
					break
				}
			}
			if participating {
				continue
			}
			return fmt.Errorf("unconverted Curio process %d: legacy adoption deferred", pid)
		}
	}
	// A parent can die before its storage registration is published. Discover
	// managed subtrees host-wide too, so a surviving native child in such a run
	// is not missed merely because this storage's observed unit list is short.
	if err := filepath.WalkDir(cgroupRoot, func(path string, e os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "curio-sdr-") {
			return nil
		}
		cg := strings.TrimPrefix(path, cgroupRoot)
		f, err := openCgroup(cg)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		live, err := populated(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if _, ok := valid[cg]; live && !ok {
			return fmt.Errorf("unconverted or parentless managed execution: %s", cg)
		}
		return filepath.SkipDir
	}); err != nil {
		return err
	}
	for _, u := range c.Units {
		f, err := openCgroup(u.Cgroup)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		pids, err := readAt(f, "cgroup.procs")
		_ = f.Close()
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(pids)) != "" {
			return fmt.Errorf("unconverted accessor in %s", u.Name)
		}
		children, err := os.ReadDir(cgroupRoot + u.Cgroup)
		if err != nil {
			return err
		}
		for _, child := range children {
			if !child.IsDir() {
				continue
			}
			cg := u.Cgroup + "/" + child.Name()
			f, err := openCgroup(cg)
			if err != nil {
				return err
			}
			live, err := populated(f)
			_ = f.Close()
			if err != nil {
				return err
			}
			if live {
				if _, ok := valid[cg]; !ok {
					return fmt.Errorf("unconverted or parentless native subtree %s", cg)
				}
			}
		}
	}
	return nil
}
