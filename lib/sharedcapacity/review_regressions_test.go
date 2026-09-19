package sharedcapacity

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// Reconstructed from the independent review's stated inputs/assertions. The
// review's original test file was not included in the supplied attachments.
func TestReviewSamplerErrorMustNotAdmitResidentFollowup(t *testing.T) {
	for _, failure := range []error{ErrCapacity, fmt.Errorf("quota sampling: %w", ErrCapacity)} {
		t.Run(failure.Error(), func(t *testing.T) {
			a, _ := fixture(t, 2*modelEnvelope+fixturePolicy.Margin)
			ctx := context.Background()
			token, _, err := a.Reserve(ctx, "sector", "boot", modelEnvelope)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Start(ctx, "sector", token); err != nil {
				t.Fatal(err)
			}
			if err := a.Returned(ctx, "sector", token); err != nil {
				t.Fatal(err)
			}
			before, _, err := a.Inspect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			a.sample = func(context.Context, State) (Sample, error) { return Sample{}, failure }
			got, _, err := a.Reserve(ctx, "sector", "boot", modelEnvelope)
			if got != "" || !errors.Is(err, failure) {
				t.Errorf("sampler failure admitted follow-up: token_nonempty=%t err=%v", got != "", err)
			}
			after, _, _ := a.Inspect(ctx)
			if !reflect.DeepEqual(before, after) {
				t.Error("rejected sample changed ledger")
			}
		})
	}
}

func TestReviewPostRenameErrorHasUncertainPublishedOutcome(t *testing.T) {
	a, _ := fixture(t, 2*modelEnvelope+fixturePolicy.Margin)
	ctx := context.Background()
	failure := errors.New("injected directory sync failure")
	a.fault = func(at string) error {
		if at == "after-rename" {
			return failure
		}
		return nil
	}
	token, _, err := a.Reserve(ctx, "sector", "boot", modelEnvelope)
	if token != "" || !errors.Is(err, failure) {
		t.Fatalf("%q %v", token, err)
	}
	a.fault = nil
	s, _, err := a.Inspect(ctx)
	if err != nil || s.Entries["sector"].State != "tentative" {
		t.Fatalf("published outcome absent: %+v %v", s, err)
	}
	if _, _, err := a.Reserve(ctx, "sector", "boot", modelEnvelope); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
}
