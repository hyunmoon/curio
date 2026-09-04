package webrpc

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/harmony/harmonytask"
)

func TestTaskSummaryRowSummary(t *testing.T) {
	now := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	posted := now.Add(-2 * time.Hour)
	started := now.Add(-5 * time.Minute)
	ownerID := "42"

	tests := []struct {
		name      string
		row       taskSummaryRow
		wantState string
		wantAge   string
		wantErr   bool
	}{
		{
			name: "pending uses posted time",
			row: taskSummaryRow{
				ID:         1,
				PostedTime: posted,
				WorkStart:  sql.NullTime{Time: started, Valid: true},
			},
			wantState: "pending",
			wantAge:   "2h0m0s",
		},
		{
			name: "running uses work start",
			row: taskSummaryRow{
				ID:         2,
				PostedTime: posted,
				WorkStart:  sql.NullTime{Time: started, Valid: true},
				OwnerID:    &ownerID,
			},
			wantState: "running",
			wantAge:   "5m0s",
		},
		{
			name: "running requires work start",
			row: taskSummaryRow{
				ID:         3,
				PostedTime: posted,
				OwnerID:    &ownerID,
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary, err := test.row.summary(now)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if summary.State != test.wantState {
				t.Fatalf("state = %q, want %q", summary.State, test.wantState)
			}
			if summary.Age != test.wantAge {
				t.Fatalf("age = %q, want %q", summary.Age, test.wantAge)
			}
		})
	}
}

func TestSortTaskSummaryRowsByAgeStart(t *testing.T) {
	base := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	ownerID := "42"
	rows := []taskSummaryRow{
		{ID: 4, Name: "same-time-high-id", PostedTime: base.Add(-20 * time.Minute)},
		{ID: 6, Name: "pending-23m", PostedTime: base.Add(-23 * time.Minute)},
		{ID: 9, Name: "running-25m", PostedTime: base.Add(-2 * time.Hour), WorkStart: sql.NullTime{Time: base.Add(-25 * time.Minute), Valid: true}, OwnerID: &ownerID},
		{ID: 5, Name: "pending-27m", PostedTime: base.Add(-27 * time.Minute)},
		{ID: 3, Name: "same-time-low-id", PostedTime: base.Add(-20 * time.Minute)},
		{ID: 7, Name: "pending-24m", PostedTime: base.Add(-24 * time.Minute)},
		{ID: 2, Name: "pending-26m", PostedTime: base.Add(-26 * time.Minute)},
	}

	if err := sortTaskSummaryRows(rows); err != nil {
		t.Fatal(err)
	}

	want := []int64{5, 2, 9, 7, 6, 3, 4}
	for i, id := range want {
		if rows[i].ID != id {
			t.Fatalf("row %d ID = %d, want %d", i, rows[i].ID, id)
		}
	}
}

type recordingSpidGetter struct {
	calls   [][]int64
	resolve func(int64) []int64
}

func (g *recordingSpidGetter) GetSpids(_ context.Context, _ *harmonydb.DB, taskIDs []int64) ([]harmonytask.TaskSPID, error) {
	call := append([]int64(nil), taskIDs...)
	g.calls = append(g.calls, call)

	var result []harmonytask.TaskSPID
	for _, taskID := range taskIDs {
		for _, spid := range g.resolve(taskID) {
			result = append(result, harmonytask.TaskSPID{TaskID: taskID, SPID: spid})
		}
	}
	return result, nil
}

func TestBuildTaskSummariesBulkSpidResolution(t *testing.T) {
	const TASK_COUNT = 10_000

	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	rows := make([]taskSummaryRow, TASK_COUNT)
	for i := range rows {
		rows[i] = taskSummaryRow{
			ID:         int64(TASK_COUNT - i),
			Name:       "SDR",
			PostedTime: now.Add(-time.Hour),
		}
	}

	getter := &recordingSpidGetter{resolve: func(taskID int64) []int64 {
		return []int64{1000 + taskID}
	}}
	summaries, err := buildTaskSummaries(context.Background(), nil, rows, map[string]SpidGetter{"SDR": getter}, now)
	if err != nil {
		t.Fatal(err)
	}

	if len(getter.calls) != 1 {
		t.Fatalf("bulk resolver calls = %d, want 1", len(getter.calls))
	}
	if len(getter.calls[0]) != TASK_COUNT {
		t.Fatalf("resolved task count = %d, want %d", len(getter.calls[0]), TASK_COUNT)
	}
	if len(summaries) != TASK_COUNT {
		t.Fatalf("summary count = %d, want %d", len(summaries), TASK_COUNT)
	}
	if summaries[0].ID != 1 || summaries[0].SpID != "1001" || summaries[0].Miner != "f01001" {
		t.Fatalf("first summary = %+v", summaries[0])
	}
	if summaries[TASK_COUNT-1].ID != TASK_COUNT || summaries[TASK_COUNT-1].SpID != "11000" || summaries[TASK_COUNT-1].Miner != "f011000" {
		t.Fatalf("last summary = %+v", summaries[TASK_COUNT-1])
	}
}

func TestBuildTaskSummariesMissingSpidTypeIsSafe(t *testing.T) {
	now := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	rows := []taskSummaryRow{{ID: 1, Name: "NoSpid", PostedTime: now.Add(-time.Minute)}}

	summaries, err := buildTaskSummaries(context.Background(), nil, rows, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summary count = %d, want 1", len(summaries))
	}
	if summaries[0].SpID != "" || summaries[0].Miner != "" {
		t.Fatalf("unexpected SpID data: %+v", summaries[0])
	}
}

func TestResolveTaskSPIDsChoosesDeterministicMinimum(t *testing.T) {
	getter := &recordingSpidGetter{resolve: func(int64) []int64 {
		return []int64{300, 100, 200}
	}}
	rows := []taskSummaryRow{{ID: 1, Name: "SDR"}}

	spids, err := resolveTaskSPIDs(context.Background(), nil, rows, map[string]SpidGetter{"SDR": getter})
	if err != nil {
		t.Fatal(err)
	}
	if spids[1] != 100 {
		t.Fatalf("SpID = %d, want 100", spids[1])
	}
}

func TestResolveTaskSPIDsBoundsBatchSize(t *testing.T) {
	rows := make([]taskSummaryRow, CLUSTER_TASK_SPID_BATCH_SIZE+1)
	for i := range rows {
		rows[i] = taskSummaryRow{ID: int64(i + 1), Name: "SDR"}
	}
	getter := &recordingSpidGetter{resolve: func(int64) []int64 { return nil }}

	_, err := resolveTaskSPIDs(context.Background(), nil, rows, map[string]SpidGetter{"SDR": getter})
	if err != nil {
		t.Fatal(err)
	}
	if len(getter.calls) != 2 {
		t.Fatalf("bulk resolver calls = %d, want 2", len(getter.calls))
	}
	if len(getter.calls[0]) != CLUSTER_TASK_SPID_BATCH_SIZE || len(getter.calls[1]) != 1 {
		t.Fatalf("batch sizes = %d, %d", len(getter.calls[0]), len(getter.calls[1]))
	}
}
