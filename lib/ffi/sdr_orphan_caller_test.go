//go:build sdr_auto_itest && sdr_retry_itest

package ffi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

// Execute the actual cleanupSealed SQL, not a hand-written approximation. This
// is not execution of the other PipelineGC stages or chain/native operations.
const sealedGCFixtureSQL = `WITH unmatched_pieces AS (
 SELECT sip.sp_id, sip.sector_number
 FROM sectors_meta_pieces smp
 FULL JOIN sectors_sdr_initial_pieces sip ON smp.sp_id = sip.sp_id
 AND smp.sector_num = sip.sector_number AND smp.piece_num = sip.piece_index
 WHERE smp.sp_id IS NULL AND sip.sp_id IS NOT NULL
 ) DELETE FROM sectors_sdr_pipeline
 WHERE after_commit_msg_success = true AND after_move_storage = true
 AND EXISTS (SELECT 1 FROM sectors_meta
 WHERE sectors_meta.sp_id = sectors_sdr_pipeline.sp_id
 AND sectors_meta.sector_num = sectors_sdr_pipeline.sector_number)
 AND NOT EXISTS (SELECT 1 FROM unmatched_pieces up
 WHERE up.sp_id = sectors_sdr_pipeline.sp_id
 AND up.sector_number = sectors_sdr_pipeline.sector_number);`

func checkSealedGCQuery(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("../../tasks/gc/pipeline_meta_gc.go")
	require.NoError(t, err)
	_, body, ok := strings.Cut(string(b), "func (s *PipelineGC) cleanupSealed() error {")
	require.True(t, ok)
	_, body, ok = strings.Cut(body, "s.db.Exec(ctx, `")
	require.True(t, ok)
	query, _, ok := strings.Cut(body, "`)")
	require.True(t, ok)
	// HarmonyQuery deliberately accepts string constants only. Keep an exact
	// token comparison so this fixture cannot silently drift from the GC SQL.
	compact := func(s string) string { return strings.Join(strings.Fields(s), "") }
	require.Equal(t, compact(query), compact(sealedGCFixtureSQL))
}

func TestSDROrphanCallerDB(t *testing.T) {
	db := SDRRetryTestDB(t)
	for _, state := range []string{"absent", "normal-gc", "completed", "metadata", "db-error"} {
		t.Run(state, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := db.Exec(ctx, `DELETE FROM sectors_sdr_pipeline WHERE sp_id=1000 AND sector_number=42`)
			require.NoError(t, err)
			_, err = db.Exec(ctx, `DELETE FROM sectors_meta WHERE sp_id=1000 AND sector_num=42`)
			require.NoError(t, err)
			if state == "normal-gc" || state == "completed" {
				_, err = db.Exec(ctx, `INSERT INTO sectors_sdr_pipeline(sp_id,sector_number,reg_seal_proof,after_sdr,after_commit_msg_success,after_move_storage) VALUES(1000,42,5,true,true,true)`)
				require.NoError(t, err)
			}
			if state == "normal-gc" || state == "metadata" {
				_, err = db.Exec(ctx, `INSERT INTO sectors_meta(sp_id,sector_num,reg_seal_proof,ticket_epoch,ticket_value,orig_sealed_cid,orig_unsealed_cid,cur_sealed_cid,cur_unsealed_cid,seed_epoch,seed_value) VALUES(1000,42,5,0,'','fixture','fixture','fixture','fixture',0,'')`)
				require.NoError(t, err)
			}
			if state == "normal-gc" {
				checkSealedGCQuery(t)
				n, e := db.Exec(ctx, sealedGCFixtureSQL)
				require.NoError(t, e)
				require.EqualValues(t, 1, n, "real GC SQL must remove the completed pipeline row")
			}
			root := t.TempDir()
			id := storiface.ID(uuid.NewString())
			meta, err := json.Marshal(storiface.LocalStorageMeta{ID: id, Weight: 1, CanSeal: true})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(root, paths.MetaFile), meta, 0600))
			var private []string
			preserved := map[string][]byte{}
			for _, ft := range []string{"cache", "key"} {
				base := filepath.Join(root, ft)
				for _, relative := range []string{"s-t01000-42.tmp", "s-t01000-42.sdr.tmp/attempt-" + uuid.NewString(), "s-t01000-42.sdr.tmp/" + sdrscratch.Prefix + uuid.NewString()} {
					p := filepath.Join(base, relative)
					require.NoError(t, os.MkdirAll(p, 0700))
					require.NoError(t, os.WriteFile(filepath.Join(p, "native-work..tmp"), make([]byte, 4097), 0600))
					private = append(private, p)
				}
			}
			// Incomplete-looking canonical cache also survives absent DB state;
			// do not use the full-layer-layout guard to mask a DB protection bug.
			for _, relative := range []string{"cache/s-t01000-42/" + proofpaths.LayerFileName(1), "key/s-t01000-42", "sealed/s-t01000-42"} {
				p := filepath.Join(root, relative)
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
				preserved[p] = []byte("canonical/TreeRC/key/sealed: preserve")
				require.NoError(t, os.WriteFile(p, preserved[p], 0600))
			}
			if state == "db-error" {
				_, err = db.Exec(ctx, `ALTER TABLE sectors_sdr_pipeline RENAME TO fixture_unreadable_sdr_pipeline`)
				require.NoError(t, err)
				defer func() {
					_, e := db.Exec(context.Background(), `ALTER TABLE fixture_unreadable_sdr_pipeline RENAME TO sectors_sdr_pipeline`)
					require.NoError(t, e)
				}()
				// The key query has its own table; make that lookup fail as well.
				_, err = db.Exec(ctx, `ALTER TABLE sectors_unseal_pipeline RENAME TO fixture_unreadable_unseal_pipeline`)
				require.NoError(t, err)
				defer func() {
					_, e := db.Exec(context.Background(), `ALTER TABLE fixture_unreadable_unseal_pipeline RENAME TO sectors_unseal_pipeline`)
					require.NoError(t, e)
				}()
			}
			sdrscratch.AutoFixtureSetup(t, []string{root})
			ls := &autoCallerStorage{storiface.StorageConfig{StoragePaths: []storiface.LocalPath{{Path: root}}}}
			local, err := paths.NewLocal(ctx, ls, paths.NewDBIndex(nil, db), "")
			if state == "db-error" {
				// Startup cleanup and index declaration both fail closed when the
				// real schema query is unavailable. This is not a missing-row case.
				require.ErrorContains(t, err, "does not exist")
			} else {
				require.NoError(t, err)
				local.PrepareSDRScratch()
			}
			for p, want := range preserved {
				got, e := os.ReadFile(p)
				require.NoError(t, e)
				require.Equal(t, want, got)
			}
			for _, p := range private {
				if state == "db-error" {
					require.FileExists(t, filepath.Join(p, "native-work..tmp"))
				} else {
					require.NoDirExists(t, p, "verified unpublished directory must not require a live pre-SDR pipeline")
				}
			}
		})
	}
}
