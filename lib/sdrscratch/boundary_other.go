//go:build !linux

package sdrscratch

import "fmt"

func platformOpenFiles([]openIdentity) (bool, error) {
	return false, fmt.Errorf("open-inode accounting requires Linux host procfs")
}

func CaptureManagedConfig(string, []string, []string) (*ManagedConfig, error) {
	return nil, fmt.Errorf("enrollment requires Linux cgroup v2")
}

func platformBoundary(*ManagedConfig) (Boundary, error) {
	return nil, fmt.Errorf("personal managed discard requires Linux cgroup v2")
}
func MaintenanceGuard(*ManagedConfig) error {
	return fmt.Errorf("maintenance requires Linux cgroup v2")
}
func RunManaged(string, []string) error {
	return fmt.Errorf("managed launcher requires Linux cgroup v2")
}
func platformHostBoot() (string, string, error) {
	return "", "", fmt.Errorf("managed host identity requires Linux")
}
