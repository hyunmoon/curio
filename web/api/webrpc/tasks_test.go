package webrpc

import (
	"database/sql"
	"testing"
	"time"
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
