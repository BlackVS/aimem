package processctx

// The hub-served set (docs/DESIGN-portable-team-context.md, increment 1d,
// the default path): a member reads the files of the selected process
// from the hub under its project grant, verifies their digest against the
// selection and caches them by that digest. Only a selection that carries
// a digest is read this way: the digest is the signal that the hub stores
// sets. Git on the member is the path for every other selection.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/process"
)

// setReadTimeout bounds the one read of a set from the hub; a cache miss
// pays it once per digest, as a Git fetch was paid once per commit.
const setReadTimeout = process.FetchTimeout

// setBodyMax bounds the hub's answer: the set's bytes base64-encoded in
// JSON, with room for the envelope.
const setBodyMax = 2 * process.MaxSetBytes

// hubSetOutcome classifies one attempt to read the set from the hub.
type hubSetOutcome int

const (
	hubSetStored      hubSetOutcome = iota // the set is verified and cached
	hubSetUnsupported                      // the hub has no such route or no stored set: use Git
	hubSetDenied                           // the hub refused the credential or the grant
	hubSetFailed                           // anything else; err says what
)

// hubSet reads the set for ref from the hub and stores it under its
// digest, which is the selection's: the answer must state it and the
// files must digest to it. byVersion asks for an earlier selection by
// commit and manifest rather than the current one.
func hubSet(hub *adapter.HubConfig, client *http.Client, root, project string, ref *process.Ref, byVersion bool) (hubSetOutcome, error) {
	ctx, cancel := context.WithTimeout(context.Background(), setReadTimeout)
	defer cancel()
	u := strings.TrimRight(hub.URL, "/") + "/v1/projects/" + url.PathEscape(project) + "/process/set"
	if byVersion {
		u += "?commit=" + url.QueryEscape(ref.Commit) + "&manifest=" + url.QueryEscape(ref.Manifest)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return hubSetFailed, err
	}
	req.Header.Set("Authorization", "Bearer "+hub.Token)
	resp, err := client.Do(req)
	if err != nil {
		return hubSetFailed, fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, setBodyMax+1))
	if err != nil {
		return hubSetFailed, fmt.Errorf("reading the set from the hub: %w", err)
	}
	if len(raw) > setBodyMax {
		return hubSetFailed, fmt.Errorf("the hub's answer is larger than %d bytes", setBodyMax)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return hubSetDenied, errors.New(hubErrorText(raw, resp.StatusCode))
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return hubSetUnsupported, errors.New(hubErrorText(raw, resp.StatusCode))
	default:
		return hubSetFailed, errors.New(hubErrorText(raw, resp.StatusCode))
	}
	var body struct {
		Selection struct {
			Commit   string `json:"commit"`
			Manifest string `json:"manifest"`
			Digest   string `json:"digest"`
			Name     string `json:"name"`
		} `json:"selection"`
		Set process.Bundle `json:"set"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		// A cut or malformed body is never cached and never a set.
		return hubSetFailed, fmt.Errorf("the hub's answer is not a complete set: %w", err)
	}
	if body.Selection.Commit != ref.Commit || body.Selection.Manifest != ref.Manifest {
		return hubSetFailed, fmt.Errorf("the hub answered for %s (manifest %s), not the selection %s (manifest %s)", short(body.Selection.Commit), body.Selection.Manifest, short(ref.Commit), ref.Manifest)
	}
	want := ref.Digest
	if body.Selection.Digest != want {
		return hubSetFailed, fmt.Errorf("the hub's set is %s, not the selection's %s", short(body.Selection.Digest), short(want))
	}
	if got := body.Set.Digest(); got != want {
		return hubSetFailed, fmt.Errorf("digest mismatch: the files digest to %s, the selection says %s; nothing cached", short(got), short(want))
	}
	if _, err := process.StoreSet(root, &body.Set, want); err != nil {
		return hubSetFailed, fmt.Errorf("the hub's set does not validate: %w", err)
	}
	if ref.Name == "" {
		ref.Name = body.Selection.Name
	}
	return hubSetStored, nil
}

func hubErrorText(raw []byte, status int) string {
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(raw, &e)
	if e.Error != "" {
		return e.Error
	}
	return fmt.Sprintf("hub returned HTTP %d", status)
}

// setFetchDeadline is the overall bound of finish's content step: one hub
// read, then at most one Git fetch for an older hub.
const setFetchDeadline = process.FetchTimeout + time.Second
