package sharedcapacity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

type publishedError struct{ error }

func (e *publishedError) Unwrap() error { return e.error }

// Request identifies one operation in a serial client session. Persist it before
// calling Apply. Never advance Sequence until this operation has been resolved.
// Client must be unique, stable across recovery, and have only one issuer. A
// receipt occupies one slot per client, not one slot per sector or operation.
type Request struct {
	Client              string
	Sequence            uint64
	Operation           string // reserve, start, returned, cancel, reconcile
	Sector, Boot, Token string
	Envelope            int64
}

type Receipt struct {
	Sequence uint64
	Digest   string
	Entry    Entry
	Decision Decision
}

type Outcome struct {
	Status   string // applied, not-applied, unknown
	Entry    Entry
	Decision Decision
}

var ErrRequest = errors.New("capacity request sequence or payload conflict")

func (r Request) digest() (string, error) {
	if r.Client == "" || len(r.Client) > 256 || r.Sequence == 0 || r.Sequence == math.MaxUint64 || r.Sector == "" {
		return "", ErrRequest
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func receipt(s State, r Request, digest string) (Receipt, bool, error) {
	last := s.Receipts[r.Client]
	if last.Sequence == r.Sequence {
		if last.Digest != digest {
			return Receipt{}, false, ErrRequest
		}
		return last, true, nil
	}
	if last.Sequence == math.MaxUint64 || r.Sequence != last.Sequence+1 {
		return Receipt{}, false, ErrRequest
	}
	return Receipt{}, false, nil
}

func applied(r Receipt) Outcome {
	return Outcome{Status: "applied", Entry: r.Entry, Decision: r.Decision}
}

// Resolve distinguishes a missing operation from an applied operation and
// retries durability of an observed receipt. A successful Start receipt is NOT
// evidence that native execution did or did not start: retain the writer's own
// exclusive execution lease. Never launch a second writer from this result.
// Resolution requires a quiescent issuer (no outstanding asynchronous Apply).
func (a *Authority) Resolve(ctx context.Context, r Request) (Outcome, error) {
	out := Outcome{Status: "unknown"}
	digest, err := r.digest()
	if err != nil {
		return out, err
	}
	err = a.lock(ctx, func() error {
		s, err := a.read()
		if err != nil {
			return err
		}
		rec, found, err := receipt(s, r, digest)
		if err != nil {
			return err
		}
		// Republish the complete current state, with file and directory sync.
		// Do not reapply a mutation or its proof, or refund its commitment.
		if err := a.write(s); err != nil {
			return err
		}
		if found {
			out = applied(rec)
		} else {
			out.Status = "not-applied"
		}
		return nil
	})
	return out, err
}

// Apply is the recoverable mutation API. Exactly one receipt per serial client
// prevents replay after cancellation/removal without accumulating per-operation
// tombstones. Older sequence numbers are rejected, never reinterpreted as new.
// The returned state is accounting state, not permission to duplicate native I/O.
func (a *Authority) Apply(ctx context.Context, r Request, proof func(Entry) error) (Outcome, error) {
	// A retry may already have published. Failure to acquire/read the ledger
	// cannot establish that the identified request was not applied earlier.
	out := Outcome{Status: "unknown"}
	digest, err := r.digest()
	if err != nil {
		return out, err
	}
	err = a.lock(ctx, func() error {
		s, err := a.read()
		if err != nil {
			out.Status = "unknown"
			return err
		}
		rec, found, err := receipt(s, r, digest)
		if err != nil {
			return err
		}
		if found {
			out.Status = "unknown"
			if err := a.write(s); err != nil {
				return err
			}
			out = applied(rec)
			return nil
		}
		out.Status = "not-applied"
		e, exists := s.Entries[r.Sector]
		var d Decision
		if r.Operation == "reserve" {
			if r.Boot == "" || r.Envelope <= 0 {
				return ErrRequest
			}
			if exists && e.State != "resident" {
				return ErrBusy
			}
			additional := r.Envelope
			if exists {
				additional = max(int64(0), r.Envelope-e.Envelope)
			}
			d, err = a.measure(ctx, s, additional)
			if err != nil && (!exists || additional != 0 || !d.validatedPressure) {
				return err
			}
			token, err := randomToken()
			if err != nil {
				return err
			}
			e = Entry{Envelope: max(e.Envelope, r.Envelope), Prior: e.Envelope, Token: token, Boot: r.Boot, State: "tentative"}
			s.Entries[r.Sector] = e
		} else {
			if !exists || r.Token == "" || r.Token != e.Token {
				return ErrIdentity
			}
			switch r.Operation {
			case "start":
				if e.State != "tentative" {
					return ErrIdentity
				}
				e.State = "running"
			case "returned":
				if e.State != "running" {
					return ErrIdentity
				}
				e.State = "resident"
			case "cancel":
				if e.State != "tentative" {
					return ErrIdentity
				}
				e.Envelope, e.Prior, e.State = e.Prior, 0, "resident"
			case "reconcile":
				if r.Envelope < 0 || r.Envelope > e.Envelope || proof == nil {
					return ErrRequest
				}
				if err := proof(e); err != nil {
					return fmt.Errorf("reconciliation evidence: %w", err)
				}
				e.Envelope, e.Prior, e.State = r.Envelope, 0, "resident"
			default:
				return ErrRequest
			}
			if e.Envelope == 0 {
				delete(s.Entries, r.Sector)
			} else {
				s.Entries[r.Sector] = e
			}
		}
		if s.Receipts == nil {
			s.Receipts = map[string]Receipt{}
		}
		d.validatedPressure = false // internal disposition, not part of the durable response
		rec = Receipt{Sequence: r.Sequence, Digest: digest, Entry: e, Decision: d}
		s.Receipts[r.Client] = rec
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.write(s); err != nil {
			var published *publishedError
			if errors.As(err, &published) {
				out.Status = "unknown"
			}
			return err
		}
		out = applied(rec)
		return nil
	})
	return out, err
}
