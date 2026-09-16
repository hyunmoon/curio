package sdrscratch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/proofpaths"
)

// Sparse files test the actual proof's size/layout predicate, not native SDR
// computation or physical reclamation of a full-size native cache.
func TestAutoDiscardProofLayout32GiB(t *testing.T) {
	proof := abi.RegisteredSealProof_StackedDrg32GiBV1_1
	layers, e := proofpaths.SDRLayers(proof)
	require.NoError(t, e)
	require.Equal(t, 11, layers)
	size, e := proof.SectorSize()
	require.NoError(t, e)
	for _, canonical := range []bool{false, true} {
		t.Run(fmt.Sprint(canonical), func(t *testing.T) {
			c, base, io, _ := autoFixture(t)
			relative := "s-t01000-42.tmp"
			if canonical {
				relative = "s-t01000-42"
			}
			p := autoWrite(t, base, relative, 0)
			stage := AutoStage{Allowed: true, LayerBytes: int64(size)}
			for i := 1; i <= layers; i++ {
				name := proofpaths.LayerFileName(i)
				stage.LayerNames = append(stage.LayerNames, name)
				f, err := os.Create(filepath.Join(p, name))
				require.NoError(t, err)
				require.NoError(t, f.Truncate(int64(size)))
				require.NoError(t, f.Close())
			}
			for _, name := range []string{"sc-02-data-layer-9..tmp", "sc-02-data-layer-11..tmp", "backend-state"} {
				require.NoError(t, os.WriteFile(filepath.Join(p, name), []byte("interrupted"), 0600))
			}
			r, err := autoDiscard(c, base, func(_ AutoTarget, apply func(AutoStage) error) error { return apply(stage) }, io)
			require.NoError(t, err)
			if canonical {
				require.Zero(t, r[0].FilesRemoved)
				entries, err := os.ReadDir(p)
				require.NoError(t, err)
				require.Len(t, entries, 14)
			} else {
				require.Equal(t, "reclaimed", r[0].Status, r[0].Reason)
				require.Equal(t, 14, r[0].FilesRemoved)
				require.NoDirExists(t, p)
			}
		})
	}
}

func TestAutoDiscardPrivateWholeFolder(t *testing.T) {
	for _, relative := range []string{"s-t01000-42.tmp", "s-t01000-42.sdr.tmp/attempt-" + uuid.NewString(), "s-t01000-42.sdr.tmp/" + Prefix + uuid.NewString()} {
		for _, temporarySize := range []int{0, 19, 2048} {
			t.Run(fmt.Sprintf("%s/%d", relative, temporarySize), func(t *testing.T) {
				c, base, io, state := autoFixture(t)
				p := autoWrite(t, base, relative, 1)
				for name, size := range map[string]int{
					"sc-02-data-layer-9..tmp":  temporarySize,
					"sc-02-data-layer-11..tmp": temporarySize,
					"sc-02-data-layer-999.dat": 17,
					"backend-work-in-progress": 31,
					"native-metadata.json":     23,
				} {
					require.NoError(t, os.WriteFile(filepath.Join(p, name), make([]byte, size), 0600))
				}
				d, e := openDir(p)
				require.NoError(t, e)
				entries, e := d.ReadDir(-1)
				require.NoError(t, e)
				var allocated uint64
				for _, entry := range entries {
					st, e := privateFile(d, entry.Name())
					require.NoError(t, e)
					allocated += uint64(st.Blocks) * 512
				}
				require.NoError(t, d.Close())
				r, e := autoDiscard(c, base, state, io)
				require.NoError(t, e)
				require.Len(t, r, 1)
				require.Equal(t, "reclaimed", r[0].Status, r[0].Reason)
				require.Equal(t, 6, r[0].FilesRemoved)
				require.Equal(t, allocated, r[0].AllocatedBytes)
				require.NoDirExists(t, p)
			})
		}
	}
}

func TestAutoDiscardPrivateTmpOnlyAndUnpublished(t *testing.T) {
	for _, mode := range []string{"tmp-only", "full-private", "unpublished-receipt"} {
		t.Run(mode, func(t *testing.T) {
			c, base, io, state := autoFixture(t)
			n := 0
			if mode != "tmp-only" {
				n = 2
			}
			p := autoWrite(t, base, "s-t01000-42.tmp", n)
			require.NoError(t, os.WriteFile(filepath.Join(p, "sc-02-data-layer-11..tmp"), []byte("partial"), 0600))
			if mode == "unpublished-receipt" {
				require.NoError(t, unix.Setxattr(p, completionAttribute, []byte(`{"Version":1}`), 0))
			}
			r, e := autoDiscard(c, base, state, io)
			require.NoError(t, e)
			require.Equal(t, "reclaimed", r[0].Status, r[0].Reason)
			require.Equal(t, n+1, r[0].FilesRemoved)
			require.NoDirExists(t, p, "unpublished staging is discarded, not reused")
		})
	}
}

func TestAutoDiscardCanonicalNotPrivate(t *testing.T) {
	for _, mode := range []string{"complete", "complete-with-tmp", "complete-with-unknown", "partial-with-tmp", "receipt"} {
		t.Run(mode, func(t *testing.T) {
			c, base, io, state := autoFixture(t)
			n := 2
			if mode == "partial-with-tmp" || mode == "receipt" {
				n = 1
			}
			p := autoWrite(t, base, "s-t01000-42", n)
			switch mode {
			case "complete-with-tmp", "partial-with-tmp":
				require.NoError(t, os.WriteFile(filepath.Join(p, "sc-02-data-layer-11..tmp"), []byte("partial"), 0600))
			case "complete-with-unknown":
				require.NoError(t, os.WriteFile(filepath.Join(p, "tree-rc-input"), []byte("keep"), 0600))
			case "receipt":
				require.NoError(t, unix.Setxattr(p, completionAttribute, []byte(`{"Version":1}`), 0))
			}
			before, e := os.ReadDir(p)
			require.NoError(t, e)
			r, e := autoDiscard(c, base, state, io)
			require.NoError(t, e)
			require.Zero(t, r[0].FilesRemoved, r[0].Reason)
			require.NotEqual(t, "reclaimed", r[0].Status)
			after, e := os.ReadDir(p)
			require.NoError(t, e)
			require.Equal(t, len(before), len(after))
			require.FileExists(t, filepath.Join(p, "sc-02-data-layer-1.dat"))
		})
	}
}

func TestAutoDiscardPrivateGuards(t *testing.T) {
	for _, mode := range []string{"symlink", "hardlink", "directory", "oversize", "unconverted", "db-error", "completed-stage", "gate", "unknown-owner"} {
		t.Run(mode, func(t *testing.T) {
			c, base, io, state := autoFixture(t)
			p := autoWrite(t, base, "s-t01000-42.tmp", 1)
			name := filepath.Join(p, "backend-work-in-progress")
			switch mode {
			case "symlink":
				require.NoError(t, os.Symlink("sc-02-data-layer-1.dat", name))
			case "hardlink":
				require.NoError(t, os.Link(filepath.Join(p, "sc-02-data-layer-1.dat"), name))
			case "directory":
				require.NoError(t, os.Mkdir(name, 0700))
			case "oversize":
				require.NoError(t, os.WriteFile(name, make([]byte, 2049), 0600))
			default:
				require.NoError(t, os.WriteFile(name, []byte("private"), 0600))
				switch mode {
				case "unconverted":
					io.participants = func(*ManagedConfig) error { return errors.New("live foreign run") }
				case "db-error":
					state = func(AutoTarget, func(AutoStage) error) error { return errors.New("unavailable") }
				case "completed-stage":
					state = func(_ AutoTarget, fn func(AutoStage) error) error { return fn(AutoStage{Reason: "TreeRC input"}) }
				case "gate":
					f, e := sectorGate(c, "s-t01000-42", false, openDir)
					require.NoError(t, e)
					defer func() { require.NoError(t, f.Close()) }()
				case "unknown-owner":
					require.NoError(t, unix.Setxattr(p, attribute, []byte("invalid"), 0))
				}
			}
			r, e := autoDiscard(c, base, state, io)
			require.NoError(t, e)
			require.Zero(t, r[0].FilesRemoved)
			require.FileExists(t, filepath.Join(p, "sc-02-data-layer-1.dat"))
		})
	}
}

func TestAutoDiscardPrivatePartialRetry(t *testing.T) {
	c, base, io, state := autoFixture(t)
	p := autoWrite(t, base, "s-t01000-42.tmp", 1)
	require.NoError(t, os.WriteFile(filepath.Join(p, "sc-02-data-layer-11..tmp"), []byte("partial"), 0600))
	calls := 0
	io.unlink = func(fd int, name string, flags int) error {
		calls++
		if calls == 2 {
			return unix.EIO
		}
		return unix.Unlinkat(fd, name, flags)
	}
	r, e := autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "partial_or_space_unconfirmed", r[0].Status)
	require.Equal(t, 1, r[0].FilesRemoved)
	io.unlink = unix.Unlinkat
	r, e = autoDiscard(c, base, state, io)
	require.NoError(t, e)
	require.Equal(t, "reclaimed", r[0].Status)
	require.Equal(t, 1, r[0].FilesRemoved)
	require.NoDirExists(t, p)
}
