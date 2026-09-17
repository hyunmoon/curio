//go:build linux

package sdrscratch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

func taskExecutionIdentity(s *personalSession) (string, error) {
	h, boot, err := hostBoot()
	if err != nil {
		return "", err
	}
	cg, err := selfCgroup()
	if err != nil {
		return "", err
	}
	f, err := openCgroup(cg)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	d, i, err := identity(f)
	if err != nil {
		return "", err
	}
	r := ManagedRun{s.domain, h, boot, cg, d, i}
	if err = validateTaskExecution(s, r); err != nil {
		return "", err
	}
	b, err := json.Marshal(r)
	return string(b), err
}

func validateTaskExecution(s *personalSession, r ManagedRun) error {
	h, _, err := hostBoot()
	if err != nil {
		return err
	}
	if h != s.host || r.Host != h || r.Domain != s.domain || r.Inode == 0 ||
		filepath.Clean(r.Cgroup) != r.Cgroup || filepath.Dir(r.Cgroup) != s.unit.Cgroup {
		return fmt.Errorf("SDR execution host/domain/service/identity mismatch")
	}
	if _, err := uuid.Parse(r.Boot); err != nil {
		return err
	}
	prefix := "curio-sdr-" + s.domain + "-"
	name := filepath.Base(r.Cgroup)
	if !strings.HasPrefix(name, prefix) {
		return fmt.Errorf("unmanaged SDR execution")
	}
	id := strings.TrimPrefix(name, prefix)
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id {
		return fmt.Errorf("invalid SDR execution UUID")
	}
	return nil
}

func taskExecutionStopped(s *personalSession, raw string) (bool, error) {
	var r ManagedRun
	if err := decodeStrict([]byte(raw), &r); err != nil {
		return false, err
	}
	if err := validateTaskExecution(s, r); err != nil {
		return false, err
	}
	_, boot, err := hostBoot()
	if err != nil {
		return false, err
	}
	if boot != r.Boot {
		return true, nil
	}
	current, err := selfCgroup()
	if err != nil {
		return false, err
	}
	if current == r.Cgroup {
		return false, nil
	}
	f, err := openCgroup(r.Cgroup)
	// Same managed contract as scratch: UUID groups are never reused/reentered
	// and processes must not migrate out. Kernel removal requires an empty group.
	if os.IsNotExist(err) {
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
		return false, fmt.Errorf("SDR cgroup identity replaced")
	}
	live, err := populated(f)
	return !live, err
}
