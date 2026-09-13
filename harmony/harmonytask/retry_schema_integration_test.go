//go:build integration && !skiff

package harmonytask

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

// No regular Curio/libpq defaults are used. Each invocation owns one fresh
// HarmonyDB test namespace and only drops that namespace during cleanup.
func retrySQLFixture(t *testing.T, capture ...func(harmonydb.Config)) (context.Context, *harmonydb.DB, *harmonydb.DB, *pgx.Conn) {
	t.Helper()
	if os.Getenv("CURIO_TASK_ATTEMPT_ITEST") != "1" {
		t.Skip("requires explicit CURIO_TASK_ATTEMPT_ITEST=1 and a disposable loopback target")
	}
	const prefix = "CURIO_TASK_ATTEMPT_ITEST_"
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		for _, ambient := range []string{"PG", "HARMONYQUERY_", "CURIO_HARMONYDB_", "CURIO_DB_"} {
			require.False(t, strings.HasPrefix(key, ambient), "clear inherited variable %s", key)
		}
	}
	host, port := os.Getenv(prefix+"HOST"), os.Getenv(prefix+"PORT")
	address := net.ParseIP(host)
	require.True(t, address != nil && address.IsLoopback(), "HOST must be a literal loopback IP")
	n, err := strconv.ParseUint(port, 10, 16)
	require.NoError(t, err)
	require.NotZero(t, n)
	database, user := os.Getenv(prefix+"DATABASE"), os.Getenv(prefix+"USER")
	require.NotEmpty(t, strings.TrimSpace(database))
	require.NotEmpty(t, strings.TrimSpace(user))
	opts := harmonydb.ItestOptions{Hosts: []string{host}, Port: port, Database: database,
		Username: user, Password: os.Getenv(prefix + "PASSWORD"), ITestID: harmonydb.ITestNewID()}
	cfg := opts.HarmonyConfig()
	if os.Getenv("CURIO_DISPOSABLE_CURIO_SCHEMA") == "1" {
		require.Equal(t, "127.0.0.1", host)
		require.True(t, strings.HasPrefix(database, "curio_test_"))
		cfg.ITestID, cfg.Schema = "", "curio"
		check, err := pgx.ParseConfig("postgresql://placeholder@127.0.0.1/placeholder?sslmode=disable&load_balance=false")
		require.NoError(t, err)
		check.Host, check.Port, check.Database, check.User, check.Password = host, uint16(n), database, user, opts.Password
		check.Fallbacks, check.ConnectTimeout = nil, 5*time.Second
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		admin, err := pgx.ConnectConfig(c, check)
		require.NoError(t, err)
		var exists bool
		err = admin.QueryRow(c, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='curio')").Scan(&exists)
		require.NoError(t, err)
		if exists {
			require.NoError(t, admin.Close(c))
		}
		require.False(t, exists, "curio must be absent: never adopt an existing schema")
		// Own cleanup before startup, including an error in a later pool open.
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 60*time.Second)
			defer stop()
			defer func() {
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer closeCancel()
				require.NoError(t, admin.Close(closeCtx))
			}()
			_, err := admin.Exec(cleanup, "SET statement_timeout='55s'")
			require.NoError(t, err)
			_, err = admin.Exec(cleanup, "SET client_min_messages='warning'")
			require.NoError(t, err)
			_, err = admin.Exec(cleanup, "DROP SCHEMA IF EXISTS curio CASCADE")
			require.NoError(t, err)
		})
	}
	cfg.ReadOnly, cfg.LoadBalance = false, false
	cfg.ApplicationName = "curio-attempt-itest"
	first, err := harmonydb.NewFromConfig(cfg)
	require.NoError(t, err)
	if cfg.Schema != "curio" {
		t.Cleanup(first.ITestDeleteAll)
	}
	cfg.ReadOnly = true
	for _, f := range capture {
		f(cfg)
	}
	second, err := harmonydb.NewFromConfig(cfg)
	require.NoError(t, err)
	if cfg.Schema != "curio" {
		t.Cleanup(second.ITestDeleteAll)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pcfg, err := pgx.ParseConfig("postgresql://placeholder@127.0.0.1/placeholder?sslmode=disable&load_balance=false")
	require.NoError(t, err)
	pcfg.Host, pcfg.Port, pcfg.Database, pcfg.User, pcfg.Password = host, uint16(n), database, user, opts.Password
	pcfg.Fallbacks, pcfg.ConnectTimeout = nil, 5*time.Second
	pcfg.RuntimeParams = map[string]string{"search_path": "itest_" + string(opts.ITestID), "statement_timeout": "5000", "lock_timeout": "2000"}
	if cfg.Schema == "curio" {
		pcfg.RuntimeParams["search_path"] = "curio"
	}
	conn, err := pgx.ConnectConfig(ctx, pcfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		require.NoError(t, conn.Close(closeCtx))
	})
	for label, query := range map[string]func() (string, string, string, error){
		"first pool": func() (string, string, string, error) {
			var s, p, a string
			e := first.QueryRow(ctx, "SELECT current_schema(),current_setting('search_path'),host(inet_server_addr())").Scan(&s, &p, &a)
			return s, p, a, e
		},
		"second pool": func() (string, string, string, error) {
			var s, p, a string
			e := second.QueryRow(ctx, "SELECT current_schema(),current_setting('search_path'),host(inet_server_addr())").Scan(&s, &p, &a)
			return s, p, a, e
		},
		"direct connection": func() (string, string, string, error) {
			var s, p, a string
			e := conn.QueryRow(ctx, "SELECT current_schema(),current_setting('search_path'),host(inet_server_addr())").Scan(&s, &p, &a)
			return s, p, a, e
		},
	} {
		s, p, a, err := query()
		require.NoError(t, err)
		require.Equal(t, pcfg.RuntimeParams["search_path"], s)
		require.Equal(t, s, strings.Trim(p, "\""))
		require.Equal(t, host, a)
		t.Logf("%s: schema=%s search_path=%s ITestID=%q", label, s, p, cfg.ITestID)
	}
	for _, column := range []struct{ table, name, kind string }{
		{"harmony_task", "update_time", "timestamp with time zone"},
		{"harmony_task", "posted_time", "timestamp with time zone"},
		{"harmony_task", "retries", "bigint"},
		{"harmony_task_history", "work_start", "timestamp with time zone"},
	} {
		var kind string
		require.NoError(t, conn.QueryRow(ctx, "SELECT format_type(atttypid,atttypmod) FROM pg_attribute WHERE attrelid=$1::regclass AND attname=$2 AND NOT attisdropped", column.table, column.name).Scan(&kind))
		require.Equal(t, column.kind, kind)
	}
	var applied int
	require.NoError(t, conn.QueryRow(ctx, "SELECT count(*) FROM base WHERE left(entry,8) IN ('20240522','20240927')").Scan(&applied))
	require.Equal(t, 2, applied, "embedded startup runner must reach timestamp and retry migrations")
	_, err = first.Exec(ctx, `INSERT INTO harmony_machines (id,host_and_port,cpu,ram,gpu) VALUES (101,'worker-a.example:12300',8,1024,0),(102,'worker-b.example:12300',8,1024,0)`)
	require.NoError(t, err)
	var version, isolation string
	require.NoError(t, conn.QueryRow(ctx, `SELECT version()`).Scan(&version))
	require.NoError(t, conn.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation))
	if strings.Contains(version, "-YB-") || strings.Contains(strings.ToLower(version), "yugabyte") {
		var effective string
		require.NoError(t, conn.QueryRow(ctx, `SELECT yb_get_effective_transaction_isolation_level()`).Scan(&effective))
		require.Equal(t, "read committed", effective)
		t.Logf("Yugabyte effective isolation=%s; process flags are independently archived", effective)
	}
	t.Logf("database=%s requested isolation=driver default; SQL transaction_isolation=%s", version, isolation)
	return ctx, first, second, conn
}
