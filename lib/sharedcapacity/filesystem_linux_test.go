//go:build linux && (amd64 || arm64)

package sharedcapacity

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestLinuxMeasurementABI(t *testing.T) {
	if unsafe.Sizeof(xfsGeometry{}) != 112 || unsafe.Offsetof(xfsGeometry{}.UUID) != 64 || unsafe.Sizeof(fileExtent{}) != 56 || unsafe.Offsetof(fileMap{}.Extents) != 32 {
		t.Fatal("Linux UAPI layout mismatch")
	}
}

// Explicit disposable-root opt-in, never a production mount or default /sdisk.
// Mount/format privileges are not requested and no other file is traversed.
func TestDisposableXFSPinnedMeasurement(t *testing.T) {
	root := os.Getenv("CURIO_CAPACITY_DISPOSABLE_XFS")
	if root == "" {
		t.Skip("NOT RUN: explicit disposable XFS root required")
	}
	dir, err := os.MkdirTemp(root, "curio-capacity-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p, err := OpenPhysicalFilesystem(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	child := filepath.Join(dir, "alias-child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	q, err := OpenPhysicalFilesystem(child)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = q.Close() }()
	if p.Identity != q.Identity {
		t.Fatal("one XFS split by root path")
	}
	f, err := os.Create(filepath.Join(dir, "owned"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(16 << 20); err != nil {
		t.Fatal(err)
	}
	credit, err := p.ExclusiveFileBytes(f)
	if err != nil || credit != 0 {
		t.Fatal("sparse file got apparent-size credit", credit, err)
	}
	if _, err := f.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	credit, err = p.ExclusiveFileBytes(f)
	if err != nil || credit < 1<<20 {
		t.Fatal("allocated file credit", credit, err)
	}
	clone, err := os.Create(filepath.Join(dir, "clone"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clone.Close() }()
	if err := unix.IoctlFileClone(int(clone.Fd()), int(f.Fd())); err != nil {
		t.Fatal("requires reflink-enabled disposable XFS", err)
	}
	credit, err = p.ExclusiveFileBytes(f)
	if err != nil || credit != 0 {
		t.Fatal("shared extent received credit", credit, err)
	}
	if _, err := p.Free(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(child, child+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Free(); err == nil {
		t.Fatal("root replacement accepted")
	}
}
