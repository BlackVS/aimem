package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"aimem/internal/schema"
	"aimem/internal/store"
)

func testServer(t *testing.T) (*Server, *store.Registry) {
	t.Helper()
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	return New(reg, slog.New(slog.NewTextHandler(new(strings.Builder), nil))), reg
}

func req(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAdminMetaAndOverview(t *testing.T) {
	s, reg := testServer(t)
	h := s.Handler()

	// Exposed meta keys round-trip; JSON-typed keys are validated.
	w := req(t, h, "PUT", "/v1/projects/group-kb/meta/about", `{"value":"the kb"}`)
	if w.Code != 200 {
		t.Fatalf("put about: %d %s", w.Code, w.Body)
	}
	w = req(t, h, "PUT", "/v1/projects/group-kb/meta/chapters", `{"value":"not json"}`)
	if w.Code != 400 {
		t.Fatalf("bad chapters accepted: %d", w.Code)
	}
	w = req(t, h, "PUT", "/v1/projects/group-kb/meta/chapters",
		`{"value":"[{\"name\":\"ci\",\"about\":\"pipelines\"}]"}`)
	if w.Code != 200 {
		t.Fatalf("put chapters: %d %s", w.Code, w.Body)
	}
	w = req(t, h, "PUT", "/v1/projects/group-kb/meta/policy", `{"value":"sometimes"}`)
	if w.Code != 400 {
		t.Fatalf("bad policy accepted: %d", w.Code)
	}
	// Unexposed keys are refused both ways.
	if w = req(t, h, "PUT", "/v1/projects/group-kb/meta/secret", `{"value":"x"}`); w.Code != 403 {
		t.Fatalf("unexposed key writable: %d", w.Code)
	}
	if w = req(t, h, "GET", "/v1/projects/group-kb/meta/secret", ""); w.Code != 403 {
		t.Fatalf("unexposed key readable: %d", w.Code)
	}
	w = req(t, h, "GET", "/v1/projects/group-kb/meta/about", "")
	var got map[string]string
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || got["value"] != "the kb" {
		t.Fatalf("get about: %d %v", w.Code, got)
	}

	// Overview carries group config and membership.
	db, _ := reg.Open("proj-x")
	db.SetMeta("groups", `["group-kb"]`)
	w = req(t, h, "GET", "/v1/overview", "")
	var ov struct {
		Projects []struct {
			ID       string          `json:"id"`
			Groups   []string        `json:"groups"`
			About    string          `json:"about"`
			Chapters json.RawMessage `json:"chapters"`
		} `json:"projects"`
	}
	json.Unmarshal(w.Body.Bytes(), &ov)
	var sawGroup, sawMember bool
	for _, p := range ov.Projects {
		if p.ID == "group-kb" && p.About == "the kb" && strings.Contains(string(p.Chapters), "ci") {
			sawGroup = true
		}
		if p.ID == "proj-x" && len(p.Groups) == 1 && p.Groups[0] == "group-kb" {
			sawMember = true
		}
	}
	if !sawGroup || !sawMember {
		t.Fatalf("overview incomplete: group=%v member=%v body=%s", sawGroup, sawMember, w.Body)
	}

	// The admin page is served and is HTML.
	w = req(t, h, "GET", "/admin", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "aimem hub") {
		t.Fatalf("admin page: %d", w.Code)
	}
}

func TestModelsConfig(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()
	envFile := t.TempDir() + "/env"
	if err := os.WriteFile(envFile, []byte(
		"AIMEM_OPENAI_API_KEY=supersecret\nAIMEM_CURATE_MODEL=old-model\nAIMEM_EMBED_MODEL=\"Old Embed\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEM_ENV_FILE", envFile)

	w := req(t, h, "GET", "/v1/config/models", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "old-model") ||
		!strings.Contains(w.Body.String(), "Old Embed") ||
		strings.Contains(w.Body.String(), "supersecret") {
		t.Fatalf("get models: %d %s", w.Code, w.Body)
	}
	// Injection and empty values refused.
	for _, bad := range []string{`{"curate_model":"","embed_model":"x"}`,
		`{"curate_model":"a\"b","embed_model":"x"}`} {
		if w := req(t, h, "PUT", "/v1/config/models", bad); w.Code != 400 {
			t.Fatalf("bad model accepted: %d", w.Code)
		}
	}
	w = req(t, h, "PUT", "/v1/config/models",
		`{"curate_model":"new-model","embed_model":"New Embed 9"}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "supersecret") {
		t.Fatalf("put models: %d %s", w.Code, w.Body)
	}
	raw, _ := os.ReadFile(envFile)
	got := string(raw)
	if !strings.Contains(got, "AIMEM_OPENAI_API_KEY=supersecret") ||
		!strings.Contains(got, `AIMEM_CURATE_MODEL="new-model"`) ||
		!strings.Contains(got, `AIMEM_EMBED_MODEL="New Embed 9"`) ||
		strings.Contains(got, "old-model") {
		t.Fatalf("env file after put:\n%s", got)
	}
}

func TestDropProject(t *testing.T) {
	s, reg := testServer(t)
	h := s.Handler()
	db, _ := reg.Open("stale-proj")
	db.Remember("doomed fact", "test", store.RememberOpts{})

	if w := req(t, h, "DELETE", "/v1/projects/user", ""); w.Code != 400 {
		t.Fatalf("user DB droppable: %d", w.Code)
	}
	if w := req(t, h, "DELETE", "/v1/projects/no-such", ""); w.Code != 400 {
		t.Fatalf("missing project drop: %d", w.Code)
	}
	if w := req(t, h, "DELETE", "/v1/projects/stale-proj", ""); w.Code != 200 {
		t.Fatalf("drop failed: %d %s", w.Code, req(t, h, "DELETE", "/v1/projects/stale-proj", "").Body)
	}
	ids, _ := reg.Projects()
	for _, id := range ids {
		if id == "stale-proj" {
			t.Fatal("project still listed after drop")
		}
	}
	// A read of the dropped project must NOT resurrect it as an empty husk.
	if w := req(t, h, "GET", "/v1/projects/stale-proj/memories", ""); w.Code != 400 {
		t.Fatalf("read of dropped project: %d", w.Code)
	}
	ids, _ = reg.Projects()
	for _, id := range ids {
		if id == "stale-proj" {
			t.Fatal("GET resurrected the dropped project")
		}
	}
}

func TestTCPAuthWrapper(t *testing.T) {
	// Exercises the REAL wrapper ListenTCP installs — a re-implementation
	// here once drifted from the shipped one and tested nothing.
	s, _ := testServer(t)
	mux := http.NewServeMux()
	mux.Handle("/", s.Handler())
	authed := s.authWrapper("sekrit", mux)

	// The complete unauthenticated surface: three GETs.
	for _, path := range []string{"/admin", "/", "/v1/status"} {
		if w := req(t, authed, "GET", path, ""); w.Code != 200 {
			t.Fatalf("GET %s should be public: %d", path, w.Code)
		}
	}
	// The same paths are NOT public for other methods.
	for _, path := range []string{"/admin", "/", "/v1/status"} {
		if w := req(t, authed, "POST", path, ""); w.Code != 401 {
			t.Fatalf("POST %s not gated: %d", path, w.Code)
		}
	}
	// Everything else requires the exact bearer token.
	if w := req(t, authed, "GET", "/v1/overview", ""); w.Code != 401 {
		t.Fatalf("api not gated: %d", w.Code)
	}
	if w := req(t, authed, "GET", "/v1/projects", ""); w.Code != 401 {
		t.Fatalf("projects not gated: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/v1/overview", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	authed.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("wrong token accepted: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/v1/overview", nil)
	r.Header.Set("Authorization", "Bearer sekrit")
	w = httptest.NewRecorder()
	authed.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("correct token refused: %d", w.Code)
	}
}

// TestReviewWindowsMatchConsole pins the console's #revDays option
// values to the server's reviewWindows list — the coupling that, if
// broken (an option added to one side only), silently empties the
// Review dropdown at the new window.
func TestReviewWindowsMatchConsole(t *testing.T) {
	page := string(adminHTML)
	i := strings.Index(page, `id="revDays"`)
	if i < 0 {
		t.Fatal("no #revDays select in admin.html")
	}
	end := strings.Index(page[i:], "</select>")
	if end < 0 {
		t.Fatal("unclosed #revDays select")
	}
	block := page[i : i+end]
	var got []int
	for _, m := range regexp.MustCompile(`<option value="(\d+)"`).FindAllStringSubmatch(block, -1) {
		n, _ := strconv.Atoi(m[1])
		got = append(got, n)
	}
	if !slices.Equal(got, reviewWindows) {
		t.Fatalf("console #revDays offers %v; server reviewWindows = %v — keep them identical", got, reviewWindows)
	}
}

// TestOverviewReviewMapWiring pins the index→window re-keying and the
// contracts the console's fail-open filter rests on: every row ships a
// review map with ALL window keys (zeros included — a healthy empty
// queue must be distinguishable from an absent map, which signals a
// count-query error), and the response carries review_windows.
func TestOverviewReviewMapWiring(t *testing.T) {
	s, reg := testServer(t)
	db, _ := reg.Open("proj-aged")
	// ImportMemory honors CreatedAt and writes no audit rows, so the
	// review predicate's last_seen falls back to 2020 — stale at every
	// window without reaching into store internals.
	if err := db.ImportMemory(&store.Memory{
		ID: "aged-1", Text: "an old thin fact", Kind: "fact",
		Confidence: 0.6, CreatedAt: "2020-01-01T00:00:00Z", Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}
	reg.Open("proj-clean") // active project, empty queue

	rec := req(t, http.HandlerFunc(s.overview), "GET", "/v1/overview", "")
	var out struct {
		Projects []struct {
			ID     string         `json:"id"`
			Review map[string]int `json:"review"`
			Stats  struct {
				Docs    int `json:"docs"`
				Records int `json:"records"`
			} `json:"stats"`
		} `json:"projects"`
		ReviewWindows []int `json:"review_windows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.ReviewWindows, reviewWindows) {
		t.Fatalf("review_windows = %v, want %v", out.ReviewWindows, reviewWindows)
	}
	rows := map[string]map[string]int{}
	for _, p := range out.Projects {
		rows[p.ID] = p.Review
	}
	for _, id := range []string{"proj-aged", "proj-clean"} {
		m := rows[id]
		if m == nil {
			t.Fatalf("%s: review map absent — zeros must ship, absence means error", id)
		}
		for _, d := range reviewWindows {
			if _, ok := m[strconv.Itoa(d)]; !ok {
				t.Fatalf("%s: window %d missing from review map %v", id, d, m)
			}
		}
	}
	// The aged fact qualifies at EVERY window (2020 is older than 90d);
	// the clean project counts zero everywhere. A swapped index/key
	// basis breaks one of these.
	for _, d := range reviewWindows {
		k := strconv.Itoa(d)
		if rows["proj-aged"][k] != 1 {
			t.Fatalf("proj-aged[%s]=%d, want 1", k, rows["proj-aged"][k])
		}
		if rows["proj-clean"][k] != 0 {
			t.Fatalf("proj-clean[%s]=%d, want 0", k, rows["proj-clean"][k])
		}
	}
}

// TestWikiSentinelsOutsideProjectIDSpace: the Wiki dropdown's mode
// toggles use option values that must NEVER be legal project ids —
// scrape them from the page and check against the real validator.
func TestWikiSentinelsOutsideProjectIDSpace(t *testing.T) {
	ms := regexp.MustCompile(`<option value="(\*+)"`).FindAllStringSubmatch(string(adminHTML), -1)
	if len(ms) != 2 {
		t.Fatalf("expected the 2 wiki mode sentinels in admin.html, found %d — scrape or markup drifted", len(ms))
	}
	for _, m := range ms {
		if schema.ValidProjectID(m[1]) {
			t.Fatalf("sentinel %q is a valid project id — collision with a real project possible", m[1])
		}
	}
}

// The project-merge action must offer the target as a dropdown of the
// hub's projects, never a free-text prompt listing candidates as text
// (user notice): pinned by scraping the page like the other console
// rules.
func TestConsoleMergeUsesDropdown(t *testing.T) {
	page := string(adminHTML)
	fn := page[strings.Index(page, "function mergeChooser"):]
	fn = fn[:strings.Index(fn, "async function mergeProjectDo")]
	if !strings.Contains(fn, `<select id="mergeInto"`) {
		t.Fatal("merge chooser must render a <select> of target projects")
	}
	if strings.Contains(fn, "prompt(") || strings.Contains(fn, "Existing projects") {
		t.Fatal("merge flow must not fall back to a typed prompt or a plain-text project list")
	}
	do := page[strings.Index(page, "async function mergeProjectDo"):]
	do = do[:strings.Index(do, "\n}")]
	if !strings.Contains(do, `$("mergeInto")`) || strings.Contains(do, "prompt(") {
		t.Fatal("mergeProjectDo must read the dropdown, not a prompt")
	}
}
