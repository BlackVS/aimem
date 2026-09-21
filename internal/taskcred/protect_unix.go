//go:build !windows

package taskcred

import (
	"errors"
	"os"
)

func checkPrivate(fi os.FileInfo) error {
	if fi.Mode().Perm()&0077 != 0 {
		return errors.New("local task credential permissions must be owner-only (directory 0700, file 0600)")
	}
	return nil
}
func protect(raw []byte) ([]byte, error)   { return raw, nil }
func unprotect(raw []byte) ([]byte, error) { return raw, nil }
