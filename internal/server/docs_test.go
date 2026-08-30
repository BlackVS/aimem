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
