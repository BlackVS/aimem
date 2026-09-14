package ident

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProjectIDPin(t *testing.T) {
	dir := t.TempDir()

	// No config: falls back to path-derived slug+hash.
	id, err := ProjectID(dir)
	if err != nil || id == "" || id == filepath.Base(dir) {
		t.Fatalf("derived id: %q err=%v", id, err)
	}

	// Pinned name is used verbatim — same name, same journal, everywhere.
	writeConfig(t, dir, `{"project": "RC", "groups": ["oboro"]}`)
	id, err = ProjectID(dir)
	if err != nil || id != "RC" {
		t.Fatalf("pinned id: %q err=%v", id, err)
	}
	// Groups parsing still works from the same file.
	gs, err := ProjectGroups(dir)
	if err != nil || len(gs) != 1 || gs[0] != "group-oboro" {
		t.Fatalf("groups: %v err=%v", gs, err)
	}

	// Reserved and malformed names are errors, not silent fallbacks.
	for _, bad := range []string{"user", "group-oboro", "has space", "/etc/x"} {
		writeConfig(t, dir, `{"project": "`+bad+`"}`)
		if _, err := ProjectID(dir); err == nil {
			t.Errorf("pin %q accepted", bad)
		}
	}
}

func TestProjectHubName(t *testing.T) {
	dir := t.TempDir()

	// No config file: default hub.
	if h, err := ProjectHubName(dir); err != nil || h != "" {
		t.Fatalf("missing config: %q err=%v", h, err)
	}

	// Config without a hub key: default hub.
	writeConfig(t, dir, `{"groups": ["oboro"]}`)
	if h, err := ProjectHubName(dir); err != nil || h != "" {
		t.Fatalf("no hub key: %q err=%v", h, err)
	}

	writeConfig(t, dir, `{"hub": "home", "groups": ["oboro"]}`)
	if h, err := ProjectHubName(dir); err != nil || h != "home" {
		t.Fatalf("bound: %q err=%v", h, err)
	}

	// Malformed name is an error, not a silent fall-through to the
	// default hub (that would leak data across hubs).
	writeConfig(t, dir, `{"hub": "Not A Name"}`)
	if _, err := ProjectHubName(dir); err == nil {
		t.Fatal("invalid hub name accepted")
	}
}

// A config file that exists but cannot be parsed stays "absent" for the
// capture paths (a checkpoint never blocks on a broken file) but is an
// error for a caller that routes data on the hub binding: taking "absent"
// for "default hub" would send a bound project's traffic to the wrong hub.
func TestUnreadableConfigIsStrictForRouting(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"hub": "home",`) // truncated by a crashed editor
	if h, err := ProjectHubName(dir); err != nil || h != "" {
		t.Fatalf("fail-open read must stay fail-open: %q err=%v", h, err)
	}
	if _, err := ProjectHubNameStrict(dir); !errors.Is(err, ErrConfigUnreadable) {
		t.Fatalf("strict read of an unparseable file: err=%v; want ErrConfigUnreadable", err)
	}
	// Absent and BOM-prefixed files are fine strictly too; a parseable
	// file with a bad name is the other error, not this one.
	if err := os.Remove(filepath.Join(dir, ".aimem.json")); err != nil {
		t.Fatal(err)
	}
	if h, err := ProjectHubNameStrict(dir); err != nil || h != "" {
		t.Fatalf("absent config strictly: %q err=%v", h, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), append([]byte("\xef\xbb\xbf"), []byte(`{"hub":"home"}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if h, err := ProjectHubNameStrict(dir); err != nil || h != "home" {
		t.Fatalf("BOM config strictly: %q err=%v", h, err)
	}
	writeConfig(t, dir, `{"hub": "Not A Name"}`)
	if _, err := ProjectHubNameStrict(dir); err == nil || errors.Is(err, ErrConfigUnreadable) {
		t.Fatalf("invalid name strictly: err=%v; want a name error", err)
	}
}

// A UTF-8 BOM in .aimem.json used to make encoding/json fail, and callers
// treat that failure as "no config" — so the hub binding vanished and the
// project's data went to the machine's default hub instead. Windows tools
// write BOMs routinely; parsing must survive one.
func TestConfigWithUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	body := append([]byte("\xef\xbb\xbf"), []byte(`{"hub":"home","groups":["ai-infra"],"project":"pinned"}`)...)
	if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	hub, err := ProjectHubName(dir)
	if err != nil || hub != "home" {
		t.Fatalf("hub = %q, %v; want \"home\", nil", hub, err)
	}
	groups, err := ProjectGroups(dir)
	if err != nil || len(groups) != 1 || groups[0] != "group-ai-infra" {
		t.Fatalf("groups = %v, %v; want [group-ai-infra], nil", groups, err)
	}
	id, err := ProjectID(dir)
	if err != nil || id != "pinned" {
		t.Fatalf("id = %q, %v; want \"pinned\", nil", id, err)
	}
}

func TestProjectDocsBinding(t *testing.T) {
	dir := t.TempDir()
	// No handoff file, no config: nothing bound.
	if docs := ProjectDocs(dir); len(docs) != 0 {
		t.Fatalf("empty project bound %v", docs)
	}
	// Handoff present: bound by default.
	os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
	os.WriteFile(filepath.Join(dir, "docs", "SESSION-STATE.md"), []byte("x"), 0o644)
	if docs := ProjectDocs(dir); len(docs) != 1 || docs[0] != DefaultDocPath {
		t.Fatalf("default binding missing: %v", docs)
	}
	// Declared extras join the default; escapes are dropped.
	os.WriteFile(filepath.Join(dir, ".aimem.json"),
		[]byte(`{"docs":["docs/RUNBOOK.md","../escape.md","/abs.md"]}`), 0o600)
	docs := ProjectDocs(dir)
	if len(docs) != 2 || docs[0] != DefaultDocPath || docs[1] != "docs/RUNBOOK.md" {
		t.Fatalf("extras wrong: %v", docs)
	}
	// Explicit empty list opts out of everything, default included.
	os.WriteFile(filepath.Join(dir, ".aimem.json"), []byte(`{"docs":[]}`), 0o600)
	if docs := ProjectDocs(dir); len(docs) != 0 {
		t.Fatalf("opt-out ignored: %v", docs)
	}
	if DocName("docs/SESSION-STATE.md") != "SESSION-STATE" || DocName("a/b/RUNBOOK.md") != "RUNBOOK" {
		t.Fatal("DocName derivation wrong")
	}
}

func TestMalformedConfigTreatedAsAbsent(t *testing.T) {
	dir := t.TempDir()
	// An UNPARSEABLE config must never block checkpoints (CHANGELOG
	// v0.1.77): identity falls back to derivation, groups and hub read
	// as unset.
	writeConfig(t, dir, `{not json`)
	id, err := ProjectID(dir)
	if err != nil || id == "" {
		t.Fatalf("malformed config blocked identity: id=%q err=%v", id, err)
	}
	if g, err := ProjectGroups(dir); err != nil || g != nil {
		t.Fatalf("malformed config: groups=%v err=%v", g, err)
	}
	if h, err := ProjectHubName(dir); err != nil || h != "" {
		t.Fatalf("malformed config: hub=%q err=%v", h, err)
	}
	// An invalid VALUE in a parseable config stays a hard error: a silent
	// fallback to a derived id would split the journal across machines.
	writeConfig(t, dir, `{"project": "user"}`)
	if _, err := ProjectID(dir); err == nil {
		t.Fatal("reserved pin in a valid config must error")
	}
}
