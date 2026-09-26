// Package introspect is aimem's client for aicrew's session introspection,
// identity.v1 §3 (docs/DESIGN-AIFORGE-IDENTITY-WIRE.md).
//
// One call is one attempt: at most 2 s for connect, TLS and the reply, no
// retry, no redirect, no proxy, and no cached answer. The peer's TLS identity
// is verified against its registered binding, the reply must answer this
// call's nonce, and every field is checked before a context is returned.
// Any failure is a *Failure whose Code is the contract's refusal code; its
// Reason is a fixed word for the audit and the operator check. Neither ever
// carries the credential or the handle.
package introspect

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"aimem/internal/privatefile"
)

const (
	// Path is the aicrew-owned route; a registered endpoint is the full URL
	// of it.
	Path          = "/v1/crew/introspect"
	VersionHeader = "X-Aimem-Identity-Version"
	// Budget bounds one call: connect, TLS, request and the whole reply.
	Budget = 2 * time.Second
	// MaxReply is the largest reply body read; a longer one is refused
	// without being parsed.
	MaxReply = 16384
	// maxCredential bounds the credential file; aicrew's bearer is one line.
	maxCredential = 4096
)

// Contract codes of a refused introspection.
const (
	CodeUnavailable = "context_unavailable"
	CodeStale       = "context_stale"
)

var (
	handleShape     = regexp.MustCompile(`^acs1_[A-Za-z0-9_-]{43}$`)
	idShape         = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	generationShape = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	// roles are aicrew's member roles (its store's Role values).
	roles = map[string]bool{"coordinator": true, "worker": true, "independent": true}

	errPinMismatch = errors.New("peer certificate does not match the registered SPKI pin")
)

// ValidHandle reports whether h has the shape of an aimem-scoped session
// handle. It says nothing about whether aicrew considers it active.
func ValidHandle(h string) bool { return handleShape.MatchString(h) }

// NewHandle returns a random, well-formed handle that no session holds; the
// operator check presents it and expects an inactive answer.
func NewHandle() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "acs1_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Peer is the registered aicrew service as the hub stores it. HubID is this
// hub's ID; the caller passes only a peer registered for this hub.
type Peer struct {
	ServiceID string
	HubID     string
	Endpoint  string
	TLSMode   string // ca_dns or spki_sha256
	TLSValue  string
}

// Context is a verified active session, exactly as aicrew reported it.
type Context struct {
	ServiceID       string
	HubID           string
	UserID          string
	TokenID         string
	AgentID         string
	TeamID          string
	Role            string
	SessionID       string
	Generation      string
	HandleExpiresAt time.Time
}

// Failure is a refused introspection. Code is CodeUnavailable or CodeStale;
// Reason is one fixed word naming the check that failed.
type Failure struct {
	Code   string
	Reason string
}

func (f *Failure) Error() string { return f.Code + " (" + f.Reason + ")" }

func unavailable(reason string) *Failure { return &Failure{CodeUnavailable, reason} }

// Client calls one peer's introspection route.
type Client struct {
	// TokenFile names the private file holding aimem's introspection bearer
	// (AIMEM_INTROSPECTION_TOKEN_FILE). It is read on every call, so a
	// replaced file takes effect on the next one.
	TokenFile string
	// RootCAs, when set, replaces the system roots for ca_dns trust. Tests
	// set it; the hub leaves it nil.
	RootCAs *x509.CertPool
	// Now is the hub clock that judges handle expiry; nil means time.Now.
	Now func() time.Time
}

// ReadCredential returns the bearer held in path, which must be a private
// file (privatefile.Check) holding one line of visible ASCII. Errors never
// quote the file's content.
func ReadCredential(path string) (string, error) {
	if path == "" {
		return "", errors.New("no introspection credential file is configured")
	}
	if err := privatefile.Check(path); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCredential+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxCredential {
		return "", fmt.Errorf("%s is larger than a credential", path)
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", fmt.Errorf("%s must hold the credential alone on one line", path)
		}
	}
	return s, nil
}

// CheckPeer validates the registered endpoint and trust binding without any
// network call. The endpoint must be an https URL of the introspection route.
func CheckPeer(p Peer) error {
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the introspection endpoint must be an https URL without credentials, query or fragment")
	}
	if u.Path != Path {
		return fmt.Errorf("the introspection endpoint's path must be %s", Path)
	}
	if !idShape.MatchString(p.ServiceID) || p.HubID == "" {
		return errors.New("the peer record is incomplete")
	}
	switch p.TLSMode {
	case "ca_dns":
		if p.TLSValue != u.Hostname() {
			return errors.New("ca_dns trust must name the endpoint host")
		}
	case "spki_sha256":
		if _, err := decodePin(p.TLSValue); err != nil {
			return err
		}
	default:
		return errors.New("the TLS trust mode must be ca_dns or spki_sha256")
	}
	return nil
}

func decodePin(v string) ([]byte, error) {
	b64, ok := strings.CutPrefix(v, "sha256-")
	pin, err := base64.StdEncoding.DecodeString(b64)
	if !ok || err != nil || len(pin) != sha256.Size {
		return nil, errors.New("spki_sha256 trust must be sha256- followed by a base64 SHA-256")
	}
	return pin, nil
}

// Operational reports, without any network call, whether an introspection of
// p could be attempted: a valid peer record and a usable credential file.
// The reason is a fixed word when it cannot.
func (c *Client) Operational(p Peer) (bool, string) {
	if err := CheckPeer(p); err != nil {
		return false, "peer_record"
	}
	if c.TokenFile == "" {
		return false, "not_configured"
	}
	if _, err := ReadCredential(c.TokenFile); err != nil {
		return false, "credential_file"
	}
	return true, ""
}

func (c *Client) tlsConfig(p Peer) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch p.TLSMode {
	case "ca_dns":
		cfg.RootCAs, cfg.ServerName = c.RootCAs, p.TLSValue
	case "spki_sha256":
		pin, err := decodePin(p.TLSValue)
		if err != nil {
			return nil, err
		}
		// The chain check is replaced by the pin, never dropped: the
		// handshake fails unless the leaf's public key hashes to it.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errPinMismatch
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
				return errPinMismatch
			}
			return nil
		}
	default:
		return nil, errors.New("unknown TLS trust mode")
	}
	return cfg, nil
}

type request struct {
	Version int    `json:"version"`
	HubID   string `json:"hub_id"`
	Nonce   string `json:"nonce"`
	Handle  string `json:"handle"`
}

// reply holds every field either reply may carry. Pointers tell a missing
// (or null) field from an empty one.
type reply struct {
	Nonce     *string `json:"nonce"`
	Active    *bool   `json:"active"`
	ServiceID *string `json:"service_id"`
	HubID     *string `json:"hub_id"`
	Identity  *struct {
		UserID  *string `json:"user_id"`
		TokenID *string `json:"token_id"`
	} `json:"identity"`
	AgentID         *string `json:"agent_id"`
	TeamID          *string `json:"team_id"`
	Role            *string `json:"role"`
	SessionID       *string `json:"session_id"`
	Generation      *string `json:"generation"`
	HandleExpiresAt *string `json:"handle_expires_at"`
}

// Introspect asks p about handle and returns the verified active context, or
// a *Failure. ctx is the caller's: its cancellation ends the call at once.
func (c *Client) Introspect(ctx context.Context, p Peer, handle string) (Context, error) {
	if !ValidHandle(handle) {
		return Context{}, unavailable("handle_shape")
	}
	if err := CheckPeer(p); err != nil {
		return Context{}, unavailable("peer_record")
	}
	bearer, err := ReadCredential(c.TokenFile)
	if err != nil {
		if c.TokenFile == "" {
			return Context{}, unavailable("not_configured")
		}
		return Context{}, unavailable("credential_file")
	}
	cfg, err := c.tlsConfig(p)
	if err != nil {
		return Context{}, unavailable("peer_record")
	}
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return Context{}, unavailable("nonce")
	}
	nonce := "n-" + hex.EncodeToString(nb[:])
	body, err := json.Marshal(request{Version: 1, HubID: p.HubID, Nonce: nonce, Handle: handle})
	if err != nil {
		return Context{}, unavailable("request")
	}

	callCtx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, p.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Context{}, unavailable("request")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(VersionHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := newHTTPClient(cfg).Do(req)
	if err != nil {
		return Context{}, transportFailure(ctx, callCtx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxReply+1))
	if err != nil {
		return Context{}, transportFailure(ctx, callCtx, err)
	}
	if len(data) > MaxReply {
		return Context{}, unavailable("oversize")
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return Context{}, unavailable("redirect")
	case resp.StatusCode == http.StatusBadRequest && refusalCode(data) == "unsupported_version":
		return Context{}, unavailable("version_rejected")
	case resp.StatusCode != http.StatusOK:
		return Context{}, unavailable("status")
	}
	return c.verify(p, nonce, data)
}

// newHTTPClient is one call's client: no proxy (the proxy environment is
// ignored), no redirect, no connection reuse.
func newHTTPClient(cfg *tls.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil, TLSClientConfig: cfg, DisableKeepAlives: true,
			ForceAttemptHTTP2: false, MaxResponseHeaderBytes: MaxReply,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// transportFailure classifies a failed exchange: the caller's cancellation,
// the 2 s budget, the peer's TLS identity, or any other transport error.
func transportFailure(caller, call context.Context, err error) *Failure {
	var verr *tls.CertificateVerificationError
	var herr x509.HostnameError
	var uerr x509.UnknownAuthorityError
	switch {
	case caller.Err() != nil:
		return unavailable("cancelled")
	case call.Err() != nil:
		return unavailable("timeout")
	case errors.Is(err, errPinMismatch), errors.As(err, &verr), errors.As(err, &herr), errors.As(err, &uerr):
		return unavailable("tls_untrusted")
	}
	return unavailable("transport")
}

func refusalCode(data []byte) string {
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(data, &body) != nil {
		return ""
	}
	return body.Code
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// verify parses one reply and applies every §3 check, in order: the nonce
// binds the reply to this call, then the inactive or active form, then the
// active reply's peer, hub, identity, generation, expiry and role.
func (c *Client) verify(p Peer, nonce string, data []byte) (Context, error) {
	var r reply
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Context{}, unavailable("malformed")
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return Context{}, unavailable("malformed")
	}
	if r.Nonce == nil || subtle.ConstantTimeCompare([]byte(*r.Nonce), []byte(nonce)) != 1 {
		return Context{}, unavailable("nonce_mismatch")
	}
	if r.Active == nil {
		return Context{}, unavailable("malformed")
	}
	if !*r.Active {
		if r.ServiceID != nil || r.HubID != nil || r.Identity != nil || r.AgentID != nil || r.TeamID != nil ||
			r.Role != nil || r.SessionID != nil || r.Generation != nil || r.HandleExpiresAt != nil {
			return Context{}, unavailable("malformed")
		}
		return Context{}, &Failure{CodeStale, "inactive"}
	}
	if r.Identity == nil {
		return Context{}, unavailable("malformed")
	}
	got := Context{
		ServiceID: str(r.ServiceID), HubID: str(r.HubID), UserID: str(r.Identity.UserID), TokenID: str(r.Identity.TokenID),
		AgentID: str(r.AgentID), TeamID: str(r.TeamID), Role: str(r.Role), SessionID: str(r.SessionID), Generation: str(r.Generation),
	}
	for _, id := range []string{got.ServiceID, got.HubID, got.UserID, got.TokenID, got.AgentID, got.TeamID, got.SessionID} {
		if !idShape.MatchString(id) {
			return Context{}, unavailable("malformed")
		}
	}
	if got.ServiceID != p.ServiceID {
		return Context{}, unavailable("wrong_service")
	}
	if got.HubID != p.HubID {
		return Context{}, unavailable("wrong_hub")
	}
	if !generationShape.MatchString(got.Generation) {
		return Context{}, unavailable("bad_generation")
	}
	exp, err := time.Parse(time.RFC3339, str(r.HandleExpiresAt))
	if err != nil {
		return Context{}, unavailable("malformed")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	// Judged by the hub clock with no allowance: a handle aicrew still calls
	// active is stale here once the hub passes its expiry, and the client
	// refreshes it through aicrew.
	if !exp.After(now()) {
		return Context{}, &Failure{CodeStale, "expired"}
	}
	if !roles[got.Role] {
		return Context{}, unavailable("bad_role")
	}
	got.HandleExpiresAt = exp.UTC()
	return got, nil
}
