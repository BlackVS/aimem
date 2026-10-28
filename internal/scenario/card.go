//go:build scenario

// Package scenario runs the documented usage scenarios, the cards in
// docs/scenarios, against the real aimem binary started as a hub and as a
// workstation. docs/scenarios/README.md is the card convention this parser
// reads.
package scenario

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Card is one parsed scenario card.
type Card struct {
	Path  string
	Steps []Step
}

// Step is one numbered step: the command runs on Side, or the request is
// sent to the hub, and every Expect line must appear in its output.
type Step struct {
	N      int
	Title  string
	Side   string // "hub" or "workstation"
	Reason string
	Line   int      // the card line of the command or request
	Args   []string // argv, placeholders substituted; Args[0] is "aimem"
	HTTP   *Request // set instead of Args for an http step
	Expect []Expect
}

// Request is an http step: one request to the hub's HTTPS API, its bearer
// token read from TokenFile when the step runs.
type Request struct {
	Method    string
	Path      string // starts with /v1/
	Header    [][2]string
	TokenFile string
	Body      string // JSON, or empty
}

// Expect is one expected output line and the card line it is on.
type Expect struct {
	Line int
	Text string
}

var (
	stepHeading = regexp.MustCompile(`^### (\d+)\. (.+) \((hub|workstation)\)$`)
	placeholder = regexp.MustCompile(`<[a-z][a-z0-9-]*>`)
	// shellSyntax is what a person's shell would interpret and the runner,
	// which takes an argv, would not.
	shellSyntax = regexp.MustCompile("[|&;<>$`\"'*?(){}\\\\]")
)

// ParseCard reads a card and substitutes vars (placeholder name without
// the angle brackets → value) into its commands and expected lines. Every
// error names the card file and line.
func ParseCard(path string, vars map[string]string) (*Card, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Card{Path: path}
	var st *Step
	block := "" // the fenced block being read: "sh", "http", "text" or "other"
	var cmd []string
	cmdLine := 0
	fenceLine := 0 // the line of the open block's fence
	errf := func(n int, format string, a ...any) error {
		return fmt.Errorf("%s:%d: %s", path, n, fmt.Sprintf(format, a...))
	}
	for i, raw := range strings.Split(string(b), "\n") {
		n := i + 1
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if block != "" {
			if trimmed != "```" {
				switch block {
				case "sh", "http":
					cmd = append(cmd, line)
					if cmdLine == 0 {
						cmdLine = n
					}
				case "text":
					if trimmed != "" {
						text, err := substitute(trimmed, vars)
						if err != nil {
							return nil, errf(n, "%v", err)
						}
						st.Expect = append(st.Expect, Expect{n, text})
					}
				}
				continue
			}
			switch block {
			case "sh":
				args, err := argv(cmd, vars)
				if err != nil {
					return nil, errf(cmdLine, "step %d: %v", st.N, err)
				}
				st.Args, st.Line = args, cmdLine
			case "http":
				req, err := request(cmd, vars)
				if err != nil {
					return nil, errf(cmdLine, "step %d: %v", st.N, err)
				}
				st.HTTP, st.Line = req, cmdLine
			}
			block = ""
			continue
		}
		if m := stepHeading.FindStringSubmatch(line); m != nil {
			if err := complete(st, path); err != nil {
				return nil, err
			}
			num, _ := strconv.Atoi(m[1])
			if num != len(c.Steps)+1 {
				return nil, errf(n, "step %d follows step %d", num, len(c.Steps))
			}
			c.Steps = append(c.Steps, Step{N: num, Title: m[2], Side: m[3], Line: n})
			st = &c.Steps[len(c.Steps)-1]
			continue
		}
		if strings.HasPrefix(line, "#") {
			if err := complete(st, path); err != nil {
				return nil, err
			}
			if strings.HasPrefix(line, "### ") {
				return nil, errf(n, "a step heading is \"### N. <title> (hub|workstation)\"")
			}
			st = nil
			continue
		}
		if st == nil {
			if strings.HasPrefix(trimmed, "```") {
				block, fenceLine = "other", n
			}
			continue
		}
		switch {
		case trimmed == "```sh", trimmed == "```http":
			if st.Reason == "" || st.Args != nil || st.HTTP != nil {
				return nil, errf(n, "step %d: the reason line, then one sh or http block, then one text block", st.N)
			}
			block, cmd, cmdLine, fenceLine = strings.TrimPrefix(trimmed, "```"), nil, 0, n
		case trimmed == "```text":
			if (st.Args == nil && st.HTTP == nil) || st.Expect != nil {
				return nil, errf(n, "step %d: the text block follows the sh or http block, once", st.N)
			}
			block, fenceLine = "text", n
		case strings.HasPrefix(trimmed, "```"):
			return nil, errf(n, "step %d: a step holds only an sh or http block and a text block", st.N)
		case trimmed != "" && st.Reason == "":
			st.Reason = trimmed
		case trimmed != "":
			return nil, errf(n, "step %d: one reason line, then the sh or http block", st.N)
		}
	}
	if block != "" {
		return nil, errf(fenceLine, "the fenced block opened here is not closed")
	}
	if err := complete(st, path); err != nil {
		return nil, err
	}
	if len(c.Steps) == 0 {
		return nil, fmt.Errorf("%s: no steps", path)
	}
	return c, nil
}

// complete checks that a finished step has everything a step needs.
func complete(st *Step, path string) error {
	if st == nil {
		return nil
	}
	if st.Reason == "" || (st.Args == nil && st.HTTP == nil) || len(st.Expect) == 0 {
		return fmt.Errorf("%s:%d: step %d needs a reason line, an sh or http block and a text block with at least one line", path, st.Line, st.N)
	}
	return nil
}

// argv joins continuation lines (a trailing backslash), splits the one
// command into words and substitutes the placeholders. There is no shell:
// quoting, pipes, redirections and variables are refused, because the
// runner could not do what a person's shell would.
func argv(lines []string, vars map[string]string) ([]string, error) {
	var words []string
	for i, l := range lines {
		l = strings.TrimSpace(l)
		cont := strings.HasSuffix(l, `\`)
		if cont {
			l = strings.TrimSuffix(l, `\`)
		}
		words = append(words, strings.Fields(l)...)
		if !cont && i != len(lines)-1 {
			return nil, fmt.Errorf("the sh block holds more than one command")
		}
	}
	if len(words) == 0 || words[0] != "aimem" {
		return nil, fmt.Errorf("the command must be an aimem command")
	}
	for i, w := range words {
		bad := shellSyntax.FindString(placeholder.ReplaceAllString(w, ""))
		if bad == "" && (strings.HasPrefix(w, "~") || strings.HasPrefix(w, "#")) {
			bad = w[:1] // a home directory or a comment, at the start of a word
		}
		if bad != "" {
			return nil, fmt.Errorf("%q: %q is shell syntax, and the runner runs no shell", w, bad)
		}
		v, err := substitute(w, vars)
		if err != nil {
			return nil, err
		}
		words[i] = v
	}
	return words, nil
}

// substitute replaces every <name> in s by vars[name]; an unknown name is
// an error, so a card cannot run a literal placeholder.
func substitute(s string, vars map[string]string) (string, error) {
	var bad string
	out := placeholder.ReplaceAllStringFunc(s, func(p string) string {
		v, ok := vars[p[1:len(p)-1]]
		if !ok && bad == "" {
			bad = p
		}
		return v
	})
	if bad != "" {
		return "", fmt.Errorf("unknown placeholder %s", bad)
	}
	return out, nil
}

var (
	requestLine = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE) (/v1/\S*)$`)
	headerLine  = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*): (\S.*)$`)
	// httpUnsafe is what would break the request's curl form, where each
	// header and the body are single-quoted.
	httpUnsafe = regexp.MustCompile("['$`\\\\]")
)

// request parses an http block: the request line `METHOD /v1/...`, header
// lines, then an optional blank line and a JSON body. The header
// `Token-File: <placeholder>` names the file whose token the runner sends
// as `Authorization: Bearer`; it is required and not sent itself.
// Placeholders are substituted everywhere, the card's own text may not
// hold what the curl form would have the shell interpret, and the body
// must be JSON.
func request(lines []string, vars map[string]string) (*Request, error) {
	for i, l := range lines {
		if bad := httpUnsafe.FindString(placeholder.ReplaceAllString(l, "")); bad != "" {
			return nil, fmt.Errorf("line %d of the http block: %q is shell syntax in the request's curl form", i+1, bad)
		}
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("the http block is empty")
	}
	m := requestLine.FindStringSubmatch(strings.TrimSpace(lines[0]))
	if m == nil {
		return nil, fmt.Errorf("an http block starts with `METHOD /v1/...`")
	}
	path, err := substitute(m[2], vars)
	if err != nil {
		return nil, err
	}
	req := &Request{Method: m[1], Path: path}
	rest := lines[1:]
	for len(rest) > 0 && strings.TrimSpace(rest[0]) != "" {
		h := headerLine.FindStringSubmatch(strings.TrimSpace(rest[0]))
		if h == nil {
			return nil, fmt.Errorf("%q is not a header line `Name: value`", strings.TrimSpace(rest[0]))
		}
		rest = rest[1:]
		if strings.EqualFold(h[1], "Token-File") {
			if placeholder.FindString(h[2]) != h[2] || req.TokenFile != "" {
				return nil, fmt.Errorf("Token-File names one placeholder, once")
			}
			if req.TokenFile, err = substitute(h[2], vars); err != nil {
				return nil, err
			}
			continue
		}
		if strings.EqualFold(h[1], "Authorization") {
			return nil, fmt.Errorf("the bearer comes from Token-File, not an Authorization line")
		}
		v, err := substitute(h[2], vars)
		if err != nil {
			return nil, err
		}
		req.Header = append(req.Header, [2]string{h[1], v})
	}
	if req.TokenFile == "" {
		return nil, fmt.Errorf("an http block needs a Token-File header")
	}
	body, err := substitute(strings.TrimSpace(strings.Join(rest, "\n")), vars)
	if err != nil {
		return nil, err
	}
	if body != "" && !json.Valid([]byte(body)) {
		return nil, fmt.Errorf("the request body is not JSON")
	}
	req.Body = body
	return req, nil
}
