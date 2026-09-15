package paths

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCleanupCacheInvalidation(t *testing.T) {
	c := newCachedLocalStorage(nil)
	root := t.TempDir()
	child := filepath.Join(root, "cache", "file")
	other := root + "-different"
	stale := make(chan diskUsageResult, 1)
	for _, p := range []string{root, child, other} {
		c.stats.Add(p, statEntry{time: time.Now()})
		c.pathDUs.Add(p, &diskUsageEntry{last: diskUsageResult{usage: 42, time: time.Now()}, usagePromise: stale})
	}
	c.invalidate(root)
	for _, p := range []string{root, child} {
		require.False(t, c.stats.Contains(p))
		entry, ok := c.pathDUs.Peek(p)
		require.True(t, ok)
		require.Nil(t, entry.usagePromise, "pre-cleanup result cannot overwrite refreshed usage")
		require.True(t, entry.last.time.IsZero())
		require.Equal(t, int64(42), entry.last.usage, "slow measurement must not invent zero usage")
	}
	require.True(t, c.stats.Contains(other))
	entry, _ := c.pathDUs.Peek(other)
	require.False(t, entry.last.time.IsZero())
}
