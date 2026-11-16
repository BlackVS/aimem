package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMergeDocEndpoint: the merge route is a CALCULATOR — it returns
// the three-way result and never writes (DESIGN-doc-collab).
func TestMergeDocEndpoint(t *testing.T) {
	s, reg := testServer(t)
	db, _ := reg.Open("proj-mrg")
	if _, err := db.PutDoc("PLAN", "a\nb\nc\n", "m1", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutDoc("PLAN", "a\nb\nC2\n", "console", 1, false); err != nil {
		t.Fatal(err)
	}
	// A draft based on rev 1 that changed the head merges cleanly with
	// rev 2's tail change.
	w := req(t, s.Handler(), "POST", "/v1/projects/proj-mrg/docs/PLAN/merge",
		`{"body":"A1\nb\nc\n","base_rev":1}`)
	if w.Code != 200 {
		t.Fatalf("merge: %d %s", w.Code, w.Body)
	}
	var res struct {
		Merged    string `json:"merged"`
		Conflicts int    `json:"conflicts"`
		Against   int64  `json:"against_rev"`
		BaseFound bool   `json:"base_found"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Conflicts != 0 || res.Against != 2 || !res.BaseFound || res.Merged != "A1\nb\nC2\n" {
		t.Fatalf("clean merge: %+v", res)
	}
	if cur, _ := db.GetDoc("PLAN", 0); cur.Rev != 2 || cur.Body != "a\nb\nC2\n" {
		t.Fatalf("merge endpoint must not write: %+v", cur)
	}
	// Overlapping draft: markers and a count, still no write.
	w = req(t, s.Handler(), "POST", "/v1/projects/proj-mrg/docs/PLAN/merge",
		`{"body":"a\nb\nC-mine\n","base_rev":1}`)
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Conflicts != 1 || !strings.Contains(res.Merged, "<<<<<<<") {
		t.Fatalf("conflict merge: %+v", res)
	}
}

// TestPutDocRefusesALikelySecretAndNamesTheProject: the hub applies the
// document policy itself, so a client that skips or predates the scan
// cannot publish a secret to every writer; nothing is stored, and the
// answer names the project and the rule, never the text.
func TestPutDocRefusesALikelySecretAndNamesTheProject(t *testing.T) {
	s, reg := testServer(t)
	value := strings.Join([]string{"abcdefghij", "klmnop123456"}, "")
	body, _ := json.Marshal(map[string]any{"body": "call with Authorization: Bearer " + value, "base_rev": 0})
	w := req(t, s.Handler(), "PUT", "/v1/projects/proj-sec/docs/RUNBOOK", string(body))
	if w.Code != 400 {
		t.Fatalf("PUT: %d %s, want 400", w.Code, w.Body)
	}
	got := w.Body.String()
	for _, want := range []string{"proj-sec", "RUNBOOK", "bearer token", "placeholder"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not name %q: %s", want, got)
		}
	}
	if strings.Contains(got, value) {
		t.Fatalf("the refusal carries the matched text: %s", got)
	}
	db, _ := reg.Open("proj-sec")
	if _, err := db.GetDoc("RUNBOOK", 0); err == nil {
		t.Fatal("a refused document was stored")
	}
	// A placeholder showing the header's form publishes.
	body, _ = json.Marshal(map[string]any{"body": "call with Authorization: Bearer <token>", "base_rev": 0})
	if w := req(t, s.Handler(), "PUT", "/v1/projects/proj-sec/docs/RUNBOOK", string(body)); w.Code != 200 {
		t.Fatalf("placeholder PUT: %d %s", w.Code, w.Body)
	}
}
