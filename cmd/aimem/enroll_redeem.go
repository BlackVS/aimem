package main

// `aimem enroll redeem`: a clean client redeems its enrollment subcode and
// stores its individual credential (enrollment.v1 §5 and "Client
// protection", docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md). aicrew-agent join
// runs it with the subcode record on standard input. The bearer never
// leaves this process except into the hub entry's credential slot.

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/enrollment"
	"aimem/internal/privatefile"
)

const enrollRedeemUsage = `usage: aimem enroll redeem --hub-name NAME --label LABEL [--json] < record

Reads the subcode record (aimem identity enroll issue's one line) on
standard input, redeems it at the hub the record names, and stores the
delivered credential as hub NAME's individual credential. aicrew-agent join
runs it; nothing of the record is written to disk.

  --hub-name NAME   the hub entry to store the credential under
  --label LABEL     this installation's name (1 to 64 of a-z, 0-9 and -)
  --json            print the result as JSON

A rerun is safe: it resumes an interrupted redemption with the same keys,
and reports already_enrolled when the credential is stored.
Exit: 0 stored or already enrolled; 3 final refusal; 4 retryable refusal;
5 outcome unknown (run the same command again); 2 usage.`

// Exit codes, as the member reservation commands use them.
const (
	enrollExitOK      = 0
	enrollExitUsage   = 2
	enrollExitFinal   = 3
	enrollExitRetry   = 4
	enrollExitUnknown = 5
)

// enrollRecordLimit is enrollment.v1's bound on a subcode record.
const enrollRecordLimit = 16384

// Test hooks: a test stops the command at a phase, as a crash would.
var (
	enrollBackoff = func(attempt int) time.Duration { return time.Duration(1<<attempt) * time.Second }
	enrollCrashAt func(phase string) bool
)

var (
	enrollLabelShape   = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	enrollHubNameShape = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	enrollSubcodeShape = regexp.MustCompile(`^aes1_[A-Za-z0-9_-]{43}$`)
	enrollBundleShape  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func enrollCmd(args []string) error {
	if len(args) > 0 && args[0] == "redeem" {
		os.Exit(runEnrollRedeem(args[1:], os.Stdin, os.Stdout, os.Stderr, stateRoot()))
	}
	return fmt.Errorf("%s", enrollRedeemUsage)
}

// enrollPending is the pending state of one bundle's redemption: the keys a
// retry must repeat, and, once the delivery is open, the identity it carried.
// It never holds the subcode or the bearer.
type enrollPending struct {
	BundleID   string    `json:"bundle_id"`
	Phase      string    `json:"phase"` // sending | delivered
	RequestKey string    `json:"request_key"`
	PrivateKey string    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`
	UserID     string    `json:"user_id,omitempty"`
	TokenID    string    `json:"token_id,omitempty"`
}

type enrollResult struct {
	Hub      string `json:"hub"`
	Outcome  string `json:"outcome"` // enrolled | already_enrolled
	UserID   string `json:"user_id"`
	TokenID  string `json:"token_id"`
	BundleID string `json:"bundle_id"`
}

// enrollRefusal is a final or retryable outcome with its exit code.
type enrollRefusal struct {
	code int
	msg  string
}

func (e *enrollRefusal) Error() string { return e.msg }

func refuse(code int, format string, a ...any) error {
	return &enrollRefusal{code: code, msg: fmt.Sprintf(format, a...)}
}

func runEnrollRedeem(args []string, stdin io.Reader, stdout, stderr io.Writer, root string) int {
	fs := flag.NewFlagSet("aimem enroll redeem", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hubName := fs.String("hub-name", "", "")
	label := fs.String("label", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || !enrollHubNameShape.MatchString(*hubName) || !enrollLabelShape.MatchString(*label) {
		fmt.Fprintln(stderr, enrollRedeemUsage)
		return enrollExitUsage
	}
	res, err := enrollRedeem(root, *hubName, *label, stdin, stderr)
	if err != nil {
		var r *enrollRefusal
		code := enrollExitUnknown
		if errors.As(err, &r) {
			code = r.code
		}
		fmt.Fprintf(stderr, "aimem: enroll redeem: %v\n", err)
		return code
	}
	if *asJSON {
		json.NewEncoder(stdout).Encode(res)
	} else {
		fmt.Fprintf(stdout, "hub %s: %s as user %s (token %s)\n", res.Hub, strings.ReplaceAll(res.Outcome, "_", " "), res.UserID, res.TokenID)
	}
	return enrollExitOK
}

func enrollRedeem(root, hubName, label string, stdin io.Reader, stderr io.Writer) (enrollResult, error) {
	raw, err := io.ReadAll(io.LimitReader(stdin, enrollRecordLimit+1))
	if err != nil {
		return enrollResult{}, refuse(enrollExitFinal, "cannot read the subcode record: %v", err)
	}
	if len(raw) > enrollRecordLimit {
		return enrollResult{}, refuse(enrollExitFinal, "the subcode record is larger than %d bytes", enrollRecordLimit)
	}
	rec, err := parseEnrollRecord(raw)
	if err != nil {
		return enrollResult{}, refuse(enrollExitFinal, "the subcode record is not valid: %v", err)
	}
	hubs, def := adapter.LoadHubs(root)
	if hubs == nil {
		hubs = map[string]*adapter.HubConfig{}
	}
	entry := hubs[hubName]
	if entry != nil && strings.TrimRight(entry.URL, "/") != strings.TrimRight(rec.Hub.URL, "/") {
		return enrollResult{}, refuse(enrollExitFinal, "identity_mismatch: hub %s on this installation is %s, but the record names %s", hubName, entry.URL, rec.Hub.URL)
	}
	client, err := enrollHTTPClient(rec.Hub.Trust)
	if err != nil {
		return enrollResult{}, refuse(enrollExitFinal, "the record's hub trust is not usable: %v", err)
	}
	pendingPath := filepath.Join(root, "enroll", rec.BundleID+".json")
	pending, err := loadEnrollPending(pendingPath)
	if err != nil {
		return enrollResult{}, refuse(enrollExitFinal, "delivery_unavailable: %v", err)
	}
	done := func(outcome, user, token string) (enrollResult, error) {
		os.Remove(pendingPath)
		return enrollResult{Hub: hubName, Outcome: outcome, UserID: user, TokenID: token, BundleID: rec.BundleID}, nil
	}
	// A first run retries only within the subcode's expiry; a recovery
	// retries until the hub answers (enrollment.v1 §5 step 4).
	var until time.Time
	switch {
	case pending == nil:
		until = rec.ExpiresAt
		if !time.Now().Before(rec.ExpiresAt) {
			return enrollResult{}, refuse(enrollExitFinal, "enrollment_invalid: the onboarding code expired at %s; ask the operator for a new one", rec.ExpiresAt.Format(time.RFC3339))
		}
		// A credential already stored and accepted: nothing to spend. The
		// code is spent only when the hub has refused the stored credential;
		// a check that could not finish is retried, never taken as a refusal,
		// or a passing outage would replace a working identity.
		if entry != nil && entry.TaskToken != "" {
			id, err := enrollWhoAmI(client, rec.Hub.URL, entry.TaskToken)
			switch {
			case err == nil:
				return done("already_enrolled", id.UserID, id.TokenID)
			case !errors.Is(err, errCredentialRefused):
				return enrollResult{}, refuse(enrollExitRetry, "hub %s already holds a credential that could not be checked (%v); nothing was spent; run the same command again", hubName, err)
			}
		}
		if pending, err = newEnrollPending(pendingPath, rec.BundleID); err != nil {
			return enrollResult{}, refuse(enrollExitFinal, "delivery_unavailable: the pending state cannot be protected (%v); nothing was sent", err)
		}
	case pending.Phase == "delivered" && entry != nil && entry.TaskToken != "":
		// The bearer was stored before the last run ended: verify it against
		// the identity the delivery carried, without the redemption route.
		id, err := enrollWhoAmI(client, rec.Hub.URL, entry.TaskToken)
		if err != nil {
			return enrollResult{}, refuse(enrollExitRetry, "the stored credential could not be verified yet: %v; run the same command again", err)
		}
		if id.UserID != pending.UserID || id.TokenID != pending.TokenID {
			return enrollResult{}, refuse(enrollExitFinal, "identity_mismatch: hub %s holds a credential for user %s, but this bundle delivered user %s", hubName, id.UserID, pending.UserID)
		}
		return done("already_enrolled", id.UserID, id.TokenID)
	}
	keyBytes, err := base64.RawURLEncoding.DecodeString(pending.PrivateKey)
	if err != nil {
		return enrollResult{}, refuse(enrollExitFinal, "delivery_unavailable: the pending state is damaged")
	}
	priv, err := ecdh.X25519().NewPrivateKey(keyBytes)
	if err != nil {
		return enrollResult{}, refuse(enrollExitFinal, "delivery_unavailable: the pending state is damaged")
	}
	sum := sha256.Sum256([]byte(pending.RequestKey))
	k1 := "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
	ans, err := enrollSend(client, rec, label, k1, priv, until)
	if err != nil {
		return enrollResult{}, err
	}
	p, err := enrollment.Open(ans.Delivery.Suite, priv, enrollment.AAD(ans.HubID, ans.BundleID, k1, ans.Identity.UserID, ans.Identity.TokenID),
		ans.Delivery.Enc, ans.Delivery.Ciphertext)
	if err != nil {
		return enrollResult{}, refuse(enrollExitFinal, "identity_mismatch: the delivery does not open with this installation's key")
	}
	if p.UserID != ans.Identity.UserID || p.TokenID != ans.Identity.TokenID || p.HubID != ans.HubID || ans.HubID != rec.Hub.HubID || ans.BundleID != rec.BundleID {
		return enrollResult{}, refuse(enrollExitFinal, "identity_mismatch: the delivery names another hub, bundle or identity than the record and the answer")
	}
	pending.Phase, pending.UserID, pending.TokenID = "delivered", p.UserID, p.TokenID
	if err := writeEnrollPending(pendingPath, pending); err != nil {
		return enrollResult{}, refuse(enrollExitUnknown, "the delivered identity could not be recorded (%v); run the same command again", err)
	}
	if enrollCrashAt != nil && enrollCrashAt("delivered") {
		return enrollResult{}, refuse(enrollExitUnknown, "stopped after recording the delivery (test)")
	}
	if entry == nil {
		entry = &adapter.HubConfig{}
		hubs[hubName] = entry
	}
	entry.URL, entry.TaskToken, entry.Insecure, entry.Pin, entry.CAFile = rec.Hub.URL, p.Token, false, "", ""
	switch {
	case rec.Hub.Trust.SPKI != "":
		entry.Pin = rec.Hub.Trust.SPKI
	case rec.Hub.Trust.CAPEM != "":
		caPath := filepath.Join(root, "hub-ca-"+hubName+".pem")
		if err := writeFileAtomic(caPath, []byte(rec.Hub.Trust.CAPEM)); err != nil {
			return enrollResult{}, refuse(enrollExitUnknown, "the hub's CA bundle could not be written (%v); run the same command again", err)
		}
		entry.CAFile = caPath
	}
	if def == "" {
		def = hubName
	}
	if err := adapter.SaveHubs(root, hubs, def); err != nil {
		return enrollResult{}, refuse(enrollExitUnknown, "the credential could not be stored (%v); run the same command again", err)
	}
	if enrollCrashAt != nil && enrollCrashAt("stored") {
		return enrollResult{}, refuse(enrollExitUnknown, "stopped after storing the credential (test)")
	}
	id, err := enrollWhoAmI(client, rec.Hub.URL, p.Token)
	if err != nil {
		return enrollResult{}, refuse(enrollExitRetry, "the stored credential could not be verified yet: %v; run the same command again", err)
	}
	if id.UserID != p.UserID || id.TokenID != p.TokenID {
		return enrollResult{}, refuse(enrollExitFinal, "identity_mismatch: the hub reports user %s for the stored credential, not %s", id.UserID, p.UserID)
	}
	return done("enrolled", id.UserID, id.TokenID)
}

type enrollTrustField struct {
	SPKI        string `json:"spki_sha256,omitempty"`
	CAPEM       string `json:"ca_pem,omitempty"`
	SystemRoots bool   `json:"system_roots,omitempty"`
}

type enrollRecordIn struct {
	Kind      string    `json:"kind"`
	Version   int       `json:"version"`
	BundleID  string    `json:"bundle_id"`
	Purpose   string    `json:"purpose"`
	Subcode   string    `json:"subcode"`
	ExpiresAt time.Time `json:"expires_at"`
	Hub       struct {
		HubID string           `json:"hub_id"`
		URL   string           `json:"url"`
		Trust enrollTrustField `json:"trust"`
	} `json:"hub"`
}

func parseEnrollRecord(raw []byte) (enrollRecordIn, error) {
	var rec enrollRecordIn
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return rec, err
	}
	u, err := url.Parse(rec.Hub.URL)
	trusts := 0
	for _, set := range []bool{rec.Hub.Trust.SPKI != "", rec.Hub.Trust.CAPEM != "", rec.Hub.Trust.SystemRoots} {
		if set {
			trusts++
		}
	}
	switch {
	case rec.Kind != "aimem-enrollment" || rec.Version != 1:
		return rec, errors.New("not an aimem enrollment record of version 1")
	case rec.Purpose != "new_user":
		return rec, fmt.Errorf("purpose %q is not supported", rec.Purpose)
	case !enrollBundleShape.MatchString(rec.BundleID) || !enrollSubcodeShape.MatchString(rec.Subcode) || rec.Hub.HubID == "":
		return rec, errors.New("the bundle ID, subcode or hub ID has the wrong shape")
	case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil:
		return rec, errors.New("the hub URL must be https://HOST:PORT")
	case trusts != 1:
		return rec, errors.New("the hub trust must name exactly one of spki_sha256, ca_pem or system_roots")
	}
	return rec, nil
}

// enrollHTTPClient trusts the hub only as the record says: never on first
// use, never without verification, never through a proxy or a redirect.
func enrollHTTPClient(t enrollTrustField) (*http.Client, error) {
	var pem []byte
	if t.CAPEM != "" {
		pem = []byte(t.CAPEM)
	}
	cfg, err := hubTLSConfig(pem, t.SPKI)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: cfg, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// trustFailure reports whether err is the hub failing the record's trust.
func trustFailure(err error) bool {
	var verify *tls.CertificateVerificationError
	return errors.Is(err, errHubPin) || errors.As(err, &verify)
}

type enrollAnswer struct {
	BundleID string `json:"bundle_id"`
	Replayed bool   `json:"replayed"`
	HubID    string `json:"hub_id"`
	Identity struct {
		UserID  string `json:"user_id"`
		TokenID string `json:"token_id"`
	} `json:"identity"`
	Delivery struct {
		Suite      string `json:"suite"`
		Enc        string `json:"enc"`
		Ciphertext string `json:"ciphertext"`
	} `json:"delivery"`
}

// enrollSend sends the redemption, retrying a lost reply or a retryable
// refusal with the same key: the hub replays a committed redemption.
func enrollSend(client *http.Client, rec enrollRecordIn, label, k1 string, priv *ecdh.PrivateKey, until time.Time) (enrollAnswer, error) {
	body, err := json.Marshal(map[string]any{"hub_id": rec.Hub.HubID, "subcode": rec.Subcode, "label": label,
		"delivery": map[string]string{"suite": enrollment.Suite, "public_key": enrollment.EncodePublicKey(priv.PublicKey())}})
	if err != nil {
		return enrollAnswer{}, err
	}
	endpoint := strings.TrimRight(rec.Hub.URL, "/") + "/v1/identity/enrollments/redemptions"
	var last error
	retryable := false
	for attempt := range 5 {
		if attempt > 0 {
			time.Sleep(enrollBackoff(attempt - 1))
			if !until.IsZero() && !time.Now().Before(until) {
				// Stop retrying a first redemption at the expiry. An earlier
				// attempt may have reached the hub, so the outcome is
				// unknown: the rerun is a recovery, which the hub replays.
				return enrollAnswer{}, refuse(enrollExitUnknown, "the onboarding code expired while retrying (%v); run the same command again: it recovers a redemption that reached the hub", last)
			}
		}
		req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
		if err != nil {
			return enrollAnswer{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Aimem-Enrollment-Version", "1")
		req.Header.Set("Idempotency-Key", k1)
		resp, err := client.Do(req)
		if err != nil {
			if trustFailure(err) {
				return enrollAnswer{}, refuse(enrollExitFinal, "the hub at %s failed the record's trust check; nothing was sent", rec.Hub.URL)
			}
			last, retryable = err, false
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if rerr != nil {
			last, retryable = rerr, false
			continue
		}
		if resp.StatusCode == http.StatusOK {
			var ans enrollAnswer
			if err := json.Unmarshal(data, &ans); err != nil {
				return enrollAnswer{}, refuse(enrollExitUnknown, "the hub's answer is unreadable; run the same command again")
			}
			return ans, nil
		}
		var env struct {
			Code       string `json:"code"`
			Retryable  bool   `json:"retryable"`
			NextAction string `json:"next_action"`
		}
		json.Unmarshal(data, &env)
		if env.Retryable || resp.StatusCode >= 500 {
			last, retryable = fmt.Errorf("hub answered %d %s", resp.StatusCode, env.Code), true
			continue
		}
		if env.Code == "" {
			env.Code = http.StatusText(resp.StatusCode)
		}
		return enrollAnswer{}, refuse(enrollExitFinal, "%s: %s", env.Code, env.NextAction)
	}
	if retryable {
		return enrollAnswer{}, refuse(enrollExitRetry, "%v; run the same command again later", last)
	}
	return enrollAnswer{}, refuse(enrollExitUnknown, "no answer from the hub (%v); the redemption may have happened: run the same command again", last)
}

// errCredentialRefused is the hub refusing a credential (401 or 403), as
// opposed to a check that could not finish.
var errCredentialRefused = errors.New("the hub refused the credential")

type enrollIdentity struct {
	UserID  string `json:"user_id"`
	TokenID string `json:"token_id"`
	Scope   string `json:"scope"`
}

// enrollWhoAmI asks the hub whose credential bearer is.
func enrollWhoAmI(client *http.Client, hubURL, bearer string) (enrollIdentity, error) {
	req, err := http.NewRequest("GET", strings.TrimRight(hubURL, "/")+"/v1/access/identity", nil)
	if err != nil {
		return enrollIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	if err != nil {
		return enrollIdentity{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return enrollIdentity{}, fmt.Errorf("%w (HTTP %d)", errCredentialRefused, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return enrollIdentity{}, fmt.Errorf("the hub answered %d", resp.StatusCode)
	}
	var id enrollIdentity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&id); err != nil || id.UserID == "" || id.Scope != "user" {
		return enrollIdentity{}, errors.New("the hub's identity answer is not a user-scoped credential")
	}
	return id, nil
}

func loadEnrollPending(path string) (*enrollPending, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		// Created and not yet written: either another run is writing it now,
		// or a run stopped before it sent anything. Neither holds keys.
		return nil, nil
	}
	if err := privatefile.Check(path); err != nil {
		return nil, fmt.Errorf("the pending state %s is not private: %v", path, err)
	}
	var p enrollPending
	if err := json.Unmarshal(raw, &p); err != nil || (p.Phase != "sending" && p.Phase != "delivered") {
		return nil, fmt.Errorf("the pending state %s is damaged", path)
	}
	return &p, nil
}

func newEnrollPending(path, bundle string) (*enrollPending, error) {
	key, err := enrollment.NewKey()
	if err != nil {
		return nil, err
	}
	var rk [32]byte
	if _, err := rand.Read(rk[:]); err != nil {
		return nil, err
	}
	p := &enrollPending{BundleID: bundle, Phase: "sending", RequestKey: hex.EncodeToString(rk[:]),
		PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()), CreatedAt: time.Now().UTC()}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// The first state is published exclusively: when two runs start at
	// once, one creates it and the other resumes with the winner's keys, so
	// the keys of a redemption that reached the hub are never overwritten.
	f, err := privatefile.Create(path)
	if errors.Is(err, os.ErrExist) {
		// The winner may still be writing it (on Windows its handle shares
		// nothing until closed): wait briefly for its keys.
		for range 20 {
			if won, err := loadEnrollPending(path); err == nil && won != nil {
				return won, nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return nil, errors.New("another run is creating the pending state; run the same command again")
	}
	if err != nil {
		return nil, err
	}
	_, werr := f.Write(raw)
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return nil, werr
	}
	return p, nil
}

// writeEnrollPending replaces the pending state atomically with an
// owner-only file: a crash leaves one phase or the other, never a mix.
func writeEnrollPending(path string, p *enrollPending) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	f, err := privatefile.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeFileAtomic writes a non-secret file (a hub's CA bundle) atomically.
func writeFileAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
