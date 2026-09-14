package ffi

import "golang.org/x/sys/unix"

// Fail closed if a destination already exists or no-replace is unsupported.
// A check followed by os.Rename would allow another attempt to be overwritten.
func publishSDR(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
