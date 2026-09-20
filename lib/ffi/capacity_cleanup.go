package ffi

import (
	"context"
	"fmt"

	"github.com/filecoin-project/curio/lib/sharedcapacity"
)

// capacityCleanupError is an unacknowledged handoff, NOT a release. The caller
// must keep the claim/quarantine. Identity is retained for a retry of the same
// durable operation; neither this helper nor recovery launches native work.
type capacityCleanupError struct {
	Operation, Sector, Token, RecoveryID string
	Outcome                              string
	Err                                  error
}

func (e *capacityCleanupError) Error() string {
	return fmt.Sprintf("capacity %s cleanup %s (%s): %v", e.Operation, e.Outcome, e.RecoveryID, e.Err)
}
func (e *capacityCleanupError) Unwrap() error { return e.Err }

func submitCapacityCleanup(ctx context.Context, owner *sharedcapacity.CleanupJournal, op, sector, token string) error {
	if owner == nil {
		return fmt.Errorf("capacity cleanup has no durable recovery owner")
	}
	result, err := owner.Submit(ctx, op, sector, token)
	if err != nil || !result.Durable {
		return &capacityCleanupError{Operation: op, Sector: sector, Token: token, RecoveryID: result.ID, Outcome: "unknown", Err: err}
	}
	// Accepted by the exclusive persistent recovery owner, not a statement that
	// Cancel/Returned has already applied. Budget remains until that transition.
	return nil
}
