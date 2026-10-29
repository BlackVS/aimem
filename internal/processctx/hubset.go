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
	"aimem/internal/teamsession"
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
	hubSetNotModified                      // the cached set is the selection's, under a current grant (304)
)

// hubSet reads the set for ref from the hub and stores it under its
// digest, which is the selection's: the answer must state it and the
// files must digest to it. byVersion asks for an earlier selection by
// commit and manifest rather than the current one. cached says this
// machine holds the set already: the read is then conditional on the
// digest, and the hub answers 304 after its grant check, without the
// files.
func hubSet(hub *adapter.HubConfig, client *http.Client, root, project string, ref *process.Ref, byVersion, cached bool) (hubSetOutcome, error) {
	outcome, set, name, err := fetchHubSet(hub, client, project, ref, byVersion, cached)
	if outcome != hubSetStored {
		return outcome, err
	}
	if _, err := process.StoreSet(root, set, ref.Digest); err != nil {
		return hubSetFailed, fmt.Errorf("the hub's set does not validate: %w", err)
	}
	if ref.Name == "" {
		ref.Name = name
	}
	return hubSetStored, nil
}

// fetchHubSet reads the set for ref from the hub and verifies it against
// the selection, storing nothing: hubSetStored with the set and the name
// the hub gives it, or the outcome that says why not.
func fetchHubSet(hub *adapter.HubConfig, client *http.Client, project string, ref *process.Ref, byVersion, cached bool) (hubSetOutcome, *process.Bundle, string, error) {
	fail := func(o hubSetOutcome, err error) (hubSetOutcome, *process.Bundle, string, error) {
		return o, nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), setReadTimeout)
	defer cancel()
	u := strings.TrimRight(hub.URL, "/") + "/v1/projects/" + url.PathEscape(project) + "/process/set"
	if byVersion {
		u += "?commit=" + url.QueryEscape(ref.Commit) + "&manifest=" + url.QueryEscape(ref.Manifest)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fail(hubSetFailed, err)
	}
	req.Header.Set("Authorization", "Bearer "+hub.Token)
	if cached {
		req.Header.Set("If-None-Match", `"`+ref.Digest+`"`)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail(hubSetFailed, fmt.Errorf("hub unreachable: %w", err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, setBodyMax+1))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// A refusal stands whatever its body: one cut short or too large
		// loses only the reason, never the denial.
		return fail(hubSetDenied, errors.New(hubErrorText(raw, resp.StatusCode)))
	}
	if err != nil {
		return fail(hubSetFailed, fmt.Errorf("reading the set from the hub: %w", err))
	}
	if len(raw) > setBodyMax {
		return fail(hubSetFailed, fmt.Errorf("the hub's answer is larger than %d bytes", setBodyMax))
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		if !cached {
			return fail(hubSetFailed, errors.New("the hub answered 304 to a read that was not conditional"))
		}
		return fail(hubSetNotModified, nil)
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return fail(hubSetUnsupported, errors.New(hubErrorText(raw, resp.StatusCode)))
	default:
		return fail(hubSetFailed, errors.New(hubErrorText(raw, resp.StatusCode)))
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
		return fail(hubSetFailed, fmt.Errorf("the hub's answer is not a complete set: %w", err))
	}
	if body.Selection.Commit != ref.Commit || body.Selection.Manifest != ref.Manifest {
		return fail(hubSetFailed, fmt.Errorf("the hub answered for %s (manifest %s), not the selection %s (manifest %s)", short(body.Selection.Commit), body.Selection.Manifest, short(ref.Commit), ref.Manifest))
	}
	want := ref.Digest
	if body.Selection.Digest != want {
		return fail(hubSetFailed, fmt.Errorf("the hub's set is %s, not the selection's %s", short(body.Selection.Digest), short(want)))
	}
	if got := body.Set.Digest(); got != want {
		return fail(hubSetFailed, fmt.Errorf("digest mismatch: the files digest to %s, the selection says %s; nothing cached", short(got), short(want)))
	}
	return hubSetStored, &body.Set, body.Selection.Name, nil
}

func hubErrorText(raw []byte, status int) string {
	if r := teamsession.ParseRefusal(status, raw); r != nil {
		return r.Error()
	}
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
