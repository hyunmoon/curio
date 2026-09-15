//go:build !linux

package sdrscratch

import "fmt"

func autoParticipants(*ManagedConfig) error {
	return fmt.Errorf("automatic legacy adoption requires Linux process/cgroup evidence")
}
