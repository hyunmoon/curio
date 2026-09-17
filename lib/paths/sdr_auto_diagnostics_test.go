//go:build sdr_auto_itest

package paths

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

func TestSDRAutoDiagnosticsAndPrivateCache(t *testing.T) {
	if !sdrscratch.AutoFixtureActive {
		t.Skip("explicit OS-evidence overlay required")
	}
	root := t.TempDir()
	id := storiface.ID(uuid.NewString())
	meta, err := json.Marshal(storiface.LocalStorageMeta{ID: id, CanSeal: true})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, MetaFile), meta, 0600))
	canonical := filepath.Join(root, "cache", "s-t01000-42")
	private := canonical + ".tmp"
	for _, p := range []string{canonical, private} {
		require.NoError(t, os.MkdirAll(p, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(p, "sc-02-data-layer-1.dat"), []byte("keep until authorized"), 0600))
	}
	sdrscratch.AutoFixtureSetup(t, []string{root})
	mode := &retryStateMode{}
	mode.mode.Store(2)
	local := &Local{index: &retryStateIndex{roots: map[string]*retryStateMode{root: mode}}}
	r := &sdrCleanupRoot{protectedUntil: map[string]time.Time{
		canonical: time.Now().Add(time.Minute),
		private:   time.Now().Add(time.Minute), // old policy's denial cannot mask new private policy
	}}
	core, logs := observer.New(zap.InfoLevel)
	oldLog := log
	log = &logging.ZapEventLogger{SugaredLogger: *zap.New(core).Sugar()}
	defer func() { log = oldLog }()
	// No NewLocal/timer goroutines: these serialized passes exercise the actual
	// logger call and use cleanSDRRoot's existing verbose cadence unchanged.
	require.True(t, local.autoDiscardSDR(context.Background(), root, r, true))
	require.NoDirExists(t, private)
	require.FileExists(t, filepath.Join(canonical, "sc-02-data-layer-1.dat"))
	require.Equal(t, int32(1), mode.calls.Load(), "only private bypasses the cached denial")
	protected := logs.FilterMessage("SDR automatic adoption").FilterField(zap.String("status", "protected"))
	require.Equal(t, 1, protected.Len(), "protected reason must be visible at startup")
	require.NotEmpty(t, protected.All()[0].ContextMap()["reason"])
	logs.TakeAll()
	require.False(t, local.autoDiscardSDR(context.Background(), root, r, false))
	require.Zero(t, logs.Len(), "ordinary 30-second retry must not repeat protected-file diagnostics")
	require.False(t, local.autoDiscardSDR(context.Background(), root, r, true))
	require.Equal(t, 1, logs.Len(), "existing limited log interval must explain all-protected passes")
}
