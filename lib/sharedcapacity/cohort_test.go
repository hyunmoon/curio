package sharedcapacity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests execute independent SDR/Trees ROLE MODELS, not Curio workers or
// native proofs. Byte values are type-8-sized, while materialization is supplied
// by a serialized fixture accounting oracle. It is not a production sampler.
func modelSampler(dir string, capacity int64) Sampler {
	return func(_ context.Context, s State) (Sample, error) {
		v := Sample{Identity: fixtureID, Free: capacity, Credit: map[string]int64{}}
		files, err := filepath.Glob(filepath.Join(dir, "bytes-*"))
		if err != nil {
			return v, err
		}
		for _, file := range files {
			b, err := os.ReadFile(file)
			if err != nil {
				return v, err
			}
			n, err := strconv.ParseInt(string(b), 10, 64)
			if err != nil || n < 0 {
				return v, errors.New("invalid model materialization")
			}
			v.Free -= n
			key := strings.TrimPrefix(filepath.Base(file), "bytes-")
			if _, ok := s.Entries[key]; ok {
				v.Credit[key] = n
			}
		}
		return v, nil
	}
}

// modelMaterialize's lock is for the test oracle only. The native integration
// must NOT lock an authority while writing a sector or walking a large tree.
func modelMaterialize(a *Authority, dir, key, token string, n int64) error {
	return a.lock(context.Background(), func() error {
		s, err := a.read()
		if err != nil {
			return err
		}
		e, ok := s.Entries[key]
		if !ok || e.Token != token || e.State != "running" || n > e.Envelope {
			return ErrIdentity
		}
		return os.WriteFile(filepath.Join(dir, "bytes-"+key), []byte(strconv.FormatInt(n, 10)), 0600)
	})
}

func waitModelFile(path string) error {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return fmt.Errorf("cohort barrier timed out: %s", filepath.Base(path))
}

func TestCohortRoleProcess(t *testing.T) {
	role := os.Getenv("CURIO_CAPACITY_ROLE")
	if role == "" {
		return
	}
	dir := os.Getenv("CURIO_CAPACITY_TEST_DIR")
	capacity, err := strconv.ParseInt(os.Getenv("CURIO_CAPACITY_TEST_FREE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	count, err := strconv.Atoi(os.Getenv("CURIO_CAPACITY_TEST_COUNT"))
	if err != nil {
		t.Fatal(err)
	}
	pair := os.Getenv("CURIO_CAPACITY_PAIR")
	a, err := Open(dir, fixtureID, fixturePolicy, modelSampler(dir, capacity))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	ctx := context.Background()
	for cycle := 0; cycle < 3; cycle++ {
		prefix := fmt.Sprintf("%s-%d", pair, cycle)
		handoff := filepath.Join(dir, "handoff-"+prefix)
		drained := filepath.Join(dir, "drained-"+prefix)
		switch role {
		case "sdr":
			tokens := map[string]string{}
			for i := 0; i < count; i++ {
				key := fmt.Sprintf("%s-%d", prefix, i)
				token, _, err := a.Reserve(ctx, key, "boot", modelEnvelope)
				if err != nil {
					t.Fatalf("healthy %s admission: %v", key, err)
				}
				if err := a.Start(ctx, key, token); err != nil {
					t.Fatal(err)
				}
				tokens[key] = token
			}
			// All slots own active permits at once, without holding a global
			// lock during their modeled work. A Trees process also remains live.
			for key, token := range tokens {
				if err := modelMaterialize(a, dir, key, token, 11*modelSector); err != nil {
					t.Fatal(err)
				}
				if err := a.Returned(ctx, key, token); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(handoff, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := waitModelFile(drained); err != nil {
				t.Fatal(err)
			}
		case "trees":
			if err := waitModelFile(handoff); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < count; i++ {
				key := fmt.Sprintf("%s-%d", prefix, i)
				token, d, err := a.Reserve(ctx, key, "boot", modelEnvelope)
				if err != nil {
					t.Fatal(err)
				}
				if d.Additional != 0 {
					t.Fatal("cross-role double reservation", d)
				}
				if err := a.Start(ctx, key, token); err != nil {
					t.Fatal(err)
				}
				if err := modelMaterialize(a, dir, key, token, 15*modelSector); err != nil {
					t.Fatal(err)
				}
				if err := a.Returned(ctx, key, token); err != nil {
					t.Fatal(err)
				}
				if err := a.Reconcile(ctx, key, token, 0, func(e Entry) error {
					if e.State != "resident" {
						return ErrBusy
					}
					// Fixture-only oracle: no native FD, staging copy or future
					// writer exists. Removing this counter models confirmed drain.
					return os.Remove(filepath.Join(dir, "bytes-"+key))
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(drained, nil, 0600); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("unknown role")
		}
	}
}

func TestHealthyCohortHandoffDrainRefill(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capacity     int64
		pairs, slots int
	}{
		{"single", 3200000000000, 1, 4},
		{"dual", 12800000000000, 2, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := Initialize(dir, fixtureID, fixturePolicy); err != nil {
				t.Fatal(err)
			}
			a, err := Open(dir, fixtureID, fixturePolicy, modelSampler(dir, tc.capacity))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = a.Close() }()
			// One persistent post-SDR sector: no assumed WaitSeed deadline and no
			// automatic timeout-based release. The model is not an assertion that
			// production always has exactly one such sector.
			token, _, err := a.Reserve(context.Background(), "older-resident", "boot", modelEnvelope)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Start(context.Background(), "older-resident", token); err != nil {
				t.Fatal(err)
			}
			if err := modelMaterialize(a, dir, "older-resident", token, 15*modelSector); err != nil {
				t.Fatal(err)
			}
			if err := a.Returned(context.Background(), "older-resident", token); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			logs := make([]strings.Builder, tc.pairs*2)
			var cmds []*exec.Cmd
			for pair := 0; pair < tc.pairs; pair++ {
				for _, role := range []string{"sdr", "trees"} {
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCohortRoleProcess$", "-test.timeout=20s")
					cmd.Env = []string{"PATH=/usr/bin:/bin", "CURIO_CAPACITY_ROLE=" + role, "CURIO_CAPACITY_PAIR=" + strconv.Itoa(pair), "CURIO_CAPACITY_TEST_DIR=" + dir, "CURIO_CAPACITY_TEST_FREE=" + strconv.FormatInt(tc.capacity, 10), "CURIO_CAPACITY_TEST_COUNT=" + strconv.Itoa(tc.slots)}
					cmd.Stdout = &logs[len(cmds)]
					cmd.Stderr = &logs[len(cmds)]
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					cmds = append(cmds, cmd)
				}
			}
			var failures []string
			for i, cmd := range cmds {
				if err := cmd.Wait(); err != nil {
					failures = append(failures, fmt.Sprint(err, logs[i].String()))
				}
			}
			if len(failures) > 0 {
				t.Fatal(strings.Join(failures, "\n"))
			}
			s, d, err := a.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Entries) != 1 || s.Entries["older-resident"].Token != token || d.Future != modelSector {
				t.Fatal("drain lost or duplicated another residency", s, d)
			}
			t.Logf("processes=%d SDR slots=%d+%d cycles=3 persistent older sectors=1 model capacity=%d", tc.pairs*2, tc.slots, (tc.pairs-1)*tc.slots, tc.capacity)
		})
	}
}
