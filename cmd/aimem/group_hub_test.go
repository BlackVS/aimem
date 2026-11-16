package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/server"
	"aimem/internal/store"
)

const (
	chaptersA = `[{"name":"alpha","about":"a"},{"name":"beta","about":"b"}]`
	chaptersH = `[{"name":"alpha","about":"a"},{"name":"gamma","about":"g"}]`
	chaptersC = `[{"name":"delta","about":"d"}]`
)

// groupHub is one hub with a group-shared store seeded from cfg.
func groupHub(t *testing.T, cfg map[string]string) (*store.Registry, *adapter.HubConfig) {
	t.Helper()
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	ts := httptest.NewServer(server.New(reg, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	t.Cleanup(ts.Close)
	if len(cfg) > 0 {
		setMeta(t, reg, "group-shared", cfg)
	}
	return reg, &adapter.HubConfig{URL: ts.URL, Token: "t"}
}

func setMeta(t *testing.T, reg *store.Registry, project string, kv map[string]string) {
	t.Helper()
	db, err := reg.Open(project)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range kv {
		if err := db.SetMeta(k, v); err != nil {
			t.Fatal(err)
		}
	}
}

func getMeta(t *testing.T, reg *store.Registry, project, key string) string {
	t.Helper()
	db, err := reg.OpenExisting(project)
	if err != nil {
		t.Fatalf("%s: %v", project, err)
	}
	v, _ := db.GetMeta(key)
	return v
}

// groupMachine is a state root whose hub entry "primary" points at hub,
// with proj-x declaring group-shared and the group's local config cfg.
func groupMachine(t *testing.T, hub *adapter.HubConfig, cfg map[string]string) *store.Registry {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"primary": hub}, "primary"); err != nil {
		t.Fatal(err)
	}
	reg, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	declare(t, reg, "proj-x")
	if len(cfg) > 0 {
		setMeta(t, reg, "group-shared", cfg)
	}
	return reg
}

// captureOut runs fn with stdout and stderr captured.
func captureOut(t *testing.T, fn func() error) (string, string, error) {
	t.Helper()
	read := func(f **os.File) func() string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := *f
		*f = w
		done := make(chan string)
		go func() { b, _ := io.ReadAll(r); done <- string(b) }()
		return func() string { w.Close(); *f = old; return <-done }
	}
	stopOut, stopErr := read(&os.Stdout), read(&os.Stderr)
	err := fn()
	return stopOut(), stopErr(), err
}

// The whole flow with one hub and two machines: an ordinary sync keeps
// both sides and names the two commands; A's publish replaces the hub's
// value but not C's; C's adopt takes the hub's.
func TestGroupPublishAdopt(t *testing.T) {
	t.Setenv("AIMEM_EMBED_MODEL", "")
	hubReg, hub := groupHub(t, map[string]string{
		"chapters": chaptersH, "about": "the hub's charter", "policy": "all", "features": `["f1"]`,
	})

	// Machine C holds a third value; an ordinary sync changes neither side
	// (its empty keys are filled from the hub, as before).
	regC, stopC := syncMachine(t, "c")
	defer stopC()
	rootC := os.Getenv("AIMEM_STATE_DIR")
	if err := adapter.SaveHubs(rootC, map[string]*adapter.HubConfig{"primary": hub}, "primary"); err != nil {
		t.Fatal(err)
	}
	declare(t, regC, "proj-c")
	setMeta(t, regC, "group-shared", map[string]string{"chapters": chaptersC})
	_, stderr, err := captureOut(t, func() error { return syncOneHTTP("primary", hub, "primary") })
	if err != nil {
		t.Fatal(err)
	}
	want := "`aimem group shared --adopt --hub primary` takes the hub's, `aimem group shared --publish --hub primary` makes this machine's the hub's"
	if !strings.Contains(stderr, "group config group-shared/chapters differs from hub \"primary\" (kept local): "+want) {
		t.Fatalf("sync did not name both commands:\n%s", stderr)
	}
	if strings.Contains(stderr, "re-save") {
		t.Fatalf("sync still gives the old advice:\n%s", stderr)
	}
	if got := getMeta(t, regC, "group-shared", "chapters"); got != chaptersC {
		t.Fatalf("ordinary sync changed machine C's chapters to %s", got)
	}
	if got := getMeta(t, hubReg, "group-shared", "chapters"); got != chaptersH {
		t.Fatalf("ordinary sync changed the hub's chapters to %s", got)
	}

	// Machine A publishes; its features are empty and are not sent.
	t.Setenv("AIMEM_STATE_DIR", "")
	groupMachine(t, hub, map[string]string{"chapters": chaptersA, "about": "machine A's charter"})
	out, _, err := captureOut(t, func() error { return groupCmd([]string{"shared", "--publish"}) })
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	for _, line := range []string{
		`publishing this machine's config to hub "primary"`,
		"chapters: replaced on the hub (chapters [alpha, gamma] -> [alpha, beta])",
		"about: replaced on the hub (17 -> 19 characters)",
		"policy: not sent (empty on this machine; the hub keeps its value)",
		"features: not sent",
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("publish output lacks %q:\n%s", line, out)
		}
	}
	for k, v := range map[string]string{"chapters": chaptersA, "about": "machine A's charter", "policy": "all", "features": `["f1"]`} {
		if got := getMeta(t, hubReg, "group-shared", k); got != v {
			t.Fatalf("hub %s after publish = %q, want %q", k, got, v)
		}
	}

	// A second publish has nothing to change.
	out, _, err = captureOut(t, func() error { return groupCmd([]string{"shared", "--publish"}) })
	if err != nil || !strings.Contains(out, "chapters: unchanged") {
		t.Fatalf("second publish: %v\n%s", err, out)
	}

	// Machine C still holds its own value and is told so at its next sync.
	t.Setenv("AIMEM_STATE_DIR", rootC)
	_, stderr, err = captureOut(t, func() error { return syncOneHTTP("primary", hub, "primary") })
	if err != nil || !strings.Contains(stderr, want) {
		t.Fatalf("machine C's sync after the publish: %v\n%s", err, stderr)
	}
	if got := getMeta(t, regC, "group-shared", "chapters"); got != chaptersC {
		t.Fatalf("the publish reached machine C's local store: %s", got)
	}

	// The comparison shows both sides and which keys differ.
	out, _, err = captureOut(t, func() error { return groupCmd([]string{"shared", "--hub", "primary"}) })
	if err != nil {
		t.Fatal(err)
	}
	var cmp struct {
		Hub     string   `json:"hub"`
		Differs []string `json:"differs"`
		OnHub   struct {
			About string `json:"about"`
		} `json:"on_hub"`
	}
	if err := json.Unmarshal([]byte(out), &cmp); err != nil {
		t.Fatalf("comparison output: %v\n%s", err, out)
	}
	if cmp.Hub != "primary" || cmp.OnHub.About != "machine A's charter" ||
		!slices.Equal(cmp.Differs, []string{"about", "chapters"}) {
		t.Fatalf("comparison = %+v", cmp)
	}

	// C adopts the hub's config.
	out, _, err = captureOut(t, func() error { return groupCmd([]string{"shared", "--adopt"}) })
	if err != nil {
		t.Fatalf("adopt: %v\n%s", err, out)
	}
	if !strings.Contains(out, "chapters: replaced here (chapters [delta] -> [alpha, beta])") ||
		!strings.Contains(out, "about: replaced here (17 -> 19 characters)") || !strings.Contains(out, "policy: unchanged") {
		t.Fatalf("adopt output:\n%s", out)
	}
	for k, v := range map[string]string{"chapters": chaptersA, "about": "machine A's charter", "policy": "all", "features": `["f1"]`} {
		if got := getMeta(t, regC, "group-shared", k); got != v {
			t.Fatalf("machine C %s after adopt = %q, want %q", k, got, v)
		}
	}
	_, stderr, _ = captureOut(t, func() error { return syncOneHTTP("primary", hub, "primary") })
	if strings.Contains(stderr, "differs") {
		t.Fatalf("a sync after the adopt still reports a difference:\n%s", stderr)
	}
}

// Adopt holds the hub's values to the hub's own rules before it writes,
// keeps a local value the hub lacks, and fails when any key failed.
func TestGroupAdoptValidates(t *testing.T) {
	_, hub := groupHub(t, map[string]string{
		"chapters": `[{"name":"<b>bold</b>","about":"x"}]`, "policy": "sometimes", "about": "hub",
	})
	reg := groupMachine(t, hub, map[string]string{
		"chapters": chaptersA, "policy": "domain", "about": "local", "features": `["f1"]`,
	})
	out, _, err := captureOut(t, func() error { return groupCmd([]string{"shared", "--adopt"}) })
	if err == nil || !strings.Contains(err.Error(), "2 of 4 keys failed") {
		t.Fatalf("adopt of invalid values: err %v\n%s", err, out)
	}
	if !strings.Contains(out, "chapters: refused (chapter name") || !strings.Contains(out, "policy: refused (policy must be all or domain") ||
		!strings.Contains(out, "features: kept (empty on the hub)") {
		t.Fatalf("adopt output:\n%s", out)
	}
	for k, v := range map[string]string{"chapters": chaptersA, "policy": "domain", "about": "hub", "features": `["f1"]`} {
		if got := getMeta(t, reg, "group-shared", k); got != v {
			t.Fatalf("local %s = %q, want %q", k, got, v)
		}
	}
}

// A key the hub refuses fails the publish after the other keys landed,
// and names the key.
func TestGroupPublishPartialFailure(t *testing.T) {
	hubReg, hub := groupHub(t, map[string]string{"chapters": chaptersH})
	groupMachine(t, hub, map[string]string{"chapters": `[{"name":"<i>","about":"x"}]`, "about": "local"})
	out, _, err := captureOut(t, func() error { return groupCmd([]string{"shared", "--publish"}) })
	if err == nil || !strings.Contains(err.Error(), "1 of 4 keys failed") {
		t.Fatalf("publish with a refused key: err %v\n%s", err, out)
	}
	if !strings.Contains(out, "chapters: hub refused: hub HTTP 400") || !strings.Contains(out, "markup character") {
		t.Fatalf("publish output:\n%s", out)
	}
	if got := getMeta(t, hubReg, "group-shared", "chapters"); got != chaptersH {
		t.Fatalf("hub chapters = %s", got)
	}
	if got := getMeta(t, hubReg, "group-shared", "about"); got != "local" {
		t.Fatalf("hub about = %q; the other keys must still land", got)
	}
}

// A group the hub does not hold: adopt and the comparison say so through
// the no_such_project code, read without creating it; publish creates it.
func TestGroupMissingOnHub(t *testing.T) {
	hubReg, hub := groupHub(t, nil)
	groupMachine(t, hub, map[string]string{"chapters": chaptersA})
	for _, args := range [][]string{{"shared", "--adopt"}, {"shared", "--hub", "primary"}} {
		_, _, err := captureOut(t, func() error { return groupCmd(args) })
		if err == nil || !strings.Contains(err.Error(), `hub "primary" holds no group "group-shared"`) {
			t.Fatalf("%v: err %v", args, err)
		}
	}
	if ids, _ := hubReg.Projects(); slices.Contains(ids, "group-shared") {
		t.Fatal("a read created the group on the hub")
	}
	if _, _, err := captureOut(t, func() error { return groupCmd([]string{"shared", "--publish"}) }); err != nil {
		t.Fatal(err)
	}
	if got := getMeta(t, hubReg, "group-shared", "chapters"); got != chaptersA {
		t.Fatalf("hub chapters after publishing a new group = %s", got)
	}
}

// The flags exclude each other's edits, and the hub must be named when
// the group's projects are bound to more than one.
func TestGroupHubFlagRules(t *testing.T) {
	_, hub := groupHub(t, nil)
	reg := groupMachine(t, hub, nil)
	for _, args := range [][]string{
		{"shared", "--publish", "--adopt"},
		{"shared", "--publish", "--about", "x"},
		{"shared", "--adopt", "--chapter", "a: b"},
		{"shared", "--hub", "primary", "--policy", "all"},
	} {
		if err := groupCmd(args); err == nil || !strings.Contains(err.Error(), "exclude each other") {
			t.Fatalf("%v: err %v", args, err)
		}
	}
	if ids, _ := reg.Projects(); slices.Contains(ids, "group-shared") {
		t.Fatal("a refused call created the group's store")
	}

	root := os.Getenv("AIMEM_STATE_DIR")
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"primary": hub, "second": hub}, "primary"); err != nil {
		t.Fatal(err)
	}
	declare(t, reg, "proj-y")
	setMeta(t, reg, "proj-y", map[string]string{"hub": "second"})
	_, _, err := captureOut(t, func() error { return groupCmd([]string{"shared", "--publish"}) })
	if err == nil || !strings.Contains(err.Error(), "bound to hubs primary, second; name one with --hub") {
		t.Fatalf("two bound hubs without --hub: %v", err)
	}
	if _, _, err := captureOut(t, func() error { return groupCmd([]string{"shared", "--publish", "--hub", "nope"}) }); err == nil ||
		!strings.Contains(err.Error(), `no hub named "nope"`) {
		t.Fatalf("unknown hub: %v", err)
	}
}

// An edit is published right away through the same publish: a machine
// whose chapters are empty edits the charter, and the hub's chapters
// survive while its charter takes the edit.
func TestGroupEditPublishesNonEmptyKeysOnly(t *testing.T) {
	hubReg, hub := groupHub(t, map[string]string{"chapters": chaptersH, "about": "old charter"})
	groupMachine(t, hub, nil)
	out, stderr, err := captureOut(t, func() error { return groupCmd([]string{"shared", "--about", "new charter"}) })
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		About string `json:"about"`
	}
	if json.Unmarshal([]byte(out), &view) != nil || view.About != "new charter" {
		t.Fatalf("stdout must stay the group's JSON:\n%s", out)
	}
	if !strings.Contains(stderr, `publishing the edit to hub "primary"`) || !strings.Contains(stderr, "chapters: not sent") {
		t.Fatalf("edit publish report:\n%s", stderr)
	}
	if got := getMeta(t, hubReg, "group-shared", "chapters"); got != chaptersH {
		t.Fatalf("an edit of another key cleared the hub's chapters: %q", got)
	}
	if got := getMeta(t, hubReg, "group-shared", "about"); got != "new charter" {
		t.Fatalf("hub about after the edit = %q", got)
	}
}
