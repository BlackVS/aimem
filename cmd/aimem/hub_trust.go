package main

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"aimem/internal/adapter"
	"aimem/internal/privatefile"
)

// hubTrust checks `aimem hub add`'s --ca-file and --pin: at most one, never
// with --insecure. A CA file is recorded by its absolute path and must hold
// a PEM certificate now; a pin must be sha256-BASE64. Every client path to
// the hub then trusts it (adapter.HubConfig.TLSConfig).
func hubTrust(caFile, pin string, insecure bool) (string, string, error) {
	switch {
	case caFile != "" && pin != "":
		return "", "", errors.New("use either --ca-file or --pin, not both")
	case (caFile != "" || pin != "") && insecure:
		return "", "", errors.New("--insecure skips the verification that --ca-file and --pin configure: use one or the other")
	case pin != "":
		if _, err := adapter.ParsePin(pin); err != nil {
			return "", "", fmt.Errorf("--pin: %w", err)
		}
		return "", pin, nil
	case caFile != "":
		abs, err := filepath.Abs(caFile)
		if err != nil {
			return "", "", fmt.Errorf("--ca-file %s: %w", caFile, err)
		}
		pem, err := os.ReadFile(abs)
		if err != nil {
			return "", "", fmt.Errorf("--ca-file %s cannot be read: %w", abs, err)
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			return "", "", fmt.Errorf("--ca-file %s holds no PEM certificate", abs)
		}
		return abs, "", nil
	}
	return "", "", nil
}

// hubStdin is where `--token-file -` reads; tests replace it.
var hubStdin io.Reader = os.Stdin

// maxHubToken bounds a token read from a file or standard input.
const maxHubToken = 4096

// readHubToken reads `aimem hub add|task-token --token-file`: a private
// file (only its owner can read it), or "-" for standard input, holding one
// token on one line. A trailing LF or CRLF is dropped (a stray CR would make
// the hub answer 401). Errors name the source, never its content.
func readHubToken(path string) (string, error) {
	src := "--token-file " + path
	var r io.Reader
	if path == "-" {
		src, r = "standard input", hubStdin
	} else {
		if err := privatefile.Check(path); err != nil {
			return "", fmt.Errorf("--token-file: %w", err)
		}
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("--token-file %s cannot be read: %w", path, err)
		}
		defer f.Close()
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxHubToken+1))
	if err != nil {
		return "", fmt.Errorf("%s cannot be read: %w", src, err)
	}
	if len(raw) > maxHubToken {
		return "", fmt.Errorf("%s is larger than a token", src)
	}
	tok := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if tok == "" {
		return "", fmt.Errorf("%s holds no token", src)
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < 0x21 || tok[i] > 0x7e {
			return "", fmt.Errorf("%s must hold the token alone on one line", src)
		}
	}
	return tok, nil
}

// hubTokenArg is the token of a hub command: the positional one or
// --token-file, exactly one of them.
func hubTokenArg(positional []string, tokenFile string) (string, error) {
	switch {
	case tokenFile != "" && len(positional) > 0:
		return "", errors.New("give the token either as an argument or with --token-file, not both")
	case tokenFile != "":
		return readHubToken(tokenFile)
	case len(positional) == 1:
		return positional[0], nil
	}
	return "", errors.New("missing token: give it as an argument or with --token-file PATH (- for standard input)")
}
