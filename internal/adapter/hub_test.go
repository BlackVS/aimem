package adapter

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubConfigLegacyAndNamed(t *testing.T) {
	root := t.TempDir()

	// Legacy flat file reads as the default-named hub.
	legacy := `{"url":"https://old.example:8440","token":"tok-old"}`
	if err := os.WriteFile(hubConfigPath(root), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	hubs, def := LoadHubs(root)
	if def != DefaultHubName || hubs[def] == nil || hubs[def].URL != "https://old.example:8440" {
		t.Fatalf("legacy fold-in: hubs=%v def=%q", hubs, def)
	}
	if h := LoadHub(root); h == nil || h.Token != "tok-old" {
		t.Fatalf("LoadHub back-compat: %v", h)
	}

	// Adding a named hub preserves the legacy entry; --default semantics.
	hubs["home"] = &HubConfig{URL: "https://home.example:8440", Token: "tok-home", Sync: "u@h"}
	if err := SaveHubs(root, hubs, "home"); err != nil {
		t.Fatal(err)
	}
	hubs, def = LoadHubs(root)
	if len(hubs) != 2 || def != "home" {
		t.Fatalf("named form: hubs=%v def=%q", hubs, def)
	}
	if name, h := ResolveHub(root, "default"); name != "default" || h.Token != "tok-old" {
		t.Fatalf("resolve named: %q %v", name, h)
	}
	// An UNKNOWN named binding must NOT fall back to the default
	// (partition guarantee, changed 2026-08-29): the name returns with no
	// config, so push spools under it instead of misrouting. Only an
	// EMPTY binding means "use the default".
	if name, h := ResolveHub(root, "nope"); name != "nope" || h != nil {
		t.Fatalf("resolve unknown must not fall back: %q %v", name, h)
	}
	if name, _ := ResolveHub(root, ""); name != "home" {
		t.Fatalf("resolve empty->default: %q", name)
	}

	// SaveHub (legacy setter) replaces the default entry only.
	if err := SaveHub(root, &HubConfig{URL: "https://new.example", Token: "tok-new"}); err != nil {
		t.Fatal(err)
	}
	hubs, def = LoadHubs(root)
	if hubs["home"].Token != "tok-new" || hubs["default"].Token != "tok-old" || def != "home" {
		t.Fatalf("SaveHub replace default: hubs=%v def=%q", hubs, def)
	}
}

func TestPushHubRoutesByBinding(t *testing.T) {
	root := t.TempDir()
	got := map[string][]string{}
	mk := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got[name] = append(got[name], r.URL.Path)
			w.WriteHeader(200)
		}))
	}
	work, home := mk("work"), mk("home")
	defer work.Close()
	defer home.Close()
	err := SaveHubs(root, map[string]*HubConfig{
		"work": {URL: work.URL, Token: "t1"},
		"home": {URL: home.URL, Token: "t2"},
	}, "work")
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(root)
	c.pushHub("home", []byte(`{"x":1}`))
	c.pushHub("", []byte(`{"x":2}`))     // unbound -> default
	c.pushHub("nope", []byte(`{"x":3}`)) // unknown name -> spooled, NEVER the default
	if len(got["home"]) != 1 || len(got["work"]) != 1 {
		t.Fatalf("routing: home=%d work=%d", len(got["home"]), len(got["work"]))
	}
	if _, err := os.Stat(filepath.Join(root, "spool", "hub-nope.jsonl")); err != nil {
		t.Fatalf("unknown-hub checkpoint not spooled under its name: %v", err)
	}

	// An unreachable hub spools under its own name and must not affect
	// the other hub's delivery.
	home.Close()
	c.pushHub("home", []byte(`{"x":4}`))
	if _, err := os.Stat(filepath.Join(root, "spool", "hub-home.jsonl")); err != nil {
		t.Fatalf("per-hub spool missing: %v", err)
	}
	c.pushHub("", []byte(`{"x":5}`))
	if len(got["work"]) != 2 {
		t.Fatalf("work delivery affected by home outage: %d", len(got["work"]))
	}
	if _, err := os.Stat(filepath.Join(root, "spool", "hub.jsonl")); err == nil {
		t.Fatal("default hub should not have spooled")
	}
}

// Anti-entropy import must not re-broadcast. A machine that syncs with
// two hubs pulls one hub's projects into its local store; if importing
// those events pushed them onward they would land on this machine's
// DEFAULT hub (exported events carry no binding), replicating a project
// onto a hub it is not bound to. Observed live: a home-bound project
// kept reappearing on the work hub minutes after being dropped.
func TestSubmitLocalDoesNotPushToAnyHub(t *testing.T) {
	root := t.TempDir()
	var hits int
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(200)
	}))
	defer hub.Close()
	if err := SaveHubs(root, map[string]*HubConfig{"work": {URL: hub.URL, Token: "t"}}, "work"); err != nil {
		t.Fatal(err)
	}
	c := NewClient(root)
	p := &Payload{ProjectID: "proj-x"}
	p.Event.Client = "claude-code"
	p.Event.SessionID = "s1"
	p.Event.TurnID = "t1"
	p.Event.Kind = "turn"
	if _, err := c.SubmitLocal(p); err != nil && !strings.Contains(err.Error(), "spool") {
		// no local service in a test root: spooling is fine, pushing is not
		_ = err
	}
	if hits != 0 {
		t.Fatalf("SubmitLocal delivered to a hub %d time(s) — sync would replicate across hubs", hits)
	}
}

// TestUnknownNamedHubNeverFallsBack pins the partition guarantee found
// broken live 2026-08-29: a project bound to a hub this machine has NOT
// configured must spool for that hub, never deliver to the default.
func TestUnknownNamedHubNeverFallsBack(t *testing.T) {
	defaultHits := 0
	def := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultHits++
	}))
	defer def.Close()
	root := t.TempDir()
	if err := SaveHubs(root, map[string]*HubConfig{
		"home": {URL: def.URL, Token: "t"},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	c := NewClient(root)
	c.pushHub("work", []byte(`{"project_id":"p","event":{}}`))
	if defaultHits != 0 {
		t.Fatalf("checkpoint bound to unconfigured hub reached the DEFAULT hub (%d hits)", defaultHits)
	}
	data, err := os.ReadFile(filepath.Join(root, "spool", "hub-work.jsonl"))
	if err != nil || !strings.Contains(string(data), `"project_id":"p"`) {
		t.Fatalf("checkpoint not spooled under the named hub: %v %q", err, data)
	}
	// Once the hub IS configured, the spool drains to it on next contact.
	workHits := 0
	work := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workHits++
	}))
	defer work.Close()
	if err := SaveHubs(root, map[string]*HubConfig{
		"home": {URL: def.URL, Token: "t"},
		"work": {URL: work.URL, Token: "t"},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	c.pushHub("work", []byte(`{"project_id":"p","event":{"n":2}}`))
	if workHits < 2 { // the new push plus the drained spool line
		t.Fatalf("spool did not drain to the newly configured hub: %d hits", workHits)
	}
	if defaultHits != 0 {
		t.Fatal("default hub was touched during the drain")
	}
}
