package ffi

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/sharedcapacity"
	"github.com/filecoin-project/curio/lib/storiface"
)

// Independently reconstructed from Review3, not the unavailable evidence ZIP.
// Actual Claim/Acquire/generateSDR + real ledger; only native/index/sampler are
// substitutes. Duplicate direct invocation is NOT proven scheduler reachability.
func TestReview3RejectedInvocationCannotReturnLiveWriter(t *testing.T) {
	a, state := callerAuthority(t, 2)
	sb, storage := capacityCaller(t, a, "review3")
	release, err := storage.Claim(1)
	require.NoError(t, err)
	res, _ := sb.Sectors.storageReservations.Load(1)
	commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
	entered, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- sb.generateSDR(context.Background(), 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, func(p abi.RegisteredSealProof, d string, r [32]byte) error {
			close(entered)
			<-finish
			return writeSDRTestLayers(p, d, r)
		}, nil, sdrscratch.Options{})
	}()
	<-entered
	defer func() { close(finish); require.NoError(t, <-done); require.NoError(t, release()) }()
	var second atomic.Bool
	err = sb.generateSDR(context.Background(), 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, func(abi.RegisteredSealProof, string, [32]byte) error { second.Store(true); return nil }, nil, sdrscratch.Options{})
	require.Error(t, err)
	require.False(t, second.Load())
	require.Equal(t, "running", state().Entries["1000/1"].State, "rejected invocation falsely certified another live writer's return")
	_, err = sb.Sectors.capacity.Reserve(context.Background(), res[0].SectorRef, res[0].Paths, res[0].PathIDs, true)
	require.ErrorIs(t, err, sharedcapacity.ErrBusy)
}

func TestReview3PreentryAndReplacementNeverReturnAnotherInvocation(t *testing.T) {
	a, state := callerAuthority(t, 3)
	sb, storage := capacityCaller(t, a, "bound-claim")
	release, err := storage.Claim(1)
	require.NoError(t, err)
	res, _ := sb.Sectors.storageReservations.Load(1)
	commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
	for _, into := range []storiface.SectorFileType{storiface.FTSealed, storiface.FTKey} {
		err := sb.generateSDR(context.Background(), 1, into, res[0].SectorRef.Ref(), make([]byte, 32), commD, writeSDRTestLayers, nil, sdrscratch.Options{})
		require.Error(t, err)
		require.Equal(t, "tentative", state().Entries["1000/1"].State)
	}
	_, err = storage.Claim(1)
	require.Error(t, err, "duplicate TaskStorage claim must not replace a live reservation")
	ctx := context.WithValue(context.Background(), capacityInvocationKey{}, &capacityInvocation{reservation: res[0]})
	replacement := &StorageReservation{SectorRef: res[0].SectorRef, Alloc: res[0].Alloc, Paths: res[0].Paths, PathIDs: res[0].PathIDs, SDR: res[0].SDR}
	sb.Sectors.storageReservations.Store(1, []*StorageReservation{replacement})
	id := harmonytask.TaskID(1)
	_, _, _, err = sb.Sectors.AcquireSector(ctx, &id, res[0].SectorRef.Ref(), storiface.FTNone, storiface.FTCache, storiface.PathSealing)
	require.ErrorContains(t, err, "replaced")
	require.NoError(t, release())
	current, ok := sb.Sectors.storageReservations.Load(1)
	require.True(t, ok)
	require.Same(t, replacement, current[0], "late release removed replacement")
	sb.Sectors.storageReservations.Delete(1)
	require.NoError(t, release())
	_, ok = sb.Sectors.storageReservations.Load(1)
	require.False(t, ok, "idempotent late cleanup inserted nil reservation")
}

func TestReview3TaskStorageUnknownCleanupRetainsClaim(t *testing.T) {
	a, _ := callerAuthority(t, 2)
	sb, storage := capacityCaller(t, a, "cleanup-claim")
	release, err := storage.Claim(1)
	require.NoError(t, err)
	res, _ := sb.Sectors.storageReservations.Load(1)
	original := res[0].cleanup
	res[0].cleanup = func() error { return errors.New("Cancel outcome unknown") }
	require.Error(t, release())
	current, ok := sb.Sectors.storageReservations.Load(1)
	require.True(t, ok)
	require.Same(t, res[0], current[0])
	res[0].cleanup = original
	require.NoError(t, release())
	_, ok = sb.Sectors.storageReservations.Load(1)
	require.False(t, ok)
}

type review3Claim struct {
	capacityClaim
	afterBegin func(capacityExecution, error) (capacityExecution, error)
}

func (c review3Claim) Begin(ctx context.Context) (capacityExecution, error) {
	e, err := c.capacityClaim.Begin(ctx)
	return c.afterBegin(e, err)
}

type review3Execution struct {
	capacityExecution
	unavailable *atomic.Bool
}

func (e review3Execution) Returned(ctx context.Context) error {
	if e.unavailable.Load() {
		return errors.New("return intent not durably accepted")
	}
	return e.capacityExecution.Returned(ctx)
}

func TestReview3UnknownStartDoesNotCertifyReturn(t *testing.T) {
	a, state := callerAuthority(t, 2)
	sb, storage := capacityCaller(t, a, "unknown-start")
	release, err := storage.Claim(1)
	require.NoError(t, err)
	defer func() { require.NoError(t, release()) }()
	res, _ := sb.Sectors.storageReservations.Load(1)
	res[0].capacity = review3Claim{capacityClaim: res[0].capacity, afterBegin: func(_ capacityExecution, err error) (capacityExecution, error) {
		require.NoError(t, err)
		return nil, errors.New("Start response lost")
	}}
	commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
	var native atomic.Bool
	err = sb.generateSDR(context.Background(), 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, func(abi.RegisteredSealProof, string, [32]byte) error { native.Store(true); return nil }, nil, sdrscratch.Options{})
	require.ErrorContains(t, err, "Start response lost")
	require.False(t, native.Load())
	require.Equal(t, "running", state().Entries["1000/1"].State, "unknown Start retains responsibility; no automatic native replay")
}

func TestReview3ReturnedIntentFailureRetainsExactExecutionForRetry(t *testing.T) {
	a, state := callerAuthority(t, 2)
	sb, storage := capacityCaller(t, a, "return-retry")
	release, err := storage.Claim(1)
	require.NoError(t, err)
	res, _ := sb.Sectors.storageReservations.Load(1)
	var fail atomic.Bool
	fail.Store(true)
	res[0].capacity = review3Claim{capacityClaim: res[0].capacity, afterBegin: func(e capacityExecution, err error) (capacityExecution, error) {
		return review3Execution{capacityExecution: e, unavailable: &fail}, err
	}}
	commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
	var native atomic.Int32
	err = sb.generateSDR(context.Background(), 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, func(p abi.RegisteredSealProof, d string, r [32]byte) error {
		native.Add(1)
		return writeSDRTestLayers(p, d, r)
	}, nil, sdrscratch.Options{})
	require.Error(t, err)
	require.Equal(t, "running", state().Entries["1000/1"].State)
	require.Error(t, release())
	_, ok := sb.Sectors.storageReservations.Load(1)
	require.True(t, ok)
	fail.Store(false)
	require.NoError(t, release())
	require.Equal(t, "resident", state().Entries["1000/1"].State)
	_, ok = sb.Sectors.storageReservations.Load(1)
	require.False(t, ok)
	require.EqualValues(t, 1, native.Load(), "recovery cannot re-execute native")
}

func TestReview3RepeatedAcquireIsScopedToOneInvocation(t *testing.T) {
	a, state := callerAuthority(t, 2)
	sb, storage := capacityCaller(t, a, "repeat-acquire")
	release, err := storage.Claim(1)
	require.NoError(t, err)
	defer func() { require.NoError(t, release()) }()
	res, _ := sb.Sectors.storageReservations.Load(1)
	scope := &capacityInvocation{reservation: res[0]}
	ctx := context.WithValue(context.Background(), capacityInvocationKey{}, scope)
	id := harmonytask.TaskID(1)
	for range 2 {
		_, _, done, err := sb.Sectors.AcquireSector(ctx, &id, res[0].SectorRef.Ref(), storiface.FTNone, storiface.FTCache, storiface.PathSealing)
		require.NoError(t, err)
		done(storiface.FTCache)
	}
	require.Equal(t, "running", state().Entries["1000/1"].State)
	require.NoError(t, scope.returned(ctx))
	_, _, _, err = sb.Sectors.AcquireSector(ctx, &id, res[0].SectorRef.Ref(), storiface.FTNone, storiface.FTCache, storiface.PathSealing)
	require.ErrorContains(t, err, "already returned")
}
