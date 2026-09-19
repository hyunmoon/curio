package sharedcapacity

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestRecoverableAdmissionRejectsSamplerFailures(t *testing.T) {
	a, _ := fixture(t, 3*modelEnvelope)
	ctx := context.Background()
	r := Request{Client: "client", Sequence: 1, Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: modelEnvelope}
	for _, op := range []string{"reserve", "start", "returned"} {
		r.Operation = op
		o, err := a.Apply(ctx, r, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Token = o.Entry.Token
		r.Sequence++
	}
	r.Operation = "reserve"
	before, _, err := a.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []error{ErrCapacity, fmt.Errorf("quota: %w", ErrCapacity)} {
		a.sample = func(context.Context, State) (Sample, error) { return Sample{}, failure }
		out, err := a.Apply(ctx, r, nil)
		if out.Status != "not-applied" || !errors.Is(err, failure) || out.Entry.Token != "" {
			t.Fatal(out, err)
		}
		after, _, _ := a.Inspect(ctx)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("sampling error published reservation")
		}
	}
	a.sample = func(context.Context, State) (Sample, error) { return Sample{Identity: fixtureID, Free: 0}, nil }
	out, err := a.Apply(ctx, r, nil)
	if err != nil || out.Status != "applied" {
		t.Fatal("validated existing growth blocked", out, err)
	}
	duplicate, err := a.Apply(ctx, r, nil)
	if err != nil || !reflect.DeepEqual(out, duplicate) {
		t.Fatal("receipt replay differs", out, duplicate, err)
	}
	r.Envelope++
	if _, err := a.Apply(ctx, r, nil); !errors.Is(err, ErrRequest) {
		t.Fatal("request identity reused for different payload", err)
	}
}
