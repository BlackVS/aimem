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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"aimem/internal/privatefile"
)

const identityUsage = `usage: aimem identity peer list                                  [hub flags]
       aimem identity peer register SERVICE --endpoint URL
                                 (--peer-trust-dns | --peer-trust-pin sha256-BASE64) [hub flags]
       aimem identity peer enable|disable|check SERVICE            [hub flags]
       aimem identity cred list SERVICE                            [hub flags]
       aimem identity cred issue|rotate SERVICE --expires 90d|RFC3339 --output FILE|-
                                 [--operation identity.redeem|reservation.read] [hub flags]
       aimem identity cred revoke --peer SERVICE --credential ID   [hub flags]
       aimem identity team list SERVICE                            [hub flags]
       aimem identity team create|enable|disable|grants --peer SERVICE --team-id TEAM [hub flags]
       aimem identity team grant --peer SERVICE --team-id TEAM --project PROJECT [hub flags]
       aimem identity team revoke --peer SERVICE --team-id TEAM (--project PROJECT | --instance ID) [hub flags]

A command that names one entity takes it as its one argument or as --peer;
a command that names several takes each by its flag. The positional forms
of earlier releases (for example 'team grant SERVICE TEAM PROJECT') keep
working for this release and print the new form. --secret-file is the old
name of --output and keeps working for this release.

Examples:
  aimem identity team grant --peer aicrew-example --team-id TEAM_ID --project example --hub https://hub.example.test:8443 --admin-token-file admin.token
  aimem identity cred issue aicrew-example --operation reservation.read --expires 90d --output reservation-read.secret --hub https://hub.example.test:8443 --admin-token-file admin.token
  aimem identity cred revoke --peer aicrew-example --credential CREDENTIAL_ID --hub https://hub.example.test:8443 --admin-token-file admin.token

Manage the aicrew identity peer, its credentials and its team access
profiles through the hub's TLS listener. The local socket is not used: identity routes require TLS
terminated by the hub.

Hub flags (after the arguments):
  --hub https://HOST:PORT    the hub's TLS listener (required; http is refused)
  --admin-token-file PATH    a hub-admin bearer on one line, in a file only you
                             can read (required; there is no other source)
  --hub-ca-file PATH         trust only this CA bundle for the hub
  --hub-pin sha256-BASE64    trust only a hub certificate with this SPKI SHA-256
  Without --hub-ca-file or --hub-pin the system roots are used. There is no
  insecure mode.

--endpoint is the full URL of the peer's introspection route, ending in
/v1/crew/introspect. --peer-trust-dns and --peer-trust-pin are its trust
binding, which the hub verifies on every introspection; they are separate
from how this command trusts the hub.

peer check has the hub send the peer one introspection with a random handle
that no session holds and verify the inactive answer. It exits non-zero and
names the failed step otherwise. The hub reads its introspection credential
from the private file named by AIMEM_INTROSPECTION_TOKEN_FILE; the credential
never passes through this command.

team commands link an aicrew team (by aicrew's team ID) to the registered
peer SERVICE and grant it projects. A team session then reads only the
projects its profile is granted, checked live on every request: a revoke or
disable takes effect on the next team request. There is no delete; disable a
profile instead. revoke --instance removes a grant whose project was renamed
away or deleted, by the instance ID that team grants lists.

cred issue and cred rotate write the new bearer once to --output: a new
file only you can read, created before anything is issued, or - for
standard output into a pipe (refused on a terminal; every other line then
goes to standard error). It is never printed to a screen. Deliver a file
to aicrew's protected storage, then delete it.
cred rotate issues the second credential only; after aicrew has switched to
it, revoke the old one explicitly with cred revoke.

A credential permits exactly one --operation: identity.redeem (the default),
which redeems identity proofs, or reservation.read, aicrew's read-only
reservation scope. At most two are active per peer and operation, and
cred rotate counts only credentials of the named operation.`

type identityCred struct {
	ID        string    `json:"id"`
	ServiceID string    `json:"service_id"`
	Operation string    `json:"operation"`
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
// run's connection options, quoted for this platform's operator shell
// (PowerShell on Windows, a POSIX shell elsewhere). It names the admin token
// file, never the token.
func (c *identityClient) command(args ...string) string {
	return shellCommand(runtime.GOOS == "windows", append([]string{"aimem", "identity"}, append(args, c.hubArgs...)...))
}

func shellCommand(powershell bool, args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellArg(a, powershell)
	}
	return strings.Join(parts, " ")
}

// shellArg quotes one argument so the shell passes it through literally.
// Plain words stay bare. Anything else is single-quoted, which neither a
// POSIX shell nor PowerShell expands ($, backquote, double quote and spaces
// are literal): POSIX closes the quote around an embedded ' ('\”), and
// PowerShell doubles each of its single-quote characters.
func shellArg(s string, powershell bool) string {
	plain := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("_./:-", r) || (powershell && r == '\\')) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	if !powershell {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if strings.ContainsRune("'\u2018\u2019\u201a\u201b", r) {
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
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

func identityCmd(args []string) error {
	if len(args) > 0 && args[0] == "proof" {
		return identityProofCmd(args[1:], os.Stdout, stateRoot())
	}
	return runIdentity(args, os.Stdout)
}

// identitySlots are the entities each command names, in the order of the
// positional form of earlier releases.
var identitySlots = map[string][]string{
	"peer list": nil, "peer register": {"peer"}, "peer enable": {"peer"}, "peer disable": {"peer"}, "peer check": {"peer"},
	"cred list": {"peer"}, "cred issue": {"peer"}, "cred rotate": {"peer"}, "cred revoke": {"peer", "credential"},
	"team list": {"peer"}, "team create": {"peer", "team-id"}, "team enable": {"peer", "team-id"}, "team disable": {"peer", "team-id"},
	"team grants": {"peer", "team-id"}, "team grant": {"peer", "team-id", "project"}, "team revoke": {"peer", "team-id", "project"},
}

func runIdentity(args []string, out io.Writer) error {
	usage := fmt.Errorf("%s", identityUsage)
	if len(args) < 2 {
		return usage
	}
	noun, verb, rest := args[0], args[1], args[2:]
	cmd := noun + " " + verb
	slots, ok := identitySlots[cmd]
	if !ok {
		return usage
	}
	// A command with one entity keeps it as its one positional argument;
	// the positional form of a command with several entities is kept for
	// this release with a notice.
	var pos []string
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") && len(pos) < len(slots) {
		pos, rest = append(pos, rest[0]), rest[1:]
	}
	fs := flag.NewFlagSet("aimem identity "+cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hub := fs.String("hub", "", "")
	tokenFile := fs.String("admin-token-file", "", "")
	caFile := fs.String("hub-ca-file", "", "")
	pin := fs.String("hub-pin", "", "")
	endpoint := fs.String("endpoint", "", "")
	trustDNS := fs.Bool("peer-trust-dns", false, "")
	trustPin := fs.String("peer-trust-pin", "", "")
	expires := fs.String("expires", "", "")
	output := fs.String("output", "", "")
	secretFile := fs.String("secret-file", "", "")
	operation := fs.String("operation", "identity.redeem", "")
	instance := fs.String("instance", "", "")
	named := map[string]*string{
		"peer": fs.String("peer", "", ""), "credential": fs.String("credential", "", ""),
		"team-id": fs.String("team-id", "", ""), "project": fs.String("project", "", ""),
	}
	fs.StringVar(named["project"], "p", "", "")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%v\n\n%s", err, identityUsage)
	}
	if fs.NArg() != 0 {
		return usage
	}
	ent := map[string]string{}
	for i, slot := range slots {
		v := *named[slot]
		if i < len(pos) {
			if v != "" {
				return fmt.Errorf("the %s is given twice, as an argument and as --%s", slot, slot)
			}
			v = pos[i]
		}
		ent[slot] = v
		if v == "" && !(cmd == "team revoke" && slot == "project") {
			return fmt.Errorf("%s needs --%s\n\n%s", cmd, slot, identityUsage)
		}
	}
	for name, p := range named {
		if *p != "" && !slices.Contains(slots, name) {
			return fmt.Errorf("--%s does not apply to %s", name, cmd)
		}
	}
	// Validate the command's own arguments before touching the network, the
	// token file or the secret file.
	var expiry time.Time
	switch cmd {
	case "peer register":
		if *endpoint == "" || *trustDNS == (*trustPin != "") {
			return fmt.Errorf("peer register needs --endpoint and exactly one of --peer-trust-dns or --peer-trust-pin")
		}
	case "team create":
		if t := ent["team-id"]; t == "." || t == ".." {
			return fmt.Errorf("team ID %q is not allowed: a team ID cannot be \".\" or \"..\"", t)
		}
	case "team revoke":
		if (ent["project"] != "") == (*instance != "") {
			return fmt.Errorf("team revoke needs exactly one of --project or --instance ID")
		}
	case "cred issue", "cred rotate":
		if *output != "" && *secretFile != "" {
			return fmt.Errorf("--secret-file is the old name of --output; give one of them")
		}
		if *output == "" && *secretFile == "" || *expires == "" {
			return fmt.Errorf("%s needs --expires and --output", cmd)
		}
		if *operation != "identity.redeem" && *operation != "reservation.read" {
			return fmt.Errorf("--operation must be identity.redeem or reservation.read")
		}
		var err error
		if expiry, err = parseIdentityExpiry(*expires, time.Now()); err != nil {
			return err
		}
	}
	if len(slots) > 1 && len(pos) > 0 {
		legacyForm(identityNewForm(cmd, slots, ent))
	}
	if *secretFile != "" {
		legacyForm("--output in place of --secret-file")
		*output = *secretFile
	}
	c, err := newIdentityClient(*hub, *tokenFile, *caFile, *pin)
	if err != nil {
		return err
	}
	peer, team := ent["peer"], ent["team-id"]
	switch cmd {
	case "peer list":
		return c.peerList(out)
	case "peer register":
		return c.peerRegister(peer, *endpoint, *trustDNS, *trustPin, out)
	case "peer enable", "peer disable":
		return c.peerSetDisabled(peer, verb == "disable", out)
	case "peer check":
		return c.peerCheck(peer, out)
	case "cred list":
		return c.credList(peer, out)
	case "cred issue", "cred rotate":
		sink, err := reserveSecretOutput("--output", *output)
		if err != nil {
			return err
		}
		if sink.toStdout() {
			out = noticeOut
		}
		return c.credIssue(peer, *operation, expiry, sink, verb == "rotate", out)
	case "cred revoke":
		if err := c.credRevoke(peer, ent["credential"]); err != nil {
			return err
		}
		fmt.Fprintf(out, "credential %s of %s revoked\n", ent["credential"], peer)
		return nil
	case "team list":
		return c.teamList(peer, out)
	case "team create":
		return c.teamCreate(peer, team, out)
	case "team enable", "team disable":
		return c.teamSetDisabled(peer, team, verb == "disable", out)
	case "team grants":
		return c.teamGrants(peer, team, out)
	case "team grant":
		return c.teamGrant(peer, team, ent["project"], out)
	case "team revoke":
		if *instance != "" {
			return c.teamRevokeInstance(peer, team, *instance, out)
		}
		return c.teamRevoke(peer, team, ent["project"], out)
	}
	return usage
}

// identityNewForm renders the named form of a positional invocation.
func identityNewForm(cmd string, slots []string, ent map[string]string) string {
	args := append([]string{"aimem", "identity"}, strings.Fields(cmd)...)
	for _, s := range slots {
		if ent[s] != "" {
			args = append(args, "--"+s, ent[s])
		}
	}
	return shellCommand(runtime.GOOS == "windows", args) + " [hub flags]"
}

type identityTeam struct {
	ProfileID string `json:"profile_id"`
	TeamID    string `json:"team_id"`
	Disabled  bool   `json:"disabled"`
	Grants    []struct {
		Project  string `json:"project"`
		Instance string `json:"instance"`
	} `json:"grants"`
}

func teamPath(service, team string) string {
	return peerPath(service) + "/teams/" + url.PathEscape(team)
}

func printTeam(out io.Writer, t identityTeam) {
	state := "enabled"
	if t.Disabled {
		state = "disabled"
	}
	fmt.Fprintf(out, "%s  %s  profile %s\n", t.TeamID, state, t.ProfileID)
	if len(t.Grants) == 0 {
		fmt.Fprintln(out, "  no project grants")
	}
	for _, g := range t.Grants {
		name := g.Project
		if name == "" {
			name = "(project gone; revoke with --instance)"
		}
		fmt.Fprintf(out, "  grant %s  instance %s\n", name, g.Instance)
	}
}

func (c *identityClient) teamList(service string, out io.Writer) error {
	var resp struct {
		Teams []identityTeam `json:"teams"`
	}
	if err := c.call("GET", peerPath(service)+"/teams", nil, http.StatusOK, &resp); err != nil {
		return err
	}
	if len(resp.Teams) == 0 {
		fmt.Fprintf(out, "no team profile for %s\n", service)
	}
	for _, t := range resp.Teams {
		printTeam(out, t)
	}
	return nil
}

func (c *identityClient) teamCreate(service, team string, out io.Writer) error {
	var t identityTeam
	if err := c.call("POST", peerPath(service)+"/teams", map[string]string{"team_id": team}, http.StatusCreated, &t); err != nil {
		return err
	}
	fmt.Fprintf(out, "team profile %s created for %s (profile %s); it has no project grants yet\n", team, service, t.ProfileID)
	return nil
}

func (c *identityClient) teamSetDisabled(service, team string, disabled bool, out io.Writer) error {
	if err := c.call("PUT", teamPath(service, team), map[string]bool{"disabled": disabled}, http.StatusOK, nil); err != nil {
		return err
	}
	state := "enabled"
	if disabled {
		state = "disabled; its team sessions are refused from the next request"
	}
	fmt.Fprintf(out, "team profile %s of %s %s\n", team, service, state)
	return nil
}

func (c *identityClient) teamGrants(service, team string, out io.Writer) error {
	var t identityTeam
	if err := c.call("GET", teamPath(service, team)+"/grants", nil, http.StatusOK, &t); err != nil {
		return err
	}
	printTeam(out, t)
	return nil
}

func (c *identityClient) teamGrant(service, team, project string, out io.Writer) error {
	var resp struct {
		Instance string `json:"instance"`
	}
	if err := c.call("PUT", teamPath(service, team)+"/grants/"+url.PathEscape(project), nil, http.StatusOK, &resp); err != nil {
		return err
	}
	fmt.Fprintf(out, "project %s (instance %s) granted to team %s of %s\n", project, resp.Instance, team, service)
	return nil
}

func (c *identityClient) teamRevoke(service, team, project string, out io.Writer) error {
	var resp struct {
		Revoked bool `json:"revoked"`
	}
	if err := c.call("DELETE", teamPath(service, team)+"/grants/"+url.PathEscape(project), nil, http.StatusOK, &resp); err != nil {
		return err
	}
	if !resp.Revoked {
		fmt.Fprintf(out, "project %s has never had an access instance, so team %s of %s holds no grant on it\n", project, team, service)
		return nil
	}
	fmt.Fprintf(out, "project %s revoked from team %s of %s; the next team read of it is refused\n", project, team, service)
	return nil
}

func (c *identityClient) teamRevokeInstance(service, team, instance string, out io.Writer) error {
	if err := c.call("DELETE", teamPath(service, team)+"/grant-instances/"+url.PathEscape(instance), nil, http.StatusOK, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "instance %s revoked from team %s of %s\n", instance, team, service)
	return nil
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
	if err := privatefile.Check(tokenFile); err != nil {
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
	var got struct {
		Introspection bool `json:"introspection_operational"`
	}
	if err := c.call("POST", "/v1/identity/peers", body, http.StatusCreated, &got); err != nil {
		return err
	}
	state := "not operational yet; see " + c.command("peer", "check", service)
	if got.Introspection {
		state = "operational; verify it with " + c.command("peer", "check", service)
	}
	fmt.Fprintf(out, "identity peer %s registered (endpoint trust %s %s); introspection %s\n", service, trust["mode"], trust["value"], state)
	return nil
}

// peerCheckHints explain the outcomes an operator can act on; every other
// outcome names the reply check that failed.
var peerCheckHints = map[string]string{
	"not_configured":    "the hub has no AIMEM_INTROSPECTION_TOKEN_FILE set",
	"credential_file":   "the hub's AIMEM_INTROSPECTION_TOKEN_FILE is missing, readable by other accounts, or not one line",
	"peer_record":       "the registered endpoint must be the https URL of /v1/crew/introspect with a matching trust binding",
	"peer_disabled":     "the peer is disabled",
	"peer_ambiguous":    "more than one peer is enabled on this hub",
	"tls_untrusted":     "the peer's certificate does not match the registered trust binding",
	"timeout":           "the peer did not answer within 2 s",
	"transport":         "the peer could not be reached",
	"version_rejected":  "the peer refused identity version 1",
	"status":            "the peer answered with an unexpected HTTP status (check its credential for this hub)",
	"unexpected_active": "the peer called a handle that no session holds active",
}

func (c *identityClient) peerCheck(service string, out io.Writer) error {
	var got struct {
		OK      bool   `json:"ok"`
		Outcome string `json:"outcome"`
	}
	if err := c.call("POST", peerPath(service)+"/check", nil, http.StatusOK, &got); err != nil {
		return err
	}
	if got.OK {
		fmt.Fprintf(out, "identity peer %s introspection works: the peer verified and answered a probe as inactive\n", service)
		return nil
	}
	msg := fmt.Sprintf("identity peer %s introspection check failed: %s", service, got.Outcome)
	if hint := peerCheckHints[got.Outcome]; hint != "" {
		msg += " (" + hint + ")"
	}
	return errors.New(msg)
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
		fmt.Fprintf(out, "  %s  %-7s  created %s  expires %s  %s\n", cr.ID, cr.state(now),
			cr.CreatedAt.Format(time.RFC3339), cr.ExpiresAt.Format(time.RFC3339), cr.Operation)
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

// credIssue issues one credential for one operation and delivers its bearer
// only to the reserved --output destination. With rotate it first requires
// exactly one active credential of that operation and never revokes anything.
func (c *identityClient) credIssue(service, operation string, expiry time.Time, sink *secretOutput, rotate bool, out io.Writer) error {
	delivered := false
	defer func() {
		if !delivered {
			sink.abandon()
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
		if cr.state(now) == "active" && cr.Operation == operation {
			old = append(old, cr)
		}
	}
	if rotate {
		switch len(old) {
		case 0:
			return fmt.Errorf("%s has no active credential for %s to rotate; use cred issue; nothing was issued", service, operation)
		case 1:
		default:
			return fmt.Errorf("%s already has two active credentials for %s; after aicrew uses the new one, revoke the old one with: %s; nothing was issued",
				service, operation, c.command("cred", "revoke", "--peer", service, "--credential", old[0].ID))
		}
	}
	status, data, err := c.do("POST", peerPath(service)+"/credentials", map[string]any{"expires_at": expiry.UTC().Format(time.RFC3339), "operation": operation})
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
		return c.undeliverable(service, resp.Credential.ID, sink.where(), errors.New("the answer carried no bearer"))
	}
	if err := sink.write(resp.Secret); err != nil {
		return c.undeliverable(service, resp.Credential.ID, sink.where(), err)
	}
	delivered = true
	fmt.Fprintf(out, "issued credential %s (%s) for %s, expiring %s\n", resp.Credential.ID, resp.Credential.Operation, service, resp.Credential.ExpiresAt.Format(time.RFC3339))
	if sink.toStdout() {
		fmt.Fprintln(out, "the bearer was written once to standard output")
	} else {
		fmt.Fprintf(out, "the bearer was written once to %s; move it into aicrew's protected storage, then delete the file\n", sink.where())
	}
	if rotate {
		fmt.Fprintf(out, "next: once aicrew uses the new credential, revoke the old one explicitly:\n  %s\n", c.command("cred", "revoke", "--peer", service, "--credential", old[0].ID))
	}
	return nil
}

// undeliverable handles a credential the hub confirmed but the bearer of
// which could not be written: only that exact credential is revoked, and
// the result of the revocation is reported.
func (c *identityClient) undeliverable(service, id, where string, cause error) error {
	if err := c.credRevoke(service, id); err != nil {
		return fmt.Errorf("credential %s was issued but its bearer could not be written to %s (%v); revoking it FAILED (%v); revoke it now with: %s",
			id, where, cause, err, c.command("cred", "revoke", "--peer", service, "--credential", id))
	}
	return fmt.Errorf("credential %s was issued but its bearer could not be written to %s (%v); that credential has been revoked, so issue a new one", id, where, cause)
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
		fmt.Fprintf(out, "  %s  created %s  expires %s (hub time)  %s\n", cr.ID, cr.CreatedAt.Format(time.RFC3339), cr.ExpiresAt.Format(time.RFC3339), cr.Operation)
	}
	fmt.Fprintln(out, "if no other operator issued a credential in this window, these bearers were never delivered: revoke each with")
	for _, cr := range candidates {
		fmt.Fprintf(out, "  %s\n", c.command("cred", "revoke", "--peer", service, "--credential", cr.ID))
	}
	fmt.Fprintln(out, "then issue again.")
	return fmt.Errorf("issue outcome unknown")
}
