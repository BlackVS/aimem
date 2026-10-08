package main

// `aimem identity peer provision`: one command registers an identity peer
// and writes its four credentials and the hub's ID into a directory aicrew
// reads them from (aicrew hub add --cred-dir). A rerun keeps what exists
// and issues only what is missing, so a provisioning cut short is finished
// by running it again.

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"aimem/internal/privatefile"
)

// provisionDefaultExpiry is the life of a provisioned credential when
// --expires is not given.
const provisionDefaultExpiry = "90d"

// provisionMaxLife is the hub's limit on a peer credential's life.
const provisionMaxLife = 366 * 24 * time.Hour

// provisionFiles are the credential files a provisioned peer gets, one per
// operation, under fixed names.
var provisionFiles = []struct{ operation, name string }{
	{"identity.redeem", "aimem-redeem.token"},
	{"reservation.read", "aimem-read.token"},
	{"team.register", "aimem-team-register.token"},
	{"team.read", "aimem-team-read.token"},
}

// provisionHubIDFile holds the hub's ID, one line, next to the credentials.
const provisionHubIDFile = "aimem-hub-id"

// identityPeerView is a registered peer as the hub lists it.
type identityPeerView struct {
	ServiceID     string `json:"service_id"`
	HubID         string `json:"hub_id"`
	Endpoint      string `json:"introspection_endpoint"`
	Disabled      bool   `json:"disabled"`
	Introspection bool   `json:"introspection_operational"`
	TLSTrust      struct {
		Mode  string `json:"mode"`
		Value string `json:"value"`
	} `json:"tls_trust"`
}

func (c *identityClient) peers() ([]identityPeerView, error) {
	var resp struct {
		Peers []identityPeerView `json:"peers"`
	}
	if err := c.call("GET", "/v1/identity/peers", nil, http.StatusOK, &resp); err != nil {
		return nil, err
	}
	return resp.Peers, nil
}

// peerTrust is the trust binding peer register sends for these flags.
func peerTrust(endpoint string, trustDNS bool, trustPin string) (map[string]string, error) {
	if !trustDNS {
		return map[string]string{"mode": "spki_sha256", "value": trustPin}, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("--endpoint must be the peer's https introspection URL")
	}
	return map[string]string{"mode": "ca_dns", "value": u.Hostname()}, nil
}

type provisionRequest struct {
	service, endpoint, dir, replace string
	trustDNS                        bool
	trustPin                        string
	expiry                          time.Time
}

// peerProvision registers the peer unless it exists with the same endpoint
// and trust, then issues a credential for every file in the directory that
// is missing or empty. Everything that can refuse is checked before the hub
// changes: the files, the peer record and the one-active-peer rule. A
// non-empty credential file is never overwritten.
func (c *identityClient) peerProvision(req provisionRequest, out io.Writer) error {
	trust, err := peerTrust(req.endpoint, req.trustDNS, req.trustPin)
	if err != nil {
		return err
	}
	if req.replace == req.service {
		return fmt.Errorf("--replace names the peer being provisioned; nothing changed")
	}
	if err := os.MkdirAll(req.dir, 0o700); err != nil {
		return fmt.Errorf("--output-dir %s cannot be created (%v); nothing changed", req.dir, err)
	}
	peers, err := c.peers()
	if err != nil {
		return fmt.Errorf("cannot list the hub's identity peers (%v); nothing changed", err)
	}
	var existing, old *identityPeerView
	var blocking []string
	for i := range peers {
		p := &peers[i]
		switch {
		case p.ServiceID == req.service:
			existing = p
		case p.ServiceID == req.replace:
			old = p
		}
		if p.ServiceID != req.service && p.ServiceID != req.replace && !p.Disabled {
			blocking = append(blocking, p.ServiceID)
		}
	}
	if existing != nil {
		if existing.Disabled {
			return fmt.Errorf("identity peer %s is registered but disabled; enable it (%s) or retire it (%s); nothing changed",
				req.service, c.command("peer", "enable", req.service), c.command("peer", "retire", req.service))
		}
		if !sameEndpoint(existing.Endpoint, req.endpoint) || existing.TLSTrust.Mode != trust["mode"] || existing.TLSTrust.Value != trust["value"] {
			return fmt.Errorf("identity peer %s is registered with endpoint %s and trust %s %s, not the ones given; nothing changed",
				req.service, existing.Endpoint, existing.TLSTrust.Mode, existing.TLSTrust.Value)
		}
	}
	// An old peer that is no longer registered (retired after an earlier
	// run) needs nothing; any other enabled peer is refused below.
	if req.replace != "" && old == nil {
		fmt.Fprintf(out, "--replace %s: no such identity peer is registered; nothing to disable\n", req.replace)
	}
	if len(blocking) > 0 {
		return fmt.Errorf("the hub allows one enabled identity peer and %s is enabled; name it with --replace to disable it in the same step; nothing changed",
			strings.Join(blocking, ", "))
	}
	// Every peer carries the hub's own ID, so any listed one tells which hub
	// this is before anything changes.
	knownHub := ""
	if len(peers) > 0 {
		knownHub = peers[0].HubID
	}
	hubIDPath := filepath.Join(req.dir, provisionHubIDFile)
	hubIDKept, err := checkHubIDFile(hubIDPath, knownHub)
	if err != nil {
		return err
	}

	// The files: present ones are kept, missing or empty ones are reserved
	// before anything is issued. A new peer starts with none present.
	var kept []string
	type pending struct {
		operation, path string
		sink            *secretOutput
	}
	var todo []pending
	abandon := func() {
		for _, p := range todo {
			p.sink.abandon()
		}
	}
	for _, f := range provisionFiles {
		path := filepath.Join(req.dir, f.name)
		fi, err := os.Lstat(path)
		switch {
		case err == nil && !fi.Mode().IsRegular():
			abandon()
			return fmt.Errorf("%s is not a regular file; nothing changed", path)
		case err == nil && fi.Size() > 0:
			if existing == nil {
				abandon()
				return fmt.Errorf("%s already holds a credential and is never overwritten, but %s is not registered; move the file away first; nothing changed", path, req.service)
			}
			kept = append(kept, f.name)
			continue
		case err == nil:
			if err := os.Remove(path); err != nil {
				abandon()
				return fmt.Errorf("%s is empty and cannot be replaced (%v); nothing changed", path, err)
			}
		case !os.IsNotExist(err):
			abandon()
			return fmt.Errorf("%s cannot be inspected (%v); nothing changed", path, err)
		}
		f2, err := reserveSecretFile("--output-dir", path)
		if err != nil {
			abandon()
			return err
		}
		todo = append(todo, pending{f.operation, path, &secretOutput{flag: "--output-dir", path: path, f: f2}})
	}

	// The hub changes from here on: the old peer steps aside, the new one
	// registers, and the old one is enabled again if that fails.
	if old != nil && !old.Disabled {
		if err := c.call("PUT", peerPath(req.replace), map[string]bool{"disabled": true}, http.StatusOK, nil); err != nil {
			abandon()
			return fmt.Errorf("cannot disable %s (%v); nothing changed", req.replace, err)
		}
		fmt.Fprintf(out, "identity peer %s disabled (replaced by %s)\n", req.replace, req.service)
	}
	if existing == nil {
		body := map[string]any{"service_id": req.service, "introspection_endpoint": req.endpoint, "tls_trust": trust}
		if err := c.call("POST", "/v1/identity/peers", body, http.StatusCreated, nil); err != nil {
			abandon()
			if old != nil && !old.Disabled {
				if rerr := c.call("PUT", peerPath(req.replace), map[string]bool{"disabled": false}, http.StatusOK, nil); rerr != nil {
					return fmt.Errorf("cannot register %s (%v), and enabling %s again FAILED (%v); enable it with: %s",
						req.service, err, req.replace, rerr, c.command("peer", "enable", req.replace))
				}
				return fmt.Errorf("cannot register %s (%v); %s is enabled again; nothing else changed", req.service, err, req.replace)
			}
			return fmt.Errorf("cannot register %s (%v); nothing changed", req.service, err)
		}
		fmt.Fprintf(out, "identity peer %s registered (endpoint trust %s %s)\n", req.service, trust["mode"], trust["value"])
	} else {
		fmt.Fprintf(out, "identity peer %s already registered with this endpoint and trust\n", req.service)
	}

	before, err := c.creds(req.service)
	if err != nil {
		abandon()
		return fmt.Errorf("cannot read the credentials of %s (%v); no credential was issued; run the same command again", req.service, err)
	}
	known := map[string]bool{}
	for _, cr := range before {
		known[cr.ID] = true
	}
	for i, p := range todo {
		cred, err := c.issueOne(req.service, p.operation, req.expiry, p.sink, known, out)
		if err != nil {
			for _, rest := range todo[i:] {
				rest.sink.abandon()
			}
			return fmt.Errorf("%s: %v; the files written so far are kept, and running the same command again issues only what is missing", p.path, err)
		}
		known[cred.ID] = true
		fmt.Fprintf(out, "issued %s credential %s into %s, expiring %s\n", cred.Operation, cred.ID, p.path, cred.ExpiresAt.Format(time.RFC3339))
	}
	// A kept file is not read; the hub's list says whether a credential of
	// its operation is still active.
	active := map[string]bool{}
	creds, credsErr := c.creds(req.service)
	now := time.Now()
	for _, cr := range creds {
		if cr.state(now) == "active" {
			active[cr.Operation] = true
		}
	}
	if credsErr != nil && len(kept) > 0 {
		fmt.Fprintf(out, "warning: the credentials of %s could not be read (%v); the kept files were not checked against the hub\n", req.service, credsErr)
	}
	for _, f := range provisionFiles {
		if !slices.Contains(kept, f.name) {
			continue
		}
		path := filepath.Join(req.dir, f.name)
		fmt.Fprintf(out, "kept %s (exists; nothing issued for it)\n", path)
		if credsErr == nil && !active[f.operation] {
			fmt.Fprintf(out, "warning: %s has no active %s credential on the hub; %s may hold an expired or revoked one: move it away and run this again\n",
				req.service, f.operation, path)
		}
	}
	hub := ""
	if peers, err := c.peers(); err == nil {
		for _, p := range peers {
			if p.ServiceID == req.service {
				hub = p.HubID
			}
		}
	}
	if hub == "" {
		return fmt.Errorf("identity peer %s is provisioned, but the hub ID could not be read back; see %s", req.service, c.command("peer", "list"))
	}
	fmt.Fprintf(out, "hub ID %s\n", hub)
	if hubIDKept {
		fmt.Fprintf(out, "%s already holds this hub ID\n", hubIDPath)
	} else {
		if err := writeHubIDFile(hubIDPath, hub); err != nil {
			return fmt.Errorf("identity peer %s is provisioned, but %v; running the same command again writes it", req.service, err)
		}
		fmt.Fprintf(out, "wrote the hub ID into %s\n", hubIDPath)
	}
	if len(todo) == 0 {
		fmt.Fprintf(out, "identity peer %s was already provisioned; nothing was issued\n", req.service)
	}
	return nil
}

// readHubIDFile reads the hub ID file: present reports whether it exists,
// and held is its trimmed content, so an empty or whitespace-only file
// holds nothing. The check and the write share this one definition.
func readHubIDFile(path string) (held string, present bool, err error) {
	fi, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("%s cannot be inspected (%v)", path, err)
	case !fi.Mode().IsRegular():
		return "", false, fmt.Errorf("%s is not a regular file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("%s cannot be read (%v)", path, err)
	}
	return strings.TrimSpace(string(b)), true, nil
}

// checkHubIDFile reports whether the hub ID file already holds this hub's
// ID, so it is kept. A missing file, or one that holds nothing, is written
// at the end. One holding another ID, or any ID while the hub has no peer
// to confirm it against, is refused.
func checkHubIDFile(path, knownHub string) (bool, error) {
	held, _, err := readHubIDFile(path)
	switch {
	case err != nil:
		return false, fmt.Errorf("%v; nothing changed", err)
	case held == "":
		return false, nil
	case knownHub == "":
		return false, fmt.Errorf("%s holds hub ID %s, but this hub has no identity peer to confirm its ID against; move the file away first; nothing changed", path, held)
	case held != knownHub:
		return false, fmt.Errorf("%s holds hub ID %s, but this hub's ID is %s: the directory belongs to another hub; nothing changed", path, held, knownHub)
	}
	return true, nil
}

// writeHubIDFile writes the hub ID as one line into an owner-only file,
// replacing one that holds nothing.
func writeHubIDFile(path, hub string) error {
	held, present, err := readHubIDFile(path)
	switch {
	case err != nil:
		return err
	case held != "":
		return fmt.Errorf("%s holds hub ID %s and is never overwritten", path, held)
	case present:
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("the empty %s cannot be replaced (%v)", path, err)
		}
	}
	f, err := privatefile.Create(path)
	if err != nil {
		return fmt.Errorf("%s cannot be created (%v)", path, err)
	}
	_, werr := f.WriteString(hub + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return fmt.Errorf("%s cannot be written (%v)", path, werr)
	}
	return nil
}

// sameEndpoint compares a registered endpoint with a flag value the way the
// hub stores it (url.URL.String of the parsed value).
func sameEndpoint(registered, given string) bool {
	u, err := url.Parse(given)
	return err == nil && u.String() == registered
}
