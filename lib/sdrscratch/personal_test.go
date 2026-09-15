package sdrscratch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPersonalOptionContract(t *testing.T) {
	t.Setenv(PersonalPolicyEnv, "")
	t.Setenv("CURIO_SDR_DOMAIN_LEASE_FD", "")
	for _, v := range []string{"", "0", "false", "1", "true", "yes", "TRUE", " 1", "/config.json"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(PersonalCleanupEnv, v)
			on, err := PersonalCleanupEnabled()
			switch v {
			case "", "0", "false":
				require.NoError(t, err)
				require.False(t, on)
			case "1", "true":
				require.NoError(t, err)
				require.True(t, on)
			default:
				require.Error(t, err)
			}
		})
	}
	t.Setenv(PersonalCleanupEnv, "1")
	t.Setenv(PersonalPolicyEnv, "/preserved/manual.json")
	_, err := PersonalCleanupEnabled()
	require.ErrorContains(t, err, "conflicts")
	t.Setenv(PersonalCleanupEnv, "0")
	_, err = PersonalCleanupEnabled()
	require.NoError(t, err, "disabled new mode preserves manual JSON semantics")
}

func personalFixture(t *testing.T) (*personalSession, []string) {
	t.Helper()
	s := &personalSession{state: t.TempDir(), host: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", domain: uuid.NewString(), unit: ManagedUnit{"fixture-a.service", "/system.slice/fixture-a.service"}, roots: map[string]string{}}
	s.access = registrationAccess{dir: openDir, read: func(p string, v any) error {
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		return decodeStrict(b, v)
	}, fs: func(*os.File) error { return nil }}
	roots := []string{t.TempDir(), t.TempDir()}
	for i, r := range roots {
		b, e := json.Marshal(map[string]any{"ID": fmt.Sprint(i), "CanSeal": true})
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(r, "sectorstore.json"), b, 0600))
		require.NoError(t, os.Mkdir(filepath.Join(r, "cache"), 0700))
		s.roots[r] = fmt.Sprint(i)
	}
	return s, roots
}

func TestPersonalRegisteredRootsIndependent(t *testing.T) {
	for _, use := range [][]int{{0}, {1}, {0, 1}} {
		t.Run(fmt.Sprint(use), func(t *testing.T) {
			s, roots := personalFixture(t)
			for _, i := range use {
				c, e := s.register(roots[i], fmt.Sprint(i))
				require.NoError(t, e)
				require.Equal(t, roots[i], c.Storage[0].Root)
			}
			// Only exact physical root identities are registry keys; not sector IDs.
			for _, r := range roots {
				require.NoError(t, os.WriteFile(filepath.Join(r, "cache", "same-sector-output"), []byte("keep"), 0600))
			}
			if len(use) == 2 {
				bad, _, e := s.configPath(roots[0])
				require.NoError(t, e)
				require.NoError(t, os.WriteFile(bad, []byte("{"), 0600))
				_, e = s.register(roots[0], "0")
				require.Error(t, e)
				_, e = s.load(roots[0])
				require.Error(t, e)
				_, e = s.register(roots[1], "1")
				require.NoError(t, e)
				_, e = s.load(roots[1])
				require.NoError(t, e)
				raw, e := os.ReadFile(bad)
				require.NoError(t, e)
				require.Equal(t, "{", string(raw))
			}
			for _, r := range roots {
				require.FileExists(t, filepath.Join(r, "cache", "same-sector-output"))
			}
		})
	}
}

func TestPersonalRegistrationDoesNotInferCapacityOrInventory(t *testing.T) {
	s, roots := personalFixture(t)
	c, err := s.register(roots[0], "0")
	require.NoError(t, err)
	require.True(t, c.Automatic)
	// A second instance joins the same root/domain, retaining the first unit.
	s.unit = ManagedUnit{"fixture-b.service", "/system.slice/fixture-b.service"}
	d, err := s.register(roots[0], "0")
	require.NoError(t, err)
	require.Equal(t, c.Domain, d.Domain)
	require.Len(t, d.Storage, 1)
	require.Len(t, d.Units, 2)
	// Failure in B's registration does not corrupt or replace A's record.
	bRoot, err := openDir(roots[1])
	require.NoError(t, err)
	_, bInode, err := identity(bRoot)
	require.NoError(t, err)
	require.NoError(t, bRoot.Close())
	s.access.fs = func(f *os.File) error {
		_, inode, err := identity(f)
		if err != nil {
			return err
		}
		if inode == bInode {
			return fmt.Errorf("ENOSPC/unavailable root")
		}
		return nil
	}
	_, err = s.register(roots[1], "1")
	require.Error(t, err)
	_, err = s.register(roots[0], "0")
	require.NoError(t, err)
	_, err = s.load(roots[0])
	require.NoError(t, err)
	_, err = s.register(roots[0], "new-id")
	require.Error(t, err)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(roots[0], alias))
	_, err = s.register(alias, "0")
	require.Error(t, err, "no alias-based duplicate space")
	_, err = s.load(t.TempDir())
	require.ErrorContains(t, err, "not a registered")
}

func TestPersonalConcurrentRegistrationPreservesBothAccessors(t *testing.T) {
	a, roots := personalFixture(t)
	b := &personalSession{state: a.state, host: a.host, domain: a.domain, unit: ManagedUnit{"fixture-b.service", "/system.slice/fixture-b.service"}, access: a.access}
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	errors := make(chan error, 2)
	for _, s := range []*personalSession{a, b} {
		wg.Add(1)
		go func(s *personalSession) { defer wg.Done(); <-barrier; _, e := s.register(roots[0], "0"); errors <- e }(s)
	}
	close(barrier)
	wg.Wait()
	close(errors)
	// A busy nonblocking lock is retried at the next preparation pass.
	for err := range errors {
		if err != nil {
			require.ErrorContains(t, err, "registry busy")
		}
	}
	_, err := a.register(roots[0], "0")
	require.NoError(t, err)
	c, err := b.register(roots[0], "0")
	require.NoError(t, err)
	require.Len(t, c.Units, 2)
}

func TestPersonalStateOutsideAllRegisteredRoots(t *testing.T) {
	require.Error(t, stateOutside("/var/lib/curio/sdr-cleanup", []string{"/var/lib"}))
	require.Error(t, stateOutside("/var/lib/curio/sdr-cleanup", []string{"/"}))
	require.Error(t, stateOutside("/var/lib/curio/sdr-cleanup", []string{"relative"}))
	require.NoError(t, stateOutside("/var/lib/curio/sdr-cleanup", []string{"/arbitrary/a", "/different/b"}))
}

type registeredTestBoundary struct {
	*testBoundary
	c *ManagedConfig
}

func (b registeredTestBoundary) checkBase(base string) error { return b.c.checkStorage(base) }
func (b registeredTestBoundary) pinBase(base string) (*os.File, error) {
	return b.c.pinStorageBase(base)
}
func (b registeredTestBoundary) checkSpace(base string) error {
	return b.c.checkSpaceWith(base, reviewAccess())
}

func TestPersonalEmptyBaseRequiresValidRegisteredRoot(t *testing.T) {
	s, roots := personalFixture(t)
	c, err := s.register(roots[0], "0")
	require.NoError(t, err)
	b := registeredTestBoundary{c: c}
	key := filepath.Join(roots[0], "key") // Not yet created on a new storage root.
	_, err = c.pinStorageBase(key)
	require.ErrorIs(t, err, errRegisteredBaseMissing)
	results, err := scanWithBoundary(key, true, b)
	require.NoError(t, err)
	require.Empty(t, results)
	// An absent directory does not erase a pending reclamation witness.
	witness := filepath.Join(c.StateDir, spacePrefix(key)+"1-2.json")
	require.NoError(t, os.WriteFile(witness, []byte("{"), 0600))
	_, err = scanWithBoundary(key, true, b)
	require.Error(t, err)
	require.FileExists(t, witness)
	// Missing metadata/root is not an empty cache/key. Never bypass identity.
	require.NoError(t, os.Remove(filepath.Join(roots[0], "sectorstore.json")))
	_, err = scanWithBoundary(key, true, b)
	require.Error(t, err)
	require.NotErrorIs(t, err, errRegisteredBaseMissing)
	_, err = scanWithBoundary(filepath.Join(roots[1], "key"), true, b)
	require.ErrorContains(t, err, "not enrolled")
}

func TestPersonalOtherRootDamageDoesNotBlockReclamation(t *testing.T) {
	s, roots := personalFixture(t)
	var attempts []string
	for i, root := range roots {
		_, err := s.register(root, fmt.Sprint(i))
		require.NoError(t, err)
		base := filepath.Join(root, "cache")
		p := filepath.Join(base, "s-t01000-42.sdr.tmp", Prefix+uuid.NewString())
		w, err := BeginWithOptions(p, Options{Boundary: new(testBoundary)})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(p, "layer"), []byte("private"), 0600))
		require.NoError(t, w.Close())
		attempts = append(attempts, p)
		require.NoError(t, os.Mkdir(filepath.Join(base, "s-t01000-42"), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(base, "s-t01000-42", "keep"), []byte("canonical"), 0600))
	}
	bad, _, err := s.configPath(roots[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(bad, []byte("{"), 0600))
	_, err = s.load(roots[0])
	require.Error(t, err)
	c, err := s.load(roots[1])
	require.NoError(t, err)
	ended := new(testBoundary) // Kernel termination replaced; file/scanner path is real.
	ended.ended.Store(true)
	b := registeredTestBoundary{testBoundary: ended, c: c}
	results, err := scanWithBoundary(filepath.Join(roots[1], "cache"), true, b)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "reclaimed", results[0].Status)
	require.FileExists(t, filepath.Join(attempts[0], "layer"))
	require.NoFileExists(t, filepath.Join(attempts[1], "layer"))
	for _, root := range roots {
		require.FileExists(t, filepath.Join(root, "cache", "s-t01000-42", "keep"))
	}
}

func TestPersonalIncompleteRegistryIsNotSilentlyRecreated(t *testing.T) {
	s, roots := personalFixture(t)
	name, _, err := s.configPath(roots[0])
	require.NoError(t, err)
	for _, raw := range []string{`{}`, `{"Units":[]}`, `{"Automatic":true}`} {
		require.NoError(t, os.WriteFile(name, []byte(raw), 0600))
		_, err = s.register(roots[0], "0")
		require.Error(t, err)
		actual, err := os.ReadFile(name)
		require.NoError(t, err)
		require.Equal(t, raw, string(actual))
	}
}
