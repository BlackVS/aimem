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
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// HubConfig is one hub entry in <state-root>/hub.json (mode 0600 — it
// holds tokens). A machine may know several hubs (e.g. a work hub and a
// home hub) so that projects on the same machine can keep their data on
// physically separate servers; a project picks its hub with a
// {"hub": "<name>"} binding in .aimem.json, everything else uses the
// default hub. Manage entries with `aimem hub add/rm/default`.
type HubConfig struct {
	URL   string `json:"url"` // e.g. https://hub.example.com:8440
	Token string `json:"token"`
	Sync  string `json:"sync,omitempty"` // optional ssh destination for `aimem sync --hub`
	// Insecure skips TLS certificate verification for this hub — for the
	// self-signed phase of a fresh hub (still TLS on the wire + bearer
	// token). Drop it once a real certificate is installed.
	Insecure bool `json:"insecure,omitempty"`
}

// HTTPClient returns the client to talk to this hub with, honoring the
// self-signed phase.
func (h *HubConfig) HTTPClient() *http.Client {
	if h.Insecure {
		return hubHTTPInsecure
	}
	return hubHTTP
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
	return os.WriteFile(hubConfigPath(root), append(b, '\n'), 0o600)
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

// SaveHub sets/replaces the default hub, preserving other named entries
// (legacy `aimem hub <url> <token>` path).
func SaveHub(root string, c *HubConfig) error {
	hubs, def := LoadHubs(root)
	if hubs == nil {
		hubs = map[string]*HubConfig{}
	}
	if def == "" {
		def = DefaultHubName
	}
	hubs[def] = c
	return SaveHubs(root, hubs, def)
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

var hubHTTP = &http.Client{Timeout: 5 * time.Second}

// hubHTTPInsecure serves hubs in their self-signed phase (Insecure flag).
var hubHTTPInsecure = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

// pushHub sends one already-redacted payload to the configured hub, spooling
// on any failure. Never returns an error to the caller — hub delivery is
// best-effort real-time; the periodic sync guarantees eventual delivery.
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
	spool := c.hubSpoolPathFor(name)
	if err := c.hubPost(hub, body); err != nil {
		if serr := c.spoolTo(spool, body); serr == nil {
			c.note("aimem: hub unreachable, checkpoint queued for hub (%v)", err)
			// A spool that keeps growing means the hub has been down — or
			// the token rejected — for a long time; the per-checkpoint
			// line above is easy to tune out, a size is not.
			if fi, e := os.Stat(spool); e == nil && fi.Size() > hubSpoolWarnBytes {
				c.note("aimem: hub spool %s holds %d MB of undelivered checkpoints — check the hub and its token",
					filepath.Base(spool), fi.Size()>>20)
			}
		}
		return
	}
	c.flushHubSpool(hub, spool)
}

func (c *Client) hubPost(hub *HubConfig, body []byte) error {
	req, err := http.NewRequest("POST", hub.URL+"/v1/events", bytes.NewReader(body))
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

// PushMeta writes one project meta key on a hub (PUT /v1/projects/{p}/
// meta/{key}). Used to distribute group config (charter/policy/chapters)
// the moment it is edited, so hub-side nightly curation never runs on
// stale routing rules. hubName selects a named hub ("" = default hub).
// Returns an error — callers decide whether the write is best-effort.
func PushMeta(root, hubName, project, key, value string) error {
	_, hub := ResolveHub(root, hubName)
	if hub == nil {
		return nil // no hub configured: local-only install, nothing to do
	}
	body, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("PUT",
		hub.URL+"/v1/projects/"+url.PathEscape(project)+"/meta/"+url.PathEscape(key),
		bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub HTTP %d", resp.StatusCode)
	}
	return nil
}

const (
	// hubSpoolWarnBytes: past this, the queued-checkpoint stderr line
	// gains the spool's size so long outages are hard to miss.
	hubSpoolWarnBytes = 8 << 20
	// hubFlushLimit bounds how much backlog one checkpoint drains
	// inline: this runs inside the coding client's Stop hook, and a
	// hub that fails mid-flush costs up to the client timeout PER LINE.
	// The remainder stays spooled for the next checkpoint or sync.
	hubFlushLimit = 100
)

// flushHubSpool drains one hub's spool after a successful contact, at
// most hubFlushLimit records per call.
func (c *Client) flushHubSpool(hub *HubConfig, spool string) {
	sweepOrphanedClaims(spool)
	claim := spool + fmt.Sprintf(".replay-%d", os.Getpid())
	if err := os.Rename(spool, claim); err != nil {
		return
	}
	defer os.Remove(claim)
	data, err := os.ReadFile(claim)
	if err != nil {
		return
	}
	n, attempts, kept, consecFails := 0, 0, 0, 0
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// Bound ATTEMPTS, not successes, and give up early on a hub that
		// died mid-flush: every further try would burn a full client
		// timeout inside the coding client's Stop hook.
		if attempts >= hubFlushLimit || consecFails >= 3 {
			c.spoolTo(spool, line)
			kept++
			continue
		}
		attempts++
		if err := c.hubPost(hub, line); err != nil {
			c.spoolTo(spool, line)
			consecFails++
			continue
		}
		consecFails = 0
		n++
	}
	if n > 0 {
		c.note("aimem: delivered %d queued checkpoint(s) to hub", n)
	}
	if kept > 0 {
		c.note("aimem: %d more remain spooled (bounded flush); they drain on coming checkpoints", kept)
	}
}

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
