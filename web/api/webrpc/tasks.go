package webrpc

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/samber/lo"
	"github.com/yugabyte/pgx/v5"

	"github.com/filecoin-project/go-address"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/harmony/harmonytask"
)

const CLUSTER_TASK_SPID_BATCH_SIZE = 10_000

type TaskSummary struct {
	ID    int64
	Name  string
	SpID  string
	State string
	Age   string

	Owner, OwnerID *string

	Miner string
}

type taskSummaryRow struct {
	ID         int64
	Name       string
	PostedTime time.Time    `db:"posted_time"`
	WorkStart  sql.NullTime `db:"work_start"`
	Owner      *string
	OwnerID    *string `db:"owner_id"`
}

func (r taskSummaryRow) ageStart() (time.Time, error) {
	if r.OwnerID != nil {
		if !r.WorkStart.Valid {
			return time.Time{}, fmt.Errorf("running task %d has no work_start", r.ID)
		}
		return r.WorkStart.Time, nil
	}
	return r.PostedTime, nil
}

func (r taskSummaryRow) summary(now time.Time) (TaskSummary, error) {
	ageStart, err := r.ageStart()
	if err != nil {
		return TaskSummary{}, err
	}

	age := now.Sub(ageStart)
	if age < 0 {
		age = 0
	}

	state := "pending"
	if r.OwnerID != nil {
		state = "running"
	}

	return TaskSummary{
		ID:      r.ID,
		Name:    r.Name,
		State:   state,
		Age:     age.Truncate(time.Second).String(),
		Owner:   r.Owner,
		OwnerID: r.OwnerID,
	}, nil
}

func sortTaskSummaryRows(rows []taskSummaryRow) error {
	ageStarts := make(map[int64]time.Time, len(rows))
	for _, row := range rows {
		ageStart, err := row.ageStart()
		if err != nil {
			return err
		}
		ageStarts[row.ID] = ageStart
	}

	sort.Slice(rows, func(i, j int) bool {
		left, right := ageStarts[rows[i].ID], ageStarts[rows[j].ID]
		if left.Equal(right) {
			return rows[i].ID < rows[j].ID
		}
		return left.Before(right)
	})
	return nil
}

func resolveTaskSPIDs(ctx context.Context, db *harmonydb.DB, rows []taskSummaryRow, getters map[string]SpidGetter) (map[int64]int64, error) {
	taskIDsByName := make(map[string][]int64)
	taskNames := make([]string, 0)
	for _, row := range rows {
		if _, ok := getters[row.Name]; !ok {
			continue
		}
		if _, ok := taskIDsByName[row.Name]; !ok {
			taskNames = append(taskNames, row.Name)
		}
		taskIDsByName[row.Name] = append(taskIDsByName[row.Name], row.ID)
	}

	spids := make(map[int64]int64)
	for _, name := range taskNames {
		ids := taskIDsByName[name]
		for start := 0; start < len(ids); start += CLUSTER_TASK_SPID_BATCH_SIZE {
			end := min(start+CLUSTER_TASK_SPID_BATCH_SIZE, len(ids))
			resolved, err := getters[name].GetSpids(ctx, db, ids[start:end])
			if err != nil {
				return nil, fmt.Errorf("resolving SpIDs for %s tasks: %w", name, err)
			}

			for _, resolvedTask := range resolved {
				current, ok := spids[resolvedTask.TaskID]
				if !ok || resolvedTask.SPID < current {
					spids[resolvedTask.TaskID] = resolvedTask.SPID
				}
			}
		}
	}

	return spids, nil
}

func buildTaskSummaries(ctx context.Context, db *harmonydb.DB, rows []taskSummaryRow, getters map[string]SpidGetter, now time.Time) ([]TaskSummary, error) {
	if err := sortTaskSummaryRows(rows); err != nil {
		return nil, err
	}

	spids, err := resolveTaskSPIDs(ctx, db, rows, getters)
	if err != nil {
		return nil, err
	}

	ts := make([]TaskSummary, 0, len(rows))
	for _, row := range rows {
		task, err := row.summary(now)
		if err != nil {
			return nil, err
		}

		if spid, ok := spids[task.ID]; ok {
			task.SpID = strconv.FormatInt(spid, 10)
			if spid > 0 {
				maddr, err := address.NewIDAddress(uint64(spid))
				if err != nil {
					return nil, err
				}
				task.Miner = maddr.String()
			}
		}

		ts = append(ts, task)
	}

	return ts, nil
}

func (a *WebRPC) ClusterTaskSummary(ctx context.Context) ([]TaskSummary, error) {
	var rows []taskSummaryRow
	err := a.Deps.DB.Select(ctx, &rows, `SELECT
		t.id as id, t.name as name, t.posted_time, t.work_start, t.owner_id as owner_id, hm.host_and_port as owner
	FROM harmony_task t LEFT JOIN harmony_machines hm ON hm.id = t.owner_id`)
	if err != nil {
		return nil, err // Handle error
	}

	return buildTaskSummaries(ctx, a.Deps.DB, rows, a.TaskSPIDs, time.Now())
}

type SpidGetter interface {
	GetSpids(ctx context.Context, db *harmonydb.DB, taskIDs []int64) ([]harmonytask.TaskSPID, error)
}

func makeTaskSPIDs() map[string]SpidGetter {
	spidGetters := lo.Filter(lo.Values(harmonytask.Registry), func(t harmonytask.TaskInterface, _ int) bool {
		_, ok := t.(SpidGetter)
		return ok
	})
	spids := make(map[string]SpidGetter)
	for _, t := range spidGetters {
		ttd := t.TypeDetails()
		spids[ttd.Name] = t.(SpidGetter)
	}
	return spids
}

type TaskStatus struct {
	TaskID   int64   `json:"task_id"`
	Status   string  `json:"status"` // "pending", "running", "done", "failed"
	OwnerID  *int64  `json:"owner_id,omitempty"`
	Name     string  `json:"name"`
	PostedAt *string `json:"posted_at,omitempty"`
}

func (a *WebRPC) GetTaskStatus(ctx context.Context, taskID int64) (*TaskStatus, error) {
	status := &TaskStatus{TaskID: taskID}

	// Check if task is present in harmony_task
	var ownerID NullInt64
	var name string
	var postedTime time.Time
	err := a.Deps.DB.QueryRow(ctx, `
        SELECT owner_id, name, posted_time FROM harmony_task WHERE id = $1
    `, taskID).Scan(&ownerID, &name, &postedTime)

	if err == nil {
		status.Name = name
		status.PostedAt = new(postedTime.Format(time.RFC3339))
		if ownerID.Valid {
			status.Status = "running"
			status.OwnerID = &ownerID.Int64
		} else {
			status.Status = "pending"
		}
		return status, nil
	}

	if err != pgx.ErrNoRows {
		return nil, fmt.Errorf("failed to query harmony_task: %w", err)
	}

	// Not found in harmony_task, check harmony_task_history
	var result bool
	err = a.Deps.DB.QueryRow(ctx, `
        SELECT result, name, posted FROM harmony_task_history WHERE task_id = $1 ORDER BY id DESC LIMIT 1
    `, taskID).Scan(&result, &name, &postedTime)

	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("task not found")
	} else if err != nil {
		return nil, fmt.Errorf("failed to query harmony_task_history: %w", err)
	}

	status.Name = name
	status.PostedAt = new(postedTime.Format(time.RFC3339))
	if result {
		status.Status = "done"
	} else {
		status.Status = "failed"
	}

	return status, nil
}

func (a *WebRPC) RestartFailedTask(ctx context.Context, taskID int64) error {
	// Check if task is present in harmony_task
	var exists bool
	err := a.Deps.DB.QueryRow(ctx, `
        SELECT 1 FROM harmony_task WHERE id = $1
    `, taskID).Scan(&exists)

	if err != pgx.ErrNoRows {
		if err != nil {
			return fmt.Errorf("failed to check harmony_task: %w", err)
		}
		return fmt.Errorf("task is already pending or running")
	}

	// Check the most recent entry in harmony_task_history
	var name string
	var postedTime time.Time
	var result bool
	err = a.Deps.DB.QueryRow(ctx, `
        SELECT name, posted, result FROM harmony_task_history WHERE task_id = $1 ORDER BY id DESC LIMIT 1
    `, taskID).Scan(&name, &postedTime, &result)

	if err == pgx.ErrNoRows {
		return fmt.Errorf("task not found in history")
	} else if err != nil {
		return fmt.Errorf("failed to query harmony_task_history: %w", err)
	}

	if result {
		return fmt.Errorf("task was successful, cannot restart")
	}

	if a.Deps.TaskEngine == nil {
		return fmt.Errorf("task engine not available")
	}

	return a.Deps.TaskEngine.RestartTaskByID(harmonytask.TaskID(taskID), name, postedTime)
}
