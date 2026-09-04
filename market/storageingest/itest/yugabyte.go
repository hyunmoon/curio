package itest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

// NewYugabyteDB creates an isolated Yugabyte schema containing the current
// storage-ingest database objects exercised by the opt-in integration tests.
// ReadOnly suppresses the historical migration replay; it does not restrict
// queries or transactions after harmonyquery opens the connection.
func NewYugabyteDB(t *testing.T) *harmonydb.DB {
	t.Helper()
	if os.Getenv("CURIO_STORAGEINGEST_ITEST") != "1" {
		t.Skip("set CURIO_STORAGEINGEST_ITEST=1 to run Yugabyte storage-ingest integration tests")
	}

	opts := harmonydb.DefaultItestOptions()
	harmonydb.YugabyteDB(true)(&opts)
	cfg := opts.HarmonyConfig()
	cfg.ReadOnly = true

	db, err := harmonydb.NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("opening isolated Yugabyte test schema: %v", err)
	}
	t.Cleanup(db.ITestDeleteAll)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := db.Exec(ctx, storageIngestSchema); err != nil {
		t.Fatalf("bootstrapping storage-ingest test schema: %v", err)
	}

	return db
}

// storageIngestSchema is a focused projection of the current Curio schema.
// It retains every column read or written by the exercised paths, along with
// the production primary keys, foreign keys, open-sector index, and current
// transfer functions. Definitions are derived from the migrations through
// 20260612-snap-sector-allocation-index.sql.
const storageIngestSchema = `
CREATE TABLE sectors_allocated_numbers (
    sp_id BIGINT NOT NULL PRIMARY KEY,
    allocated JSONB NOT NULL
);

CREATE TABLE sectors_sdr_pipeline (
    sp_id BIGINT NOT NULL,
    sector_number BIGINT NOT NULL,
    create_time TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    reg_seal_proof INT NOT NULL,
    user_sector_duration_epochs BIGINT DEFAULT NULL,
    ticket_epoch BIGINT,
    ticket_value BYTEA,
    task_id_sdr BIGINT,
    after_sdr BOOL NOT NULL DEFAULT FALSE,
    tree_d_cid TEXT,
    task_id_tree_d BIGINT,
    after_tree_d BOOL NOT NULL DEFAULT FALSE,
    task_id_tree_c BIGINT,
    after_tree_c BOOL NOT NULL DEFAULT FALSE,
    tree_r_cid TEXT,
    task_id_tree_r BIGINT,
    after_tree_r BOOL NOT NULL DEFAULT FALSE,
    task_id_synth BIGINT,
    after_synth BOOL NOT NULL DEFAULT FALSE,
    precommit_ready_at TIMESTAMPTZ,
    precommit_msg_cid TEXT,
    task_id_precommit_msg BIGINT,
    after_precommit_msg BOOL NOT NULL DEFAULT FALSE,
    seed_epoch BIGINT,
    precommit_msg_tsk BYTEA,
    after_precommit_msg_success BOOL NOT NULL DEFAULT FALSE,
    seed_value BYTEA,
    task_id_porep BIGINT,
    porep_proof BYTEA,
    after_porep BOOL NOT NULL DEFAULT FALSE,
    task_id_finalize BIGINT,
    after_finalize BOOL NOT NULL DEFAULT FALSE,
    task_id_move_storage BIGINT,
    after_move_storage BOOL NOT NULL DEFAULT FALSE,
    commit_ready_at TIMESTAMPTZ,
    start_epoch BIGINT DEFAULT NULL,
    commit_msg_cid TEXT,
    task_id_commit_msg BIGINT,
    after_commit_msg BOOL NOT NULL DEFAULT FALSE,
    commit_msg_tsk BYTEA,
    after_commit_msg_success BOOL NOT NULL DEFAULT FALSE,
    failed BOOL NOT NULL DEFAULT FALSE,
    failed_at TIMESTAMPTZ,
    failed_reason VARCHAR(20) NOT NULL DEFAULT '',
    failed_reason_msg TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (sp_id, sector_number)
);

CREATE TABLE sectors_sdr_initial_pieces (
    sp_id BIGINT NOT NULL,
    sector_number BIGINT NOT NULL,
    piece_index BIGINT NOT NULL,
    piece_cid TEXT NOT NULL,
    piece_size BIGINT NOT NULL,
    data_url TEXT NOT NULL,
    data_headers JSONB NOT NULL DEFAULT '{}',
    data_raw_size BIGINT NOT NULL,
    data_delete_on_finalize BOOL NOT NULL,
    f05_publish_cid TEXT,
    f05_deal_id BIGINT,
    f05_deal_proposal JSONB,
    f05_deal_start_epoch BIGINT,
    f05_deal_end_epoch BIGINT,
    direct_start_epoch BIGINT,
    direct_end_epoch BIGINT,
    direct_piece_activation_manifest JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (sp_id, sector_number)
        REFERENCES sectors_sdr_pipeline (sp_id, sector_number) ON DELETE CASCADE,
    PRIMARY KEY (sp_id, sector_number, piece_index)
);

CREATE TABLE open_sector_pieces (
    sp_id BIGINT NOT NULL,
    sector_number BIGINT NOT NULL,
    piece_index BIGINT NOT NULL,
    piece_cid TEXT NOT NULL,
    piece_size BIGINT NOT NULL,
    data_url TEXT NOT NULL,
    data_headers JSONB NOT NULL DEFAULT '{}',
    data_raw_size BIGINT NOT NULL,
    data_delete_on_finalize BOOL NOT NULL,
    f05_publish_cid TEXT,
    f05_deal_id BIGINT,
    f05_deal_proposal JSONB,
    f05_deal_start_epoch BIGINT,
    f05_deal_end_epoch BIGINT,
    direct_start_epoch BIGINT,
    direct_end_epoch BIGINT,
    direct_piece_activation_manifest JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    is_snap BOOL NOT NULL DEFAULT FALSE,
    PRIMARY KEY (sp_id, sector_number, piece_index)
);

CREATE TABLE sectors_meta (
    sp_id BIGINT NOT NULL,
    sector_num BIGINT NOT NULL,
    reg_seal_proof INT NOT NULL,
    ticket_epoch BIGINT NOT NULL,
    ticket_value BYTEA NOT NULL,
    orig_sealed_cid TEXT NOT NULL,
    orig_unsealed_cid TEXT NOT NULL,
    cur_sealed_cid TEXT NOT NULL,
    cur_unsealed_cid TEXT NOT NULL,
    msg_cid_precommit TEXT,
    msg_cid_commit TEXT,
    msg_cid_update TEXT,
    seed_epoch BIGINT NOT NULL,
    seed_value BYTEA NOT NULL,
    expiration_epoch BIGINT,
    is_cc BOOL,
    deadline BIGINT,
    partition BIGINT,
    target_unseal_state BOOL,
    min_claim_epoch BIGINT,
    max_claim_epoch BIGINT,
    has_sector_key BOOL NOT NULL DEFAULT FALSE,
    is_live BOOL NOT NULL DEFAULT TRUE,
    is_faulty BOOL NOT NULL DEFAULT FALSE,
    PRIMARY KEY (sp_id, sector_num)
);

CREATE TABLE sectors_snap_pipeline (
    sp_id BIGINT NOT NULL,
    sector_number BIGINT NOT NULL,
    start_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    upgrade_proof INT NOT NULL,
    data_assigned BOOL NOT NULL DEFAULT TRUE,
    update_unsealed_cid TEXT,
    update_sealed_cid TEXT,
    task_id_encode BIGINT,
    after_encode BOOL NOT NULL DEFAULT FALSE,
    proof BYTEA,
    task_id_prove BIGINT,
    after_prove BOOL NOT NULL DEFAULT FALSE,
    update_ready_at TIMESTAMPTZ,
    prove_msg_cid TEXT,
    task_id_submit BIGINT,
    after_submit BOOL NOT NULL DEFAULT FALSE,
    after_prove_msg_success BOOL NOT NULL DEFAULT FALSE,
    prove_msg_tsk BYTEA,
    task_id_move_storage BIGINT,
    after_move_storage BOOL NOT NULL DEFAULT FALSE,
    failed BOOL NOT NULL DEFAULT FALSE,
    failed_at TIMESTAMPTZ,
    failed_reason VARCHAR(20) NOT NULL DEFAULT '',
    failed_reason_msg TEXT NOT NULL DEFAULT '',
    submit_after TIMESTAMPTZ,
    FOREIGN KEY (sp_id, sector_number) REFERENCES sectors_meta (sp_id, sector_num),
    PRIMARY KEY (sp_id, sector_number)
);

CREATE TABLE sectors_snap_initial_pieces (
    sp_id BIGINT NOT NULL,
    sector_number BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    piece_index BIGINT NOT NULL,
    piece_cid TEXT NOT NULL,
    piece_size BIGINT NOT NULL,
    data_url TEXT NOT NULL,
    data_headers JSONB NOT NULL DEFAULT '{}',
    data_raw_size BIGINT NOT NULL,
    data_delete_on_finalize BOOL NOT NULL,
    direct_start_epoch BIGINT,
    direct_end_epoch BIGINT,
    direct_piece_activation_manifest JSONB,
    FOREIGN KEY (sp_id, sector_number)
        REFERENCES sectors_snap_pipeline (sp_id, sector_number) ON DELETE CASCADE,
    PRIMARY KEY (sp_id, sector_number, piece_index)
);

CREATE TABLE market_mk20_pipeline (
    created_at TIMESTAMPTZ NOT NULL DEFAULT TIMEZONE('UTC', NOW()),
    id TEXT NOT NULL,
    sp_id BIGINT NOT NULL,
    contract TEXT NOT NULL,
    client TEXT NOT NULL,
    piece_cid_v2 TEXT NOT NULL,
    piece_cid TEXT NOT NULL,
    piece_size BIGINT NOT NULL,
    raw_size BIGINT NOT NULL,
    offline BOOL NOT NULL,
    url TEXT DEFAULT NULL,
    indexing BOOL NOT NULL,
    announce BOOL NOT NULL,
    allocation_id BIGINT DEFAULT NULL,
    duration BIGINT NOT NULL,
    piece_aggregation INT NOT NULL DEFAULT 0,
    started BOOL DEFAULT FALSE,
    downloaded BOOL DEFAULT FALSE,
    commp_task_id BIGINT DEFAULT NULL,
    after_commp BOOL DEFAULT FALSE,
    deal_aggregation INT NOT NULL DEFAULT 0,
    aggr_index BIGINT DEFAULT 0,
    agg_task_id BIGINT DEFAULT NULL,
    aggregated BOOL DEFAULT FALSE,
    sector BIGINT DEFAULT NULL,
    reg_seal_proof INT DEFAULT NULL,
    sector_offset BIGINT DEFAULT NULL,
    sealed BOOL DEFAULT FALSE,
    indexing_created_at TIMESTAMPTZ DEFAULT NULL,
    indexing_task_id BIGINT DEFAULT NULL,
    indexed BOOL DEFAULT FALSE,
    complete BOOL NOT NULL DEFAULT FALSE,
    PRIMARY KEY (id, aggr_index)
);

CREATE INDEX open_sector_pieces_spid_snap_piece_idx
    ON open_sector_pieces (sp_id, is_snap, piece_index DESC, sector_number);

CREATE OR REPLACE FUNCTION transfer_and_delete_sorted_open_piece(v_sp_id BIGINT, v_sector_number BIGINT)
RETURNS VOID AS $$
DECLARE
    sorted_pieces RECORD;
    new_index INT := 0;
BEGIN
    FOR sorted_pieces IN
        SELECT piece_index AS old_index, piece_size, piece_cid, data_url, data_headers,
               data_raw_size, data_delete_on_finalize, f05_publish_cid, f05_deal_id,
               f05_deal_proposal, f05_deal_start_epoch, f05_deal_end_epoch, direct_start_epoch,
               direct_end_epoch, direct_piece_activation_manifest, created_at
        FROM open_sector_pieces
        WHERE sp_id = v_sp_id AND sector_number = v_sector_number
        ORDER BY piece_size DESC
    LOOP
        INSERT INTO sectors_sdr_initial_pieces (
            sp_id,
            sector_number,
            piece_index,
            piece_cid,
            piece_size,
            data_url,
            data_headers,
            data_raw_size,
            data_delete_on_finalize,
            f05_publish_cid,
            f05_deal_id,
            f05_deal_proposal,
            f05_deal_start_epoch,
            f05_deal_end_epoch,
            direct_start_epoch,
            direct_end_epoch,
            direct_piece_activation_manifest,
            created_at
        )
        VALUES (
            v_sp_id,
            v_sector_number,
            new_index,
            sorted_pieces.piece_cid,
            sorted_pieces.piece_size,
            sorted_pieces.data_url,
            sorted_pieces.data_headers,
            sorted_pieces.data_raw_size,
            sorted_pieces.data_delete_on_finalize,
            sorted_pieces.f05_publish_cid,
            sorted_pieces.f05_deal_id,
            sorted_pieces.f05_deal_proposal,
            sorted_pieces.f05_deal_start_epoch,
            sorted_pieces.f05_deal_end_epoch,
            sorted_pieces.direct_start_epoch,
            sorted_pieces.direct_end_epoch,
            sorted_pieces.direct_piece_activation_manifest,
            sorted_pieces.created_at
        );

        new_index := new_index + 1;
    END LOOP;

    IF FOUND THEN
        DELETE FROM open_sector_pieces
        WHERE sp_id = v_sp_id AND sector_number = v_sector_number;
    ELSE
        RAISE EXCEPTION 'No data found to transfer for sp_id % and sector_number %', v_sp_id, v_sector_number;
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION transfer_and_delete_sorted_open_piece_snap(v_sp_id BIGINT, v_sector_number BIGINT)
RETURNS VOID AS $$
DECLARE
    sorted_pieces RECORD;
    new_index INT := 0;
BEGIN
    IF EXISTS (
        SELECT 1
        FROM open_sector_pieces
        WHERE sp_id = v_sp_id AND sector_number = v_sector_number AND f05_deal_id IS NOT NULL
    ) THEN
        RAISE EXCEPTION 'Cannot transfer open_sector_pieces with f05_deal_id not null for sp_id % and sector_number %', v_sp_id, v_sector_number;
    END IF;

    FOR sorted_pieces IN
        SELECT piece_index AS old_index, piece_size, piece_cid, data_url, data_headers,
               data_raw_size, data_delete_on_finalize, direct_start_epoch,
               direct_end_epoch, direct_piece_activation_manifest, created_at
        FROM open_sector_pieces
        WHERE sp_id = v_sp_id AND sector_number = v_sector_number
        ORDER BY piece_size DESC
    LOOP
        INSERT INTO sectors_snap_initial_pieces (
            sp_id,
            sector_number,
            piece_index,
            piece_cid,
            piece_size,
            data_url,
            data_headers,
            data_raw_size,
            data_delete_on_finalize,
            direct_start_epoch,
            direct_end_epoch,
            direct_piece_activation_manifest,
            created_at
        )
        VALUES (
            v_sp_id,
            v_sector_number,
            new_index,
            sorted_pieces.piece_cid,
            sorted_pieces.piece_size,
            sorted_pieces.data_url,
            sorted_pieces.data_headers,
            sorted_pieces.data_raw_size,
            sorted_pieces.data_delete_on_finalize,
            sorted_pieces.direct_start_epoch,
            sorted_pieces.direct_end_epoch,
            sorted_pieces.direct_piece_activation_manifest,
            sorted_pieces.created_at
        );

        new_index := new_index + 1;
    END LOOP;

    IF FOUND THEN
        DELETE FROM open_sector_pieces
        WHERE sp_id = v_sp_id AND sector_number = v_sector_number;
    ELSE
        RAISE EXCEPTION 'No data found to transfer for sp_id % and sector_number %', v_sp_id, v_sector_number;
    END IF;
END;
$$ LANGUAGE plpgsql;
`
