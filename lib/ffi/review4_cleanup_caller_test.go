package ffi

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

type review4LostReturnAck struct {
	claim *capacityCallerClaim
	lost  atomic.Bool
}

func (e *review4LostReturnAck) Returned(ctx context.Context) error {
	c := e.claim
	if err := submitCapacityCleanup(ctx, c.adapter.cleanup, "returned", c.key, c.token); err != nil {
		return err
	}
	if !e.lost.Swap(true) {
		return errors.New("handoff response lost")
	}
	return nil
}

// Actual TaskStorage/Acquire/generateSDR/release with the installed fixture
// backend. Fault is at the acknowledgement boundary, not a real I/O outage.
// Follow-up uses the real authority but does not execute TreeRC/native.
func TestReview4CallerRetriesOldReturnAfterFollowupTokenChange(t *testing.T) {
	ctx := context.Background()
	a, state := callerAuthority(t, 2)
	sb, storage := capacityCaller(t, a, "review4")
	release, err := storage.Claim(1)
	require.NoError(t, err)
	res, _ := sb.Sectors.storageReservations.Load(1)
	claim := res[0].capacity.(*capacityCallerClaim)
	res[0].capacity = review3Claim{capacityClaim: claim, afterBegin: func(_ capacityExecution, err error) (capacityExecution, error) {
		if err != nil {
			return nil, err
		}
		return &review4LostReturnAck{claim: claim}, nil
	}}
	commD := newSDRCleanupFixture(t, storiface.FTCache, "").commD
	err = sb.generateSDR(ctx, 1, storiface.FTCache, res[0].SectorRef.Ref(), make([]byte, 32), commD, writeSDRTestLayers, nil, sdrscratch.Options{})
	require.ErrorContains(t, err, "handoff response lost")
	require.NotNil(t, res[0].pendingReturn)
	require.NoError(t, claim.adapter.cleanup.Recover(ctx))
	next, err := claim.adapter.apply(ctx, "reserve", claim.key, "", claim.adapter.envelope)
	require.NoError(t, err)
	require.NotEqual(t, claim.token, next.Entry.Token)
	before := state()
	require.NoError(t, release())
	require.Nil(t, res[0].pendingReturn)
	_, exists := sb.Sectors.storageReservations.Load(1)
	require.False(t, exists)
	require.Equal(t, before, state(), "old release must not modify follow-up responsibility")
}
