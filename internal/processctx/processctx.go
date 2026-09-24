// Package processctx builds the session-start process context: the
// enablement signal and the selected reference from the hub, the set from
// the cache or Git, rendered as the bootstrap the session-start hook
// injects. Every step is bounded and fails to a notice, never to a blocked
// session start. The CLI (`aimem session-start`, `aimem process show`,
// `aimem teams setup`) and the checkout-bound local MCP facade share it.
//
// Load is the typed form: it says which state the process context is in
// (docs/DESIGN-portable-team-context.md, decision 3) and carries the set and
// the complete unit. Bootstrap renders the same result as the notice text
// the hook and the CLI have always printed.
package processctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/ident"
	"aimem/internal/process"
	"aimem/internal/taskcred"
)

// hubLookupTimeout bounds each hub contact at session start; the Git
// fetch has its own bound in the process package and is paid only on a
// cache miss.
const hubLookupTimeout = 1500 * time.Millisecond

// State is what Load could establish about the project's process context.
type State string

const (
	// Ready: the hub named the selection just now and the complete set is
	// here, fetched or from this machine's exact-commit cache.
	Ready State = "ready"
	// LastObserved: the hub could not be reached; the set is the exact
	// cached commit of the selection this machine last observed. Complete,
	// possibly stale, and never authorization for a task write.
	LastObserved State = "last_observed"
	// Disabled: tasks are off for the project (or the hub predates the
	// signal), so there is no process context by design.
	Disabled State = "disabled"
	// NotSelected: tasks are on but no process reference is selected.
	NotSelected State = "not_selected"
	// Denied: the hub refused this checkout's credential, or Git refused
	// this machine's access to the process repository. Never masked by a
	// cached selection.
	Denied State = "denied"
	// Unavailable: anything else that prevents a complete set; Detail says
	// what.
	Unavailable State = "unavailable"
	// TooLarge: the complete unit exists but is over MaxDeliveryBytes, so
	// it is not delivered in part.
	TooLarge State = "too_large"
)

// MaxDeliveryBytes bounds the complete unit a tool delivers in one result
// (decision 2): over it, nothing is delivered and the state is TooLarge.
const MaxDeliveryBytes = 32 << 10

// Source says where a complete set came from.
const (
	SourceFetched = "fetched"
	SourceCache   = "cache"
)

// Result is Load's outcome. Set and Unit are present for Ready and
// LastObserved; Set alone for TooLarge (the session-start budget still
// applies to it); neither otherwise.
type Result struct {
	State      State
	Project    string
	Ref        *process.Ref // the selection the set belongs to, when one is known
	Source     string       // SourceFetched or SourceCache, with a set
	ObservedAt string       // when this machine last read the selection from the hub, for a last-observed one
	// FromLastObserved: the hub was not asked successfully and the selection
	// is this machine's last observed one (or the pinned one, unconfirmed),
	// so the result is never Ready.
	FromLastObserved bool
	// Pinned: Ref is a version the caller supplied from trusted local state
	// (LoadRef), not the hub's current selection; Current is that current
	// selection when the hub answered it.
	Pinned    bool
	Current   *process.Ref
	PinnedFor string // whose version a pinned result is, for its notice
	Detail    string // what happened, for every state but Ready
	Fix       string // who does what about it, when someone can
	Set       *process.Set
	Unit      string // the complete unit, as `aimem process show --full` prints it
	// notice is the text Bootstrap has always returned for this outcome;
	// "" where it said nothing.
	notice string
}

// Complete reports whether the result carries the whole unit.
func (r *Result) Complete() bool { return r.State == Ready || r.State == LastObserved }

func (r *Result) set(state State, detail, fix, notice string) *Result {
	r.State, r.Detail, r.Fix, r.notice = state, detail, fix, notice
	return r
}

// Bootstrap builds the session-start process context for the project in
// dir (or projectID when given), with root as the state root: the enablement signal and
// the handbook when the hub says tasks are on and a process reference
// is selected, or an explicit availability notice otherwise. Every step
// is bounded and fails to a notice, never to a blocked session start.
// The returned Set is non-nil only when a process set was obtained. full
// skips the injection budget: the read path the over-budget notice names.
func Bootstrap(dir, root, projectID string, full bool) (string, *process.Set) {
	r := Load(dir, root, projectID)
	if r.Set == nil {
		return r.notice, nil
	}
	home, _ := os.UserHomeDir()
	var text string
	if full {
		text = process.BootstrapFull(r.Set, r.Project, process.SkillInstalled(dir, home))
	} else {
		var err error
		if text, err = process.Bootstrap(r.Set, r.Project, process.SkillInstalled(dir, home)); err != nil {
			return "process context unavailable: " + err.Error(), r.Set
		}
	}
	if r.FromLastObserved { // whether or not it fits a delivery
		text = lastObservedNote(r.ObservedAt) + text
	}
	return text, r.Set
}

func lastObservedNote(at string) string {
	return fmt.Sprintf("NOTE: the hub is unreachable; the process selection below is as last observed at %s and tasks availability is unknown. Cached context never authorizes a task write.\n", at)
}

// Load resolves the process context for the project in dir (or projectID
// when given) with root as the state root, and says which state it is in.
// The credential is the checkout's, selected strictly: a required local
// credential that is missing or refused ends the lookup, never falls back.
func Load(dir, root, projectID string) *Result {
	r := &Result{Project: projectID}
	hub, hubClient, ok := r.connect(dir, root)
	if !ok {
		return r
	}
	id := r.Project
	lastPath := filepath.Join(root, "process", "last-"+id+".json")
	// 1. Enablement and the selection, from the hub, bounded.
	var identity struct {
		TasksEnabled *bool `json:"tasks_enabled"`
	}
	ierr := hubGetJSONClient(hub, hubClient, "/v1/access/identity?project="+url.QueryEscape(id), &identity)
	var sel struct {
		Current *process.Ref `json:"current"`
	}
	var serr error
	if ierr == nil {
		serr = hubGetJSONClient(hub, hubClient, "/v1/projects/"+url.PathEscape(id)+"/process", &sel)
	}
	var refused *hubRefusal
	switch {
	case errors.As(ierr, &refused) && refused.denied():
		// The hub answered and refused this credential: a denial, never
		// disguised as an outage by a cached selection.
		return r.set(Denied, "the hub refused this checkout's credential: "+ierr.Error(), credentialFix,
			fmt.Sprintf("process context unavailable: the hub refused this checkout's credential (%v); no cached selection is used", ierr))
	case ierr != nil && !unreachable(ierr):
		return r.set(Unavailable, "the hub answered with an error: "+ierr.Error(), "retry; if it persists, check the hub's log", "process context unavailable: "+ierr.Error())
	case ierr != nil:
		// Hub offline or unreachable: availability unknown. Use the last
		// observed selection, saying so, or report unavailable.
		b, rerr := os.ReadFile(lastPath)
		var last struct {
			Ref        process.Ref `json:"ref"`
			ObservedAt string      `json:"observed_at"`
		}
		if rerr != nil || json.Unmarshal(b, &last) != nil || last.Ref.Validate() != nil || !validTime(last.ObservedAt) {
			return r.set(Unavailable, fmt.Sprintf("hub unreachable (%v) and no last-observed selection on this machine", ierr), "retry when the hub is reachable",
				fmt.Sprintf("process context unavailable: hub unreachable (%v) and no last-observed selection on this machine; tasks availability unknown", ierr))
		}
		r.Ref, r.ObservedAt, r.FromLastObserved = &last.Ref, last.ObservedAt, true
	case identity.TasksEnabled == nil || !*identity.TasksEnabled:
		return r.set(Disabled, "tasks are not enabled for project "+id+" (or the hub predates the signal)", "an admin enables tasks for the project on the hub", "") // by design, nothing to say
	case errors.As(serr, &refused) && refused.denied():
		return r.set(Denied, "the hub refused to read the process selection: "+serr.Error(), credentialFix, "process context unavailable: "+serr.Error())
	case serr != nil && strings.Contains(serr.Error(), "no process reference"):
		return r.set(NotSelected, "tasks are on for project "+id+" but no process reference is selected", "an admin selects one on the hub host: aimem process select <repo> <commit> <manifest>",
			"Tasks (Kanban) are ON for project "+id+", but no process reference is selected: process context unavailable until an admin selects one (`aimem process select <repo> <commit> <manifest>` on the hub host).")
	case serr != nil:
		return r.set(Unavailable, serr.Error(), "retry; if it persists, check the hub's log", "process context unavailable: "+serr.Error())
	case sel.Current == nil:
		return r.set(NotSelected, "tasks are on for project "+id+" but no process reference is selected", "an admin selects one on the hub host: aimem process select <repo> <commit> <manifest>",
			"Tasks (Kanban) are ON for project "+id+", but no process reference is selected: process context unavailable until an admin selects one.")
	default:
		r.Ref = sel.Current
		if b, err := json.Marshal(map[string]any{"ref": r.Ref, "observed_at": time.Now().UTC().Format(time.RFC3339)}); err == nil {
			os.MkdirAll(filepath.Dir(lastPath), 0o700)
			os.WriteFile(lastPath, b, 0o600)
		}
	}
	return r.finish(dir, root)
}

// LoadRef resolves one exact process version the caller holds as trusted
// local state (the version an accepted attempt was taken under) instead of
// the hub's current selection. The live checks are Load's: the checkout's
// credential, strictly selected and validated; a refusal is a denial; tasks
// must be on. The hub's current selection is read only to report it
// (Current), never to replace the pinned one. The content is the pinned
// commit exactly, from the cache or a bounded fetch of that commit; when the
// hub cannot be asked, from the cache only, and never ready.
func LoadRef(dir, root, projectID string, pinned process.Ref) *Result {
	r := &Result{Project: projectID, Pinned: true}
	hub, hubClient, ok := r.connect(dir, root)
	if !ok {
		return r
	}
	id := r.Project
	if err := pinned.Validate(); err != nil {
		return r.set(Unavailable, "the recorded process version is malformed: "+err.Error(), "", "")
	}
	r.Ref = &pinned
	var identity struct {
		TasksEnabled *bool `json:"tasks_enabled"`
	}
	ierr := hubGetJSONClient(hub, hubClient, "/v1/access/identity?project="+url.QueryEscape(id), &identity)
	var refused *hubRefusal
	switch {
	case errors.As(ierr, &refused) && refused.denied():
		return r.set(Denied, "the hub refused this checkout's credential: "+ierr.Error(), credentialFix, "")
	case ierr != nil && !unreachable(ierr):
		return r.set(Unavailable, "the hub answered with an error: "+ierr.Error(), "retry; if it persists, check the hub's log", "")
	case ierr != nil:
		// The hub cannot be asked: no live check ran, so the content comes
		// only from the exact cache and is never ready.
		r.FromLastObserved = true
	case identity.TasksEnabled == nil || !*identity.TasksEnabled:
		return r.set(Disabled, "tasks are not enabled for project "+id+" (or the hub predates the signal)", "an admin enables tasks for the project on the hub", "")
	default:
		var sel struct {
			Current *process.Ref `json:"current"`
		}
		if err := hubGetJSONClient(hub, hubClient, "/v1/projects/"+url.PathEscape(id)+"/process", &sel); err == nil && sel.Current != nil {
			r.Current = sel.Current
		}
	}
	return r.finish(dir, root)
}

// connect resolves the project, its hub binding and the checkout's
// credential, strictly: a required local credential that is missing or
// refused ends the lookup, never falls back. ok false means r is set.
func (r *Result) connect(dir, root string) (*adapter.HubConfig, *http.Client, bool) {
	if r.Project == "" {
		id, err := ident.ProjectID(dir)
		if err != nil {
			r.set(Unavailable, "not a project checkout: "+err.Error(), "", "")
			return nil, nil, false
		}
		r.Project = id
	}
	fail := func(state State, detail, fix, notice string) (*adapter.HubConfig, *http.Client, bool) {
		r.set(state, detail, fix, notice)
		return nil, nil, false
	}
	hubName, err := ident.ProjectHubNameStrict(dir)
	if err != nil {
		return fail(Unavailable, err.Error(), "fix the checkout's .aimem.json hub binding", "process context unavailable: "+err.Error())
	}
	_, hub := adapter.ResolveHub(root, hubName)
	if hub == nil {
		// No hub: no tasks anywhere; the hook says nothing.
		return fail(Unavailable, "no hub is configured for this checkout", "configure the hub this project syncs with (aimem hub add)", "")
	}
	hubClient := hub.HTTPClient()
	local, err := taskcred.LocalRequired(dir)
	if err != nil {
		return fail(Unavailable, err.Error(), "fix the checkout's .aimem.json", "process context unavailable: "+err.Error())
	}
	if local {
		selected, err := taskcred.Resolve(dir, root)
		if err != nil {
			return fail(Unavailable, err.Error(), "install this checkout's project credential with `aimem task-token set`, as the account that runs aimem; no other credential is used", "process context unavailable: "+err.Error())
		}
		ctx, cancel := context.WithTimeout(context.Background(), hubLookupTimeout)
		err = selected.Validate(ctx)
		cancel()
		if err != nil {
			var rejected *taskcred.Rejected
			if errors.As(err, &rejected) && (rejected.Status == http.StatusUnauthorized || rejected.Status == http.StatusForbidden) {
				return fail(Denied, err.Error(), credentialFix, "process context unavailable: "+err.Error())
			}
			// A local credential that cannot be validated is not used, and
			// nothing cached stands in for it.
			return fail(Unavailable, err.Error(), "retry when the hub is reachable", "process context unavailable: "+err.Error())
		}
		copyHub := *hub
		copyHub.Token = selected.Token
		hub = &copyHub
		hubClient = selected.Client()
	}
	return hub, hubClient, true
}

// finish reads the files of r.Ref and sets the final state. A selection
// the hub just confirmed comes from the cache or Git, bounded; one the hub
// did not confirm now (last observed, or pinned while the hub cannot be
// asked) only from this machine's exact cache (docs/DESIGN-kanban-docs.md:
// the cache must match that selection), never from Git. Neither
// substitutes another commit.
func (r *Result) finish(dir, root string) *Result {
	id := r.Project
	var res process.Result
	if r.FromLastObserved {
		res = process.Cached(root, *r.Ref)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), process.FetchTimeout+time.Second)
		defer cancel()
		res = process.Fetch(ctx, root, *r.Ref)
	}
	switch res.Status {
	case process.StatusDenied:
		return r.set(Denied, fmt.Sprintf("Git refused this machine's access to %s: %v", r.Ref.Repo, res.Err), "give the account running aimem on this machine read access to "+r.Ref.Repo,
			fmt.Sprintf("process context unavailable: access to the process repository was DENIED on this machine (%v); this machine needs read access to %s", res.Err, r.Ref.Repo))
	case process.StatusUnavailable:
		return r.set(Unavailable, fmt.Sprintf("%v (selection %s @ %s)", res.Err, r.Ref.Repo, short(r.Ref.Commit)), "retry when Git is reachable; only the exact selected commit is used",
			fmt.Sprintf("process context unavailable: %v (selection %s @ %s)", res.Err, r.Ref.Repo, short(r.Ref.Commit)))
	}
	r.Set = res.Set
	r.Source = SourceFetched
	if res.Status == process.StatusCached {
		r.Source = SourceCache
	}
	home, _ := os.UserHomeDir()
	unit := process.BootstrapFull(res.Set, id, process.SkillInstalled(dir, home))
	if len(unit) > MaxDeliveryBytes {
		r.State, r.Detail, r.Fix = TooLarge, fmt.Sprintf("the complete process unit is %d bytes, over the %d-byte delivery limit; it is not delivered in part", len(unit), MaxDeliveryBytes),
			"the process owner splits the handbook so the complete unit fits; `aimem process show --full` prints it on this machine"
		return r
	}
	r.Unit = unit
	r.State = Ready
	switch {
	case r.FromLastObserved && r.Pinned:
		r.State = LastObserved
		r.Detail = "the hub could not be asked, so no live check ran; this is the exact cached commit of the pinned version and never authorizes a task write"
	case r.FromLastObserved:
		r.State = LastObserved
		r.Detail = "the hub is unreachable; this is the exact cached commit of the selection last observed at " + r.ObservedAt + "; it may be stale and never authorizes a task write"
	}
	return r
}

// validTime reports an RFC 3339 observation time: a last-observed record
// without one cannot say how old it is and is not used.
func validTime(s string) bool {
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// short is a commit's display prefix; a malformed commit from a damaged
// record or a misbehaving hub is shown as it is, never sliced past its end.
func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

const credentialFix = "check the checkout's credential (aimem task-token show-source); an operator reissues or re-grants it; no other credential is substituted"

// hubRefusal is a hub response other than 200: the hub answered.
type hubRefusal struct {
	status int
	msg    string
}

func (e *hubRefusal) Error() string { return e.msg }

// denied reports a refusal of the credential itself.
func (e *hubRefusal) denied() bool {
	return e.status == http.StatusUnauthorized || e.status == http.StatusForbidden
}

// unreachable reports whether err means the hub could not be asked: a
// transport failure, or a gateway in front of it saying so (502, 503, 504).
// Only then may a cached selection stand in for the hub's answer.
func unreachable(err error) bool {
	var refused *hubRefusal
	if !errors.As(err, &refused) {
		return true
	}
	switch refused.status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// hubGetJSONClient uses the selected credential and redirect policy for a
// bounded identity/process read; both routes admit ordinary tokens. A
// non-200 answer is a *hubRefusal carrying the status.
func hubGetJSONClient(hub *adapter.HubConfig, client *http.Client, path string, into any) error {
	ctx, cancel := context.WithTimeout(context.Background(), hubLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hub.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+hub.Token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error != "" {
			return &hubRefusal{status: resp.StatusCode, msg: e.Error}
		}
		return &hubRefusal{status: resp.StatusCode, msg: fmt.Sprintf("hub returned HTTP %d", resp.StatusCode)}
	}
	return json.Unmarshal(raw, into)
}
