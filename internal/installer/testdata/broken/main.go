// Command broken stands in for a release that cannot start: it reports a
// version, and its serve changes the state root, as a one-way migration
// would, then exits with an error.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("aimem v0.0.3")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		_ = os.WriteFile(filepath.Join(os.Getenv("AIMEM_STATE_DIR"), "migrated-by-broken"), []byte("x\n"), 0o600)
	}
	os.Exit(1)
}
