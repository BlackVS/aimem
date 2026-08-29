package main

// `aimem token` manages the hub's named-token registry
// (DESIGN-hub-sync): run ON the hub host, it edits
// <state-root>/tokens.json. Secrets are printed exactly once at
// creation; only sha256 digests land on disk, so `list` can never leak
// and revoking is deleting one entry.

import (
	"flag"
	"fmt"
	"slices"

	"aimem/internal/server"
)

func tokenCmd(args []string) error {
	usage := `usage: aimem token add <name> [--role writer|admin]   create (prints the secret ONCE)
       aimem token list                             names and roles (no secrets exist to show)
       aimem token rm <name>                        revoke

Named bearer tokens for this hub. A writer token covers events, sync,
recall, and shared documents; admin adds config, providers, rename,
drop, retention, chapter tools, and logs. The AIMEM_HTTP_TOKEN env
token keeps working as an implicit admin named "env".`
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	root := stateRoot()
	list := server.LoadTokens(root)
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("token add", flag.ExitOnError)
		role := fs.String("role", "writer", "writer or admin")
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: aimem token add <name> [--role writer|admin]")
		}
		name := fs.Arg(0)
		if *role != "writer" && *role != "admin" {
			return fmt.Errorf("role must be writer or admin")
		}
		for _, t := range list {
			if t.Name == name {
				return fmt.Errorf("token %q already exists (rm it first to rotate)", name)
			}
		}
		secret, digest, err := server.NewTokenSecret()
		if err != nil {
			return err
		}
		list = append(list, server.TokenEntry{Name: name, Role: *role, SHA256: digest})
		if err := server.SaveTokens(root, list); err != nil {
			return err
		}
		fmt.Printf("token %q (%s) created — the secret is shown ONCE, store it now:\n%s\n", name, *role, secret)
		fmt.Println("restart is not required; the hub reads the registry per request")
		return nil
	case "list", "ls":
		if len(list) == 0 {
			fmt.Println("no named tokens (the AIMEM_HTTP_TOKEN env token is an implicit admin)")
			return nil
		}
		for _, t := range list {
			fmt.Printf("%-24s %s\n", t.Name, t.Role)
		}
		return nil
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: aimem token rm <name>")
		}
		n := len(list)
		list = slices.DeleteFunc(list, func(t server.TokenEntry) bool { return t.Name == args[1] })
		if len(list) == n {
			return fmt.Errorf("no token named %q", args[1])
		}
		if err := server.SaveTokens(root, list); err != nil {
			return err
		}
		fmt.Printf("token %q revoked\n", args[1])
		return nil
	}
	return fmt.Errorf("%s", usage)
}
