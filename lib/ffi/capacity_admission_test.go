package ffi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/sharedcapacity"
	"github.com/filecoin-project/curio/lib/storiface"

	"github.com/filecoin-project/lotus/storage/sealer/fsutil"
)

// These fixtures exercise the actual Claim -> Acquire -> generateSDR and release
// boundaries. The sampler, DB index and native body are replaced. They do NOT
// claim a closed filesystem/writer protocol or actual 32-GiB/native occupancy.
type capacityCallerIndex struct {
	paths.SectorIndex
	info storiface.StorageInfo
}

func (i *capacityCallerIndex) StorageAttach(_ context.Context, info storiface.StorageInfo, _ fsutil.FsStat) error {
	i.info = info
	return nil
}
func (*capacityCallerIndex) StorageList(context.Context, storiface.ID) ([]storiface.Decl, error) {
	return nil, nil
}
func (*capacityCallerIndex) BatchStorageDeclareSectors(context.Context, []paths.SectorDeclaration) error {
	return nil
}
func (*capacityCallerIndex) StorageReportHealth(context.Context, storiface.ID, storiface.HealthReport) error {
	return nil
}
func (*capacityCallerIndex) StorageTryLock(context.Context, abi.SectorID, storiface.SectorFileType, storiface.SectorFileType) (bool, error) {
	return true, nil
}
func (i *capacityCallerIndex) StorageBestAlloc(context.Context, storiface.SectorFileType, abi.SectorSize, storiface.PathType, abi.ActorID) ([]storiface.StorageInfo, error) {
	return []storiface.StorageInfo{i.info}, nil
}
func (*capacityCallerIndex) StorageFindSector(context.Context, abi.SectorID, storiface.SectorFileType, abi.SectorSize, bool) ([]storiface.SectorStorageInfo, error) {
	return nil, nil
}
func (*capacityCallerIndex) StorageDeclareSector(context.Context, storiface.ID, abi.SectorID, storiface.SectorFileType, bool) error {
	return nil
}

type capacityCallerStorage struct{ config storiface.StorageConfig }

func (s *capacityCallerStorage) GetStorage() (storiface.StorageConfig, error) { return s.config, nil }
func (s *capacityCallerStorage) SetStorage(f func(*storiface.StorageConfig)) error {
	f(&s.config)
	return nil
}
func (*capacityCallerStorage) Stat(string) (fsutil.FsStat, error) {
	return fsutil.FsStat{Capacity: 16 << 40, Available: 16 << 40, FSAvailable: 16 << 40}, nil
}
func (*capacityCallerStorage) DiskUsage(string) (int64, error) { return 0, nil }

type capacityCallerAdapter struct {
	a        *sharedcapacity.Authority
	mu       sync.Mutex
	sequence uint64
	journal  string
	envelope int64
	client   string
	cleanup  *sharedcapacity.CleanupJournal
}

func (a *capacityCallerAdapter) apply(ctx context.Context, op, key, token string, bytes int64) (sharedcapacity.Outcome, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := sharedcapacity.Request{Client: a.client, Sequence: a.sequence + 1, Operation: op, Sector: key, Token: token, Boot: "fixture-boot", Envelope: bytes}
	// Test-only durable intent; production needs an exclusive, restartable
	// client journal/lease, not this fixture's process-local mutex.
	b, e := json.Marshal(r)
	if e != nil {
		return sharedcapacity.Outcome{}, e
	}
	f, e := os.OpenFile(a.journal, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if e != nil {
		return sharedcapacity.Outcome{}, e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return sharedcapacity.Outcome{}, e
	}
	out, e := a.a.Apply(ctx, r, nil)
	if e == nil {
		a.sequence++
	}
	return out, e
}
func (a *capacityCallerAdapter) Reserve(ctx context.Context, s SectorRef, _ storiface.SectorPaths, _ storiface.SectorPaths, sdr bool) (capacityClaim, error) {
	if !sdr {
		return nil, errors.New("fixture supports SDR only")
	}
	key := fmt.Sprintf("%d/%d", s.SpID, s.SectorNumber)
	out, e := a.apply(ctx, "reserve", key, "", a.envelope)
	if e != nil {
		return nil, e
	}
	return &capacityCallerClaim{adapter: a, key: key, token: out.Entry.Token}, nil
}

type capacityCallerClaim struct {
	mu                        sync.Mutex
	adapter                   *capacityCallerAdapter
	key, token                string
	started, ended, cancelled bool
}

func (c *capacityCallerClaim) Begin(ctx context.Context) (capacityExecution, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.ended || c.cancelled {
		return nil, errors.New("writer already entered or claim cancelled")
	}
	_, e := c.adapter.apply(ctx, "start", c.key, c.token, 0)
	if e == nil {
		c.started = true
	}
	if e != nil {
		return nil, e
	}
	return &capacityCallerExecution{claim: c}, nil
}

type capacityCallerExecution struct{ claim *capacityCallerClaim }

func (execution *capacityCallerExecution) Returned(ctx context.Context) error {
	c := execution.claim
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || c.ended {
		return nil
	}
	e := submitCapacityCleanup(ctx, c.adapter.cleanup, "returned", c.key, c.token)
	if e == nil {
		c.ended = true
		// Assertions below inspect immediately. Production handoff does NOT
		// wait for this worker; keep the deterministic observation test-only.
		e = c.adapter.cleanup.Recover(ctx)
	}
	return e
}
func (c *capacityCallerClaim) Cancel(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.cancelled {
		return nil
	}
	e := submitCapacityCleanup(ctx, c.adapter.cleanup, "cancel", c.key, c.token)
	if e == nil {
		c.cancelled = true
		e = c.adapter.cleanup.Recover(ctx)
	}
	return e
}

func capacityCaller(t *testing.T, a *sharedcapacity.Authority, id string) (*SealCalls, *TaskStorage) {
	t.Helper()
	root := t.TempDir()
	b, e := json.Marshal(storiface.LocalStorageMeta{ID: storiface.ID(id), CanSeal: true, Weight: 1})
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(filepath.Join(root, paths.MetaFile), b, 0600))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	idx := &capacityCallerIndex{}
	local, e := paths.NewLocal(ctx, &capacityCallerStorage{storiface.StorageConfig{StoragePaths: []storiface.LocalPath{{Path: root}}}}, idx, "")
	require.NoError(t, e)
	remote, e := paths.NewRemote(local, idx, nil, 1, nil)
	require.NoError(t, e)
	sb := NewSealCalls(remote, local, idx)
	cleanup, e := sharedcapacity.OpenCleanupJournal(a, t.TempDir(), id+"-cleanup", true)
	require.NoError(t, e)
	t.Cleanup(func() { require.NoError(t, cleanup.Close()) })
	sb.Sectors.capacity = &capacityCallerAdapter{a: a, client: id, journal: filepath.Join(root, "intent.json"), envelope: 64 << 20, cleanup: cleanup}
	storage := sb.Storage(func(id harmonytask.TaskID) (SectorRef, error) {
		return SectorRef{SpID: 1000, SectorNumber: int64(id), RegSealProof: 8}, nil
	}, storiface.FTCache, storiface.FTNone, 32<<30, storiface.PathSealing, 0).ForSDR()
	return sb, storage
}

func callerAuthority(t *testing.T, slots int) (*sharedcapacity.Authority, func() sharedcapacity.State) {
	t.Helper()
	dir := t.TempDir()
	id := sharedcapacity.Identity{Host: "fixture", Filesystem: "shared-fixture"}
	policy := sharedcapacity.Policy{Protocol: sharedcapacity.VERSION, Margin: 32 << 20}
	require.NoError(t, sharedcapacity.Initialize(dir, id, policy))
	a, e := sharedcapacity.Open(dir, id, policy, func(context.Context, sharedcapacity.State) (sharedcapacity.Sample, error) {
		return sharedcapacity.Sample{Identity: id, Free: int64(slots)*64<<20 + policy.Margin}, nil
	})
	require.NoError(t, e)
	t.Cleanup(func() { _ = a.Close() })
	return a, func() sharedcapacity.State {
		s, _, e := a.Inspect(context.Background())
		require.NoError(t, e)
		return s
	}
}

func TestCapacityCallerClaimDenialCancelAndResidency(t *testing.T) {
	a, state := callerAuthority(t, 2)
	sb, storage := capacityCaller(t, a, "caller")
	_, e := storage.Claim(1)
	require.NoError(t, e)
	release, e := storage.Claim(2)
	require.NoError(t, e)
	_, e = storage.Claim(3)
	require.ErrorIs(t, e, sharedcapacity.ErrCapacity)
	require.Len(t, state().Entries, 2)
	require.NoError(t, release())
	require.Len(t, state().Entries, 1)
	res, _ := sb.Sectors.storageReservations.Load(1)
	commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
	require.NoError(t, sb.generateSDR(context.Background(), 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, writeSDRTestLayers, nil, sdrscratch.Options{}))
	require.Equal(t, "resident", state().Entries["1000/1"].State)
	require.NoError(t, storage.markComplete(1, nil))
	require.Contains(t, state().Entries, "1000/1", "scheduler release cannot refund published residency")
	stat, e := sb.Sectors.localStore.FsStat(context.Background(), "caller")
	require.NoError(t, e)
	require.Zero(t, stat.Reserved, "not double charged through old process-local reservation")
	_, e = storage.Claim(3)
	require.NoError(t, e)
	_, e = storage.Claim(4)
	require.ErrorIs(t, e, sharedcapacity.ErrCapacity)
	require.NoError(t, storage.markComplete(3, nil))
}

func TestCapacityCallerCancellationIsNotNativeReturn(t *testing.T) {
	a, state := callerAuthority(t, 2)
	sb, storage := capacityCaller(t, a, "caller")
	release, e := storage.Claim(1)
	require.NoError(t, e)
	res, _ := sb.Sectors.storageReservations.Load(1)
	commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- sb.generateSDR(ctx, 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, func(p abi.RegisteredSealProof, d string, r [32]byte) error {
			close(entered)
			<-finish
			return writeSDRTestLayers(p, d, r)
		}, nil, sdrscratch.Options{})
	}()
	<-entered
	cancel()
	require.Equal(t, "running", state().Entries["1000/1"].State)
	// Explicit early scheduler release must not fabricate a native return.
	res[0].Release()
	require.Equal(t, "running", state().Entries["1000/1"].State)
	close(finish)
	require.NoError(t, <-done)
	require.Equal(t, "resident", state().Entries["1000/1"].State)
	require.NoError(t, release())
}

func TestCapacityCallerPanicKeepsResponsibility(t *testing.T) {
	a, state := callerAuthority(t, 1)
	sb, storage := capacityCaller(t, a, "caller")
	release, e := storage.Claim(1)
	require.NoError(t, e)
	res, _ := sb.Sectors.storageReservations.Load(1)
	commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
	require.Panics(t, func() {
		_ = sb.generateSDR(context.Background(), 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, func(abi.RegisteredSealProof, string, [32]byte) error { panic("native substitute panic") }, nil, sdrscratch.Options{})
	})
	require.NoError(t, release())
	require.Equal(t, "running", state().Entries["1000/1"].State)
}

func TestCapacityCallerGoexitKeepsResponsibility(t *testing.T) {
	a, state := callerAuthority(t, 1)
	sb, storage := capacityCaller(t, a, "caller")
	release, e := storage.Claim(1)
	require.NoError(t, e)
	res, _ := sb.Sectors.storageReservations.Load(1)
	commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = sb.generateSDR(context.Background(), 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, func(abi.RegisteredSealProof, string, [32]byte) error { runtime.Goexit(); return nil }, nil, sdrscratch.Options{})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Goexit barrier")
	}
	require.NoError(t, release())
	require.Equal(t, "running", state().Entries["1000/1"].State)
}

func TestCapacityCallerReturnedErrorStillRetainsResidency(t *testing.T) {
	for _, mode := range []string{"native-error", "post-publication-error"} {
		t.Run(mode, func(t *testing.T) {
			a, state := callerAuthority(t, 1)
			sb, storage := capacityCaller(t, a, "caller")
			release, e := storage.Claim(1)
			require.NoError(t, e)
			res, _ := sb.Sectors.storageReservations.Load(1)
			commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
			native := writeSDRTestLayers
			if mode == "native-error" {
				native = func(p abi.RegisteredSealProof, d string, r [32]byte) error {
					if e := writeSDRTestLayers(p, d, r); e != nil {
						return e
					}
					return errors.New("native substitute failed")
				}
			} else {
				r, e := paths.NewRemote(&sdrCleanupStore{err: errors.New("post-publication index failure")}, &sdrCleanupIndex{}, nil, 1, nil)
				require.NoError(t, e)
				sb.Sectors.storage = r
			}
			require.Error(t, sb.generateSDR(context.Background(), 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, native, nil, sdrscratch.Options{}))
			require.NoError(t, release())
			require.Equal(t, "resident", state().Entries["1000/1"].State, "return is not permission to refund all bytes")
			_, e = storage.Claim(2)
			require.ErrorIs(t, e, sharedcapacity.ErrCapacity)
		})
	}
}

// Real OS processes and the production Claim/Acquire/generateSDR path, but a
// synthetic free-space oracle and substitute native body. This is a last-permit
// race test, not the requested complete SDR/Trees/Finalize/Move cohort test.
func TestCapacityCallerProcessesLastPermit(t *testing.T) {
	if dir := os.Getenv("CURIO_CAPACITY_CALLER_CHILD"); dir != "" {
		id := os.Getenv("CURIO_CAPACITY_CALLER_ID")
		identity := sharedcapacity.Identity{Host: "fixture", Filesystem: "shared-fixture"}
		policy := sharedcapacity.Policy{Protocol: sharedcapacity.VERSION, Margin: 32 << 20}
		a, e := sharedcapacity.Open(dir, identity, policy, func(context.Context, sharedcapacity.State) (sharedcapacity.Sample, error) {
			return sharedcapacity.Sample{Identity: identity, Free: 96 << 20}, nil
		})
		require.NoError(t, e)
		defer func() { require.NoError(t, a.Close()) }()
		sb, storage := capacityCaller(t, a, id)
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".ready"), nil, 0600))
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, e := os.Stat(filepath.Join(dir, "go")); e == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("start barrier")
			}
			time.Sleep(time.Millisecond)
		}
		taskID := harmonytask.TaskID(1)
		if id == "right" {
			taskID = 2
		}
		release, e := storage.Claim(int(taskID))
		outcome := "denied"
		if e == nil {
			outcome = "resident"
			res, _ := sb.Sectors.storageReservations.Load(taskID)
			commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
			require.NoError(t, sb.generateSDR(context.Background(), taskID, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, writeSDRTestLayers, nil, sdrscratch.Options{}))
			require.NoError(t, release())
		} else {
			require.ErrorIs(t, e, sharedcapacity.ErrCapacity)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".result"), []byte(outcome), 0600))
		return
	}
	dir := t.TempDir()
	identity := sharedcapacity.Identity{Host: "fixture", Filesystem: "shared-fixture"}
	policy := sharedcapacity.Policy{Protocol: sharedcapacity.VERSION, Margin: 32 << 20}
	require.NoError(t, sharedcapacity.Initialize(dir, identity, policy))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dones := make([]chan error, 2)
	for i, id := range []string{"left", "right"} {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCapacityCallerProcessesLastPermit$", "-test.timeout=20s")
		cmd.Env = append(os.Environ(), "CURIO_CAPACITY_CALLER_CHILD="+dir, "CURIO_CAPACITY_CALLER_ID="+id)
		f, e := os.Create(filepath.Join(dir, id+".log"))
		require.NoError(t, e)
		t.Cleanup(func() { _ = f.Close() })
		cmd.Stdout, cmd.Stderr = f, f
		require.NoError(t, cmd.Start())
		dones[i] = make(chan error, 1)
		go func(done chan error) { done <- cmd.Wait() }(dones[i])
	}
	require.Eventually(t, func() bool {
		_, a := os.Stat(filepath.Join(dir, "left.ready"))
		_, b := os.Stat(filepath.Join(dir, "right.ready"))
		return a == nil && b == nil
	}, 15*time.Second, time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), nil, 0600))
	admitted := 0
	for i, id := range []string{"left", "right"} {
		e := <-dones[i]
		if e != nil {
			b, _ := os.ReadFile(filepath.Join(dir, id+".log"))
			t.Fatalf("child %s: %v\n%s", id, e, b)
		}
		b, e := os.ReadFile(filepath.Join(dir, id+".result"))
		require.NoError(t, e)
		if string(b) == "resident" {
			admitted++
		} else {
			require.Equal(t, "denied", string(b))
		}
	}
	require.Equal(t, 1, admitted)
}
