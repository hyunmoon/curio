package sharedcapacity

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRecoverEveryPublicationBoundary(t *testing.T) {
	for _, op := range []string{"reserve", "start", "returned", "cancel", "reconcile"} {
		for _, boundary := range []string{"before-write", "before-file-sync", "before-rename", "after-rename", "after-sync", "lost-response"} {
			t.Run(op+"/"+boundary, func(t *testing.T) {
				a, dir := fixture(t, 10*modelEnvelope)
				ctx := context.Background()
				r := Request{Client: "durable-client", Sequence: 1, Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: modelEnvelope}
				proofCalls := 0
				proof := func(Entry) error { proofCalls++; return nil }
				apply := func() Outcome {
					t.Helper()
					out, err := a.Apply(ctx, r, proof)
					if err != nil || out.Status != "applied" {
						t.Fatalf("setup: %+v %v", out, err)
					}
					r.Sequence++
					r.Token = out.Entry.Token
					return out
				}
				if op != "reserve" {
					apply()
				}
				if op == "returned" || op == "reconcile" {
					r.Operation = "start"
					apply()
				}
				if op == "reconcile" {
					r.Operation = "returned"
					apply()
					r.Envelope = 0
				}
				r.Operation = op
				failure := errors.New("injected I/O error")
				a.fault = func(at string) error {
					if at == boundary {
						return failure
					}
					return nil
				}
				first, err := a.Apply(ctx, r, proof)
				published := boundary == "after-rename" || boundary == "after-sync" || boundary == "lost-response"
				if boundary == "lost-response" {
					if err != nil || first.Status != "applied" {
						t.Fatal(first, err)
					}
				} else if !errors.Is(err, failure) {
					t.Fatal(first, err)
				}
				if published && boundary != "lost-response" && first.Status != "unknown" {
					t.Fatal(first)
				}
				if !published && first.Status != "not-applied" {
					t.Fatal(first)
				}
				// A fresh handle has no fault hook or in-memory outcome knowledge.
				b, err := Open(dir, fixtureID, fixturePolicy, a.sample)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = b.Close() }()
				resolved, err := b.Resolve(ctx, r)
				if err != nil || (resolved.Status == "applied") != published {
					t.Fatal(resolved, err)
				}
				beforeRetryCalls := proofCalls
				last, err := b.Apply(ctx, r, proof)
				if err != nil || last.Status != "applied" {
					t.Fatal(last, err)
				}
				if published && !reflect.DeepEqual(last, resolved) {
					t.Fatal("receipt changed on replay")
				}
				if published && proofCalls != beforeRetryCalls {
					t.Fatal("published reconciliation re-executed")
				}
				again, err := b.Apply(ctx, r, proof)
				if err != nil || !reflect.DeepEqual(again, last) {
					t.Fatal("duplicate operation", again, err)
				}
				state, _, err := b.Inspect(ctx)
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if op == "cancel" || op == "reconcile" {
					want = 0
				}
				if len(state.Entries) != want || len(state.Receipts) != 1 {
					t.Fatal(state)
				}
			})
		}
	}
}

func TestRequestCancellationFencingAndEvidence(t *testing.T) {
	a, _ := fixture(t, 4*modelEnvelope)
	r := Request{Client: "client", Sequence: 1, Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: modelEnvelope}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := a.Apply(ctx, r, nil)
	if !errors.Is(err, context.Canceled) || out.Status != "unknown" {
		t.Fatal(out, err)
	}
	ctx = context.Background()
	out, err = a.Apply(ctx, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	reserve := r
	r.Sequence++
	r.Operation = "start"
	r.Token = out.Entry.Token
	_, err = a.Apply(ctx, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(ctx, reserve, nil); !errors.Is(err, ErrRequest) {
		t.Fatal("old reserve replayed", err)
	}
	r.Sequence++
	r.Operation = "cancel"
	if _, err := a.Apply(ctx, r, nil); !errors.Is(err, ErrIdentity) {
		t.Fatal("started writer refunded", err)
	}
	r.Operation = "reconcile"
	r.Envelope = 0
	if _, err := a.Apply(ctx, r, func(Entry) error { return errors.New("termination unknown") }); err == nil {
		t.Fatal("missing proof accepted")
	}
	s, _, err := a.Inspect(ctx)
	if err != nil || s.Entries["sector"].State != "running" {
		t.Fatal(s, err)
	}
}

func TestResolveFailureNeverRefundsAndRecoveryReusesToken(t *testing.T) {
	a, _ := fixture(t, 4*modelEnvelope)
	ctx, cancel := context.WithCancel(context.Background())
	r := Request{Client: "client", Sequence: 1, Operation: "reserve", Sector: "sector", Boot: "boot", Envelope: modelEnvelope}
	a.fault = func(at string) error {
		if at == "after-rename" {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	out, err := a.Apply(ctx, r, nil)
	if out.Status != "unknown" || !errors.Is(err, context.Canceled) {
		t.Fatal(out, err)
	}
	retry, retryErr := a.Apply(ctx, r, nil)
	if retry.Status != "unknown" || !errors.Is(retryErr, context.Canceled) {
		t.Fatal("cancelled retry claimed non-publication", retry, retryErr)
	}
	out, err = a.Resolve(context.Background(), r)
	if out.Status != "unknown" || err == nil {
		t.Fatal(out, err)
	}
	s, _, _ := a.Inspect(context.Background())
	token := s.Entries["sector"].Token
	if token == "" {
		t.Fatal("uncertain reservation refunded")
	}
	a.fault = nil
	out, err = a.Resolve(context.Background(), r)
	if out.Status != "applied" || out.Entry.Token != token || err != nil {
		t.Fatal(out, err)
	}
	r.Sequence++
	r.Operation = "cancel"
	r.Token = token
	if _, err := a.Apply(context.Background(), r, nil); err != nil {
		t.Fatal(err)
	}
	r.Sequence++
	r.Operation = "reserve"
	r.Token = ""
	out, err = a.Apply(context.Background(), r, nil)
	if err != nil || out.Entry.Token == token {
		t.Fatal("pre-write recovery leaked or reused execution token", out, err)
	}
}
