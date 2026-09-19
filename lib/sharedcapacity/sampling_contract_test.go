package sharedcapacity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A checksum and a cross-process lock cannot make an unsafe sampler safe. These
// negative controls deliberately lie to the engine, execute a REAL assertion
// failure in a child test process, and retain that failure in verbose output.
// They guard against presenting accounting-model PASS as filesystem safety.
func TestUnsafeSamplerControl(t *testing.T) {
	mode := os.Getenv("CURIO_CAPACITY_UNSAFE_SAMPLE")
	if mode == "" {
		return
	}
	a, _ := fixture(t, 2*modelEnvelope+fixturePolicy.Margin)
	ctx := context.Background()
	if _, _, err := a.Reserve(ctx, "A", "boot", modelEnvelope); err != nil {
		t.Fatal(err)
	}
	// A owns E materialized bytes. Untracked use or an extra staging copy
	// consumes another E, leaving only M actual free.
	actualFree := fixturePolicy.Margin
	switch mode {
	case "stale-free-new-credit":
		a.sample = func(context.Context, State) (Sample, error) {
			return Sample{Identity: fixtureID, Free: 2*modelEnvelope + fixturePolicy.Margin, Credit: map[string]int64{"A": modelEnvelope}}, nil
		}
	case "reflink-or-wrong-attempt-credit":
		// Those claimed bytes are not exclusively attributable to A: the
		// growing writer still owes E. Counting them as credit is unsound.
		actualFree = modelEnvelope + fixturePolicy.Margin
		a.sample = func(context.Context, State) (Sample, error) {
			return Sample{Identity: fixtureID, Free: actualFree, Credit: map[string]int64{"A": modelEnvelope}}, nil
		}
	default:
		t.Fatal("unexpected control mode")
	}
	_, _, err := a.Reserve(ctx, "B", "boot", modelEnvelope)
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("UNSAFE_SAMPLER_ASSERTION: %s admitted despite actual unpromised headroom below E; error=%v", mode, err)
	}
}

func TestUnsafeSamplerNegativeControlsFailAssertions(t *testing.T) {
	for _, mode := range []string{"stale-free-new-credit", "reflink-or-wrong-attempt-credit"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestUnsafeSamplerControl$", "-test.timeout=10s")
			cmd.Env = []string{"PATH=/usr/bin:/bin", "CURIO_CAPACITY_UNSAFE_SAMPLE=" + mode}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "UNSAFE_SAMPLER_ASSERTION:") {
				t.Fatalf("negative control did not fail its assertion: %v\n%s", err, out)
			}
			t.Logf("expected broken sampler assertion failure:\n%s", out)
		})
	}
}

func TestFreshConservativeSampleRejectsAndResumes(t *testing.T) {
	a, _ := fixture(t, 2*modelEnvelope+fixturePolicy.Margin)
	ctx := context.Background()
	token, _, err := a.Reserve(ctx, "A", "boot", modelEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx, "A", token); err != nil {
		t.Fatal(err)
	}
	if err := a.Returned(ctx, "A", token); err != nil {
		t.Fatal(err)
	}
	// The same sector owns known materialized bytes; unrelated/open-unlinked
	// data still occupies physical free space and receives NO budget credit.
	a.sample = func(context.Context, State) (Sample, error) {
		return Sample{Identity: fixtureID, Free: fixturePolicy.Margin, Credit: map[string]int64{"A": modelEnvelope}}, nil
	}
	if _, _, err := a.Reserve(ctx, "B", "boot", modelEnvelope); !errors.Is(err, ErrCapacity) {
		t.Fatal("unknown/open-unlinked bytes counted free", err)
	}
	// Loss of a cached file removes its credit. It does not remove the
	// commitment: the subsequent rewrite must still fit within its promise.
	a.sample = func(context.Context, State) (Sample, error) {
		return Sample{Identity: fixtureID, Free: modelEnvelope + fixturePolicy.Margin}, nil
	}
	if _, _, err := a.Reserve(ctx, "B", "boot", modelEnvelope); !errors.Is(err, ErrCapacity) {
		t.Fatal("lost file credit reused", err)
	}
	// Actual safe drain has returned physical space, not merely set a flag.
	a.sample = func(context.Context, State) (Sample, error) {
		return Sample{Identity: fixtureID, Free: 2*modelEnvelope + fixturePolicy.Margin}, nil
	}
	if _, _, err := a.Reserve(ctx, "B", "boot", modelEnvelope); err != nil {
		t.Fatal(err)
	}
}
