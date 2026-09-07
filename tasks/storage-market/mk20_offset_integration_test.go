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
		firstDone := make(chan mk20OffsetITestTxnResult, 1)
		secondDone := make(chan mk20OffsetITestTxnResult, 1)
		holderPIDs := make(chan int, 8)
		recorder := newMK20OffsetITestContentionRecorder()
		firstCtx, cancelFirst := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
		secondCtx, cancelSecond := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
		var participants sync.WaitGroup
		release := func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) }
		registerMK20OffsetITestParticipantCleanup(t, &participants, release, cancelFirst, cancelSecond)

		participants.Add(1)
		go func() {
			defer participants.Done()
			committed, err := fixture.DB.BeginTransaction(firstCtx, func(tx *harmonydb.Tx) (bool, error) {
				pid, pidErr := mk20OffsetITestBackendPID(tx)
				if pidErr != nil {
					return false, pidErr
				}
				holderPIDs <- pid
				updated, resolveErr := resolveMK20PieceOffset(&harmonyMK20OffsetStore{tx: tx}, target)
				firstResolvedOnce.Do(func() { close(firstResolved) })
				select {
				case <-releaseFirst:
				case <-firstCtx.Done():
					return false, firstCtx.Err()
				}
				return updated, resolveErr
			}, harmonydb.OptionRetry())
			firstDone <- mk20OffsetITestTxnResult{Committed: committed, Err: err}
		}()

		requireSignalBefore(t, firstResolved, mk20OffsetITestTimeout)
		holderPID := requireIntResult(t, holderPIDs)
		participants.Add(1)
		go func() {
			defer participants.Done()
			var finalUpdated bool
			committed, err := peer.BeginTransaction(secondCtx, func(tx *harmonydb.Tx) (bool, error) {
				finalUpdated = false
				pid, pidErr := mk20OffsetITestBackendPID(tx)
				if pidErr != nil {
					return false, pidErr
				}
				attempt := recorder.beginAttempt(pid)
				store := &mk20OffsetITestLockProbeStore{
					harmonyMK20OffsetStore: harmonyMK20OffsetStore{tx: tx},
					attempt:                attempt,
					recorder:               recorder,
				}
				updated, resolveErr := resolveMK20PieceOffset(store, target)
				finalUpdated = updated
				return updated, resolveErr
			}, harmonydb.OptionRetry())
			result := mk20OffsetITestTxnResult{Committed: committed, Updated: finalUpdated, Err: err}
			recorder.recordTransaction(result)
			secondDone <- result
		}()

		attempt := requireMK20OffsetITestAttempt(t, recorder.attemptStarted)
		observation := observeMK20OffsetITestContention(fixture.DB, fixture.Target.Kind, attempt.PID, holderPID, recorder, 2*time.Second)
		beforeRelease := recorder.releaseHolder(release)

		first := requireTxnResult(t, firstDone)
		second := requireTxnResult(t, secondDone)
		cancelFirst()
		cancelSecond()
		require.True(t, waitMK20OffsetITestParticipants(&participants, mk20OffsetITestTimeout), "timed out joining contention participants")

		require.NoError(t, first.Err)
		require.True(t, first.Committed)
		if second.Err != nil {
			require.True(t, harmonydb.IsErrSerialization(second.Err), "unexpected second-attempt error: %v", second.Err)
		} else {
			require.False(t, second.Committed)
			require.False(t, second.Updated)
		}
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
		requireMK20OffsetITestContention(t, fixture.Target.Kind, "second MK20 target lock", observation, beforeRelease, recorder.snapshot())
	})

	t.Run("existing retained rows coordinate a content-changing metadata writer", func(t *testing.T) {
		fixture := newMK20OffsetITestDatabase(t)
		peer := newMK20OffsetITestPeer(t, fixture)
		target, layout, expected := retainedOffsetFixture(t, abi.RegisteredSealProof_StackedDrg2KiBV1_1)
		insertMK20OffsetITestTarget(t, fixture.DB, target, nil)
		insertMK20OffsetITestMetadata(t, fixture.DB, target, layout, layout.UnsealedCID)
		alternatePieces := append([]mk20RetainedPiece(nil), layout.Pieces...)
		alternatePieces[0].CID = syntheticPieceCID(t, 90)
		alternatePieces[0].Size = 256
		alternateLayout := retainedLayout(t, abi.RegisteredSealProof(target.RegSealProof), alternatePieces)
		alternateOffset, found := findMK20PieceOffset([]mk20SectorPiece{
			{CID: alternatePieces[0].CID, Size: abi.PaddedPieceSize(alternatePieces[0].Size), Index: alternatePieces[0].Index},
			{CID: alternatePieces[1].CID, Size: abi.PaddedPieceSize(alternatePieces[1].Size), Index: alternatePieces[1].Index},
			{CID: alternatePieces[2].CID, Size: abi.PaddedPieceSize(alternatePieces[2].Size), Index: alternatePieces[2].Index},
		}, target.PieceCID, abi.PaddedPieceSize(target.PieceSize))
		require.True(t, found)
		require.NotEqual(t, expected, alternateOffset, "the competing layout must change the target offset")

		resolved := make(chan struct{})
		var resolvedOnce sync.Once
		releaseResolver := make(chan struct{})
		var releaseResolverOnce sync.Once
		resolverDone := make(chan mk20OffsetITestTxnResult, 1)
		writerDone := make(chan mk20OffsetITestTxnResult, 1)
		holderPIDs := make(chan int, 8)
		recorder := newMK20OffsetITestContentionRecorder()
		resolverCtx, cancelResolver := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
		writerCtx, cancelWriter := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
		var participants sync.WaitGroup
		release := func() { releaseResolverOnce.Do(func() { close(releaseResolver) }) }
		registerMK20OffsetITestParticipantCleanup(t, &participants, release, cancelResolver, cancelWriter)

		participants.Add(1)
		go func() {
			defer participants.Done()
			committed, err := fixture.DB.BeginTransaction(resolverCtx, func(tx *harmonydb.Tx) (bool, error) {
				pid, pidErr := mk20OffsetITestBackendPID(tx)
				if pidErr != nil {
					return false, pidErr
				}
				holderPIDs <- pid
				updated, resolveErr := resolveMK20PieceOffset(&harmonyMK20OffsetStore{tx: tx}, target)
				resolvedOnce.Do(func() { close(resolved) })
				select {
				case <-releaseResolver:
				case <-resolverCtx.Done():
					return false, resolverCtx.Err()
				}
				return updated, resolveErr
			}, harmonydb.OptionRetry())
			resolverDone <- mk20OffsetITestTxnResult{Committed: committed, Err: err}
		}()

		requireSignalBefore(t, resolved, mk20OffsetITestTimeout)
		holderPID := requireIntResult(t, holderPIDs)
		participants.Add(1)
		go func() {
			defer participants.Done()
			committed, err := peer.BeginTransaction(writerCtx, func(tx *harmonydb.Tx) (bool, error) {
				pid, pidErr := mk20OffsetITestBackendPID(tx)
				if pidErr != nil {
					return false, pidErr
				}
				attempt := recorder.beginAttempt(pid)
				n, updateErr := tx.Exec(`UPDATE sectors_meta SET cur_unsealed_cid = $1 WHERE sp_id = $2 AND sector_num = $3`, alternateLayout.UnsealedCID, target.SPID, target.Sector)
				if updateErr == nil && n != 1 {
					updateErr = errors.New("metadata header writer affected an unexpected number of rows")
				}
				recorder.recordStatement(attempt, updateErr == nil, updateErr)
				if updateErr != nil {
					return false, updateErr
				}
				n, updateErr = tx.Exec(`UPDATE sectors_meta_pieces SET piece_cid = $1, piece_size = $2 WHERE sp_id = $3 AND sector_num = $4 AND piece_num = 0`, alternateLayout.Pieces[0].CID, alternateLayout.Pieces[0].Size, target.SPID, target.Sector)
				if updateErr == nil && n != 1 {
					updateErr = errors.New("metadata piece writer affected an unexpected number of rows")
				}
				recorder.recordStatement(attempt, updateErr == nil, updateErr)
				if updateErr != nil {
					return false, updateErr
				}
				return true, nil
			}, harmonydb.OptionRetry())
			result := mk20OffsetITestTxnResult{Committed: committed, Err: err}
			recorder.recordTransaction(result)
			writerDone <- result
		}()

		attempt := requireMK20OffsetITestAttempt(t, recorder.attemptStarted)
		observation := observeMK20OffsetITestContention(fixture.DB, fixture.Target.Kind, attempt.PID, holderPID, recorder, 2*time.Second)
		beforeRelease := recorder.releaseHolder(release)

		resolvedResult := requireTxnResult(t, resolverDone)
		writerResult := requireTxnResult(t, writerDone)
		cancelResolver()
		cancelWriter()
		require.True(t, waitMK20OffsetITestParticipants(&participants, mk20OffsetITestTimeout), "timed out joining contention participants")

		require.NoError(t, resolvedResult.Err)
		require.True(t, resolvedResult.Committed)
		if writerResult.Err != nil {
			require.True(t, harmonydb.IsErrSerialization(writerResult.Err), "unexpected metadata-writer error: %v", writerResult.Err)
			require.False(t, writerResult.Committed)
		} else {
			require.True(t, writerResult.Committed)
		}
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
		metadata := readMK20OffsetITestMetadataState(t, fixture.DB, target)
		if writerResult.Err == nil {
			require.Equal(t, mk20OffsetITestMetadataState{UnsealedCID: alternateLayout.UnsealedCID, FirstPieceCID: alternateLayout.Pieces[0].CID, FirstPieceSize: alternateLayout.Pieces[0].Size}, metadata)
		} else {
			require.Equal(t, mk20OffsetITestMetadataState{UnsealedCID: layout.UnsealedCID, FirstPieceCID: layout.Pieces[0].CID, FirstPieceSize: layout.Pieces[0].Size}, metadata)
		}
		requireMK20OffsetITestContention(t, fixture.Target.Kind, "retained metadata row lock", observation, beforeRelease, recorder.snapshot())
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
		resolverDone := make(chan mk20OffsetITestTxnResult, 1)
		writerDone := make(chan mk20OffsetITestTxnResult, 1)
		holderPIDs := make(chan int, 8)
		recorder := newMK20OffsetITestContentionRecorder()
		resolverCtx, cancelResolver := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
		writerCtx, cancelWriter := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
		var participants sync.WaitGroup
		release := func() { releaseResolverOnce.Do(func() { close(releaseResolver) }) }
		registerMK20OffsetITestParticipantCleanup(t, &participants, release, cancelResolver, cancelWriter)

		participants.Add(1)
		go func() {
			defer participants.Done()
			committed, err := fixture.DB.BeginTransaction(resolverCtx, func(tx *harmonydb.Tx) (bool, error) {
				pid, pidErr := mk20OffsetITestBackendPID(tx)
				if pidErr != nil {
					return false, pidErr
				}
				holderPIDs <- pid
				updated, resolveErr := resolveMK20PieceOffset(&harmonyMK20OffsetStore{tx: tx}, target)
				resolvedOnce.Do(func() { close(resolved) })
				select {
				case <-releaseResolver:
				case <-resolverCtx.Done():
					return false, resolverCtx.Err()
				}
				return updated, resolveErr
			}, harmonydb.OptionRetry())
			resolverDone <- mk20OffsetITestTxnResult{Committed: committed, Err: err}
		}()

		requireSignalBefore(t, resolved, mk20OffsetITestTimeout)
		holderPID := requireIntResult(t, holderPIDs)
		participants.Add(1)
		go func() {
			defer participants.Done()
			committed, err := peer.BeginTransaction(writerCtx, func(tx *harmonydb.Tx) (bool, error) {
				pid, pidErr := mk20OffsetITestBackendPID(tx)
				if pidErr != nil {
					return false, pidErr
				}
				attempt := recorder.beginAttempt(pid)
				n, insertErr := tx.Exec(`INSERT INTO sectors_snap_pipeline (sp_id, sector_number, upgrade_proof) VALUES ($1, $2, $3)`, target.SPID, target.Sector, updateProof)
				if insertErr == nil && n != 1 {
					insertErr = errors.New("Snap pipeline writer affected an unexpected number of rows")
				}
				recorder.recordStatement(attempt, insertErr == nil, insertErr)
				if insertErr != nil {
					return false, insertErr
				}
				return true, nil
			}, harmonydb.OptionRetry())
			result := mk20OffsetITestTxnResult{Committed: committed, Err: err}
			recorder.recordTransaction(result)
			writerDone <- result
		}()

		attempt := requireMK20OffsetITestAttempt(t, recorder.attemptStarted)
		observation := observeMK20OffsetITestContention(fixture.DB, fixture.Target.Kind, attempt.PID, holderPID, recorder, 2*time.Second)
		beforeRelease := recorder.releaseHolder(release)

		resolvedResult := requireTxnResult(t, resolverDone)
		writerResult := requireTxnResult(t, writerDone)
		cancelResolver()
		cancelWriter()
		require.True(t, waitMK20OffsetITestParticipants(&participants, mk20OffsetITestTimeout), "timed out joining contention participants")

		require.NoError(t, resolvedResult.Err)
		require.True(t, resolvedResult.Committed)
		if writerResult.Err != nil {
			require.True(t, harmonydb.IsErrSerialization(writerResult.Err), "unexpected Snap-writer error: %v", writerResult.Err)
			require.False(t, writerResult.Committed)
			require.Zero(t, countMK20OffsetITestSnapRows(t, fixture.DB, target))
		} else {
			require.True(t, writerResult.Committed)
			require.Equal(t, 1, countMK20OffsetITestSnapRows(t, fixture.DB, target))
		}
		require.Equal(t, sql.NullInt64{Int64: int64(expected), Valid: true}, readMK20OffsetITestOffset(t, fixture.DB, target))
		requireMK20OffsetITestContention(t, fixture.Target.Kind, "Snap metadata foreign-key lock", observation, beforeRelease, recorder.snapshot())
	})
}

type mk20OffsetITestTxnResult struct {
	Committed bool
	Updated   bool
	Err       error
}

type mk20OffsetITestAttempt struct {
	Number int
	PID    int
}

type mk20OffsetITestAttemptEvidence struct {
	Number                      int
	PID                         int
	StatementReturned           bool
	StatementSucceeded          bool
	RecognizedConflict          bool
	ConflictAtRetryBoundary     bool
	ConflictAtTransactionReturn bool
}

type mk20OffsetITestContentionSnapshot struct {
	Attempts                         []mk20OffsetITestAttemptEvidence
	HolderReleased                   bool
	TransactionDone                  bool
	TransactionFinishedBeforeRelease bool
	TransactionFinalAttempt          int
	TransactionResult                mk20OffsetITestTxnResult
}

func (s mk20OffsetITestContentionSnapshot) recognizedConflict() bool {
	for _, attempt := range s.Attempts {
		if attempt.RecognizedConflict {
			return true
		}
	}
	return false
}

func (s mk20OffsetITestContentionSnapshot) statementReturned() bool {
	for _, attempt := range s.Attempts {
		if attempt.StatementReturned {
			return true
		}
	}
	return false
}

func (s mk20OffsetITestContentionSnapshot) statementSucceeded() bool {
	for _, attempt := range s.Attempts {
		if attempt.StatementSucceeded {
			return true
		}
	}
	return false
}

type mk20OffsetITestContentionRecorder struct {
	mu sync.Mutex

	attempts                         []mk20OffsetITestAttemptEvidence
	holderReleased                   bool
	transactionDone                  bool
	transactionFinishedBeforeRelease bool
	transactionFinalAttempt          int
	transactionResult                mk20OffsetITestTxnResult

	attemptStarted chan mk20OffsetITestAttempt
	changed        chan struct{}
}

func newMK20OffsetITestContentionRecorder() *mk20OffsetITestContentionRecorder {
	return &mk20OffsetITestContentionRecorder{
		// HarmonyDB currently makes at most seven OptionRetry attempts. Keep
		// every attempt observable without allowing the transaction callback to
		// block on test coordination.
		attemptStarted: make(chan mk20OffsetITestAttempt, 16),
		changed:        make(chan struct{}, 1),
	}
}

func (r *mk20OffsetITestContentionRecorder) beginAttempt(pid int) int {
	r.mu.Lock()
	if len(r.attempts) > 0 {
		previous := &r.attempts[len(r.attempts)-1]
		if !previous.RecognizedConflict {
			// HarmonyDB's OptionRetry starts another callback only after a
			// recognized SQLSTATE 40001. The resolver probe does not wrap every
			// later statement, so record the retry boundary without guessing
			// whether the conflict came from an unobserved statement or commit.
			previous.RecognizedConflict = true
			previous.ConflictAtRetryBoundary = true
		}
	}
	attempt := len(r.attempts) + 1
	r.attempts = append(r.attempts, mk20OffsetITestAttemptEvidence{Number: attempt, PID: pid})
	r.mu.Unlock()

	r.attemptStarted <- mk20OffsetITestAttempt{Number: attempt, PID: pid}
	r.signalChange()
	return attempt
}

func (r *mk20OffsetITestContentionRecorder) recordStatement(attempt int, succeeded bool, err error) {
	r.mu.Lock()
	current := &r.attempts[attempt-1]
	current.StatementReturned = true
	current.StatementSucceeded = current.StatementSucceeded || succeeded
	if harmonydb.IsErrSerialization(err) {
		current.RecognizedConflict = true
	}
	r.mu.Unlock()
	r.signalChange()
}

func (r *mk20OffsetITestContentionRecorder) recordTransaction(result mk20OffsetITestTxnResult) {
	r.mu.Lock()
	if harmonydb.IsErrSerialization(result.Err) && len(r.attempts) > 0 {
		last := &r.attempts[len(r.attempts)-1]
		if !last.RecognizedConflict {
			last.RecognizedConflict = true
			last.ConflictAtTransactionReturn = true
		}
	}
	r.transactionDone = true
	r.transactionFinishedBeforeRelease = !r.holderReleased
	r.transactionFinalAttempt = len(r.attempts)
	r.transactionResult = result
	r.mu.Unlock()
	r.signalChange()
}

func (r *mk20OffsetITestContentionRecorder) snapshot() mk20OffsetITestContentionSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *mk20OffsetITestContentionRecorder) releaseHolder(release func()) mk20OffsetITestContentionSnapshot {
	r.mu.Lock()
	// Keep the pre-release evidence, release marker, and channel close on one
	// synchronization boundary with recordTransaction.
	snapshot := r.snapshotLocked()
	r.holderReleased = true
	release()
	r.mu.Unlock()
	return snapshot
}

func (r *mk20OffsetITestContentionRecorder) snapshotLocked() mk20OffsetITestContentionSnapshot {
	attempts := append([]mk20OffsetITestAttemptEvidence(nil), r.attempts...)
	return mk20OffsetITestContentionSnapshot{
		Attempts:                         attempts,
		HolderReleased:                   r.holderReleased,
		TransactionDone:                  r.transactionDone,
		TransactionFinishedBeforeRelease: r.transactionFinishedBeforeRelease,
		TransactionFinalAttempt:          r.transactionFinalAttempt,
		TransactionResult:                r.transactionResult,
	}
}

func (r *mk20OffsetITestContentionRecorder) signalChange() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

type mk20OffsetITestLockProbeStore struct {
	harmonyMK20OffsetStore
	attempt  int
	recorder *mk20OffsetITestContentionRecorder
}

func (s *mk20OffsetITestLockProbeStore) lockTarget(target mk20OffsetTarget) (bool, error) {
	locked, err := s.harmonyMK20OffsetStore.lockTarget(target)
	s.recorder.recordStatement(s.attempt, err == nil, err)
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
	UnsealedCID    string `db:"cur_unsealed_cid"`
	FirstPieceCID  string `db:"piece_cid"`
	FirstPieceSize int64  `db:"piece_size"`
}

func readMK20OffsetITestMetadataState(t *testing.T, db *harmonydb.DB, target mk20OffsetTarget) mk20OffsetITestMetadataState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mk20OffsetITestTimeout)
	defer cancel()
	var rows []mk20OffsetITestMetadataState
	require.NoError(t, db.Select(ctx, &rows, `SELECT sm.cur_unsealed_cid, smp.piece_cid, smp.piece_size
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

func requireMK20OffsetITestAttempt(t *testing.T, attempts <-chan mk20OffsetITestAttempt) mk20OffsetITestAttempt {
	t.Helper()
	select {
	case attempt := <-attempts:
		return attempt
	case <-time.After(mk20OffsetITestTimeout):
		t.Fatal("timed out waiting for a contention transaction attempt")
		return mk20OffsetITestAttempt{}
	}
}

func registerMK20OffsetITestParticipantCleanup(t *testing.T, participants *sync.WaitGroup, release func(), cancels ...context.CancelFunc) {
	t.Helper()
	t.Cleanup(func() {
		release()
		for _, cancel := range cancels {
			cancel()
		}
		if !waitMK20OffsetITestParticipants(participants, mk20OffsetITestTimeout) {
			t.Errorf("timed out joining test-owned contention participants during cleanup")
		}
	})
}

func waitMK20OffsetITestParticipants(participants *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		participants.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
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

type mk20OffsetITestLockObservation struct {
	Observed bool
	Err      error
}

func observeMK20OffsetITestContention(db *harmonydb.DB, kind string, waiterPID, holderPID int, recorder *mk20OffsetITestContentionRecorder, timeout time.Duration) mk20OffsetITestLockObservation {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	canObserveLinkedWait := kind == "postgresql"
	var observationErr error
	if !canObserveLinkedWait {
		observationErr = errors.New("linked lock observation is not established for this Yugabyte version; a recognized serialization conflict is required")
	}

	for {
		snapshot := recorder.snapshot()
		if snapshot.recognizedConflict() || snapshot.TransactionDone {
			return mk20OffsetITestLockObservation{Err: observationErr}
		}

		if canObserveLinkedWait {
			var rows []struct {
				Waiting bool `db:"waiting"`
			}
			if err := db.Select(ctx, &rows, `SELECT $2 = ANY(pg_blocking_pids($1)) AS waiting`, waiterPID, holderPID); err != nil {
				observationErr = err
				canObserveLinkedWait = false
			} else if len(rows) != 1 {
				observationErr = errors.New("expected one linked lock-observation row")
				canObserveLinkedWait = false
			} else if rows[0].Waiting {
				return mk20OffsetITestLockObservation{Observed: true}
			}
		}

		select {
		case <-recorder.changed:
		case <-ticker.C:
		case <-ctx.Done():
			if observationErr == nil {
				observationErr = errors.New("timed out without observing the expected holder as blocker")
			}
			return mk20OffsetITestLockObservation{Err: observationErr}
		}
	}
}

func requireMK20OffsetITestContention(t *testing.T, kind, operation string, observation mk20OffsetITestLockObservation, beforeRelease, final mk20OffsetITestContentionSnapshot) {
	t.Helper()
	evidence := mk20OffsetContentionEvidence{
		WaitObserved:                      observation.Observed,
		ObservationFailed:                 observation.Err != nil,
		RecognizedConflict:                final.recognizedConflict(),
		StatementReturnedBeforeRelease:    beforeRelease.statementReturned(),
		StatementSucceededBeforeRelease:   beforeRelease.statementSucceeded(),
		TransactionFinished:               final.TransactionDone,
		TransactionCommitted:              final.TransactionDone && final.TransactionResult.Err == nil && final.TransactionResult.Committed,
		TransactionFinishedBeforeRelease:  final.TransactionFinishedBeforeRelease,
		TransactionCommittedBeforeRelease: final.TransactionFinishedBeforeRelease && final.TransactionResult.Err == nil && final.TransactionResult.Committed,
	}

	verdict := classifyMK20OffsetContention(evidence)
	detail := "kind=%s attempts=%+v final_attempt=%d wait_observed=%t observation_error=%v statement_returned_before_release=%t statement_succeeded_before_release=%t transaction_finished=%t transaction_committed=%t transaction_finished_before_release=%t transaction_committed_before_release=%t"
	switch verdict {
	case mk20OffsetContentionObservedBlocking, mk20OffsetContentionRecognizedConflict:
		t.Logf("%s contention evidence=%s: "+detail, operation, verdict, kind, final.Attempts, final.TransactionFinalAttempt, observation.Observed, observation.Err, evidence.StatementReturnedBeforeRelease, evidence.StatementSucceededBeforeRelease, evidence.TransactionFinished, evidence.TransactionCommitted, evidence.TransactionFinishedBeforeRelease, evidence.TransactionCommittedBeforeRelease)
	case mk20OffsetContentionOrderingViolation:
		require.Failf(t, "contention ordering violation", "%s completed successfully before the held resolver was released: "+detail, operation, kind, final.Attempts, final.TransactionFinalAttempt, observation.Observed, observation.Err, evidence.StatementReturnedBeforeRelease, evidence.StatementSucceededBeforeRelease, evidence.TransactionFinished, evidence.TransactionCommitted, evidence.TransactionFinishedBeforeRelease, evidence.TransactionCommittedBeforeRelease)
	case mk20OffsetContentionInconclusive:
		require.Failf(t, "contention not established", "%s produced neither a linked database wait nor a recognized serialization conflict: "+detail, operation, kind, final.Attempts, final.TransactionFinalAttempt, observation.Observed, observation.Err, evidence.StatementReturnedBeforeRelease, evidence.StatementSucceededBeforeRelease, evidence.TransactionFinished, evidence.TransactionCommitted, evidence.TransactionFinishedBeforeRelease, evidence.TransactionCommittedBeforeRelease)
	}
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
