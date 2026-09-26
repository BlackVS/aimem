//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// createPrivateFile creates path exclusively (it must not exist), readable
// and writable by the current user only.
func createPrivateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil { // the umask can only narrow it; be explicit anyway
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return f, nil
}

// checkPrivateFile refuses a secret file another local account could read:
// it must be a regular file owned by the current user with no group or
// other permission bits.
func checkPrivateFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is accessible to other accounts (mode %04o); restrict it with: chmod 600 %s", path, perm, path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by the current user", path)
	}
	return nil
}
