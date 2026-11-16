package redact

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// ScanDocument is the stricter scan for a shared document: the client
// publisher, MCP update_doc and the hub's document write all call it, so
// what they refuse cannot drift apart. It differs from ScanAuthored (which
// records and task text keep, unchanged) in three ways:
//
//   - a value that is a placeholder is no finding: a document that shows a
//     header's or an assignment's form must still publish;
//   - a bearer token refuses, unless its value is a placeholder or a
//     descriptive stand-in (letter-only words joined by - or _, all
//     lowercase or all uppercase), which only warns;
//   - a credential assignment refuses when its value is 20 characters or
//     more and mixes letters and digits; a shorter or letters-only value,
//     or a filesystem path, warns.
//
// The authorization header stays a warning. Like ScanAuthored it reports
// rule names only, never the matched text.
func ScanDocument(s string) (warn, refuse []string) {
	for _, p := range patterns {
		if p.high && p.re.MatchString(s) {
			refuse = append(refuse, p.kind)
		}
	}
	add := func(kind string, outcome docOutcome) {
		switch outcome {
		case docRefuse:
			refuse = appendOnce(refuse, kind)
		case docWarn:
			warn = appendOnce(warn, kind)
		}
	}
	for _, m := range authValue.FindAllStringSubmatch(s, -1) {
		v := m[1]
		if scheme.MatchString(v) && m[2] != "" {
			v = m[2]
		}
		if !isPlaceholder(v) {
			add("authorization header", docWarn)
		}
	}
	for _, m := range bearerValue.FindAllStringSubmatch(s, -1) {
		add("bearer token", bearerOutcome(m[1]))
	}
	for _, m := range credentialValue.FindAllStringSubmatch(s, -1) {
		add("credential assignment", credentialOutcome(m[1]))
	}
	// A refused rule is not also a warning.
	kept := warn[:0]
	for _, w := range warn {
		if !slices.Contains(refuse, w) {
			kept = append(kept, w)
		}
	}
	return kept, refuse
}

// DocumentFix is what every refusal of a document tells its author to do.
// There is no override: a document that must show a token's form shows it
// with a placeholder, which the policy never refuses.
const DocumentFix = "remove the secret, or show its form with a placeholder such as <token>"

type docOutcome int

const (
	docNone docOutcome = iota
	docWarn
	docRefuse
)

// The soft rules of patterns, with the value captured. The bearer value is
// captured as a whole word, so that a wrapped value is judged too;
// bearerShape is its counterpart's own shape.
var (
	authValue       = regexp.MustCompile(`(?i)\bauthorization\b\s*[:=]\s*(\S+)(?:\s+(\S+))?`)
	bearerValue     = regexp.MustCompile(`(?i)\bbearer\s+(\S+)`)
	bearerShape     = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{16,}`)
	tokenRun        = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+`)
	credentialValue = regexp.MustCompile(`(?i)\b(?:api[_-]?key|apikey|secret[_-]?key|client[_-]?secret|secret|access[_-]?token|refresh[_-]?token|auth[_-]?token|token|passwd|password|access[_-]?key)\b["']?\s*[:=]\s*["']?([^\s"',;]{8,})`)

	scheme = regexp.MustCompile(`(?i)^(bearer|basic|token|digest|negotiate)$`)
	// A template: <name>, {name}, {{name}}, ${name}, %{name}, [name], $NAME, %NAME%.
	template = regexp.MustCompile(`^(<[^<>]*>|\{\{.*\}\}|\$\{[^{}]*\}|%\{[^{}]*\}|\{[^{}]*\}|\[[^\[\]]*\]|\$[A-Za-z_][A-Za-z0-9_]*|%[A-Za-z_][A-Za-z0-9_]*%)$`)
	standIn  = regexp.MustCompile(`^(?:[a-z]+(?:[-_][a-z]+)+|[A-Z]+(?:[-_][A-Z]+)+)$`)
	// A filesystem path names where a secret lives; it is not one.
	pathStart  = regexp.MustCompile(`^(?:/|\./|\.\./|~/|[A-Za-z]:[\\/])`)
	hasLetter  = regexp.MustCompile(`[A-Za-z]`)
	hasDigit   = regexp.MustCompile(`[0-9]`)
	alnumRun   = regexp.MustCompile(`[A-Za-z0-9]+`)
	dummyStart = []string{"your", "example", "placeholder", "changeme", "dummy", "redacted"}
	dummyEqual = []string{"token", "secret", "password"}
)

// shortCut is the longest part a "..." cut may show and still be an
// illustration: a short prefix shows a token's form, a long one with dots
// after it is most of a secret.
const shortCut = 12

// bearerOutcome judges the word after "bearer". The word is captured whole,
// so a wrapper that is no placeholder (`<generated value>`, `*value*`) is
// opened, and the leading run of token characters is judged: whatever
// follows it (`"}`, `</code>`, `|next`, `&x=1`) is the text around a pasted
// token, not part of the decision. Prose ("bearer of") is no token.
func bearerOutcome(raw string) docOutcome {
	if isPlaceholder(raw) {
		return docNone
	}
	run := tokenRun.FindString(strings.TrimLeft(raw, "\"'`(<[{*$%"))
	run = strings.TrimRight(run, ".") // a full stop ends the sentence, not the token
	switch {
	case len(run) < 16:
		if bearerShape.MatchString(raw) {
			return docWarn // the soft rule's shape, cut short by trailing punctuation
		}
		return docNone
	case standIn.MatchString(run):
		return docWarn
	}
	return docRefuse
}

func credentialOutcome(raw string) docOutcome {
	if isPlaceholder(raw) {
		return docNone
	}
	v := trimValue(raw)
	switch {
	case pathStart.MatchString(v) && !longMixed(v):
		return docWarn // a path made of names; base64 may start with / too
	case len(v) >= 20 && hasLetter.MatchString(v) && hasDigit.MatchString(v):
		return docRefuse
	}
	return docWarn
}

// trimValue strips the quoting and sentence punctuation around a value as
// it stands in prose or markdown.
func trimValue(v string) string {
	return strings.Trim(v, "\"'`.,;:()")
}

// isPlaceholder reports whether a value shows a form instead of a secret:
// a template, a mask, an obvious dummy, or a short prefix cut with dots.
func isPlaceholder(v string) bool {
	v = strings.Trim(v, "\"'`,;:()")
	// The cut first, while its dots are still there. It is a form only when
	// it ends the value after a short part: whatever follows the dots is
	// more of the value, and a long part before them is most of a secret.
	if strings.HasSuffix(v, "...") || strings.HasSuffix(v, "…") {
		if utf8.RuneCountInString(strings.TrimRight(v, ".…")) < shortCut {
			return true
		}
	}
	// Then the value without the sentence around it: `<token>`. ends a
	// sentence as often as it stands alone.
	v = strings.Trim(v, ".\"'`,;:()")
	if v == "" {
		return true
	}
	// A wrapper names a value; one around a generated-looking value is the
	// value itself in emphasis or brackets.
	if template.MatchString(v) || len(v) > 2 && strings.HasPrefix(v, "*") && strings.HasSuffix(v, "*") {
		return !longMixed(v)
	}
	if isMask(v) {
		return true
	}
	k := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(v))
	if slices.Contains(dummyEqual, k) {
		return true
	}
	for _, d := range dummyStart {
		if strings.HasPrefix(k, d) {
			return true
		}
	}
	return false
}

// longMixed reports a run of 12 or more letters and digits that holds both,
// the shape of a generated value; names such as DB_PASSWORD or api-key are
// not one.
func longMixed(v string) bool {
	for _, run := range alnumRun.FindAllString(v, -1) {
		if len(run) >= shortCut && hasLetter.MatchString(run) && hasDigit.MatchString(run) {
			return true
		}
	}
	return false
}

// isMask reports a value of one repeated character (xxxxxxxx, ********).
func isMask(v string) bool {
	if utf8.RuneCountInString(v) < 3 {
		return false
	}
	first, _ := utf8.DecodeRuneInString(v)
	return strings.Trim(v, string(first)) == ""
}

func appendOnce(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}
