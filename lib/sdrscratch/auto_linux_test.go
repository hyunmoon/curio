//go:build linux

package sdrscratch

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Run only as a disposable delegated systemd service. Unlike the portable
// caller overlay this uses the real cgroup, process census, xattr, filesystem,
// sector gate and /proc open-inode checks. Pipeline state is a fixture, not DB.
func TestAutoLinuxWorker(t *testing.T) {
	if os.Getenv("CURIO_SDR_DISCARD_TEST_TARGET") != "disposable" {
		t.Skip("dedicated Linux disposable service required")
	}
	root := os.Getenv("CURIO_AUTO_TEST_ROOT")
	state := os.Getenv("CURIO_AUTO_TEST_STATE")
	require.NotEmpty(t, root)
	require.NotEmpty(t, state)
	require.Equal(t, "1", os.Getenv(PersonalCleanupEnv))
	// Both locations must be pre-created by the disposable runner, never
	// inferred from a production profile. Scratch must be on another device.
	meta, e := json.Marshal(map[string]any{"ID": "auto-disposable", "CanSeal": true})
	require.NoError(t, e)
	_, e = os.Lstat(filepath.Join(root, "sectorstore.json"))
	require.True(t, os.IsNotExist(e), "fixture root must be new")
	require.NoError(t, os.WriteFile(filepath.Join(root, "sectorstore.json"), meta, 0600))
	require.NoError(t, os.Mkdir(filepath.Join(root, "cache"), 0700))
	s, e := startPersonalAt([]string{root}, state)
	require.NoError(t, e)
	personalSessionPtr.Store(s)
	defer personalSessionPtr.Store(nil)
	require.NoError(t, RegisterPersonalStorage(root, "auto-disposable"))
	// The worker was moved into its managed child. An empty parent PID list
	// must not prevent the real participant census from reaching that child.
	parent, e := openCgroup(s.unit.Cgroup)
	require.NoError(t, e)
	pids, e := readAt(parent, "cgroup.procs")
	require.NoError(t, e)
	require.Empty(t, strings.TrimSpace(string(pids)))
	live, e := populated(parent)
	require.NoError(t, e)
	require.True(t, live, "empty parent still has the live worker in its subtree")
	require.NoError(t, parent.Close())
	c, e := s.load(root)
	require.NoError(t, e)
	require.NoError(t, autoParticipants(c))
	// A different EOF source is still refused, with file/stage diagnostics.
	badCapability := filepath.Join(state, "auto-access-invalid-fixture.json")
	f, e := os.OpenFile(badCapability, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	require.NoError(t, e)
	require.NoError(t, f.Close())
	e = autoParticipants(c)
	require.ErrorIs(t, e, io.EOF)
	require.ErrorContains(t, e, "auto cleanup capability")
	require.ErrorContains(t, e, badCapability)
	require.FileExists(t, badCapability, "bad evidence must not be auto-deleted")
	require.NoError(t, os.Remove(badCapability)) // only this test-created file
	base := filepath.Join(root, "cache")
	p := autoWrite(t, base, "s-t01000-42.tmp", 1)
	keep := autoWrite(t, base, "s-t02000-42", 2)
	r, e := AutoDiscard(base, func(target AutoTarget, apply func(AutoStage) error) error {
		return apply(AutoStage{Allowed: target.Sector == "s-t01000-42", Reason: "disposable pipeline fixture", LayerNames: []string{"sc-02-data-layer-1.dat", "sc-02-data-layer-2.dat"}, LayerBytes: 2048})
	})
	require.NoError(t, e)
	require.Len(t, r, 2)
	require.NoDirExists(t, p)
	require.DirExists(t, keep)
}
