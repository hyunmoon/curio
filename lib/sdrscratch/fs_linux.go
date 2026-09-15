package sdrscratch

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func supportedFS(f *os.File) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Type != unix.EXT4_SUPER_MAGIC && st.Type != unix.XFS_SUPER_MAGIC {
		return fmt.Errorf("SDR scratch ownership unsupported on filesystem %#x", st.Type)
	}
	return nil
}

func sameMount(a, b *os.File) error {
	var sa, sb unix.Statx_t
	if err := unix.Statx(int(a.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &sa); err != nil {
		return err
	}
	if err := unix.Statx(int(b.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &sb); err != nil {
		return err
	}
	if sa.Mask&unix.STATX_MNT_ID == 0 || sb.Mask&unix.STATX_MNT_ID == 0 || sa.Mnt_id != sb.Mnt_id {
		return fmt.Errorf("unknown/cross-mount SDR scratch")
	}
	return nil
}
