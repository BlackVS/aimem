// Package teamstate keeps the nonsecret record of a checkout's team
// membership under the state root: the handle a restarted agent continues
// with, the declared profile, and the retry keys of unconfirmed requests.
// It is bound to the exact checkout, project, hub, URL and credential
// (token ID), never holds a secret, and is shared by the CLI (`aimem teams
// setup`, `continue`, `leave`) and the stdio MCP facade (team_leave).
package teamstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"aimem/internal/store"
)

// State is the record. Version 1.
type State struct {
	Version               int               `json:"version"`
	Repo                  string            `json:"repo"`
	Project               string            `json:"project"`
	HubName               string            `json:"hub"`
	HubURL                string            `json:"url"`
	TokenID               string            `json:"token_id"`
	User                  string            `json:"user"`
	Team                  string            `json:"team"`
	TeamID                string            `json:"team_id,omitempty"`
	Role                  string            `json:"role"`
	Profile               store.TeamProfile `json:"profile"`
	JoinKey               string            `json:"join_key,omitempty"`
	ResumeKey             string            `json:"resume_key,omitempty"`
	SessionID             string            `json:"session_id,omitempty"`
	Generation            int64             `json:"generation,omitempty"`
	CoordinatorGeneration int64             `json:"coordinator_generation,omitempty"`
	ProfileRevision       int64             `json:"profile_revision,omitempty"`
	BaseCommit            string            `json:"base_commit,omitempty"` // HEAD when the membership was last verified
	JoinedAt              string            `json:"joined_at,omitempty"`
	VerifiedAt            string            `json:"verified_at,omitempty"`
	// RoleDigest and RoleVersion are the team guidance last delivered to this
	// membership, so a later run can report that it changed.
	RoleDigest  string `json:"role_digest,omitempty"`
	RoleVersion string `json:"role_version,omitempty"`
	// PendingWork is an attempt write sent (or about to be sent) whose
	// outcome this checkout has not seen; a replay repeats it exactly.
	PendingWork *PendingWork `json:"pending_work,omitempty"`
}

// PendingWork is one not-ready attempt write: its operation, attempt,
// session generation, retry key and the exact request content, so an
// uncertain outcome is retried as the identical request.
type PendingWork struct {
	Op               string `json:"op"` // decline or block
	Attempt          string `json:"attempt"`
	Generation       int64  `json:"generation"`
	Key              string `json:"key"`
	Reason           string `json:"reason"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
}

// Canonical resolves dir the way the credential store does, so the state
// and the credential of one checkout share one key.
func Canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if canon, err := filepath.EvalSymlinks(abs); err == nil {
		return canon, nil
	}
	return abs, nil
}

// Path is the state file of the checkout at the canonical path repo.
func Path(root, repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(root, "team-sessions", hex.EncodeToString(sum[:])+".json")
}

// ErrUnreadable says a file exists at the path but is not a version-1 state.
var ErrUnreadable = errors.New("team session state file is unreadable")

// Load returns the state at path, nil when there is none.
func Load(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if json.Unmarshal(raw, &st) != nil || st.Version != 1 {
		return nil, ErrUnreadable
	}
	return &st, nil
}

// Save writes the state atomically, readable by the owner only.
func Save(path string, st *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Clear removes the state; a missing file is not an error.
func Clear(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// NoteLeave clears the saved handle after an explicit leave of sessionID
// from the checkout at dir, so the next setup or continue knows the
// membership ended on purpose rather than finding a closed handle. Best
// effort: nothing here can fail the leave that already happened.
func NoteLeave(dir, root, sessionID string) {
	repo, err := Canonical(dir)
	if err != nil {
		return
	}
	path := Path(root, repo)
	st, err := Load(path)
	if err != nil || st == nil || st.SessionID != sessionID {
		return
	}
	Clear(path)
}
