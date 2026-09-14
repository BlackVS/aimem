// Package process fetches a project's selected process documents — the
// handbook, the checklists that gate READY and DONE, the task template —
// from Git at a pinned commit, keeps an exact-version cache per commit,
// and composes the session-start bootstrap an agent receives when a
// project's tasks are on. The hub stores only the reference
// (docs/DESIGN-kanban-docs.md, "Session bootstrap"); every machine fetches
// with its own Git access, and a commit is immutable, so a complete cache
// entry for a commit is the commit and needs no network to be trusted.
package process

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Ref is the hub-stored selection: where the process set lives and which
// exact version of it this project follows.
type Ref struct {
	Repo       string `json:"repo"`
	Ref        string `json:"ref,omitempty"` // branch or tag to fetch when the server refuses fetch-by-hash
	Commit     string `json:"commit"`        // full 40-hex commit id
	Manifest   string `json:"manifest"`      // manifest path in the repository at that commit
	SelectedAt string `json:"selected_at,omitempty"`
	SelectedBy string `json:"selected_by,omitempty"`
}

var (
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	refRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
)

// Validate checks the shape of a selection: a Git URL a client can hand
// to `git fetch` (https, ssh, or the scp-like form), a full lowercase
// commit id, a relative manifest path that stays inside the repository,
// and an optional plain ref name.
func (r Ref) Validate() error {
	if len(r.Repo) == 0 || len(r.Repo) > 512 || strings.ContainsAny(r.Repo, " \t\r\n\"'") || strings.HasPrefix(r.Repo, "-") {
		return errors.New("repo must be a Git URL (at most 512 bytes, no whitespace)")
	}
	switch {
	case strings.HasPrefix(r.Repo, "https://"), strings.HasPrefix(r.Repo, "ssh://"), strings.HasPrefix(r.Repo, "git@"):
	default:
		return errors.New("repo must start with https://, ssh:// or git@")
	}
	if !commitRE.MatchString(r.Commit) {
		return errors.New("commit must be the full 40-character lowercase hex id")
	}
	if err := checkRepoPath(r.Manifest); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if r.Ref != "" && !refRE.MatchString(r.Ref) {
		return errors.New("ref must be a plain branch or tag name")
	}
	return nil
}

// checkRepoPath accepts a relative slash path inside the repository.
func checkRepoPath(p string) error {
	if p == "" || len(p) > 256 || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsAny(p, " \t\r\n") {
		return errors.New("must be a relative slash path (at most 256 bytes)")
	}
	if path.Clean(p) != p || p == "." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") {
		return errors.New("must be a clean relative path inside the repository")
	}
	return nil
}

// Manifest names the files of one process set at one commit. Version 1.
type Manifest struct {
	Version     int               `json:"version"`
	Handbook    string            `json:"handbook"`
	Checklists  map[string]string `json:"checklists,omitempty"` // task state -> checklist file
	Templates   map[string]string `json:"templates,omitempty"`  // kind -> template file
	Skills      []string          `json:"skills,omitempty"`     // installed skill names the process requires
	BudgetBytes int               `json:"budget_bytes,omitempty"`
}

// DefaultBudgetBytes bounds the bootstrap injected into a session when the
// manifest does not say: the handbook plus the checklists.
const DefaultBudgetBytes = 24 * 1024

// MaxFileBytes bounds any single file of a process set.
const MaxFileBytes = 1 << 20

var stateRE = regexp.MustCompile(`^[A-Z][A-Z_]{1,31}$`)
var skillRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ParseManifest decodes and validates a manifest.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	if m.Version != 1 {
		return m, fmt.Errorf("manifest: version %d is not supported (want 1)", m.Version)
	}
	if err := checkRepoPath(m.Handbook); err != nil {
		return m, fmt.Errorf("manifest: handbook: %w", err)
	}
	for st, p := range m.Checklists {
		if !stateRE.MatchString(st) {
			return m, fmt.Errorf("manifest: checklist state %q is not a task state name", st)
		}
		if err := checkRepoPath(p); err != nil {
			return m, fmt.Errorf("manifest: checklist %s: %w", st, err)
		}
	}
	for kind, p := range m.Templates {
		if !skillRE.MatchString(kind) {
			return m, fmt.Errorf("manifest: template kind %q is not a plain name", kind)
		}
		if err := checkRepoPath(p); err != nil {
			return m, fmt.Errorf("manifest: template %s: %w", kind, err)
		}
	}
	for _, s := range m.Skills {
		if !skillRE.MatchString(s) {
			return m, fmt.Errorf("manifest: skill %q is not a plain name", s)
		}
	}
	if m.BudgetBytes < 0 {
		return m, errors.New("manifest: budget_bytes must not be negative")
	}
	if m.BudgetBytes == 0 {
		m.BudgetBytes = DefaultBudgetBytes
	}
	return m, nil
}

// Checklist is a Definition of Ready or Done: items with stable ids, so
// evidence can name the item it assessed and the commit it came from.
type Checklist struct {
	State string          `json:"state"`
	Items []ChecklistItem `json:"items"`
}

// ChecklistItem is one line of a checklist.
type ChecklistItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ParseChecklist decodes and validates a checklist for the given state.
func ParseChecklist(state string, b []byte) (Checklist, error) {
	var c Checklist
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("checklist %s: %w", state, err)
	}
	if c.State != state {
		return c, fmt.Errorf("checklist %s: file says state %q", state, c.State)
	}
	if len(c.Items) == 0 {
		return c, fmt.Errorf("checklist %s: no items", state)
	}
	seen := map[string]bool{}
	for _, it := range c.Items {
		if it.ID == "" || len(it.ID) > 64 || strings.ContainsAny(it.ID, " \t\r\n") || seen[it.ID] {
			return c, fmt.Errorf("checklist %s: item id %q is empty, too long or repeated", state, it.ID)
		}
		if strings.TrimSpace(it.Text) == "" || len(it.Text) > 1024 {
			return c, fmt.Errorf("checklist %s: item %s has no text (or more than 1 KiB)", state, it.ID)
		}
		seen[it.ID] = true
	}
	return c, nil
}

// Set is one fetched process set: the manifest and every file it names,
// read from the cache directory for the commit.
type Set struct {
	Ref        Ref
	Manifest   Manifest
	Dir        string
	Handbook   string
	Checklists map[string]Checklist
	Templates  map[string]json.RawMessage
	FromCache  bool // served from a complete cache entry without contacting Git
}

// Status classifies a fetch outcome. The distinctions matter to the
// notice an agent sees: denied is not offline, offline with an exact cache
// is usable, and unavailable names its reason.
type Status string

const (
	StatusFetched     Status = "fetched"
	StatusCached      Status = "cached"
	StatusDenied      Status = "denied"
	StatusUnavailable Status = "unavailable"
)

// Result of Fetch: a Set when Status is fetched or cached; otherwise Err
// says why and Status says what kind of why.
type Result struct {
	Set    *Set
	Status Status
	Err    error
}

// completeMarker names the file whose presence makes a cache entry
// complete: written last, after every file, so an interrupted fetch never
// looks like a finished one.
const completeMarker = ".complete"

func repoKey(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:8])
}

// CacheDir is where the files of a commit live once fetched.
func CacheDir(root string, ref Ref) string {
	return filepath.Join(root, "process", repoKey(ref.Repo), ref.Commit)
}

// FetchTimeout bounds one Git contact. Only a cache miss pays it: the
// first session on a machine after a selection, then never for that
// commit again.
const FetchTimeout = 10 * time.Second

// Fetch returns the process set for ref, from the cache when the commit
// is already there complete, otherwise from Git. It never returns a
// partial set: files are written to a temporary directory and promoted
// atomically once every one is present.
func Fetch(ctx context.Context, root string, ref Ref) Result {
	if err := ref.Validate(); err != nil {
		return Result{Status: StatusUnavailable, Err: err}
	}
	return fetchNoValidate(ctx, root, ref)
}

// fetchNoValidate is Fetch after validation; tests drive it against a
// local repository, which Validate refuses for a hub selection.
func fetchNoValidate(ctx context.Context, root string, ref Ref) Result {
	dir := CacheDir(root, ref)
	if set, err := loadSet(dir, ref); err == nil {
		set.FromCache = true
		return Result{Set: set, Status: StatusCached}
	}
	repoDir := filepath.Join(filepath.Dir(dir), "repo.git")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return Result{Status: StatusUnavailable, Err: err}
	}
	if _, err := os.Stat(filepath.Join(repoDir, "HEAD")); err != nil {
		if out, err := git(ctx, "", "init", "--bare", "-q", repoDir); err != nil {
			return Result{Status: StatusUnavailable, Err: fmt.Errorf("git init: %s", firstLine(out, err))}
		}
	}
	// By hash first; a server that refuses unadvertised objects gets the
	// ref instead, and the commit must then be present in what arrived.
	if out, err := git(ctx, repoDir, "fetch", "-q", "--depth", "1", ref.Repo, ref.Commit); err != nil {
		if st, cerr := classify(out, err); st == StatusDenied || ref.Ref == "" {
			return Result{Status: st, Err: cerr}
		}
		if out2, err2 := git(ctx, repoDir, "fetch", "-q", "--depth", "1", ref.Repo, ref.Ref); err2 != nil {
			st, cerr := classify(out2, err2)
			return Result{Status: st, Err: cerr}
		}
		if _, err := git(ctx, repoDir, "cat-file", "-e", ref.Commit+"^{commit}"); err != nil {
			return Result{Status: StatusUnavailable, Err: fmt.Errorf("commit %s is not what %s points at (fetched at depth 1); select the commit the ref holds, or a ref that holds the commit", ref.Commit[:12], ref.Ref)}
		}
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-"+ref.Commit[:12]+"-")
	if err != nil {
		return Result{Status: StatusUnavailable, Err: err}
	}
	defer os.RemoveAll(tmp) // a no-op after a successful rename
	manifestRaw, err := show(ctx, repoDir, ref.Commit, ref.Manifest)
	if err != nil {
		return Result{Status: StatusUnavailable, Err: err}
	}
	m, err := ParseManifest(manifestRaw)
	if err != nil {
		return Result{Status: StatusUnavailable, Err: err}
	}
	files := map[string][]byte{ref.Manifest: manifestRaw}
	for _, p := range m.files() {
		if _, ok := files[p]; ok {
			continue
		}
		b, err := show(ctx, repoDir, ref.Commit, p)
		if err != nil {
			return Result{Status: StatusUnavailable, Err: err}
		}
		files[p] = b
	}
	for p, b := range files {
		fp := filepath.Join(tmp, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
			return Result{Status: StatusUnavailable, Err: err}
		}
		if err := os.WriteFile(fp, b, 0o600); err != nil {
			return Result{Status: StatusUnavailable, Err: err}
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, completeMarker), []byte(ref.Manifest+"\n"), 0o600); err != nil {
		return Result{Status: StatusUnavailable, Err: err}
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another session promoted the same commit meanwhile: theirs is
		// as good as ours.
		if _, err2 := os.Stat(filepath.Join(dir, completeMarker)); err2 != nil {
			return Result{Status: StatusUnavailable, Err: err}
		}
	}
	set, err := loadSet(dir, ref)
	if err != nil {
		return Result{Status: StatusUnavailable, Err: err}
	}
	return Result{Set: set, Status: StatusFetched}
}

func (m Manifest) files() []string {
	out := []string{m.Handbook}
	for _, p := range m.Checklists {
		out = append(out, p)
	}
	for _, p := range m.Templates {
		out = append(out, p)
	}
	return out
}

// loadSet reads a complete cache entry; anything missing or unparseable
// is an error, never a partial set.
func loadSet(dir string, ref Ref) (*Set, error) {
	marker, err := os.ReadFile(filepath.Join(dir, completeMarker))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(marker)) != ref.Manifest {
		return nil, fmt.Errorf("cache entry was fetched for manifest %q, not %q", strings.TrimSpace(string(marker)), ref.Manifest)
	}
	read := func(p string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return nil, fmt.Errorf("cache entry incomplete: %w", err)
		}
		return b, nil
	}
	raw, err := read(ref.Manifest)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	set := &Set{Ref: ref, Manifest: m, Dir: dir, Checklists: map[string]Checklist{}, Templates: map[string]json.RawMessage{}}
	hb, err := read(m.Handbook)
	if err != nil {
		return nil, err
	}
	set.Handbook = string(hb)
	for st, p := range m.Checklists {
		b, err := read(p)
		if err != nil {
			return nil, err
		}
		c, err := ParseChecklist(st, b)
		if err != nil {
			return nil, err
		}
		set.Checklists[st] = c
	}
	for kind, p := range m.Templates {
		b, err := read(p)
		if err != nil {
			return nil, err
		}
		if !json.Valid(b) {
			return nil, fmt.Errorf("template %s is not valid JSON", kind)
		}
		set.Templates[kind] = json.RawMessage(b)
	}
	return set, nil
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Never a prompt: a private source without credentials is a denial,
	// reported as one, not a hung session start.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	out, err := cmd.CombinedOutput()
	return out, err
}

func show(ctx context.Context, repoDir, commit, p string) ([]byte, error) {
	out, err := git(ctx, repoDir, "show", commit+":"+p)
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %s", p, commit[:12], firstLine(out, err))
	}
	if len(out) > MaxFileBytes {
		return nil, fmt.Errorf("%s at %s: larger than %d bytes", p, commit[:12], MaxFileBytes)
	}
	return out, nil
}

func firstLine(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return err.Error()
	}
	return s
}

// classify turns git's stderr into the status the design distinguishes:
// a confirmed denial is reported as one, never disguised as offline.
func classify(out []byte, err error) (Status, error) {
	line := firstLine(out, err)
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "authentication failed"), strings.Contains(l, "permission denied"),
		strings.Contains(l, "could not read username"), strings.Contains(l, "403"),
		strings.Contains(l, "repository not found"), strings.Contains(l, "terminal prompts disabled"):
		return StatusDenied, fmt.Errorf("git access denied: %s", line)
	case strings.Contains(l, "couldn't find remote ref"), strings.Contains(l, "not our ref"),
		strings.Contains(l, "not a valid object"), strings.Contains(l, "remote error: upload-pack: not our ref"):
		return StatusUnavailable, fmt.Errorf("commit or ref not found in the repository: %s", line)
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(l, "could not resolve host"),
		strings.Contains(l, "unable to access"), strings.Contains(l, "connection"),
		strings.Contains(l, "timed out"), strings.Contains(l, "network"):
		return StatusUnavailable, fmt.Errorf("git unreachable: %s", line)
	}
	return StatusUnavailable, fmt.Errorf("git: %s", line)
}

// TaskTools is the list an agent is told about in the bootstrap.
var TaskTools = []string{"list_tasks", "get_task", "create_task", "update_task", "get_task_history", "list_task_comments", "get_task_comment", "add_task_comment"}

// Bootstrap composes the session-start context for a project whose tasks
// are on: the signal line, the process set's identity, the required
// skills with what this machine knows about them, the handbook and the
// checklists — as one unit. If the unit exceeds the manifest's budget it
// is not injected at all; the error names the size and the retrieval
// path, because a policy with a gate cut off is worse than none.
// installed reports whether a named skill is present on this machine.
func Bootstrap(set *Set, projectID string, installed func(name string) bool) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Tasks (Kanban) are ON for project %s. Task tools: %s.\n", projectID, strings.Join(TaskTools, ", "))
	fmt.Fprintf(&b, "Process set: %s @ %s (manifest %s)%s.\n", set.Ref.Repo, set.Ref.Commit[:12], set.Ref.Manifest, map[bool]string{true: ", from this machine's cache", false: ""}[set.FromCache])
	if len(set.Manifest.Skills) > 0 {
		b.WriteString("Required skills: ")
		parts := make([]string, 0, len(set.Manifest.Skills))
		for _, s := range set.Manifest.Skills {
			if installed(s) {
				parts = append(parts, s+" (installed)")
			} else {
				parts = append(parts, s+" (NOT FOUND on this machine — check before the step that needs it)")
			}
		}
		b.WriteString(strings.Join(parts, "; "))
		b.WriteString(".\n")
	}
	if len(set.Templates) > 0 {
		kinds := make([]string, 0, len(set.Templates))
		for k := range set.Templates {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		fmt.Fprintf(&b, "Templates in the process set (read with `aimem process show --template <kind>`): %s.\n", strings.Join(kinds, ", "))
	}
	b.WriteString("\n--- Process handbook ---\n")
	b.WriteString(strings.TrimRight(set.Handbook, "\n"))
	b.WriteString("\n")
	states := make([]string, 0, len(set.Checklists))
	for st := range set.Checklists {
		states = append(states, st)
	}
	sort.Strings(states)
	for _, st := range states {
		fmt.Fprintf(&b, "\n--- Checklist gating %s ---\n", st)
		for _, it := range set.Checklists[st].Items {
			fmt.Fprintf(&b, "- [%s] %s\n", it.ID, it.Text)
		}
	}
	unit := b.String()
	if len(unit) > set.Manifest.BudgetBytes {
		return "", fmt.Errorf("process bootstrap is %d bytes, over the manifest's budget of %d: not injected — read it with `aimem process show`", len(unit), set.Manifest.BudgetBytes)
	}
	return unit, nil
}

// SkillInstalled reports whether a skill directory of that name exists in
// the places the installers put them, for this project or this machine.
func SkillInstalled(projectDir, homeDir string) func(name string) bool {
	return func(name string) bool {
		if !skillRE.MatchString(name) {
			return false
		}
		for _, d := range []string{
			filepath.Join(projectDir, ".claude", "skills", name),
			filepath.Join(projectDir, ".agents", "skills", name),
			filepath.Join(homeDir, ".claude", "skills", name),
			filepath.Join(homeDir, ".codex", "skills", name),
		} {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				return true
			}
		}
		return false
	}
}
