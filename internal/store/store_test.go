package store

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"aimem/internal/schema"
)

func testEvent(key, turn string) *schema.Event {
	return &schema.Event{
		SchemaVersion:  schema.Version,
		IdempotencyKey: key,
		Client:         "claude-code",
		SessionID:      "sess-1",
		TurnID:         turn,
		Kind:           schema.KindTurn,
		Outcome:        schema.OutcomeOK,
		TS:             time.Now().UTC().Format(time.RFC3339),
		UserRequest:    "fix the login bug in auth.go",
		AssistantReply: "Fixed by checking token expiry; tests pass.",
		ToolSummary:    []string{"Edit auth.go", "Bash go test ./..."},
		TouchedPaths:   []string{"auth.go"},
		GitBranch:      "master",
	}
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func TestAppendIdempotent(t *testing.T) {
	r := newTestRegistry(t)
	db, err := r.Open("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	id1, ins1, err := db.Append(testEvent("k1", "t1"))
	if err != nil || !ins1 {
		t.Fatalf("first insert: id=%s ins=%v err=%v", id1, ins1, err)
	}
	id2, ins2, err := db.Append(testEvent("k1", "t1"))
	if err != nil {
		t.Fatal(err)
	}
	if ins2 || id2 != id1 {
		t.Errorf("duplicate not idempotent: id1=%s id2=%s ins2=%v", id1, id2, ins2)
	}
	tl, err := db.Timeline("sess-1", 0)
	if err != nil || len(tl) != 1 {
		t.Fatalf("timeline: %v len=%d", err, len(tl))
	}
}

func TestOrderingAndLatest(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	for i, k := range []string{"a", "b", "c"} {
		ev := testEvent("key-"+k, "turn-"+k)
		ev.UserRequest = "request " + k
		if _, _, err := db.Append(ev); err != nil {
			t.Fatal(i, err)
		}
	}
	tl, _ := db.Timeline("sess-1", 0)
	if len(tl) != 3 || !strings.HasSuffix(tl[0].UserRequest, "a") || !strings.HasSuffix(tl[2].UserRequest, "c") {
		t.Fatalf("wrong order: %+v", tl)
	}
	latest, err := db.Latest("sess-1")
	if err != nil || latest == nil || latest.TurnID != "turn-c" {
		t.Fatalf("latest wrong: %+v err=%v", latest, err)
	}
}

func TestSearchFTS(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	ev := testEvent("k1", "t1")
	ev.AssistantReply = "implemented the retention policy with vacuum"
	db.Append(ev)
	db.Append(testEvent("k2", "t2"))
	hits, err := db.Search("retention", 0)
	if err != nil || len(hits) != 1 || hits[0].TurnID != "t1" {
		t.Fatalf("search: err=%v hits=%+v", err, hits)
	}
	if hits2, _ := db.Search("nonexistentword", 0); len(hits2) != 0 {
		t.Errorf("unexpected hits: %+v", hits2)
	}
}

func TestSanitizationOnIngest(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	ev := testEvent("k1", "t1")
	ev.AssistantReply = "set Authorization: Bearer verysecrettoken12345 in the header"
	db.Append(ev)
	latest, _ := db.Latest("sess-1")
	if strings.Contains(latest.AssistantReply, "verysecrettoken12345") {
		t.Errorf("secret persisted: %q", latest.AssistantReply)
	}
	// The secret must not be findable via FTS either.
	if hits, _ := db.Search("verysecrettoken12345", 0); len(hits) != 0 {
		t.Error("secret indexed in FTS")
	}
}

func TestProjectIsolation(t *testing.T) {
	r := newTestRegistry(t)
	dbA, _ := r.Open("proj-a")
	dbB, _ := r.Open("proj-b")
	dbA.Append(testEvent("k1", "t1"))
	if hits, _ := dbB.Search("login", 0); len(hits) != 0 {
		t.Error("cross-project leakage via search")
	}
	if sess, _ := dbB.Sessions(); len(sess) != 0 {
		t.Error("cross-project session listing")
	}
	ids, _ := r.Projects()
	if len(ids) != 2 {
		t.Errorf("projects: %v", ids)
	}
}

func TestInvalidProjectID(t *testing.T) {
	r := newTestRegistry(t)
	for _, bad := range []string{"", "..", "a/b", "a\\b", "x y", strings.Repeat("a", 200)} {
		if _, err := r.Open(bad); err == nil {
			t.Errorf("accepted invalid project id %q", bad)
		}
	}
}

func TestRetentionByAge(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	old := testEvent("k-old", "t-old")
	old.TS = time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	db.Append(old)
	db.Append(testEvent("k-new", "t-new"))
	n, err := db.Retention(24*time.Hour, 0)
	if err != nil || n != 1 {
		t.Fatalf("retention: n=%d err=%v", n, err)
	}
	tl, _ := db.Timeline("sess-1", 0)
	if len(tl) != 1 || tl[0].TurnID != "t-new" {
		t.Errorf("wrong survivor: %+v", tl)
	}
}

func TestSchemaValidation(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	bad := testEvent("k1", "t1")
	bad.Kind = "invented-kind"
	if _, _, err := db.Append(bad); err == nil {
		t.Error("accepted unknown kind")
	}
	bad2 := testEvent("k2", "t2")
	bad2.SchemaVersion = 99
	if _, _, err := db.Append(bad2); err == nil {
		t.Error("accepted future schema version")
	}
}

func TestDBFilePermissions(t *testing.T) {
	// Windows has no POSIX mode bits; Go synthesizes 0666 (or 0444 for a
	// read-only file) from the file attributes, so os.Chmod cannot express
	// what this asserts. Access control there comes from the ACL on the
	// per-user state directory instead.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	r := newTestRegistry(t)
	db, err := r.Open("proj-perm")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("journal.db mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestRenameProject(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	src, err := r.Open("0f05aa6db5cf-4b0b914c3ba2")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.Remember("a fact worth keeping", "curator", RememberOpts{Kind: "fact"}); err != nil {
		t.Fatal(err)
	}

	// A group DB cites the contributor by id; the rename must follow.
	grp, err := r.Open("group-ai-infra")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := grp.Remember("shared fact", "curator", RememberOpts{
		Kind:    "fact",
		Sources: []string{"project:0f05aa6db5cf-4b0b914c3ba2"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.Rename("0f05aa6db5cf-4b0b914c3ba2", "aimem-pre-remote"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if _, err := r.OpenExisting("0f05aa6db5cf-4b0b914c3ba2"); err == nil {
		t.Fatal("old project id still resolves after rename")
	}
	moved, err := r.OpenExisting("aimem-pre-remote")
	if err != nil {
		t.Fatalf("new project id missing: %v", err)
	}
	mems, err := moved.Memories(false)
	if err != nil || len(mems) != 1 || mems[0].Text != "a fact worth keeping" {
		t.Fatalf("memories = %v, %v; want the one fact", mems, err)
	}

	grp2, err := r.OpenExisting("group-ai-infra")
	if err != nil {
		t.Fatal(err)
	}
	gm, err := grp2.Memories(false)
	if err != nil || len(gm) != 1 {
		t.Fatalf("group memories = %v, %v", gm, err)
	}
	var cited string
	for _, s := range gm[0].Sources {
		if strings.HasPrefix(s, "project:") {
			cited = s
		}
	}
	if cited != "project:aimem-pre-remote" {
		t.Fatalf("group fact cites %q; want project:aimem-pre-remote", cited)
	}
}

func TestRenameProjectRefusals(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, id := range []string{"alpha", "beta", UserScopeProject, "group-x"} {
		if _, err := r.Open(id); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct{ old, new, why string }{
		{"alpha", "beta", "target exists"},
		{UserScopeProject, "whatever", "user DB is reserved"},
		{"group-x", "whatever", "group DB is reserved"},
		{"alpha", "group-y", "cannot become a group DB"},
		{"missing", "whatever", "no such project"},
		{"alpha", "../escape", "invalid id"},
	}
	for _, c := range cases {
		if err := r.Rename(c.old, c.new); err == nil {
			t.Errorf("Rename(%q, %q) succeeded; want refusal (%s)", c.old, c.new, c.why)
		}
	}
	// Every refusal must leave the projects untouched.
	for _, id := range []string{"alpha", "beta", UserScopeProject, "group-x"} {
		if _, err := r.OpenExisting(id); err != nil {
			t.Errorf("project %q disappeared after a refused rename: %v", id, err)
		}
	}
}
