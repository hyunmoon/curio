// sdr-scratch is a PERSONAL-ONLY host-local helper. It has no Curio RPC, DB,
// sector GC or service mutation client. Linux execution requires root.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/filecoin-project/curio/lib/sdrscratch"
)

type names []string

func (n *names) String() string     { return fmt.Sprint([]string(*n)) }
func (n *names) Set(v string) error { *n = append(*n, v); return nil }

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("expected enroll, run, preview or apply")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	config := f.String("config", "", "root-owned managed config JSON")
	paths := f.String("paths", "", "exact root/relative path list JSON (preview)")
	plan := f.String("plan", "", "plan JSON")
	journal := f.String("journal", "", "new durable journal outside storage (apply)")
	state := f.String("state", "", "existing root-owned state directory outside storage (enroll)")
	var roots, units names
	f.Var(&roots, "storage", "exact storage root (repeat for enroll)")
	f.Var(&units, "unit", "every storage accessor service (repeat for enroll)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] == "run" {
		return sdrscratch.RunManaged(*config, f.Args())
	}
	if args[0] == "enroll" {
		if len(f.Args()) != 0 {
			return fmt.Errorf("unexpected arguments")
		}
		c, err := sdrscratch.CaptureManagedConfig(*state, roots, units)
		if err != nil {
			return err
		}
		return sdrscratch.WriteNewJSON(*config, c)
	}
	if len(f.Args()) != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	c, err := sdrscratch.LoadManagedConfig(*config)
	if err != nil {
		return err
	}
	switch args[0] {
	case "preview":
		var targets []sdrscratch.LegacyTarget
		if err = sdrscratch.ReadJSON(*paths, &targets); err != nil {
			return err
		}
		h, b, err := sdrscratch.HostBoot()
		if err != nil {
			return err
		}
		p, err := sdrscratch.BuildLegacyPlan(c, targets, h, b)
		if err != nil {
			return err
		}
		if err = json.NewEncoder(os.Stdout).Encode(p); err != nil {
			return err
		}
		return sdrscratch.WriteNewJSON(*plan, p)
	case "apply":
		var p sdrscratch.LegacyPlan
		if err = sdrscratch.ReadJSON(*plan, &p); err != nil {
			return err
		}
		r, err := sdrscratch.ExecuteLegacy(c, &p, *journal, os.Stdin, os.Stdout)
		_ = json.NewEncoder(os.Stdout).Encode(r)
		return err
	default:
		return fmt.Errorf("unknown operation; no automatic target discovery")
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
