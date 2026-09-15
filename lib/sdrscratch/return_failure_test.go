package sdrscratch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type failingRecordIO struct {
	syncFailure bool
	state       string
}

func (f *failingRecordIO) SetXattr(fd int, key string, value []byte, flags int) error {
	var r record
	if err := json.Unmarshal(value, &r); err != nil {
		return err
	}
	f.state = r.State
	if r.State != "active" && !f.syncFailure {
		return unix.ENOSPC
	}
	return unix.Fsetxattr(fd, key, value, flags)
}

func (f *failingRecordIO) Sync(dir *os.File) error {
	if f.state != "active" && f.syncFailure {
		return unix.EIO
	}
	return dir.Sync()
}

func TestReturnRecordFailureOwnVersusRestart(t *testing.T) {
	for _, syncFailure := range []bool{false, true} {
		for _, immediate := range []bool{false, true} {
			t.Run(map[bool]string{false: "write", true: "sync"}[syncFailure]+"/"+map[bool]string{false: "restart", true: "own"}[immediate], func(t *testing.T) {
				base, p, w := fixture(t)
				w.recordIO = &failingRecordIO{syncFailure: syncFailure}
				wantErr := error(unix.ENOSPC)
				if syncFailure {
					wantErr = unix.EIO
				}
				require.ErrorIs(t, w.Returned(), wantErr)
				require.Equal(t, "active", w.r.State, "failed durability must not claim a persisted certificate")
				_, err := w.Reclaim()
				require.Error(t, err, "certificate-only path still refuses")
				if immediate {
					n, err := w.ReclaimOwn()
					require.Equal(t, 1, n, "directly observed return permits own file reclamation")
					require.ErrorIs(t, err, wantErr, "failed final diagnostics remain an error even after files were removed")
					require.NoFileExists(t, filepath.Join(p, "layer-1"))
				} else if syncFailure {
					// Emulate loss of an unsynced certificate at restart. A failed
					// Sync does not promise whether its preceding xattr survives.
					raw, err := json.Marshal(w.r)
					require.NoError(t, err)
					require.NoError(t, unix.Fsetxattr(int(w.dir.Fd()), attribute, raw, 0))
					require.NoError(t, w.dir.Sync())
				}
				require.NoError(t, w.Close())
				_, err = w.ReclaimOwn()
				require.Error(t, err, "closing the pinned FD revokes immediate authority")
				if !immediate {
					r, err := Sweep(base)
					require.NoError(t, err)
					require.Equal(t, "needs_review", r[0].Status)
					require.FileExists(t, filepath.Join(p, "layer-1"))
				}
			})
		}
	}
}

func TestImmediateReturnKeepsFileGuards(t *testing.T) {
	for _, kind := range []string{"completed", "symlink", "directory", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			base, p, w := fixture(t)
			w.recordIO = &failingRecordIO{}
			require.ErrorIs(t, w.Returned(), unix.ENOSPC)
			switch kind {
			case "completed":
				require.NoError(t, unix.Fsetxattr(int(w.dir.Fd()), completionAttribute, []byte(`{"Version":1}`), 0))
			case "symlink":
				require.NoError(t, os.Symlink(filepath.Join(p, "layer-1"), filepath.Join(p, "link")))
			case "directory":
				require.NoError(t, os.Mkdir(filepath.Join(p, "nested"), 0700))
			case "hardlink":
				require.NoError(t, os.Link(filepath.Join(p, "layer-1"), filepath.Join(base, "shared")))
			}
			n, err := w.ReclaimOwn()
			require.Error(t, err)
			require.Zero(t, n)
			require.FileExists(t, filepath.Join(p, "layer-1"))
		})
	}
}
