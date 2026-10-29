package server

// The hub-served process set (docs/DESIGN-portable-team-context.md,
// increment 1d): a selection carries its files or names a kept set, the
// hub stores the set with the selection in one step, keeps the sets of the
// last five selections, and serves the current one (or a kept earlier one
// by commit and manifest) to a caller with a current grant on the project,
// never naming the repository URL.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"aimem/internal/process"
)

// withSet adds to a selection body the files of a minimal set for its
// manifest: the manifest naming one handbook, and that handbook.
func withSet(t *testing.T, body, handbook string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	manifest, _ := m["manifest"].(string)
	m["files"] = setFiles(manifest, handbook)
	out, _ := json.Marshal(m)
	return string(out)
}

// setFiles is the smallest valid set for a manifest at manifest.
func setFiles(manifest, handbook string) map[string][]byte {
	dir := path.Dir(manifest)
	if dir == "." {
		dir = ""
	} else {
		dir += "/"
	}
	return map[string][]byte{
		manifest:            []byte(`{"version":1,"handbook":"` + dir + `handbook.md"}`),
		dir + "handbook.md": []byte(handbook),
	}
}

type setAnswer struct {
	Project   string `json:"project"`
	Selection struct {
		Name, Commit, Manifest, Digest string
		SelectedAt                     string `json:"selected_at"`
	} `json:"selection"`
	Set struct {
		Version  int               `json:"version"`
		Manifest string            `json:"manifest"`
		Files    map[string]string `json:"files"`
	} `json:"set"`
}

type historyAnswer struct {
	Current *process.Ref  `json:"current"`
	History []process.Ref `json:"history"`
	Sets    []keptSet     `json:"sets"`
}

func TestProcessSetIsStoredWithTheSelectionAndServedUnderAGrant(t *testing.T) {
	f := newTaskFixture(t)
	const repo = "https://example.com/team/p.git"
	commit := func(i int) string { return strings.Repeat(fmt.Sprintf("%x", i), 40) }
	sel := func(token, body string) (int, string) {
		w := taskReq(t, f.h, "PUT", "/v1/projects/alpha/process", token, "", body)
		return w.Code, w.Body.String()
	}
	getSet := func(h http.Handler, token, query string) (int, string, http.Header) {
		w := taskReq(t, h, "GET", "/v1/projects/alpha/process/set"+query, token, "", "")
		return w.Code, w.Body.String(), w.Header()
	}
	history := func() historyAnswer {
		w := taskReq(t, f.h, "GET", "/v1/projects/alpha/process?history=1", f.admin, "", "")
		var out historyAnswer
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("history: %d %s", w.Code, w.Body)
		}
		return out
	}
	body := func(i int, expected string) string {
		return `{"repo":"` + repo + `","commit":"` + commit(i) + `","manifest":"m.json","expected_commit":"` + expected + `"}`
	}

	// Before any selection: the grant rule applies first, then 404.
	if code, b, _ := getSet(f.h, f.alice, ""); code != 404 || !strings.Contains(b, "no process reference") {
		t.Fatalf("granted, nothing selected: %d %s", code, b)
	}
	if code, _, _ := getSet(f.h, f.bob, ""); code != 403 {
		t.Fatalf("no grant: %d", code)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/nope/process/set", f.alice, "", ""); w.Code != 403 {
		t.Fatalf("unknown project reads as ungranted: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/process/set", "", "", ""); w.Code != 401 {
		t.Fatalf("anonymous: %d", w.Code)
	}

	// A selection without its set is refused: the hub runs no Git.
	if code, b := sel(f.admin, body(1, "")); code != 400 || !strings.Contains(b, "a selection carries its set") {
		t.Fatalf("bare reference: %d %s", code, b)
	}
	// A set that does not validate is refused whole: nothing is stored.
	broken := withSet(t, body(1, ""), "one")
	broken = strings.Replace(broken, `"handbook.md":`, `"other.md":`, 1)
	if code, b := sel(f.admin, broken); code != 400 || !strings.Contains(b, "nothing stored") {
		t.Fatalf("invalid set: %d %s", code, b)
	}
	if h := history(); h.Current != nil || len(h.Sets) != 0 || len(h.History) != 0 {
		t.Fatalf("a refused set left something: %+v", h)
	}
	// A stated digest must be the files' own.
	stated := strings.TrimSuffix(withSet(t, body(1, ""), "one"), "}") + `,"digest":"` + strings.Repeat("0", 64) + `"}`
	if code, b := sel(f.admin, stated); code != 400 || !strings.Contains(b, "not the stated") {
		t.Fatalf("wrong stated digest: %d %s", code, b)
	}

	// The first import: the selection carries the digest and the name.
	d1 := process.Digest(setFiles("m.json", "one"))
	code, b := sel(f.admin, withSet(t, body(1, ""), "one"))
	var put struct {
		Current  process.Ref `json:"current"`
		Imported bool        `json:"imported"`
	}
	if code != 200 || json.Unmarshal([]byte(b), &put) != nil || !put.Imported || put.Current.Digest != d1 || put.Current.Name != "p" {
		t.Fatalf("import: %d %s", code, b)
	}
	// The reference read carries them too, for an ordinary token.
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/process", f.alice, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"digest":"`+d1+`"`) || !strings.Contains(w.Body.String(), `"name":"p"`) {
		t.Fatalf("reference with digest: %d %s", w.Code, w.Body)
	}

	// The set: for the granted token, the admin and the socket operator;
	// never for the ungranted one; verified by digest on the client, so the
	// answer is exactly the stored files; nothing in it names the URL.
	for _, c := range []struct {
		name  string
		h     http.Handler
		token string
	}{{"granted", f.h, f.alice}, {"admin", f.h, f.admin}, {"socket", f.s.Handler(), ""}} {
		code, b, hdr := getSet(c.h, c.token, "")
		var got setAnswer
		if code != 200 || json.Unmarshal([]byte(b), &got) != nil {
			t.Fatalf("%s: %d %s", c.name, code, b)
		}
		if got.Selection.Digest != d1 || got.Selection.Commit != commit(1) || got.Selection.Name != "p" || got.Set.Manifest != "m.json" || hdr.Get("ETag") != `"`+d1+`"` {
			t.Fatalf("%s: %+v %v", c.name, got, hdr)
		}
		files := map[string][]byte{}
		for p, enc := range got.Set.Files {
			raw, err := base64.StdEncoding.DecodeString(enc)
			if err != nil {
				t.Fatal(err)
			}
			files[p] = raw
		}
		if process.Digest(files) != d1 || string(files["handbook.md"]) != "one" {
			t.Fatalf("%s: the served files are not the stored set: %v", c.name, got.Set.Files)
		}
		if strings.Contains(b, "example.com") {
			t.Fatalf("%s: the set answer names the repository URL:\n%s", c.name, b)
		}
	}
	if code, _, _ := getSet(f.h, f.bob, ""); code != 403 {
		t.Fatalf("no grant after selection: %d", code)
	}
	cond := httptest.NewRequest("GET", "/v1/projects/alpha/process/set", nil)
	cond.Header.Set("Authorization", "Bearer "+f.alice)
	cond.Header.Set("If-None-Match", `"`+d1+`"`)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, cond)
	if rec.Code != 304 {
		t.Fatalf("If-None-Match: %d %s", rec.Code, rec.Body)
	}

	// A stale compare-and-swap stores nothing, not even the set.
	if code, _ := sel(f.admin, withSet(t, body(2, commit(9)), "two")); code != 409 {
		t.Fatalf("stale: %d", code)
	}
	if h := history(); len(h.Sets) != 1 || h.Sets[0].Digest != d1 {
		t.Fatalf("a refused change stored a set: %+v", h.Sets)
	}

	// A second import, then the first re-selected by digest: no import,
	// the kept list has both, newest first, the history all three.
	d2 := process.Digest(setFiles("m.json", "two"))
	if code, b := sel(f.admin, withSet(t, body(2, commit(1)), "two")); code != 200 || !strings.Contains(b, `"imported":true`) {
		t.Fatalf("second import: %d %s", code, b)
	}
	if code, b := sel(f.admin, `{"repo":"`+repo+`","commit":"`+commit(1)+`","manifest":"m.json","digest":"`+d1+`","expected_commit":"`+commit(2)+`"}`); code != 200 || !strings.Contains(b, `"imported":false`) || !strings.Contains(b, `"digest":"`+d1+`"`) {
		t.Fatalf("re-select by digest: %d %s", code, b)
	}
	if h := history(); len(h.Sets) != 2 || h.Sets[0].Digest != d1 || h.Sets[1].Digest != d2 || h.Sets[0].Bytes != len(setFiles("m.json", "one")["m.json"])+3 || len(h.History) != 3 || h.History[0].Digest != d1 || h.History[1].Digest != d2 {
		t.Fatalf("kept sets and history: %+v", h)
	}
	// A digest this hub does not keep, or kept for another manifest, is refused.
	if code, b := sel(f.admin, `{"repo":"`+repo+`","commit":"`+commit(3)+`","manifest":"m.json","digest":"`+strings.Repeat("f", 64)+`","expected_commit":"`+commit(1)+`"}`); code != 400 || !strings.Contains(b, "keeps no set") {
		t.Fatalf("unknown digest: %d %s", code, b)
	}
	if code, b := sel(f.admin, `{"repo":"`+repo+`","commit":"`+commit(3)+`","manifest":"other/m.json","digest":"`+d2+`","expected_commit":"`+commit(1)+`"}`); code != 400 || !strings.Contains(b, "not other/m.json") {
		t.Fatalf("digest for another manifest: %d %s", code, b)
	}
	// The earlier selection's set is served by commit and manifest (a
	// pinned version), with its own ETag.
	code, b, hdr := getSet(f.h, f.alice, "?commit="+commit(2)+"&manifest=m.json")
	if code != 200 || !strings.Contains(b, `"digest":"`+d2+`"`) || hdr.Get("ETag") != `"`+d2+`"` {
		t.Fatalf("kept earlier version: %d %s", code, b)
	}
	if code, b, _ := getSet(f.h, f.alice, "?commit="+commit(8)+"&manifest=m.json"); code != 404 || !strings.Contains(b, "no selection of that commit") {
		t.Fatalf("never selected: %d %s", code, b)
	}
	if code, _, _ := getSet(f.h, f.alice, "?commit=abc&manifest=m.json"); code != 400 {
		t.Fatalf("malformed version: %d", code)
	}

	// Five more imports: the kept list holds the last five selections'
	// sets; the two older sets are gone, their selections still recorded.
	prev := commit(1)
	for i := 3; i <= 7; i++ {
		if code, b := sel(f.admin, withSet(t, body(i, prev), fmt.Sprint("set ", i))); code != 200 {
			t.Fatalf("import %d: %d %s", i, code, b)
		}
		prev = commit(i)
	}
	h := history()
	if len(h.Sets) != processSetsKept || h.Sets[0].Digest != process.Digest(setFiles("m.json", "set 7")) || h.Sets[4].Digest != process.Digest(setFiles("m.json", "set 3")) || len(h.History) != 8 {
		t.Fatalf("pruned kept list: %+v", h)
	}
	for _, c := range []int{1, 2} {
		if code, b, _ := getSet(f.h, f.alice, "?commit="+commit(c)+"&manifest=m.json"); code != 404 || !strings.Contains(b, "no longer kept") {
			t.Fatalf("dropped set %d: %d %s", c, code, b)
		}
	}
	if code, _, _ := getSet(f.h, f.alice, "?commit="+commit(3)+"&manifest=m.json"); code != 200 {
		t.Fatalf("oldest kept: %d", code)
	}
	if code, b := sel(f.admin, `{"repo":"`+repo+`","commit":"`+commit(1)+`","manifest":"m.json","digest":"`+d1+`","expected_commit":"`+commit(7)+`"}`); code != 400 {
		t.Fatalf("a dropped set re-selected without files: %d %s", code, b)
	}

	// A selection recorded before sets were stored (an older hub's) still
	// reads as a reference; its set is 404, so a member falls back to Git.
	db, err := f.reg.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(process.Ref{Repo: repo, Commit: commit(8), Manifest: "m.json", SelectedAt: "2026-01-01T00:00:00Z"})
	if err := db.SetMeta(processMetaKey, string(old)); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/process", f.alice, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), commit(8)) {
		t.Fatalf("older selection as a reference: %d %s", w.Code, w.Body)
	}
	if code, b, _ := getSet(f.h, f.alice, ""); code != 404 || !strings.Contains(b, "before this hub stored sets") {
		t.Fatalf("older selection's set: %d %s", code, b)
	}

	// Clear takes no set; after it the set is 404 and the kept sets stay.
	if code, _ := sel(f.admin, withSet(t, `{"clear":true,"manifest":"m.json","expected_commit":"`+commit(8)+`"}`, "x")); code != 400 {
		t.Fatalf("clear with files: %d", code)
	}
	if code, _ := sel(f.admin, `{"clear":true,"expected_commit":"`+commit(8)+`"}`); code != 200 {
		t.Fatalf("clear: %d", code)
	}
	if code, b, _ := getSet(f.h, f.alice, ""); code != 404 || !strings.Contains(b, "no process reference") {
		t.Fatalf("set after clear: %d %s", code, b)
	}
	if h := history(); len(h.Sets) != processSetsKept {
		t.Fatalf("clear dropped kept sets: %+v", h.Sets)
	}

	// A revoked grant ends the read.
	acc, err := f.s.openAccess(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.SetGrant("admin", f.alphaInstance, "user", f.aliceUser, false); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := getSet(f.h, f.alice, "?commit="+commit(7)+"&manifest=m.json"); code != 403 {
		t.Fatalf("revoked grant: %d", code)
	}
}

// A team session reads the process reference of a project its profile is
// granted, without who selected it, and is refused every other project
// and the history.
func TestProcessReferenceForATeamSession(t *testing.T) {
	g := newTeamRig(t)
	commit := strings.Repeat("7", 40)
	body := withSet(t, `{"repo":"https://forge.example.org/team/process.git","commit":"`+commit+`","manifest":"proc/m.json","expected_commit":""}`, "team rules")
	for _, p := range []string{"alpha", "beta"} {
		if r := g.call(t, g.tls, "PUT", "/v1/projects/"+p+"/process", g.env, nil, body, true); r.status != 200 {
			t.Fatalf("select %s: %d %s", p, r.status, r.body)
		}
	}
	r := g.team(t, "GET", "/v1/projects/alpha/process")
	var got struct {
		Current map[string]any `json:"current"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &got) != nil || got.Current["commit"] != commit || got.Current["digest"] == nil {
		t.Fatalf("team mode, granted: %d %s", r.status, r.body)
	}
	if _, ok := got.Current["selected_by"]; ok {
		t.Fatalf("team mode names who selected: %s", r.body)
	}
	for _, path := range []string{"/v1/projects/beta/process", "/v1/projects/user/process", "/v1/projects/nope/process"} {
		if r := g.team(t, "GET", path); r.status != 403 || r.code() != "grant_denied" {
			t.Fatalf("team mode %s: %d %s", path, r.status, r.body)
		}
	}
	if r := g.team(t, "GET", "/v1/projects/alpha/process?history=1"); r.status != 400 || r.code() != "invalid_request" {
		t.Fatalf("team mode history: %d %s", r.status, r.body)
	}
}

// A team session reads the set of a project its profile is granted, and
// nothing else; the answer never names the repository URL.
func TestProcessSetForATeamSession(t *testing.T) {
	g := newTeamRig(t)
	commit := strings.Repeat("7", 40)
	body := withSet(t, `{"repo":"https://forge.example.org/team/process.git","commit":"`+commit+`","manifest":"proc/m.json","expected_commit":""}`, "team rules")
	if r := g.call(t, g.tls, "PUT", "/v1/projects/alpha/process", g.env, nil, body, true); r.status != 200 {
		t.Fatalf("select: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "PUT", "/v1/projects/beta/process", g.env, nil, body, true); r.status != 200 {
		t.Fatalf("select beta: %d %s", r.status, r.body)
	}
	r := g.team(t, "GET", "/v1/projects/alpha/process/set")
	if r.status != 200 || !strings.Contains(string(r.body), `"name":"proc"`) || strings.Contains(string(r.body), "forge.example.org") {
		t.Fatalf("team mode, granted: %d %s", r.status, r.body)
	}
	for _, path := range []string{"/v1/projects/beta/process/set", "/v1/projects/user/process/set", "/v1/projects/nope/process/set"} {
		if r := g.team(t, "GET", path); r.status != 403 || r.code() != "grant_denied" || strings.Contains(string(r.body), "forge.example.org") {
			t.Fatalf("team mode %s: %d %s", path, r.status, r.body)
		}
	}
}
