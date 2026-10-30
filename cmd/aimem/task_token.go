package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"aimem/internal/taskcred"
)

// sourceReport is `task-token show-source`: the selection, the hub's state
// for its credential, and the token's label and expiry, live from the
// hub's answer when it accepts the credential, else as recorded.
type sourceReport struct {
	*taskcred.Selection
	State  string `json:"state"` // active, refused or unreachable
	Detail string `json:"detail,omitempty"`
}

// sourceTimeout bounds show-source's identity read; a var so that TestMain
// can raise it for the loopback fakes.
var sourceTimeout = 5 * time.Second

func showSource(s *taskcred.Selection) sourceReport {
	ctx, cancel := context.WithTimeout(context.Background(), sourceTimeout)
	defer cancel()
	id, err := s.Identity(ctx)
	var rejected *taskcred.Rejected
	switch {
	case errors.As(err, &rejected):
		return sourceReport{Selection: s, State: "refused", Detail: rejected.Error()}
	case err != nil:
		return sourceReport{Selection: s, State: "unreachable", Detail: redactCred(err.Error(), s.Token)}
	}
	if id.Label != "" {
		s.Meta = id.Meta()
	}
	return sourceReport{Selection: s, State: "active"}
}

func taskTokenCmd(args []string) error {
	return runTaskToken(args, ".", stateRoot(), os.Stdin, os.Stdout)
}

func runTaskToken(args []string, dir, root string, in io.Reader, out io.Writer) error {
	const usage = "usage: aimem task-token <set [--token-file FILE]|show-source|clear> (run at project root; set reads the token from stdin unless --token-file names an owner-only file)"
	if len(args) == 0 {
		return errors.New(usage)
	}
	// Only set takes an option: --token-file FILE or --token-file=FILE.
	tokenFile, rest := "", args[1:]
	switch {
	case len(rest) == 0:
	case args[0] == "set" && len(rest) == 2 && rest[0] == "--token-file":
		tokenFile = rest[1]
	case args[0] == "set" && len(rest) == 1 && strings.HasPrefix(rest[0], "--token-file="):
		tokenFile = strings.TrimPrefix(rest[0], "--token-file=")
	default:
		return errors.New(usage)
	}
	switch {
	case len(rest) > 0 && tokenFile == "":
		return errors.New(usage)
	case tokenFile == "-":
		return errors.New("--token-file -: standard input is already the default; run aimem task-token set without --token-file")
	}
	switch args[0] {
	case "set":
		var token string
		if tokenFile != "" {
			tok, err := readHubToken(tokenFile)
			if err != nil {
				return err
			}
			token = tok
		} else {
			raw, err := io.ReadAll(io.LimitReader(in, 4097))
			if err != nil {
				return errors.New("cannot read token from stdin")
			}
			if len(raw) > 4096 {
				return errors.New("token input too large")
			}
			token = strings.TrimSpace(string(trimBOM(raw)))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := taskcred.Set(ctx, dir, root, token); err != nil {
			return err
		}
		fmt.Fprintln(out, "Project-local task credential set; .aimem.json contains only the local requirement.")
	case "show-source":
		selected, err := taskcred.Resolve(dir, root)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(showSource(selected))
	case "clear":
		if err := taskcred.Clear(dir, root); err != nil {
			return err
		}
		fmt.Fprintln(out, "Project-local task credential cleared; tasks now use the per-hub user credential when configured.")
	default:
		return errors.New(usage)
	}
	return nil
}
