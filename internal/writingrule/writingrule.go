// Package writingrule serves the rule for text agents keep: the bodies they
// send to aimem's write tools and the Markdown files they save
// (docs/WRITING-PERSISTED-TEXT.md, embedded by package docs). The text is
// public and built into the binary; the writing_rule MCP tool returns it
// whole, with the build version, a SHA-256 digest of its bytes and a
// terminator line, so a client that cut the result can tell
// (internal/builtintext renders it).
package writingrule

import (
	"aimem/docs"
	"aimem/internal/builtintext"
)

const (
	// UnitID names the text in its header and terminator.
	UnitID = "aimem-writing-rule"
	// MaxBytes bounds the rule: it is read before writing, so it stays short.
	MaxBytes = 8 << 10
)

// Source is where the text lives in the repository.
const Source = "docs/" + docs.WritingRuleFile

var rule = builtintext.Text{
	Unit:     UnitID,
	Name:     "aimem rule for kept text",
	From:     "from " + Source,
	About:    "It applies to the exact text you send to a write tool and to the Markdown files you save. Public guidance built into the aimem binary; it grants no permission.",
	Body:     docs.WritingRule,
	MaxBytes: MaxBytes,
}

// Digest is "sha256:<hex>" over the embedded bytes; the build version is
// not part of it.
func Digest() string { return rule.Digest() }

// Check validates the embedded text; the package tests make a failure a
// failed build.
func Check() error { return rule.Check() }

// Terminator is the last line of the rendered text.
func Terminator(version string) string { return rule.Terminator(version) }

// Text renders the whole rule: a header naming the unit, version and
// digest, the rule as committed, and the terminator.
func Text(version string) (string, error) { return rule.Render(version) }
