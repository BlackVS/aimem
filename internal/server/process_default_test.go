package server

import (
	"encoding/json"
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
	if w := taskReq(t, f.h, "GET", "/v1/process-default", f.admin, "", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"default":null}` {
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
