package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
)

// The hub's default process is admin configuration in the state root:
// only an admin reads or changes it, a body without its tag or digest is
// refused, the hub stamps it, and clearing it removes the file.
func TestProcessDefaultRoutes(t *testing.T) {
	f := newTaskFixture(t)
	commit, digest := strings.Repeat("a", 40), strings.Repeat("b", 64)
	body := func(ref, dig string) string {
		return `{"repo":"https://example.com/p.git","ref":"` + ref + `","commit":"` + commit + `","manifest":"m.json","digest":"` + dig + `"}`
	}
	for _, tok := range []string{f.alice, f.writer} {
		for _, m := range []string{"GET", "PUT", "DELETE"} {
			if w := taskReq(t, f.h, m, "/v1/process-default", tok, "", body("v1", digest)); w.Code != 403 && w.Code != 401 {
				t.Fatalf("%s by a non-admin: %d %s", m, w.Code, w.Body)
			}
		}
	}
	if w := taskReq(t, f.h, "GET", "/v1/process-default", f.admin, "", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"default":null,"set_kept":false}` {
		t.Fatalf("none set: %d %s", w.Code, w.Body)
	}
	for name, b := range map[string]string{
		"no tag":        body("", digest),
		"no digest":     body("v1", ""),
		"bad commit":    strings.Replace(body("v1", digest), commit, "abc", 1),
		"unknown field": strings.Replace(body("v1", digest), `"repo"`, `"x":1,"repo"`, 1),
		"stamp":         strings.Replace(body("v1", digest), `"repo"`, `"selected_by":"mallory","repo"`, 1),
	} {
		w := taskReq(t, f.h, "PUT", "/v1/process-default", f.admin, "", b)
		if name == "stamp" {
			var got struct{ Default process.Ref }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Default.SelectedBy == "mallory" {
				t.Fatalf("a caller's stamp was kept: %d %s", w.Code, w.Body)
			}
			continue
		}
		if w.Code != 400 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	w := taskReq(t, f.h, "PUT", "/v1/process-default", f.admin, "", body("v1", digest))
	var got struct{ Default process.Ref }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("set: %d %s", w.Code, w.Body)
	}
	d := got.Default
	if d.Ref != "v1" || d.Commit != commit || d.Digest != digest || d.Name == "" || d.SelectedAt == "" || d.SelectedBy == "" {
		t.Fatalf("stored %+v", d)
	}
	path := filepath.Join(f.reg.Root(), processDefaultFile)
	if fi, err := os.Stat(path); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Fatalf("the default file: %v %v", fi, err)
	}
	if w := taskReq(t, f.h, "GET", "/v1/process-default", f.admin, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"ref":"v1"`) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	// Setting the default selects nothing for any project.
	if w := taskReq(t, f.h, "GET", "/v1/processes", f.admin, "", ""); w.Code != 200 || strings.Contains(w.Body.String(), commit) {
		t.Fatalf("the default shows as a selection in use: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "DELETE", "/v1/process-default", f.admin, "", ""); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file is still there: %v", err)
	}
	if w := taskReq(t, f.h, "DELETE", "/v1/process-default", f.admin, "", ""); w.Code != 404 {
		t.Fatalf("second clear: %d %s", w.Code, w.Body)
	}
}

// The default's set: kept when the body carries files that are the
// stated set, refused when they are not, served only to an admin and only
// while the default names its digest, dropped by a body without files and
// by clear, and bounded like a selection body.
func TestProcessDefaultKeptSet(t *testing.T) {
	f := newTaskFixture(t)
	files := setFiles("p/manifest.json", "# Handbook\n")
	digest := process.Digest(files)
	body := func(dig string, files map[string][]byte) string {
		m := map[string]any{"repo": "https://example.com/p.git", "ref": "v1", "commit": strings.Repeat("a", 40), "manifest": "p/manifest.json", "digest": dig}
		if files != nil {
			m["files"] = files
		}
		raw, _ := json.Marshal(m)
		return string(raw)
	}
	keptPath := filepath.Join(f.reg.Root(), processDefaultSetFile)
	set := func(b string) *httptest.ResponseRecorder {
		return taskReq(t, f.h, "PUT", "/v1/process-default", f.admin, "", b)
	}
	if w := taskReq(t, f.h, "GET", "/v1/process-default/set", f.admin, "", ""); w.Code != 404 {
		t.Fatalf("no default: %d %s", w.Code, w.Body)
	}
	other := setFiles("p/manifest.json", "# Other\n")
	invalid := map[string][]byte{"p/manifest.json": []byte("{")}
	for name, b := range map[string]string{
		"another digest": body(digest, other),
		"invalid set":    body(process.Digest(invalid), invalid),
	} {
		if w := set(b); w.Code != 400 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if _, err := os.Stat(keptPath); !os.IsNotExist(err) {
		t.Fatalf("a refused body kept a set: %v", err)
	}

	if w := set(body(digest, files)); w.Code != 200 || !strings.Contains(w.Body.String(), `"set_kept":true`) {
		t.Fatalf("set with files: %d %s", w.Code, w.Body)
	}
	if fi, err := os.Stat(keptPath); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Fatalf("the kept set file: %v %v", fi, err)
	}
	if w := taskReq(t, f.h, "GET", "/v1/process-default", f.admin, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"set_kept":true`) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	for _, tok := range []string{f.alice, f.writer} {
		if w := taskReq(t, f.h, "GET", "/v1/process-default/set", tok, "", ""); w.Code != 403 && w.Code != 401 {
			t.Fatalf("the set read by a non-admin: %d %s", w.Code, w.Body)
		}
	}
	w := taskReq(t, f.h, "GET", "/v1/process-default/set", f.admin, "", "")
	var got struct {
		Digest string
		Set    process.Bundle
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Digest != digest || got.Set.Digest() != digest || got.Set.Manifest != "p/manifest.json" {
		t.Fatalf("the kept set: %d %s", w.Code, w.Body)
	}

	// A kept set the default does not name (a write that stopped between
	// the two files) is never served.
	stale, _ := json.Marshal(map[string]any{"version": 1, "manifest": "p/manifest.json", "files": other})
	if err := os.WriteFile(keptPath, stale, 0o600); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, f.h, "GET", "/v1/process-default/set", f.admin, "", ""); w.Code != 404 {
		t.Fatalf("a set of another digest was served: %d %s", w.Code, w.Body)
	}

	// A body without files (an older command) drops the kept set.
	if w := set(body(digest, files)); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := set(body(digest, nil)); w.Code != 200 || !strings.Contains(w.Body.String(), `"set_kept":false`) {
		t.Fatalf("set without files: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(keptPath); !os.IsNotExist(err) {
		t.Fatalf("a body without files left the kept set: %v", err)
	}

	// Bounded like a selection body.
	big := map[string][]byte{"p/manifest.json": files["p/manifest.json"], "p/handbook.md": []byte(strings.Repeat("x", processBodyMax))}
	if w := set(body(process.Digest(big), big)); w.Code != 400 {
		t.Fatalf("an oversized body: %d", w.Code)
	}

	// clear removes the kept set with the default.
	if w := set(body(digest, files)); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := taskReq(t, f.h, "DELETE", "/v1/process-default", f.admin, "", ""); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(keptPath); !os.IsNotExist(err) {
		t.Fatalf("clear left the kept set: %v", err)
	}
	if w := taskReq(t, f.h, "GET", "/v1/process-default/set", f.admin, "", ""); w.Code != 404 {
		t.Fatalf("after clear: %d %s", w.Code, w.Body)
	}
}
