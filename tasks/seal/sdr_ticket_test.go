package seal

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/crypto"

	"github.com/filecoin-project/lotus/chain/actors/policy"
	"github.com/filecoin-project/lotus/chain/types"
)

type ticketValidationAPI struct {
	ts         *types.TipSet
	randomness abi.Randomness
	err        error
	requested  abi.ChainEpoch
}

func (a *ticketValidationAPI) ChainHead(context.Context) (*types.TipSet, error) { return a.ts, a.err }
func (a *ticketValidationAPI) StateGetRandomnessFromTickets(_ context.Context, _ crypto.DomainSeparationTag, e abi.ChainEpoch, _ []byte, _ types.TipSetKey) (abi.Randomness, error) {
	a.requested = e
	return a.randomness, a.err
}

func TestSDRPublishedTicketValidity(t *testing.T) {
	miner, err := address.NewIDAddress(1000)
	require.NoError(t, err)
	ts := porepLifecycleTipSet(t)
	epoch := ts.Height() - policy.SealRandomnessLookback
	for _, name := range []string{"valid-original", "expired", "future", "reorg", "api-error"} {
		t.Run(name, func(t *testing.T) {
			api := &ticketValidationAPI{ts: ts, randomness: make([]byte, 32)}
			ticket, e := make([]byte, 32), epoch
			switch name {
			case "expired":
				e = ts.Height() - policy.MaxPreCommitRandomnessLookback - 1
			case "future":
				e = ts.Height() - policy.SealRandomnessLookback + 1
			case "reorg":
				api.randomness[0] = 1
			case "api-error":
				api.err = errors.New("unavailable")
			}
			err := validateSDRTicket(context.Background(), api, miner, ticket, e)
			if name == "valid-original" {
				require.NoError(t, err)
				require.Equal(t, epoch, api.requested)
			} else {
				require.Error(t, err)
			}
		})
	}
}
