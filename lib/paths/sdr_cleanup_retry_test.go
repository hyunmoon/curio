package paths

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/lib/storiface"
)

func TestSDRCleanupRootOverlapAndCancellation(t *testing.T) {
	st := &Local{}
	root := t.TempDir()
	r := st.cleanupRoot(root)
	r.mu.Lock()
	var wg sync.WaitGroup
	// Both synchronous Claim preparation and periodic dispatch are bounded
	// non-blocking skips when this root already has a running pass.
	done := make(chan struct{})
	go func() {
		st.prepareSDRRoot(context.Background(), root, "fixture", false)
		st.startSDRRoot(context.Background(), root, "fixture", &wg)
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("same-root cleanup overlapped or blocked")
	}
	other := st.cleanupRoot(t.TempDir())
	require.True(t, other.mu.TryLock(), "one root cannot serialize another")
	other.mu.Unlock()
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st.prepareSDRRoot(ctx, root, "fixture", false)
	st.startSDRRoot(ctx, root, "fixture", &wg)
	wg.Wait()
	require.True(t, r.nextLog.IsZero(), "cancelled context must not start registration/cleanup")
	st.paths = map[storiface.ID]*path{"seal": {Local: root, CanSeal: true}, "store": {Local: "/not-a-sealing-root"}}
	require.Equal(t, map[string]storiface.ID{root: "seal"}, st.sdrRoots())
}
