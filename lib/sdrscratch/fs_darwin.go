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
	name := unix.ByteSliceToString(st.Fstypename[:])
	if name != "apfs" {
		return fmt.Errorf("SDR scratch ownership unsupported on filesystem %s", name)
	}
	return nil
}

func sameMount(a, b *os.File) error {
	var sa, sb unix.Statfs_t
	if err := unix.Fstatfs(int(a.Fd()), &sa); err != nil {
		return err
	}
	if err := unix.Fstatfs(int(b.Fd()), &sb); err != nil {
		return err
	}
	if sa.Fsid != sb.Fsid {
		return fmt.Errorf("cross-mount SDR scratch")
	}
	return nil
}
