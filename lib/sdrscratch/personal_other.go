//go:build !linux

package sdrscratch

import (
	"fmt"
	"os"
)

func startPersonal([]string) (*personalSession, error) {
	return nil, fmt.Errorf("personal SDR cleanup requires Linux cgroup v2/systemd; no uncontained fallback")
}

func personalPlatformBoundary(c *ManagedConfig, _ *os.File) (Boundary, error) {
	return platformBoundary(c)
}
