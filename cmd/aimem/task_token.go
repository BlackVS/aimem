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

func taskTokenCmd(args []string) error {
	return runTaskToken(args, ".", stateRoot(), os.Stdin, os.Stdout)
}

func runTaskToken(args []string, dir, root string, in io.Reader, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: aimem task-token <set|show-source|clear> (run at project root; set reads the token from stdin)")
	}
	switch args[0] {
	case "set":
		raw, err := io.ReadAll(io.LimitReader(in, 4097))
		if err != nil {
			return errors.New("cannot read token from stdin")
		}
		if len(raw) > 4096 {
			return errors.New("token input too large")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := taskcred.Set(ctx, dir, root, strings.TrimSpace(string(raw))); err != nil {
			return err
		}
		fmt.Fprintln(out, "Project-local task credential set; .aimem.json contains only the local requirement.")
	case "show-source":
		selected, err := taskcred.Resolve(dir, root)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(selected)
	case "clear":
		if err := taskcred.Clear(dir, root); err != nil {
			return err
		}
		fmt.Fprintln(out, "Project-local task credential cleared; tasks now use the per-hub user credential when configured.")
	default:
		return errors.New("usage: aimem task-token <set|show-source|clear>")
	}
	return nil
}
