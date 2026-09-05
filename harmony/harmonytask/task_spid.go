package harmonytask

// TaskSPID associates a Harmony task with a storage provider ID.
type TaskSPID struct {
	TaskID int64 `db:"task_id"`
	SPID   int64 `db:"sp_id"`
}
