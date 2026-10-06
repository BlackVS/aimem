package main

// `aimem identity enroll issue|revoke|list`: the hub admin's enrollment
// bundles (enrollment.v1, docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md §1, §2, §4
// and §5). issue writes the one-line subcode record to standard output for
// aicrew's console (`aicrew invitation issue --bundle -`) and nothing else
// there; the record never goes to a file or a terminal.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"aimem/internal/uuidv7"
)

const enrollUsage = `usage: aimem identity enroll issue --purpose new-user --output - [--expires 24h] [--user-name NAME] [hub flags]
       aimem identity enroll revoke --bundle-id ID                   [hub flags]
       aimem identity enroll list [--state issued|redeemed|revoked|expired] [hub flags]

issue records a new enrollment bundle on the hub and writes its one-line
subcode record to standard output, for a pipe into aicrew's console:

  aimem identity enroll issue --purpose new-user --output - --hub https://hub.example.test:8443 --admin-token-file admin.token | aicrew invitation issue ... --bundle -

--output - is the only destination, and it is refused when standard output is
a terminal: the subcode is never written to a file or a screen. --expires is a
duration of at most 72h (default 24h). --user-name is the display name the new
user gets; without it, the client's label at redemption is used. The bundle ID
and the command to revoke it go to standard error before the hub is asked, so
a failed pipeline can always be revoked.

revoke revokes a bundle that was not redeemed. A redeemed bundle names the user
and token it issued: revoking a spent code does not revoke them; use
aimem access token-revoke and aimem access user-set for that.

list shows the bundles, newest first, without any subcode.

Examples, one per command:
  aimem identity enroll issue --purpose new-user --expires 24h --output - --hub https://hub.example.test:8443 --admin-token-file admin.token
  aimem identity enroll revoke --bundle-id 01a10c90-0000-7000-8000-000000000001 --hub https://hub.example.test:8443 --admin-token-file admin.token
  aimem identity enroll list --state issued --hub https://hub.example.test:8443 --admin-token-file admin.token

Hub flags (after the arguments): --hub, --admin-token-file, --hub-ca-file or
--hub-pin, as for every aimem identity command.`

const enrollmentsPath = "/v1/identity/enrollments"

// enrollMaxLife mirrors the hub's cap (enrollment.v1, bounds).
const enrollMaxLife = 72 * time.Hour

// enrollRecord is the subcode record (enrollment.v1 §4).
type enrollRecord struct {
	Kind      string    `json:"kind"`
	Version   int       `json:"version"`
	BundleID  string    `json:"bundle_id"`
	Purpose   string    `json:"purpose"`
	Subcode   string    `json:"subcode"`
	ExpiresAt time.Time `json:"expires_at"`
	Hub       enrollHub `json:"hub"`
}

type enrollHub struct {
	HubID string         `json:"hub_id"`
	URL   string         `json:"url"`
	Trust map[string]any `json:"trust"`
}

func runIdentityEnroll(args []string, out io.Writer) error {
	usage := fmt.Errorf("%s", enrollUsage)
	if len(args) < 1 {
		return usage
	}
	verb, rest := args[0], args[1:]
	fs := flag.NewFlagSet("aimem identity enroll "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hub := fs.String("hub", "", "")
	tokenFile := fs.String("admin-token-file", "", "")
	caFile := fs.String("hub-ca-file", "", "")
	pin := fs.String("hub-pin", "", "")
	purpose := fs.String("purpose", "", "")
	expires := fs.String("expires", "24h", "")
	output := fs.String("output", "", "")
	userName := fs.String("user-name", "", "")
	bundleID := fs.String("bundle-id", "", "")
	state := fs.String("state", "", "")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%v\n\n%s", err, enrollUsage)
	}
	if fs.NArg() != 0 {
		return usage
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	allowed := map[string][]string{
		"issue":  {"purpose", "expires", "output", "user-name"},
		"revoke": {"bundle-id"},
		"list":   {"state"},
	}
	own, ok := allowed[verb]
	if !ok {
		return usage
	}
	for name := range given {
		if !identityHubFlags[name] && !slices.Contains(own, name) {
			return fmt.Errorf("--%s does not apply to enroll %s", name, verb)
		}
	}
	// Validate before touching the network, the token file or the output.
	var life time.Duration
	switch verb {
	case "issue":
		if *purpose != "new-user" {
			return fmt.Errorf("enroll issue needs --purpose new-user (existing-user reissue is not available yet)")
		}
		if *output != "-" {
			return fmt.Errorf("enroll issue needs --output -: the subcode record goes only into a pipe, never to a file")
		}
		var err error
		if life, err = time.ParseDuration(*expires); err != nil || life <= 0 || life > enrollMaxLife {
			return fmt.Errorf("--expires %q: use a duration of at most 72h, such as 24h", *expires)
		}
	case "revoke":
		if *bundleID == "" {
			return fmt.Errorf("enroll revoke needs --bundle-id\n\n%s", enrollUsage)
		}
	}
	var sink *secretOutput
	var trust map[string]any
	if verb == "issue" {
		var err error
		if sink, err = reserveSecretOutput("--output", "-"); err != nil {
			return err
		}
		// A terminal is refused above; a file that standard output was
		// redirected to is refused here: the record exists only in a pipe.
		if err := requireStdoutPipe(); err != nil {
			return err
		}
		out = noticeOut
		if trust, err = enrollTrust(*caFile, *pin); err != nil {
			return err
		}
	}
	c, err := newIdentityClient(*hub, *tokenFile, *caFile, *pin)
	if err != nil {
		return err
	}
	switch verb {
	case "issue":
		// The record's size is known before the hub is asked: only the
		// subcode and the hub ID come back, and their sizes are bounded.
		if n := enrollRecordSize(c.base, trust); n > enrollRecordMax {
			return fmt.Errorf("the subcode record would be %d bytes, more than enrollment.v1's %d (the --hub-ca-file bundle is too large; use --hub-pin or a smaller CA file); nothing was issued", n, enrollRecordMax)
		}
		return c.enrollIssue(uuidv7.New(), time.Now().Add(life), *userName, trust, sink, out)
	case "revoke":
		return c.enrollRevoke(*bundleID, out)
	case "list":
		return c.enrollList(*state, out)
	}
	return usage
}

// enrollRecordMax is enrollment.v1's bound on a subcode record (§4).
const enrollRecordMax = 16384

// requireStdoutPipe refuses a standard output that is not a pipe: a file it
// was redirected to would hold the subcode (enrollment.v1 §5).
func requireStdoutPipe() error {
	fi, err := secretStdout.Stat()
	if err != nil {
		return fmt.Errorf("--output -: cannot inspect standard output (%v); nothing was issued", err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		return fmt.Errorf("--output -: standard output is not a pipe; the subcode record goes only into a pipe to the command that reads it, never to a file; nothing was issued")
	}
	return nil
}

// enrollRecordSize is the size of the record this command would write, with
// the hub's two answers at their largest: a subcode has a fixed length, and
// the hub ID is given 128 bytes, far more than its UUID needs.
func enrollRecordSize(base string, trust map[string]any) int {
	rec, _ := json.Marshal(enrollRecord{Kind: "aimem-enrollment", Version: 1, BundleID: uuidv7.New(), Purpose: "new_user",
		Subcode: "aes1_" + strings.Repeat("x", 43), ExpiresAt: time.Now().Add(enrollMaxLife),
		Hub: enrollHub{HubID: strings.Repeat("x", 128), URL: base, Trust: trust}})
	return len(rec) + 1 // and its newline
}

// enrollTrust is the trust the client will use for the hub: the same this
// command used, so the record carries a binding the operator verified.
func enrollTrust(caFile, pin string) (map[string]any, error) {
	switch {
	case pin != "":
		return map[string]any{"spki_sha256": pin}, nil
	case caFile != "":
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("--hub-ca-file: %w", err)
		}
		return map[string]any{"ca_pem": string(pem)}, nil
	}
	return map[string]any{"system_roots": true}, nil
}

func (c *identityClient) enrollIssue(bundle string, expires time.Time, userName string, trust map[string]any, sink *secretOutput, out io.Writer) error {
	revoke := c.command("enroll", "revoke", "--bundle-id", bundle)
	fmt.Fprintf(out, "aimem: issuing enrollment bundle %s; if this pipeline fails, revoke it with: %s\n", bundle, revoke)
	body := map[string]any{"bundle_id": bundle, "purpose": "new_user", "expires_at": expires.UTC().Truncate(time.Second).Format(time.RFC3339)}
	if userName != "" {
		body["user_name"] = userName
	}
	status, data, err := c.do("POST", enrollmentsPath, body)
	if err != nil {
		return fmt.Errorf("%w; the bundle may exist: revoke it with: %s", err, revoke)
	}
	if status != http.StatusCreated {
		return c.hubRefusal(status, data)
	}
	var issued struct {
		BundleID  string    `json:"bundle_id"`
		Purpose   string    `json:"purpose"`
		ExpiresAt time.Time `json:"expires_at"`
		HubID     string    `json:"hub_id"`
		Subcode   string    `json:"subcode"`
	}
	if err := json.Unmarshal(data, &issued); err != nil || issued.Subcode == "" || issued.BundleID != bundle || issued.HubID == "" {
		return fmt.Errorf("unreadable answer from the hub; revoke the bundle with: %s", revoke)
	}
	rec, err := json.Marshal(enrollRecord{Kind: "aimem-enrollment", Version: 1, BundleID: bundle, Purpose: issued.Purpose,
		Subcode: issued.Subcode, ExpiresAt: issued.ExpiresAt, Hub: enrollHub{HubID: issued.HubID, URL: c.base, Trust: trust}})
	if err != nil {
		return err
	}
	// On Unix a write to a standard output whose reader has gone ends the
	// process with SIGPIPE before the revoke below could run; ignored, the
	// write returns EPIPE and the bundle is revoked.
	signal.Ignore(syscall.SIGPIPE)
	if len(rec)+1 > enrollRecordMax {
		// Unreachable when the hub answers within the contract; a record a
		// consumer must refuse is never written, and the bundle is revoked.
		err = fmt.Errorf("the record would be %d bytes, more than %d", len(rec)+1, enrollRecordMax)
	} else {
		err = sink.write(string(rec))
	}
	if err != nil {
		// The subcode exists only in this process now; the bundle is
		// revoked, so it can never be redeemed.
		if rerr := c.enrollRevokeQuiet(bundle); rerr != nil {
			return fmt.Errorf("the subcode record could not be written (%v), and revoking the bundle failed (%v); revoke it with: %s", err, rerr, revoke)
		}
		return fmt.Errorf("the subcode record could not be written (%v); bundle %s was revoked", err, bundle)
	}
	fmt.Fprintf(out, "aimem: enrollment bundle %s issued, expires %s\n", bundle, issued.ExpiresAt.Format(time.RFC3339))
	return nil
}

func (c *identityClient) enrollRevokeQuiet(bundle string) error {
	status, data, err := c.do("POST", enrollmentsPath+"/"+url.PathEscape(bundle)+"/revocation", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return c.hubRefusal(status, data)
	}
	return nil
}

func (c *identityClient) enrollRevoke(bundle string, out io.Writer) error {
	status, data, err := c.do("POST", enrollmentsPath+"/"+url.PathEscape(bundle)+"/revocation", nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		var resp struct {
			State string `json:"state"`
		}
		json.Unmarshal(data, &resp)
		fmt.Fprintf(out, "enrollment bundle %s: %s\n", bundle, resp.State)
		return nil
	case http.StatusConflict:
		var ref struct {
			Code     string `json:"code"`
			Redeemed struct {
				UserID  string `json:"user_id"`
				TokenID string `json:"token_id"`
			} `json:"redeemed"`
		}
		if json.Unmarshal(data, &ref) == nil && ref.Code == "enrollment_redeemed" {
			return fmt.Errorf("enrollment bundle %s was already redeemed: user %s, token %s; revoking the code does not revoke them. "+
				"If the redemption was not legitimate, run: aimem access token-revoke --token-id %s, and: aimem access user-set --user-id %s --user-name NAME --state disabled",
				bundle, ref.Redeemed.UserID, ref.Redeemed.TokenID, ref.Redeemed.TokenID, ref.Redeemed.UserID)
		}
	}
	return c.hubRefusal(status, data)
}

func (c *identityClient) enrollList(state string, out io.Writer) error {
	path := enrollmentsPath
	if state != "" {
		path += "?state=" + url.QueryEscape(state)
	}
	var resp struct {
		Enrollments []struct {
			BundleID  string    `json:"bundle_id"`
			Purpose   string    `json:"purpose"`
			State     string    `json:"state"`
			ExpiresAt time.Time `json:"expires_at"`
			UserName  string    `json:"user_name"`
			Redeemed  *struct {
				UserID  string `json:"user_id"`
				TokenID string `json:"token_id"`
			} `json:"redeemed"`
		} `json:"enrollments"`
	}
	if err := c.call("GET", path, nil, http.StatusOK, &resp); err != nil {
		return err
	}
	if len(resp.Enrollments) == 0 {
		fmt.Fprintln(out, "no enrollment bundles")
		return nil
	}
	for _, e := range resp.Enrollments {
		line := fmt.Sprintf("%s  %s  %s  expires %s", e.BundleID, e.Purpose, e.State, e.ExpiresAt.Format(time.RFC3339))
		if e.UserName != "" {
			line += "  user name " + e.UserName
		}
		if e.Redeemed != nil {
			line += fmt.Sprintf("  redeemed: user %s, token %s", e.Redeemed.UserID, e.Redeemed.TokenID)
		}
		fmt.Fprintln(out, line)
	}
	return nil
}
