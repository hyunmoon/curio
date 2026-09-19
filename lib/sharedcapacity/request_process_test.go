package sharedcapacity

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRequestPublisherProcess(t *testing.T) {
	dir := os.Getenv("CURIO_CAPACITY_REQUEST_TEST_DIR")
	if dir == "" {
		return
	}
	var r Request
	if err := json.Unmarshal([]byte(os.Getenv("CURIO_CAPACITY_REQUEST_JSON")), &r); err != nil {
		t.Fatal(err)
	}
	a, err := Open(dir, fixtureID, fixturePolicy, func(context.Context, State) (Sample, error) {
		return Sample{Identity: fixtureID, Free: 20 * modelEnvelope}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a.fault = func(at string) error {
		if at == "after-rename" {
			if err := unix.Kill(os.Getpid(), unix.SIGKILL); err != nil {
				return err
			}
			select {}
		}
		return nil
	}
	_, err = a.Apply(context.Background(), r, func(Entry) error { return nil })
	t.Fatal("publisher should have exited at fault boundary", err)
}

func TestRequestRecoveryAcrossProcessExit(t *testing.T) {
	for _, op := range []string{"reserve", "start", "returned", "reconcile"} {
		t.Run(op, func(t *testing.T) {
			a, dir := fixture(t, 20*modelEnvelope)
			r := Request{Client: "persisted-client", Sequence: 1, Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: modelEnvelope}
			setup := func() {
				t.Helper()
				o, err := a.Apply(context.Background(), r, nil)
				if err != nil {
					t.Fatal(err)
				}
				r.Sequence++
				r.Token = o.Entry.Token
			}
			if op != "reserve" {
				setup()
			}
			if op == "returned" || op == "reconcile" {
				r.Operation = "start"
				setup()
			}
			if op == "reconcile" {
				r.Operation = "returned"
				setup()
				r.Envelope = 0
			}
			r.Operation = op
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRequestPublisherProcess$", "-test.timeout=10s")
			cmd.Env = []string{"PATH=/usr/bin:/bin", "CURIO_CAPACITY_REQUEST_TEST_DIR=" + dir, "CURIO_CAPACITY_REQUEST_JSON=" + string(b)}
			if output, err := cmd.CombinedOutput(); err == nil {
				t.Fatal("missing SIGKILL", string(output))
			}
			out, err := a.Resolve(context.Background(), r)
			if err != nil || out.Status != "applied" {
				t.Fatal("no durable published receipt", out, err)
			}
			again, err := a.Apply(context.Background(), r, func(Entry) error { t.Fatal("replayed proof"); return nil })
			if err != nil || again.Entry != out.Entry {
				t.Fatal("recovery reissued execution", again, err)
			}
		})
	}
}
