package processctx

// The process pin: what an offer names for the project's process (aicrew's
// process pin and instruction digest), read from the hub with the
// checkout's own credential under its project grant. It needs no Git and
// no route to the process repository, and stores nothing.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"

	"aimem/internal/process"
)

// Pin is the project's current selection as an offer pins it.
type Pin struct {
	Project    string `json:"project"`
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Manifest   string `json:"manifest"`
	// SetDigest is the digest of the whole stored set (process.Digest).
	SetDigest string `json:"set_digest"`
	// InstructionDigest is "sha256:" and the hex SHA-256 of the manifest
	// file's exact bytes at the commit.
	InstructionDigest string `json:"instruction_digest"`
}

// ReadPin reads the pin of the project's current selection. The
// credential is selected as Load selects it: a required local credential
// that is missing or refused ends the read. The set is read from the hub
// and must digest to the selection's digest before the manifest is hashed.
func ReadPin(dir, root, projectID string) (*Pin, error) {
	r := &Result{Project: projectID}
	hub, client, ok := r.connect(dir, root)
	if !ok {
		return nil, pinError(r.Detail, r.Fix)
	}
	id := r.Project
	var sel struct {
		Current *process.Ref `json:"current"`
	}
	err := hubGetJSONClient(hub, client, "/v1/projects/"+url.PathEscape(id)+"/process", &sel)
	var refused *hubRefusal
	switch {
	case errors.As(err, &refused) && refused.denied():
		return nil, pinError("the hub refused to read project "+id+"'s process selection: "+err.Error(), credentialFix)
	case errors.As(err, &refused) && refused.status == 404:
		return nil, pinError("no process is selected for project "+id, "an admin selects one on the hub host: aimem process select <repo> <tag> -p "+id)
	case err != nil:
		return nil, pinError("cannot read project "+id+"'s process selection: "+err.Error(), "retry when the hub is reachable")
	case sel.Current == nil:
		return nil, pinError("no process is selected for project "+id, "an admin selects one on the hub host: aimem process select <repo> <tag> -p "+id)
	}
	ref := *sel.Current
	if err := ref.Validate(); err != nil {
		return nil, pinError("the hub's process selection for project "+id+" is malformed: "+err.Error(), "")
	}
	if ref.Digest == "" {
		return nil, pinError("project "+id+"'s selection predates stored sets, so the hub cannot vouch for its manifest", "an admin re-selects it on the hub host (aimem process select …), which stores the set")
	}
	outcome, set, _, err := fetchHubSet(hub, client, id, &ref, false, false)
	switch outcome {
	case hubSetStored:
	case hubSetDenied:
		return nil, pinError("the hub refused to serve project "+id+"'s process set: "+err.Error(), credentialFix)
	case hubSetUnsupported:
		return nil, pinError("the hub serves no stored set for project "+id+"'s selection: "+err.Error(), "an admin re-selects it on the hub host (aimem process select …), which stores the set; a hub that predates stored sets is upgraded first")
	default:
		return nil, pinError("cannot read project "+id+"'s process set: "+err.Error(), "")
	}
	manifest, ok := set.Files[ref.Manifest]
	if !ok {
		return nil, pinError("project "+id+"'s process set has no manifest at "+ref.Manifest, "")
	}
	sum := sha256.Sum256(manifest)
	return &Pin{Project: id, Repository: ref.Repo, Commit: ref.Commit, Manifest: ref.Manifest,
		SetDigest: ref.Digest, InstructionDigest: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

func pinError(detail, fix string) error {
	if fix == "" {
		return errors.New(detail)
	}
	return fmt.Errorf("%s; %s", detail, fix)
}
