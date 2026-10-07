// Command broken stands in for a release that cannot start: it reports a
// version, and its serve changes the state root, as a one-way migration
// would, then exits with an error. It finds the state root as aimem does:
// AIMEM_STATE_DIR from the environment, else from ~/.config/aimem/env.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func stateRoot() string {
	if v := os.Getenv("AIMEM_STATE_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	raw, _ := os.ReadFile(filepath.Join(home, ".config", "aimem", "env"))
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "AIMEM_STATE_DIR="); ok {
			return v
		}
	}
	return ""
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("aimem v0.0.3")
		return
	}
	if root := stateRoot(); len(os.Args) > 1 && os.Args[1] == "serve" && root != "" {
		_ = os.WriteFile(filepath.Join(root, "migrated-by-broken"), []byte("x\n"), 0o600)
	}
	os.Exit(1)
}
