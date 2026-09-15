package sdrscratch

import (
	"fmt"
	"path/filepath"
	"sort"
)

type PersonalMaintenancePlan struct {
	Config ManagedConfig
	Plan   LegacyPlan
}

// Units are an explicit operator inventory, not inferred from auto enrollment.
// This only reads existing registration; it never creates termination evidence.
func PersonalMaintenanceConfig(roots, units []string) (*ManagedConfig, error) {
	h, _, err := HostBoot()
	if err != nil {
		return nil, err
	}
	s := &personalSession{state: personalStateDir}
	var result *ManagedConfig
	known := map[string]ManagedUnit{}
	seen := map[string]bool{}
	for _, root := range roots {
		name, storage, err := s.configPath(root)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		c, err := LoadManagedConfig(name)
		if err != nil {
			return nil, err
		}
		if !c.Automatic || len(c.Storage) != 1 || c.Storage[0].Root != root || c.Storage[0].Device != storage.Device || c.Storage[0].Inode != storage.Inode || c.Host != h || c.StateDir != personalStateDir {
			return nil, fmt.Errorf("registered storage identity changed")
		}
		if result == nil {
			result = &ManagedConfig{Version: 1, Host: h, Domain: c.Domain, StateDir: c.StateDir}
		}
		if result.Domain != c.Domain {
			return nil, fmt.Errorf("incompatible storage domains")
		}
		result.Storage = append(result.Storage, c.Storage...)
		for _, u := range c.Units {
			if old, ok := known[u.Name]; ok && old != u {
				return nil, fmt.Errorf("accessor identity changed")
			}
			known[u.Name] = u
		}
	}
	if result == nil || len(units) == 0 {
		return nil, fmt.Errorf("registered sealing storage and explicit complete accessor inventory required")
	}
	selected := map[string]bool{}
	for _, name := range units {
		u, ok := known[name]
		if !ok || selected[name] {
			return nil, fmt.Errorf("unit missing enrollment or duplicate: %s", name)
		}
		selected[name] = true
		result.Units = append(result.Units, u)
	}
	for name := range known {
		if !selected[name] {
			return nil, fmt.Errorf("accessor omitted from maintenance inventory: %s", name)
		}
	}
	sort.Slice(result.Storage, func(i, j int) bool { return result.Storage[i].Root < result.Storage[j].Root })
	sort.Slice(result.Units, func(i, j int) bool { return result.Units[i].Name < result.Units[j].Name })
	for _, root := range result.Storage {
		if err = result.checkStorage(filepath.Join(root.Root, "cache")); err != nil {
			return nil, err
		}
	}
	return result, result.validate()
}
