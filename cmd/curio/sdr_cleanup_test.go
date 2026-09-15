package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"

	"github.com/filecoin-project/curio/lib/sdrscratch"
	"github.com/filecoin-project/curio/lib/storiface"
)

func TestCleanupUsesRegisteredPathsNotDiskNames(t *testing.T) {
	repo := t.TempDir()
	var cfg storiface.StorageConfig
	var sealing []string
	for i := 0; i < 3; i++ {
		root := t.TempDir()
		meta, err := json.Marshal(storiface.LocalStorageMeta{ID: storiface.ID(string(rune('A' + i))), CanSeal: i < 2, CanStore: true})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, "sectorstore.json"), meta, 0600))
		cfg.StoragePaths = append(cfg.StoragePaths, storiface.LocalPath{Path: root})
		if i < 2 {
			sealing = append(sealing, root)
		}
	}
	b, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "storage.json"), b, 0600))
	roots, err := registeredCleanupRoots(repo, true)
	require.NoError(t, err)
	require.Equal(t, sealing, roots)
	roots, err = registeredCleanupRoots(repo, false)
	require.NoError(t, err)
	require.Len(t, roots, 3)
	for _, i := range []int{0, 1} {
		one := storiface.StorageConfig{StoragePaths: []storiface.LocalPath{cfg.StoragePaths[i]}}
		b, err = json.Marshal(one)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(repo, "storage.json"), b, 0600))
		roots, err = registeredCleanupRoots(repo, true)
		require.NoError(t, err)
		require.Equal(t, []string{sealing[i]}, roots)
	}
}

func TestCleanupReadonlyCLIAndDisabledStartup(t *testing.T) {
	t.Setenv(sdrscratch.PersonalCleanupEnv, "1")
	// Actual urfave help/version paths do not execute run.Action or touch storage.
	for _, args := range [][]string{{"curio", "--version"}, {"curio-0", "run", "--help"}, {"curio", "sdr-cleanup", "--help"}} {
		app := &cli.App{Name: "curio", Version: "fixture", Commands: []*cli.Command{runCmd, sdrCleanupCmd}, Writer: &bytes.Buffer{}, ErrWriter: &bytes.Buffer{}}
		require.NoError(t, app.Run(args))
	}
	t.Setenv(sdrscratch.PersonalCleanupEnv, "0")
	require.NoError(t, startPersonalSDRCleanup(filepath.Join(t.TempDir(), "absent")))
	t.Setenv(sdrscratch.PersonalCleanupEnv, "invalid")
	require.ErrorContains(t, startPersonalSDRCleanup("unread"), "must be unset")
}

func TestCleanupCLIRejectsBypassesBeforeIO(t *testing.T) {
	for _, args := range [][]string{{"curio", "sdr-cleanup", "apply", "--yes"}, {"curio", "sdr-cleanup", "apply", "--force"}, {"curio", "sdr-cleanup", "preview", "--all"}} {
		app := &cli.App{Commands: []*cli.Command{sdrCleanupCmd}, Writer: &bytes.Buffer{}, ErrWriter: &bytes.Buffer{}}
		require.Error(t, app.Run(args))
	}
}
