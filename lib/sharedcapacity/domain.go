package sharedcapacity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// DomainDirectory maps an independently verified host/filesystem identity to
// one ledger location within a cohort-wide registry. Storage root, storage ID,
// bind mount ID and boot ID deliberately do not participate in the key.
// This is not a filesystem identity discovery routine. Passing guessed IDs or
// using different registries for one cohort violates the protocol.
func DomainDirectory(registry string, id Identity) (string, error) {
	if id.Host == "" || id.Filesystem == "" || !filepath.IsAbs(registry) {
		return "", ErrIdentity
	}
	b, err := json.Marshal(id)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return filepath.Join(registry, hex.EncodeToString(sum[:])), nil
}

// InitializeDomain creates metadata only; it neither probes nor adopts existing
// writers. The caller must first establish the drained-cohort bootstrap proof.
// An existing or corrupt ledger is never overwritten.
func InitializeDomain(registry string, id Identity, policy Policy) (string, error) {
	if !validPolicy(id, policy) {
		return "", ErrIdentity
	}
	dir, err := DomainDirectory(registry, id)
	if err != nil {
		return "", err
	}
	root, err := openDirectory(registry, id, policy, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	// The registry is a trusted, cohort-wide private directory. Replacement of
	// that directory by its owner is outside this library's writer protocol.
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := root.dir.Sync(); err != nil {
		return "", err
	}
	return dir, Initialize(dir, id, policy)
}
