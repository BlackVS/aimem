package docs_test

// TestDocExamples checks the names the documentation's examples use against
// the code: every `aimem` command word and flag, every /v1/ route and every
// MCP tool named as one. docs/DEVELOPMENT.md ("Examples in the
// documentation") states the rule, the sources and the exclusions.

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"aimem/internal/mcp"
)

// skipMarker exempts the line it is on, for prose that names something on
// purpose, such as a command that does not exist.
const skipMarker = "<!-- doc-check: skip -->"

// foreignRoutes are /v1/ routes of other services the docs talk about: an
// OpenAI-compatible model proxy and aicrew.
var foreignRoutes = []string{"/v1/models", "/v1/chat/", "/v1/embeddings", "/v1/crew/"}

// checkedDoc reports whether a Markdown file is an operational document the
// check covers. Designs, proposals and probe reports describe what is
// proposed or was corrected; the archive, the roadmap and the changelog
// are records of the past.
func checkedDoc(rel string) bool {
	base := filepath.Base(rel)
	switch {
	case strings.HasPrefix(rel, "archive/"), base == "ROADMAP.md":
		return false
	case strings.HasPrefix(base, "DESIGN"), strings.HasSuffix(base, "-PROPOSAL.md"), strings.HasSuffix(base, "-PROBE.md"):
		return false
	}
	return strings.HasSuffix(base, ".md")
}

type vocabulary struct {
	top, words, flags map[string]bool
	routes            []*regexp.Regexp
	routePaths        []string
	tools             map[string]bool
}

func TestDocExamples(t *testing.T) {
	v := loadVocabulary(t)
	files := []string{"../README.md"}
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && checkedDoc(filepath.ToSlash(p)) {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, f := range files {
		problems = append(problems, checkFile(t, f, v)...)
	}
	if len(problems) > 0 {
		t.Errorf("%d stale name(s) in documentation examples:\n%s\n\nFix the example to the current name, or, for prose that names something on purpose, end the line with %s. See docs/DEVELOPMENT.md.",
			len(problems), strings.Join(problems, "\n"), skipMarker)
	}
}

func checkFile(t *testing.T, f string, v vocabulary) []string {
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.ToSlash(filepath.Join("docs", f))
	if f == "../README.md" {
		name = "README.md"
	}
	var out []string
	fenced := false
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if strings.Contains(line, skipMarker) {
			continue
		}
		at := fmt.Sprintf("%s:%d", name, i+1)
		spans := []string{line}
		if !fenced {
			spans = nil
			for _, m := range inlineCode.FindAllStringSubmatch(line, -1) {
				spans = append(spans, m[1])
			}
			for _, m := range toolMention.FindAllStringSubmatch(line, -1) {
				for _, c := range inlineCode.FindAllStringSubmatch(m[1], -1) {
					if !v.tools[c[1]] {
						out = append(out, fmt.Sprintf("%s: MCP tool %q (nearest: %s)", at, c[1], nearest(c[1], keys(v.tools))))
					}
				}
			}
		}
		for _, s := range spans {
			out = append(out, checkCommands(at, s, v)...)
			out = append(out, checkRoutes(at, s, v)...)
			for _, m := range prefixedTool.FindAllStringSubmatch(s, -1) {
				if !v.tools[m[1]] {
					out = append(out, fmt.Sprintf("%s: MCP tool %q (nearest: %s)", at, m[1], nearest(m[1], keys(v.tools))))
				}
			}
		}
	}
	return out
}

var (
	inlineCode   = regexp.MustCompile("`([^`]+)`")
	toolMention  = regexp.MustCompile("((?:`[a-z]+(?:_[a-z]+)+`(?:,? (?:and|or) |, ))*`[a-z]+(?:_[a-z]+)+`) (?:MCP )?tools?\\b")
	prefixedTool = regexp.MustCompile(`mcp__aimem__([a-z_]+)`)
	segmentSep   = regexp.MustCompile(`\s*(?:\|\||\||&&|;|\$\()\s*`)
	invocation   = regexp.MustCompile(`^(?:\S*/)?aimem(?:\.exe)?\s+(.*)$`)
	commandWord  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	flagToken    = regexp.MustCompile(`^--?([a-z][a-z0-9-]*)`)
	placeholder  = regexp.MustCompile(`<[^>]+>`)
	routeMention = regexp.MustCompile(`(/v1/[A-Za-z0-9_{}<>.\-/]*)`)
)

func checkCommands(at, s string, v vocabulary) []string {
	var out []string
	for _, seg := range segmentSep.Split(strings.TrimPrefix(strings.TrimSpace(s), "$ "), -1) {
		m := invocation.FindStringSubmatch(strings.TrimSpace(seg))
		if m == nil {
			continue
		}
		toks := strings.Fields(m[1])
		if len(toks) == 0 || !commandWord.MatchString(toks[0]) {
			continue
		}
		if !v.top[toks[0]] {
			out = append(out, fmt.Sprintf("%s: command \"aimem %s\" (nearest: %s)", at, toks[0], nearest(toks[0], keys(v.top))))
			continue
		}
		if len(toks) > 1 && commandWord.MatchString(toks[1]) && !v.words[toks[1]] {
			out = append(out, fmt.Sprintf("%s: subcommand \"aimem %s %s\" (nearest: %s)", at, toks[0], toks[1], nearest(toks[1], keys(v.words))))
		}
		for _, tok := range toks[1:] {
			if fm := flagToken.FindStringSubmatch(tok); fm != nil && !v.flags[fm[1]] {
				out = append(out, fmt.Sprintf("%s: flag %q of \"aimem %s\" (nearest: --%s)", at, tok, toks[0], nearest(fm[1], keys(v.flags))))
			}
		}
	}
	return out
}

func checkRoutes(at, s string, v vocabulary) []string {
	var out []string
	for _, m := range routeMention.FindAllStringSubmatch(s, -1) {
		r := strings.TrimRight(m[1], ".,:")
		r = placeholder.ReplaceAllString(r, "{x}")
		if slices.ContainsFunc(foreignRoutes, func(p string) bool { return strings.HasPrefix(r, p) }) || knownRoute(r, v) {
			continue
		}
		out = append(out, fmt.Sprintf("%s: route %q (nearest: %s)", at, m[1], nearest(r, v.routePaths)))
	}
	return out
}

// knownRoute accepts a full route, with {param} or <placeholder> standing
// for a segment, and a prefix that names a family of routes ("/v1/sync",
// "/v1/identity/").
func knownRoute(r string, v vocabulary) bool {
	for _, re := range v.routes {
		if re.MatchString(r) {
			return true
		}
	}
	prefix := strings.TrimSuffix(r, "/") + "/"
	for _, p := range v.routePaths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// loadVocabulary reads the current names from the code: the cmd/aimem
// source, the OpenAPI document the server's parity test pins to its route
// table, and the MCP facades' tool list.
func loadVocabulary(t *testing.T) vocabulary {
	v := vocabulary{top: map[string]bool{}, words: map[string]bool{}, flags: map[string]bool{}, tools: map[string]bool{}}
	fset := token.NewFileSet()
	sources, err := filepath.Glob("../cmd/aimem/*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, p := range sources {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	usageWords := regexp.MustCompile(`aimem((?:\s+[a-z<\[][a-z0-9_|<>\[\]-]*)+)`)
	usageFlags := regexp.MustCompile(`(?:^|[^\w-])--?([a-z][a-z0-9-]*)`)
	wordList := regexp.MustCompile(`^[a-z][a-z0-9_-]*(?: [a-z][a-z0-9_-]*)*$`)
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Name.Name == "main" && n.Recv == nil {
					addMainCases(n, v.top)
				}
			case *ast.CallExpr:
				addFlagSetName(n, v.flags)
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(n.Value)
				if err != nil {
					return true
				}
				if wordList.MatchString(s) {
					for w := range strings.FieldsSeq(s) {
						v.words[w] = true
					}
				}
				for _, m := range usageWords.FindAllStringSubmatch(s, -1) {
					for _, w := range strings.FieldsFunc(m[1], func(r rune) bool { return strings.ContainsRune(" \t\n|<>[]", r) }) {
						v.words[w] = true
					}
				}
				for _, m := range usageFlags.FindAllStringSubmatch(s, -1) {
					v.flags[m[1]] = true
				}
			}
			return true
		})
	}
	if len(v.top) < 20 {
		t.Fatalf("found %d top-level commands in main()'s switch; the check no longer reads the dispatch", len(v.top))
	}

	b, err := os.ReadFile("../internal/server/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	param := regexp.MustCompile(`\\\{[^}]+\\\}`)
	rest := regexp.MustCompile(`\\\{[^}]+\.\.\.\\\}`)
	for p := range doc.Paths {
		q := regexp.QuoteMeta(p)
		q = rest.ReplaceAllString(q, `.+`)
		q = param.ReplaceAllString(q, `[^/]+`)
		v.routes = append(v.routes, regexp.MustCompile(`^`+q+`/?$`))
		v.routePaths = append(v.routePaths, p)
	}
	slices.Sort(v.routePaths)

	for _, n := range mcp.ToolNames() {
		v.tools[n] = true
	}
	return v
}

// addMainCases records the case labels of main()'s `switch cmd`.
func addMainCases(fn *ast.FuncDecl, top map[string]bool) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if id, ok := sw.Tag.(*ast.Ident); !ok || id.Name != "cmd" {
			return true
		}
		for _, st := range sw.Body.List {
			for _, e := range st.(*ast.CaseClause).List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, _ := strconv.Unquote(lit.Value)
					top[s] = true
				}
			}
		}
		return false
	})
}

// addFlagSetName records the name of a flag registered on a FlagSet:
// fs.String("name", ...), fs.BoolVar(&x, "name", ...) and the like.
func addFlagSetName(call *ast.CallExpr, flags map[string]bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	pos := 0
	switch sel.Sel.Name {
	case "String", "Bool", "Int", "Int64", "Uint", "Uint64", "Float64", "Duration", "Func", "BoolFunc":
	case "StringVar", "BoolVar", "IntVar", "Int64Var", "UintVar", "Uint64Var", "Float64Var", "DurationVar", "Var", "TextVar":
		pos = 1
	default:
		return
	}
	if len(call.Args) <= pos {
		return
	}
	if lit, ok := call.Args[pos].(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, _ := strconv.Unquote(lit.Value)
		flags[s] = true
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// nearest names the closest current name by edit distance.
func nearest(s string, names []string) string {
	best, bestD := "", -1
	for _, n := range names {
		if d := distance(s, n); bestD < 0 || d < bestD {
			best, bestD = n, d
		}
	}
	return best
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
