package sdrscratch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAutoDiscardPrivateWithoutPipelineLayout(t *testing.T) {
	for _, relative := range []string{"s-t01000-42.tmp", "s-t01000-42.sdr.tmp/attempt-" + uuid.NewString(), "s-t01000-42.sdr.tmp/" + Prefix + uuid.NewString()} {
		t.Run(relative, func(t *testing.T) {
			c, base, io, _ := autoFixture(t)
			p := autoWrite(t, base, relative, 1)
			require.NoError(t, os.WriteFile(filepath.Join(p, "unknown-native-work"), make([]byte, 4097), 0600))
			r, err := autoDiscard(c, base, func(_ AutoTarget, fn func(AutoStage) error) error {
				// No proof/layout can be recovered from an absent pipeline row.
				return fn(AutoStage{Allowed: true, Reason: "private unpublished folder; pipeline absent"})
			}, io)
			require.NoError(t, err)
			require.Len(t, r, 1)
			require.Equal(t, "reclaimed", r[0].Status, r[0].Reason)
			require.Equal(t, 2, r[0].FilesRemoved)
			require.NoDirExists(t, p)
		})
	}
}

func TestAutoDiscardCanonicalStillNeedsLayout(t *testing.T) {
	c, base, io, _ := autoFixture(t)
	p := autoWrite(t, base, "s-t01000-42", 1)
	r, err := autoDiscard(c, base, func(_ AutoTarget, fn func(AutoStage) error) error {
		return fn(AutoStage{Allowed: true})
	}, io)
	require.NoError(t, err)
	require.Zero(t, r[0].FilesRemoved)
	require.Contains(t, r[0].Reason, "layout unknown")
	require.FileExists(t, filepath.Join(p, "sc-02-data-layer-1.dat"))
}

func TestPrivateTemporaryScope(t *testing.T) {
	for _, test := range []struct {
		base, relative, sector string
		canonical, want        bool
	}{
		{"cache", "s-t01000-42.tmp", "s-t01000-42", false, true},
		{"key", "s-t01000-42.sdr.tmp/attempt-" + uuid.NewString(), "s-t01000-42", false, true},
		{"cache", "s-t01000-42.sdr.tmp/" + Prefix + uuid.NewString(), "s-t01000-42", false, true},
		{"sealed", "s-t01000-42.tmp", "s-t01000-42", false, false},
		{"cache", "s-t01000-42.tmp", "s-t02000-42", false, false},
		{"cache", "s-t01000-42", "s-t01000-42", true, false},
		{"cache", "s-t01000-42", "s-t01000-42", false, false},
		{"cache", "s-t01000-42.sdr.tmp/unrecognized", "s-t01000-42", false, false},
		{"cache", "../s-t01000-42.tmp", "s-t01000-42", false, false},
	} {
		target := AutoTarget{Base: test.base, Relative: test.relative, Sector: test.sector, Canonical: test.canonical}
		require.Equal(t, test.want, target.PrivateTemporary(), "%+v", target)
	}
}
