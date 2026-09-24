package mcp

// team_accept from a checkout records, before it is sent, the exact process
// version and role guidance the attempt is accepted under
// (docs/DESIGN-portable-team-context.md, decision 6): the attempt's rules
// for as long as it is reserved, recovered by team_setup and team_continue
// even after the project selects a newer version. The record is trusted
// local state, written only here; the hub keeps no such field.
//
// The accept is refused locally, with nothing sent, when the version cannot
// be recorded: no saved membership for the session in the request, a
// project process that is not ready (a worker that is not ready accepts
// nothing), or a state file that cannot be written. A retry for the same
// attempt, with the same or a new key, after an uncertain reply or a
// restart, reuses the recorded version and never reads the current
// selection again, so a newer selection is never bound to an attempt
// accepted under an older one.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"aimem/internal/processctx"
	"aimem/internal/server"
	"aimem/internal/teamguide"
	"aimem/internal/teamstate"
)

// acceptWithRecord sends team_accept for the checkout at dir (state root
// root) after recording the attempt's process version.
func acceptWithRecord(ctx context.Context, call TaskCallFunc, defaultProject, dir, root string, raw json.RawMessage) (string, error) {
	tool, _ := teamWorkToolByName("team_accept")
	method, path, headers, body, err := buildTeamWorkRequest(defaultProject, tool, raw)
	if err != nil {
		return "", err
	}
	var a struct {
		Project    string `json:"project"`
		SessionID  string `json:"session_id"`
		Generation int64  `json:"generation"`
		Attempt    string `json:"attempt"`
		Key        string `json:"idempotency_key"`
	}
	json.Unmarshal(raw, &a)
	if a.Project == "" {
		a.Project = defaultProject
	}
	refuse := func(why string) (string, error) {
		return "", errors.New("team_accept refused locally, nothing was sent: " + why)
	}
	repo, err := teamstate.Canonical(dir)
	if err != nil {
		return refuse(err.Error())
	}
	statePath := teamstate.Path(root, repo)
	st, err := teamstate.Load(statePath)
	switch {
	case err != nil:
		return refuse("this checkout's team state is unreadable (" + err.Error() + "), so the process version cannot be recorded")
	case st == nil || st.SessionID == "" || st.SessionID != a.SessionID:
		return refuse("this checkout holds no saved membership for session " + a.SessionID + "; accept from the checkout whose team_setup joined that session, so the process version the attempt is accepted under can be recorded")
	case a.Project != "" && st.Project != a.Project:
		return refuse("the request names project " + a.Project + ", but this checkout's membership is in " + st.Project)
	}
	prev := st.Accepted
	created := false
	if prev == nil || prev.Attempt != a.Attempt || prev.SessionID != a.SessionID {
		r := processctx.Load(dir, root, st.Project)
		if r.State != processctx.Ready {
			return refuse(fmt.Sprintf("the project process is %s (%s); a worker that is not ready accepts nothing: run team_continue", r.State, r.Detail))
		}
		u, err := teamguide.Embedded()
		if err != nil {
			return refuse("this binary's team guidance is invalid: " + err.Error())
		}
		version := server.Version
		if version == "" {
			version = "dev"
		}
		sum := sha256.Sum256([]byte(r.Unit))
		st.Accepted = &teamstate.AcceptedAttempt{Attempt: a.Attempt, SessionID: a.SessionID, Generation: a.Generation, Key: a.Key,
			Repo: r.Ref.Repo, Commit: r.Ref.Commit, Manifest: r.Ref.Manifest, ProcessDigest: "sha256:" + hex.EncodeToString(sum[:]),
			RoleDigest: u.Digest, RoleVersion: version, RecordedAt: time.Now().UTC().Format(time.RFC3339)}
		if err := teamstate.Save(statePath, st); err != nil {
			return refuse("the process version could not be recorded (" + err.Error() + ")")
		}
		created = true
	}
	rec := st.Accepted
	status, resp, err := call(ctx, method, path, headers, body)
	if err != nil {
		return "", fmt.Errorf("%w (the accept's outcome is unknown; the process version recorded for attempt %s, commit %s, is kept; retry with the same idempotency_key)", err, a.Attempt, rec.Commit)
	}
	var res struct {
		Version    int    `json:"protocol_version"`
		Error      string `json:"error"`
		Assignment struct {
			State string `json:"state"`
		} `json:"assignment"`
	}
	if json.Unmarshal(resp, &res) != nil {
		return "", fmt.Errorf("hub team protocol unavailable or malformed (the accept's outcome is unknown; the process version recorded for attempt %s is kept)", a.Attempt)
	}
	if status/100 != 2 {
		if created {
			// A definite refusal: nothing was accepted, so the record this
			// call made is withdrawn and the previous one, if any, stands.
			if cur, err := teamstate.Load(statePath); err == nil && cur != nil {
				cur.Accepted = prev
				teamstate.Save(statePath, cur)
			}
		}
		return "", fmt.Errorf("team request HTTP %d: %s", status, res.Error)
	}
	if res.Version != 1 {
		return "", errors.New("unsupported team protocol; upgrade compatible client/hub, do not emulate with task writes")
	}
	if res.Assignment.State == "RUNNING" {
		if cur, err := teamstate.Load(statePath); err == nil && cur != nil && cur.Accepted != nil && cur.Accepted.Attempt == a.Attempt {
			cur.Accepted.Confirmed = true
			teamstate.Save(statePath, cur)
		}
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, resp, "", "  "); err != nil {
		return "", err
	}
	return pretty.String() + fmt.Sprintf("\nThis attempt's process version is recorded on this checkout: commit %s (manifest %s); team_setup, team_continue and process_context deliver it for as long as the attempt is reserved, even after the project selects a newer one.", rec.Commit, rec.Manifest), nil
}
