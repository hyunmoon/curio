package webrpc

import "time"

// Keep this read-only classification in step with taskTookAge. A claimed row
// can legitimately have no token yet; a Do-entry row cannot.
const clusterTaskTookStateSQL = `CASE
 WHEN owner_id IS NULL THEN 'pending'
 WHEN attempt_session IS NULL OR process_session IS NULL OR attempt_session = '' THEN 'unknown'
 WHEN attempt_session IS DISTINCT FROM process_session THEN 'previous-process'
 WHEN name='SDR' AND COALESCE(sdr_references,0)=0 THEN 'unreferenced'
 WHEN name='SDR' AND (sdr_references<>1 OR NOT sdr_ready) THEN 'invalid-reference'
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
	if !row.AttemptSession.Valid || row.AttemptSession.String == "" || !row.ProcessSession.Valid {
		return nil, "unknown"
	}
	if row.AttemptSession.String != row.ProcessSession.String {
		return nil, "previous-process"
	}
	if row.Name == "SDR" {
		if !row.SDRReferences.Valid {
			return nil, "unknown"
		}
		if row.SDRReferences.Int64 == 0 {
			return nil, "unreferenced"
		}
		if row.SDRReferences.Int64 != 1 || !row.SDRReady {
			return nil, "invalid-reference"
		}
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
