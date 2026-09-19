package sharedcapacity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOneDomainForRootAliasesAndDistinctStorageIDs(t *testing.T) {
	registry := t.TempDir()
	first, err := InitializeDomain(registry, fixtureID, fixturePolicy)
	if err != nil {
		t.Fatal(err)
	}
	// Discovery is deliberately outside this test: both roots/IDs are supplied
	// with the same verified filesystem identity. This is NOT a bind-mount test.
	for _, storageID := range []string{"storage-A", "storage-B", "symlink-alias", "bind-alias"} {
		dir, err := DomainDirectory(registry, fixtureID)
		if err != nil || dir != first {
			t.Fatalf("%s split the domain: %s %v", storageID, dir, err)
		}
	}
	if _, err := InitializeDomain(registry, fixtureID, fixturePolicy); err == nil {
		t.Fatal("rebootstrap overwrote ledger")
	}
	different, err := DomainDirectory(registry, Identity{Host: fixtureID.Host, Filesystem: "replacement-uuid"})
	if err != nil || different == first {
		t.Fatal("replacement device inherited old budget", err)
	}
	if _, err := Open(first, Identity{Host: "different-host", Filesystem: fixtureID.Filesystem}, fixturePolicy, func(context.Context, State) (Sample, error) { return Sample{}, nil }); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
}

func TestLocksBoundedAndIndependentDomains(t *testing.T) {
	a, dir := fixture(t, 4*modelEnvelope)
	b, _ := fixture(t, 4*modelEnvelope)
	fd, err := unix.Open(filepath.Join(dir, "lock"), unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := a.Reserve(ctx, "blocked", "boot", modelEnvelope); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, _, err := b.Reserve(context.Background(), "independent", "boot", modelEnvelope); err != nil {
		t.Fatal("other filesystem blocked", err)
	}
}

func TestSymlinkAndHardlinkMetadataRejected(t *testing.T) {
	for _, name := range []string{"lock", "ledger.json"} {
		for _, kind := range []string{"symlink", "hardlink"} {
			t.Run(name+"-"+kind, func(t *testing.T) {
				a, dir := fixture(t, modelEnvelope+fixturePolicy.Margin)
				target := filepath.Join(dir, name)
				backup := filepath.Join(dir, name+"-backup")
				if err := os.Rename(target, backup); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					if err := os.Symlink(backup, target); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Link(backup, target); err != nil {
						t.Fatal(err)
					}
				}
				if _, _, err := a.Reserve(context.Background(), "sector", "boot", modelEnvelope); err == nil {
					t.Fatal("unsafe metadata admitted")
				}
			})
		}
	}
}

func TestMissingOrReplacedLockNeverCreatesSecondAuthority(t *testing.T) {
	a, dir := fixture(t, modelEnvelope*2)
	old, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	if err := unix.Flock(int(old.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "lock")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Reserve(context.Background(), "new", "boot", modelEnvelope); err == nil {
		t.Fatal("missing lock silently recreated")
	}
	if _, err := os.Stat(filepath.Join(dir, "lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("admission created a lock", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Reserve(context.Background(), "new", "boot", modelEnvelope); !errors.Is(err, ErrIdentity) {
		t.Fatal("replacement lock inherited old ledger", err)
	}
}
