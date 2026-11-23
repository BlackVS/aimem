// Package adapter implements the client-side submission path shared by all
// adapters: redact before the record leaves the adapter, POST to the local
// service, and degrade to a durable local spool when the service is
// unreachable — fail-open, but never a silent drop. Spooled records replay
// opportunistically on the next successful contact.
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aimem/internal/ident"
	"aimem/internal/redact"
	"aimem/internal/schema"
	"aimem/internal/server"
)

// Payload is one submission: a project-scoped event. Adapters that cannot
// cheaply compute the project identity may send project_dir instead;
// ResolveProjectID fills project_id from it.
type Payload struct {
	ProjectID  string `json:"project_id"`
	ProjectDir string `json:"project_dir,omitempty"`
	// Groups mirrors the project's .aimem.json membership (backing ids,
	// "group-<name>"). Riding on every event push it reaches the hub in
	// real time, which is what lets hub-side curation know where group
	// facts may land. nil = unknown (field omitted); empty = none.
	Groups *[]string `json:"groups,omitempty"`
	// Hub mirrors the project's .aimem.json hub binding (a named hub in
	// this machine's hub.json; "" = the default hub). It routes the
	// real-time push and, stored as project meta, lets `aimem sync --hub`
	// keep each hub's data physically separate. nil = unknown.
	Hub   *string      `json:"hub,omitempty"`
	Event schema.Event `json:"event"`
	// Degraded says why the event lacks what the transcript would have
	// given (its time share ran out); never sent, it goes to the run's
	// adapter.log line.
	Degraded string `json:"-"`
}

// ResolveProjectID derives ProjectID from ProjectDir when unset, and
// stamps group membership while the directory is at hand (best-effort: a
// malformed .aimem.json must not block checkpoints).
func (p *Payload) ResolveProjectID() error {
	// Adapters that send the launch directory (the OpenCode plugin) get
	// the same resolution as the hooks: the directory whose .aimem.json
	// governs it.
	if p.ProjectDir != "" {
		p.ProjectDir = ident.ProjectDir(p.ProjectDir)
	}
	if p.ProjectDir != "" && p.Groups == nil {
		if g, err := ident.ProjectGroups(p.ProjectDir); err == nil {
			if g == nil {
				g = []string{}
			}
			p.Groups = &g
		}
	}
	if p.ProjectDir != "" && p.Hub == nil {
		if h, err := ident.ProjectHubName(p.ProjectDir); err == nil {
			p.Hub = &h
		} else {
			// A malformed hub name in a parseable .aimem.json must NOT
			// fall back to the default hub — that silently moves data
			// across the hub partition (ProjectHubName's own contract).
			// Route to a reserved unconfigured name instead: pushHub
			// spools under it and warns, local capture is untouched, and
			// once the config is fixed the periodic sync delivers from
			// the journal — nothing lost, nothing leaked.
			q := QuarantineHubName
			p.Hub = &q
		}
	}
	if p.ProjectID != "" || p.ProjectDir == "" {
		return nil
	}
	id, err := ident.ProjectID(p.ProjectDir)
	if err != nil {
		return err
	}
	p.ProjectID = id
	return nil
}

// Client submits payloads to the service with spool fallback.
type Client struct {
	root string
	http *http.Client
	// pushOff notes, once per run, a hub that has no checkpoint token.
	pushOff sync.Once
	// deadline ends a hook run (WithDeadline); zero outside a hook.
	deadline time.Time
	// deferredList names what this run left for a later one.
	deferredList []string
	// hubFailed holds the hubs (by URL) that failed a request in this run;
	// they are not asked again in it.
	hubFailed map[string]bool
}

// localTimeout bounds each call to the local service: hooks must never
// hang the coding client. A var so that TestMain can raise it for the
// in-process services.
var localTimeout = 5 * time.Second

func NewClient(stateRoot string) *Client {
	sock := server.SocketPath(stateRoot)
	return &Client{
		root: stateRoot,
		http: &http.Client{
			Timeout: localTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
		},
	}
}

func (c *Client) spoolPath() string { return filepath.Join(c.root, "spool", "pending.jsonl") }

// Redact sanitizes payload content adapter-side (first of the two redaction
// passes; the service redacts again at ingestion).
func Redact(p *Payload) {
	san := func(s string, cap int) string { out, _ := redact.String(s, cap); return out }
	e := &p.Event
	e.UserRequest = san(e.UserRequest, redact.DefaultMaxFieldBytes)
	e.AssistantReply = san(e.AssistantReply, redact.DefaultMaxFieldBytes)
	e.GitStatus = san(e.GitStatus, redact.DefaultMaxFieldBytes)
	e.ToolSummary, _ = redact.Strings(e.ToolSummary, 4096)
	e.TouchedPaths, _ = redact.Strings(e.TouchedPaths, 1024)
}

// Submit redacts and posts one payload. On service unavailability the record
// is spooled and Submit reports spooled=true with a nil error (fail-open).
// On success it also opportunistically replays any spooled backlog.
// Submit records a fresh checkpoint: local store plus real-time delivery
// to the project's hub.
func (c *Client) Submit(p *Payload) (spooled bool, err error) {
	return c.submit(p, true)
}

// SubmitLocal records an event WITHOUT any hub delivery — the anti-entropy
// import path. A synced event already exists on the peer it came from;
// re-pushing it would send it to this machine's DEFAULT hub, which is how
// a project bound to one hub ends up replicated onto another (exported
// events carry no binding, so the push targets the default). Sync must
// move data between peers, never broadcast it onward.
func (c *Client) SubmitLocal(p *Payload) (spooled bool, err error) {
	return c.submit(p, false)
}

func (c *Client) submit(p *Payload, toHub bool) (spooled bool, err error) {
	// Idempotent: fills project id and group membership from ProjectDir
	// for callers that construct payloads directly.
	if err := p.ResolveProjectID(); err != nil {
		return false, err
	}
	Redact(p)
	body, err := json.Marshal(p)
	if err != nil {
		return false, err
	}
	hubName := ""
	if p.Hub != nil {
		hubName = *p.Hub
	}
	if p.Degraded != "" {
		c.deferred(p.Degraded)
	}
	defer c.noteDeferred(p)
	if err := c.post(body); err != nil {
		// The service is UP and refused the record: spooling would replay
		// the same rejection forever and "unreachable" would mislead —
		// surface it now, exactly as post's contract says.
		if _, invalid := err.(*rejectError); invalid {
			return false, err
		}
		if serr := c.spool(body); serr != nil {
			return false, fmt.Errorf("submit failed (%v) and spool failed: %w", err, serr)
		}
		if c.outOfTime(err) {
			c.deferred("the local post (out of time)")
		} else {
			c.deferred(fmt.Sprintf("the local post (%v)", err))
			c.note("aimem: service unreachable, checkpoint spooled (%v)", err)
		}
		if toHub {
			c.pushHub(hubName, body)
			c.publishDocsQuiet(p)
		} // hub delivery is independent of local availability
		return true, nil
	}
	c.replaySpool()
	if toHub {
		c.pushHub(hubName, body)
		// Bound shared documents ride the same trigger, never the same
		// message: a separate CAS PUT per changed file (adapter/docs.go).
		c.publishDocsQuiet(p)
	}
	return false, nil
}

func (c *Client) post(body []byte) error {
	ctx, cancel := c.reqCtx()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://aimem/v1/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("service error HTTP %d", resp.StatusCode)
	}
	// 4xx means the record itself is invalid — spooling would replay the
	// same rejection forever, so surface it instead.
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return &rejectError{msg: fmt.Sprintf("rejected (HTTP %d): %s", resp.StatusCode, e.Error)}
	}
	return nil
}

type rejectError struct{ msg string }

func (e *rejectError) Error() string { return e.msg }

// spool appends one already-redacted record atomically (single O_APPEND write).
func (c *Client) spool(line []byte) error {
	if err := os.MkdirAll(filepath.Dir(c.spoolPath()), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(c.spoolPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// sweepOrphanedClaims returns crash-orphaned replay claims to the spool.
// A replay claims the spool by renaming it and removes the claim when
// done — but a hard kill mid-replay leaves the claim file holding
// records that nothing would ever re-scan: a silent drop. Events are
// idempotent end to end (idempotency_key), so the rare sweep of a claim
// whose owner is still alive only causes a double-post the service
// deduplicates. A claim we cannot read (Windows keeps it locked while
// its live owner reads it) is skipped and swept next time.
func sweepOrphanedClaims(spool string) {
	for _, cl := range spoolSiblings(spool, ".replay-") {
		data, err := os.ReadFile(cl)
		if err != nil {
			continue
		}
		if len(bytes.TrimSpace(data)) > 0 {
			f, err := os.OpenFile(spool, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				continue
			}
			if !bytes.HasSuffix(data, []byte{'\n'}) {
				data = append(data, '\n')
			}
			if _, err := f.Write(data); err != nil {
				f.Close()
				continue
			}
			f.Close()
		}
		os.Remove(cl)
	}
}

// ReplaySpool drains the spool into the service. The file is claimed by
// atomic rename first so concurrent adapters never double-replay; records
// that still fail transport, and the untouched rest when the run runs
// out of time, go back to the spool, in order; requeued counts the
// records kept back, not the untouched rest. Invalid
// records are dropped with a warning (they would never succeed). A run
// already out of time does not claim the spool. If the write fails, the
// claim is left in place and sweepOrphanedClaims returns it to the spool.
func (c *Client) ReplaySpool() (replayed, requeued int, err error) {
	if c.left() <= 0 {
		if fi, err := os.Stat(c.spoolPath()); err == nil && fi.Size() > 0 {
			c.deferred("the local spool replay (out of time)")
		}
		return 0, 0, nil
	}
	sweepOrphanedClaims(c.spoolPath())
	claim := c.spoolPath() + fmt.Sprintf(".replay-%d", os.Getpid())
	if err := os.Rename(c.spoolPath(), claim); err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	data, err := os.ReadFile(claim)
	if err != nil {
		return 0, 0, err // the claim stays for sweepOrphanedClaims
	}
	var kept bytes.Buffer
	rest := data
	for len(rest) > 0 {
		line, after, _ := bytes.Cut(rest, []byte{'\n'})
		if len(bytes.TrimSpace(line)) == 0 {
			rest = after
			continue
		}
		if c.left() <= 0 {
			// Out of time: the rest goes back as it is, neither walked
			// nor counted.
			c.deferred("the local spool replay (out of time)")
			break
		}
		rest = after
		if err := c.post(line); err != nil {
			if _, invalid := err.(*rejectError); invalid {
				c.note("aimem: dropping invalid spooled record: %v", err)
				continue
			}
			if kept.Len() > 0 {
				kept.WriteByte('\n')
			}
			kept.Write(line)
			requeued++
			continue
		}
		replayed++
	}
	if err := writeBack(c.spoolPath(), kept.Bytes(), rest); err != nil {
		return replayed, requeued, err // the claim stays for sweepOrphanedClaims
	}
	os.Remove(claim)
	return replayed, requeued, nil
}

// noteDeferred leaves the one adapter.log line of a hook run that left
// work for a later run: which event, what it left and why. No content of
// the turn goes into it.
func (c *Client) noteDeferred(p *Payload) {
	list := c.deferredList
	c.deferredList = nil // one client may submit many events (an import)
	if len(list) == 0 {
		return
	}
	c.note("aimem: the hook for the %s event of session %s turn %s left work for a later run: %s; all of it stays queued unless named LOST",
		p.Event.Kind, p.Event.SessionID, p.Event.TurnID, strings.Join(list, ", "))
}

func (c *Client) replaySpool() {
	if n, _, err := c.ReplaySpool(); err == nil && n > 0 {
		c.note("aimem: replayed %d spooled checkpoint(s)", n)
	}
}
