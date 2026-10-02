// Package writingrule serves the rule for text agents keep: the bodies they
// send to aimem's write tools and the Markdown files they save
// (docs/WRITING-PERSISTED-TEXT.md, embedded by package docs). The text is
// public and built into the binary; the writing_rule MCP tool returns it
// whole, with the build version, a SHA-256 digest of its bytes and a
// terminator line, so a client that cut the result can tell.
package writingrule

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"aimem/docs"
)

const (
	// UnitID names the text in its header and terminator.
	UnitID = "aimem-writing-rule"
	// MaxBytes bounds the rule: it is read before writing, so it stays short.
	MaxBytes = 8 << 10
)

// Source is where the text lives in the repository.
const Source = "docs/" + docs.WritingRuleFile

// Digest is "sha256:<hex>" over the embedded bytes; the build version is
// not part of it.
func Digest() string {
	sum := sha256.Sum256(docs.WritingRule)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Check validates the embedded text; the package tests make a failure a
// failed build.
func Check() error {
	b := docs.WritingRule
	switch {
	case !bytes.HasPrefix(b, []byte("# ")):
		return errors.New("writing rule: the file must start with its title heading")
	case bytes.IndexByte(b, '\r') >= 0:
		return errors.New("writing rule: the file contains a carriage return; it is LF-only so its digest is the same on every platform")
	case len(b) > MaxBytes:
		return fmt.Errorf("writing rule: %d bytes, over the %d-byte limit", len(b), MaxBytes)
	case b[len(b)-1] != '\n':
		return errors.New("writing rule: the file must end with a newline")
	}
	return nil
}

func orDev(version string) string {
	if version == "" {
		return "dev"
	}
	return version
}

// Terminator is the last line of the rendered text.
func Terminator(version string) string {
	return fmt.Sprintf("=== end %s version %s digest %s ===", UnitID, orDev(version), Digest())
}

// Text renders the whole rule: a header naming the unit, version and
// digest, the rule as committed, and the terminator.
func Text(version string) (string, error) {
	if err := Check(); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "aimem rule for kept text. Unit %s, version %s, digest %s, from %s.\n", UnitID, orDev(version), Digest(), Source)
	b.WriteString("It applies to the exact text you send to a write tool and to the Markdown files you save. Public guidance built into the aimem binary; it grants no permission.\n")
	fmt.Fprintf(&b, "Complete only if the last line is %q.\n\n", Terminator(version))
	b.Write(docs.WritingRule)
	b.WriteString(Terminator(version))
	b.WriteByte('\n')
	return b.String(), nil
}
