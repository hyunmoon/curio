package mk20release

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yugabyte/pgx/v5"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

type releaseRetryITestDB struct {
	primary   *harmonydb.DB
	secondary *harmonydb.DB
}

func newReleaseRetryITestDB(t *testing.T) releaseRetryITestDB {
	t.Helper()
	if os.Getenv("CURIO_MK20_RELEASE_ITEST") != "1" {
		t.Skip("set CURIO_MK20_RELEASE_ITEST=1 to run local Yugabyte MK20 release integration tests")
	}

	host := strings.TrimSpace(os.Getenv("CURIO_HARMONYDB_HOSTS"))
	if !isReleaseRetryITestLoopback(host) {
		t.Fatalf("refusing non-loopback CURIO_HARMONYDB_HOSTS=%q", host)
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
	if _, err := primary.Exec(ctx, releaseRetryITestSchema); err != nil {
		t.Fatalf("bootstrapping focused release-retry schema: %v", err)
	}
	applyReleaseRetryGateMigration(t, ctx, releaseRetryITestTarget{
		host:     host,
		port:     opts.Port,
		database: opts.Database,
		username: opts.Username,
		password: opts.Password,
		schema:   "itest_" + string(opts.ITestID),
	})

	var version, defaultIsolation string
	if err := primary.QueryRow(ctx, `SELECT version()`).Scan(&version); err != nil {
		t.Fatalf("reading database version: %v", err)
	}
	if err := primary.QueryRow(ctx, `SHOW default_transaction_isolation`).Scan(&defaultIsolation); err != nil {
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
	t.Logf("session default_transaction_isolation=%s effective Yugabyte isolation=UNVERIFIED", defaultIsolation)
	t.Logf("primary_backend=%d secondary_backend=%d", primaryPID, secondaryPID)

	return releaseRetryITestDB{primary: primary, secondary: secondary}
}

func isReleaseRetryITestLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func readReleaseGateMigration(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locating MK20 release retry integration test source")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "harmony", "harmonydb", "sql", "20260906-mk20-release-gate.sql")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading MK20 release gate migration %s: %v", path, err)
	}
	return string(contents)
}

type releaseRetryITestTarget struct {
	host     string
	port     string
	database string
	username string
	password string
	schema   string
}

func applyReleaseRetryGateMigration(t *testing.T, ctx context.Context, target releaseRetryITestTarget) {
	t.Helper()
	// HarmonyDB intentionally accepts only SQL literals. Use a separate pgx
	// connection to execute the bytes read from the real migration file in the
	// same loopback-only, random integration-test schema.
	baseConfig, err := pgx.ParseConfig("postgresql://yugabyte:yugabyte@localhost:5433/yugabyte?sslmode=disable")
	if err != nil {
		t.Fatalf("building isolated migration connection config: %v", err)
	}
	baseConfig.Host = strings.Trim(target.host, "[]")
	port, err := strconv.ParseUint(target.port, 10, 16)
	if err != nil {
		t.Fatalf("parsing isolated migration port %q: %v", target.port, err)
	}
	baseConfig.Port = uint16(port)
	baseConfig.Database = target.database
	baseConfig.User = target.username
	baseConfig.Password = target.password
	baseConfig.RuntimeParams["search_path"] = target.schema
	conn, err := pgx.ConnectConfig(ctx, baseConfig)
	if err != nil {
		t.Fatalf("opening isolated migration connection: %v", err)
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("closing isolated migration connection: %v", err)
		}
	}()
	if _, err := conn.Exec(ctx, readReleaseGateMigration(t)); err != nil {
		t.Fatalf("applying MK20 release gate migration: %v", err)
	}
}

type releaseRetryITestResult struct {
	outcome Outcome
	err     error
}

// TestMK20ReleaseDBCoreRetriesWithFreshAuthoritativeState uses the same
// release orchestration and dbTransaction adapter as Release. The custom
// transaction runner adds only deterministic conflict coordination around the
// production callback, allowing its first provisional release to be rolled
// back by a real database serialization error before HarmonyDB retries it.
func TestMK20ReleaseDBCoreRetriesWithFreshAuthoritativeState(t *testing.T) {
	for _, tc := range []struct {
		name         string
		competitorID string
		maxActive    int64
		wantOutcome  Outcome
	}{
		{name: "competitor consumes last slot", competitorID: "winner", maxActive: 1, wantOutcome: AtCapacity},
		{name: "competitor releases same waiting deal", competitorID: "target", maxActive: 10, wantOutcome: NoLongerWaiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbs := newReleaseRetryITestDB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			seedReleaseRetryWaiting(t, ctx, dbs.primary, "target")
			if tc.competitorID != "target" {
				seedReleaseRetryWaiting(t, ctx, dbs.primary, tc.competitorID)
			}

			conflictStart := make(chan struct{})
			conflictDone := make(chan error, 1)
			go func() {
				select {
				case <-conflictStart:
					_, err := dbs.secondary.Exec(ctx, `UPDATE release_retry_conflict
						SET token = NOT token
						WHERE singleton = TRUE`)
					conflictDone <- err
				case <-ctx.Done():
					conflictDone <- ctx.Err()
				}
			}()

			competitorStart := make(chan struct{})
			competitorDone := make(chan releaseRetryITestResult, 1)
			var competitorInserts atomic.Int32
			go func() {
				select {
				case <-competitorStart:
					outcome, err := Release(ctx, dbs.secondary, tc.competitorID, tc.maxActive, func(tx *harmonydb.Tx) (Plan, error) {
						return releaseRetryITestPlan(tx, tc.competitorID, &competitorInserts), nil
					})
					competitorDone <- releaseRetryITestResult{outcome: outcome, err: err}
				case <-ctx.Done():
					competitorDone <- releaseRetryITestResult{err: ctx.Err()}
				}
			}()

			var attempts, provisionalReleased, serializationFailures, prepareCalls, insertCalls atomic.Int32
			outcome, err := release("target", tc.maxActive, func(callback func(transaction) (bool, error)) (bool, error) {
				return dbs.primary.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
					attempt := attempts.Add(1)
					switch attempt {
					case 1:
						if _, err := tx.Exec(`SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
							return false, err
						}
						var displayedIsolation string
						if err := tx.QueryRow(`SHOW transaction_isolation`).Scan(&displayedIsolation); err != nil {
							return false, err
						}
						t.Logf("retry transaction requested isolation=REPEATABLE READ displayed transaction_isolation=%s effective Yugabyte isolation=UNVERIFIED", displayedIsolation)

						var token bool
						if err := tx.QueryRow(`SELECT token FROM release_retry_conflict WHERE singleton = TRUE`).Scan(&token); err != nil {
							return false, err
						}
						close(conflictStart)
						if err := <-conflictDone; err != nil {
							return false, fmt.Errorf("committing competing conflict-row write: %w", err)
						}
					case 2:
						close(competitorStart)
						competing := <-competitorDone
						if competing.err != nil {
							return false, fmt.Errorf("competing release outcome=%q: %w", competing.outcome, competing.err)
						}
						if competing.outcome != Released {
							return false, fmt.Errorf("competing release outcome=%q, want %q", competing.outcome, Released)
						}
						if _, err := tx.Exec(`SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
							return false, err
						}
					default:
						return false, fmt.Errorf("unexpected transaction attempt %d", attempt)
					}

					commit, err := callback(dbTransaction{tx: tx})
					if err != nil || attempt != 1 {
						return commit, err
					}
					if !commit {
						return false, fmt.Errorf("first production-core attempt did not reach provisional Released")
					}
					provisionalReleased.Add(1)
					if _, err := tx.Exec(`UPDATE release_retry_conflict
						SET token = NOT token
						WHERE singleton = TRUE`); err != nil {
						if harmonydb.IsErrSerialization(err) {
							serializationFailures.Add(1)
						}
						return false, err
					}
					return false, fmt.Errorf("stale transaction updated the conflict row without a serialization failure")
				}, harmonydb.OptionRetry())
			}, func(tx transaction) (Plan, error) {
				prepareCalls.Add(1)
				return releaseRetryITestPlan(tx.(dbTransaction).tx, "target", &insertCalls), nil
			})
			if err != nil || outcome != tc.wantOutcome {
				t.Fatalf("final outcome=%q err=%v, want %q", outcome, err, tc.wantOutcome)
			}
			if attempts.Load() != 2 || provisionalReleased.Load() != 1 || serializationFailures.Load() != 1 {
				t.Fatalf("attempts=%d provisional releases=%d serialization failures=%d", attempts.Load(), provisionalReleased.Load(), serializationFailures.Load())
			}
			if prepareCalls.Load() != 1 || insertCalls.Load() != 1 || competitorInserts.Load() != 1 {
				t.Fatalf("prepare calls=%d target inserts=%d competitor inserts=%d", prepareCalls.Load(), insertCalls.Load(), competitorInserts.Load())
			}

			assertReleaseRetryFinalState(t, ctx, dbs.primary, tc.wantOutcome)
		})
	}
}

func releaseRetryITestPlan(tx *harmonydb.Tx, id string, insertCalls *atomic.Int32) Plan {
	return Plan{Rows: 1, Insert: func() error {
		insertCalls.Add(1)
		if _, err := tx.Exec(`INSERT INTO market_mk20_pipeline (id, aggr_index, complete)
			VALUES ($1, 0, FALSE)`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO release_retry_refs (deal_id) VALUES ($1)`, id)
		return err
	}}
}

func seedReleaseRetryWaiting(t *testing.T, ctx context.Context, db *harmonydb.DB, id string) {
	t.Helper()
	if _, err := db.Exec(ctx, `INSERT INTO market_mk20_pipeline_waiting (id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
}

func assertReleaseRetryFinalState(t *testing.T, ctx context.Context, db *harmonydb.DB, outcome Outcome) {
	t.Helper()
	var targetWaiting, targetPipeline, targetRefs, totalPipeline int
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM market_mk20_pipeline_waiting WHERE id = 'target'`).Scan(&targetWaiting); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM market_mk20_pipeline WHERE id = 'target'`).Scan(&targetPipeline); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM release_retry_refs WHERE deal_id = 'target'`).Scan(&targetRefs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM market_mk20_pipeline`).Scan(&totalPipeline); err != nil {
		t.Fatal(err)
	}

	switch outcome {
	case AtCapacity:
		if targetWaiting != 1 || targetPipeline != 0 || targetRefs != 0 || totalPipeline != 1 {
			t.Fatalf("at-capacity state waiting=%d target pipeline=%d target refs=%d total pipeline=%d", targetWaiting, targetPipeline, targetRefs, totalPipeline)
		}
	case NoLongerWaiting:
		if targetWaiting != 0 || targetPipeline != 1 || targetRefs != 1 || totalPipeline != 1 {
			t.Fatalf("no-longer-waiting state waiting=%d target pipeline=%d target refs=%d total pipeline=%d", targetWaiting, targetPipeline, targetRefs, totalPipeline)
		}
	default:
		t.Fatalf("unsupported final outcome %q", outcome)
	}
}

const releaseRetryITestSchema = `
CREATE TABLE market_mk20_pipeline_waiting (
    id TEXT PRIMARY KEY
);

CREATE TABLE market_mk20_pipeline (
    id TEXT NOT NULL,
    aggr_index BIGINT NOT NULL DEFAULT 0,
    complete BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (id, aggr_index)
);

CREATE TABLE release_retry_refs (
    deal_id TEXT PRIMARY KEY
);

CREATE TABLE release_retry_conflict (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton = TRUE),
    token BOOLEAN NOT NULL DEFAULT FALSE
);

INSERT INTO release_retry_conflict (singleton, token) VALUES (TRUE, FALSE);
`
