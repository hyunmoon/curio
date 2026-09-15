package seal

import (
	"bytes"
	"context"
	"fmt"

	"github.com/ipfs/go-cid"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/crypto"

	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/storiface"

	"github.com/filecoin-project/lotus/chain/actors/policy"
)

func (s *SDRTask) sdrTicket(ctx context.Context, id harmonytask.TaskID, sector storiface.SectorRef, d cid.Cid, miner address.Address) (abi.SealRandomness, abi.ChainEpoch, error) {
	previous, err := s.sc.SDRTicket(id, sector, d)
	if err != nil {
		return nil, 0, err
	}
	if previous == nil {
		return GetTicket(ctx, s.api, miner)
	}
	if err := validateSDRTicket(ctx, s.api, miner, previous.Value, previous.Epoch); err != nil {
		return nil, 0, err
	}
	return previous.Value, previous.Epoch, nil
}

// A successful prior cache is usable only with its original, still-valid
// randomness. Do not relabel old labels with the latest head's ticket.
func validateSDRTicket(ctx context.Context, api TicketNodeAPI, miner address.Address, ticket abi.SealRandomness, epoch abi.ChainEpoch) error {
	ts, err := api.ChainHead(ctx)
	if err != nil {
		return err
	}
	if epoch < ts.Height()-policy.MaxPreCommitRandomnessLookback || epoch > ts.Height()-policy.SealRandomnessLookback {
		return fmt.Errorf("published SDR ticket epoch %d is outside the current sealing window", epoch)
	}
	var entropy bytes.Buffer
	if err := miner.MarshalCBOR(&entropy); err != nil {
		return err
	}
	value, err := api.StateGetRandomnessFromTickets(ctx, crypto.DomainSeparationTag_SealRandomness, epoch, entropy.Bytes(), ts.Key())
	if err != nil {
		return err
	}
	if !bytes.Equal(ticket, value) {
		return fmt.Errorf("published SDR ticket no longer matches chain randomness")
	}
	return nil
}
