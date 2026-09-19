// Package sharedcapacity implements the durable accounting part of a local
// filesystem admission protocol. It does not infer native termination or obtain
// materialization credit from arbitrary files. Writers must obey the Sampler
// contract before this authority can be connected to a storage path.
package sharedcapacity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const VERSION = 2
const MAX_LEDGER_BYTES = 8 << 20

var ErrCapacity = errors.New("WAIT_CAPACITY")
var ErrBusy = errors.New("capacity execution already active")
var ErrIdentity = errors.New("capacity identity or policy mismatch")

// Identity is a verified host/filesystem identity, not a path, storage ID or
// mount ID. Boot ID belongs to execution evidence, not to the budget key.
type Identity struct {
	Host, Filesystem string
}

type Policy struct {
	Protocol int
	Margin   int64
}

type Entry struct {
	Envelope int64
	Prior    int64 // envelope before the current tentative request
	Token    string
	Boot     string
	State    string // tentative, running, resident, uncertain
}

type State struct {
	Version    int
	Identity   Identity
	Policy     Policy
	LockDevice uint64
	LockInode  uint64
	Sequence   uint64
	Entries    map[string]Entry
	Receipts   map[string]Receipt `json:",omitempty"`
}

// Sample must be fresh, uncached and measured while the authority lock is held.
// Credit is a lower bound on exclusively allocated bytes belonging to that
// entry, not logical length or reflink-shared blocks. A writer deleting,
// truncating, copying or replacing credited bytes MUST participate in the same
// accounting protocol. Sampling two unrelated moments is not a valid Sample.
// The storage integration, not this arithmetic engine, has to prove this contract.
type Sample struct {
	Identity Identity
	Free     int64
	Credit   map[string]int64
}

type Sampler func(context.Context, State) (Sample, error)

type Decision struct {
	Free, Credit, Future, Margin, Additional, Headroom int64
	Sequence                                           uint64
	// Set only after this allocator has validated the complete sample. A
	// sampler error (even ErrCapacity) cannot manufacture this disposition.
	validatedPressure bool
}

// Authority owns an already initialized, privately writable domain directory.
// No work is admitted merely by opening it. Missing/corrupt state is an error.
type Authority struct {
	dir      *os.File
	identity Identity
	policy   Policy
	sample   Sampler
	// Tests inject crashes at persistence boundaries. Never configured by env.
	fault func(string) error
}

type diskRecord struct {
	Payload json.RawMessage
	SHA256  string
}

func validPolicy(id Identity, p Policy) bool {
	return id.Host != "" && id.Filesystem != "" && p.Protocol == VERSION && p.Margin > 0
}

// Initialize is only for an independently verified drained cohort. It never
// overwrites an existing ledger. There is deliberately no automatic bootstrap
// from a missing file: existing writers/resident commitments cannot be inferred.
func Initialize(dir string, id Identity, p Policy) error {
	if !validPolicy(id, p) {
		return ErrIdentity
	}
	a, err := openDirectory(dir, id, p, nil)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	// Only explicit, drained bootstrap may create a lock file. Open/admit
	// must never recreate a missing lock while another process still holds
	// the unlinked old inode.
	fd, err := unix.Openat(int(a.dir.Fd()), "lock", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err == nil {
		_ = unix.Close(fd)
		if err := a.dir.Sync(); err != nil {
			return err
		}
	} else if !errors.Is(err, unix.EEXIST) {
		return err
	}
	return a.lock(context.Background(), func() error {
		fd, e := unix.Openat(int(a.dir.Fd()), "ledger.json", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
		if e == nil {
			_ = unix.Close(fd)
			return errors.New("capacity ledger already exists")
		}
		if !errors.Is(e, unix.ENOENT) {
			return e
		}
		var anchor unix.Stat_t
		if err := unix.Fstatat(int(a.dir.Fd()), "lock", &anchor, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		return a.write(State{Version: VERSION, Identity: id, Policy: p, LockDevice: uint64(anchor.Dev), LockInode: anchor.Ino, Entries: map[string]Entry{}})
	})
}

func Open(dir string, id Identity, p Policy, sample Sampler) (*Authority, error) {
	if !validPolicy(id, p) || sample == nil {
		return nil, ErrIdentity
	}
	a, err := openDirectory(dir, id, p, sample)
	if err != nil {
		return nil, err
	}
	err = a.lock(context.Background(), func() error { _, e := a.read(); return e })
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	return a, nil
}

func openDirectory(dir string, id Identity, p Policy, sample Sampler) (*Authority, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), dir)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err != nil || st.Mode&0022 != 0 || st.Uid != uint32(os.Geteuid()) {
		_ = f.Close()
		return nil, fmt.Errorf("capacity directory must be privately writable by this user: %w", errors.Join(err, ErrIdentity))
	}
	return &Authority{dir: f, identity: id, policy: p, sample: sample}, nil
}

func (a *Authority) Close() error { return a.dir.Close() }

func (a *Authority) lock(ctx context.Context, f func() error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	fd, err := unix.Openat(int(a.dir.Fd()), "lock", unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }() // releases the lock; never the budget
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0022 != 0 || st.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("unsafe capacity lock: %w", errors.Join(err, ErrIdentity))
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	var anchor unix.Stat_t
	if err := unix.Fstatat(int(a.dir.Fd()), "lock", &anchor, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Dev != anchor.Dev || st.Ino != anchor.Ino {
		return ErrIdentity
	}
	return f()
}

func (a *Authority) read() (State, error) {
	var state State
	fd, err := unix.Openat(int(a.dir.Fd()), "ledger.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return state, err
	}
	f := os.NewFile(uintptr(fd), "ledger.json")
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0022 != 0 || st.Uid != uint32(os.Geteuid()) {
		return state, fmt.Errorf("unsafe capacity ledger: %w", errors.Join(err, ErrIdentity))
	}
	b, err := io.ReadAll(io.LimitReader(f, MAX_LEDGER_BYTES+1))
	if err != nil || len(b) > MAX_LEDGER_BYTES {
		return state, errors.New("unreadable or oversized capacity ledger")
	}
	var record diskRecord
	if err := json.Unmarshal(b, &record); err != nil {
		return state, err
	}
	sum := sha256.Sum256(record.Payload)
	if hex.EncodeToString(sum[:]) != record.SHA256 {
		return state, errors.New("capacity ledger checksum mismatch")
	}
	if err := json.Unmarshal(record.Payload, &state); err != nil {
		return state, err
	}
	if state.Version != VERSION || state.Identity != a.identity || state.Policy != a.policy || state.Entries == nil {
		return state, ErrIdentity
	}
	var anchor unix.Stat_t
	if err := unix.Fstatat(int(a.dir.Fd()), "lock", &anchor, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return state, err
	}
	if state.LockInode == 0 || state.LockDevice != uint64(anchor.Dev) || state.LockInode != anchor.Ino {
		return state, ErrIdentity
	}
	for k, e := range state.Entries {
		if k == "" || e.Envelope <= 0 || e.Prior < 0 || e.Prior > e.Envelope || e.Boot == "" || e.Token == "" {
			return state, errors.New("invalid capacity entry")
		}
		switch e.State {
		case "tentative", "running", "resident", "uncertain":
		default:
			return state, errors.New("unknown capacity entry state")
		}
	}
	for client, r := range state.Receipts {
		digest, err := hex.DecodeString(r.Digest)
		if client == "" || len(client) > 256 || r.Sequence == 0 || r.Sequence == math.MaxUint64 || err != nil || len(digest) != sha256.Size {
			return state, errors.New("invalid capacity receipt")
		}
	}
	return state, nil
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (a *Authority) write(s State) error {
	if s.Sequence == math.MaxUint64 {
		return errors.New("capacity sequence exhausted")
	}
	s.Sequence++
	if a.fault != nil {
		if err := a.fault("before-write"); err != nil {
			return err
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	b, err = json.Marshal(diskRecord{Payload: b, SHA256: hex.EncodeToString(sum[:])})
	if err != nil || len(b) > MAX_LEDGER_BYTES {
		return errors.New("capacity ledger cannot be encoded within limit")
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	tmp := ".ledger-" + token
	fd, err := unix.Openat(int(a.dir.Fd()), tmp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer func() { _ = f.Close() }()
	defer func() { _ = unix.Unlinkat(int(a.dir.Fd()), tmp, 0) }() // only this call's unpublished metadata
	if _, err = f.Write(b); err != nil {
		return err
	}
	if a.fault != nil {
		if err := a.fault("before-file-sync"); err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if a.fault != nil {
		if err := a.fault("before-rename"); err != nil {
			return err
		}
	}
	if err = unix.Renameat(int(a.dir.Fd()), tmp, int(a.dir.Fd()), "ledger.json"); err != nil {
		return err
	}
	if a.fault != nil {
		if err := a.fault("after-rename"); err != nil {
			return &publishedError{err}
		}
	}
	if err := a.dir.Sync(); err != nil {
		return &publishedError{err}
	}
	if a.fault != nil {
		if err := a.fault("after-sync"); err != nil {
			return &publishedError{err}
		}
	}
	return nil
}

func add(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, errors.New("capacity byte arithmetic overflow")
	}
	return a + b, nil
}

func (a *Authority) measure(ctx context.Context, s State, additional int64) (Decision, error) {
	d := Decision{Margin: s.Policy.Margin, Additional: additional, Sequence: s.Sequence}
	view := s
	// Receipts are not sampler inputs; do not share a mutable map with it.
	view.Receipts = nil
	view.Entries = make(map[string]Entry, len(s.Entries))
	for key, entry := range s.Entries {
		view.Entries[key] = entry
	}
	v, err := a.sample(ctx, view)
	if err != nil {
		return d, err
	}
	if v.Identity != s.Identity || v.Free < 0 || additional < 0 {
		return d, ErrIdentity
	}
	d.Free = v.Free
	for key := range v.Credit {
		if _, ok := s.Entries[key]; !ok {
			return d, errors.New("unrelated materialization credit")
		}
	}
	for key, entry := range s.Entries {
		c := v.Credit[key]
		if c < 0 || c > entry.Envelope {
			return d, errors.New("invalid materialization credit")
		}
		if d.Credit, err = add(d.Credit, c); err != nil {
			return d, err
		}
		if d.Future, err = add(d.Future, entry.Envelope-c); err != nil {
			return d, err
		}
	}
	required, err := add(d.Future, d.Margin)
	if err != nil {
		return d, err
	}
	d.Headroom = max(int64(0), d.Free-required)
	if d.Free < required || d.Headroom < additional {
		d.validatedPressure = true
		return d, ErrCapacity
	}
	return d, nil
}

func (a *Authority) Inspect(ctx context.Context) (State, Decision, error) {
	var s State
	var d Decision
	err := a.lock(ctx, func() error {
		var err error
		s, err = a.read()
		if err == nil {
			d, err = a.measure(ctx, s, 0)
		}
		return err
	})
	return s, d, err
}
