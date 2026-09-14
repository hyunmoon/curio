package ffi

import "golang.org/x/sys/unix"

func publishSDR(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
