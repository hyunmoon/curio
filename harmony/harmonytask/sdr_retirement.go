package harmonytask

import (
	"context"
	"fmt"
	"time"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

// Only SDR opts in. A timestamp, missing task reference, missing Go handle or
// heartbeat is NOT an implementation of this process-subtree boundary.
type taskExecutionBoundary interface {
	TaskExecutionIdentity() (string, error)
	TaskExecutionStopped(string) (bool, error)
}

type sdrRetirement struct {
	ID         TaskID
	Owner      int
	Generation int64
	Token      string
	Session    string
	Execution  string
}

// Deliberately independent of admission, cordon, capacity and start pacing.
// One goroutine, bounded pages and contexts; a refusal never releases ownership.
func (h *taskTypeHandler) retireStoppedSDRLoop(boundary taskExecutionBoundary) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var after TaskID
	for {
		ctx, cancel := context.WithTimeout(h.TaskEngine.cfg.ctx, 5*time.Second)
		var rows []sdrRetirement
		err := h.TaskEngine.cfg.db.Select(ctx, &rows, `SELECT id, owner_id AS owner, owner_generation AS generation,
attempt_id AS token, attempt_session AS session, sdr_execution AS execution FROM harmony_task
WHERE name='SDR' AND owner_id=$1 AND id>$2 AND attempt_session IS NOT NULL
AND attempt_session<>$3 AND attempt_id IS NOT NULL AND sdr_execution IS NOT NULL
ORDER BY id LIMIT 32`, h.TaskEngine.cfg.ownerID, after, h.TaskEngine.cfg.session)
		if err == nil {
			for _, r := range rows {
				after = r.ID
				if _, err = h.retireStoppedSDR(ctx, r, boundary); err != nil {
					break
				}
			}
			if len(rows) < 32 {
				after = 0
			}
		}
		cancel()
		if err != nil && h.TaskEngine.cfg.ctx.Err() == nil {
			log.Warnw("SDR retirement deferred", "error", err)
		}
		select {
		case <-h.TaskEngine.cfg.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *taskTypeHandler) retireStoppedSDR(ctx context.Context, r sdrRetirement, boundary taskExecutionBoundary) (bool, error) {
	if h.Name != "SDR" || r.Owner != h.TaskEngine.cfg.ownerID || r.Session == "" || r.Session == h.TaskEngine.cfg.session || r.Token == "" || r.Execution == "" {
		return false, nil
	}
	if _, active := h.running.Get(int64(r.ID)); active {
		return false, nil
	}
	stopped, err := boundary.TaskExecutionStopped(r.Execution)
	if err != nil || !stopped {
		return false, err
	}
	retired := false
	committed, err := h.TaskEngine.cfg.db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		retired = false // retry-local result
		var rows []struct{ Original string }
		err := tx.Select(&rows, `SELECT to_jsonb(t)::text AS original FROM harmony_task t
WHERE id=$1 AND name='SDR' AND owner_id=$2 AND owner_generation=$3 AND attempt_id=$4
AND attempt_session=$5 AND sdr_execution=$6 FOR UPDATE`, r.ID, r.Owner, r.Generation, r.Token, r.Session, r.Execution)
		if err != nil || len(rows) == 0 {
			return false, err
		}
		if _, active := h.running.Get(int64(r.ID)); active {
			return false, nil
		}
		// Reconnection trigger takes this same task-row lock, and the tombstone
		// rejects future INSERT/UPDATE links after deletion. NOT EXISTS alone
		// would not establish either exclusion guarantee.
		var references int
		if err = tx.QueryRow(`SELECT count(*) FROM sectors_sdr_pipeline WHERE task_id_sdr=$1`, r.ID).Scan(&references); err != nil {
			return false, err
		}
		if references != 0 {
			return false, nil
		} // Includes failed/completed/ambiguous links.
		if _, err = tx.Exec(`INSERT INTO harmony_sdr_task_retirements(task_id,reason,original_task)
VALUES($1,'unreferenced SDR; recorded managed execution subtree terminated',$2::jsonb)`, r.ID, rows[0].Original); err != nil {
			return false, err
		}
		n, err := tx.Exec(`DELETE FROM harmony_task WHERE id=$1 AND name='SDR' AND owner_id=$2
AND owner_generation=$3 AND attempt_id=$4 AND attempt_session=$5 AND sdr_execution=$6`, r.ID, r.Owner, r.Generation, r.Token, r.Session, r.Execution)
		if err != nil {
			return false, err
		}
		if n != 1 {
			return false, fmt.Errorf("SDR retirement CAS affected %d rows", n)
		}
		retired = true
		return true, nil
	})
	if err == nil && committed && retired {
		log.Infow("Retired stopped unreferenced SDR task", "id", r.ID, "owner", r.Owner, "generation", r.Generation, "attempt", r.Token)
	}
	// No normal completion/history/callback/peer-success notification.
	return err == nil && committed && retired, err
}
