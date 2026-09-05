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
}

// ResolveProjectID derives ProjectID from ProjectDir when unset, and
// stamps group membership while the directory is at hand (best-effort: a
// malformed .aimem.json must not block checkpoints).
func (p *Payload) ResolveProjectID() error {
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
}

func NewClient(stateRoot string) *Client {
	sock := server.SocketPath(stateRoot)
	return &Client{
		root: stateRoot,
		http: &http.Client{
			Timeout: 5 * time.Second, // hooks must never hang the coding client
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
		c.note("aimem: service unreachable, checkpoint spooled (%v)", err)
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
	resp, err := c.http.Post("http://aimem/v1/events", "application/json", bytes.NewReader(body))
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
	claims, _ := filepath.Glob(spool + ".replay-*")
	for _, cl := range claims {
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
// that still fail transport are re-spooled, invalid records are dropped
// with a warning (they would never succeed).
func (c *Client) ReplaySpool() (replayed, requeued int, err error) {
	sweepOrphanedClaims(c.spoolPath())
	claim := c.spoolPath() + fmt.Sprintf(".replay-%d", os.Getpid())
	if err := os.Rename(c.spoolPath(), claim); err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	defer os.Remove(claim)
	data, err := os.ReadFile(claim)
	if err != nil {
		return 0, 0, err
	}
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := c.post(line); err != nil {
			if _, invalid := err.(*rejectError); invalid {
				c.note("aimem: dropping invalid spooled record: %v", err)
				continue
			}
			c.spool(line)
			requeued++
			continue
		}
		replayed++
	}
	return replayed, requeued, nil
}

func (c *Client) replaySpool() {
	if n, _, err := c.ReplaySpool(); err == nil && n > 0 {
		c.note("aimem: replayed %d spooled checkpoint(s)", n)
	}
}
