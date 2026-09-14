package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// Selecting a process reference is an admin action with a compare-and-swap
// on the previous commit; reading the selection is on the ordinary
// surface; the history survives a change; every credential is refused a
// malformed selection.
func TestProcessReferenceSelection(t *testing.T) {
	f := newTaskFixture(t)
	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	sel := func(token, body string) (int, string) {
		w := taskReq(t, f.h, "PUT", "/v1/projects/alpha/process", token, "", body)
		return w.Code, w.Body.String()
	}
	// Nothing selected yet: 404 for readers, and the writer token may not select.
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/process", f.alice, "", ""); w.Code != 404 || !strings.Contains(w.Body.String(), "no process reference") {
		t.Fatalf("unselected: %d %s", w.Code, w.Body)
	}
	if code, _ := sel(f.writer, `{"repo":"https://example.com/p.git","commit":"`+c1+`","manifest":"m.json","expected_commit":""}`); code != 403 {
		t.Fatalf("writer token selected: %d", code)
	}
	if code, _ := sel(f.alice, `{"repo":"https://example.com/p.git","commit":"`+c1+`","manifest":"m.json","expected_commit":""}`); code != 403 {
		t.Fatalf("ordinary token selected: %d", code)
	}
	for _, bad := range []string{
		`{"repo":"ftp://x","commit":"` + c1 + `","manifest":"m.json","expected_commit":""}`,
		`{"repo":"https://example.com/p.git","commit":"abc","manifest":"m.json","expected_commit":""}`,
		`{"repo":"https://example.com/p.git","commit":"` + c1 + `","manifest":"../m.json","expected_commit":""}`,
		`{"repo":"https://example.com/p.git","commit":"` + c1 + `","manifest":"m.json","expected_commit":"","surprise":1}`,
	} {
		if code, body := sel(f.admin, bad); code != 400 {
			t.Fatalf("bad selection accepted: %d %s (%s)", code, body, bad)
		}
	}
	// Admin selects; the ordinary token reads it; the socket operator can too.
	code, body := sel(f.admin, `{"repo":"https://example.com/p.git","commit":"`+c1+`","manifest":"m.json","ref":"main","expected_commit":""}`)
	if code != 200 || !strings.Contains(body, c1) || !strings.Contains(body, `"selected_by":"host-admin"`) {
		t.Fatalf("select: %d %s", code, body)
	}
	w := taskReq(t, f.h, "GET", "/v1/projects/alpha/process", f.alice, "", "")
	var got struct {
		Current struct {
			Repo, Commit, Manifest, Ref string
		} `json:"current"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Current.Commit != c1 || got.Current.Ref != "main" {
		t.Fatalf("read by ordinary token: %d %s", w.Code, w.Body)
	}
	// Stale expectation: refused with the current selection.
	code, body = sel(f.admin, `{"repo":"https://example.com/p.git","commit":"`+c2+`","manifest":"m.json","expected_commit":"`+strings.Repeat("9", 40)+`"}`)
	if code != 409 || !strings.Contains(body, c1) {
		t.Fatalf("stale expected_commit: %d %s", code, body)
	}
	// Correct expectation: changed; history keeps both, newest first.
	if code, _ := sel(f.admin, `{"repo":"https://example.com/p.git","commit":"`+c2+`","manifest":"m.json","expected_commit":"`+c1+`"}`); code != 200 {
		t.Fatalf("change: %d", code)
	}
	w = taskReq(t, f.s.Handler(), "GET", "/v1/projects/alpha/process?history=1", "", "", "")
	var hist struct {
		History []struct{ Commit string } `json:"history"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &hist) != nil || len(hist.History) != 2 || hist.History[0].Commit != c2 || hist.History[1].Commit != c1 {
		t.Fatalf("history: %d %s", w.Code, w.Body)
	}
	// Clear needs the current commit too; afterwards readers get 404 but the history stays.
	if code, _ := sel(f.admin, `{"clear":true,"expected_commit":""}`); code != 409 {
		t.Fatalf("clear with a stale expectation: %d", code)
	}
	w = taskReq(t, f.s.Handler(), "PUT", "/v1/projects/alpha/process", "", "", `{"clear":true,"expected_commit":"`+c2+`"}`)
	if w.Code != 200 {
		t.Fatalf("clear by the operator: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/process", f.bob, "", ""); w.Code != 404 {
		t.Fatalf("after clear: %d", w.Code)
	}
	w = taskReq(t, f.h, "GET", "/v1/projects/alpha/process?history=1", f.bob, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), c2) || !strings.Contains(w.Body.String(), `"current":null`) {
		t.Fatalf("history after clear: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/nope/process", f.alice, "", ""); w.Code != 404 {
		t.Fatalf("unknown project: %d", w.Code)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/user/process", f.alice, "", ""); w.Code != 400 {
		t.Fatalf("reserved project: %d", w.Code)
	}
}
