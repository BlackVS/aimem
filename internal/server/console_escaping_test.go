package server

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Stored values reach the browser pages through template literals. A value
// HTML-escaped into an inline handler's JavaScript string is decoded by the
// browser before the script is parsed, so escaping cannot protect it there:
// the admin console passes data to handlers only as data-args JSON, and
// every page's escape helper covers the apostrophe for the attributes that
// remain. These tests pin both rules on the pages as shipped.

var inlineHandler = regexp.MustCompile(`\son[a-z]+=("[^"]*"|'[^']*')`)

func pageSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPagesEscapeTheApostrophe(t *testing.T) {
	for _, name := range []string{"admin.html", "tasks.html", "status.html"} {
		page := pageSource(t, name)
		if !strings.Contains(page, `"'":"&#39;"`) || !strings.Contains(page, `/[&<>"']/g`) {
			t.Errorf("%s: the escape helper must encode & < > \" and '", name)
		}
	}
}

func TestAdminConsoleInlineHandlersCarryNoData(t *testing.T) {
	page := pageSource(t, "admin.html")
	for _, m := range inlineHandler.FindAllString(page, -1) {
		if strings.Contains(m, "${") {
			t.Errorf("inline handler interpolates a value; pass it with act()/acts() instead:%s", m)
		}
	}
}

// The tasks page keeps a few interpolating handlers. Each expression below
// was reviewed: a fixed state name, a task or comment id the hub mints (a
// dependency is checked against the task id pattern on write), or JSON
// that is itself a valid script expression. A new one fails here until it
// is reviewed and added, or moved out of the handler.
var reviewedTasksHandlerValues = []string{
	"${s}",
	"${st}",
	"${esc(t.id)}",
	"${esc(c.id)}",
	"${taskURL(t.id)}",
	"${taskURL(ref)}",
	"${taskURL(d)}",
	"${esc(JSON.stringify(attempt))}",
}

func TestTasksPageInlineHandlerValuesAreReviewed(t *testing.T) {
	page := pageSource(t, "tasks.html")
	expr := regexp.MustCompile(`\$\{[^}]*\}`)
	for _, m := range inlineHandler.FindAllString(page, -1) {
		for _, e := range expr.FindAllString(m, -1) {
			if !slices.Contains(reviewedTasksHandlerValues, e) {
				t.Errorf("unreviewed value %s in tasks.html inline handler:%s", e, m)
			}
		}
	}
}

// Every handler name an element can carry is an entry of the ACT
// allowlist, and the allowlist holds no entry nothing uses.
func TestAdminConsoleHandlerNamesMatchTheAllowlist(t *testing.T) {
	page := pageSource(t, "admin.html")
	start := strings.Index(page, "const ACT = {")
	if start < 0 {
		t.Fatal("admin.html has no ACT allowlist")
	}
	body := page[start:]
	body = body[:strings.Index(body, "\n};")]
	allowed := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  (\w+):`).FindAllStringSubmatch(body, -1) {
		allowed[m[1]] = true
	}
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bact\("(\w+)"`).FindAllStringSubmatch(page, -1) {
		used[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`\bacts\(\{([^}]*)\}`).FindAllStringSubmatch(page, -1) {
		for _, n := range regexp.MustCompile(`:"(\w+)"`).FindAllStringSubmatch(m[1], -1) {
			used[n[1]] = true
		}
	}
	if len(used) == 0 {
		t.Fatal("found no act()/acts() call sites; the scrape drifted")
	}
	var missing, unused []string
	for n := range used {
		if !allowed[n] {
			missing = append(missing, n)
		}
	}
	for n := range allowed {
		if !used[n] {
			unused = append(unused, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(unused)
	if len(missing) > 0 || len(unused) > 0 {
		t.Errorf("handler names not in ACT: %v; ACT entries never used: %v", missing, unused)
	}
}

// A memory id is interpolated into the console's markup, so import and
// sync-in refuse an id that could end a quoted string or an attribute.
func TestImportRefusesAMemoryIDThatCarriesMarkup(t *testing.T) {
	s, reg := testServer(t)
	h := s.Handler()
	bad := `x'),alert(1)//`
	body, _ := json.Marshal(map[string]any{"memory": map[string]any{
		"id": bad, "text": "a fact", "kind": "fact", "created_at": "2026-10-19T00:00:00Z"}})
	if w := req(t, h, "POST", "/v1/projects/proj-x/memories/import", string(body)); w.Code != 400 {
		t.Fatalf("import of id %q: %d %s, want 400", bad, w.Code, w.Body)
	}
	good, _ := json.Marshal(map[string]any{"memory": map[string]any{
		"id": "01a150e8-6560-7000-9318-25013f9bc20b", "text": "a fact", "kind": "fact", "created_at": "2026-10-19T00:00:00Z"}})
	if w := req(t, h, "POST", "/v1/projects/proj-x/memories/import", string(good)); w.Code != 200 {
		t.Fatalf("import of a UUIDv7 id: %d %s", w.Code, w.Body)
	}

	line, _ := json.Marshal(map[string]any{"project_id": "proj-y", "memory": map[string]any{
		"id": `m"><img src=x>`, "text": "another fact", "kind": "fact", "created_at": "2026-10-19T00:00:00Z"}})
	w := syncReq(t, s, "POST", "/v1/sync/memories", string(line)+"\n")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"failed":1`) || !strings.Contains(w.Body.String(), `"imported":0`) {
		t.Fatalf("sync-in of a markup id: %d %s, want it counted failed", w.Code, w.Body)
	}
	if db, err := reg.OpenExisting("proj-y"); err == nil {
		if mems, _ := db.Memories(true); len(mems) != 0 {
			t.Fatalf("markup id stored by sync-in: %+v", mems)
		}
	}
}

func TestChapterNamesWithMarkupAreRefused(t *testing.T) {
	s, reg := testServer(t)
	h := s.Handler()
	for _, v := range []string{
		`[{"name":"a'b","about":"x"}]`,
		`[{"name":"<img>","about":"x"}]`,
		`[{"name":"","about":"x"}]`,
		`[{"name":"` + strings.Repeat("n", 65) + `","about":"x"}]`,
		`{"name":"ci"}`,
	} {
		body, _ := json.Marshal(map[string]string{"value": v})
		if w := req(t, h, "PUT", "/v1/projects/group-kb/meta/chapters", string(body)); w.Code != 400 {
			t.Errorf("chapters %s accepted: %d", v, w.Code)
		}
	}
	// Free prose stays allowed in "about", and an empty value clears.
	for _, v := range []string{`[{"name":"release notes","about":"what's shipped, <b>and</b> why"}]`, ``} {
		body, _ := json.Marshal(map[string]string{"value": v})
		if w := req(t, h, "PUT", "/v1/projects/group-kb/meta/chapters", string(body)); w.Code != 200 {
			t.Errorf("chapters %q refused: %d %s", v, w.Code, w.Body)
		}
	}

	// The same rule holds for chapters arriving by group-config sync.
	rec, _ := json.Marshal(map[string]string{"project": "group-sync", "key": "chapters",
		"value": `[{"name":"x' onmouseover='y","about":"z"}]`})
	if w := syncReq(t, s, "POST", "/v1/sync/group-config", string(rec)+"\n"); !strings.Contains(w.Body.String(), `"applied":0`) {
		t.Fatalf("markup chapter adopted from sync: %s", w.Body)
	}
	if db, err := reg.OpenExisting("group-sync"); err == nil {
		if v, _ := db.GetMeta("chapters"); v != "" {
			t.Fatalf("markup chapter stored by sync: %q", v)
		}
	}
}
