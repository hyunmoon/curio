DROP TRIGGER IF EXISTS harmony_task_sync_work_start_trigger ON harmony_task;
DROP FUNCTION IF EXISTS harmony_task_sync_work_start();

ALTER TABLE harmony_task
    DROP COLUMN IF EXISTS work_start;
