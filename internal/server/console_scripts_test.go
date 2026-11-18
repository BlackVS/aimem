package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The console pages and the inline script each one must have. The checks
// below read the scripts as they are served; a page that loses its script
// fails here instead of passing by finding nothing.
var consolePages = []string{"admin.html", "tasks.html", "status.html"}

var scriptRE = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// pageScript returns the page's one inline script and the page line it
// starts on.
func pageScript(page string) (string, int, error) {
	if strings.Contains(page, "<script src") {
		return "", 0, fmt.Errorf("the page loads an external script; the checks read inline scripts only")
	}
	m := scriptRE.FindAllStringSubmatchIndex(page, -1)
	if len(m) != 1 {
		return "", 0, fmt.Errorf("the page has %d inline scripts, want 1", len(m))
	}
	if strings.TrimSpace(page[m[0][2]:m[0][3]]) == "" {
		return "", 0, fmt.Errorf("the page's script is empty")
	}
	return page[m[0][2]:m[0][3]], strings.Count(page[:m[0][2]], "\n") + 1, nil
}

func readConsolePage(t *testing.T, name string) (script string, line int) {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	script, line, err = pageScript(string(b))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return script, line
}

// TestConsoleScriptsSyntax runs node --check on each page's script. CI's
// lint job runs it with AIMEM_TEST_REQUIRE_NODE=1, where a missing node
// fails instead of skipping.
func TestConsoleScriptsSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("AIMEM_TEST_REQUIRE_NODE") != "" {
			t.Fatal("node is required here and not on PATH")
		}
		t.Skip("node is not on PATH")
	}
	dir := t.TempDir()
	for _, name := range consolePages {
		script, _ := readConsolePage(t, name)
		f := filepath.Join(dir, strings.TrimSuffix(name, ".html")+".js")
		if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Errorf("%s: node --check: %v\n%s", name, err, out)
		}
	}
}

func TestPageScriptRefusesMissingScript(t *testing.T) {
	for _, page := range []string{
		"<html><body>no script</body></html>",
		"<script></script>",
		"<script>a()</script><script>b()</script>",
		`<script src="x.js"></script><script>a()</script>`,
	} {
		if _, _, err := pageScript(page); err == nil {
			t.Errorf("pageScript(%q) took a page without exactly one inline script", page)
		}
	}
}

// ---- A lexer for the pages' JavaScript, enough to find template holes.

type jsTemplate struct {
	start  int
	parts  []string // the literal text around the holes
	holes  []jsHole
	parent *jsTemplate // the template whose hole holds this one
}

type jsHole struct {
	start int // offset of the hole's expression in the script
	expr  string
}

type jsLexer struct {
	s     string
	tpls  []*jsTemplate
	stack []*jsTemplate
}

// regexPrev holds the last significant characters after which a '/' starts
// a regular expression literal rather than a division.
const regexPrev = "(,=:[!&|?{};+-*%<>~^"

var regexKeywords = map[string]bool{"return": true, "typeof": true, "case": true, "in": true, "of": true,
	"new": true, "delete": true, "void": true, "throw": true, "else": true, "do": true, "yield": true, "await": true}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func (l *jsLexer) skipString(i int) int {
	q := l.s[i]
	for i++; i < len(l.s) && l.s[i] != q; i++ {
		if l.s[i] == '\\' {
			i++
		}
	}
	return i + 1
}

func (l *jsLexer) skipRegex(i int) int {
	class := false
	for i++; i < len(l.s); i++ {
		switch c := l.s[i]; {
		case c == '\\':
			i++
		case c == '[':
			class = true
		case c == ']':
			class = false
		case c == '/' && !class:
			i++
			for i < len(l.s) && isIdent(l.s[i]) {
				i++
			}
			return i
		}
	}
	return i
}

// code scans code from i. With stop it returns at the '}' that closes a
// template hole; otherwise at the end of the source.
func (l *jsLexer) code(i int, stop bool) int {
	depth, prev, word := 0, byte(0), ""
	for i < len(l.s) {
		c := l.s[i]
		switch {
		case strings.HasPrefix(l.s[i:], "//"):
			if j := strings.IndexByte(l.s[i:], '\n'); j >= 0 {
				i += j
			} else {
				i = len(l.s)
			}
			continue
		case strings.HasPrefix(l.s[i:], "/*"):
			if j := strings.Index(l.s[i+2:], "*/"); j >= 0 {
				i += j + 4
			} else {
				i = len(l.s)
			}
			continue
		case c == '\'' || c == '"':
			i, prev, word = l.skipString(i), 'a', ""
			continue
		case c == '`':
			i, prev, word = l.template(i), 'a', ""
			continue
		case c == '/' && (prev == 0 || strings.IndexByte(regexPrev, prev) >= 0 || regexKeywords[word]):
			i, prev, word = l.skipRegex(i), 'a', ""
			continue
		case c == '{':
			depth++
		case c == '}':
			if depth == 0 && stop {
				return i
			}
			depth--
		}
		if isIdent(c) {
			j := i
			for j < len(l.s) && isIdent(l.s[j]) {
				j++
			}
			word, prev, i = l.s[i:j], 'a', j
			continue
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			prev, word = c, ""
		}
		i++
	}
	return i
}

func (l *jsLexer) template(i int) int {
	t := &jsTemplate{start: i}
	if n := len(l.stack); n > 0 {
		t.parent = l.stack[n-1]
	}
	l.tpls = append(l.tpls, t)
	var text strings.Builder
	for i++; i < len(l.s) && l.s[i] != '`'; {
		switch {
		case l.s[i] == '\\':
			text.WriteString(l.s[i : i+2])
			i += 2
		case strings.HasPrefix(l.s[i:], "${"):
			t.parts = append(t.parts, text.String())
			text.Reset()
			l.stack = append(l.stack, t)
			end := l.code(i+2, true)
			l.stack = l.stack[:len(l.stack)-1]
			t.holes = append(t.holes, jsHole{start: i + 2, expr: l.s[i+2 : end]})
			i = end + 1
		default:
			text.WriteByte(l.s[i])
			i++
		}
	}
	t.parts = append(t.parts, text.String())
	return i + 1
}

func scanTemplates(src string) []*jsTemplate {
	l := &jsLexer{s: src}
	l.code(0, false)
	return l.tpls
}

// ---- The rule.

// htmlSinkRE: a template whose literal text holds markup builds HTML.
var htmlSinkRE = regexp.MustCompile(`<|="`)

// assignedHTMLRE: a template assigned straight to innerHTML builds HTML
// even without markup of its own.
var assignedHTMLRE = regexp.MustCompile(`(innerHTML|outerHTML)\s*\+?=\s*$|insertAdjacentHTML\([^,]*,\s*$`)

// inlineAttrRE finds an inline event handler attribute that is still open.
var inlineAttrRE = regexp.MustCompile(`\son[a-z]+\s*=\s*(["'])`)

// contextOf says whether text ends inside an inline handler attribute, where
// the browser decodes HTML entities before the script runs, so esc is not
// the escape.
func contextOf(text string) string {
	m := inlineAttrRE.FindAllStringSubmatchIndex(text, -1)
	if len(m) > 0 {
		last := m[len(m)-1]
		quote := text[last[2]:last[3]]
		if !strings.Contains(text[last[1]:], quote) {
			return ctxInline
		}
	}
	return ctxHTML
}

const (
	ctxHTML   = "html"
	ctxInline = "inline"
)

// pageHelpers are the functions of a page whose result may stand in HTML
// as it is. Each was read: it escapes what it puts into its result, or
// builds it only from the page's own templates, whose holes this check
// reads as well, or returns only fixed text or a formatted number. None
// is trusted in an inline handler.
var pageHelpers = map[string]map[string]string{
	"admin.html": {
		"esc":          "escapes & < > \" '",
		"md2html":      "escapes each block before it adds markup",
		"act":          "acts with one handler",
		"acts":         "data- attributes through esc(JSON.stringify(...))",
		"ago":          "a fixed word or Math.round of a time",
		"when":         "esc of a time stamp",
		"tok":          "formats a hub count",
		"bar":          "the page's own template",
		"modeBtn":      "the page's own template",
		"entry":        "the page's own templates",
		"editLink":     "the page's own template",
		"chapRow":      "the page's own template",
		"treeEntry":    "the page's own templates and mergeChooser",
		"section":      "the page's own template and treeEntry",
		"colTableHTML": "the page's own template",
		"colWikiHTML":  "the page's own templates",
		"fact":         "the page's own template",
	},
	"tasks.html": {
		"esc":            "escapes & < > \" '",
		"md2html":        "esc and inline before it adds markup",
		"inline":         "esc before it adds markup",
		"stChip":         "the page's own template",
		"refHTML":        "the page's own templates, or esc of a plain ref",
		"refsHTML":       "the page's own template",
		"fieldHTML":      "the page's own template",
		"taskURL":        "encodeURIComponent, which encodes \" < > &",
		"ago":            "a fixed word or Math.round of a time",
		"v":              "esc of a form field",
		"lines":          "esc of a form list",
		"stateChipsHTML": "the page's own templates",
	},
}

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

// numberNames are fields the hub fills with numbers, and the page's own
// counters and indexes.
var numberRE = regexp.MustCompile(`^[A-Za-z_$][\w$]*(\??\.[A-Za-z_$][\w$]*|\[[\w$.'"]*\])*$`)

var numberNames = set("rev", "revision", "sequence", "length", "size", "count", "total",
	"memories", "events", "records", "tokens", "cpus", "filed", "latency_ms", "calls_today",
	"daily_calls_cap", "against_rev", "conflicts", "corroboration",
	// a chapter proposal's counts (internal/curate/refile.go), in a meta
	// key no client writes
	"unfiled", "considered")

func isNumberPath(e string) bool {
	if !numberRE.MatchString(e) {
		return false
	}
	last := e
	if i := strings.LastIndexAny(e, ".]"); i >= 0 {
		last = e[i+1:]
	}
	return numberNames[last] || strings.HasSuffix(last, "_mb")
}

var literalRE = regexp.MustCompile(`^(-?\d+(\.\d+)?|true|false|null|undefined|'(\\.|[^'\\])*'|"(\\.|[^"\\])*")$`)

// topLevel calls fn for each character of e outside brackets, strings,
// templates and regular expressions, with its offset.
func topLevel(e string, fn func(i int) bool) {
	l := &jsLexer{s: e}
	depth, prev := 0, byte(0)
	for i := 0; i < len(e); {
		c := e[i]
		switch {
		case c == '\'' || c == '"':
			i, prev = l.skipString(i), 'a'
			continue
		case c == '`':
			i, prev = l.template(i), 'a'
			continue
		case c == '/' && (prev == 0 || strings.IndexByte(regexPrev, prev) >= 0):
			i, prev = l.skipRegex(i), 'a'
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		default:
			if depth == 0 && !fn(i) {
				return
			}
		}
		if c != ' ' && c != '\n' && c != '\t' && c != '\r' {
			prev = c
			if isIdent(c) {
				prev = 'a'
			}
		}
		i++
	}
}

// matchClose returns the offset of the bracket that closes the one at
// open, or -1.
func matchClose(e string, open int) int {
	l := &jsLexer{s: e}
	depth := 0
	for i := open; i < len(e); {
		switch e[i] {
		case '\'', '"':
			i = l.skipString(i)
			continue
		case '`':
			i = l.template(i)
			continue
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

// closes reports whether the bracket at open is closed by the last
// character of e.
func closes(e string, open int) bool { return matchClose(e, open) == len(e)-1 }

type holeFault struct {
	expr, ctx string
	at        int // offset in the script
}

type ruleChecker struct {
	page string
}

var toFixedRE = regexp.MustCompile(`\.toFixed\(\d\)$`)
var callRE = regexp.MustCompile(`^([A-Za-z_$][\w$]*)\(`)
var mapJoinRE = regexp.MustCompile(`\.map\(([\s\S]*)\)\.join\(('[^'\\]*'|"[^"\\]*")\)$`)
var arrowRE = regexp.MustCompile(`^(\(?[\w$, \[\]{}]*\)?)\s*=>\s*([\s\S]*)$`)

// check returns the parts of expression e (at offset at) that the rule
// does not allow in context ctx.
func (c ruleChecker) check(e string, at int, ctx string) []holeFault {
	lead := len(e) - len(strings.TrimLeft(e, " \t\r\n"))
	e, at = strings.TrimSpace(e), at+lead
	if e == "" {
		return nil
	}
	if e[0] == '(' && closes(e, 0) {
		return c.check(e[1:len(e)-1], at+1, ctx)
	}
	if literalRE.MatchString(e) || isNumberPath(e) {
		return nil
	}
	if e[0] == '`' && closes("("+e+")", 0) && len(scanTemplates(e)) > 0 {
		return c.template(e, at, ctx)
	}
	// cond ? a : b
	q, colon, nest := -1, -1, 0
	topLevel(e, func(i int) bool {
		switch {
		case e[i] == '?' && i+1 < len(e) && (e[i+1] == '.' || e[i+1] == '?'):
		case e[i] == '?' && i > 0 && e[i-1] == '?':
		case e[i] == '?':
			if q < 0 {
				q = i
			} else {
				nest++
			}
		case e[i] == ':' && q >= 0:
			if nest == 0 {
				colon = i
				return false
			}
			nest--
		}
		return true
	})
	if q >= 0 && colon > q {
		return append(c.check(e[q+1:colon], at+q+1, ctx), c.check(e[colon+1:], at+colon+1, ctx)...)
	}
	// a || b, a ?? b: either value may be printed. a && b: a is printed only
	// when falsy, which prints nothing an agent wrote.
	for _, op := range []string{"||", "??", "&&"} {
		if parts, offs := splitTop(e, op); len(parts) > 1 {
			var out []holeFault
			for k, p := range parts {
				if op == "&&" && k < len(parts)-1 {
					continue
				}
				out = append(out, c.check(p, at+offs[k], ctx)...)
			}
			return out
		}
	}
	// Unary plus and minus, and binary - * %, give a number.
	if (e[0] == '+' || e[0] == '-') && !strings.HasPrefix(e, "++") && !strings.HasPrefix(e, "--") {
		return nil
	}
	for _, op := range []string{"-", "*", "%"} {
		if parts, _ := splitTop(e, op); len(parts) > 1 {
			return nil
		}
	}
	if parts, offs := splitTop(e, "+"); len(parts) > 1 {
		var out []holeFault
		for k, p := range parts {
			out = append(out, c.check(p, at+offs[k], ctx)...)
		}
		return out
	}
	// list.map(x => template).join('')
	if m := mapJoinRE.FindStringSubmatchIndex(e); m != nil && matchClose(e, m[2]-1) == m[3] {
		arg := e[m[2]:m[3]]
		if a := arrowRE.FindStringSubmatchIndex(arg); a != nil && !strings.HasPrefix(strings.TrimSpace(arg[a[4]:a[5]]), "{") {
			return c.check(arg[a[4]:a[5]], at+m[2]+a[4], ctx)
		}
		if _, ok := pageHelpers[c.page][strings.TrimSpace(arg)]; ok && ctx == ctxHTML {
			return nil
		}
	}
	if m := callRE.FindStringSubmatch(e); m != nil && closes(e, len(m[0])-1) {
		if _, ok := pageHelpers[c.page][m[1]]; ok && ctx == ctxHTML {
			return nil
		}
		if m[1] == "Math" {
			return nil
		}
	}
	if strings.HasSuffix(e, ".length") || strings.HasPrefix(e, "Math.") || toFixedRE.MatchString(e) {
		return nil
	}
	return []holeFault{{expr: strings.Join(strings.Fields(e), " "), ctx: ctx, at: at}}
}

// splitTop splits e at top-level occurrences of a binary operator.
func splitTop(e, op string) (parts []string, offs []int) {
	start := 0
	topLevel(e, func(i int) bool {
		if !strings.HasPrefix(e[i:], op) {
			return true
		}
		if op == "+" || op == "-" || op == "*" || op == "%" {
			// a binary operator: not doubled, not an assignment, not unary
			if len(e) > i+1 && (e[i+1] == op[0] || e[i+1] == '=') || i > 0 && e[i-1] == op[0] {
				return true
			}
			j := strings.TrimRight(e[:i], " \t\r\n")
			if j == "" || strings.IndexByte(regexPrev, j[len(j)-1]) >= 0 {
				return true
			}
		}
		if op == "||" && i > 0 && e[i-1] == '|' {
			return true
		}
		if op == "??" && (i > 0 && e[i-1] == '?' || i+2 < len(e) && e[i+2] == '.') {
			return true
		}
		parts, offs = append(parts, e[start:i]), append(offs, start)
		start = i + len(op)
		return true
	})
	if parts == nil {
		return []string{e}, []int{0}
	}
	return append(parts, e[start:]), append(offs, start)
}

// template checks the holes of the template literal e, one whole template.
func (c ruleChecker) template(e string, at int, ctx string) []holeFault {
	t := scanTemplates(e)[0]
	var out []holeFault
	var text strings.Builder
	for k, h := range t.holes {
		text.WriteString(t.parts[k])
		hctx := ctx
		if hctx == ctxHTML {
			hctx = contextOf(text.String())
		}
		out = append(out, c.check(h.expr, at+h.start, hctx)...)
		text.WriteString("0")
	}
	return out
}

// pageHoles is the result of reading a page: every hole, those in templates
// that build HTML, and the faults the rule leaves.
type pageHoles struct {
	all, judged int
	faults      []holeFault
}

func checkPage(page, script string) pageHoles {
	tpls := scanTemplates(script)
	var r pageHoles
	sink := map[*jsTemplate]bool{}
	for _, t := range tpls {
		r.all += len(t.holes)
		before := strings.TrimRight(script[:t.start], " \t\r\n")
		sink[t] = htmlSinkRE.MatchString(strings.Join(t.parts, "")) || assignedHTMLRE.MatchString(before)
	}
	c := ruleChecker{page: page}
	for _, t := range tpls {
		if !sink[t] {
			continue
		}
		// A template inside a hole of a template that builds HTML is
		// judged through that hole.
		root := true
		for p := t.parent; p != nil; p = p.parent {
			if sink[p] {
				root = false
				break
			}
		}
		if !root {
			continue
		}
		var text strings.Builder
		for k, h := range t.holes {
			text.WriteString(t.parts[k])
			r.judged++
			r.faults = append(r.faults, c.check(h.expr, h.start, contextOf(text.String()))...)
			text.WriteString("0")
		}
	}
	return r
}

// ---- The reviewed list.

// holeEntry is one distinct hole the rule does not allow, reviewed and
// found safe. N is how many times it occurs on the page, so a new
// occurrence of the same expression is reviewed again. An inline entry
// names its ground, one of inlineGrounds.
type holeEntry struct {
	page, ctx, expr string
	n               int
	ground          string // inline entries only
	why             string
}

// inlineGrounds are the kinds of value allowed in an inline handler (task
// 01a1cf05, comment seq 582): each is validated before it reaches the store
// or never comes from the store.
var inlineGrounds = set(
	"hub-assigned id", // uuidv7.New() in the store; no client sets one
	"dependency id",   // TaskContent.validate checks taskIDRE on every write
	"gated ref",       // the page links a ref only when it matches its id regex
	"fixed enum",      // a value from the page's own constant list
	"json literal",    // JSON.stringify of the user's own unsaved edit
	"page constant",   // a value the page's code sets, never from the hub
)

func entryKey(page, ctx, expr string) string { return page + "\x00" + ctx + "\x00" + expr }

// compareWithList returns the faults the list does not cover, and the
// entries that no longer match the page.
func compareWithList(page string, faults []holeFault, script string, scriptLine int, list []holeEntry) (problems []string) {
	got := map[string][]holeFault{}
	for _, f := range faults {
		k := entryKey(page, f.ctx, f.expr)
		got[k] = append(got[k], f)
	}
	want := map[string]holeEntry{}
	for _, e := range list {
		if e.page != page {
			continue
		}
		k := entryKey(e.page, e.ctx, e.expr)
		if _, dup := want[k]; dup {
			problems = append(problems, fmt.Sprintf("%s: the list has %q (%s) twice", page, e.expr, e.ctx))
		}
		if e.ctx == ctxInline && !inlineGrounds[e.ground] {
			problems = append(problems, fmt.Sprintf("%s: inline entry %q has no ground from the fixed set", page, e.expr))
		}
		if strings.TrimSpace(e.why) == "" {
			problems = append(problems, fmt.Sprintf("%s: entry %q has no reason", page, e.expr))
		}
		want[k] = e
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fs := got[k]
		e, ok := want[k]
		switch {
		case !ok:
			var lines []string
			for _, f := range fs {
				lines = append(lines, fmt.Sprint(scriptLine+strings.Count(script[:f.at], "\n")))
			}
			problems = append(problems, fmt.Sprintf("%s:%s: %s hole %q is neither escaped by the rule nor on the reviewed list",
				page, strings.Join(lines, ","), fs[0].ctx, fs[0].expr))
		case e.n != len(fs):
			problems = append(problems, fmt.Sprintf("%s: %s hole %q occurs %d times, the list says %d: review the new place",
				page, fs[0].ctx, fs[0].expr, len(fs), e.n))
		}
	}
	for k, e := range want {
		if _, ok := got[k]; !ok {
			problems = append(problems, fmt.Sprintf("%s: list entry %q (%s) matches no hole: remove it", page, e.expr, e.ctx))
		}
	}
	sort.Strings(problems)
	return problems
}

// TestConsoleTemplateHoles reads every template hole of the console pages
// and fails on one the rule does not allow and the reviewed list does not
// hold, and on a list entry that matches nothing.
func TestConsoleTemplateHoles(t *testing.T) {
	for _, name := range consolePages {
		script, line := readConsolePage(t, name)
		r := checkPage(name, script)
		listed := 0
		for _, e := range reviewedHoles {
			if e.page == name {
				listed += e.n
			}
		}
		t.Logf("%s: %d holes, %d in templates that build HTML, %d left by the rule, %d occurrences on the list",
			name, r.all, r.judged, len(r.faults), listed)
		for _, p := range compareWithList(name, r.faults, script, line, reviewedHoles) {
			t.Error(p)
		}
	}
}

// The rule on small pages, each shown to fail on what it must refuse.
func TestConsoleHoleRule(t *testing.T) {
	const page = "tasks.html"
	cases := []struct {
		name, script string
		faults       []string
	}{
		{"escaped", "el.innerHTML = `<b>${esc(t.title)}</b>`", nil},
		{"bare agent text", "el.innerHTML = `<b>${t.title}</b>`", []string{"html t.title"}},
		{"escaped made bare in a branch", "x = `<i>${ok ? esc(t.body) : t.body}</i>`", []string{"html t.body"}},
		{"number", "x = `<span>${t.revision}</span>`", nil},
		{"assigned without markup", "el.innerHTML = `${t.title}`", []string{"html t.title"}},
		{"not HTML", "toast(`saved ${t.title}`)", nil},
		{"map of a template", "x = `<ul>${xs.map(x => `<li>${x.name}</li>`).join('')}</ul>`", []string{"html x.name"}},
		{"concatenation", "x = `<p>${esc(a) + b}</p>`", []string{"html b"}},
		{"and", "x = `<p>${a && `<b>${esc(a)}</b>`}</p>`", nil},
		{"or", "x = `<p>${a || t.title}</p>`", []string{"html a", "html t.title"}},
		{"esc in an inline handler", "x = `<a onclick=\"go('${esc(t.title)}')\">x</a>`", []string{"inline esc(t.title)"}},
		{"after a closed handler", "x = `<a onclick=\"go()\" title=\"${esc(t.title)}\">x</a>`", nil},
		{"inline inherited by a nested template", "x = `<a onclick=\"${c ? `go('${esc(t.id)}')` : ''}\">x</a>`", []string{"inline esc(t.id)"}},
		{"helper of another page", "x = `<p>${bar(t.title)}</p>`", []string{"html bar(t.title)"}},
		{"helper with more after it", "x = `<p>${esc(a)}${esc(a).b}</p>`", []string{"html esc(a).b"}},
	}
	for _, tc := range cases {
		r := checkPage(page, tc.script)
		var got []string
		for _, f := range r.faults {
			got = append(got, f.ctx+" "+f.expr)
		}
		if strings.Join(got, "|") != strings.Join(tc.faults, "|") {
			t.Errorf("%s: faults %q, want %q", tc.name, got, tc.faults)
		}
	}
}

func TestReviewedListRefusesDeadAndCountedEntries(t *testing.T) {
	script := "x = `<p>${kind}</p><p>${kind}</p>`"
	faults := checkPage("p.html", script).faults
	ok := []holeEntry{{page: "p.html", ctx: ctxHTML, expr: "kind", n: 2, why: "built from esc above"}}
	if p := compareWithList("p.html", faults, script, 1, ok); len(p) != 0 {
		t.Fatalf("a matching list: %q", p)
	}
	for name, list := range map[string][]holeEntry{
		"count":   {{page: "p.html", ctx: ctxHTML, expr: "kind", n: 1, why: "x"}},
		"dead":    append(ok[:1:1], holeEntry{page: "p.html", ctx: ctxHTML, expr: "gone", n: 1, why: "x"}),
		"missing": nil,
		"reason":  {{page: "p.html", ctx: ctxHTML, expr: "kind", n: 2}},
		"ground":  {{page: "p.html", ctx: ctxHTML, expr: "kind", n: 2, why: "x"}, {page: "p.html", ctx: ctxInline, expr: "s", n: 1, why: "x"}},
	} {
		if p := compareWithList("p.html", faults, script, 1, list); len(p) == 0 {
			t.Errorf("%s: the list was accepted", name)
		}
	}
}
