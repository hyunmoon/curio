//go:build sdr_retry_itest

package ffi

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v2"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/harmony/harmonytask"
	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/storiface"
)

// These tests require the documented Go overlay, which replaces only the
// synchronous native function argument. Without it, fail before calling Do.
var SDRRetryAdapterActive = false
var retryNatives sync.Map

func SDRRetryTestNative(sb *SealCalls) func(abi.RegisteredSealProof, string, [32]byte) error {
	v, ok := retryNatives.Load(sb)
	if !ok {
		panic("missing isolated SDR fixture")
	}
	return v.(func(abi.RegisteredSealProof, string, [32]byte) error)
}

type retryTestIndex struct{ paths.SectorIndex }

func (*retryTestIndex) StorageDeclareSector(context.Context, storiface.ID, abi.SectorID, storiface.SectorFileType, bool) error {
	return nil
}
func (*retryTestIndex) StorageFindSector(context.Context, abi.SectorID, storiface.SectorFileType, abi.SectorSize, bool) ([]storiface.SectorStorageInfo, error) {
	return nil, nil
}

type retryTestStore struct{ paths.Store }

func (*retryTestStore) Remove(context.Context, abi.SectorID, storiface.SectorFileType, bool, []storiface.ID) error {
	return nil
}

func SDRRetryTestCalls(t *testing.T, ft storiface.SectorFileType, s storiface.SectorRef, id harmonytask.TaskID, dest string) (*SealCalls, *atomic.Int32) {
	t.Helper()
	require.True(t, SDRRetryAdapterActive, "use the native-substitution Go overlay; never execute real proofs in this fixture")
	index := &retryTestIndex{}
	remote, err := paths.NewRemote(&retryTestStore{}, index, nil, 1, nil)
	require.NoError(t, err)
	sb := &SealCalls{Sectors: &storageProvider{storage: remote, sindex: index, storageReservations: xsync.NewIntegerMapOf[harmonytask.TaskID, []*StorageReservation]()}}
	pp, ids := storiface.SectorPaths{}, storiface.SectorPaths{}
	storiface.SetPathByType(&pp, ft, dest)
	storiface.SetPathByType(&ids, ft, "fixture")
	r, err := prepareSDRReservation(dest, s, ft)
	require.NoError(t, err)
	sb.Sectors.storageReservations.Store(id, []*StorageReservation{{SectorRef: SectorRef{SpID: int64(s.ID.Miner), SectorNumber: int64(s.ID.Number), RegSealProof: s.ProofType}, Alloc: ft, Paths: pp, PathIDs: ids, SDR: r, Release: func() {}}})
	calls := new(atomic.Int32)
	retryNatives.Store(sb, func(p abi.RegisteredSealProof, dir string, _ [32]byte) error {
		calls.Add(1)
		n, err := proofpaths.SDRLayers(p)
		if err != nil {
			return err
		}
		for i := 1; i <= n; i++ {
			if err := os.WriteFile(filepath.Join(dir, proofpaths.LayerFileName(i)), make([]byte, 8192), 0600); err != nil {
				return err
			}
		}
		return nil
	})
	t.Cleanup(func() { retryNatives.Delete(sb) })
	return sb, calls
}

func SDRRetryTestDB(t *testing.T) *harmonydb.DB {
	t.Helper()
	if os.Getenv("CURIO_SDR_RETRY_ITEST") != "1" {
		t.Skip("dedicated disposable DB opt-in not enabled")
	}
	host, port := os.Getenv("CURIO_SDR_RETRY_ITEST_HOST"), os.Getenv("CURIO_SDR_RETRY_ITEST_PORT")
	require.Equal(t, "127.0.0.1", host)
	p, err := strconv.Atoi(port)
	require.NoError(t, err)
	require.Greater(t, p, 0)
	require.Less(t, p, 65536)
	require.Equal(t, "curio_test_sdr_retry", os.Getenv("CURIO_SDR_RETRY_ITEST_DATABASE"))
	require.Equal(t, "curio_sdr_retry", os.Getenv("CURIO_SDR_RETRY_ITEST_USER"))
	id := harmonydb.ITestNewID()
	db, err := harmonydb.NewFromConfig(harmonydb.Config{Hosts: []string{host}, Port: port, Database: "curio_test_sdr_retry", Username: "curio_sdr_retry", LoadBalance: false, UseTemplate: false, ITestID: id, ApplicationName: "sdr-retry-owned-fixture"})
	require.NoError(t, err)
	t.Cleanup(db.ITestDeleteAll)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var schema, addr, version string
	require.NoError(t, db.QueryRow(ctx, `SELECT current_schema(),host(inet_server_addr()),version()`).Scan(&schema, &addr, &version))
	require.Equal(t, "itest_"+string(id), schema)
	require.Equal(t, "127.0.0.1", addr)
	t.Logf("actual migration runner; schema=%s server=%s", schema, version)
	return db
}
