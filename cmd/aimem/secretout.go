package main

// `--output <file|->`: the one way an aimem command writes an issued secret
// (docs/DESIGN-AIFORGE-PILOT-1.md §6). A file must not exist and is created
// readable by its owner only, before anything is issued; `-` writes to
// standard output for a pipe and is refused when standard output is a
// terminal, so the value never lands on a screen.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"aimem/internal/privatefile"
)

// secretStdout and noticeOut are where `--output -` and the one-release
// notices write; tests replace them.
var (
	secretStdout           = os.Stdout
	noticeOut    io.Writer = os.Stderr
)

// legacyForm prints the one-line notice an old command form carries for one
// release, naming the form that replaces it.
func legacyForm(newForm string) {
	fmt.Fprintf(noticeOut, "aimem: this form is kept for one release; use: %s\n", newForm)
}

type secretOutput struct {
	flag string // the flag that named the destination, for messages
	path string // "-" for standard output
	f    *os.File
}

// reserveSecretOutput checks and claims the destination before anything is
// issued.
func reserveSecretOutput(flag, path string) (*secretOutput, error) {
	if path == "" {
		return nil, fmt.Errorf("%s is required: a new file only you can read, or - for a pipe", flag)
	}
	if path == "-" {
		fi, err := secretStdout.Stat()
		if err != nil {
			return nil, fmt.Errorf("%s -: cannot inspect standard output (%v); nothing was issued", flag, err)
		}
		if fi.Mode()&os.ModeCharDevice != 0 {
			return nil, fmt.Errorf("%s -: standard output is a terminal; pipe it into the command that reads the secret, or name a new file; nothing was issued", flag)
		}
		return &secretOutput{flag: flag, path: "-", f: secretStdout}, nil
	}
	f, err := reserveSecretFile(flag, path)
	if err != nil {
		return nil, err
	}
	return &secretOutput{flag: flag, path: path, f: f}, nil
}

// reserveSecretFile checks the destination before anything is issued: the
// directory must exist and the path must not, and the file is created
// exclusively with owner-only access right away.
func reserveSecretFile(flag, path string) (*os.File, error) {
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s %s: its directory does not exist; nothing was issued", flag, path)
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%s %s already exists and is never overwritten; choose a new path; nothing was issued", flag, path)
	}
	f, err := privatefile.Create(path)
	if err != nil {
		return nil, fmt.Errorf("%s %s cannot be created (%v); nothing was issued", flag, path, err)
	}
	if err := privatefile.Check(path); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("%s %s is not private after creation (%v); nothing was issued", flag, path, err)
	}
	return f, nil
}

// toStdout reports whether the secret goes to standard output, in which
// case every other line a command prints goes to standard error.
func (o *secretOutput) toStdout() bool { return o.path == "-" }

// where names the destination for the operator.
func (o *secretOutput) where() string {
	if o.toStdout() {
		return "standard output"
	}
	return o.path + " (readable only by you)"
}

// write delivers the secret once.
func (o *secretOutput) write(secret string) error {
	if secret == "" {
		return errors.New("the answer carried no secret")
	}
	if o.toStdout() {
		_, err := fmt.Fprintln(o.f, secret)
		return err
	}
	return deliverSecret(o.f, secret)
}

// abandon removes a reserved file that never received a secret.
func (o *secretOutput) abandon() {
	if o.toStdout() {
		return
	}
	o.f.Close()
	os.Remove(o.path)
}
