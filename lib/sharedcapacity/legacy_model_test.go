package sharedcapacity

import (
	"context"
	"errors"
)

// Historical low-level methods remain ONLY in tests to preserve the original
// model/regression fixtures. Runtime callers must use recoverable Apply/Resolve;
// an unkeyed Reserve cannot resolve a lost publication response safely.
// Reserve grants only a tentative execution; no I/O is permitted before Start.
// An existing resident sector is not charged a second complete envelope.
func (a *Authority) Reserve(ctx context.Context, sector, boot string, envelope int64) (string, Decision, error) {
	var d Decision
	if sector == "" || boot == "" || envelope <= 0 {
		return "", d, errors.New("invalid capacity request")
	}
	token, err := randomToken()
	if err != nil {
		return "", d, err
	}
	err = a.lock(ctx, func() error {
		s, err := a.read()
		if err != nil {
			return err
		}
		old, ok := s.Entries[sector]
		if ok && old.State != "resident" {
			return ErrBusy
		}
		additional := envelope
		if ok {
			additional = max(int64(0), envelope-old.Envelope)
			envelope = max(envelope, old.Envelope)
		}
		d, err = a.measure(ctx, s, additional)
		// Existing protected growth is already promised. A physical free-space
		// alarm must not revoke it or hand its budget to a new sector.
		if err != nil && (!ok || additional != 0 || !d.validatedPressure) {
			return err
		}
		s.Entries[sector] = Entry{Envelope: envelope, Prior: old.Envelope, Token: token, Boot: boot, State: "tentative"}
		return a.write(s)
	})
	if err != nil {
		return "", d, err
	}
	return token, d, nil
}

func (a *Authority) transition(ctx context.Context, sector, token, from, to string) error {
	return a.lock(ctx, func() error {
		s, err := a.read()
		if err != nil {
			return err
		}
		e, ok := s.Entries[sector]
		if !ok || e.Token != token || e.State != from {
			return ErrIdentity
		}
		e.State = to
		s.Entries[sector] = e
		return a.write(s)
	})
}

func (a *Authority) Start(ctx context.Context, sector, token string) error {
	return a.transition(ctx, sector, token, "tentative", "running")
}

// Returned requires synchronous completion of all writes, not cancellation,
// loss of task ownership, heartbeat expiration or release of an OS lock.
func (a *Authority) Returned(ctx context.Context, sector, token string) error {
	return a.transition(ctx, sector, token, "running", "resident")
}

// Cancel rolls back only an unstarted request. No writer (including fetch) may
// write before Start. It cannot remove a previous sector residency commitment.
func (a *Authority) Cancel(ctx context.Context, sector, token string) error {
	return a.lock(ctx, func() error {
		s, err := a.read()
		if err != nil {
			return err
		}
		e, ok := s.Entries[sector]
		if !ok || e.Token != token || e.State != "tentative" {
			return ErrIdentity
		}
		if e.Prior == 0 {
			delete(s.Entries, sector)
		} else {
			e.Envelope, e.Prior, e.State = e.Prior, 0, "resident"
			s.Entries[sector] = e
		}
		return a.write(s)
	})
}

// Reconcile lowers an envelope only after the caller proves the old writer has
// ended AND the new footprint/future-write bound. It must also retain any debt
// from unlinked-but-open files. Neither age nor process death supplies that proof.
// proof executes under the short authority lock; it must not wait for native I/O.
func (a *Authority) Reconcile(ctx context.Context, sector, token string, envelope int64, proof func(Entry) error) error {
	if envelope < 0 || proof == nil {
		return errors.New("missing capacity reconciliation proof")
	}
	return a.lock(ctx, func() error {
		s, err := a.read()
		if err != nil {
			return err
		}
		e, ok := s.Entries[sector]
		if !ok || e.Token != token || envelope > e.Envelope {
			return ErrIdentity
		}
		if err := proof(e); err != nil {
			return err
		}
		if envelope == 0 {
			delete(s.Entries, sector)
		} else {
			e.Envelope, e.Prior, e.State = envelope, 0, "resident"
			s.Entries[sector] = e
		}
		return a.write(s)
	})
}
