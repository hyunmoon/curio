package webrpc

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Same source-level assertion also runs on the reviewed base: absence of
// current-process evidence must not turn a forty-hour-old Do entry into Running.
func TestLegacyAttemptIsNotConfirmedCurrentExecution(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	owner := int64(1)
	r := clusterTaskSummaryLimitedRow{ID: 1, Name: "SDR", OwnerID: &owner,
		AttemptID: sql.NullString{String: "old", Valid: true}, AttemptStartSource: sql.NullString{String: "do_entry", Valid: true},
		AttemptStartedAt: sql.NullTime{Time: now.Add(-43 * time.Hour), Valid: true}}
	age, state := taskTookAge(r, now)
	require.Nil(t, age)
	require.Equal(t, "unknown", state)
}

func TestTaskLifetimeClassification(t *testing.T) {
	now := time.Now().UTC()
	owner := int64(1)
	r := clusterTaskSummaryLimitedRow{Name: "SDR", OwnerID: &owner, AttemptID: sql.NullString{String: "a", Valid: true},
		AttemptStartSource: sql.NullString{String: "do_entry", Valid: true}, AttemptStartedAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
		AttemptSession: sql.NullString{String: "old", Valid: true}, ProcessSession: sql.NullString{String: "new", Valid: true},
		SDRReferences: sql.NullInt64{Int64: 1, Valid: true}, SDRReady: true}
	_, state := taskTookAge(r, now)
	require.Equal(t, "previous-process", state)
	r.AttemptSession = r.ProcessSession
	age, state := taskTookAge(r, now)
	require.Equal(t, "running", state)
	require.EqualValues(t, 3600, *age)
	r.SDRReferences.Int64 = 0
	_, state = taskTookAge(r, now)
	require.Equal(t, "unreferenced", state)
	r.SDRReferences.Int64 = 2
	_, state = taskTookAge(r, now)
	require.Equal(t, "invalid-reference", state)
	r.SDRReferences.Int64 = 1
	r.SDRReady = false
	_, state = taskTookAge(r, now)
	require.Equal(t, "invalid-reference", state)
	r.SDRReferences = sql.NullInt64{}
	_, state = taskTookAge(r, now)
	require.Equal(t, "unknown", state)
}

func TestWaitingProvenance(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 8, 57, 0, time.UTC)
	r := clusterTaskSummaryLimitedRow{Name: "SDR", PostedTime: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		SDRReferences: sql.NullInt64{Int64: 1, Valid: true}, SectorCreated: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}}
	originalPosted := r.PostedTime
	age, state := taskWaitingAge(r, now)
	require.Nil(t, age)
	require.Equal(t, "priority-before-sector", state)
	r.CreatedAt = sql.NullTime{Time: now.Add(-30 * time.Minute), Valid: true}
	r.QueuedAt = r.CreatedAt
	age, state = taskWaitingAge(r, now)
	require.Equal(t, "queue-entry", state)
	require.EqualValues(t, 1800, *age)
	// Retry re-entry, not posted_time, update_time or a sector timestamp.
	r.QueuedAt.Time = now.Add(-time.Minute)
	age, _ = taskWaitingAge(r, now)
	require.EqualValues(t, 60, *age)
	r.CreatedAt.Time = r.PostedTime
	r.QueuedAt = r.CreatedAt
	r.SectorCreated.Time = r.PostedTime.Add(-time.Hour)
	age, state = taskWaitingAge(r, now)
	require.Equal(t, "queue-entry", state)
	require.Greater(t, *age, int64(50000*3600))
	r.QueuedAt.Time = now.Add(time.Second)
	age, state = taskWaitingAge(r, now)
	require.Nil(t, age)
	require.Equal(t, "future-queue", state)
	r.QueuedAt = r.CreatedAt
	r.SectorCreated.Time = now
	age, state = taskWaitingAge(r, now)
	require.Nil(t, age)
	require.Equal(t, "relinked-or-inconsistent", state)
	require.Equal(t, originalPosted, r.PostedTime)
}

func TestWaitingMissingAndFutureLegacyTimes(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		posted time.Time
		state  string
	}{
		{time.Time{}, "missing-posted"}, {now.Add(time.Hour), "future-posted"}, {now.Add(-time.Hour), "unknown-provenance"},
	} {
		age, state := taskWaitingAge(clusterTaskSummaryLimitedRow{PostedTime: tc.posted}, now)
		require.Nil(t, age)
		require.Equal(t, tc.state, state)
	}
}
