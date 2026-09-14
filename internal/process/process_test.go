package process

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefValidate(t *testing.T) {
	good := Ref{Repo: "https://example.com/org/process.git", Commit: strings.Repeat("a", 40), Manifest: "process/manifest.json", Ref: "main"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Ref{
		{Repo: "ftp://x", Commit: good.Commit, Manifest: good.Manifest},
		{Repo: "-https://x", Commit: good.Commit, Manifest: good.Manifest},
		{Repo: good.Repo, Commit: "abc", Manifest: good.Manifest},
		{Repo: good.Repo, Commit: strings.ToUpper(good.Commit), Manifest: good.Manifest},
		{Repo: good.Repo, Commit: good.Commit, Manifest: "../x.json"},
		{Repo: good.Repo, Commit: good.Commit, Manifest: "/abs.json"},
		{Repo: good.Repo, Commit: good.Commit, Manifest: "a/../b.json"},
		{Repo: good.Repo, Commit: good.Commit, Manifest: good.Manifest, Ref: "-bad"},
		{Repo: good.Repo, Commit: good.Commit, Manifest: good.Manifest, Ref: "has space"},
	}
	for i, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("bad ref %d accepted: %+v", i, r)
		}
	}
}

func TestManifestAndChecklist(t *testing.T) {
	m, err := ParseManifest([]byte(`{"version":1,"handbook":"h.md","checklists":{"READY":"dor.json","DONE":"dod.json"},"templates":{"task":"t.json"},"skills":["oh-code-review"]}`))
	if err != nil || m.BudgetBytes != DefaultBudgetBytes {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	for _, raw := range []string{
		`{"version":2,"handbook":"h.md"}`,
		`{"version":1}`,
		`{"version":1,"handbook":"../h.md"}`,
		`{"version":1,"handbook":"h.md","checklists":{"ready":"x"}}`,
		`{"version":1,"handbook":"h.md","skills":["Bad Name"]}`,
		`{"version":1,"handbook":"h.md","extra":1}`,
		`{"version":1,"handbook":"h.md","budget_bytes":-1}`,
	} {
		if _, err := ParseManifest([]byte(raw)); err == nil {
			t.Errorf("manifest accepted: %s", raw)
		}
	}
	if _, err := ParseChecklist("READY", []byte(`{"state":"READY","items":[{"id":"r1","text":"scoped"},{"id":"r2","text":"criteria written"}]}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"state":"DONE","items":[{"id":"r1","text":"x"}]}`,
		`{"state":"READY","items":[]}`,
		`{"state":"READY","items":[{"id":"r1","text":"x"},{"id":"r1","text":"y"}]}`,
		`{"state":"READY","items":[{"id":"","text":"x"}]}`,
		`{"state":"READY","items":[{"id":"r1","text":"  "}]}`,
	} {
		if _, err := ParseChecklist("READY", []byte(raw)); err == nil {
			t.Errorf("checklist accepted: %s", raw)
		}
	}
}

// makeRepo builds a local process repository with one commit and returns
// its path and commit id.
func makeRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	for p, body := range files {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(fp), 0o700)
		os.WriteFile(fp, []byte(body), 0o600)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "process set")
	return dir, run("rev-parse", "HEAD")
}

const manifestJSON = `{"version":1,"handbook":"process/HANDBOOK.md","checklists":{"READY":"process/dor.json","DONE":"process/dod.json"},"templates":{"task":"process/task.json"},"skills":["oh-code-review","never-installed-skill"],"budget_bytes":4096}`

func fixtureFiles() map[string]string {
	return map[string]string{
		"process/manifest.json": manifestJSON,
		"process/HANDBOOK.md":   "# Handbook\n\nStates and who moves them.\n",
		"process/dor.json":      `{"state":"READY","items":[{"id":"dor-1","text":"objective written"},{"id":"dor-2","text":"acceptance criteria written"}]}`,
		"process/dod.json":      `{"state":"DONE","items":[{"id":"dod-1","text":"merged with evidence"}]}`,
		"process/task.json":     `{"fields":["title","objective","acceptance_criteria"]}`,
	}
}

func TestFetchCachesAndFallsBackToRef(t *testing.T) {
	repo, sha := makeRepo(t, fixtureFiles())
	root := t.TempDir()
	// A hub selection must be https, ssh or git@; the test drives the same
	// fetch path against a local repository through file://, which
	// Validate refuses and fetchNoValidate skips.
	ref := Ref{Repo: "file://" + filepath.ToSlash(repo), Commit: sha, Manifest: "process/manifest.json", Ref: "main"}
	if err := ref.Validate(); err == nil {
		t.Fatal("file:// must not validate for a hub selection")
	}
	res := fetchNoValidate(context.Background(), root, ref)
	if res.Status != StatusFetched || res.Set == nil {
		t.Fatalf("first fetch: %s %v", res.Status, res.Err)
	}
	if res.Set.Handbook == "" || len(res.Set.Checklists) != 2 || len(res.Set.Templates) != 1 {
		t.Fatalf("set incomplete: %+v", res.Set)
	}
	if _, err := os.Stat(filepath.Join(CacheDir(root, ref), completeMarker)); err != nil {
		t.Fatal("complete marker missing")
	}
	// The repository can vanish: the cache entry is the commit.
	os.RemoveAll(repo)
	res2 := fetchNoValidate(context.Background(), root, ref)
	if res2.Status != StatusCached || !res2.Set.FromCache {
		t.Fatalf("second fetch must be served from cache: %s %v", res2.Status, res2.Err)
	}
	// A commit the ref does not hold is unavailable, never a wrong version.
	other := ref
	other.Commit = strings.Repeat("b", 40)
	if res3 := fetchNoValidate(context.Background(), root, other); res3.Status != StatusUnavailable || res3.Set != nil {
		t.Fatalf("missing commit: %s %v", res3.Status, res3.Err)
	}
	// A leftover temporary directory is never a cache entry.
	tmp := filepath.Join(filepath.Dir(CacheDir(root, other)), ".tmp-"+other.Commit[:12]+"-x")
	os.MkdirAll(tmp, 0o700)
	os.WriteFile(filepath.Join(tmp, "process", "manifest.json"), []byte(manifestJSON), 0o600)
	if _, err := loadSet(CacheDir(root, other), other); err == nil {
		t.Fatal("an unpromoted directory must not load as a set")
	}
	// An entry fetched for another manifest path is not this selection's.
	wrongManifest := ref
	wrongManifest.Manifest = "process/other.json"
	if _, err := loadSet(CacheDir(root, ref), wrongManifest); err == nil {
		t.Fatal("cache entry for a different manifest must not load")
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Status{
		"fatal: Authentication failed for 'https://x/'":                             StatusDenied,
		"remote: Repository not found.":                                             StatusDenied,
		"fatal: could not read Username for 'https://x': terminal prompts disabled": StatusDenied,
		"fatal: couldn't find remote ref deadbeef":                                  StatusUnavailable,
		"fatal: unable to access 'https://x/': Could not resolve host: x":           StatusUnavailable,
		"something else entirely":                                                   StatusUnavailable,
	}
	for line, want := range cases {
		if st, err := classify([]byte(line), os.ErrNotExist); st != want || err == nil {
			t.Errorf("%q: %s %v (want %s)", line, st, err, want)
		}
	}
	if st, err := classify([]byte("fatal: Authentication failed"), nil); st != StatusDenied || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denial wording: %s %v", st, err)
	}
	if st, err := classify([]byte("fatal: unable to access 'https://x/': Could not resolve host"), nil); st != StatusUnavailable || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("offline wording: %s %v", st, err)
	}
}

func TestBootstrapBudgetAndSkills(t *testing.T) {
	set := &Set{
		Ref:      Ref{Repo: "https://example.com/p.git", Commit: strings.Repeat("c", 40), Manifest: "m.json"},
		Manifest: Manifest{Version: 1, Handbook: "h.md", Skills: []string{"oh-code-review", "missing-one"}, BudgetBytes: 4096},
		Handbook: "# Handbook\nRules.\n",
		Checklists: map[string]Checklist{
			"READY": {State: "READY", Items: []ChecklistItem{{ID: "dor-1", Text: "scoped"}}},
			"DONE":  {State: "DONE", Items: []ChecklistItem{{ID: "dod-1", Text: "merged"}}},
		},
		Templates: map[string]json.RawMessage{"task": json.RawMessage(`{}`)},
	}
	installed := func(n string) bool { return n == "oh-code-review" }
	text, err := Bootstrap(set, "alpha", installed)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Tasks (Kanban) are ON for project alpha", "list_tasks", "cccccccccccc", "oh-code-review (installed)", "missing-one (NOT FOUND", "Templates in the process set", "task", "--- Process handbook ---", "Rules.", "Checklist gating DONE", "[dor-1] scoped", "[dod-1] merged"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bootstrap lacks %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "Checklist gating DONE") > strings.Index(text, "Checklist gating READY") {
		t.Fatal("checklists must be in a stable order")
	}
	set.Manifest.BudgetBytes = 64
	if _, err := Bootstrap(set, "alpha", installed); err == nil || !strings.Contains(err.Error(), "over the manifest's budget") || !strings.Contains(err.Error(), "aimem process show") {
		t.Fatalf("over budget must refuse with the retrieval path: %v", err)
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude", "skills", "oh-code-review"), 0o700)
	is := SkillInstalled(t.TempDir(), home)
	if !is("oh-code-review") || is("nope") || is("Bad Name") {
		t.Fatal("skill detection")
	}
}
