package sdrscratch

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrAccessBusy identifies lock contention only, not identity/permission errors.
var ErrAccessBusy = errors.New("sector access busy")

const accessWaitLimit = 30 * time.Second
const accessRetryInterval = 25 * time.Millisecond

// AccessPathsContext waits only for transient gate contention. Every failed
// attempt releases all partial acquisitions before waiting. Once acquired,
// cancellation never releases the gate: the caller must finish its actual I/O.
func AccessPathsContext(ctx context.Context, paths ...string) (func(), error) {
	if on, err := PersonalCleanupEnabled(); err != nil || !on {
		return func() {}, err
	}
	ctx, cancel := context.WithTimeout(ctx, accessWaitLimit)
	defer cancel()
	var lastBusy error
	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(lastBusy, err)
		}
		release, err := AccessPaths(paths...)
		if !errors.Is(err, ErrAccessBusy) {
			return release, err
		}
		lastBusy = err
		timer := time.NewTimer(accessRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("waiting for sector access: %w", errors.Join(ErrAccessBusy, ctx.Err()))
		case <-timer.C:
		}
	}
}

// AccessPaths must span actual file access, not just path selection or context
// ownership. Cancellation does not release it; callers close it after native IO
// returns. Keys include the physical registered root and full miner/sector name.
func AccessPaths(paths ...string) (func(), error) {
	on, err := PersonalCleanupEnabled()
	if err != nil || !on {
		return func() {}, err
	}
	s := personalSessionPtr.Load()
	if s == nil {
		return nil, fmt.Errorf("personal SDR lifetime not established")
	}
	keys := map[string]string{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		root := filepath.Dir(filepath.Dir(p))
		s.mu.RLock()
		_, registered := s.roots[root]
		s.mu.RUnlock()
		if !registered {
			continue // non-sealing storage is never an automatic discard target
		}
		if _, err := s.load(root); err != nil {
			return nil, err
		}
		name := filepath.Base(p)
		if !canonicalSector.MatchString(name) {
			return nil, fmt.Errorf("invalid sector access path")
		}
		keys[root+"/"+name] = root
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	var held []*os.File
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, f := range held {
				_ = f.Close()
			}
		})
	}
	for _, key := range ordered {
		c, err := s.load(keys[key])
		if err != nil {
			release()
			return nil, err
		}
		f, err := sectorGate(c, filepath.Base(key), false, s.access.dir)
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, f)
	}
	return release, nil
}

func sectorGate(c *ManagedConfig, sector string, exclusive bool, open func(string) (*os.File, error)) (*os.File, error) {
	if len(c.Storage) != 1 || !canonicalSector.MatchString(sector) {
		return nil, fmt.Errorf("one registered root and exact sector required")
	}
	d, err := open(c.StateDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	r := c.Storage[0]
	key := fmt.Sprintf("access-%x.lock", sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%s/%s", r.Device, r.Inode, r.ID, sector))))
	fd, err := unix.Openat(int(d.Fd()), key, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), key)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err == nil && (st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&0077 != 0) {
		err = fmt.Errorf("unsafe sector gate")
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	if err == nil {
		err = unix.Flock(fd, mode|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			err = fmt.Errorf("%w: %w", ErrAccessBusy, err)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sector access busy or unavailable: %w", err)
	}
	return f, nil
}
