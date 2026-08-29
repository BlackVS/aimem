package adapter

// Shared-document publishing (docs/DESIGN-shared-docs.md): after each
// checkpoint's event push, bound files whose content hash changed are
// PUT to the project's hub with compare-and-swap. The doc body is never
// embedded in the event payload — POST /v1/events is idempotent by
// contract and a CAS write is not — and doc publishes never enter the
// event spool: a failed or conflicted publish leaves the local hash
// stale, so the very next checkpoint retries it for free.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"aimem/internal/ident"
	"aimem/internal/redact"
	"aimem/internal/store"
)

// docState is one doc's local bookkeeping: the last revision and content
// hash this machine pushed or pulled. It lives in a sidecar JSON under
// <state-root>/docsync/ rather than the project DB's meta — the submit
// path is a short-lived CLI process that talks to the service over a
// socket, and giving it (or the token-gated meta API) DB write access for
// two bookkeeping fields would be a bigger surface than a file.
type docState struct {
	Rev  int64  `json:"rev"`
	Hash string `json:"hash"`
}

func docSyncPath(root, projectID string) string {
	return filepath.Join(root, "docsync", projectID+".json")
}

func loadDocSync(root, projectID string) map[string]docState {
	out := map[string]docState{}
	raw, err := os.ReadFile(docSyncPath(root, projectID))
	if err == nil {
		json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &out)
	}
	return out
}

func saveDocSync(root, projectID string, m map[string]docState) {
	p := docSyncPath(root, projectID)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

func hashBody(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DocBodyHash is the content hash the sidecar bookkeeping keys on,
// exported for callers (MCP update_doc) that record a push themselves.
func DocBodyHash(b []byte) string { return hashBody(b) }

// DocPushResult reports one bound doc's publish outcome (for `aimem docs
// push` output; the checkpoint path prints warnings instead).
type DocPushResult struct {
	Name string
	Rev  int64
	Err  error
}

// PublishDocs pushes every bound file whose hash differs from the last
// published one. Failures never block anything: capture must not fail
// because a document diverged.
func (c *Client) PublishDocs(projectDir, projectID, hubName, by string) []DocPushResult {
	_, hub := ResolveHub(c.root, hubName)
	if hub == nil {
		return nil
	}
	paths := ident.ProjectDocs(projectDir)
	if len(paths) == 0 {
		return nil
	}
	state := loadDocSync(c.root, projectID)
	var out []DocPushResult
	changed := false
	seen := map[string]string{} // doc name -> first bound path
	for _, rel := range paths {
		docName := ident.DocName(rel)
		// Two bound files with one base name would publish as the same
		// document and fight each other on every checkpoint — the first
		// binding wins (the default handoff, then declared order) and the
		// later one refuses loudly until renamed or unbound.
		if first, dup := seen[docName]; dup {
			out = append(out, DocPushResult{Name: docName,
				Err: fmt.Errorf("%s and %s both bind document name %q; rename one or remove a binding", first, rel, docName)})
			continue
		}
		seen[docName] = rel
		body, err := os.ReadFile(filepath.Join(projectDir, filepath.FromSlash(rel)))
		if err != nil {
			continue // an unbound or deleted file is not an error; `docs rm` retires deliberately
		}
		if len(body) > store.MaxDocBytes {
			out = append(out, DocPushResult{Name: docName,
				Err: fmt.Errorf("%s is %d bytes (limit %d); not published", rel, len(body), store.MaxDocBytes)})
			continue
		}
		// Docs publish AS WRITTEN — the journal's redact-on-write does not
		// run here, so scan instead: refuse the unambiguous secret shapes
		// (the hub refuses them too; failing here names the file), warn
		// the softer ones. A refused doc retries (and re-warns) on every
		// checkpoint until the secret is removed or the file opted out.
		warn, refuse := redact.ScanAuthored(string(body))
		if len(refuse) > 0 {
			out = append(out, DocPushResult{Name: docName,
				Err: fmt.Errorf("%s contains secret-shaped content (%s); not published — remove it or opt the file out via .aimem.json \"docs\"", rel, strings.Join(refuse, ", "))})
			continue
		}
		if len(warn) > 0 {
			c.note("aimem: shared doc %s has secret-shaped content (%s) — it publishes as written to every machine on the project",
				docName, strings.Join(warn, ", "))
		}
		h := hashBody(body)
		st := state[docName]
		if st.Hash == h {
			continue // unchanged since last publish: the normal, free case
		}
		doc, err := c.PutHubDoc(hub, projectID, docName, string(body), by, st.Rev)
		if err != nil {
			out = append(out, DocPushResult{Name: docName, Err: err})
			continue
		}
		state[docName] = docState{Rev: doc.Rev, Hash: h}
		changed = true
		out = append(out, DocPushResult{Name: docName, Rev: doc.Rev})
	}
	if changed {
		saveDocSync(c.root, projectID, state)
	}
	return out
}

// publishDocsQuiet is the checkpoint-path wrapper: outcomes go to stderr
// where the agent sees them, and nothing propagates.
func (c *Client) publishDocsQuiet(p *Payload) {
	if p.ProjectDir == "" {
		return
	}
	hubName := ""
	if p.Hub != nil {
		hubName = *p.Hub
	}
	host, _ := os.Hostname()
	for _, r := range c.PublishDocs(p.ProjectDir, p.ProjectID, hubName, host+"/"+p.Event.Client) {
		if r.Err != nil {
			c.note("aimem: shared doc %s not published: %v", r.Name, r.Err)
		}
	}
}

// HubDoc mirrors the hub's document representation (a subset of
// store.Doc plus the conflict fields).
type HubDoc struct {
	Name          string `json:"name"`
	Body          string `json:"body"`
	Rev           int64  `json:"rev"`
	UpdatedAt     string `json:"updated_at"`
	UpdatedBy     string `json:"updated_by"`
	Deleted       bool   `json:"deleted"`
	BodyTruncated bool   `json:"body_truncated"`
}

// DocConflictError is the client-side CAS refusal, carrying the hub's
// current document so the caller (or the agent) can merge.
type DocConflictError struct {
	Doc HubDoc
}

func (e *DocConflictError) Error() string {
	if e.Doc.Deleted {
		return fmt.Sprintf("deleted on the hub at rev %d by %s (republish deliberately with that base rev)",
			e.Doc.Rev, e.Doc.UpdatedBy)
	}
	return fmt.Sprintf("diverged on the hub: rev %d by %s at %s — pull, merge, and push again",
		e.Doc.Rev, e.Doc.UpdatedBy, e.Doc.UpdatedAt)
}

func hubDocURL(hub *HubConfig, projectID, name, suffix string) string {
	return strings.TrimRight(hub.URL, "/") + "/v1/projects/" + url.PathEscape(projectID) +
		"/docs" + suffix + url.PathEscape(name)
}

// GetHubDoc fetches the current document (or, with rev > 0, a retained
// revision) from a hub.
func (c *Client) GetHubDoc(hub *HubConfig, projectID, name string, rev int64) (HubDoc, error) {
	u := hubDocURL(hub, projectID, name, "/")
	if rev > 0 {
		u += fmt.Sprintf("?rev=%d", rev)
	}
	var doc HubDoc
	return doc, c.hubDocDo(hub, "GET", u, nil, &doc)
}

// ListHubDocs enumerates a project's documents on a hub.
func (c *Client) ListHubDocs(hub *HubConfig, projectID string) ([]HubDoc, error) {
	var res struct {
		Docs []HubDoc `json:"docs"`
	}
	u := strings.TrimRight(hub.URL, "/") + "/v1/projects/" + url.PathEscape(projectID) + "/docs"
	return res.Docs, c.hubDocDo(hub, "GET", u, nil, &res)
}

// PutHubDoc is the CAS write. A *DocConflictError result carries the
// hub's current document.
func (c *Client) PutHubDoc(hub *HubConfig, projectID, name, body, by string, baseRev int64) (HubDoc, error) {
	payload, _ := json.Marshal(map[string]any{"body": body, "base_rev": baseRev, "updated_by": by})
	var res HubDoc
	err := c.hubDocDo(hub, "PUT", hubDocURL(hub, projectID, name, "/"), payload, &res)
	res.Name = name
	return res, err
}

// DeleteHubDoc writes the tombstone.
func (c *Client) DeleteHubDoc(hub *HubConfig, projectID, name, by string, baseRev int64) error {
	u := hubDocURL(hub, projectID, name, "/") + fmt.Sprintf("?base_rev=%d&by=%s", baseRev, url.QueryEscape(by))
	return c.hubDocDo(hub, "DELETE", u, nil, &struct{}{})
}

func (c *Client) hubDocDo(hub *HubConfig, method, u string, body []byte, into any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+hub.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hub.HTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, store.MaxDocBytes*2))
	if resp.StatusCode == http.StatusConflict {
		var doc HubDoc
		json.Unmarshal(raw, &doc)
		return &DocConflictError{Doc: doc}
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error != "" {
			return fmt.Errorf("hub: %s", e.Error)
		}
		return fmt.Errorf("hub returned HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, into)
}

// DocSyncRev returns the last revision this machine recorded for a doc.
func (c *Client) DocSyncRev(projectID, name string) int64 {
	return loadDocSync(c.root, projectID)[name].Rev
}

// DocSyncHash returns the last content hash this machine recorded.
func (c *Client) DocSyncHash(projectID, name string) string {
	return loadDocSync(c.root, projectID)[name].Hash
}

// DocSyncRecord updates the sidecar after a deliberate push or pull.
func (c *Client) DocSyncRecord(projectID, name string, rev int64, hash string) {
	st := loadDocSync(c.root, projectID)
	st[name] = docState{Rev: rev, Hash: hash}
	saveDocSync(c.root, projectID, st)
}

// HubDocGetJSON fetches an arbitrary doc-scoped GET (e.g. "/log").
func (c *Client) HubDocGetJSON(hub *HubConfig, projectID, name, suffix string, into any) error {
	u := hubDocURL(hub, projectID, name, "/") + suffix
	return c.hubDocDo(hub, "GET", u, nil, into)
}
