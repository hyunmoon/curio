package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/filecoin-project/lotus/storage/sealer/fsutil"
)

const PERSONAL_STORAGE_PROFILE_ENV = "CURIO_PERSONAL_STORAGE_PROFILE"

// Personal profiles intentionally report virtual capacity, not physical free
// space. They are selected once per Local instance, outside cluster config.
type personalCapacityProfile struct {
	name     string
	root     string
	capacity int64
}

func parsePersonalCapacityProfile(name string) (*personalCapacityProfile, error) {
	switch name {
	case "":
		return nil, nil
	case "sdisk-4slot":
		return &personalCapacityProfile{name: name, root: "/sdisk/sealworker", capacity: 5600000000000}, nil
	case "sdisk-sm":
		return &personalCapacityProfile{name: name, root: "/sdisk_sm/sealworker", capacity: 16800000000000}, nil
	default:
		return nil, fmt.Errorf("invalid %s: expected sdisk-4slot, sdisk-sm, or unset", PERSONAL_STORAGE_PROFILE_ENV)
	}
}

func (p *personalCapacityProfile) matches(local string) bool {
	if p == nil {
		return false
	}
	clean := filepath.Clean(local)
	return clean == p.root || strings.HasPrefix(clean, p.root+string(os.PathSeparator))
}

func (p *personalCapacityProfile) stat(local string, stat fsutil.FsStat, usage func(string) (int64, error)) (fsutil.FsStat, error) {
	used, err := usage(local)
	if err != nil {
		return fsutil.FsStat{}, fmt.Errorf("personal virtual capacity disk usage: %w", err)
	}
	if used < 0 || stat.Reserved < 0 {
		return fsutil.FsStat{}, fmt.Errorf("negative personal virtual-capacity accounting")
	}
	fsAvail := int64(0)
	if used < p.capacity {
		fsAvail = p.capacity - used
	}
	available := int64(0)
	if stat.Reserved < fsAvail {
		available = fsAvail - stat.Reserved
	}
	stat.Capacity, stat.Max, stat.Used = p.capacity, p.capacity, used
	stat.FSAvailable, stat.Available = fsAvail, available
	return stat, nil
}
