package tui

import (
	"strings"
	"testing"
	"time"

	"aimem/internal/schema"
	"aimem/internal/store"
)

func TestCollectAndRender(t *testing.T) {
	// Hermetic: modelConfig() reads ~/.config/aimem/env, so a dev
	// machine's real config must not leak into the test (CI has none —
	// this exact divergence kept CI red while local runs were green).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIMEM_CURATE_MODEL", "")
	t.Setenv("AIMEM_EMBED_MODEL", "")
	root := t.TempDir()
	reg, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := reg.Open("proj-tui")
	_, _, err = db.Append(&schema.Event{
		SchemaVersion: 1, IdempotencyKey: "k1", Client: "claude-code",
		SessionID: "s1", TurnID: "t1", Kind: schema.KindTurn,
		Outcome: schema.OutcomeOK, TS: time.Now().UTC().Format(time.RFC3339),
		UserRequest: "make the dashboard",
	})
	if err != nil {
		t.Fatal(err)
	}
	db.Remember("tui shows live stats", "test", store.RememberOpts{})
	db.SetMeta("groups", `["group-ops"]`)
	db.AddCurateRun(&store.CurateRun{TS: time.Now().UTC().Format(time.RFC3339),
		Model:      "test-curate-model",
		EventsRead: 5, Written: 2, InputTokens: 1000, OutputTokens: 200})
	reg.Close()

	snap := collect(root, 0)
	if snap.Err != nil {
		t.Fatal(snap.Err)
	}
	if len(snap.Projects) != 1 || snap.Projects[0].ID != "proj-tui" {
		t.Fatalf("projects: %+v", snap.Projects)
	}
	st := snap.Projects[0].Stats
	if st.Events != 1 || st.Sessions != 1 || st.Memories != 1 || st.LastClient != "claude-code" {
		t.Errorf("stats: %+v", st)
	}
	if len(snap.Groups["group-ops"]) != 1 {
		t.Errorf("groups: %+v", snap.Groups)
	}
	if gs := snap.GroupSess["group-ops"]; len(gs) != 1 || gs[0].SessionID != "s1" ||
		gs[0].Project != "proj-tui" || gs[0].Events != 1 {
		t.Errorf("group sessions: %+v", gs)
	}
	if len(snap.Tail) != 1 || snap.Tail[0].UserRequest != "make the dashboard" {
		t.Errorf("tail: %+v", snap.Tail)
	}

	out := model{snap: snap}.View()
	for _, want := range []string{"proj-tui", "group-ops", "make the dashboard",
		"1000 in / 200 out", "not configured"} {
		if !strings.Contains(out, want) {
			t.Errorf("view missing %q:\n%s", want, out)
		}
	}

	groupsOut := model{snap: snap, view: viewGroups}.View()
	for _, want := range []string{"claude-code:s1", "1 ev"} {
		if !strings.Contains(groupsOut, want) {
			t.Errorf("groups view missing %q:\n%s", want, groupsOut)
		}
	}
}
