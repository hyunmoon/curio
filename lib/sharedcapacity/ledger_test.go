package sharedcapacity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

var fixtureID = Identity{Host: "fixture-host", Filesystem: "fixture-fs"}
var fixturePolicy = Policy{Protocol: VERSION, Margin: 32 << 30}

const modelSector = int64(32 << 30)

// Deliberately a model bound, not a native-measured peak. The adapter must prove
// its chosen envelope before using the accounting engine in production.
const modelEnvelope = 16 * modelSector

func fixture(t *testing.T, free int64) (*Authority, string) {
	t.Helper()
	dir := t.TempDir()
	if err := Initialize(dir, fixtureID, fixturePolicy); err != nil {
		t.Fatal(err)
	}
	a, err := Open(dir, fixtureID, fixturePolicy, func(_ context.Context, _ State) (Sample, error) {
		return Sample{Identity: fixtureID, Free: free}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, dir
}

func TestBudgetLifecycle(t *testing.T) {
	a, _ := fixture(t, modelEnvelope+fixturePolicy.Margin)
	ctx := context.Background()
	token, _, err := a.Reserve(ctx, "sector", "boot", modelEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Reserve(ctx, "sector", "boot", modelEnvelope); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err := a.Cancel(ctx, "sector", "other-token"); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
	if err := a.Cancel(ctx, "sector", token); err != nil {
		t.Fatal(err)
	}
	token, _, err = a.Reserve(ctx, "sector", "boot", modelEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx, "sector", token); err != nil {
		t.Fatal(err)
	}
	if err := a.Cancel(ctx, "sector", token); !errors.Is(err, ErrIdentity) {
		t.Fatal("started request refunded", err)
	}
	if err := a.Returned(ctx, "sector", token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Reserve(ctx, "other-sector", "boot", modelEnvelope); !errors.Is(err, ErrCapacity) {
		t.Fatal("task completion returned residency", err)
	}
	// Existing follow-up is not charged again, even if unrelated physical use
	// leaves no headroom. It already owns its future-growth commitment.
	a.sample = func(context.Context, State) (Sample, error) { return Sample{Identity: fixtureID, Free: 0}, nil }
	next, _, err := a.Reserve(ctx, "sector", "boot", modelEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Cancel(ctx, "sector", next); err != nil {
		t.Fatal(err)
	}
	s, _, _ := a.Inspect(ctx)
	if s.Entries["sector"].Envelope != modelEnvelope {
		t.Fatal("follow-up cancel lost prior budget")
	}
	if err := a.Reconcile(ctx, "sector", token, 0, func(Entry) error { return nil }); !errors.Is(err, ErrIdentity) {
		t.Fatal("stale release accepted", err)
	}
	if err := a.Reconcile(ctx, "sector", next, 0, func(Entry) error { return errors.New("native termination unknown") }); err == nil {
		t.Fatal("unknown termination allowed")
	}
}

func TestMeasurementAndFaults(t *testing.T) {
	ctx := context.Background()
	a, dir := fixture(t, modelEnvelope*2+fixturePolicy.Margin)
	token, _, err := a.Reserve(ctx, "sector", "boot", modelEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	before, _, err := a.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, sample := range map[string]Sampler{
		"stale-stat-error": func(context.Context, State) (Sample, error) { return Sample{}, errors.New("stale stat") },
		"wrong-filesystem": func(context.Context, State) (Sample, error) {
			return Sample{Identity: Identity{"host", "replacement"}, Free: 1 << 60}, nil
		},
		"negative-free": func(context.Context, State) (Sample, error) { return Sample{Identity: fixtureID, Free: -1}, nil },
		"unrelated-credit": func(context.Context, State) (Sample, error) {
			return Sample{Identity: fixtureID, Free: 1 << 60, Credit: map[string]int64{"orphan": modelEnvelope}}, nil
		},
		"too-much-credit": func(context.Context, State) (Sample, error) {
			return Sample{Identity: fixtureID, Free: 1 << 60, Credit: map[string]int64{"sector": modelEnvelope + 1}}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			a.sample = sample
			if _, _, err := a.Reserve(ctx, "new", "boot", 1); err == nil {
				t.Fatal("unsafe observation admitted")
			}
		})
	}
	a.sample = func(_ context.Context, s State) (Sample, error) {
		delete(s.Entries, "sector")
		return Sample{Identity: fixtureID, Free: modelEnvelope*2 + fixturePolicy.Margin}, nil
	}
	a.fault = func(at string) error {
		if at == "before-rename" {
			return errors.New("fixture sync/rename error")
		}
		return nil
	}
	if _, _, err := a.Reserve(ctx, "new", "boot", 1); err == nil {
		t.Fatal("persistence error ignored")
	}
	a.fault = nil
	after, _, err := a.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed persistence or sampler mutated state")
	}
	if err := a.Start(ctx, "sector", token); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, fixtureID, Policy{Protocol: 2, Margin: 1}, a.sample); err == nil {
		t.Fatal("mixed version allowed")
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Reserve(ctx, "new", "boot", 1); err == nil {
		t.Fatal("corrupt ledger reset")
	}
	if err := Initialize(dir, fixtureID, fixturePolicy); err == nil {
		t.Fatal("corrupt ledger overwritten")
	}
}

// The helper uses a real second OS process and opens the same durable authority.
// No database, production environment, native code or live storage is involved.
func TestCapacitySubprocess(t *testing.T) {
	if os.Getenv("CURIO_CAPACITY_TEST_CHILD") != "1" {
		return
	}
	dir := os.Getenv("CURIO_CAPACITY_TEST_DIR")
	free, err := strconv.ParseInt(os.Getenv("CURIO_CAPACITY_TEST_FREE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	a, err := Open(dir, fixtureID, fixturePolicy, func(context.Context, State) (Sample, error) { return Sample{Identity: fixtureID, Free: free}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	name := os.Getenv("CURIO_CAPACITY_TEST_NAME")
	if err := os.WriteFile(filepath.Join(dir, name+".ready"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier timeout")
		}
		time.Sleep(time.Millisecond)
	}
	if crash := os.Getenv("CURIO_CAPACITY_TEST_CRASH"); crash != "" {
		a.fault = func(at string) error {
			if at == crash {
				p, _ := os.FindProcess(os.Getpid())
				_ = p.Kill()
				// Signal delivery can occur after Kill returns. Never let this
				// goroutine pass the injected persistence boundary meanwhile.
				select {}
			}
			return nil
		}
	}
	count, _ := strconv.Atoi(os.Getenv("CURIO_CAPACITY_TEST_COUNT"))
	accepted := 0
	for i := 0; i < count; i++ {
		token, _, err := a.Reserve(context.Background(), fmt.Sprintf("%s-%d", name, i), "boot", modelEnvelope)
		if errors.Is(err, ErrCapacity) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Start(context.Background(), fmt.Sprintf("%s-%d", name, i), token); err != nil {
			t.Fatal(err)
		}
		accepted++
	}
	if err := os.WriteFile(filepath.Join(dir, name+".result"), []byte(strconv.Itoa(accepted)), 0600); err != nil {
		t.Fatal(err)
	}
}

func runProcesses(t *testing.T, dir string, free int64, counts []int, crash string) []int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var cmds []*exec.Cmd
	logs := make([]strings.Builder, len(counts))
	for i, n := range counts {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCapacitySubprocess$", "-test.timeout=10s")
		cmd.Env = []string{"PATH=/usr/bin:/bin", "CURIO_CAPACITY_TEST_CHILD=1", "CURIO_CAPACITY_TEST_DIR=" + dir, "CURIO_CAPACITY_TEST_FREE=" + strconv.FormatInt(free, 10), "CURIO_CAPACITY_TEST_NAME=p" + strconv.Itoa(i), "CURIO_CAPACITY_TEST_COUNT=" + strconv.Itoa(n), "CURIO_CAPACITY_TEST_CRASH=" + crash}
		cmd.Stdout = &logs[i]
		cmd.Stderr = &logs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}
	deadline := time.Now().Add(5 * time.Second)
	for i := range cmds {
		for {
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("p%d.ready", i))); err == nil {
				break
			}
			if time.Now().After(deadline) {
				for _, cmd := range cmds {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
				t.Fatal("child ready timeout")
			}
			time.Sleep(time.Millisecond)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	results := make([]int, len(cmds))
	for i, cmd := range cmds {
		err := cmd.Wait()
		if crash != "" {
			if err == nil {
				t.Fatal("crash did not happen")
			}
			continue
		}
		if err != nil {
			t.Fatal(err, logs[i].String())
		}
		b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("p%d.result", i)))
		if err != nil {
			t.Fatal(err)
		}
		results[i], err = strconv.Atoi(string(b))
		if err != nil {
			t.Fatal(err)
		}
	}
	return results
}

func TestMultipleProcessAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		free   int64
		counts []int
		want   int
	}{
		{"last-slot", modelEnvelope + fixturePolicy.Margin, []int{1, 1}, 1},
		{"enough-for-both", 2*modelEnvelope + fixturePolicy.Margin, []int{1, 1}, 2},
		{"single-two-processes", 3200000000000, []int{4, 0}, 4},
		{"dual-four-processes", 12800000000000, []int{6, 0, 6, 0}, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, dir := fixture(t, tc.free)
			results := runProcesses(t, dir, tc.free, tc.counts, "")
			total := 0
			for _, n := range results {
				total += n
			}
			if total != tc.want {
				t.Fatalf("accepted %v want %d", results, tc.want)
			}
			if tc.want == 12 && (results[0] != 6 || results[2] != 6) {
				t.Fatal("asymmetric admission", results)
			}
			s, d, err := a.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Entries) != tc.want || d.Future != int64(tc.want)*modelEnvelope {
				t.Fatal(s, d)
			}
		})
	}
}

func TestCrashDoesNotRefundBudget(t *testing.T) {
	for _, at := range []string{"before-write", "before-rename", "after-rename"} {
		t.Run(at, func(t *testing.T) {
			a, dir := fixture(t, modelEnvelope+fixturePolicy.Margin)
			runProcesses(t, dir, modelEnvelope+fixturePolicy.Margin, []int{1}, at)
			s, _, err := a.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if at == "after-rename" {
				want = 1
			}
			if len(s.Entries) != want {
				t.Fatal("crash changed accounting", s)
			}
			if want == 1 {
				if _, _, err := a.Reserve(context.Background(), "after-restart", "new-boot", modelEnvelope); !errors.Is(err, ErrCapacity) {
					t.Fatal("restart refunded uncertain budget", err)
				}
			}
		})
	}
}

func TestResidencyBackpressureAndRecovery(t *testing.T) {
	for _, stage := range []string{"TreeRC", "PreCommit", "WaitSeed", "PoRep", "Finalize", "MoveStorage"} {
		t.Run(stage, func(t *testing.T) {
			a, _ := fixture(t, 2*modelEnvelope+fixturePolicy.Margin)
			ctx := context.Background()
			tokens := map[string]string{}
			for _, key := range []string{"A", "B"} {
				token, _, err := a.Reserve(ctx, key, "boot", modelEnvelope)
				if err != nil {
					t.Fatal(err)
				}
				tokens[key] = token
				if err := a.Start(ctx, key, token); err != nil {
					t.Fatal(err)
				}
				if err := a.Returned(ctx, key, token); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := a.Reserve(ctx, "new", "boot", modelEnvelope); !errors.Is(err, ErrCapacity) {
				t.Fatal("stalled residency uncharged")
			}
			if err := a.Reconcile(ctx, "A", tokens["A"], 0, func(e Entry) error {
				if e.State != "resident" {
					return errors.New("writer not returned")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := a.Reserve(ctx, "new", "boot", modelEnvelope); err != nil {
				t.Fatal("did not resume", err)
			}
			s, _, _ := a.Inspect(ctx)
			if s.Entries["B"].Envelope != modelEnvelope {
				t.Fatal("released other sector")
			}
		})
	}
}

func TestNoDoubleCountWithCertifiedMaterialization(t *testing.T) {
	a, _ := fixture(t, 2*modelEnvelope+fixturePolicy.Margin)
	ctx := context.Background()
	if _, _, err := a.Reserve(ctx, "A", "boot", modelEnvelope); err != nil {
		t.Fatal(err)
	}
	a.sample = func(context.Context, State) (Sample, error) {
		return Sample{Identity: fixtureID, Free: modelEnvelope + fixturePolicy.Margin, Credit: map[string]int64{"A": modelEnvelope}}, nil
	}
	if _, _, err := a.Reserve(ctx, "B", "boot", modelEnvelope); err != nil {
		t.Fatal("materialized bytes double charged", err)
	}
	_, d, err := a.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Future != modelEnvelope {
		t.Fatal(d)
	}
}

func TestRecordRoundtrip(t *testing.T) {
	a, _ := fixture(t, modelEnvelope)
	s, _, _ := a.Inspect(context.Background())
	b, e := json.Marshal(s)
	if e != nil {
		t.Fatal(e)
	}
	var got State
	if e = json.Unmarshal(b, &got); e != nil || !reflect.DeepEqual(got, s) {
		t.Fatal(e)
	}
}
