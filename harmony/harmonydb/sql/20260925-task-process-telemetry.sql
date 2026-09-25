-- Additive compatibility with workers/UI predating or following task-lifetime.
-- Never backfill execution/queue provenance or install SDR retirement policy.
ALTER TABLE harmony_machines ADD COLUMN IF NOT EXISTS process_session TEXT;
ALTER TABLE harmony_task ADD COLUMN IF NOT EXISTS attempt_session TEXT;
-- The deployed task-lifetime UI also selects these queue-age fields.
ALTER TABLE harmony_task ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ;
ALTER TABLE harmony_task ADD COLUMN IF NOT EXISTS queued_at TIMESTAMPTZ;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema=current_schema() AND (
            (table_name='harmony_machines' AND column_name='process_session'
                AND (data_type<>'text' OR is_nullable<>'YES')) OR
            (table_name='harmony_task' AND column_name='attempt_session'
                AND (data_type<>'text' OR is_nullable<>'YES')) OR
            (table_name='harmony_task' AND column_name IN ('created_at','queued_at')
                AND (data_type<>'timestamp with time zone' OR is_nullable<>'YES'))
        )
    ) THEN
        RAISE EXCEPTION 'incompatible task process telemetry column type/nullability';
    END IF;
END;
$$;

-- Retain the task-lifetime registration trigger's name and exact semantics.
-- Periodic last_contact writes do not reset process provenance.
CREATE OR REPLACE FUNCTION harmony_machine_clear_process() RETURNS TRIGGER AS $$
BEGIN NEW.process_session := NULL; RETURN NEW; END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS harmony_machine_clear_process ON harmony_machines;
CREATE TRIGGER harmony_machine_clear_process BEFORE UPDATE OF cpu,ram,gpu ON harmony_machines
FOR EACH ROW EXECUTE FUNCTION harmony_machine_clear_process();

-- Independent of existing lifetime/retirement triggers, which are retained.
-- Runs after acquisition-generation and uses identical queue-age semantics.
CREATE OR REPLACE FUNCTION harmony_task_process_telemetry() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.created_at := statement_timestamp();
        NEW.queued_at := CASE WHEN NEW.owner_id IS NULL THEN statement_timestamp() END;
        NEW.attempt_session := NULL;
    ELSE
        NEW.created_at := OLD.created_at;
        NEW.queued_at := OLD.queued_at;
        IF NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
            NEW.queued_at := CASE WHEN NEW.owner_id IS NULL THEN statement_timestamp() END;
        END IF;
        IF NEW.owner_id IS DISTINCT FROM OLD.owner_id
           OR NEW.owner_generation IS DISTINCT FROM OLD.owner_generation THEN
            NEW.attempt_session := NULL;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS harmony_task_process_telemetry ON harmony_task;
CREATE TRIGGER harmony_task_process_telemetry BEFORE INSERT OR UPDATE ON harmony_task
FOR EACH ROW EXECUTE FUNCTION harmony_task_process_telemetry();
