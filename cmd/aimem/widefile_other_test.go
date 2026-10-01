//go:build !windows

package main

import (
	"os"
	"testing"
)

// writeWide writes a file others can read.
func writeWide(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // the umask can only remove bits
		t.Fatal(err)
	}
}
