package sharedcapacity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// CleanupJournal is a single-issuer, durable queue ONLY for Cancel and Returned.
// A Returned intent must be submitted by the invocation that observed actual
// synchronous return. Replay never starts native work or infers termination.
// It has its own stable receipt client; an unknown request retains that exact
// client/sequence/payload until Resolve succeeds. It does not own Start/Reserve.
// Callers must retain/quarantine an intent if Submit itself fails; a failed
// journal write is NOT a successful durable handoff.
type CleanupJournal struct {
	mu        sync.Mutex
	a         *Authority
	dir, lock *os.File
	client    string
	state     cleanupState
	done      chan struct{}
	wake      chan struct{}
	cancel    context.CancelFunc
	fault     func(string) error // deterministic tests, never configured externally
}

type cleanupState struct {
	Version    int
	Identity   Identity
	Client     string
	Sequence   uint64 // last applied receipt; denied operations don't advance it
	LockDevice uint64
	LockInode  uint64
	Jobs       []cleanupJob
	Completed  []cleanupJob
}

type cleanupJob struct {
	ID           string
	Request      Request
	Acknowledged bool // caller relinquished Submit retry rights; not disk refund
}

const maxCleanupRecords = 1024

// CleanupResult separates durable acceptance from terminal handling. Completed
// includes a token-validated no-op (e.g. Cancel of a running writer), NOT a disk
// refund. Repeat Submit with the exact payload to query retained evidence;
// callers never resubmit under a different request identity.
type CleanupResult struct {
	ID        string
	Durable   bool
	Completed bool
}

type PendingCleanup struct {
	ID      string
	Request Request
	Outcome string
}

// Pending is a diagnostic snapshot of durable responsibility, not execution
// liveness. A sequence-zero intent has not been issued; an issued intent whose
// acknowledgement is missing is unknown. Completed jobs disappear only after
// Resolve/Apply is acknowledged and the journal transition has been persisted.
// Like all journal I/O, this must not be called from the scheduler event loop.
func (j *CleanupJournal) Pending() ([]PendingCleanup, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.read(); err != nil {
		return nil, err
	}
	out := make([]PendingCleanup, 0, len(j.state.Jobs))
	for _, job := range j.state.Jobs {
		status := "unknown"
		if job.Request.Sequence == 0 {
			status = "not-applied"
		}
		out = append(out, PendingCleanup{ID: job.ID, Request: job.Request, Outcome: status})
	}
	return out, nil
}

// OpenCleanupJournal uses a private directory dedicated to one stable issuer.
// create is permitted only for a genuinely new issuer. Reopening missing state
// never silently resets its sequence. An exclusive OS lease prevents two live
// issuers; closing it must occur outside scheduler/event-loop paths.
func OpenCleanupJournal(a *Authority, dir, client string, create bool) (*CleanupJournal, error) {
	if client == "" || len(client) > 256 {
		return nil, ErrRequest
	}
	d, err := openDirectory(dir, a.identity, a.policy, nil)
	if err != nil {
		return nil, err
	}
	j := &CleanupJournal{a: a, dir: d.dir, client: client, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	fail := func(e error) (*CleanupJournal, error) {
		if j.lock != nil {
			_ = j.lock.Close()
		}
		_ = j.dir.Close()
		return nil, e
	}
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if create {
		flags |= unix.O_CREAT | unix.O_EXCL
	}
	fd, err := unix.Openat(int(j.dir.Fd()), "issuer.lock", flags, 0600)
	if err != nil {
		return fail(err)
	}
	j.lock = os.NewFile(uintptr(fd), "issuer.lock")
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fail(err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0022 != 0 || st.Uid != uint32(os.Geteuid()) {
		return fail(ErrIdentity)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(err)
	}
	if create {
		// Must not reuse a receipt namespace from an earlier/missing journal.
		if err := a.lock(context.Background(), func() error {
			s, e := a.read()
			if e != nil {
				return e
			}
			if _, ok := s.Receipts[client]; ok {
				return ErrRequest
			}
			return nil
		}); err != nil {
			return fail(err)
		}
		j.state = cleanupState{Version: 2, Identity: a.identity, Client: client, LockDevice: uint64(st.Dev), LockInode: st.Ino}
		if err := j.write(); err != nil {
			return fail(err)
		}
	} else if err := j.read(); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	go j.run(ctx) // one bounded worker for this issuer, not one per failure
	j.signal()
	return j, nil
}

func (j *CleanupJournal) Close() error {
	j.cancel()
	<-j.done // not a scheduler callback; blocked kernel I/O is not success
	j.mu.Lock()
	defer j.mu.Unlock()
	return errors.Join(j.lock.Close(), j.dir.Close())
}

func (j *CleanupJournal) run(ctx context.Context) {
	defer close(j.done)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-j.wake:
		case <-tick.C:
		}
		_ = j.Recover(ctx) // errors leave the exact durable intent for next pass
	}
}

func (j *CleanupJournal) Submit(ctx context.Context, op, sector, token string) (CleanupResult, error) {
	if (op != "cancel" && op != "returned") || sector == "" || token == "" {
		return CleanupResult{}, ErrRequest
	}
	id := cleanupID(op, sector, token)
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return CleanupResult{ID: id}, err
	}
	// Read authoritative state after any uncertain journal rename. Retry the
	// directory durability before acknowledging an already observed intent.
	if err := j.read(); err != nil {
		return CleanupResult{ID: id}, err
	}
	for _, job := range j.state.Completed {
		if job.ID == id {
			if err := j.dir.Sync(); err != nil {
				return CleanupResult{ID: id}, err
			}
			return CleanupResult{ID: id, Durable: true, Completed: true}, nil
		}
	}
	for _, job := range j.state.Jobs {
		if job.ID == id {
			if err := j.dir.Sync(); err != nil {
				return CleanupResult{ID: id}, err
			}
			j.signal()
			return CleanupResult{ID: id, Durable: true}, nil
		}
	}
	if len(j.state.Jobs)+len(j.state.Completed) >= maxCleanupRecords {
		return CleanupResult{ID: id}, errors.New("cleanup journal full; acknowledge completed handoffs or retain caller quarantine")
	}
	// A new request may already be a no-op against this exact current token.
	// Persist that observation before acknowledging it, independently of later
	// token changes. Absent/replaced entries alone never prove old completion.
	done, err := j.completed(ctx, op, sector, token)
	if err != nil {
		return CleanupResult{ID: id}, err
	}
	job := cleanupJob{ID: id, Request: Request{Client: j.client, Operation: op, Sector: sector, Token: token}}
	if done {
		j.state.Completed = append(j.state.Completed, job)
	} else {
		j.state.Jobs = append(j.state.Jobs, job)
	}
	if err := j.write(); err != nil {
		return CleanupResult{ID: id}, err
	}
	j.signal()
	return CleanupResult{ID: id, Durable: true, Completed: done}, nil
}

func cleanupID(op, sector, token string) string {
	payload, _ := json.Marshal([]string{op, sector, token})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

// Acknowledge ends the caller's right to retry Submit for this ID. The caller
// must first durably retire its own retry intent. Pending mutations remain the
// journal's responsibility; completed evidence may now be collected. A failed
// acknowledgement is retried using Acknowledge, never by issuing Submit again.
// There is no time-based eviction. Unacknowledged evidence consumes a bounded
// slot; a crashed caller must recover its retire/ack protocol before GC.
func (j *CleanupJournal) Acknowledge(ctx context.Context, id string) error {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != sha256.Size {
		return ErrRequest
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := j.read(); err != nil {
		return err
	}
	for i := range j.state.Jobs {
		if j.state.Jobs[i].ID == id {
			j.state.Jobs[i].Acknowledged = true
			return j.write()
		}
	}
	for i := range j.state.Completed {
		if j.state.Completed[i].ID == id {
			j.state.Completed = append(j.state.Completed[:i], j.state.Completed[i+1:]...)
			return j.write()
		}
	}
	// Idempotent ACK after an uncertain rename/previous GC. This says nothing
	// about completion and never issues any authority mutation.
	return j.dir.Sync()
}

func (j *CleanupJournal) signal() {
	select {
	case j.wake <- struct{}{}:
	default:
	}
}

func (j *CleanupJournal) completed(ctx context.Context, op, sector, token string) (bool, error) {
	var done bool
	err := j.a.lock(ctx, func() error {
		s, err := j.a.read()
		if err != nil {
			return err
		}
		e, ok := s.Entries[sector]
		if !ok {
			return ErrIdentity
		}
		if e.Token != token {
			return ErrIdentity
		}
		if op == "returned" {
			done = e.State == "resident"
			return nil
		}
		// A started writer is never refunded by cancellation. Its eventual
		// Returned intent has a separate invocation-owned submission path.
		done = e.State == "running" || e.State == "resident"
		return nil
	})
	return done, err
}

// Recover is serialized with Submit and with this issuer's only Apply call.
// No Resolve is attempted while an asynchronous Apply is still in flight.
func (j *CleanupJournal) Recover(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.read(); err != nil {
		return err
	}
	for len(j.state.Jobs) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		job := &j.state.Jobs[0]
		if job.Request.Sequence == 0 {
			if j.state.Sequence >= math.MaxUint64-1 {
				return ErrRequest
			}
			job.Request.Sequence = j.state.Sequence + 1
			if err := j.write(); err != nil {
				return err
			}
		}
		out, err := j.a.Resolve(ctx, job.Request)
		if err != nil {
			return err
		}
		if out.Status == "not-applied" {
			done, e := j.completed(ctx, job.Request.Operation, job.Request.Sector, job.Request.Token)
			if e != nil {
				return e
			}
			if done {
				if !job.Acknowledged {
					terminal := *job
					terminal.Request.Sequence = 0 // observed no-op, not an Apply receipt
					j.state.Completed = append(j.state.Completed, terminal)
				}
				j.state.Jobs = j.state.Jobs[1:]
				if err := j.write(); err != nil {
					return err
				}
				continue // no mutation/receipt: do not consume the sequence
			}
			out, err = j.a.Apply(ctx, job.Request, nil)
			if err != nil {
				return err
			}
		}
		if out.Status != "applied" {
			return fmt.Errorf("cleanup unresolved: %s", out.Status)
		}
		j.state.Sequence = job.Request.Sequence
		if !job.Acknowledged {
			j.state.Completed = append(j.state.Completed, *job)
		}
		j.state.Jobs = j.state.Jobs[1:]
		if err := j.write(); err != nil {
			return err
		}
	}
	return nil
}

func (j *CleanupJournal) read() error {
	var lock, anchor unix.Stat_t
	if err := unix.Fstat(int(j.lock.Fd()), &lock); err != nil {
		return err
	}
	if err := unix.Fstatat(int(j.dir.Fd()), "issuer.lock", &anchor, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if lock.Dev != anchor.Dev || lock.Ino != anchor.Ino {
		return ErrIdentity
	}
	fd, err := unix.Openat(int(j.dir.Fd()), "cleanup.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "cleanup.json")
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0022 != 0 || st.Uid != uint32(os.Geteuid()) {
		return ErrIdentity
	}
	b, err := io.ReadAll(io.LimitReader(f, MAX_LEDGER_BYTES+1))
	if err != nil {
		return err
	}
	if len(b) > MAX_LEDGER_BYTES {
		return ErrRequest
	}
	var record diskRecord
	if err := json.Unmarshal(b, &record); err != nil {
		return err
	}
	h := sha256.Sum256(record.Payload)
	if hex.EncodeToString(h[:]) != record.SHA256 {
		return errors.New("cleanup journal checksum mismatch")
	}
	var state cleanupState
	if err := json.Unmarshal(record.Payload, &state); err != nil {
		return err
	}
	if (state.Version != 1 && state.Version != 2) || state.Identity != j.a.identity || state.Client != j.client || state.LockDevice != uint64(lock.Dev) || state.LockInode != lock.Ino || len(state.Jobs)+len(state.Completed) > maxCleanupRecords {
		return ErrIdentity
	}
	if state.Version == 1 && len(state.Completed) != 0 {
		return ErrRequest
	}
	// Forward-only format upgrade; a v1 reader must not silently drop terminal
	// evidence. Old, already-discarded receipts cannot be reconstructed.
	state.Version = 2
	seen := map[string]bool{}
	for i, job := range append(append([]cleanupJob(nil), state.Jobs...), state.Completed...) {
		r := job.Request
		if seen[job.ID] || job.ID != cleanupID(r.Operation, r.Sector, r.Token) || r.Client != j.client || r.Sector == "" || r.Token == "" || (r.Operation != "cancel" && r.Operation != "returned") || r.Envelope != 0 || r.Boot != "" {
			return ErrRequest
		}
		if i < len(state.Jobs) && r.Sequence != 0 && (i != 0 || r.Sequence != state.Sequence+1) {
			return ErrRequest
		}
		if i >= len(state.Jobs) && (r.Sequence > state.Sequence || job.Acknowledged) {
			return ErrRequest
		}
		seen[job.ID] = true
	}
	j.state = state
	return nil
}

func (j *CleanupJournal) write() error {
	payload, err := json.Marshal(j.state)
	if err != nil {
		return err
	}
	h := sha256.Sum256(payload)
	b, err := json.Marshal(diskRecord{Payload: payload, SHA256: hex.EncodeToString(h[:])})
	if err != nil {
		return err
	}
	if len(b) > MAX_LEDGER_BYTES {
		return ErrRequest
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	tmp := ".cleanup-" + token
	fd, err := unix.Openat(int(j.dir.Fd()), tmp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer func() { _ = f.Close(); _ = unix.Unlinkat(int(j.dir.Fd()), tmp, 0) }()
	if _, err := f.Write(b); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if j.fault != nil {
		if err := j.fault("before-rename"); err != nil {
			return err
		}
	}
	if err := unix.Renameat(int(j.dir.Fd()), tmp, int(j.dir.Fd()), "cleanup.json"); err != nil {
		return err
	}
	if j.fault != nil {
		if err := j.fault("after-rename"); err != nil {
			return err
		}
	}
	return j.dir.Sync()
}
