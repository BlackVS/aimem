// Package redact sanitizes event content before persistence and enforces
// per-field size caps. It runs at the service ingestion boundary; adapters
// are expected to redact before sending too (defense in depth). Pattern
// redaction is best-effort — archives are never assumed publishable.
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const Placeholder = "[REDACTED]"

// DefaultMaxFieldBytes caps individual persisted fields.
const DefaultMaxFieldBytes = 64 * 1024

// pattern names each secret shape. high marks shapes that are a secret
// with near-certainty (private keys, recognised vendor token formats):
// authored-content publishers refuse on those, and only warn on the
// softer ones, which catch too much ordinary prose to refuse on.
type pattern struct {
	kind string
	high bool
	re   *regexp.Regexp
}

var patterns = []pattern{
	// Authorization headers and assignments.
	{"authorization header", false, regexp.MustCompile(`(?i)\bauthorization\b\s*[:=]\s*\S+(?:\s+\S+)?`)},
	{"bearer token", false, regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`)},
	// Private key blocks (multiline).
	{"private key block", true, regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)},
	// Common credential assignments: KEY=value, "api_key": "value", etc.
	{"credential assignment", false, regexp.MustCompile(`(?i)\b(api[_-]?key|apikey|secret[_-]?key|client[_-]?secret|secret|access[_-]?token|refresh[_-]?token|auth[_-]?token|token|passwd|password|access[_-]?key)\b["']?\s*[:=]\s*["']?[^\s"',;]{8,}`)},
	// Well-known token shapes.
	{"AWS access key id", true, regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"GitHub PAT", true, regexp.MustCompile(`\bghp_[A-Za-z0-9]{30,}\b`)},
	{"GitHub fine-grained PAT", true, regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}\b`)},
	{"OpenAI-style secret key", true, regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	{"Slack token", true, regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{"GitLab PAT", true, regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{"JWT", true, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
}

// ScanAuthored reports secret-shaped content in an authored document.
// Shared documents publish AS WRITTEN — silently redacting one would
// corrupt intended content — so publishers scan instead: refuse names
// high-confidence shapes that must block publication, warn the softer
// ones worth a stderr note (DESIGN-shared-docs open question 2).
func ScanAuthored(s string) (warn, refuse []string) {
	for _, p := range patterns {
		if p.re.MatchString(s) {
			if p.high {
				refuse = append(refuse, p.kind)
			} else {
				warn = append(warn, p.kind)
			}
		}
	}
	return warn, refuse
}

// secretEnvValues returns literal secret values named by SESSIOND_SECRET_ENV
// (comma-separated env var names) so known live secrets are scrubbed even
// when no pattern matches their shape.
func secretEnvValues() []string {
	names := os.Getenv("SESSIOND_SECRET_ENV")
	if names == "" {
		return nil
	}
	var vals []string
	for n := range strings.SplitSeq(names, ",") {
		if v := os.Getenv(strings.TrimSpace(n)); len(v) >= 6 {
			vals = append(vals, v)
		}
	}
	return vals
}

// String sanitizes one field: pattern redaction, literal known-secret
// scrubbing, then the size cap. truncated reports whether capping occurred;
// a truncated field ends with a marker recording the sha256 and original
// size of the full sanitized content, so loss is visible, never silent.
func String(s string, maxBytes int) (out string, truncated bool) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFieldBytes
	}
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, Placeholder)
	}
	for _, v := range secretEnvValues() {
		s = strings.ReplaceAll(s, v, Placeholder)
	}
	if len(s) > maxBytes {
		sum := sha256.Sum256([]byte(s))
		cut := s[:maxBytes]
		// Avoid splitting a UTF-8 rune.
		for len(cut) > 0 && cut[len(cut)-1] >= 0x80 && cut[len(cut)-1] < 0xC0 {
			cut = cut[:len(cut)-1]
		}
		s = fmt.Sprintf("%s\n[TRUNCATED sha256=%s orig_bytes=%d]", cut, hex.EncodeToString(sum[:]), len(s))
		return s, true
	}
	return s, false
}

// Strings sanitizes a slice with a shared per-element cap.
func Strings(in []string, maxBytes int) (out []string, truncated bool) {
	out = make([]string, len(in))
	for i, s := range in {
		var t bool
		out[i], t = String(s, maxBytes)
		truncated = truncated || t
	}
	return out, truncated
}
