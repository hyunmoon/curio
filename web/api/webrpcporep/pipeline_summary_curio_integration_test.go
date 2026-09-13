//go:build integration && !skiff

package webrpcporep

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

// Opt-in alternative to the historical projected-DDL fixture. Each invocation
// owns a fresh curio schema in a dedicated disposable database. Never adopt an
// existing schema or run concurrently with another fixture in that database.
func summaryCurioRunnerFixture(t *testing.T) (context.Context, *harmonydb.DB, *pgx.Conn) {
	t.Helper()
	require.Equal(t, "1", os.Getenv("CURIO_POREP_PAGE_ITEST"))
	require.Equal(t, "1", os.Getenv("CURIO_COMPLETION_TRIAL_ITEST"))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		for _, prefix := range []string{"PG", "HARMONYQUERY_", "CURIO_HARMONYDB_", "CURIO_DB_"} {
			require.False(t, strings.HasPrefix(key, prefix), "clear inherited %s", key)
		}
	}
	const prefix = "CURIO_POREP_PAGE_ITEST_"
	require.Equal(t, "127.0.0.1", os.Getenv(prefix+"HOST"))
	require.Equal(t, "curio_test_cas_summary", os.Getenv(prefix+"DATABASE"))
	port, err := strconv.ParseUint(os.Getenv(prefix+"PORT"), 10, 16)
	require.NoError(t, err)
	require.NotZero(t, port)
	user := os.Getenv(prefix + "USER")
	require.NotEmpty(t, user)
	cfg, err := pgx.ParseConfig("postgresql://placeholder@127.0.0.1/placeholder?sslmode=disable&load_balance=false")
	require.NoError(t, err)
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = "127.0.0.1", uint16(port), "curio_test_cas_summary", user, os.Getenv(prefix+"PASSWORD")
	cfg.Fallbacks, cfg.ConnectTimeout = nil, 5*time.Second
	cfg.RuntimeParams = map[string]string{"search_path": "curio", "statement_timeout": "10000", "lock_timeout": "2000"}
	setup, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	conn, err := pgx.ConnectConfig(setup, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, conn.Close(c))
	})
	var exists bool
	require.NoError(t, conn.QueryRow(setup, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='curio')`).Scan(&exists))
	require.False(t, exists, "never adopt an existing namespace")
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, err := conn.Exec(c, "SET statement_timeout='55s'")
		require.NoError(t, err)
		_, err = conn.Exec(c, "SET client_min_messages='warning'")
		require.NoError(t, err)
		_, err = conn.Exec(c, "DROP SCHEMA IF EXISTS curio CASCADE")
		require.NoError(t, err)
	})
	opts := harmonydb.ItestOptions{Hosts: []string{cfg.Host}, Port: strconv.Itoa(int(port)), Database: cfg.Database, Username: user, Password: cfg.Password}
	hcfg := opts.HarmonyConfig()
	hcfg.Schema, hcfg.ITestID, hcfg.ReadOnly, hcfg.LoadBalance = "curio", "", false, false
	hcfg.ApplicationName = "curio-completion-summary-fixture"
	db, err := harmonydb.NewFromConfig(hcfg) // Actual embedded startup/migration runner.
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	var schema, search, host, poolSchema, poolSearch, poolHost string
	require.NoError(t, conn.QueryRow(ctx, `SELECT current_schema(),current_setting('search_path'),host(inet_server_addr())`).Scan(&schema, &search, &host))
	require.NoError(t, db.QueryRow(ctx, `SELECT current_schema(),current_setting('search_path'),host(inet_server_addr())`).Scan(&poolSchema, &poolSearch, &poolHost))
	require.Equal(t, "curio", schema)
	require.Equal(t, schema, poolSchema)
	require.Equal(t, "curio", strings.Trim(search, "\""))
	require.Equal(t, "curio", strings.Trim(poolSearch, "\""))
	require.Equal(t, "127.0.0.1", host)
	require.Equal(t, host, poolHost)
	var applied int
	require.NoError(t, conn.QueryRow(ctx, `SELECT count(*) FROM base WHERE left(entry,8) IN ('20260910','20260912')`).Scan(&applied))
	require.GreaterOrEqual(t, applied, 2, "runner must reach telemetry reconciliation and acquisition migration")
	var version, effective string
	require.NoError(t, conn.QueryRow(ctx, `SELECT version()`).Scan(&version))
	if strings.Contains(version, "-YB-") || strings.Contains(strings.ToLower(version), "yugabyte") {
		require.NoError(t, conn.QueryRow(ctx, `SELECT yb_get_effective_transaction_isolation_level()`).Scan(&effective))
		require.Equal(t, "read committed", effective)
	}
	t.Logf("actual startup runner; schema=%s pool=%s ITestID=empty version=%s effective isolation=%s", schema, poolSchema, version, effective)
	return ctx, db, conn
}
