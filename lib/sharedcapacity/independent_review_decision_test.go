package sharedcapacity

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Reconstructed from R2-02's stated inputs; the independent Go attachment was
// not supplied with the review document in this workspace.
func TestReviewCapacityDenialRetainsDecision(t *testing.T) {
	a, _ := fixture(t, 100<<30)
	before, err := a.read()
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Apply(context.Background(), Request{Client: "review", Sequence: 1,
		Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: 200 << 30}, nil)
	if !errors.Is(err, ErrCapacity) || out.Status != "not-applied" || out.Entry.Token != "" {
		t.Fatalf("incorrect denial: %+v %v", out, err)
	}
	after, err := a.read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("denial mutated ledger")
	}
	d := out.Decision
	if !d.MeasurementValid || d.Free != 100<<30 || d.Credit != 0 || d.Future != 0 || d.Margin != 32<<30 ||
		d.Additional != 200<<30 || d.Headroom != 68<<30 || d.Sequence != before.Sequence {
		t.Fatalf("denial lost measured decision: %+v", d)
	}
}

func TestDenialSampleValidity(t *testing.T) {
	for _, mode := range []string{"sentinel", "identity", "credit", "negative-free"} {
		t.Run(mode, func(t *testing.T) {
			a, _ := fixture(t, 100<<30)
			a.sample = func(context.Context, State) (Sample, error) {
				v := Sample{Identity: fixtureID, Free: 100 << 30}
				switch mode {
				case "sentinel":
					return v, ErrCapacity
				case "identity":
					v.Identity.Host = "wrong"
				case "credit":
					v.Credit = map[string]int64{"unknown": 1}
				case "negative-free":
					v.Free = -1
				}
				return v, nil
			}
			out, err := a.Apply(context.Background(), Request{Client: "review", Sequence: 1,
				Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: 200 << 30}, nil)
			if err == nil || out.Decision.MeasurementValid || out.Entry.Token != "" || out.Status != "not-applied" {
				t.Fatalf("invalid sample accepted: %+v %v", out, err)
			}
		})
	}
}
