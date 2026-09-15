package webrpc

import "time"

// Keep this read-only classification in step with taskTookAge. A claimed row
// can legitimately have no token yet; a Do-entry row cannot.
const clusterTaskTookStateSQL = `CASE
 WHEN owner_id IS NULL THEN 'pending'
 WHEN attempt_start_source IN ('claimed', 'prepared') THEN
   CASE WHEN attempt_started_at IS NULL THEN 'awaiting-start' ELSE 'unknown' END
 WHEN attempt_id IS NULL OR attempt_id = '' OR attempt_started_at IS NULL
   OR attempt_start_source IS DISTINCT FROM 'do_entry' THEN 'unknown'
 WHEN attempt_started_at > observed_at THEN 'future-start'
 ELSE 'running' END`

func taskTookAge(row clusterTaskSummaryLimitedRow, observedAt time.Time) (*int64, string) {
	if row.OwnerID == nil {
		return nil, "pending"
	}
	if row.AttemptStartSource.Valid && (row.AttemptStartSource.String == "claimed" || row.AttemptStartSource.String == "prepared") {
		if row.AttemptStartedAt.Valid {
			return nil, "unknown"
		}
		return nil, "awaiting-start"
	}
	if !row.AttemptID.Valid || row.AttemptID.String == "" || !row.AttemptStartedAt.Valid || !row.AttemptStartSource.Valid || row.AttemptStartSource.String != "do_entry" {
		return nil, "unknown"
	}
	if row.AttemptStartedAt.Time.After(observedAt) {
		return nil, "future-start"
	}
	age := int64(observedAt.Sub(row.AttemptStartedAt.Time) / time.Second)
	return &age, "running"
}
