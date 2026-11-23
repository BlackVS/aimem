package adapter

// Hub push-on-checkpoint: when a hub is configured, every locally-accepted
// checkpoint is also POSTed to the central aimem over HTTP(S) with a
// bearer token, so other machines' agents see it at ask-time instead of at
// the next periodic sync. Hub unavailability degrades to a dedicated spool
// (hub.jsonl) flushed opportunistically — local durability never depends on
// the network, and the periodic sync remains the anti-entropy backstop.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
			if err := c.spoolTo(c.hubSpoolPathFor(name), body); err == nil {
				c.note("aimem: this project's .aimem.json hub name is INVALID — checkpoint quarantined, not sent to the default hub; fix the \"hub\" value (lowercase letters, digits, dashes)")
			}
			return
		}
		if name != "" {
			// Bound to an unconfigured hub: spool under its name so the
			// data delivers the moment `aimem hub add <name>` runs —
			// never misrouted, never dropped.
			if err := c.spoolTo(c.hubSpoolPathFor(name), body); err == nil {
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
	if err := c.spoolTo(spool, body); err != nil {
		// No queue to fall back on: one direct attempt, as before the
		// spool came first.
		if err := c.hubPost(hub, body); err != nil {
			c.note("aimem: hub unreachable and its spool cannot be written, checkpoint not queued for hub (%v)", err)
		}
		return
	}
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
	// Other 4xx: record is invalid; retrying forever is pointless — drop
	// from the hub path (it stays in the local journal regardless).
	return nil
}

const (
	// hubSpoolWarnBytes: past this, the queued-checkpoint stderr line
	// gains the spool's size so long outages are hard to miss.
	hubSpoolWarnBytes = 8 << 20
	// hubFlushLimit bounds how many records one run tries to deliver;
	// the remainder stays spooled for the next checkpoint or sync.
	hubFlushLimit = 100
)

// flushHubSpool delivers one hub's spool: the newest record first (the
// checkpoint this run just queued), then the backlog in order, at most
// hubFlushLimit records. It stops at the first failure, since every
// further try would wait out the same unreachable hub inside a hook, and
// at the run's deadline. What it does not deliver stays spooled.
func (c *Client) flushHubSpool(hub *HubConfig, spool string) {
	sweepOrphanedClaims(spool)
	claim := spool + fmt.Sprintf(".replay-%d", os.Getpid())
	if err := os.Rename(spool, claim); err != nil {
		return // nothing queued, or another run holds it and delivers it
	}
	defer os.Remove(claim)
	data, err := os.ReadFile(claim)
	if err != nil {
		os.Rename(claim, spool) // keep it queued
		return
	}
	var lines [][]byte
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return
	}
	order := append([][]byte{lines[len(lines)-1]}, lines[:len(lines)-1]...)
	delivered, kept := 0, 0
	var stop error
	for i, line := range order {
		if stop == nil && i >= hubFlushLimit {
			stop = errFlushLimit
		}
		if stop == nil && c.left() < hubRequestMin {
			stop = errShare
		}
		if stop == nil {
			if err := c.hubPost(hub, line); err != nil {
				stop = err
			} else {
				delivered++
				continue
			}
		}
		c.spoolTo(spool, line)
		kept++
	}
	if stop != nil && stop != errFlushLimit && stop != errShare {
		c.markHubFailed(hub)
	}
	if errors.Is(stop, errShare) || errors.Is(stop, context.DeadlineExceeded) {
		c.deferred("the hub delivery")
	} else if stop != nil && stop != errFlushLimit && delivered == 0 {
		c.note("aimem: hub unreachable, checkpoint queued for hub (%v)", stop)
		// A spool that keeps growing means the hub has been down — or
		// the token rejected — for a long time; the per-checkpoint
		// line above is easy to tune out, a size is not.
		if fi, e := os.Stat(spool); e == nil && fi.Size() > hubSpoolWarnBytes {
			c.note("aimem: hub spool %s holds %d MB of undelivered checkpoints — check the hub and its token",
				filepath.Base(spool), fi.Size()>>20)
		}
	}
	if delivered > 1 {
		c.note("aimem: delivered %d queued checkpoint(s) to hub", delivered-1)
	}
	if kept > 0 && delivered > 0 {
		c.note("aimem: %d more remain spooled (bounded flush); they drain on coming checkpoints", kept)
	}
}

// markHubFailed records that hub failed a request in this run.
func (c *Client) markHubFailed(hub *HubConfig) {
	if c.hubFailed == nil {
		c.hubFailed = map[string]bool{}
	}
	c.hubFailed[hub.URL] = true
}

// errFlushLimit stops a flush at hubFlushLimit records.
var errFlushLimit = errors.New("flush limit reached")

// spoolTo appends one record atomically to the named spool file.
func (c *Client) spoolTo(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
