ALTER TABLE harmony_task
    ADD COLUMN IF NOT EXISTS work_start TIMESTAMPTZ;

COMMENT ON COLUMN harmony_task.work_start IS 'When the current owner claimed or recovered this task; null while unowned.';

CREATE OR REPLACE FUNCTION harmony_task_sync_work_start()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.owner_id IS NULL THEN
        NEW.work_start := NULL;
    ELSIF TG_OP = 'INSERT' THEN
        NEW.work_start := CURRENT_TIMESTAMP;
    ELSIF NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
        NEW.work_start := CURRENT_TIMESTAMP;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS harmony_task_sync_work_start_trigger ON harmony_task;

CREATE TRIGGER harmony_task_sync_work_start_trigger
    BEFORE INSERT OR UPDATE OF owner_id ON harmony_task
    FOR EACH ROW
    EXECUTE FUNCTION harmony_task_sync_work_start();

-- Existing owned tasks predate work_start. Start their observable runtime at
-- migration time rather than presenting queue age as execution duration.
UPDATE harmony_task
SET work_start = CURRENT_TIMESTAMP
WHERE owner_id IS NOT NULL
  AND work_start IS NULL;
