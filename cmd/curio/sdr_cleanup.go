package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mitchellh/go-homedir"
	"github.com/urfave/cli/v2"

	"github.com/filecoin-project/curio/deps"
	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

func startPersonalSDRCleanup(repo string) error {
	on, err := sdrscratch.PersonalCleanupEnabled()
	if err != nil || !on {
		return err
	}
	roots, err := registeredCleanupRoots(repo, false)
	if err != nil {
		return err
	}
	return sdrscratch.StartPersonal(roots)
}

func registeredCleanupRoots(repo string, sealingOnly bool) ([]string, error) {
	ls := &paths.BasicLocalStorage{PathToJSON: filepath.Join(repo, "storage.json")}
	cfg, err := ls.GetStorage()
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, p := range cfg.StoragePaths {
		root, err := homedir.Expand(p.Path)
		if err != nil {
			return nil, err
		}
		root, err = filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		if sealingOnly {
			b, err := os.ReadFile(filepath.Join(root, paths.MetaFile))
			if err != nil {
				return nil, err
			}
			var meta storiface.LocalStorageMeta
			if err = json.Unmarshal(b, &meta); err != nil {
				return nil, err
			}
			if !meta.CanSeal {
				continue
			}
		}
		roots = append(roots, root)
	}
	return roots, nil
}

var sdrCleanupCmd = &cli.Command{
	Name:  "sdr-cleanup",
	Usage: "Personal-only exact-list SDR maintenance; no sector GC",
	Subcommands: []*cli.Command{
		{Name: "preview", Flags: []cli.Flag{
			&cli.StringFlag{Name: "paths", Required: true},
			&cli.StringFlag{Name: "plan", Required: true},
			&cli.StringSliceFlag{Name: "unit", Required: true, Usage: "Complete accessor service inventory (repeat)"},
		}, Action: func(c *cli.Context) error {
			if c.NArg() != 0 {
				return fmt.Errorf("unexpected arguments")
			}
			roots, err := registeredCleanupRoots(c.String(deps.FlagRepoPath), true)
			if err != nil {
				return err
			}
			cfg, err := sdrscratch.PersonalMaintenanceConfig(roots, c.StringSlice("unit"))
			if err != nil {
				return err
			}
			var targets []sdrscratch.LegacyTarget
			if err = sdrscratch.ReadJSON(c.String("paths"), &targets); err != nil {
				return err
			}
			h, b, err := sdrscratch.HostBoot()
			if err != nil {
				return err
			}
			p, err := sdrscratch.BuildLegacyPlan(cfg, targets, h, b)
			if err != nil {
				return err
			}
			bundle := sdrscratch.PersonalMaintenancePlan{Config: *cfg, Plan: *p}
			if err = json.NewEncoder(c.App.Writer).Encode(bundle); err != nil {
				return err
			}
			return sdrscratch.WriteNewJSON(c.String("plan"), bundle)
		}},
		{Name: "apply", Flags: []cli.Flag{
			&cli.StringFlag{Name: "plan", Required: true},
			&cli.StringFlag{Name: "journal", Required: true},
		}, Action: func(c *cli.Context) error {
			if c.NArg() != 0 {
				return fmt.Errorf("unexpected arguments")
			}
			var bundle sdrscratch.PersonalMaintenancePlan
			if err := sdrscratch.ReadJSON(c.String("plan"), &bundle); err != nil {
				return err
			}
			roots, err := registeredCleanupRoots(c.String(deps.FlagRepoPath), true)
			if err != nil {
				return err
			}
			var units []string
			for _, u := range bundle.Config.Units {
				units = append(units, u.Name)
			}
			cfg, err := sdrscratch.PersonalMaintenanceConfig(roots, units)
			if err != nil {
				return err
			}
			results, err := sdrscratch.ExecuteLegacy(cfg, &bundle.Plan, c.String("journal"), c.App.Reader, c.App.Writer)
			if outErr := json.NewEncoder(c.App.Writer).Encode(results); outErr != nil {
				return outErr
			}
			return err
		}},
	},
}
