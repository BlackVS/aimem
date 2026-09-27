package teamsession

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/privatefile"
)

const testHandle = "acs1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func sample() File {
	return File{Version: 1, Hub: "hub", URL: "https://hub.example:8440", UserID: "user-1", TokenID: "tok-1",
		ServiceID: "aicrew-example", TeamID: "team-1", SessionID: "sess-1", Generation: "4",
		Handle: testHandle, HandleExpiresAt: "2026-09-27T12:00:00Z", UpdatedAt: time.Now().UTC()}
}

func TestSaveLoadPrivateAndAtomic(t *testing.T) {
	root := t.TempDir()
	path := PathFor(root, "sess:1") // ':' is valid in a session ID, not in a Windows file name
	if strings.Contains(filepath.Base(path), ":") || filepath.Dir(filepath.Dir(path)) != root {
		t.Fatalf("session path %q", path)
	}
	f := sample()
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Check(path); err != nil {
		t.Fatalf("the session file is not private: %v", err)
	}
	got, err := Load(path)
	if err != nil || got.Binding() != f.Binding() || got.Handle != f.Handle {
		t.Fatalf("load: %+v %v", got, err)
	}
	// A save replaces the file as a whole and leaves no temporary behind.
	f.Handle = "acs1_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	f.Generation = "5"
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(path); err != nil || got.Handle != f.Handle || got.Generation != "5" {
		t.Fatalf("after replace: %+v %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("the session directory holds %d entries", len(entries))
	}
	if err := privatefile.Check(path); err != nil {
		t.Fatalf("the replaced file is not private: %v", err)
	}
	// A file other accounts can read is refused.
	if err := privatefile.Expose(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a file readable by other accounts was loaded")
	}
}

func TestLoadRefusesInvalidFiles(t *testing.T) {
	for name, edit := range map[string]func(f *File){
		"version":     func(f *File) { f.Version = 2 },
		"handle":      func(f *File) { f.Handle = "amr1_" + strings.Repeat("A", 43) },
		"plain hub":   func(f *File) { f.URL = "http://hub.example" },
		"no session":  func(f *File) { f.SessionID = "" },
		"bad team ID": func(f *File) { f.TeamID = "team one" },
	} {
		f := sample()
		edit(&f)
		if err := Save(filepath.Join(t.TempDir(), "s.json"), f); err == nil {
			t.Errorf("%s: an invalid file was saved", name)
		}
	}
	path := filepath.Join(t.TempDir(), "s.json")
	w, err := privatefile.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(`{"version":1,"extra":"x"}`)
	w.Close()
	if _, err := Load(path); err == nil {
		t.Fatal("a file with unknown fields was loaded")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("a missing file was loaded")
	}
}

func TestHubClientAndCredential(t *testing.T) {
	for name, h := range map[string]*adapter.HubConfig{
		"plain http": {URL: "http://hub.example:8440"},
		"insecure":   {URL: "https://hub.example:8440", Insecure: true},
		"no CA":      {URL: "https://hub.example:8440", CAFile: filepath.Join(t.TempDir(), "absent.pem")},
	} {
		if _, err := HubClient(h); err == nil {
			t.Errorf("%s: a team-mode client was built", name)
		}
	}
	c, err := HubClient(&adapter.HubConfig{URL: "https://hub.example:8440"})
	if err != nil || c.CheckRedirect == nil {
		t.Fatalf("a verified client: %v", err)
	}
	if _, err := Credential(&adapter.HubConfig{TaskToken: "not-a-user-token"}); err == nil {
		t.Fatal("a non-individual credential was accepted")
	}
	if tok, err := Credential(&adapter.HubConfig{TaskToken: "aimem_user_x"}); err != nil || tok != "aimem_user_x" {
		t.Fatalf("individual credential: %q %v", tok, err)
	}
}

func TestParseRefusal(t *testing.T) {
	r := ParseRefusal(403, []byte(`{"code":"context_stale","message":"m","next_action":"n","correlation_id":"c","active_mode":"team"}`))
	if r == nil || r.Code != "context_stale" || !strings.Contains(r.Error(), "Next: n") || !strings.Contains(r.Error(), "correlation c") {
		t.Fatalf("refusal: %+v", r)
	}
	if ParseRefusal(403, []byte(`{"error":"plain"}`)) != nil {
		t.Fatal("a plain error was read as an envelope")
	}
}
