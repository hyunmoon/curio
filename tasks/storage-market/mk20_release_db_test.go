package storage_market

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/oklog/ulid"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/deps/config"
	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/market/mk20"
	"github.com/filecoin-project/curio/market/mk20release"
)

const mk20ReleaseITestPieceCID = "bafkzcibfxx3meais3xzh6qn56y6hiasmrufhegoweu3o5ccofs74nfdfr4yn76pqz4pq"

type mk20ReleaseITestDB struct {
	primary   *harmonydb.DB
	secondary *harmonydb.DB
}

// newMK20ReleaseITestDB opens two independent pools on one random, isolated
// schema. The explicit opt-in and loopback check happen before HarmonyDB opens
// a connection so an inherited production database setting cannot be used by
// these tests. ReadOnly suppresses historical migrations; it does not prevent
// normal Exec or transaction calls after the connection is established.
func newMK20ReleaseITestDB(t *testing.T) mk20ReleaseITestDB {
	t.Helper()
	if os.Getenv("CURIO_MK20_RELEASE_ITEST") != "1" {
		t.Skip("set CURIO_MK20_RELEASE_ITEST=1 to run local Yugabyte MK20 release integration tests")
	}

	host := strings.TrimSpace(os.Getenv("CURIO_HARMONYDB_HOSTS"))
	if err := validateMK20ReleaseITestHost(host); err != nil {
		t.Fatal(err)
	}

	opts := harmonydb.DefaultItestOptions()
	opts.Hosts = []string{host}
	harmonydb.YugabyteDB(true)(&opts)
	cfg := opts.HarmonyConfig()
	cfg.ReadOnly = true

	primary, err := harmonydb.NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("opening primary isolated Yugabyte test handle: %v", err)
	}
	var secondary *harmonydb.DB
	t.Cleanup(func() {
		if secondary != nil {
			secondary.ITestDeleteAll()
		}
		primary.ITestDeleteAll()
	})

	secondary, err = harmonydb.NewFromConfig(cfg)
	if err != nil {
		t.Fatalf("opening secondary isolated Yugabyte test handle: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := primary.Exec(ctx, mk20ReleaseITestSchema); err != nil {
		t.Fatalf("bootstrapping focused MK20 release schema: %v", err)
	}

	var version, isolation string
	if err := primary.QueryRow(ctx, `SELECT version()`).Scan(&version); err != nil {
		t.Fatalf("reading database version: %v", err)
	}
	if err := primary.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		t.Fatalf("reading default transaction isolation: %v", err)
	}
	var primaryPID, secondaryPID int64
	if err := primary.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&primaryPID); err != nil {
		t.Fatalf("reading primary backend PID: %v", err)
	}
	if err := secondary.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&secondaryPID); err != nil {
		t.Fatalf("reading secondary backend PID: %v", err)
	}
	if primaryPID == secondaryPID {
		t.Fatalf("integration handles unexpectedly share backend PID %d", primaryPID)
	}
	t.Logf("database=%s", version)
	t.Logf("default transaction_isolation=%s primary_backend=%d secondary_backend=%d", isolation, primaryPID, secondaryPID)

	return mk20ReleaseITestDB{primary: primary, secondary: secondary}
}

func isLoopbackDBHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func validateMK20ReleaseITestHost(host string) error {
	if host == "" {
		return fmt.Errorf("CURIO_HARMONYDB_HOSTS must explicitly name a loopback host for MK20 release integration tests")
	}
	if strings.Contains(host, ",") || !isLoopbackDBHost(host) {
		return fmt.Errorf("refusing non-loopback CURIO_HARMONYDB_HOSTS=%q", host)
	}
	return nil
}

func TestMK20ReleaseITestHostValidation(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "::1", "[::1]", "localhost"} {
		if err := validateMK20ReleaseITestHost(host); err != nil {
			t.Fatalf("loopback host %q rejected: %v", host, err)
		}
	}
	for _, host := range []string{"", "10.10.35.91", "db.example.com", "127.0.0.1,10.10.35.91"} {
		if err := validateMK20ReleaseITestHost(host); err == nil {
			t.Fatalf("unsafe host %q accepted", host)
		}
	}
}

func TestMK20ReleaseDBLastSlotIsGlobalAcrossProviders(t *testing.T) {
	dbs := newMK20ReleaseITestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	seedActiveMK20PipelineRow(t, ctx, dbs.primary, "existing-active", 9000, false)
	first := seedOfflineMK20WaitingDeal(t, ctx, dbs.primary, 1000)
	second := seedOfflineMK20WaitingDeal(t, ctx, dbs.primary, 2000)

	type result struct {
		outcome mk20release.Outcome
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, attempt := range []struct {
		db *harmonydb.DB
		id string
	}{{dbs.primary, first}, {dbs.secondary, second}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outcome, err := releaseMK20WaitingDeal(ctx, attempt.db, attempt.id, 2)
			results <- result{outcome: outcome, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	released, atCapacity := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		switch result.outcome {
		case mk20release.Released:
			released++
		case mk20release.AtCapacity:
			atCapacity++
		default:
			t.Fatalf("unexpected outcome %q", result.outcome)
		}
	}
	if released != 1 || atCapacity != 1 {
		t.Fatalf("released=%d atCapacity=%d, want 1 each", released, atCapacity)
	}
	assertMK20ReleaseCounts(t, ctx, dbs.primary, 2, 1)

	var providerRows int
	if err := dbs.primary.QueryRow(ctx, `SELECT COUNT(DISTINCT sp_id)
		FROM market_mk20_pipeline
		WHERE id IN ($1, $2)`, first, second).Scan(&providerRows); err != nil {
		t.Fatal(err)
	}
	if providerRows != 1 {
		t.Fatalf("released provider rows=%d, want exactly one provider to consume the global last slot", providerRows)
	}
}

func TestMK20ReleaseDBSameWaitingDealReleasedOnce(t *testing.T) {
	dbs := newMK20ReleaseITestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := seedOfflineMK20WaitingDeal(t, ctx, dbs.primary, 1000)

	start := make(chan struct{})
	outcomes := make(chan mk20release.Outcome, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, db := range []*harmonydb.DB{dbs.primary, dbs.secondary} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outcome, err := releaseMK20WaitingDeal(ctx, db, id, 10)
			if err != nil {
				errs <- err
				return
			}
			outcomes <- outcome
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(outcomes)
	for err := range errs {
		t.Fatal(err)
	}

	counts := map[mk20release.Outcome]int{}
	for outcome := range outcomes {
		counts[outcome]++
	}
	if counts[mk20release.Released] != 1 || counts[mk20release.NoLongerWaiting] != 1 {
		t.Fatalf("outcomes=%v, want one released and one no-longer-waiting", counts)
	}
	assertMK20ReleaseCounts(t, ctx, dbs.primary, 1, 0)
	var rows int
	if err := dbs.primary.QueryRow(ctx, `SELECT COUNT(*) FROM market_mk20_pipeline WHERE id = $1`, id).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("pipeline rows for deal=%d, want 1", rows)
	}
	var duration int64
	var startEpoch sql.NullInt64
	if err := dbs.primary.QueryRow(ctx, `SELECT p.duration,
		(d.ddo_v1->'ddo'->>'start_epoch')::BIGINT
		FROM market_mk20_pipeline p
		JOIN market_mk20_deal d ON d.id = p.id
		WHERE p.id = $1`, id).Scan(&duration, &startEpoch); err != nil {
		t.Fatal(err)
	}
	if duration != 5_256_000 || startEpoch.Valid {
		t.Fatalf("release changed DDO schedule: duration=%d start_epoch=%v", duration, startEpoch)
	}
}

func TestMK20ReleaseDBActiveCountIncludesEveryIncompleteRow(t *testing.T) {
	dbs := newMK20ReleaseITestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	seedActiveMK20PipelineRow(t, ctx, dbs.primary, "unassigned", 1000, false)
	seedActiveMK20PipelineRow(t, ctx, dbs.primary, "running-task", 1000, false)
	if _, err := dbs.primary.Exec(ctx, `UPDATE market_mk20_pipeline
		SET commp_task_id = 99
		WHERE id = 'running-task'`); err != nil {
		t.Fatal(err)
	}
	seedActiveMK20PipelineRow(t, ctx, dbs.primary, "failed-sector", 2000, false)
	if _, err := dbs.primary.Exec(ctx, `UPDATE market_mk20_pipeline
		SET sector = 7
		WHERE id = 'failed-sector'`); err != nil {
		t.Fatal(err)
	}
	if _, err := dbs.primary.Exec(ctx, `INSERT INTO sectors_sdr_pipeline
		(sp_id, sector_number, failed) VALUES (2000, 7, TRUE)`); err != nil {
		t.Fatal(err)
	}
	seedActiveMK20PipelineRow(t, ctx, dbs.primary, "complete", 3000, true)

	active, err := countActiveMK20PipelineRows(ctx, dbs.primary)
	if err != nil {
		t.Fatal(err)
	}
	if active != 3 {
		t.Fatalf("active rows=%d, want all three incomplete rows", active)
	}
}

func TestMK20ReleaseDBCompleteRowReturnsCapacity(t *testing.T) {
	dbs := newMK20ReleaseITestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	seedActiveMK20PipelineRow(t, ctx, dbs.primary, "capacity-holder", 1000, false)
	id := seedOfflineMK20WaitingDeal(t, ctx, dbs.primary, 2000)

	outcome, err := releaseMK20WaitingDeal(ctx, dbs.primary, id, 1)
	if err != nil || outcome != mk20release.AtCapacity {
		t.Fatalf("initial outcome=%q err=%v, want at-capacity", outcome, err)
	}
	if _, err := dbs.primary.Exec(ctx, `UPDATE market_mk20_pipeline
		SET complete = TRUE
		WHERE id = 'capacity-holder'`); err != nil {
		t.Fatal(err)
	}
	outcome, err = releaseMK20WaitingDeal(ctx, dbs.secondary, id, 1)
	if err != nil || outcome != mk20release.Released {
		t.Fatalf("post-completion outcome=%q err=%v, want released", outcome, err)
	}
	assertMK20ReleaseCounts(t, ctx, dbs.primary, 1, 0)
}

func TestMK20ReleaseDBMissingGateFailsClosed(t *testing.T) {
	dbs := newMK20ReleaseITestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := seedOfflineMK20WaitingDeal(t, ctx, dbs.primary, 1000)
	if _, err := dbs.primary.Exec(ctx, `DELETE FROM market_mk20_release_gate WHERE singleton = TRUE`); err != nil {
		t.Fatal(err)
	}

	outcome, err := releaseMK20WaitingDeal(ctx, dbs.secondary, id, 0)
	if err == nil || outcome != "" || !errors.Is(err, mk20release.ErrGateUnavailable) {
		t.Fatalf("missing gate outcome=%q err=%v", outcome, err)
	}
	assertMK20ReleaseCounts(t, ctx, dbs.primary, 0, 1)
}

func TestMK20ReleaseDBPipelineInsertCallSiteUsesGate(t *testing.T) {
	dbs := newMK20ReleaseITestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := seedOfflineMK20WaitingDeal(t, ctx, dbs.primary, 1000)
	if _, err := dbs.primary.Exec(ctx, `DELETE FROM market_mk20_release_gate WHERE singleton = TRUE`); err != nil {
		t.Fatal(err)
	}

	market := &CurioStorageDealMarket{
		cfg: config.DefaultCurioConfig(),
		db:  dbs.secondary,
	}
	market.insertDDODealInPipeline(ctx)

	assertMK20ReleaseCounts(t, ctx, dbs.primary, 0, 1)
	var rows int
	if err := dbs.primary.QueryRow(ctx, `SELECT COUNT(*) FROM market_mk20_pipeline WHERE id = $1`, id).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("pipeline insert call site bypassed missing gate: rows=%d", rows)
	}
}

func TestMK20ReleaseDBPartialInsertRollsBackAllRows(t *testing.T) {
	dbs := newMK20ReleaseITestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := seedHTTPMK20WaitingDeal(t, ctx, dbs.primary, 1000)
	parsed := ulid.MustParse(id)
	injected := errors.New("injected insert failure")

	outcome, err := mk20release.Release(ctx, dbs.primary, id, 10, func(tx *harmonydb.Tx) (mk20release.Plan, error) {
		deal, err := mk20.DealFromTX(tx, parsed)
		if err != nil {
			return mk20release.Plan{}, err
		}
		rows, err := mk20PipelineRowCost(deal)
		if err != nil {
			return mk20release.Plan{}, err
		}
		return mk20release.Plan{Rows: rows, Insert: func() error {
			if err := insertPiecesInTransaction(ctx, tx, deal); err != nil {
				return err
			}
			return injected
		}}, nil
	})
	if err == nil || outcome == mk20release.Released || !errors.Is(err, injected) {
		t.Fatalf("partial insert outcome=%q err=%v", outcome, err)
	}

	assertMK20ReleaseCounts(t, ctx, dbs.primary, 0, 1)
	var pipelineRows, downloadRows, parkedRows, referenceRows int
	if err := dbs.primary.QueryRow(ctx, `SELECT COUNT(*) FROM market_mk20_pipeline WHERE id = $1`, id).Scan(&pipelineRows); err != nil {
		t.Fatal(err)
	}
	if err := dbs.primary.QueryRow(ctx, `SELECT COUNT(*) FROM market_mk20_download_pipeline WHERE id = $1`, id).Scan(&downloadRows); err != nil {
		t.Fatal(err)
	}
	if err := dbs.primary.QueryRow(ctx, `SELECT COUNT(*) FROM parked_pieces`).Scan(&parkedRows); err != nil {
		t.Fatal(err)
	}
	if err := dbs.primary.QueryRow(ctx, `SELECT COUNT(*) FROM parked_piece_refs`).Scan(&referenceRows); err != nil {
		t.Fatal(err)
	}
	if pipelineRows != 0 || downloadRows != 0 || parkedRows != 0 || referenceRows != 0 {
		t.Fatalf("pipeline=%d download=%d parked=%d refs=%d after rollback, want 0 each", pipelineRows, downloadRows, parkedRows, referenceRows)
	}
}

func TestMK20ReleaseDBStaleSnapshotRetriesCleanly(t *testing.T) {
	dbs := newMK20ReleaseITestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	id := seedOfflineMK20WaitingDeal(t, ctx, dbs.primary, 1000)

	var attempts atomic.Int32
	snapshotReady := make(chan struct{})
	releaseFinished := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		_, err := dbs.primary.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
			attempt := attempts.Add(1)
			if _, err := tx.Exec(`SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
				return false, err
			}
			var token bool
			if err := tx.QueryRow(`SELECT token FROM market_mk20_release_gate WHERE singleton = TRUE`).Scan(&token); err != nil {
				return false, err
			}
			if _, err := tx.Exec(`INSERT INTO release_retry_writes (attempt) VALUES ($1)`, attempt); err != nil {
				return false, err
			}
			if attempt == 1 {
				close(snapshotReady)
				select {
				case <-releaseFinished:
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}
			_, err := tx.Exec(`UPDATE market_mk20_release_gate
				SET token = NOT token
				WHERE singleton = TRUE`)
			return err == nil, err
		}, harmonydb.OptionRetry())
		errCh <- err
	}()

	select {
	case <-snapshotReady:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	outcome, err := releaseMK20WaitingDeal(ctx, dbs.secondary, id, 10)
	if err != nil || outcome != mk20release.Released {
		t.Fatalf("competing release outcome=%q err=%v", outcome, err)
	}
	close(releaseFinished)
	if err := <-errCh; err != nil {
		t.Fatalf("retrying stale-snapshot transaction: %v", err)
	}
	if attempts.Load() < 2 {
		t.Fatalf("transaction attempts=%d, want a serialization retry", attempts.Load())
	}

	var writes, firstAttemptWrites int
	if err := dbs.primary.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE attempt = 1)
		FROM release_retry_writes`).Scan(&writes, &firstAttemptWrites); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || firstAttemptWrites != 0 {
		t.Fatalf("durable retry writes=%d first-attempt writes=%d, want one clean final-attempt row", writes, firstAttemptWrites)
	}
}

func seedOfflineMK20WaitingDeal(t *testing.T, ctx context.Context, db *harmonydb.DB, providerID uint64) string {
	return seedMK20WaitingDeal(t, ctx, db, providerID, false)
}

func seedHTTPMK20WaitingDeal(t *testing.T, ctx context.Context, db *harmonydb.DB, providerID uint64) string {
	return seedMK20WaitingDeal(t, ctx, db, providerID, true)
}

func seedMK20WaitingDeal(t *testing.T, ctx context.Context, db *harmonydb.DB, providerID uint64, useHTTP bool) string {
	t.Helper()
	provider, err := address.NewIDAddress(providerID)
	if err != nil {
		t.Fatal(err)
	}
	piece, err := cid.Parse(mk20ReleaseITestPieceCID)
	if err != nil {
		t.Fatal(err)
	}
	data := &mk20.DataSource{
		PieceCID: piece,
		Format:   mk20.PieceDataFormat{Car: &mk20.FormatCar{}},
	}
	if useHTTP {
		data.SourceHTTP = &mk20.DataSourceHTTP{URLs: []mk20.HttpUrl{{
			URL:     "https://example.invalid/piece.car",
			Headers: http.Header{"X-Test": []string{"mk20-release"}},
		}}}
	} else {
		data.SourceOffline = &mk20.DataSourceOffline{}
	}

	deal := &mk20.Deal{
		Identifier: ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader),
		Client:     fmt.Sprintf("client-%d", providerID),
		Data:       data,
		Products: mk20.Products{
			DDOV1: &mk20.DDOV1{
				Provider:   provider,
				Duration:   abi.ChainEpoch(5_256_000),
				StartEpoch: nil,
			},
			RetrievalV1: &mk20.RetrievalV1{},
		},
	}
	committed, err := db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		if err := deal.SaveToDB(tx); err != nil {
			return false, err
		}
		n, err := tx.Exec(`INSERT INTO market_mk20_pipeline_waiting (id) VALUES ($1)`, deal.Identifier.String())
		return n == 1 && err == nil, err
	})
	if err != nil || !committed {
		t.Fatalf("seeding waiting deal: committed=%t err=%v", committed, err)
	}
	return deal.Identifier.String()
}

func seedActiveMK20PipelineRow(t *testing.T, ctx context.Context, db *harmonydb.DB, id string, providerID int64, complete bool) {
	t.Helper()
	if _, err := db.Exec(ctx, `INSERT INTO market_mk20_pipeline (
		id, sp_id, contract, client, piece_cid_v2, piece_cid, piece_size,
		raw_size, offline, indexing, announce, duration, complete
	) VALUES ($1, $2, '', 'client', 'piece-v2', 'piece-v1', 2048,
		2032, TRUE, FALSE, FALSE, 5256000, $3)`, id, providerID, complete); err != nil {
		t.Fatal(err)
	}
}

func assertMK20ReleaseCounts(t *testing.T, ctx context.Context, db *harmonydb.DB, active, waiting int) {
	t.Helper()
	var gotActive, gotWaiting int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM market_mk20_pipeline WHERE complete = FALSE`).Scan(&gotActive); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM market_mk20_pipeline_waiting`).Scan(&gotWaiting); err != nil {
		t.Fatal(err)
	}
	if gotActive != active || gotWaiting != waiting {
		t.Fatalf("active=%d waiting=%d, want active=%d waiting=%d", gotActive, gotWaiting, active, waiting)
	}
}

// mk20ReleaseITestSchema is a focused projection of the current Curio schema.
// Production columns and primary keys used by DealFromTX,
// insertPiecesInTransaction, and release accounting are retained. The two
// release_* tables are test-only probes for transaction rollback/retry.
const mk20ReleaseITestSchema = `
CREATE TABLE market_mk20_release_gate (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton = TRUE),
    token BOOLEAN NOT NULL DEFAULT FALSE
);

INSERT INTO market_mk20_release_gate (singleton, token) VALUES (TRUE, FALSE);

CREATE TABLE market_mk20_deal (
    created_at TIMESTAMPTZ NOT NULL DEFAULT TIMEZONE('UTC', NOW()),
    id TEXT PRIMARY KEY,
    client TEXT NOT NULL,
    piece_cid_v2 TEXT,
    data JSONB NOT NULL DEFAULT 'null',
    ddo_v1 JSONB NOT NULL DEFAULT 'null',
    retrieval_v1 JSONB NOT NULL DEFAULT 'null',
    pdp_v1 JSONB NOT NULL DEFAULT 'null'
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
    offline BOOLEAN NOT NULL,
    url TEXT DEFAULT NULL,
    indexing BOOLEAN NOT NULL,
    announce BOOLEAN NOT NULL,
    allocation_id BIGINT DEFAULT NULL,
    duration BIGINT NOT NULL,
    piece_aggregation INT NOT NULL DEFAULT 0,
    started BOOLEAN DEFAULT FALSE,
    downloaded BOOLEAN DEFAULT FALSE,
    commp_task_id BIGINT DEFAULT NULL,
    after_commp BOOLEAN DEFAULT FALSE,
    deal_aggregation INT NOT NULL DEFAULT 0,
    aggr_index BIGINT DEFAULT 0,
    agg_task_id BIGINT DEFAULT NULL,
    aggregated BOOLEAN DEFAULT FALSE,
    sector BIGINT DEFAULT NULL,
    reg_seal_proof INT DEFAULT NULL,
    sector_offset BIGINT DEFAULT NULL,
    sealed BOOLEAN DEFAULT FALSE,
    indexing_created_at TIMESTAMPTZ DEFAULT NULL,
    indexing_task_id BIGINT DEFAULT NULL,
    indexed BOOLEAN DEFAULT FALSE,
    complete BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (id, aggr_index)
);

CREATE TABLE market_mk20_pipeline_waiting (
    id TEXT PRIMARY KEY
);

CREATE TABLE market_mk20_download_pipeline (
    id TEXT NOT NULL,
    product TEXT NOT NULL,
    piece_cid_v2 TEXT NOT NULL,
    ref_ids BIGINT[] NOT NULL,
    PRIMARY KEY (id, product, piece_cid_v2)
);

CREATE TABLE parked_pieces (
    id BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    piece_cid TEXT NOT NULL,
    piece_padded_size BIGINT NOT NULL,
    piece_raw_size BIGINT NOT NULL,
    complete BOOLEAN NOT NULL DEFAULT FALSE,
    task_id BIGINT,
    cleanup_task_id BIGINT,
    long_term BOOLEAN NOT NULL DEFAULT FALSE,
    skip BOOLEAN NOT NULL DEFAULT FALSE,
    ref_count INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX parked_pieces_active_piece_key
    ON parked_pieces (piece_cid, piece_padded_size, long_term)
    WHERE cleanup_task_id IS NULL;

CREATE TABLE parked_piece_refs (
    ref_id BIGSERIAL PRIMARY KEY,
    piece_id BIGINT NOT NULL REFERENCES parked_pieces(id) ON DELETE CASCADE,
    data_url TEXT,
    data_headers JSONB NOT NULL DEFAULT '{}',
    long_term BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE sectors_sdr_pipeline (
    sp_id BIGINT NOT NULL,
    sector_number BIGINT NOT NULL,
    failed BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (sp_id, sector_number)
);

CREATE TABLE release_retry_writes (
    attempt INT NOT NULL
);
`
