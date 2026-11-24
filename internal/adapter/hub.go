package adapter

// Hub push-on-checkpoint: when a hub is configured, every locally-accepted
// checkpoint is also POSTed to the central aimem over HTTP(S) with a
// bearer token, so other machines' agents see it at ask-time instead of at
// the next periodic sync. Hub unavailability degrades to a dedicated spool
// (hub.jsonl) flushed opportunistically — local durability never depends on
// the network, and the periodic sync remains the anti-entropy backstop.

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aimem/internal/privatefile"
)

// VersionAtLeast parses release-shaped versions ("v0.3.24", plus git
// describe suffixes) and reports v >= major.minor.patch. Unparseable
// versions ("dev", "") report false: requiring a capability of a build
// we cannot date would break sync against older source installs.
func VersionAtLeast(v string, major, minor, patch int) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var a, b, c int
	if n, err := fmt.Sscanf(v, "%d.%d.%d", &a, &b, &c); err != nil || n != 3 {
		return false
	}
	if a != major {
		return a > major
	}
	if b != minor {
		return b > minor
	}
	return c >= patch
}

// HubConfig is one hub entry in <state-root>/hub.json (owner-only — it
// holds tokens). A machine may know several hubs (e.g. a work hub and a
// home hub) so that projects on the same machine can keep their data on
// physically separate servers; a project picks its hub with a
// {"hub": "<name>"} binding in .aimem.json, everything else uses the
// default hub. Manage entries with `aimem hub add/rm/default`.
type HubConfig struct {
	URL   string `json:"url"` // e.g. https://hub.example.com:8440
	Token string `json:"token"`
	Sync  string `json:"sync,omitempty"` // optional ssh destination for `aimem sync --hub`
	// TaskToken is the OS user's per-hub task credential, used unless a
	// checkout requires a local override. Separate from Token on purpose:
	// the checkpoint token is a shared writer credential and must never
	// carry task authority.
	TaskToken string `json:"task_token,omitempty"`
	// TaskTokenMeta is what the hub said about TaskToken when it was
	// stored; nil when it was not asked (or before this was recorded).
	TaskTokenMeta *TokenMeta `json:"task_token_meta,omitempty"`
	// Insecure skips TLS certificate verification for this hub — for the
	// self-signed phase of a fresh hub (still TLS on the wire + bearer
	// token). Drop it once a real certificate is installed.
	Insecure bool `json:"insecure,omitempty"`
	// CAFile, when set, is the PEM bundle every client path trusts for
	// this hub instead of the system roots (a hub with a private CA).
	// `aimem hub add … --ca-file` records it.
	CAFile string `json:"ca_file,omitempty"`
	// Pin, when set, is the SHA-256 of the hub certificate's public key
	// (sha256-BASE64): the hub is trusted by that key alone. At most one of
	// CAFile and Pin; `aimem hub add … --pin` records it.
	Pin string `json:"pin,omitempty"`
	// Strict marks an in-memory copy of the entry whose Token is an
	// individual credential: HTTPClient then hands out only the strict
	// client (https with the entry's trust, no proxy, no redirect), and a
	// failing transport when that client cannot be built. It is never
	// stored.
	Strict bool `json:"-"`
}

// TokenMeta is what a hub's identity answer says about a task credential:
// its label, scope and expiry (RFC 3339). It is recorded beside the
// credential when it is stored, because a token the hub refuses cannot be
// asked about itself; a refusal names the token from this record. Nothing
// in it is secret.
type TokenMeta struct {
	Label     string `json:"label,omitempty"`
	Scope     string `json:"scope,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// HTTPClient returns the client to talk to this hub with: the strict client
// for an entry marked Strict, else its ca_file or pin when set (TLSConfig),
// else the self-signed phase or the system roots.
func (h *HubConfig) HTTPClient() *http.Client {
	if h.Strict {
		c, err := h.StrictClient(PushTimeout)
		if err != nil {
			return &http.Client{Transport: failTransport{err}} // nothing is sent
		}
		return c
	}
	if h.CAFile != "" || h.Pin != "" {
		return h.cachedClient()
	}
	if h.Insecure {
		return &http.Client{Timeout: PushTimeout, Transport: insecureTransport}
	}
	return &http.Client{Timeout: PushTimeout}
}

// hubsFile is the on-disk shape of hub.json. Two generations coexist:
// the legacy flat single-hub form {url,token} and the named form
// {hubs:{name:{...}}, default:"name"}; the flat fields read as a hub
// named "default" so existing installs keep working untouched.
type hubsFile struct {
	Hubs    map[string]*HubConfig `json:"hubs,omitempty"`
	Default string                `json:"default,omitempty"`
	URL     string                `json:"url,omitempty"`
	Token   string                `json:"token,omitempty"`
	Sync    string                `json:"sync,omitempty"`
}

// DefaultHubName is the name a legacy flat config file is folded under.
const DefaultHubName = "default"

func hubConfigPath(root string) string { return filepath.Join(root, "hub.json") }

// LoadHubs returns every configured hub and the default hub's name.
// A nil map means no hub is configured.
func LoadHubs(root string) (map[string]*HubConfig, string) {
	raw, err := os.ReadFile(hubConfigPath(root))
	if err != nil {
		return nil, ""
	}
	var f hubsFile
	if json.Unmarshal(raw, &f) != nil {
		return nil, ""
	}
	hubs := f.Hubs
	if hubs == nil {
		hubs = map[string]*HubConfig{}
	}
	if f.URL != "" {
		if _, ok := hubs[DefaultHubName]; !ok {
			hubs[DefaultHubName] = &HubConfig{URL: f.URL, Token: f.Token, Sync: f.Sync}
		}
		if f.Default == "" {
			f.Default = DefaultHubName
		}
	}
	for k, v := range hubs {
		if v == nil || v.URL == "" {
			delete(hubs, k)
		}
	}
	if len(hubs) == 0 {
		return nil, ""
	}
	if _, ok := hubs[f.Default]; !ok {
		// Unset or dangling default: unambiguous with one hub, otherwise
		// no default (unbound projects then have nowhere to push).
		f.Default = ""
		if len(hubs) == 1 {
			for k := range hubs {
				f.Default = k
			}
		}
	}
	return hubs, f.Default
}

// SaveHubs writes the full hub set in the named format.
func SaveHubs(root string, hubs map[string]*HubConfig, def string) error {
	b, err := json.MarshalIndent(hubsFile{Hubs: hubs, Default: def}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	// hub.json holds bearers, so it is created owner-only (a protected
	// DACL on Windows, where a file mode alone sets no ACL and the file
	// would inherit its folder's) and replaced atomically. Every save
	// rewrites it this way, which also repairs an older file with a broader
	// ACL.
	path := hubConfigPath(root)
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	f, err := privatefile.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// On Windows a rename over a file that another aimem process is reading
	// at that instant fails; those reads are brief, so a short retry wins.
	for attempt := 0; ; attempt++ {
		err = os.Rename(tmp, path)
		if err == nil {
			return nil
		}
		if attempt == 9 {
			os.Remove(tmp)
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// HubConfigPrivate reports whether hub.json, when it exists, is readable by
// its owner alone: nil when it is, or when there is no file.
func HubConfigPrivate(root string) error {
	path := hubConfigPath(root)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return privatefile.Check(path)
}

// LoadHub returns the default hub, or nil when none is configured. Callers
// that care about a specific project's hub use ResolveHub instead.
func LoadHub(root string) *HubConfig {
	hubs, def := LoadHubs(root)
	if hubs == nil || def == "" {
		return nil
	}
	return hubs[def]
}

// SaveHub sets/replaces the default hub's URL and checkpoint token,
// preserving other named entries and the default entry's other settings
// (legacy `aimem hub <url> <token>` path).
func SaveHub(root string, c *HubConfig) error {
	hubs, def := LoadHubs(root)
	if hubs == nil {
		hubs = map[string]*HubConfig{}
	}
	if def == "" {
		def = DefaultHubName
	}
	hubs[def] = c.Over(hubs[def])
	return SaveHubs(root, hubs, def)
}

// Over returns c completed from prev for a re-run of `hub add` / `hub <url>
// <token>` against the SAME host: the sync destination and the task
// credential carry over when not restated, and so does the hub's trust
// (ca_file or pin) unless a new one is given. A different URL inherits
// nothing (a per-user credential must never travel to another host), and
// the TLS downgrade is never inherited: --insecure is restated or gone.
func (c *HubConfig) Over(prev *HubConfig) *HubConfig {
	out := *c
	if prev == nil || prev.URL != c.URL {
		return &out
	}
	if out.Sync == "" {
		out.Sync = prev.Sync
	}
	if out.TaskToken == "" {
		out.TaskToken, out.TaskTokenMeta = prev.TaskToken, prev.TaskTokenMeta
	}
	if out.CAFile == "" && out.Pin == "" {
		out.CAFile, out.Pin = prev.CAFile, prev.Pin
	}
	return &out
}

// OverSystemRoots is Over for `hub add --system-roots`: the entry keeps
// everything Over carries over except its recorded trust, so it verifies
// against the system roots again.
func (c *HubConfig) OverSystemRoots(prev *HubConfig) *HubConfig {
	out := c.Over(prev)
	out.CAFile, out.Pin = "", ""
	return out
}

// ResolveHub maps a project's hub binding to a configured hub: the named
// QuarantineHubName is the reserved routing target for a project whose
// .aimem.json carries a hub name that fails validation. It is never a
// configured hub, so pushes spool under it (visible in `aimem logs`)
// instead of riding to the DEFAULT hub — the leak ProjectHubName's
// contract forbids. Once the config is fixed, sync delivers from the
// journal; the quarantine spool is a redundant, idempotent copy.
const QuarantineHubName = "invalid-hub-binding"

// entry when it exists, else the default. Returns the resolved name so
// spooling stays per-hub.
func ResolveHub(root, name string) (string, *HubConfig) {
	hubs, def := LoadHubs(root)
	if hubs == nil {
		return "", nil
	}
	if name != "" {
		if h, ok := hubs[name]; ok {
			return name, h
		}
		// A project bound to a hub this machine has NOT configured must
		// never fall back to the default: that silently moves its data
		// across the work/home partition — observed live 2026-08-29 when
		// a freshly pinned project's checkpoints would have routed to the
		// default hub. Return the name with no config: push spools under
		// it (delivered once `aimem hub add` runs), everything else skips.
		return name, nil
	}
	if def == "" {
		return "", nil
	}
	return def, hubs[def]
}

// hubSpoolPathFor: one spool per hub so an outage of one hub never blocks
// or misroutes another. The default-era file name is kept for the hub
// named "default" so pre-multi-hub spools drain without migration.
func (c *Client) hubSpoolPathFor(name string) string {
	if name == DefaultHubName {
		return filepath.Join(c.root, "spool", "hub.jsonl")
	}
	return filepath.Join(c.root, "spool", "hub-"+name+".jsonl")
}

// PushTimeout bounds each request of the push clients (HTTPClient). It
// is a var only so that test mains whose loopback fakes answer these
// clients can raise it; nothing else changes it.
var PushTimeout = 5 * time.Second

// insecureTransport serves hubs in their self-signed phase (Insecure flag).
var insecureTransport = &http.Transport{
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}

// pushHub delivers one already-redacted payload to the configured hub.
// It is written to the hub's spool first, before any hub request, so that
// a run cut at any moment (a client's hook timeout, a kill) leaves it
// queued; the spool is then flushed, newest first, within the run's
// deadline. Never returns an error to the caller: hub delivery is
// best-effort real-time, the spool and the periodic sync guarantee
// eventual delivery, and a checkpoint delivered twice is one event on the
// hub (idempotency key).
func (c *Client) pushHub(hubName string, body []byte) {
	name, hub := ResolveHub(c.root, hubName)
	if hub == nil {
		if name == QuarantineHubName {
			// Invalid hub name in .aimem.json (see spool.go): quarantine
			// with a message that names the actual fix.
			if err := c.queue(c.hubSpoolPathFor(name), body); err == nil {
				c.note("aimem: this project's .aimem.json hub name is INVALID — checkpoint quarantined, not sent to the default hub; fix the \"hub\" value (lowercase letters, digits, dashes)")
			}
			return
		}
		if name != "" {
			// Bound to an unconfigured hub: spool under its name so the
			// data delivers the moment `aimem hub add <name>` runs —
			// never misrouted, never dropped.
			if err := c.queue(c.hubSpoolPathFor(name), body); err == nil {
				c.note("aimem: hub %q is not configured on this machine — checkpoint spooled for it (aimem hub add %s <url> <token>)", name, name)
			}
		}
		return
	}
	if hub.Token == "" {
		// An installation enrolled through D1 holds only its individual
		// credential; journal push needs the hub's checkpoint token, which
		// enrollment never delivers. Spooling would only grow a queue that
		// can never drain, so the push is skipped and said once.
		c.pushOff.Do(func() {
			c.note("aimem: hub %q has no checkpoint token, so journal push to it is off (an enrolled installation receives none; add one with aimem hub add)", name)
		})
		return
	}
	spool := c.hubSpoolPathFor(name)
	// Durable before the hub is asked, in a file no other run writes: a run
	// killed at any later step leaves the record here, and a later run
	// moves it into the spool.
	pend, perr := pendingWrite(spool, body)
	queued := false
	if perr != nil {
		// No pending file: the spool itself keeps the record before the
		// hub is asked. If the hub takes it, the flush below or a later
		// run delivers it once more, which the hub keeps as one event.
		if aerr := c.spoolTo(spool, body); aerr == nil {
			queued = true
		} else {
			c.note("aimem: neither a pending file (%v) nor hub spool %s (%v) can be written; nothing keeps the checkpoint for the hub, and it goes to the hub directly, its only chance there",
				perr, filepath.Base(spool), aerr)
		}
	}
	// Recovery of runs that died comes only once this run's own record is
	// kept.
	c.sweepPending(spool)
	// The record goes from memory. A hub that does not answer costs this
	// one request and one append below, whatever the backlog's size.
	if err := c.hubTry(hub, body); err != nil {
		if pend != nil {
			switch aerr := c.spoolTo(spool, body); {
			case aerr == nil:
				removePending(pend)
			case errors.Is(aerr, errQueueBusy):
				// Another run holds the queue: the record stays in its
				// pending file, which a later run moves in.
			default:
				c.note("aimem: hub spool %s cannot be written (%v); the checkpoint stays in %s for a later run", filepath.Base(spool), aerr, filepath.Base(pend.path))
			}
		} else if !queued {
			c.deferred("the checkpoint for the hub, LOST: nothing could be written and the hub did not take it")
			c.note("aimem: hub unreachable and nothing could be written: the checkpoint is lost to the hub (%v); only its local copy, in the journal or the local spool, remains", err)
			return
		}
		c.hubDown(hub, spool, err)
		return
	}
	removePending(pend)
	c.flushHubSpool(hub, spool)
}

// hubRequestMin is the least time left in a run worth starting a hub
// request with; below it the request is left to a later run.
const hubRequestMin = 500 * time.Millisecond

func (c *Client) hubPost(hub *HubConfig, body []byte) error {
	ctx, cancel := c.reqCtx()
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", hub.URL+"/v1/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+hub.Token)
	resp, err := hub.HTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("hub HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("hub rejected token (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests {
		// Come back later, never "the record is invalid": the events
		// route answers neither, so a proxy in front of the hub did. The
		// record stays queued and the run stops asking this hub, as for a
		// 5xx. Retry-After is named, not waited for: a hook has seconds,
		// and the next checkpoint comes at the user's pace.
		return &hubLaterError{status: resp.StatusCode, retryAfter: retryAfterText(resp.Header.Get("Retry-After"))}
	}
	// Other 4xx: record is invalid; retrying forever is pointless — drop
	// from the hub path (it stays in the local journal regardless).
	return nil
}

// hubLaterError is a hub's (or its proxy's) answer to come back later.
type hubLaterError struct {
	status     int
	retryAfter string // as retryAfterText renders it; empty when none was sent
}

func (e *hubLaterError) Error() string {
	if e.retryAfter == "" {
		return fmt.Sprintf("hub HTTP %d", e.status)
	}
	return fmt.Sprintf("hub HTTP %d, Retry-After %s", e.status, e.retryAfter)
}

// retryAfterText renders a Retry-After value for the run's line from what
// it parses to, never from the text a proxy sent: a number of seconds, an
// HTTP date in its canonical form, or "unreadable".
func retryAfterText(v string) string {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return ""
	case len(v) <= 10 && strings.Trim(v, "0123456789") == "":
		n, err := strconv.Atoi(v)
		if err != nil {
			return "unreadable"
		}
		return fmt.Sprintf("%d s", n)
	}
	if t, err := http.ParseTime(v); err == nil {
		return t.UTC().Format(http.TimeFormat)
	}
	return "unreadable"
}

// hubTry posts one record unless the run is out of time or the hub
// already failed in it; a failure marks the hub failed for the run.
func (c *Client) hubTry(hub *HubConfig, body []byte) error {
	if c.left() < hubRequestMin {
		return errShare
	}
	if c.hubFailed[hub.URL] {
		return errHubFailed
	}
	err := c.hubPost(hub, body)
	if err != nil && !c.outOfTime(err) {
		c.markHubFailed(hub)
	}
	return err
}

// hubDown records a delivery that stopped at err in the run's line, and
// says once how big the spool has grown. The record it stopped at is
// queued: in the spool or its pending file.
func (c *Client) hubDown(hub *HubConfig, spool string, err error) {
	switch {
	case errors.Is(err, errShare) || c.outOfTime(err):
		c.deferred("the hub delivery (out of time)")
	case errors.Is(err, errHubFailed):
		c.deferred("the hub delivery (the hub failed earlier in this run)")
	default:
		c.deferred(fmt.Sprintf("the hub delivery (%v)", err))
		if later := (*hubLaterError)(nil); errors.As(err, &later) {
			c.note("aimem: the hub asked to come back later, checkpoint queued for hub (%v)", err)
		} else {
			c.note("aimem: hub unreachable, checkpoint queued for hub (%v)", err)
		}
		// A spool that keeps growing means the hub has been down — or
		// the token rejected — for a long time; the per-checkpoint
		// line above is easy to tune out, a size is not.
		if fi, e := os.Stat(spool); e == nil && fi.Size() > hubSpoolWarnBytes {
			c.note("aimem: hub spool %s holds %d MB of undelivered checkpoints — check the hub and its token",
				filepath.Base(spool), fi.Size()>>20)
		}
	}
}

// pendingStale is the age past which a pending file is taken to be left
// by a run that died. A live run keeps one for a single hub request
// (PushTimeout at most, less inside a hook) and one append, so a minute
// is far past any live run; a run stalled longer than that (a suspended
// machine) and swept meanwhile delivers its record twice, which the hub
// keeps as one event (idempotency key).
var pendingStale = time.Minute

// pendingWrite is writePendingFile; a test replaces it to fail.
var pendingWrite = writePendingFile

// pendingFile is a pending file this run wrote: its path, and what it
// was when written, so that the run removes only its own file.
type pendingFile struct {
	path string
	fi   os.FileInfo
}

// writePendingFile keeps one record in a file of its own beside spool,
// created exclusively under a name that carries the process id and a
// nanosecond clock, so no name recurs while its writer lives. It holds
// the spool's pending lock shared from before the file's creation to
// after its close, waiting at most appendLockWait: the pending sweep,
// which lists the files under the same lock exclusive, does not list a
// file its writer has not finished. Past the wait, or on a filesystem
// that refuses the lock, the file is written without it, and the sweep's
// age rule (pendingStale) keeps it off a file that fresh.
func writePendingFile(spool string, body []byte) (*pendingFile, error) {
	l, err := lockSpoolShared(spool, "pending", appendLockWait)
	if nl := (*noLocksError)(nil); errors.Is(err, errQueueBusy) || errors.As(err, &nl) {
		l = nil
	} else if err != nil {
		return nil, err
	}
	defer l.release()
	// An older client's sweep takes no lock: it may remove the file while
	// it is still empty, after a stall of over pendingStale here. A file
	// whose name no longer names it is written once more, fresh.
	for try := 0; ; try++ {
		p, err := writePendingOnce(spool, body)
		if err != nil || p != nil {
			return p, err
		}
		if try > 0 {
			return nil, errPendingTaken
		}
	}
}

// errPendingTaken: another run removed a pending file twice before its
// record was in it.
var errPendingTaken = errors.New("another run removed the pending file before its record was written")

// writePendingOnce writes one pending file. It returns nil without an
// error when the file's name no longer names it after the close.
func writePendingOnce(spool string, body []byte) (*pendingFile, error) {
	prefix := fmt.Sprintf("%s.pending-%d-%d-", filepath.Base(spool), os.Getpid(), time.Now().UnixNano())
	f, err := os.CreateTemp(filepath.Dir(spool), prefix+"*")
	if err != nil {
		return nil, err
	}
	if duringPendingWrite != nil {
		duringPendingWrite(f.Name(), false)
	}
	_, err = f.Write(append(append([]byte(nil), body...), '\n'))
	if duringPendingWrite != nil {
		duringPendingWrite(f.Name(), true)
	}
	fi, serr := f.Stat()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = serr
	}
	if err != nil {
		os.Remove(f.Name())
		return nil, err
	}
	// The name is this writer's alone (pid and clock), so the same file
	// under it means no run removed it; what it is now, after the close,
	// is what removePending compares against later.
	cur, err := os.Stat(f.Name())
	if err != nil || !os.SameFile(cur, fi) {
		return nil, nil
	}
	return &pendingFile{f.Name(), cur}, nil
}

// isMine reports whether p's name still names the file this run wrote.
// The modification time and size are compared too: a filesystem may give
// a file created at the freed name the removed file's inode at once.
func (p *pendingFile) isMine() bool {
	cur, err := os.Stat(p.path)
	return err == nil && os.SameFile(cur, p.fi) && cur.ModTime().Equal(p.fi.ModTime()) && cur.Size() == p.fi.Size()
}

// duringPendingWrite runs under the shared lock after a pending file's
// creation (written false) and after its write (true); a test sweeps in
// those windows.
var duringPendingWrite func(path string, written bool)

// removePending removes the pending file this run wrote, and only that
// one: a sweep may have moved it in and another run may have created a
// file under the freed name since.
func removePending(p *pendingFile) {
	if p != nil && p.isMine() {
		os.Remove(p.path)
	}
}

// sweepPending moves the pending files of runs that died into the spool,
// while the run has time for its hub request: a run without it does not
// list the directory, and one that runs out midway stops at the next
// file; the files it does not reach wait for a later run. It runs as the
// one run holding the spool's flush lock, tried once: a run that finds it
// held leaves the pending files to the run holding it, so no two runs
// move one file. It lists the files holding the pending lock exclusive,
// tried once, so every file it lists is whole (writePendingFile), but
// for one written past the lock's wait, which the age rule keeps it off;
// a run that finds a writer holding it leaves the files to a later run. A file is
// moved only once older than pendingStale, which keeps the sweep off a
// live run's file between its write and the hub's answer, and removed
// only after its record is in the spool. A record whose run had already
// delivered it, or appended it, before dying is delivered twice; the hub
// keeps one event per idempotency key. An empty stale file is removed: a
// writer of this release leaves one only when it died before its write,
// and an older client's is a record it never wrote.
func (c *Client) sweepPending(spool string) {
	if c.left() < hubRequestMin {
		return // nothing looked at, nothing to name
	}
	fl, err := lockSpool(spool, "flush", 0)
	if err != nil {
		c.noteNoLocks(err) // busy: another run sweeps
		return
	}
	defer fl.release()
	pl, err := lockSpool(spool, "pending", 0)
	if err != nil {
		c.noteNoLocks(err) // busy: a run is writing one
		return
	}
	files := listSiblings(spool, ".pending-")
	pl.release()
	for _, p := range files {
		if c.left() < hubRequestMin {
			c.deferred("the pending files of interrupted runs, if any (out of time)")
			return
		}
		fi, err := os.Stat(p)
		if err != nil || time.Since(fi.ModTime()) < pendingStale {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if line := bytes.TrimSpace(data); len(line) > 0 {
			if err := appendRecords(spool, line, appendLockWait, nil); errors.Is(err, errQueueBusy) {
				return // another run holds the queue: the rest waits for a later run
			} else if err != nil {
				continue
			}
		}
		os.Remove(p)
	}
}

// listSiblings is spoolSiblings; a test replaces it.
var listSiblings = spoolSiblings

// spoolSiblings lists the files beside spool whose names start with the
// spool's name and infix (its pending files, its replay claims). The
// directory is read, never globbed: a state directory may be named with
// characters a pattern would take as wildcards.
func spoolSiblings(spool, infix string) []string {
	entries, err := os.ReadDir(filepath.Dir(spool))
	if err != nil {
		return nil
	}
	prefix := filepath.Base(spool) + infix
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(filepath.Dir(spool), e.Name()))
		}
	}
	return out
}

// appendLines appends records (newline-separated, a final newline optional)
// to the spool in one write, under its append lock.
func appendLines(path string, lines []byte) error {
	return appendRecords(path, lines, appendLockWait, nil)
}

const (
	// hubSpoolWarnBytes: past this, the queued-checkpoint stderr line
	// gains the spool's size so long outages are hard to miss.
	hubSpoolWarnBytes = 8 << 20
	// hubFlushLimit bounds how many records one run tries to deliver;
	// the remainder stays spooled for the next checkpoint or sync.
	hubFlushLimit = 100
)

// flushHubSpool drains one hub's backlog after the hub has taken this
// run's checkpoint, in order, at most hubFlushLimit records, as the one run
// holding the spool's flush lock (drainSpool): a run that finds another
// delivering it leaves it alone and says nothing. It does not read the
// backlog when the run has no time left for a request. It stops at the
// first failure, since every further try would wait out the same hub
// inside a hook, at the limit and at the run's deadline; the record it
// stopped at and the rest stay queued, in order.
func (c *Client) flushHubSpool(hub *HubConfig, spool string) {
	noTime := func() bool { // and says so when a backlog waits
		if c.left() >= hubRequestMin {
			return false
		}
		if fi, err := os.Stat(spool); err == nil && fi.Size() > 0 {
			c.hubDown(hub, spool, errShare)
		}
		return true
	}
	if noTime() {
		return
	}
	delivered := 0
	stop, err := c.drainSpool(spool, hubRequestMin, noTime, func(line []byte) (bool, error) {
		if delivered >= hubFlushLimit {
			return false, errFlushLimit
		}
		if err := c.hubTry(hub, line); err != nil {
			return false, err
		}
		delivered++
		return false, nil
	})
	switch {
	case stop == errFlushLimit:
		c.deferred(fmt.Sprintf("the hub backlog past the flush limit of %d records", hubFlushLimit))
	case stop != nil:
		c.hubDown(hub, spool, stop)
	}
	if delivered > 0 {
		c.note("aimem: delivered %d queued checkpoint(s) to hub", delivered)
	}
	if err != nil && !errors.Is(err, errQueueBusy) && !c.noteNoLocks(err) { // busy: another run delivers it
		c.deferred(fmt.Sprintf("the hub backlog (%v)", err))
	}
	if stop != nil && delivered > 0 {
		c.note("aimem: more remain spooled (bounded flush); they drain on coming checkpoints")
	}
}

// errHubFailed stops a delivery to a hub that failed earlier in the run.
var errHubFailed = errors.New("the hub failed earlier in this run")

// markHubFailed records that hub failed a request in this run.
func (c *Client) markHubFailed(hub *HubConfig) {
	if c.hubFailed == nil {
		c.hubFailed = map[string]bool{}
	}
	c.hubFailed[hub.URL] = true
}

// errFlushLimit stops a flush at hubFlushLimit records.
var errFlushLimit = errors.New("flush limit reached")

// spoolTo appends one record to the named spool, waiting at most
// appendLockWait for another run's hold on it (errQueueBusy past that).
func (c *Client) spoolTo(path string, line []byte) error {
	return appendRecords(path, line, appendLockWait, func(err error) { c.noteNoLocks(err) })
}

// queue appends one record to spool; when another run holds the spool past
// appendLockWait, the record goes into a pending file of its own, which a
// later run moves in.
func (c *Client) queue(spool string, line []byte) error {
	err := c.spoolTo(spool, line)
	if !errors.Is(err, errQueueBusy) {
		return err
	}
	_, err = pendingWrite(spool, line)
	return err
}
