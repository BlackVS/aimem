// Package process handles a project's selected process documents — the
// handbook, the checklists that gate READY and DONE, the task template —
// and composes the session-start bootstrap an agent receives when a
// project's tasks are on. The hub stores the selection and, since the
// hub-served set became the default path (docs/DESIGN-portable-team-context.md,
// increment 1d), the files of the selected commit with their digest
// (bundle.go); a member reads them from the hub and caches them by digest.
// The Git path below is the admin's import on the hub host and a member's
// fallback against an older hub: a commit is immutable, so a complete
// cache entry for a commit is the commit and needs no network to be
// trusted.
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
	Ref        string `json:"ref,omitempty"`    // branch or tag to fetch when the server refuses fetch-by-hash
	Commit     string `json:"commit"`           // full 40-hex commit id
	Manifest   string `json:"manifest"`         // manifest path in the repository at that commit
	Digest     string `json:"digest,omitempty"` // SHA-256 of the stored set (Digest); empty for a selection made before the hub stored sets
	Name       string `json:"name,omitempty"`   // the set's display name, derived at selection (SetName)
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
	if r.Digest != "" && !digestRE.MatchString(r.Digest) {
		return errors.New("digest must be 64 lowercase hex characters")
	}
	if len(r.Name) > 128 || strings.ContainsAny(r.Name, " \t\r\n") {
		return errors.New("name must be one word of at most 128 bytes")
	}
	return nil
}

// Key is the selection without its stamps and derived fields: what makes
// two selections the same process version.
func (r Ref) Key() Ref { return Ref{Repo: r.Repo, Ref: r.Ref, Commit: r.Commit, Manifest: r.Manifest} }

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

// Manifest names the files of one process set at one commit. Version 1
// names the required skills by name; version 2 pins each one as name,
// version and source (docs/DESIGN-portable-team-context.md, increment 1e),
// and an aimem from before version 2 refuses it with a message naming the
// version, never misreading it.
type Manifest struct {
	Version     int               `json:"version"`
	Handbook    string            `json:"handbook"`
	Checklists  map[string]string `json:"checklists,omitempty"` // task state -> checklist file
	Templates   map[string]string `json:"templates,omitempty"`  // kind -> template file
	Skills      []SkillPin        `json:"skills,omitempty"`     // the skills the process requires
	BudgetBytes int               `json:"budget_bytes,omitempty"`
}

// SkillPin is one required skill: its name alone (version 1), or its name
// with the version of the skills set it comes from and that set's source,
// release or instance (version 2). An instance skill of a name the release
// also has is the one meant, by the sources' precedence.
type SkillPin struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source,omitempty"`
}

// UnmarshalJSON accepts the version 1 form (a name) and the version 2 form
// (an object); ParseManifest decides which the manifest's version allows.
func (p *SkillPin) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		p.Version, p.Source = "", ""
		return json.Unmarshal(b, &p.Name)
	}
	type plain SkillPin
	var v plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	*p = SkillPin(v)
	return nil
}

// MarshalJSON writes the shortest form that holds the pin.
func (p SkillPin) MarshalJSON() ([]byte, error) {
	if p.Version == "" && p.Source == "" {
		return json.Marshal(p.Name)
	}
	type plain SkillPin
	return json.Marshal(plain(p))
}

// Pinned reports whether the pin names a version and a source.
func (p SkillPin) Pinned() bool { return p.Version != "" && p.Source != "" }

// SkillNames lists the required skills' names, in manifest order.
func (m Manifest) SkillNames() []string {
	out := make([]string, 0, len(m.Skills))
	for _, p := range m.Skills {
		out = append(out, p.Name)
	}
	return out
}

// Pinned reports whether the manifest pins its skills (version 2).
func (m Manifest) Pinned() bool { return m.Version >= 2 }

// The sources a version 2 manifest may pin a skill to.
var skillSources = map[string]bool{"release": true, "instance": true}

var skillVersionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

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
	if m.Version != 1 && m.Version != 2 {
		return m, fmt.Errorf("manifest: version %d is not supported (want 1 or 2)", m.Version)
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
	seenSkill := map[string]bool{}
	for _, p := range m.Skills {
		if !skillRE.MatchString(p.Name) {
			return m, fmt.Errorf("manifest: skill %q is not a plain name", p.Name)
		}
		if seenSkill[p.Name] {
			return m, fmt.Errorf("manifest: skill %s is named twice", p.Name)
		}
		seenSkill[p.Name] = true
		switch {
		case m.Version == 1 && (p.Version != "" || p.Source != ""):
			return m, fmt.Errorf("manifest: skill %s carries a version or source, which needs manifest version 2", p.Name)
		case m.Version >= 2 && !p.Pinned():
			return m, fmt.Errorf("manifest: skill %s must name its version and source (release or instance) in a version 2 manifest", p.Name)
		case m.Version >= 2 && !skillSources[p.Source]:
			return m, fmt.Errorf("manifest: skill %s: source %q is not release or instance", p.Name, p.Source)
		case m.Version >= 2 && !skillVersionRE.MatchString(p.Version):
			return m, fmt.Errorf("manifest: skill %s: version %q is not one plain word", p.Name, p.Version)
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

// CacheDir is where the files of one selection live once fetched: keyed
// by repository, commit and manifest, because two manifests at one commit
// are two different sets and each must serve from its own complete entry.
func CacheDir(root string, ref Ref) string {
	sum := sha256.Sum256([]byte(ref.Manifest))
	return filepath.Join(root, "process", repoKey(ref.Repo), ref.Commit+"-"+hex.EncodeToString(sum[:4]))
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

// Cached returns the set for ref from this machine's complete cache entry
// (the selection's digest, or the exact commit fetched from Git) and never
// contacts anything: the only source a selection the hub has not just
// confirmed (a last-observed one) may use.
func Cached(root string, ref Ref) Result {
	if err := ref.Validate(); err != nil {
		return Result{Status: StatusUnavailable, Err: err}
	}
	if ref.Digest != "" {
		if set, err := CachedSet(root, ref); err == nil {
			return Result{Set: set, Status: StatusCached}
		}
	}
	set, err := loadSet(CacheDir(root, ref), ref)
	if err != nil {
		return Result{Status: StatusUnavailable, Err: fmt.Errorf("commit %s is not complete in this machine's cache: %w", ref.Commit[:12], err)}
	}
	set.FromCache = true
	return Result{Set: set, Status: StatusCached}
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
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-"+filepath.Base(dir)+"-")
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
	set, err := loadSetFrom(func(p string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return nil, fmt.Errorf("cache entry incomplete: %w", err)
		}
		return b, nil
	}, ref)
	if err != nil {
		return nil, err
	}
	set.Dir = dir
	return set, nil
}

// loadSetFrom parses the set for ref from read, which answers each
// repository path with its bytes.
func loadSetFrom(read func(p string) ([]byte, error), ref Ref) (*Set, error) {
	raw, err := read(ref.Manifest)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	set := &Set{Ref: ref, Manifest: m, Checklists: map[string]Checklist{}, Templates: map[string]json.RawMessage{}}
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
	// reported as one, not a hung session start. ssh in batch mode for
	// the same reason — but only on a machine that configured no ssh
	// command of its own: GIT_SSH_COMMAND overrides core.sshCommand and
	// GIT_SSH, and those are exactly the access this design reuses.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	if dir != "" && !sshCommandConfigured(ctx, dir) {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	return runBounded(cmd)
}

// sshCommandConfigured reports whether this machine (or the cache
// repository) already names the ssh command git should use — through the
// environment or git configuration — in which case it is left alone.
func sshCommandConfigured(ctx context.Context, dir string) bool {
	if os.Getenv("GIT_SSH_COMMAND") != "" || os.Getenv("GIT_SSH") != "" {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "core.sshCommand")
	cmd.Dir = dir
	out, err := runBounded(cmd)
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// PipeDrainDelay bounds how long a finished (or killed) git may keep us
// waiting for its output pipes: a transport or credential helper that
// inherited them and stalls would otherwise hold the session start past
// the context deadline (os/exec: Wait waits for the pipes, not the
// process, and WaitDelay is what closes them).
const PipeDrainDelay = 2 * time.Second

// runBounded runs a context-bound command and returns its combined
// output, guaranteed to return within the context deadline plus
// PipeDrainDelay even if a descendant keeps the output pipes open.
func runBounded(cmd *exec.Cmd) ([]byte, error) {
	cmd.WaitDelay = PipeDrainDelay
	return cmd.CombinedOutput()
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
	// 403 counts only as git's HTTP status: the line also names the
	// repository path or URL, whose digits must never read as a denial.
	case strings.Contains(l, "authentication failed"), strings.Contains(l, "permission denied"),
		strings.Contains(l, "could not read username"), strings.Contains(l, "returned error: 403"),
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
// are on: the signal line, the process set's identity (its name, commit
// and manifest; never the repository URL, which is the admin's and is
// shown by `aimem process show --full` outside this unit), the required
// skills with what this machine knows about them, the handbook and the
// checklists — as one unit. If the unit exceeds the manifest's budget it
// is not injected at all; the error names the size and the retrieval
// path, because a policy with a gate cut off is worse than none.
// installed reports what this machine has of a named skill.
func Bootstrap(set *Set, projectID string, installed func(name string) SkillState) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Tasks (Kanban) are ON for project %s. Task tools: %s.\n", projectID, strings.Join(TaskTools, ", "))
	fmt.Fprintf(&b, "Process set: %s @ %s (manifest %s)%s.\n", set.Ref.DisplayName(), set.Ref.Commit[:12], set.Ref.Manifest, map[bool]string{true: ", from this machine's cache", false: ""}[set.FromCache])
	if len(set.Manifest.Skills) > 0 {
		b.WriteString("Required skills: ")
		parts := make([]string, 0, len(set.Manifest.Skills))
		for _, p := range set.Manifest.Skills {
			parts = append(parts, SkillLine(p, installed(p.Name)))
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
		return "", fmt.Errorf("process bootstrap is %d bytes, over the manifest's budget of %d: not injected — read it in full with `aimem process show --full`", len(unit), set.Manifest.BudgetBytes)
	}
	return unit, nil
}

// BootstrapFull is Bootstrap without the budget: the retrieval path the
// over-budget notice names, for a person or an agent reading the whole
// unit on demand rather than receiving it at session start.
func BootstrapFull(set *Set, projectID string, installed func(name string) SkillState) string {
	saved := set.Manifest.BudgetBytes
	set.Manifest.BudgetBytes = int(^uint(0) >> 1)
	unit, _ := Bootstrap(set, projectID, installed)
	set.Manifest.BudgetBytes = saved
	return unit
}

// SkillState is what this machine has of one required skill: whether a
// directory of its name exists where the installers put skills, and, when
// the installer's record beside it says, which set version and source it
// came from ("" when unknown: a copy made by hand).
type SkillState struct {
	Installed bool
	Version   string
	Source    string
	Dir       string // the skill's directory, when installed
}

// SkillDirs lists where the installers put skills, for this project and
// this user: the project locations first.
func SkillDirs(projectDir, homeDir string) []string {
	var dirs []string
	if projectDir != "" {
		for _, rel := range [][]string{{".claude", "skills"}, {".agents", "skills"}, {".opencode", "skills"}, {".openhands", "skills"}} {
			dirs = append(dirs, filepath.Join(append([]string{projectDir}, rel...)...))
		}
	}
	if homeDir != "" {
		for _, rel := range [][]string{{".claude", "skills"}, {".agents", "skills"}, {".codex", "skills"}, {".config", "opencode", "skills"}, {".openhands", "skills"}} {
			dirs = append(dirs, filepath.Join(append([]string{homeDir}, rel...)...))
		}
	}
	return dirs
}

// InstalledRecordName is the installers' record in a skills directory:
// which set version its skills came from, and per skill where a hub
// install mixes sources.
const InstalledRecordName = ".ai-skills.json"

// InstalledRecord is that record's content, as the aiskills installers
// write it (version, commit, skills, archive_sha256, installed_at) and as
// `aimem skills install` extends it (versions and sources per skill).
type InstalledRecord struct {
	Version       string            `json:"version"`
	Commit        string            `json:"commit,omitempty"`
	Skills        []string          `json:"skills"`
	ArchiveSHA256 string            `json:"archive_sha256,omitempty"`
	InstalledAt   string            `json:"installed_at"`
	Versions      map[string]string `json:"versions,omitempty"`
	Sources       map[string]string `json:"sources,omitempty"`
}

// ReadInstalledRecord reads the record of a skills directory; none, or an
// unreadable one, is nil.
func ReadInstalledRecord(dir string) *InstalledRecord {
	raw, err := os.ReadFile(filepath.Join(dir, InstalledRecordName))
	if err != nil {
		return nil
	}
	var rec InstalledRecord
	if json.Unmarshal(raw, &rec) != nil {
		return nil
	}
	return &rec
}

// Of returns the version and source the record gives a skill: its own
// entries, else the record's version for a skill it lists.
func (rec *InstalledRecord) Of(name string) (version, source string) {
	if rec == nil {
		return "", ""
	}
	if v, ok := rec.Versions[name]; ok {
		return v, rec.Sources[name]
	}
	for _, s := range rec.Skills {
		if s == name {
			return rec.Version, rec.Sources[name]
		}
	}
	return "", ""
}

// SkillStateOn reports, per name, what this project or this machine has
// of a skill: the first directory of that name holding SKILL.md in the
// installers' locations, with the record beside it.
func SkillStateOn(projectDir, homeDir string) func(name string) SkillState {
	dirs := SkillDirs(projectDir, homeDir)
	return func(name string) SkillState {
		if !skillRE.MatchString(name) {
			return SkillState{}
		}
		for _, d := range dirs {
			if fi, err := os.Stat(filepath.Join(d, name, "SKILL.md")); err == nil && fi.Mode().IsRegular() {
				st := SkillState{Installed: true, Dir: filepath.Join(d, name)}
				st.Version, st.Source = ReadInstalledRecord(d).Of(name)
				return st
			}
		}
		return SkillState{}
	}
}

// SkillInstallCommand is what a member runs to install or update the
// required skills from the hub; no text names where the hub got them.
const SkillInstallCommand = "aimem skills install"

// SkillLine is one required skill as the bootstrap and the readiness
// checks print it: the pin, then what this machine has. A missing skill or
// one at another version names the command, never a URL.
func SkillLine(p SkillPin, st SkillState) string {
	if !p.Pinned() {
		if st.Installed {
			return p.Name + " (installed)"
		}
		return p.Name + " (NOT FOUND on this machine; run `" + SkillInstallCommand + "`)"
	}
	head := p.Name + " " + p.Version + " (" + p.Source + "; "
	switch {
	case !st.Installed:
		return head + "NOT FOUND, run `" + SkillInstallCommand + "`)"
	case st.Version == "":
		return head + "installed, version unknown)"
	case st.Version != p.Version:
		return head + "installed " + st.Version + ", run `" + SkillInstallCommand + "`)"
	}
	return head + "installed)"
}
