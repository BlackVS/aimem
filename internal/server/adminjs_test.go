package server

import (
	"strings"
	"testing"
)

// scanJSStrings walks the admin page's inline script and reports every line
// where a single- or double-quoted string literal is still open at end of
// line.
//
// This exists because that exact mistake shipped once. A tool rewriting the
// page turned "\n" escapes into real newlines inside string literals, which
// is a JavaScript SyntaxError — and a SyntaxError does not break one
// function, it discards the ENTIRE script block. The console then rendered
// as dead chrome: the token gate accepted a paste, the Connect button did
// nothing, and no page function existed at all. Nothing caught it. The Go
// build passed (the page is an opaque []byte), the Go tests passed, and
// grepping the deployed page for the new markup found it present and
// correct. Comments, template literals (which may legally span lines) and
// regex literals are skipped.
func scanJSStrings(script string) []int {
	var bad []int
	line := 1
	var quote byte // 0, or the quote character when inside a string
	inTmpl, inLine, inBlock := false, false, false
	prevSig := byte(0) // last significant char, to tell regex from division
	for i := 0; i < len(script); i++ {
		c := script[i]
		if c == '\n' {
			if quote != 0 {
				bad = append(bad, line)
				quote = 0 // resync, so one mistake does not cascade
			}
			line++
			inLine = false
			continue
		}
		switch {
		case inLine:
		case inBlock:
			if c == '*' && i+1 < len(script) && script[i+1] == '/' {
				inBlock = false
				i++
			}
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case inTmpl:
			if c == '\\' {
				i++
			} else if c == '`' {
				inTmpl = false
			}
		case c == '/' && i+1 < len(script) && script[i+1] == '/':
			inLine = true
			i++
		case c == '/' && i+1 < len(script) && script[i+1] == '*':
			inBlock = true
			i++
		case c == '/' && regexCanStart(prevSig):
			// Regex literal: skip to its unescaped closing slash. A regex
			// cannot span a line, so stop at one rather than mis-consume
			// the rest of the file.
			for i++; i < len(script) && script[i] != '\n'; i++ {
				if script[i] == '\\' {
					i++
					continue
				}
				if script[i] == '[' { // a class may contain an unescaped /
					for i++; i < len(script) && script[i] != ']' && script[i] != '\n'; i++ {
						if script[i] == '\\' {
							i++
						}
					}
					continue
				}
				if script[i] == '/' {
					break
				}
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '`':
			inTmpl = true
		}
		if c != ' ' && c != '\t' {
			prevSig = c
		}
	}
	return bad
}

// regexCanStart reports whether a slash following prev opens a regex literal
// rather than being division. Anything that can END an expression — an
// identifier, a digit, a closing bracket — means division.
func regexCanStart(prev byte) bool {
	switch {
	case prev == 0:
		return true
	case prev >= 'a' && prev <= 'z', prev >= 'A' && prev <= 'Z',
		prev >= '0' && prev <= '9',
		prev == '_', prev == '$', prev == ')', prev == ']':
		return false
	}
	return true
}

// TestAdminPageScriptParses is the regression guard for the shipped
// SyntaxError described on scanJSStrings.
func TestAdminPageScriptParses(t *testing.T) {
	page := string(adminHTML)
	open := strings.Index(page, "<script>")
	closeAt := strings.LastIndex(page, "</script>")
	if open < 0 || closeAt < open {
		t.Fatal("admin.html has no <script> block")
	}
	if bad := scanJSStrings(page[open+len("<script>") : closeAt]); len(bad) != 0 {
		t.Errorf("unterminated string literal at script line(s) %v: a raw "+
			"newline inside a JS string is a SyntaxError that discards the "+
			"whole script block and leaves the console unresponsive", bad)
	}
}

// TestScanJSStrings pins the detector itself. The last case is verbatim the
// shape that shipped.
func TestScanJSStrings(t *testing.T) {
	clean := []string{
		`const a = "one" + 'two';`,
		"const t = `spans\nlines`;",
		`if (s.replace(/[&<>"]/g, c => c)) {}`,
		`// a comment with an apostrophe: doesn't count`,
		`/* block ' comment */ const x = 1;`,
		`const r = a / b; const s = "fine";`,
		`const id = id.replace(/-[0-9a-f]{12}$/, "");`,
	}
	for _, s := range clean {
		if bad := scanJSStrings(s); len(bad) != 0 {
			t.Errorf("false positive on %q: lines %v", s, bad)
		}
	}
	broken := "const msg = prompt(\n    \"New id for this project.\n\n\"+\n    \"more\");"
	if bad := scanJSStrings(broken); len(bad) == 0 {
		t.Error("detector missed a raw newline inside a string literal")
	}
}
