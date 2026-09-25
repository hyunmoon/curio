-- Do not backfill queue-age or process provenance for existing tasks.
ALTER TABLE harmony_task ADD COLUMN created_at TIMESTAMPTZ;
ALTER TABLE harmony_task ADD COLUMN queued_at TIMESTAMPTZ;
ALTER TABLE harmony_task ADD COLUMN attempt_session TEXT;
ALTER TABLE harmony_task ADD COLUMN sdr_execution TEXT;
ALTER TABLE harmony_machines ADD COLUMN process_session TEXT;

-- Registration refreshes resources; the periodic heartbeat does not. Clear
-- provenance even when a pre-protocol binary registers this machine again.
CREATE FUNCTION harmony_machine_clear_process() RETURNS TRIGGER AS $$
BEGIN NEW.process_session := NULL; RETURN NEW; END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER harmony_machine_clear_process BEFORE UPDATE OF cpu,ram,gpu ON harmony_machines
FOR EACH ROW EXECUTE FUNCTION harmony_machine_clear_process();

CREATE FUNCTION harmony_task_lifetime() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        NEW.created_at := statement_timestamp();
        NEW.queued_at := CASE WHEN NEW.owner_id IS NULL THEN statement_timestamp() END;
    ELSE
        NEW.created_at := OLD.created_at;
        NEW.queued_at := OLD.queued_at;
        IF NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
            NEW.queued_at := CASE WHEN NEW.owner_id IS NULL THEN statement_timestamp() END;
        END IF;
        IF NEW.owner_id IS DISTINCT FROM OLD.owner_id
           OR NEW.owner_generation IS DISTINCT FROM OLD.owner_generation THEN
            NEW.attempt_session := NULL;
            NEW.sdr_execution := NULL;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER harmony_task_lifetime BEFORE INSERT OR UPDATE ON harmony_task
FOR EACH ROW EXECUTE FUNCTION harmony_task_lifetime();

-- Diagnostic retirement, deliberately separate from successful task history.
-- This tombstone also prevents a later producer from reconnecting the old ID.
CREATE TABLE harmony_sdr_task_retirements (
    task_id BIGINT PRIMARY KEY,
    retired_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    reason TEXT NOT NULL,
    original_task JSONB NOT NULL
);

CREATE FUNCTION harmony_sdr_reference_guard() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.task_id_sdr IS NULL THEN RETURN NEW; END IF;
    IF TG_OP = 'UPDATE' THEN
        IF NEW.task_id_sdr IS NOT DISTINCT FROM OLD.task_id_sdr THEN RETURN NEW; END IF;
    END IF;
    -- Serialize reconnects with retirement and the real scheduler claim UPDATE.
    -- Normal AddTask creates this row and connects the pipeline in one tx.
    PERFORM id FROM harmony_task WHERE id=NEW.task_id_sdr FOR UPDATE;
    IF EXISTS (SELECT 1 FROM harmony_sdr_task_retirements WHERE task_id=NEW.task_id_sdr) THEN
        RAISE EXCEPTION 'cannot connect retired SDR task %', NEW.task_id_sdr;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER harmony_sdr_reference_guard BEFORE INSERT OR UPDATE OF task_id_sdr
ON sectors_sdr_pipeline FOR EACH ROW EXECUTE FUNCTION harmony_sdr_reference_guard();
