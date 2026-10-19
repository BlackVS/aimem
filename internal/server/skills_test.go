package server

// The hub-served skills set (docs/DESIGN-portable-team-context.md,
// increment 1e): an admin imports a set per source, the hub keeps five per
// source and serves the index and the files to a caller with a current
// grant on a project; nothing imports through the API but the admin's
// PUT, and nothing in an answer names where a set came from.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/skills"
)

func skillsBody(version string, files map[string]string) string {
	enc := map[string][]byte{}
	for p, c := range files {
		enc[p] = []byte(c)
	}
	raw, _ := json.Marshal(map[string]any{"version": version, "files": enc})
	return string(raw)
}

func skillsDigest(files map[string]string) string {
	enc := map[string][]byte{}
	for p, c := range files {
		enc[p] = []byte(c)
	}
	return (&skills.Set{Files: enc}).Digest()
}

type skillsIndex struct {
	Project   string                    `json:"project"`
	Sources   map[string]*skills.Entry  `json:"sources"`
	Kept      map[string][]skills.Entry `json:"kept"`
	Effective []skills.Skill            `json:"effective"`
}

func TestSkillsSetsAreImportedByTheAdminAndServedUnderAGrant(t *testing.T) {
	f := newTaskFixture(t)
	release := map[string]string{"oh-code-review/SKILL.md": "review v1", "verify-delivery/SKILL.md": "verify"}
	put := func(token, source, body string) (int, string) {
		w := taskReq(t, f.h, "PUT", "/v1/skills/"+source, token, "", body)
		return w.Code, w.Body.String()
	}
	index := func(h http.Handler, token string) (int, skillsIndex, string) {
		w := taskReq(t, h, "GET", "/v1/projects/alpha/skills", token, "", "")
		var ix skillsIndex
		json.Unmarshal(w.Body.Bytes(), &ix)
		return w.Code, ix, w.Body.String()
	}
	getSet := func(h http.Handler, token, query string) (int, string, http.Header) {
		w := taskReq(t, h, "GET", "/v1/projects/alpha/skills/set"+query, token, "", "")
		return w.Code, w.Body.String(), w.Header()
	}

	// Nothing imported: the grant rule first, then an empty index and 404
	// for a set.
	if code, ix, b := index(f.h, f.alice); code != 200 || ix.Project != "alpha" || ix.Sources["release"] != nil || len(ix.Effective) != 0 {
		t.Fatalf("empty index: %d %s", code, b)
	}
	if code, _, _ := index(f.h, f.bob); code != 403 {
		t.Fatalf("index without a grant: %d", code)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/skills", "", "", ""); w.Code != 401 {
		t.Fatalf("anonymous: %d", w.Code)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/nope/skills/set?source=release", f.alice, "", ""); w.Code != 403 {
		t.Fatalf("unknown project reads as ungranted: %d", w.Code)
	}
	if code, b, _ := getSet(f.h, f.alice, "?source=release"); code != 404 || !strings.Contains(b, "no release skills set is imported") {
		t.Fatalf("no set: %d %s", code, b)
	}
	if code, b, _ := getSet(f.h, f.alice, ""); code != 400 || !strings.Contains(b, "source=release") {
		t.Fatalf("no source: %d %s", code, b)
	}

	// Only the admin imports; an ordinary token is refused before the body.
	if w := taskReq(t, f.h, "PUT", "/v1/skills/release", f.alice, "", skillsBody("1.0", release)); w.Code != 403 {
		t.Fatalf("ordinary import: %d %s", w.Code, w.Body)
	}
	if code, b := put(f.admin, "github", skillsBody("1.0", release)); code != 400 || !strings.Contains(b, "release or instance") {
		t.Fatalf("bad source: %d %s", code, b)
	}
	if code, b := put(f.admin, "release", skillsBody("1.0", map[string]string{"oh-code-review/notes.md": "no skill file"})); code != 400 || !strings.Contains(b, "nothing stored") {
		t.Fatalf("invalid set: %d %s", code, b)
	}
	if code, ix, _ := index(f.h, f.alice); code != 200 || ix.Sources["release"] != nil {
		t.Fatal("a refused import left something")
	}
	d1 := skillsDigest(release)
	code, b := put(f.admin, "release", skillsBody("1.33.13", release))
	if code != 200 || !strings.Contains(b, `"imported":true`) || !strings.Contains(b, `"digest":"`+d1+`"`) || !strings.Contains(b, `"version":"1.33.13"`) {
		t.Fatalf("import: %d %s", code, b)
	}

	// The index: for the granted token, the admin and the socket; the
	// effective list names each skill's source.
	for _, c := range []struct {
		name  string
		h     http.Handler
		token string
	}{{"granted", f.h, f.alice}, {"admin", f.h, f.admin}, {"socket", f.s.Handler(), ""}} {
		code, ix, b := index(c.h, c.token)
		if code != 200 || ix.Sources["release"] == nil || ix.Sources["release"].Digest != d1 || ix.Sources["instance"] != nil || len(ix.Effective) != 2 || ix.Effective[0].Name != "oh-code-review" || ix.Effective[0].Source != "release" || ix.Effective[0].Version != "1.33.13" {
			t.Fatalf("%s index: %d %s", c.name, code, b)
		}
	}
	if code, _, _ := index(f.h, f.bob); code != 403 {
		t.Fatalf("index without a grant after import: %d", code)
	}
	// The set by source and by digest, verified against the digest; ETag.
	for _, q := range []string{"?source=release", "?digest=" + d1} {
		code, b, hdr := getSet(f.h, f.alice, q)
		var got struct {
			Project, Source, Version, Digest string
			Skills                           []string
			Files                            map[string]string
		}
		if code != 200 || json.Unmarshal([]byte(b), &got) != nil || got.Project != "alpha" || got.Source != "release" || got.Version != "1.33.13" || got.Digest != d1 || len(got.Skills) != 2 || hdr.Get("ETag") != `"`+d1+`"` {
			t.Fatalf("set %s: %d %s", q, code, b)
		}
		files := map[string][]byte{}
		for p, enc := range got.Files {
			raw, err := base64.StdEncoding.DecodeString(enc)
			if err != nil {
				t.Fatal(err)
			}
			files[p] = raw
		}
		if (&skills.Set{Files: files}).Digest() != d1 || string(files["verify-delivery/SKILL.md"]) != "verify" {
			t.Fatalf("served files are not the stored set: %v", got.Files)
		}
	}
	if code, _, _ := getSet(f.h, f.bob, "?source=release"); code != 403 {
		t.Fatalf("set without a grant: %d", code)
	}
	cond := httptest.NewRequest("GET", "/v1/projects/alpha/skills/set?source=release", nil)
	cond.Header.Set("Authorization", "Bearer "+f.alice)
	cond.Header.Set("If-None-Match", `"`+d1+`"`)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, cond)
	if rec.Code != 304 {
		t.Fatalf("If-None-Match: %d %s", rec.Code, rec.Body)
	}
	if code, b, _ := getSet(f.h, f.alice, "?digest="+strings.Repeat("0", 64)); code != 404 || !strings.Contains(b, "keeps no skills set") {
		t.Fatalf("unknown digest: %d %s", code, b)
	}

	// The instance set overrides by name; the release's other skill stays;
	// the previous release import is kept and served by digest.
	instance := map[string]string{"oh-code-review/SKILL.md": "house rules", "house-style/SKILL.md": "style"}
	di := skillsDigest(instance)
	if code, b := put(f.admin, "instance", skillsBody("site-1", instance)); code != 200 {
		t.Fatalf("instance import: %d %s", code, b)
	}
	release2 := map[string]string{"oh-code-review/SKILL.md": "review v2", "verify-delivery/SKILL.md": "verify"}
	d2 := skillsDigest(release2)
	if code, b := put(f.admin, "release", skillsBody("1.33.14", release2)); code != 200 {
		t.Fatalf("second release import: %d %s", code, b)
	}
	code, ix, b := index(f.h, f.alice)
	if code != 200 || ix.Sources["release"].Digest != d2 || len(ix.Kept["release"]) != 2 || ix.Kept["release"][1].Digest != d1 || ix.Sources["instance"].Digest != di {
		t.Fatalf("index after three imports: %s", b)
	}
	want := []string{"house-style site-1 instance", "oh-code-review site-1 instance", "verify-delivery 1.33.14 release"}
	var got []string
	for _, s := range ix.Effective {
		got = append(got, s.Name+" "+s.Version+" "+s.Source)
	}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("effective: %v", got)
	}
	if code, b, _ := getSet(f.h, f.alice, "?digest="+d1); code != 200 || !strings.Contains(b, `"version":"1.33.13"`) {
		t.Fatalf("kept release by digest: %d %s", code, b)
	}
	// Re-importing the kept one makes it current without writing files.
	if code, b := put(f.admin, "release", skillsBody("1.33.13", release)); code != 200 || !strings.Contains(b, `"imported":false`) {
		t.Fatalf("re-import kept: %d %s", code, b)
	}
	// The admin's own routes on the hub host answer the same.
	if w := taskReq(t, f.s.Handler(), "GET", "/v1/skills", "", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"effective"`) {
		t.Fatalf("admin index: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.s.Handler(), "GET", "/v1/skills/instance", "", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"digest":"`+di+`"`) {
		t.Fatalf("admin set: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", "/v1/skills", f.alice, "", ""); w.Code != 403 {
		t.Fatalf("admin index for an ordinary token: %d", w.Code)
	}

	// A revoked grant ends the reads.
	db, _ := f.s.openAccess(true)
	if err := db.SetGrant("admin", f.alphaInstance, "user", f.aliceUser, false); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := index(f.h, f.alice); code != 403 {
		t.Fatalf("revoked grant, index: %d", code)
	}
	if code, _, _ := getSet(f.h, f.alice, "?source=release"); code != 403 {
		t.Fatalf("revoked grant, set: %d", code)
	}

	// Health names the imported sets, and an unreadable index as a fault.
	health := func() map[string]any {
		w := taskReq(t, f.s.Handler(), "GET", "/v1/health", "", "", "")
		var out map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("health: %d %s", w.Code, w.Body)
		}
		sk, _ := out["skills"].(map[string]any)
		return sk
	}
	if sk := health(); sk["state"] != "ok" || sk["sources"].(map[string]any)["release"] != "1.33.13" || sk["sources"].(map[string]any)["instance"] != "site-1" {
		t.Fatalf("health skills: %v", sk)
	}
	indexPath := filepath.Join(f.reg.Root(), "skills", "index.json")
	if err := os.WriteFile(indexPath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sk := health(); sk["state"] != "fault" || !strings.Contains(sk["error"].(string), "not readable") {
		t.Fatalf("health with a broken index: %v", sk)
	}
	// A member's read says the store is in a fault and names the health
	// check, not the hub's path; the admin's route carries the cause.
	if code, _, b := index(f.h, f.admin); code != 500 || !strings.Contains(b, "aimem health") || strings.Contains(b, indexPath) || strings.Contains(b, "index.json") {
		t.Fatalf("index read with a broken index: %d %s", code, b)
	}
	if w := taskReq(t, f.s.Handler(), "GET", "/v1/skills", "", "", ""); w.Code != 500 || !strings.Contains(w.Body.String(), "not readable") {
		t.Fatalf("admin index with a broken index: %d %s", w.Code, w.Body)
	}
}

// A team session reads the skills of a project its profile is granted,
// and nothing else.
func TestSkillsForATeamSession(t *testing.T) {
	g := newTeamRig(t)
	body := skillsBody("1.33.13", map[string]string{"oh-code-review/SKILL.md": "review"})
	if r := g.call(t, g.tls, "PUT", "/v1/skills/release", g.env, nil, body, true); r.status != 200 {
		t.Fatalf("import: %d %s", r.status, r.body)
	}
	r := g.team(t, "GET", "/v1/projects/alpha/skills")
	if r.status != 200 || !strings.Contains(string(r.body), `"effective"`) || !strings.Contains(string(r.body), `"oh-code-review"`) {
		t.Fatalf("team mode index, granted: %d %s", r.status, r.body)
	}
	if r := g.team(t, "GET", "/v1/projects/alpha/skills/set?source=release"); r.status != 200 || !strings.Contains(string(r.body), `"version":"1.33.13"`) {
		t.Fatalf("team mode set, granted: %d %s", r.status, r.body)
	}
	for _, path := range []string{"/v1/projects/beta/skills", "/v1/projects/user/skills/set?source=release", "/v1/projects/nope/skills"} {
		if r := g.team(t, "GET", path); r.status != 403 || r.code() != "grant_denied" {
			t.Fatalf("team mode %s: %d %s", path, r.status, r.body)
		}
	}
}
