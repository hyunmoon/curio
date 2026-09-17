package webrpc

import "time"

func taskExecutionReason(state string) string {
	switch state {
	case "previous-process":
		return "Attempt belongs to a previous worker process; recovery/termination is unconfirmed."
	case "unreferenced":
		return "No SDR pipeline references this task in the snapshot; execution termination is unconfirmed."
	case "invalid-reference":
		return "SDR pipeline reference is ambiguous, failed or already past SDR; not confirmed current execution."
	case "unknown":
		return "Current process/attempt provenance is unavailable."
	default:
		return ""
	}
}

// posted_time remains a FIFO/priority key, not an authoritative waiting clock.
// queued_at records the last transition into the unowned queue, including retry.
// Existing rows receive no invented backfill. A new link to a newer sector does
// not make the old task's queue age evidence for that sector's wait.
func taskWaitingAge(row clusterTaskSummaryLimitedRow, now time.Time) (*int64, string) {
	if row.OwnerID != nil {
		return nil, "not-pending"
	}
	if !row.QueuedAt.Valid || !row.CreatedAt.Valid {
		if row.PostedTime.IsZero() {
			return nil, "missing-posted"
		}
		if row.PostedTime.After(now) {
			return nil, "future-posted"
		}
		if row.SectorCreated.Valid && row.PostedTime.Before(row.SectorCreated.Time) {
			return nil, "priority-before-sector"
		}
		return nil, "unknown-provenance"
	}
	if row.QueuedAt.Time.After(now) || row.CreatedAt.Time.After(now) {
		return nil, "future-queue"
	}
	if row.QueuedAt.Time.Before(row.CreatedAt.Time) {
		return nil, "inconsistent-queue"
	}
	if row.Name == "SDR" {
		if !row.SDRReferences.Valid || row.SDRReferences.Int64 != 1 {
			return nil, "unknown-reference"
		}
		if !row.SectorCreated.Valid || row.CreatedAt.Time.Before(row.SectorCreated.Time) {
			return nil, "relinked-or-inconsistent"
		}
	}
	age := clusterTaskAgeSeconds(now, row.QueuedAt.Time)
	return &age, "queue-entry"
}
