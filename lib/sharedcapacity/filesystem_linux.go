//go:build linux && (amd64 || arm64)

package sharedcapacity

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Read-only Linux UAPI layouts, checked against Linux v6.8 xfs_fs.h/fiemap.h.
// This is a measurement primitive, not an all-writer mutation protocol.
type xfsGeometry struct {
	BlockSize, RTextSize, AGBlocks, AGCount, LogBlocks, SectSize, InodeSize, IMaxPct uint32
	DataBlocks, RTBlocks, RTExtents, LogStart                                        uint64
	UUID                                                                             [16]byte
	SUnit, SWidth                                                                    uint32
	Version                                                                          int32
	Flags, LogSectSize, RTSectSize, DirBlockSize                                     uint32
}

type PhysicalFilesystem struct {
	Root     *os.File
	Identity Identity
	device   uint64
	fsid     unix.Fsid
}

func OpenPhysicalFilesystem(path string) (*PhysicalFilesystem, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	p := &PhysicalFilesystem{Root: os.NewFile(uintptr(fd), path)}
	fail := func(err error) (*PhysicalFilesystem, error) { _ = p.Close(); return nil, err }
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fail(err)
	}
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return fail(err)
	}
	if fs.Type != unix.XFS_SUPER_MAGIC {
		return fail(errors.New("shared capacity requires local XFS"))
	}
	var geometry xfsGeometry
	if unsafe.Sizeof(geometry) != 112 {
		return fail(errors.New("unexpected XFS ABI"))
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0x80705864, uintptr(unsafe.Pointer(&geometry)))
	if errno != 0 {
		return fail(errno)
	}
	if geometry.UUID == [16]byte{} || geometry.RTBlocks != 0 {
		return fail(errors.New("missing UUID or unsupported realtime XFS"))
	}
	b, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return fail(err)
	}
	host := strings.TrimSpace(string(b))
	decoded, err := hex.DecodeString(host)
	if err != nil || len(decoded) != 16 {
		return fail(errors.New("invalid host machine identity"))
	}
	p.Identity = Identity{Host: host, Filesystem: hex.EncodeToString(geometry.UUID[:])}
	p.device, p.fsid = uint64(st.Dev), fs.Fsid
	return p, nil
}

func (p *PhysicalFilesystem) Close() error { return p.Root.Close() }

// Free is fresh Bavail, never cached DU or apparent file length. It does NOT
// implement per-user/group/project quota or per-storage MaxStorage limits.
// Admission must separately validate those scopes; do not use this alone.
func (p *PhysicalFilesystem) Free() (int64, error) {
	var st, path unix.Stat_t
	var fs unix.Statfs_t
	fd := int(p.Root.Fd())
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, err
	}
	if err := unix.Lstat(p.Root.Name(), &path); err != nil {
		return 0, err
	}
	if st.Dev != path.Dev || st.Ino != path.Ino || uint64(st.Dev) != p.device {
		return 0, ErrIdentity
	}
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return 0, err
	}
	if fs.Type != unix.XFS_SUPER_MAGIC || fs.Fsid != p.fsid || fs.Bsize <= 0 || fs.Bavail > uint64(math.MaxInt64)/uint64(fs.Bsize) {
		return 0, ErrIdentity
	}
	return int64(fs.Bavail) * fs.Bsize, nil
}

type fileMap struct {
	Start, Length                  uint64
	Flags, Mapped, Count, Reserved uint32
	Extents                        [128]fileExtent
}

// ExclusiveFileBytes may only be used inside an established immutable-file
// phase. The caller must prevent truncate/unlink/reflink/overwrite until Free
// has been sampled and admission committed. An FD or stable stat alone is NOT
// that fence. Unknown/delalloc/shared/encoded extents receive zero credit.
func (p *PhysicalFilesystem) ExclusiveFileBytes(f *os.File) (int64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	if uint64(st.Dev) != p.device || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		return 0, ErrIdentity
	}
	var total int64
	var start uint64
	// Bounded calls: highly fragmented files receive partial, conservative
	// credit. No FIEMAP_FLAG_SYNC: unknown delayed allocation gets no credit.
	for range 64 {
		m := fileMap{Start: start, Length: math.MaxUint64 - start, Count: 128}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), 0xc020660b, uintptr(unsafe.Pointer(&m)))
		if errno != 0 {
			return 0, fmt.Errorf("FIEMAP: %w", errno)
		}
		if m.Mapped > m.Count || m.Mapped > 128 {
			return 0, ErrIdentity
		}
		if m.Mapped == 0 {
			break
		}
		last := false
		for _, e := range m.Extents[:m.Mapped] {
			credit, end, err := extentCredit(e, start)
			if err != nil {
				return 0, err
			}
			start = end
			total, err = add(total, credit)
			if err != nil {
				return 0, err
			}
			last = e.Flags&1 != 0
		}
		if last {
			break
		}
	}
	var after unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &after); err != nil {
		return 0, err
	}
	if after.Dev != st.Dev || after.Ino != st.Ino || after.Nlink != st.Nlink || after.Size != st.Size || after.Blocks != st.Blocks || after.Mtim != st.Mtim || after.Ctim != st.Ctim {
		return 0, errors.New("file changed during extent observation")
	}
	return total, nil
}
