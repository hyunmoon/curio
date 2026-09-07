//go:build harmony_itest

package storage_market

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/curiostorage/harmonyquery"
	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/ipfs/go-cid"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

const (
	mk20OffsetITestOptInEnv   = "CURIO_MK20_OFFSET_ITEST"
	mk20OffsetITestKindEnv    = "CURIO_MK20_OFFSET_ITEST_DB_KIND"
	mk20OffsetITestHostEnv    = "CURIO_MK20_OFFSET_ITEST_DB_HOST"
	mk20OffsetITestPortEnv    = "CURIO_MK20_OFFSET_ITEST_DB_PORT"
	mk20OffsetITestNameEnv    = "CURIO_MK20_OFFSET_ITEST_DB_NAME"
	mk20OffsetITestUserEnv    = "CURIO_MK20_OFFSET_ITEST_DB_USER"
	mk20OffsetITestPassEnv    = "CURIO_MK20_OFFSET_ITEST_DB_PASSWORD"
	mk20OffsetITestSSLModeEnv = "CURIO_MK20_OFFSET_ITEST_DB_SSLMODE"

	mk20OffsetITestTimeout = 30 * time.Second
)

type mk20OffsetITestTargetConfig struct {
	Kind     string
	Host     string
	Port     string
	Database string
	Username string
	Password string
	SSLMode  string
}

type mk20OffsetITestDatabase struct {
	DB     *harmonydb.DB
	Target mk20OffsetITestTargetConfig
	Schema string
}

// mk20OffsetITestTarget validates every dedicated setting before a connection
// can be opened. Inherited Curio, HarmonyDB, and libpq settings are rejected.
func mk20OffsetITestTarget(t *testing.T) mk20OffsetITestTargetConfig {
	t.Helper()

	if os.Getenv(mk20OffsetITestOptInEnv) != "1" {
		t.Skipf("set %s=1 and every CURIO_MK20_OFFSET_ITEST_DB_* setting to run this isolated SQL integration test", mk20OffsetITestOptInEnv)
	}
	for _, inherited := range []string{
		"CURIO_HARMONYDB_HOSTS", "CURIO_DB_READONLY", "HARMONYQUERY_HOSTS", "HARMONYQUERY_READONLY", "DATABASE_URL",
		"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE", "PGAPPNAME", "PGCONNECT_TIMEOUT",
		"PGSSLMODE", "PGSSLKEY", "PGSSLCERT", "PGSSLSNI", "PGSSLROOTCERT", "PGSSLPASSWORD", "PGSSLNEGOTIATION",
		"PGTARGETSESSIONATTRS", "PGSERVICE", "PGSERVICEFILE", "PGTZ", "PGOPTIONS",
	} {
		if value := os.Getenv(inherited); value != "" {
			t.Fatalf("%s must be unset; the test supplies its complete connection configuration explicitly", inherited)
		}
	}

	target := mk20OffsetITestTargetConfig{
		Kind:     strings.TrimSpace(os.Getenv(mk20OffsetITestKindEnv)),
		Host:     strings.TrimSpace(os.Getenv(mk20OffsetITestHostEnv)),
		Port:     strings.TrimSpace(os.Getenv(mk20OffsetITestPortEnv)),
		Database: strings.TrimSpace(os.Getenv(mk20OffsetITestNameEnv)),
		Username: strings.TrimSpace(os.Getenv(mk20OffsetITestUserEnv)),
		Password: os.Getenv(mk20OffsetITestPassEnv),
		SSLMode:  strings.TrimSpace(os.Getenv(mk20OffsetITestSSLModeEnv)),
	}

	for _, required := range []struct {
		name  string
		value string
	}{
		{mk20OffsetITestKindEnv, target.Kind},
		{mk20OffsetITestHostEnv, target.Host},
		{mk20OffsetITestPortEnv, target.Port},
		{mk20OffsetITestNameEnv, target.Database},
		{mk20OffsetITestUserEnv, target.Username},
		{mk20OffsetITestPassEnv, target.Password},
		{mk20OffsetITestSSLModeEnv, target.SSLMode},
	} {
		if required.value == "" {
			t.Fatalf("%s must be set explicitly", required.name)
		}
	}

	if target.Kind != "postgresql" && target.Kind != "yugabyte" {
		t.Fatalf("%s must be postgresql or yugabyte", mk20OffsetITestKindEnv)
	}
	if target.Host != "127.0.0.1" {
		t.Fatalf("%s must be the literal loopback address 127.0.0.1", mk20OffsetITestHostEnv)
	}
	if !mk20OffsetITestIdentifier(target.Database) {
		t.Fatalf("%s must contain only ASCII letters, digits, and underscores", mk20OffsetITestNameEnv)
	}
	port, err := strconv.Atoi(target.Port)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("%s must be an integer in the range 1..65535", mk20OffsetITestPortEnv)
	}
	if target.SSLMode != "disable" {
		t.Fatalf("%s must be disable for the loopback-only test target", mk20OffsetITestSSLModeEnv)
	}

	return target
}

func mk20OffsetITestIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func newMK20OffsetITestDatabase(t *testing.T) mk20OffsetITestDatabase {
	t.Helper()
	target := mk20OffsetITestTarget(t)
	id := newMK20OffsetITestID(t)
	schema := "itest_" + string(id)

	db, err := harmonydb.NewFromConfig(harmonydb.Config{
		Hosts:       []string{target.Host},
		Database:    target.Database,
		Username:    target.Username,
		Password:    target.Password,
		Port:        target.Port,
		LoadBalance: false,
		// ReadOnly suppresses the unrelated production migration history during
		// construction. HarmonyDB still permits the test-owned fixture writes.
		ReadOnly:        true,
		SSLMode:         target.SSLMode,
		ApplicationName: "curio-mk20-offset-itest",
		ITestID:         id,
		UseTemplate:     false,
	})
	if db != nil {
		t.Cleanup(db.ITestDeleteAll)
	}
	require.NoError(t, err)
	verifyMK20OffsetITestNamespace(t, db, target.Database, schema)
	logMK20OffsetITestDatabase(t, db, target.Kind)
	applyMK20OffsetITestSchema(t, db)

	return mk20OffsetITestDatabase{DB: db, Target: target, Schema: schema}
}

func newMK20OffsetITestPeer(t *testing.T, owner mk20OffsetITestDatabase) *harmonydb.DB {
	t.Helper()
	peer, err := harmonydb.NewFromConfig(harmonydb.Config{
		Hosts:           []string{owner.Target.Host},
		Database:        owner.Target.Database,
		Username:        owner.Target.Username,
		Password:        owner.Target.Password,
		Port:            owner.Target.Port,
		LoadBalance:     false,
		ReadOnly:        true,
		SSLMode:         owner.Target.SSLMode,
		Schema:          owner.Schema,
		ApplicationName: "curio-mk20-offset-itest-peer",
	})
	if peer != nil {
		// ITestDeleteAll closes this independent pool and is restricted to the
		// owner's generated itest_ namespace. The owner's later cleanup may log
		// an expected missing-schema warning after this peer drops it first.
		t.Cleanup(peer.ITestDeleteAll)
	}
	require.NoError(t, err)
	verifyMK20OffsetITestNamespace(t, peer, owner.Target.Database, owner.Schema)
	return peer
}

func newMK20OffsetITestID(t *testing.T) harmonyquery.ITestID {
	t.Helper()
	bytes := make([]byte, 16)
	_, err := rand.Read(bytes)
	require.NoError(t, err)
	return harmonyquery.ITestID(hex.EncodeToString(bytes))
}

func verifyMK20OffsetITestNamespace(t *testing.T, db *harmonydb.DB, database, schema string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	var rows []struct {
		Database string `db:"database_name"`
		Schema   string `db:"schema_name"`
	}
	require.NoError(t, db.Select(ctx, &rows, `SELECT current_database() AS database_name, current_schema() AS schema_name`))
	require.Equal(t, []struct {
		Database string `db:"database_name"`
		Schema   string `db:"schema_name"`
	}{{Database: database, Schema: schema}}, rows)
}

func logMK20OffsetITestDatabase(t *testing.T, db *harmonydb.DB, kind string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()

	var versions []struct {
		Version string `db:"version"`
	}
	require.NoError(t, db.Select(ctx, &versions, "SELECT version() AS version"))
	require.Len(t, versions, 1)
	version := strings.ToLower(versions[0].Version)
	isYugabyte := strings.Contains(version, "yugabyte") || strings.Contains(version, "-yb-")
	if kind == "yugabyte" {
		require.True(t, isYugabyte, "configured Yugabyte target reported unexpected version %q", versions[0].Version)
	} else {
		require.Contains(t, version, "postgresql")
		require.False(t, isYugabyte, "configured PostgreSQL target reported Yugabyte version %q", versions[0].Version)
	}

	var isolation []struct {
		Isolation string `db:"transaction_isolation"`
	}
	require.NoError(t, db.Select(ctx, &isolation, "SHOW transaction_isolation"))
	require.Len(t, isolation, 1)

	t.Logf("database kind=%s version=%q requested isolation=pgx/server default reported transaction_isolation=%q", kind, versions[0].Version, isolation[0].Isolation)
	if kind == "yugabyte" {
		t.Log("Yugabyte effective isolation=UNVERIFIED; SHOW transaction_isolation alone does not establish the effective isolation mode")
	}
}

func applyMK20OffsetITestSchema(t *testing.T, db *harmonydb.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()

	// This is a focused projection of the current schema. It retains the
	// production key types and the FK/cascade relationships relevant to offset
	// source selection and locking. In particular, open_sector_pieces
	// deliberately has no FK to retained metadata.
	_, err := db.Exec(ctx, `CREATE TABLE sectors_meta (
			sp_id BIGINT NOT NULL,
			sector_num BIGINT NOT NULL,
			reg_seal_proof INT NOT NULL,
			ticket_epoch BIGINT NOT NULL,
			ticket_value BYTEA NOT NULL,
			orig_sealed_cid TEXT NOT NULL,
			orig_unsealed_cid TEXT NOT NULL,
			cur_sealed_cid TEXT NOT NULL,
			cur_unsealed_cid TEXT NOT NULL,
			seed_epoch BIGINT NOT NULL,
			seed_value BYTEA NOT NULL,
			is_cc BOOLEAN,
			has_sector_key BOOLEAN NOT NULL DEFAULT FALSE,
			PRIMARY KEY (sp_id, sector_num)
		)`)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `CREATE TABLE sectors_meta_pieces (
			sp_id BIGINT NOT NULL,
			sector_num BIGINT NOT NULL,
			piece_num BIGINT NOT NULL,
			piece_cid TEXT NOT NULL,
			piece_size BIGINT NOT NULL,
			requested_keep_data BOOLEAN NOT NULL,
			raw_data_size BIGINT,
			start_epoch BIGINT,
			orig_end_epoch BIGINT,
			f05_deal_id BIGINT,
			ddo_pam JSONB,
			f05_deal_proposal JSONB,
			PRIMARY KEY (sp_id, sector_num, piece_num),
			FOREIGN KEY (sp_id, sector_num)
				REFERENCES sectors_meta (sp_id, sector_num) ON DELETE CASCADE
		)`)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `CREATE TABLE sectors_sdr_pipeline (
			sp_id BIGINT NOT NULL,
			sector_number BIGINT NOT NULL,
			create_time TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			reg_seal_proof INT NOT NULL,
			PRIMARY KEY (sp_id, sector_number)
		)`)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `CREATE TABLE sectors_sdr_initial_pieces (
			sp_id BIGINT NOT NULL,
			sector_number BIGINT NOT NULL,
			piece_index BIGINT NOT NULL,
			piece_cid TEXT NOT NULL,
			piece_size BIGINT NOT NULL,
			data_url TEXT NOT NULL,
			data_headers JSONB NOT NULL DEFAULT '{}',
			data_raw_size BIGINT NOT NULL,
			data_delete_on_finalize BOOLEAN NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (sp_id, sector_number, piece_index),
			FOREIGN KEY (sp_id, sector_number)
				REFERENCES sectors_sdr_pipeline (sp_id, sector_number) ON DELETE CASCADE
		)`)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `CREATE TABLE sectors_snap_pipeline (
			sp_id BIGINT NOT NULL,
			sector_number BIGINT NOT NULL,
			start_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			upgrade_proof INT NOT NULL,
			data_assigned BOOLEAN NOT NULL DEFAULT TRUE,
			PRIMARY KEY (sp_id, sector_number),
			FOREIGN KEY (sp_id, sector_number)
				REFERENCES sectors_meta (sp_id, sector_num)
		)`)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `CREATE TABLE sectors_snap_initial_pieces (
			sp_id BIGINT NOT NULL,
			sector_number BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			piece_index BIGINT NOT NULL,
			piece_cid TEXT NOT NULL,
			piece_size BIGINT NOT NULL,
			data_url TEXT NOT NULL,
			data_headers JSONB NOT NULL DEFAULT '{}',
			data_raw_size BIGINT NOT NULL,
			data_delete_on_finalize BOOLEAN NOT NULL,
			PRIMARY KEY (sp_id, sector_number, piece_index),
			FOREIGN KEY (sp_id, sector_number)
				REFERENCES sectors_snap_pipeline (sp_id, sector_number) ON DELETE CASCADE
		)`)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `CREATE TABLE open_sector_pieces (
			sp_id BIGINT NOT NULL,
			sector_number BIGINT NOT NULL,
			piece_index BIGINT NOT NULL,
			piece_cid TEXT NOT NULL,
			piece_size BIGINT NOT NULL,
			data_url TEXT NOT NULL,
			data_headers JSONB NOT NULL DEFAULT '{}',
			data_raw_size BIGINT NOT NULL,
			data_delete_on_finalize BOOLEAN NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			is_snap BOOLEAN NOT NULL DEFAULT FALSE,
			PRIMARY KEY (sp_id, sector_number, piece_index)
		)`)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `CREATE TABLE market_mk20_pipeline (
			id TEXT NOT NULL,
			sp_id BIGINT NOT NULL,
			contract TEXT NOT NULL,
			client TEXT NOT NULL,
			piece_cid_v2 TEXT NOT NULL,
			piece_cid TEXT NOT NULL,
			piece_size BIGINT NOT NULL,
			raw_size BIGINT NOT NULL,
			offline BOOLEAN NOT NULL,
			indexing BOOLEAN NOT NULL,
			announce BOOLEAN NOT NULL,
			duration BIGINT NOT NULL,
			aggr_index BIGINT NOT NULL DEFAULT 0,
			sector BIGINT,
			reg_seal_proof INT,
			sector_offset BIGINT,
			PRIMARY KEY (id, aggr_index)
		)`)
	require.NoError(t, err)
}

func TestMK20OffsetDBProductionRoundTrip(t *testing.T) {
	t.Run("initial pieces remain authoritative", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		target, layout, expected := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
		retained := retainedLayout(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1, []mk20RetainedPiece{
			{Index: 0, CID: target.PieceCID, Size: target.PieceSize},
			{Index: 1, CID: syntheticPieceCID(t, 70), Size: 128},
		})
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, retained, retained.UnsealedCID)
		insertMK20OffsetITestSDRInitial(t, fixture.DB, target, layout.Pieces)

		other := target
		other.AggregationIndex++
		other.SPID++
		other.Sector++
		insertMK20OffsetITestTarget(t, fixture.DB, other, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, other, layout, layout.UnsealedCID)

		require.NoError(t, callMK20AddDealOffset(t, fixture.DB, target))
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
		require.False(t, readMK20OffsetITestOffset(t, fixture.DB, other).Valid)
	})

	t.Run("Snap initial pieces remain authoritative", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		proof := abi.RegisteredSealProof_StackedDrg2KiBV1_1
		target, layout, expected := retainedOffsetFixture(t, proof)
		retained := retainedLayout(t, proof, []mk20RetainedPiece{
			{Index: 0, CID: target.PieceCID, Size: target.PieceSize},
			{Index: 1, CID: syntheticPieceCID(t, 74), Size: 128},
		})
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, retained, retained.UnsealedCID)
		insertMK20OffsetITestSnapInitial(t, fixture.DB, target, layout.Pieces)

		require.NoError(t, callMK20AddDealOffset(t, fixture.DB, target))
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
	})

	t.Run("GC-shaped SDR transition uses retained metadata", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		target, layout, expected := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)
		insertMK20OffsetITestSDRInitial(t, fixture.DB, target, layout.Pieces)

		ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
		defer cancel()
		updated, err := fixture.DB.Exec(ctx, `DELETE FROM sectors_sdr_pipeline WHERE sp_id = $1 AND sector_number = $2`, target.SPID, target.Sector)
		require.NoError(t, err)
		require.Equal(t, 1, updated)
		require.Zero(t, countMK20OffsetITestSDRInitialRows(t, fixture.DB, target))

		require.NoError(t, callMK20AddDealOffset(t, fixture.DB, target))
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
	})

	t.Run("retained single piece persists zero", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		proof := abi.RegisteredSealProof_StackedDrg2KiBV1_1
		piece := mk20RetainedPiece{Index: 0, CID: syntheticPieceCID(t, 71), Size: 512}
		layout := retainedLayout(t, proof, []mk20RetainedPiece{piece})
		target := defaultMK20OffsetTarget(proof, piece.CID, piece.Size)
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)

		require.NoError(t, callMK20AddDealOffset(t, fixture.DB, target))
		require.Equal(t, sql.NullInt64{Int64: 0, Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
	})

	t.Run("populated offset is never overwritten", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		target, layout, _ := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
		insertMK20OffsetITestTarget(t, fixture.DB, target, int64(777))
		insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)

		require.NoError(t, callMK20AddDealOffset(t, fixture.DB, target))
		require.Equal(t, sql.NullInt64{Int64: 777, Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
	})

	for _, state := range []string{"open", "SDR pipeline", "Snap pipeline"} {
		t.Run(state+" blocks retained fallback", func(t *testing.T) {
			fixture := newMK20OffsetITestDatabase(t)
			target, layout, _ := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
			insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
			insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)
			insertMK20OffsetITestLifecycleState(t, fixture.DB, target, state)

			require.NoError(t, callMK20AddDealOffset(t, fixture.DB, target))
			require.False(t, readMK20OffsetITestOffset(t, fixture.DB, target).Valid)
		})
	}

	t.Run("missing and invalid retained layouts fabricate no offset", func(t *testing.T) {
		for _, invalid := range []string{"missing", "missing target", "commitment mismatch"} {
			t.Run(invalid, func(t *testing.T) {
				fixture := newMK20OffsetITestDatabase(t)
				target, layout, _ := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
				insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
				switch invalid {
				case "missing target":
					target.PieceCID = syntheticPieceCID(t, 72)
					deleteAndReinsertMK20OffsetITestTarget(t, fixture.DB, target)
					insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)
				case "commitment mismatch":
					insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, syntheticPieceCID(t, 73))
				}

				err := callMK20AddDealOffset(t, fixture.DB, target)
				if invalid == "missing" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				require.False(t, readMK20OffsetITestOffset(t, fixture.DB, target).Valid)
			})
		}
	})
}

func TestMK20OffsetDBConditionalUpdateGuards(t *testing.T) {
	t.Run("valid production array parameters update exactly one row", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		target, layout, expected := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)
		guard := mk20OffsetITestGuard(layout)

		updated, err := persistMK20OffsetITestGuard(t, fixture.DB, target, expected, guard, true)
		require.NoError(t, err)
		require.True(t, updated)
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
	})

	for _, stale := range []string{
		"MK20 aggregate index", "MK20 provider", "MK20 sector", "MK20 proof", "MK20 piece CID", "MK20 piece size",
		"metadata proof", "metadata commitment", "piece numbers", "piece CIDs", "piece sizes",
		"open lifecycle", "SDR lifecycle", "Snap lifecycle", "already populated",
	} {
		t.Run(stale+" affects zero rows", func(t *testing.T) {
			fixture := newMK20OffsetITestDatabase(t)
			target, layout, expected := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
			insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
			insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)
			guard := mk20OffsetITestGuard(layout)
			persistTarget := target
			other := target
			other.AggregationIndex += 100
			other.SPID += 100
			other.Sector += 100
			insertMK20OffsetITestTarget(t, fixture.DB, other, nil)

			switch stale {
			case "MK20 aggregate index":
				persistTarget.AggregationIndex++
			case "MK20 provider":
				persistTarget.SPID++
			case "MK20 sector":
				persistTarget.Sector++
			case "MK20 proof":
				persistTarget.RegSealProof++
			case "MK20 piece CID":
				persistTarget.PieceCID = syntheticPieceCID(t, 80)
			case "MK20 piece size":
				persistTarget.PieceSize *= 2
			case "metadata proof":
				setMK20OffsetITestMetadataProof(t, fixture.DB, target, target.RegSealProof+1)
			case "metadata commitment":
				guard.UnsealedCID = syntheticPieceCID(t, 81)
			case "piece numbers":
				guard.PieceNums[1]++
			case "piece CIDs":
				guard.PieceCIDs[1] = syntheticPieceCID(t, 82)
			case "piece sizes":
				guard.PieceSizes[1] *= 2
			case "open lifecycle":
				insertMK20OffsetITestLifecycleState(t, fixture.DB, target, "open")
			case "SDR lifecycle":
				insertMK20OffsetITestLifecycleState(t, fixture.DB, target, "SDR pipeline")
			case "Snap lifecycle":
				insertMK20OffsetITestLifecycleState(t, fixture.DB, target, "Snap pipeline")
			case "already populated":
				setMK20OffsetITestOffset(t, fixture.DB, target, 999)
			}

			updated, err := persistMK20OffsetITestGuard(t, fixture.DB, persistTarget, expected, guard, true)
			require.NoError(t, err)
			require.False(t, updated)
			if stale == "already populated" {
				require.Equal(t, sql.NullInt64{Int64: 999, Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
			} else {
				require.False(t, readMK20OffsetITestOffset(t, fixture.DB, target).Valid)
			}
			require.False(t, readMK20OffsetITestOffset(t, fixture.DB, other).Valid)
		})
	}

	t.Run("transaction rollback leaves no partial offset", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		target, layout, expected := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)

		updated, err := persistMK20OffsetITestGuard(t, fixture.DB, target, expected, mk20OffsetITestGuard(layout), false)
		require.NoError(t, err)
		require.True(t, updated, "the statement must have affected the row before the enclosing rollback")
		require.False(t, readMK20OffsetITestOffset(t, fixture.DB, target).Valid)
	})
}

func TestMK20OffsetDBIndependentConnections(t *testing.T) {
	t.Run("two stale discoveries cannot overwrite the first offset", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		peer := newMK20OffsetITestPeer(t, fixture)
		target, layout, expected := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)

		firstResolved := make(chan struct{})
		var firstResolvedOnce sync.Once
		releaseFirst := make(chan struct{})
		var releaseFirstOnce sync.Once
		t.Cleanup(func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) })
		firstDone := make(chan mk20OffsetITestTxnResult, 1)
		secondReturned := make(chan struct{})
		var secondReturnedOnce sync.Once
		secondPID := make(chan int, 1)
		var secondPIDOnce sync.Once
		secondDone := make(chan mk20OffsetITestTxnResult, 1)

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
			defer cancel()
			committed, err := fixture.DB.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
				updated, resolveErr := resolveMK20PieceOffset(&harmonyMK20OffsetStore{tx: tx}, target)
				firstResolvedOnce.Do(func() { close(firstResolved) })
				select {
				case <-releaseFirst:
				case <-ctx.Done():
					return false, ctx.Err()
				}
				return updated, resolveErr
			}, harmonydb.OptionRetry())
			firstDone <- mk20OffsetITestTxnResult{Committed: committed, Err: err}
		}()

		requireSignalBefore(t, firstResolved, mk20OffsetITestTimeout)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
			defer cancel()
			var finalUpdated bool
			committed, err := peer.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
				finalUpdated = false
				pid, pidErr := mk20OffsetITestBackendPID(tx)
				if pidErr != nil {
					return false, pidErr
				}
				secondPIDOnce.Do(func() { secondPID <- pid })
				store := &mk20OffsetITestLockProbeStore{
					harmonyMK20OffsetStore: harmonyMK20OffsetStore{tx: tx},
					returned:               secondReturned,
					doneOnce:               &secondReturnedOnce,
				}
				updated, resolveErr := resolveMK20PieceOffset(store, target)
				finalUpdated = updated
				return updated, resolveErr
			}, harmonydb.OptionRetry())
			secondDone <- mk20OffsetITestTxnResult{Committed: committed, Updated: finalUpdated, Err: err}
		}()

		pid := requireIntResult(t, secondPID)
		lockObserved, observeErr := observeMK20OffsetITestLockWait(fixture.DB, pid, secondReturned, 2*time.Second)
		releaseFirstOnce.Do(func() { close(releaseFirst) })

		first := requireTxnResult(t, firstDone)
		require.NoError(t, first.Err)
		require.True(t, first.Committed)
		second := requireTxnResult(t, secondDone)
		if second.Err != nil {
			require.True(t, harmonydb.IsErrSerialization(second.Err), "unexpected second-attempt error: %v", second.Err)
		} else {
			require.False(t, second.Committed)
			require.False(t, second.Updated)
		}
		requireMK20OffsetITestLockObservation(t, fixture.Target.Kind, lockObserved, observeErr, "second MK20 target lock")
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
	})

	t.Run("existing retained rows coordinate a content-changing metadata writer", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		peer := newMK20OffsetITestPeer(t, fixture)
		target, layout, expected := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)
		alternatePieces := append([]mk20RetainedPiece(nil), layout.Pieces...)
		alternatePieces[0].CID = syntheticPieceCID(t, 90)
		alternateLayout := retainedLayout(t, abi.RegisteredSealProof(target.RegSealProof), alternatePieces)

		resolved := make(chan struct{})
		var resolvedOnce sync.Once
		releaseResolver := make(chan struct{})
		var releaseResolverOnce sync.Once
		t.Cleanup(func() { releaseResolverOnce.Do(func() { close(releaseResolver) }) })
		resolverDone := make(chan mk20OffsetITestTxnResult, 1)
		writerReturned := make(chan struct{})
		var writerReturnedOnce sync.Once
		writerPID := make(chan int, 1)
		var writerPIDOnce sync.Once
		writerDone := make(chan error, 1)

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
			defer cancel()
			committed, err := fixture.DB.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
				updated, resolveErr := resolveMK20PieceOffset(&harmonyMK20OffsetStore{tx: tx}, target)
				resolvedOnce.Do(func() { close(resolved) })
				select {
				case <-releaseResolver:
				case <-ctx.Done():
					return false, ctx.Err()
				}
				return updated, resolveErr
			}, harmonydb.OptionRetry())
			resolverDone <- mk20OffsetITestTxnResult{Committed: committed, Err: err}
		}()

		requireSignalBefore(t, resolved, mk20OffsetITestTimeout)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
			defer cancel()
			_, err := peer.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
				pid, pidErr := mk20OffsetITestBackendPID(tx)
				if pidErr != nil {
					return false, pidErr
				}
				writerPIDOnce.Do(func() { writerPID <- pid })
				n, updateErr := tx.Exec(`UPDATE sectors_meta SET cur_unsealed_cid = $1 WHERE sp_id = $2 AND sector_num = $3`, alternateLayout.UnsealedCID, target.SPID, target.Sector)
				if updateErr != nil {
					writerReturnedOnce.Do(func() { close(writerReturned) })
					return false, updateErr
				}
				if n != 1 {
					writerReturnedOnce.Do(func() { close(writerReturned) })
					return false, errors.New("metadata header writer affected an unexpected number of rows")
				}
				n, updateErr = tx.Exec(`UPDATE sectors_meta_pieces SET piece_cid = $1 WHERE sp_id = $2 AND sector_num = $3 AND piece_num = 0`, alternateLayout.Pieces[0].CID, target.SPID, target.Sector)
				writerReturnedOnce.Do(func() { close(writerReturned) })
				if updateErr != nil {
					return false, updateErr
				}
				return n == 1, nil
			}, harmonydb.OptionRetry())
			writerDone <- err
		}()

		pid := requireIntResult(t, writerPID)
		lockObserved, observeErr := observeMK20OffsetITestLockWait(fixture.DB, pid, writerReturned, 2*time.Second)
		releaseResolverOnce.Do(func() { close(releaseResolver) })

		resolvedResult := requireTxnResult(t, resolverDone)
		require.NoError(t, resolvedResult.Err)
		require.True(t, resolvedResult.Committed)
		writerErr := requireErrorResult(t, writerDone)
		if writerErr != nil {
			require.True(t, harmonydb.IsErrSerialization(writerErr), "unexpected metadata-writer error: %v", writerErr)
		}
		requireMK20OffsetITestLockObservation(t, fixture.Target.Kind, lockObserved, observeErr, "retained metadata row lock")
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
		metadata := readMK20OffsetITestMetadataState(t, fixture.DB, target)
		if writerErr == nil {
			require.Equal(t, mk20OffsetITestMetadataState{UnsealedCID: alternateLayout.UnsealedCID, FirstPieceCID: alternateLayout.Pieces[0].CID}, metadata)
		} else {
			require.Equal(t, mk20OffsetITestMetadataState{UnsealedCID: layout.UnsealedCID, FirstPieceCID: layout.Pieces[0].CID}, metadata)
		}
	})

	t.Run("Snap FK transition waits for retained parent lock", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		peer := newMK20OffsetITestPeer(t, fixture)
		target, layout, expected := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)
		updateProof := mk20OffsetITestUpdateProof(t, target)

		resolved := make(chan struct{})
		var resolvedOnce sync.Once
		releaseResolver := make(chan struct{})
		var releaseResolverOnce sync.Once
		t.Cleanup(func() { releaseResolverOnce.Do(func() { close(releaseResolver) }) })
		resolverDone := make(chan mk20OffsetITestTxnResult, 1)
		writerReturned := make(chan struct{})
		var writerReturnedOnce sync.Once
		writerPID := make(chan int, 1)
		var writerPIDOnce sync.Once
		writerDone := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
			defer cancel()
			committed, err := fixture.DB.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
				updated, resolveErr := resolveMK20PieceOffset(&harmonyMK20OffsetStore{tx: tx}, target)
				resolvedOnce.Do(func() { close(resolved) })
				select {
				case <-releaseResolver:
				case <-ctx.Done():
					return false, ctx.Err()
				}
				return updated, resolveErr
			}, harmonydb.OptionRetry())
			resolverDone <- mk20OffsetITestTxnResult{Committed: committed, Err: err}
		}()

		requireSignalBefore(t, resolved, mk20OffsetITestTimeout)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
			defer cancel()
			_, err := peer.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
				pid, pidErr := mk20OffsetITestBackendPID(tx)
				if pidErr != nil {
					return false, pidErr
				}
				writerPIDOnce.Do(func() { writerPID <- pid })
				n, insertErr := tx.Exec(`INSERT INTO sectors_snap_pipeline (sp_id, sector_number, upgrade_proof) VALUES ($1, $2, $3)`, target.SPID, target.Sector, updateProof)
				writerReturnedOnce.Do(func() { close(writerReturned) })
				if insertErr != nil {
					return false, insertErr
				}
				return n == 1, nil
			}, harmonydb.OptionRetry())
			writerDone <- err
		}()

		pid := requireIntResult(t, writerPID)
		lockObserved, observeErr := observeMK20OffsetITestLockWait(fixture.DB, pid, writerReturned, 2*time.Second)
		releaseResolverOnce.Do(func() { close(releaseResolver) })

		resolvedResult := requireTxnResult(t, resolverDone)
		require.NoError(t, resolvedResult.Err)
		require.True(t, resolvedResult.Committed)
		writerErr := requireErrorResult(t, writerDone)
		if writerErr != nil {
			require.True(t, harmonydb.IsErrSerialization(writerErr), "unexpected Snap-writer error: %v", writerErr)
			require.Zero(t, countMK20OffsetITestSnapRows(t, fixture.DB, target))
		} else {
			require.Equal(t, 1, countMK20OffsetITestSnapRows(t, fixture.DB, target))
		}
		requireMK20OffsetITestLockObservation(t, fixture.Target.Kind, lockObserved, observeErr, "Snap metadata foreign-key lock")
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
	})
}

type mk20OffsetITestTxnResult struct {
	Committed bool
	Updated   bool
	Err       error
}

type mk20OffsetITestLockProbeStore struct {
	harmonyMK20OffsetStore
	returned chan struct{}
	doneOnce *sync.Once
}

func (s *mk20OffsetITestLockProbeStore) lockTarget(target mk20OffsetTarget) (bool, error) {
	locked, err := s.harmonyMK20OffsetStore.lockTarget(target)
	s.doneOnce.Do(func() { close(s.returned) })
	return locked, err
}

func callMK20AddDealOffset(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	market := &CurioStorageDealMarket{db: db}
	return market.addDealOffset(ctx, MK20PipelinePiece{
		ID:               target.ID,
		SPID:             target.SPID,
		AggregationIndex: target.AggregationIndex,
		PieceCID:         target.PieceCID,
		PieceSize:        target.PieceSize,
		Sector:           sql.NullInt64{Int64: target.Sector, Valid: true},
		RegSealProof:     sql.NullInt64{Int64: target.RegSealProof, Valid: true},
	})
}

func insertMK20OffsetITestTarget(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget, offset any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	pieceCIDV2, rawSize := mk20OffsetITestPieceCIDV2(t, target.PieceCID, target.PieceSize)
	n, err := db.Exec(ctx, `INSERT INTO market_mk20_pipeline (
		id, sp_id, contract, client, piece_cid_v2, piece_cid, piece_size,
		raw_size, offline, indexing, announce, duration, aggr_index,
		sector, reg_seal_proof, sector_offset
	) VALUES ($1, $2, 'synthetic-contract', 'synthetic-client', $3, $4, $5,
		$6, FALSE, TRUE, FALSE, 1000, $7, $8, $9, $10)`,
		target.ID, target.SPID, pieceCIDV2, target.PieceCID, target.PieceSize, rawSize,
		target.AggregationIndex, target.Sector, target.RegSealProof, offset)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func deleteAndReinsertMK20OffsetITestTarget(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	_, err := db.Exec(ctx, `DELETE FROM market_mk20_pipeline WHERE id = $1 AND aggr_index = $2`, target.ID, target.AggregationIndex)
	require.NoError(t, err)
	insertMK20OffsetITestTarget(t, db, target, nil)
}

func insertMK20OffsetITestMetadata(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget, layout mk20RetainedLayout, unsealedCID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	sealedCID := mk20OffsetITestSealedCID(t, 91)
	n, err := db.Exec(ctx, `INSERT INTO sectors_meta (
		sp_id, sector_num, reg_seal_proof, ticket_epoch, ticket_value,
		orig_sealed_cid, orig_unsealed_cid, cur_sealed_cid, cur_unsealed_cid,
		seed_epoch, seed_value, is_cc
	) VALUES ($1, $2, $3, 0, $4, $5, $6, $5, $6, 0, $4, FALSE)`,
		target.SPID, target.Sector, layout.RegSealProof, []byte{0}, sealedCID, unsealedCID)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	for _, piece := range layout.Pieces {
		n, err = db.Exec(ctx, `INSERT INTO sectors_meta_pieces (
			sp_id, sector_num, piece_num, piece_cid, piece_size, requested_keep_data
		) VALUES ($1, $2, $3, $4, $5, FALSE)`,
			target.SPID, target.Sector, piece.Index, piece.CID, piece.Size)
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}
}

func insertMK20OffsetITestSDRInitial(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget, pieces []mk20RetainedPiece) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	n, err := db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline (sp_id, sector_number, reg_seal_proof) VALUES ($1, $2, $3)`, target.SPID, target.Sector, target.RegSealProof)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	for _, piece := range pieces {
		rawSize := int64(abi.PaddedPieceSize(piece.Size).Unpadded())
		n, err = db.Exec(ctx, `INSERT INTO sectors_sdr_initial_pieces (
			sp_id, sector_number, piece_index, piece_cid, piece_size,
			data_url, data_raw_size, data_delete_on_finalize
		) VALUES ($1, $2, $3, $4, $5, 'pieceref:synthetic', $6, FALSE)`,
			target.SPID, target.Sector, piece.Index, piece.CID, piece.Size, rawSize)
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}
}

func insertMK20OffsetITestSnapInitial(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget, pieces []mk20RetainedPiece) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	n, err := db.Exec(ctx, `INSERT INTO sectors_snap_pipeline (sp_id, sector_number, upgrade_proof) VALUES ($1, $2, $3)`, target.SPID, target.Sector, mk20OffsetITestUpdateProof(t, target))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	for _, piece := range pieces {
		rawSize := int64(abi.PaddedPieceSize(piece.Size).Unpadded())
		n, err = db.Exec(ctx, `INSERT INTO sectors_snap_initial_pieces (
			sp_id, sector_number, created_at, piece_index, piece_cid, piece_size,
			data_url, data_raw_size, data_delete_on_finalize
		) VALUES ($1, $2, CURRENT_TIMESTAMP, $3, $4, $5, 'pieceref:synthetic', $6, FALSE)`,
			target.SPID, target.Sector, piece.Index, piece.CID, piece.Size, rawSize)
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}
}

func insertMK20OffsetITestLifecycleState(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget, state string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	var (
		n   int
		err error
	)
	switch state {
	case "open":
		rawSize := int64(abi.PaddedPieceSize(target.PieceSize).Unpadded())
		n, err = db.Exec(ctx, `INSERT INTO open_sector_pieces (
			sp_id, sector_number, piece_index, piece_cid, piece_size,
			data_url, data_raw_size, data_delete_on_finalize
		) VALUES ($1, $2, 0, $3, $4, 'pieceref:synthetic', $5, FALSE)`,
			target.SPID, target.Sector, target.PieceCID, target.PieceSize, rawSize)
	case "SDR pipeline":
		n, err = db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline (sp_id, sector_number, reg_seal_proof) VALUES ($1, $2, $3)`, target.SPID, target.Sector, target.RegSealProof)
	case "Snap pipeline":
		n, err = db.Exec(ctx, `INSERT INTO sectors_snap_pipeline (sp_id, sector_number, upgrade_proof) VALUES ($1, $2, $3)`, target.SPID, target.Sector, mk20OffsetITestUpdateProof(t, target))
	default:
		t.Fatalf("unknown lifecycle fixture %q", state)
	}
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func setMK20OffsetITestOffset(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget, offset int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	n, err := db.Exec(ctx, `UPDATE market_mk20_pipeline SET sector_offset = $1 WHERE id = $2 AND aggr_index = $3`, offset, target.ID, target.AggregationIndex)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func setMK20OffsetITestMetadataProof(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget, proof int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	n, err := db.Exec(ctx, `UPDATE sectors_meta SET reg_seal_proof = $1 WHERE sp_id = $2 AND sector_num = $3`, proof, target.SPID, target.Sector)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func readMK20OffsetITestOffset(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget) sql.NullInt64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	var rows []struct {
		Offset sql.NullInt64 `db:"sector_offset"`
	}
	require.NoError(t, db.Select(ctx, &rows, `SELECT sector_offset FROM market_mk20_pipeline WHERE id = $1 AND aggr_index = $2`, target.ID, target.AggregationIndex))
	require.Len(t, rows, 1)
	return rows[0].Offset
}

type mk20OffsetITestMetadataState struct {
	UnsealedCID   string `db:"cur_unsealed_cid"`
	FirstPieceCID string `db:"piece_cid"`
}

func readMK20OffsetITestMetadataState(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget) mk20OffsetITestMetadataState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	var rows []mk20OffsetITestMetadataState
	require.NoError(t, db.Select(ctx, &rows, `SELECT sm.cur_unsealed_cid, smp.piece_cid
		FROM sectors_meta sm
		JOIN sectors_meta_pieces smp ON smp.sp_id = sm.sp_id AND smp.sector_num = sm.sector_num
		WHERE sm.sp_id = $1 AND sm.sector_num = $2 AND smp.piece_num = 0`, target.SPID, target.Sector))
	require.Len(t, rows, 1)
	return rows[0]
}

func countMK20OffsetITestSDRInitialRows(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	var rows []struct {
		Count int `db:"count"`
	}
	require.NoError(t, db.Select(ctx, &rows, `SELECT COUNT(*) AS count FROM sectors_sdr_initial_pieces WHERE sp_id = $1 AND sector_number = $2`, target.SPID, target.Sector))
	require.Len(t, rows, 1)
	return rows[0].Count
}

func countMK20OffsetITestSnapRows(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	var rows []struct {
		Count int `db:"count"`
	}
	require.NoError(t, db.Select(ctx, &rows, `SELECT COUNT(*) AS count FROM sectors_snap_pipeline WHERE sp_id = $1 AND sector_number = $2`, target.SPID, target.Sector))
	require.Len(t, rows, 1)
	return rows[0].Count
}

func mk20OffsetITestGuard(layout mk20RetainedLayout) *mk20OffsetMetadataGuard {
	guard := &mk20OffsetMetadataGuard{
		UnsealedCID: layout.UnsealedCID,
		PieceNums:   make([]int64, len(layout.Pieces)),
		PieceCIDs:   make([]string, len(layout.Pieces)),
		PieceSizes:  make([]int64, len(layout.Pieces)),
	}
	for index, piece := range layout.Pieces {
		guard.PieceNums[index] = piece.Index
		guard.PieceCIDs[index] = piece.CID
		guard.PieceSizes[index] = piece.Size
	}
	return guard
}

func persistMK20OffsetITestGuard(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget, offset abi.PaddedPieceSize, guard *mk20OffsetMetadataGuard, commit bool) (bool, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	var updated bool
	_, err := db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		updated = false
		attemptUpdated, persistErr := (&harmonyMK20OffsetStore{tx: tx}).persistOffset(target, offset, guard)
		updated = attemptUpdated
		return commit && attemptUpdated, persistErr
	}, harmonydb.OptionRetry())
	return updated, err
}

func requireSignalBefore(t *testing.T, signal <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for coordinated transaction state")
	}
}

func requireIntResult(t *testing.T, result <-chan int) int {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(mk20OffsetITestTimeout):
		t.Fatal("timed out waiting for database backend ID")
		return 0
	}
}

func mk20OffsetITestBackendPID(tx *harmonydb.Tx) (int, error) {
	var rows []struct {
		PID int `db:"pid"`
	}
	if err := tx.Select(&rows, `SELECT pg_backend_pid() AS pid`); err != nil {
		return 0, err
	}
	if len(rows) != 1 {
		return 0, errors.New("expected one database backend ID")
	}
	return rows[0].PID, nil
}

func observeMK20OffsetITestLockWait(db *harmonydb.DB, pid int, statementReturned <-chan struct{}, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-statementReturned:
			return false, nil
		default:
		}

		var rows []struct {
			Waiting bool `db:"waiting"`
		}
		if err := db.Select(ctx, &rows, `SELECT EXISTS (
			SELECT 1 FROM pg_locks WHERE pid = $1 AND NOT granted
		) AS waiting`, pid); err != nil {
			return false, err
		}
		if len(rows) != 1 {
			return false, errors.New("expected one lock-observation row")
		}
		if rows[0].Waiting {
			return true, nil
		}

		select {
		case <-statementReturned:
			return false, nil
		case <-ticker.C:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func requireMK20OffsetITestLockObservation(t *testing.T, kind string, observed bool, err error, operation string) {
	t.Helper()
	if kind == "postgresql" {
		require.NoError(t, err)
		require.True(t, observed, "%s was not observed waiting in pg_locks", operation)
		return
	}
	if err != nil {
		t.Logf("%s lock observation unavailable on Yugabyte: %v", operation, err)
		return
	}
	t.Logf("%s pg_locks wait observed=%t; Yugabyte effective isolation remains UNVERIFIED", operation, observed)
}

func mk20OffsetITestUpdateProof(t *testing.T, target mk20OffsetTarget) int64 {
	t.Helper()
	proof, err := abi.RegisteredSealProof(target.RegSealProof).RegisteredUpdateProof()
	require.NoError(t, err)
	return int64(proof)
}

func mk20OffsetITestPieceCIDV2(t *testing.T, pieceCID string, paddedSize int64) (string, int64) {
	t.Helper()
	parsed, err := cid.Parse(pieceCID)
	require.NoError(t, err)
	rawSize := int64(abi.PaddedPieceSize(paddedSize).Unpadded())
	pieceCIDV2, err := commcid.PieceCidV2FromV1(parsed, uint64(rawSize))
	require.NoError(t, err)
	return pieceCIDV2.String(), rawSize
}

func mk20OffsetITestSealedCID(t *testing.T, marker byte) string {
	t.Helper()
	sealedCID, err := commcid.ReplicaCommitmentV1ToCID(bytes.Repeat([]byte{marker}, 32))
	require.NoError(t, err)
	return sealedCID.String()
}

func requireTxnResult(t *testing.T, result <-chan mk20OffsetITestTxnResult) mk20OffsetITestTxnResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(mk20OffsetITestTimeout):
		t.Fatal("timed out waiting for transaction result")
		return mk20OffsetITestTxnResult{}
	}
}

func requireErrorResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(mk20OffsetITestTimeout):
		t.Fatal("timed out waiting for transaction result")
		return errors.New("unreachable")
	}
}
