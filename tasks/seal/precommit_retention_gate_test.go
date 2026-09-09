package seal

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/builtin"
	verifregtypes9 "github.com/filecoin-project/go-state-types/builtin/v9/verifreg"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/actors/policy"
	"github.com/filecoin-project/lotus/chain/types"
)

type precommitRetentionAllocationAPI struct {
	allocation *verifregtypes9.Allocation
}

func (a precommitRetentionAllocationAPI) StateGetAllocation(context.Context, address.Address, verifregtypes9.AllocationId, types.TipSetKey) (*verifregtypes9.Allocation, error) {
	return a.allocation, nil
}

// This is an executable boundary demonstration, not actor execution or proof
// that later sector/claim extensions will satisfy the requested retention.
func TestPrecommitClampDoesNotEstablishAllocationOrRetention(t *testing.T) {
	const precommitHead, activationHead = abi.ChainEpoch(1000), abi.ChainEpoch(1100)
	maxExtension, err := policy.GetMaxSectorExpirationExtension(network.Version28)
	require.NoError(t, err)
	requestedDuration := abi.ChainEpoch(builtin.EpochsInFiveYears)
	requestedEnd := activationHead + requestedDuration
	pieces := []precommitPiece{makeDirectPrecommitPiece(activationHead, requestedEnd)}
	expiration, failure := calculatePrecommitExpiration(precommitHead, precommitHead, 0,
		maxExtension, sql.NullInt64{}, pieces)
	require.Nil(t, failure)
	require.Equal(t, precommitHead+maxExtension, expiration)
	require.EqualValues(t, requestedEnd, pieces[0].DirectDealEndEpoch.Int64)

	commitment, err := cid.Parse(makePrecommitSector(1).UnsealedCID)
	require.NoError(t, err)
	piece := &miner.PieceActivationManifest{CID: commitment, Size: 128}
	piece.VerifiedAllocationKey = &miner.VerifiedAllocationKey{Client: 1001, ID: 1}
	allocation := &verifregtypes9.Allocation{Provider: 1000, Client: 1001,
		Size: piece.Size, TermMin: requestedDuration, TermMax: requestedDuration}
	permanent, err := AllocationCheck(context.Background(), precommitRetentionAllocationAPI{allocation},
		piece, expiration, 1000, makePrecommitTipSet(t, activationHead))
	require.True(t, permanent)
	require.ErrorContains(t, err, "allocation TermMin")

	allocation.TermMin = 180 * builtin.EpochsInDay
	permanent, err = AllocationCheck(context.Background(), precommitRetentionAllocationAPI{allocation},
		piece, expiration, 1000, makePrecommitTipSet(t, activationHead))
	require.False(t, permanent)
	require.NoError(t, err)
	require.Less(t, expiration, requestedEnd, "valid initial activation still leaves a requested-retention gap")
}
