//go:build linux

package sdrscratch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Regular temporary files exercise the actual openat/read helper without
// cgroup privileges. They are not a delegated-cgroup integration test.
func TestReadAtRecord(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		oversized     bool
	}{
		{"empty", "", false},
		{"pids", "123\n456\n", false},
		{"limit", strings.Repeat("1", 4096), false},
		{"limit_plus_one", strings.Repeat("1", 4097), true},
		{"large", strings.Repeat("1", 8192), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(path, "cgroup.procs"), []byte(tc.content), 0600))
			d, err := os.Open(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, d.Close()) }()
			b, err := readAt(d, "cgroup.procs")
			if tc.oversized {
				require.ErrorContains(t, err, "oversized kernel record")
				require.Nil(t, b)
				return
			}
			require.NoError(t, err, "empty direct PID list is a valid read, not a cleanup authorization")
			require.Equal(t, tc.content, string(b))
		})
	}
}

func TestReadAtErrorsRemainErrors(t *testing.T) {
	path := t.TempDir()
	d, err := os.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, d.Close()) }()
	_, err = readAt(d, "missing")
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorContains(t, err, filepath.Join(path, "missing"))
	require.NoError(t, os.WriteFile(filepath.Join(path, "target"), nil, 0600))
	require.NoError(t, os.Symlink("target", filepath.Join(path, "symlink")))
	_, err = readAt(d, "symlink")
	require.ErrorIs(t, err, unix.ELOOP)
	require.ErrorContains(t, err, filepath.Join(path, "symlink"))
	require.NoError(t, os.Mkdir(filepath.Join(path, "directory"), 0700))
	_, err = readAt(d, "directory")
	require.ErrorIs(t, err, unix.EISDIR)
	require.ErrorContains(t, err, filepath.Join(path, "directory"))
}

func TestPopulatedRequiresEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		live, invalid bool
	}{
		{"empty", "", false, true},
		{"missing", "frozen 0\n", false, true},
		{"malformed", "populated nope\n", false, true},
		{"zero", "populated 0\nfrozen 0\n", false, false},
		{"one", "populated 1\nfrozen 0\n", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(path, "cgroup.events"), []byte(tc.content), 0600))
			d, err := os.Open(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, d.Close()) }()
			live, err := populated(d)
			if tc.invalid {
				require.ErrorContains(t, err, "cgroup.events")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.live, live)
		})
	}
}
