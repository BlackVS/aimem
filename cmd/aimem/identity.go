package main

// `aimem identity`: the operator CLI for identity.v1 peers and peer
// credentials (docs/DESIGN-AIFORGE-IDENTITY-WIRE.md). Unlike `aimem access`,
// it cannot use the local unix socket: the identity routes answer only over
// TLS terminated by the hub. So it talks to the hub's TLS listener, verifies
// its certificate (system roots, a CA file or an SPKI pin, never nothing),
// authenticates with a hub-admin bearer read from a protected file, and
// writes an issued peer bearer only to a new protected file. Neither secret
// is ever printed.

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const identityUsage = `usage: aimem identity peer list                            [hub flags]
       aimem identity peer register SERVICE --endpoint URL
                                 (--peer-trust-dns | --peer-trust-pin sha256-BASE64) [hub flags]
       aimem identity peer enable SERVICE                    [hub flags]
       aimem identity peer disable SERVICE                   [hub flags]
       aimem identity cred list SERVICE                      [hub flags]
       aimem identity cred issue SERVICE --expires 90d|RFC3339 --secret-file PATH  [hub flags]
       aimem identity cred rotate SERVICE --expires 90d|RFC3339 --secret-file PATH [hub flags]
       aimem identity cred revoke SERVICE CREDENTIAL_ID      [hub flags]

Manage the aicrew identity peer and its credentials through the hub's TLS
listener. The local socket is not used: identity routes require TLS
terminated by the hub.

Hub flags (after the positional arguments):
  --hub https://HOST:PORT    the hub's TLS listener (required; http is refused)
  --admin-token-file PATH    a hub-admin bearer on one line, in a file only you
                             can read (required; there is no other source)
  --hub-ca-file PATH         trust only this CA bundle for the hub
  --hub-pin sha256-BASE64    trust only a hub certificate with this SPKI SHA-256
  Without --hub-ca-file or --hub-pin the system roots are used. There is no
  insecure mode.

--peer-trust-dns and --peer-trust-pin are the registered peer endpoint's
trust binding, stored for introspection; they are separate from how this
command trusts the hub.

cred issue and cred rotate write the new bearer once to --secret-file, a new
file only you can read, created before anything is issued; it is never
printed. Deliver it to aicrew's protected storage, then delete the file.
cred rotate issues the second credential only; after aicrew has switched to
it, revoke the old one explicitly with cred revoke.`

type identityCred struct {
	ID        string    `json:"id"`
	ServiceID string    `json:"service_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}

func (c identityCred) state(now time.Time) string {
	switch {
	case c.Revoked:
		return "revoked"
	case !c.ExpiresAt.After(now):
		return "expired"
	}
	return "active"
}

// identityClient calls the hub-admin identity routes. The token is only ever
// placed in the Authorization header.
type identityClient struct {
	base  string
	token string
	http  *http.Client
	// hubArgs are the non-secret connection options (the hub URL, the
	// token file's path and the trust option) repeated in every command
	// the CLI tells the operator to run next.
	hubArgs []string
}

// command renders a complete, runnable aimem identity invocation with this
// run's connection options. It names the admin token file, never the token.
func (c *identityClient) command(args ...string) string {
	parts := []string{"aimem", "identity"}
	for _, a := range append(args, c.hubArgs...) {
		parts = append(parts, shellArg(a))
	}
	return strings.Join(parts, " ")
}

// shellArg quotes an argument for POSIX shells, cmd and PowerShell alike:
// plain words stay bare, anything else goes in double quotes, which none of
// the three treats a backslash inside of as an escape for ordinary paths.
func shellArg(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@%+=,\\-") == "" {
		return s
	}
	return `"` + s + `"`
}

// deliverSecret writes the issued bearer to the reserved secret file. A test
// replaces it to simulate a delivery failure.
var deliverSecret = func(f *os.File, secret string) error {
	if _, err := f.WriteString(secret + "\n"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

func identityCmd(args []string) error { return runIdentity(args, os.Stdout) }

func runIdentity(args []string, out io.Writer) error {
	usage := fmt.Errorf("%s", identityUsage)
	if len(args) < 2 {
		return usage
	}
	noun, verb, rest := args[0], args[1], args[2:]
	positional := map[string]int{
		"peer list": 0, "peer register": 1, "peer enable": 1, "peer disable": 1,
		"cred list": 1, "cred issue": 1, "cred rotate": 1, "cred revoke": 2,
	}
	n, ok := positional[noun+" "+verb]
	if !ok || len(rest) < n {
		return usage
	}
	pos, flags := rest[:n], rest[n:]
	for _, p := range pos {
		if strings.HasPrefix(p, "-") {
			return usage
		}
	}
	fs := flag.NewFlagSet("aimem identity "+noun+" "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hub := fs.String("hub", "", "")
	tokenFile := fs.String("admin-token-file", "", "")
	caFile := fs.String("hub-ca-file", "", "")
	pin := fs.String("hub-pin", "", "")
	endpoint := fs.String("endpoint", "", "")
	trustDNS := fs.Bool("peer-trust-dns", false, "")
	trustPin := fs.String("peer-trust-pin", "", "")
	expires := fs.String("expires", "", "")
	secretFile := fs.String("secret-file", "", "")
	if err := fs.Parse(flags); err != nil {
		return fmt.Errorf("%v\n\n%s", err, identityUsage)
	}
	if fs.NArg() != 0 {
		return usage
	}
	// Validate the command's own arguments before touching the network, the
	// token file or the secret file.
	var expiry time.Time
	switch noun + " " + verb {
	case "peer register":
		if *endpoint == "" || *trustDNS == (*trustPin != "") {
			return fmt.Errorf("peer register needs --endpoint and exactly one of --peer-trust-dns or --peer-trust-pin")
		}
	case "cred issue", "cred rotate":
		if *secretFile == "" || *expires == "" {
			return fmt.Errorf("%s needs --expires and --secret-file", "cred "+verb)
		}
		var err error
		if expiry, err = parseIdentityExpiry(*expires, time.Now()); err != nil {
			return err
		}
	}
	c, err := newIdentityClient(*hub, *tokenFile, *caFile, *pin)
	if err != nil {
		return err
	}
	switch noun + " " + verb {
	case "peer list":
		return c.peerList(out)
	case "peer register":
		return c.peerRegister(pos[0], *endpoint, *trustDNS, *trustPin, out)
	case "peer enable", "peer disable":
		return c.peerSetDisabled(pos[0], verb == "disable", out)
	case "cred list":
		return c.credList(pos[0], out)
	case "cred issue":
		return c.credIssue(pos[0], expiry, *secretFile, false, out)
	case "cred rotate":
		return c.credIssue(pos[0], expiry, *secretFile, true, out)
	case "cred revoke":
		if err := c.credRevoke(pos[0], pos[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "credential %s of %s revoked\n", pos[1], pos[0])
		return nil
	}
	return usage
}

// parseIdentityExpiry accepts "<days>d" or an RFC 3339 time.
func parseIdentityExpiry(s string, now time.Time) (time.Time, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return time.Time{}, fmt.Errorf("--expires %q: use a positive number of days such as 90d, or an RFC 3339 time", s)
		}
		return now.Add(time.Duration(n) * 24 * time.Hour), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--expires %q: use a positive number of days such as 90d, or an RFC 3339 time", s)
	}
	return t, nil
}

func newIdentityClient(hub, tokenFile, caFile, pin string) (*identityClient, error) {
	u, err := url.Parse(hub)
	if hub == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("--hub must be the hub's TLS listener as https://HOST:PORT (plain http is refused)")
	}
	if tokenFile == "" {
		return nil, fmt.Errorf("--admin-token-file is required: a hub-admin bearer in a file only you can read")
	}
	if err := checkPrivateFile(tokenFile); err != nil {
		return nil, fmt.Errorf("--admin-token-file: %w", err)
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("--admin-token-file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, fmt.Errorf("--admin-token-file must hold exactly one bearer on one line")
	}
	client, err := identityHTTPClient(caFile, pin)
	if err != nil {
		return nil, err
	}
	hubArgs := []string{"--hub", hub, "--admin-token-file", tokenFile}
	switch {
	case caFile != "":
		hubArgs = append(hubArgs, "--hub-ca-file", caFile)
	case pin != "":
		hubArgs = append(hubArgs, "--hub-pin", pin)
	}
	return &identityClient{base: "https://" + u.Host, token: token, http: client, hubArgs: hubArgs}, nil
}

// identityHTTPClient verifies the hub's certificate against the system
// roots, a CA bundle, or an SPKI pin. It never follows redirects (the
// bearer must reach only the named hub) and never uses a proxy.
func identityHTTPClient(caFile, pin string) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case caFile != "" && pin != "":
		return nil, fmt.Errorf("use either --hub-ca-file or --hub-pin, not both")
	case caFile != "":
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("--hub-ca-file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--hub-ca-file %s holds no PEM certificate", caFile)
		}
		cfg.RootCAs = pool
	case pin != "":
		b64, ok := strings.CutPrefix(pin, "sha256-")
		want, err := base64.StdEncoding.DecodeString(b64)
		if !ok || err != nil || len(want) != sha256.Size {
			return nil, fmt.Errorf("--hub-pin must be sha256- followed by the base64 SHA-256 of the hub certificate's public key")
		}
		// The chain check is replaced by the pin, never dropped: the
		// connection is refused unless the leaf's SPKI hashes to the pin.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the hub presented no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(sum[:], want) != 1 {
				return errors.New("the hub certificate does not match --hub-pin")
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: cfg, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// errIdentityTransport marks a request whose outcome the hub never reported.
var errIdentityTransport = errors.New("no answer from the hub")

func (c *identityClient) do(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", errIdentityTransport, c.scrub(err.Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%w: reading the answer: %v", errIdentityTransport, c.scrub(err.Error()))
	}
	return resp.StatusCode, data, nil
}

// scrub removes the admin bearer from any text shown to the operator.
func (c *identityClient) scrub(s string) string {
	if c.token != "" {
		s = strings.ReplaceAll(s, c.token, "[admin token]")
	}
	return s
}

// hubRefusal renders a hub error body (operator {"error"} or the identity
// refusal envelope) without echoing anything secret.
func (c *identityClient) hubRefusal(status int, data []byte) error {
	var body struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &body) == nil {
		switch {
		case body.Code != "":
			msg = body.Code + ": " + body.Message
		case body.Error != "":
			msg = body.Error
		}
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	return fmt.Errorf("hub answered %d: %s", status, c.scrub(msg))
}

func (c *identityClient) call(method, path string, body any, want int, out any) error {
	status, data, err := c.do(method, path, body)
	if err != nil {
		return err
	}
	if status != want {
		return c.hubRefusal(status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unreadable answer from the hub: %w", err)
		}
	}
	return nil
}

func peerPath(service string) string { return "/v1/identity/peers/" + url.PathEscape(service) }

func (c *identityClient) peerList(out io.Writer) error {
	var resp struct {
		Peers []struct {
			ServiceID     string `json:"service_id"`
			HubID         string `json:"hub_id"`
			Endpoint      string `json:"introspection_endpoint"`
			Disabled      bool   `json:"disabled"`
			Introspection bool   `json:"introspection_operational"`
			TLSTrust      struct {
				Mode  string `json:"mode"`
				Value string `json:"value"`
			} `json:"tls_trust"`
		} `json:"peers"`
	}
	if err := c.call("GET", "/v1/identity/peers", nil, http.StatusOK, &resp); err != nil {
		return err
	}
	if len(resp.Peers) == 0 {
		fmt.Fprintln(out, "no identity peer is registered")
		return nil
	}
	for _, p := range resp.Peers {
		state := "enabled"
		if p.Disabled {
			state = "disabled"
		}
		introspection := "not operational"
		if p.Introspection {
			introspection = "operational"
		}
		fmt.Fprintf(out, "%s  %s  hub %s\n  introspection endpoint %s (%s)\n  endpoint trust %s %s\n",
			p.ServiceID, state, p.HubID, p.Endpoint, introspection, p.TLSTrust.Mode, p.TLSTrust.Value)
	}
	return nil
}

func (c *identityClient) peerRegister(service, endpoint string, trustDNS bool, trustPin string, out io.Writer) error {
	trust := map[string]string{"mode": "spki_sha256", "value": trustPin}
	if trustDNS {
		u, err := url.Parse(endpoint)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("--endpoint must be the peer's https introspection URL")
		}
		trust = map[string]string{"mode": "ca_dns", "value": u.Hostname()}
	}
	body := map[string]any{"service_id": service, "introspection_endpoint": endpoint, "tls_trust": trust}
	if err := c.call("POST", "/v1/identity/peers", body, http.StatusCreated, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "identity peer %s registered (endpoint trust %s %s); introspection is not operational yet\n", service, trust["mode"], trust["value"])
	return nil
}

func (c *identityClient) peerSetDisabled(service string, disabled bool, out io.Writer) error {
	if err := c.call("PUT", peerPath(service), map[string]bool{"disabled": disabled}, http.StatusOK, nil); err != nil {
		return err
	}
	state := "enabled"
	if disabled {
		state = "disabled; its credentials are refused"
	}
	fmt.Fprintf(out, "identity peer %s %s\n", service, state)
	return nil
}

func (c *identityClient) creds(service string) ([]identityCred, error) {
	var resp struct {
		Credentials []identityCred `json:"credentials"`
	}
	if err := c.call("GET", peerPath(service)+"/credentials", nil, http.StatusOK, &resp); err != nil {
		return nil, err
	}
	return resp.Credentials, nil
}

func printCreds(out io.Writer, creds []identityCred, now time.Time) {
	for _, cr := range creds {
		fmt.Fprintf(out, "  %s  %-7s  created %s  expires %s\n", cr.ID, cr.state(now),
			cr.CreatedAt.Format(time.RFC3339), cr.ExpiresAt.Format(time.RFC3339))
	}
}

func (c *identityClient) credList(service string, out io.Writer) error {
	creds, err := c.creds(service)
	if err != nil {
		return err
	}
	if len(creds) == 0 {
		fmt.Fprintf(out, "%s has no credentials\n", service)
		return nil
	}
	fmt.Fprintf(out, "credentials of %s:\n", service)
	printCreds(out, creds, time.Now())
	return nil
}

func (c *identityClient) credRevoke(service, id string) error {
	return c.call("DELETE", peerPath(service)+"/credentials/"+url.PathEscape(id), nil, http.StatusOK, nil)
}

// reserveSecretFile checks the destination before anything is issued: the
// directory must exist and the path must not, and the file is created
// exclusively with owner-only access right away.
func reserveSecretFile(path string) (*os.File, error) {
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("--secret-file %s: its directory does not exist; nothing was issued", path)
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("--secret-file %s already exists and is never overwritten; choose a new path; nothing was issued", path)
	}
	f, err := createPrivateFile(path)
	if err != nil {
		return nil, fmt.Errorf("--secret-file %s cannot be created (%v); nothing was issued", path, err)
	}
	if err := checkPrivateFile(path); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("--secret-file %s is not private after creation (%v); nothing was issued", path, err)
	}
	return f, nil
}

// credIssue issues one credential and delivers its bearer only to the
// reserved secret file. With rotate it first requires exactly one active
// credential and never revokes anything.
func (c *identityClient) credIssue(service string, expiry time.Time, secretFile string, rotate bool, out io.Writer) error {
	f, err := reserveSecretFile(secretFile)
	if err != nil {
		return err
	}
	delivered := false
	defer func() {
		if !delivered {
			f.Close()
			os.Remove(secretFile)
		}
	}()
	// The credentials that exist before the request: after an unknown
	// outcome, anything new is a candidate. Comparing IDs, not times, keeps
	// the answer independent of the operator's and the hub's clocks.
	before, err := c.creds(service)
	if err != nil {
		return fmt.Errorf("cannot read the current credentials (%v); nothing was issued", err)
	}
	known := map[string]bool{}
	var old []identityCred
	now := time.Now()
	for _, cr := range before {
		known[cr.ID] = true
		if cr.state(now) == "active" {
			old = append(old, cr)
		}
	}
	if rotate {
		switch len(old) {
		case 0:
			return fmt.Errorf("%s has no active credential to rotate; use cred issue; nothing was issued", service)
		case 1:
		default:
			return fmt.Errorf("%s already has two active credentials; after aicrew uses the new one, revoke the old one with: %s; nothing was issued",
				service, c.command("cred", "revoke", service, old[0].ID))
		}
	}
	status, data, err := c.do("POST", peerPath(service)+"/credentials", map[string]any{"expires_at": expiry.UTC().Format(time.RFC3339)})
	switch {
	case err != nil || status >= 500:
		cause := err
		if cause == nil {
			cause = c.hubRefusal(status, data)
		}
		return c.unknownOutcome(service, known, cause, out)
	case status != http.StatusCreated:
		return fmt.Errorf("%v; nothing was issued", c.hubRefusal(status, data))
	}
	var resp struct {
		Credential identityCred `json:"credential"`
		Secret     string       `json:"secret"`
	}
	json.Unmarshal(data, &resp)
	if resp.Credential.ID == "" {
		return c.unknownOutcome(service, known, errors.New("the hub's answer did not name the credential"), out)
	}
	if !strings.HasPrefix(resp.Secret, "aimem_peer_") {
		return c.undeliverable(service, resp.Credential.ID, secretFile, errors.New("the answer carried no bearer"))
	}
	if err := deliverSecret(f, resp.Secret); err != nil {
		return c.undeliverable(service, resp.Credential.ID, secretFile, err)
	}
	delivered = true
	fmt.Fprintf(out, "issued credential %s for %s, expiring %s\n", resp.Credential.ID, service, resp.Credential.ExpiresAt.Format(time.RFC3339))
	fmt.Fprintf(out, "the bearer was written once to %s (readable only by you); move it into aicrew's protected storage, then delete the file\n", secretFile)
	if rotate {
		fmt.Fprintf(out, "next: once aicrew uses the new credential, revoke the old one explicitly:\n  %s\n", c.command("cred", "revoke", service, old[0].ID))
	}
	return nil
}

// undeliverable handles a credential the hub confirmed but the bearer of
// which could not be written: only that exact credential is revoked, and
// the result of the revocation is reported.
func (c *identityClient) undeliverable(service, id, secretFile string, cause error) error {
	if err := c.credRevoke(service, id); err != nil {
		return fmt.Errorf("credential %s was issued but its bearer could not be written to %s (%v); revoking it FAILED (%v); revoke it now with: %s",
			id, secretFile, cause, err, c.command("cred", "revoke", service, id))
	}
	return fmt.Errorf("credential %s was issued but its bearer could not be written to %s (%v); that credential has been revoked, so issue a new one", id, secretFile, cause)
}

// unknownOutcome handles an issue whose result the hub never reported. It
// never reissues and never revokes: it lists the active credentials that did
// not exist before the request and says what is safe to do.
func (c *identityClient) unknownOutcome(service string, known map[string]bool, cause error, out io.Writer) error {
	fmt.Fprintf(out, "the outcome of the issue request is unknown (%v).\n", cause)
	fmt.Fprintln(out, "nothing was reissued and nothing was revoked. If a credential was issued, its bearer was never delivered and cannot be recovered.")
	creds, err := c.creds(service)
	if err != nil {
		fmt.Fprintf(out, "the credential list is unavailable too (%v); when the hub answers again, run: %s\n", err, c.command("cred", "list", service))
		return fmt.Errorf("issue outcome unknown")
	}
	// Every new credential the hub has not revoked is a candidate. Its
	// expiry is shown as the hub reports it, never judged against this
	// machine's clock, which may disagree with the hub's.
	var candidates []identityCred
	for _, cr := range creds {
		if !known[cr.ID] && !cr.Revoked {
			candidates = append(candidates, cr)
		}
	}
	if len(candidates) == 0 {
		fmt.Fprintf(out, "no new unrevoked credential of %s exists since the request; it is safe to issue again.\n", service)
		return fmt.Errorf("issue outcome unknown")
	}
	fmt.Fprintf(out, "unrevoked credentials of %s that did not exist before the request:\n", service)
	for _, cr := range candidates {
		fmt.Fprintf(out, "  %s  created %s  expires %s (hub time)\n", cr.ID, cr.CreatedAt.Format(time.RFC3339), cr.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Fprintln(out, "if no other operator issued a credential in this window, these bearers were never delivered: revoke each with")
	for _, cr := range candidates {
		fmt.Fprintf(out, "  %s\n", c.command("cred", "revoke", service, cr.ID))
	}
	fmt.Fprintln(out, "then issue again.")
	return fmt.Errorf("issue outcome unknown")
}
