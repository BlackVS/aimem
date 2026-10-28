package main

import (
	"bytes"
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

// trimBOM drops one leading UTF-8 byte order mark from a secret read from
// a file or standard input: Windows PowerShell 5.1 prepends one to what it
// pipes under a UTF-8 console, and writes one with Out-File and
// Set-Content -Encoding utf8. A second one stays, for the reader's own
// validation to refuse.
func trimBOM(raw []byte) []byte { return bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) }

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
		if tokenShaped(path) {
			return "", errTokenAsPath
		}
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
	tok := strings.TrimSuffix(strings.TrimSuffix(string(trimBOM(raw)), "\n"), "\r")
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

// errTokenAsPath refuses a --token-file value that is the token itself.
// It never quotes the value: every other error names the path, and here
// the path is the secret.
var errTokenAsPath = errors.New("--token-file takes the path of a file that holds the token, and the value given names no file and has the shape of a token, so it is not repeated here: write the token to a file only you can read, and pass that file's path")

// tokenShaped reports a --token-file value that names no file and looks
// like a hub token: a user or peer credential (aimem_user_, aimem_peer_),
// or a long word of token characters such as a writer token (64 hex) or
// the installer's env token (48 hex). A value with a path separator or a
// dot is a path, as is anything that exists.
func tokenShaped(v string) bool {
	if strings.ContainsAny(v, `/\.`) {
		return false
	}
	if _, err := os.Lstat(v); err == nil {
		return false
	}
	if strings.HasPrefix(v, "aimem_user_") || strings.HasPrefix(v, "aimem_peer_") {
		return true
	}
	if len(v) < 32 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
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
