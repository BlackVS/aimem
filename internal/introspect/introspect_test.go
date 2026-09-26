package introspect

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aimem/internal/introspect/introspecttest"
	"aimem/internal/privatefile"
)

const (
	testBearer  = "aicrew_introspect_TESTSECRET_0123456789"
	testService = "aicrew-example"
	testHub     = "hub-0001"
)

type fake = introspecttest.Fake

func newFake(t *testing.T) *fake { return introspecttest.New(t, testService, testHub) }

// peerOf is the fake registered with the given trust mode.
func peerOf(f *fake, mode string) Peer {
	p := Peer{ServiceID: testService, HubID: testHub, Endpoint: f.Endpoint(), TLSMode: mode}
	if mode == "ca_dns" {
		p.TLSValue = "127.0.0.1"
	} else {
		p.TLSValue = f.Pin
	}
	return p
}

func activeReply(nonce string) map[string]any {
	return introspecttest.ActiveReply(nonce, testService, testHub)
}

func credentialFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "introspection.token")
	f, err := privatefile.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}

func newClient(t *testing.T, f *fake) *Client {
	return &Client{TokenFile: credentialFile(t, testBearer+"\n"), RootCAs: f.CA.Pool}
}

func handle(t *testing.T) string {
	h, err := NewHandle()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func wantFailure(t *testing.T, err error, code, reason string) {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) || f.Code != code || f.Reason != reason {
		t.Fatalf("got %v, want %s (%s)", err, code, reason)
	}
}

func TestIntrospectActiveBothTrustModes(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	for _, mode := range []string{"ca_dns", "spki_sha256"} {
		h := handle(t)
		got, err := c.Introspect(context.Background(), peerOf(f, mode), h)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		want := Context{ServiceID: testService, HubID: testHub, UserID: "user-1", TokenID: "tok-1", AgentID: "agent-1",
			TeamID: "team-1", Role: "worker", SessionID: "sess-1", Generation: "4"}
		got.HandleExpiresAt = time.Time{}
		if got != want {
			t.Fatalf("%s: got %+v", mode, got)
		}
		s := f.Last()
		if s.Path != Path || s.Header.Get("Authorization") != "Bearer "+testBearer || s.Header.Get(VersionHeader) != "1" {
			t.Fatalf("%s: request path %q, headers %v", mode, s.Path, s.Header)
		}
		if s.Body.Version != 1 || s.Body.HubID != testHub || s.Body.Handle != h || !regexp.MustCompile(`^n-[0-9a-f]{32}$`).MatchString(s.Body.Nonce) {
			t.Fatalf("%s: request body %+v", mode, s.Body)
		}
	}
}

func TestIntrospectNonceIsFreshPerCall(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	h := handle(t)
	nonces := map[string]bool{}
	for i := 0; i < 5; i++ {
		if _, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), h); err != nil {
			t.Fatal(err)
		}
		nonces[f.Last().Body.Nonce] = true
	}
	if len(nonces) != 5 || f.Calls() != 5 {
		t.Fatalf("%d distinct nonces over %d calls; each call must be one fresh attempt", len(nonces), f.Calls())
	}
}

func TestIntrospectReplyChecks(t *testing.T) {
	type edit func(m map[string]any)
	set := func(k string, v any) edit { return func(m map[string]any) { m[k] = v } }
	del := func(k string) edit { return func(m map[string]any) { delete(m, k) } }
	cases := []struct {
		name         string
		edit         edit
		code, reason string
	}{
		{"nonce mismatch", set("nonce", "n-00000000000000000000000000000000"), CodeUnavailable, "nonce_mismatch"},
		{"nonce missing", del("nonce"), CodeUnavailable, "nonce_mismatch"},
		{"active missing", del("active"), CodeUnavailable, "malformed"},
		{"another service", set("service_id", "aicrew-other"), CodeUnavailable, "wrong_service"},
		{"another hub", set("hub_id", "hub-other"), CodeUnavailable, "wrong_hub"},
		{"generation zero", set("generation", "0"), CodeUnavailable, "bad_generation"},
		{"generation negative", set("generation", "-3"), CodeUnavailable, "bad_generation"},
		{"generation padded", set("generation", "04"), CodeUnavailable, "bad_generation"},
		{"generation as number", set("generation", 4), CodeUnavailable, "malformed"},
		{"expired", set("handle_expires_at", time.Now().Add(-time.Second).UTC().Format(time.RFC3339)), CodeStale, "expired"},
		{"expiry unparsable", set("handle_expires_at", "soon"), CodeUnavailable, "malformed"},
		{"unknown role", set("role", "admin"), CodeUnavailable, "bad_role"},
		{"identity missing", del("identity"), CodeUnavailable, "malformed"},
		{"token missing", set("identity", map[string]any{"user_id": "user-1"}), CodeUnavailable, "malformed"},
		{"agent missing", del("agent_id"), CodeUnavailable, "malformed"},
		{"session empty", set("session_id", ""), CodeUnavailable, "malformed"},
		{"team null", set("team_id", nil), CodeUnavailable, "malformed"},
		{"unknown field", set("grant", "all"), CodeUnavailable, "malformed"},
		{"unknown identity field", set("identity", map[string]any{"user_id": "user-1", "token_id": "tok-1", "name": "x"}), CodeUnavailable, "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
				m := activeReply(got.Nonce)
				tc.edit(m)
				introspecttest.WriteJSON(w, m)
			})
			_, err := newClient(t, f).Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
			wantFailure(t, err, tc.code, tc.reason)
		})
	}
}

func TestIntrospectExpiryUsesTheHubClock(t *testing.T) {
	f := newFake(t)
	exp := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		m := activeReply(got.Nonce)
		m["handle_expires_at"] = exp.Format(time.RFC3339)
		introspecttest.WriteJSON(w, m)
	})
	c := newClient(t, f)
	c.Now = func() time.Time { return exp }
	_, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
	wantFailure(t, err, CodeStale, "expired")
	c.Now = func() time.Time { return exp.Add(-time.Millisecond) }
	if _, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t)); err != nil {
		t.Fatalf("a handle one millisecond before expiry was refused: %v", err)
	}
}

func TestIntrospectInactive(t *testing.T) {
	f := newFake(t)
	f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, map[string]any{"nonce": got.Nonce, "active": false})
	})
	c := newClient(t, f)
	_, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
	wantFailure(t, err, CodeStale, "inactive")
	// An inactive reply carries nothing else: a reason or IDs make it untrusted.
	f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, map[string]any{"nonce": got.Nonce, "active": false, "service_id": testService})
	})
	_, err = c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
	wantFailure(t, err, CodeUnavailable, "malformed")
	f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, map[string]any{"nonce": "n-ffffffffffffffffffffffffffffffff", "active": false})
	})
	_, err = c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
	wantFailure(t, err, CodeUnavailable, "nonce_mismatch")
}

func TestIntrospectReplyFormAndSize(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	valid := func(nonce string) []byte {
		raw, _ := json.Marshal(activeReply(nonce))
		return raw
	}
	pad := func(raw []byte, size int) []byte {
		return append(raw, []byte(strings.Repeat(" ", size-len(raw)))...)
	}
	for _, tc := range []struct {
		name   string
		body   func(nonce string) []byte
		reason string
	}{
		{"trailing data", func(n string) []byte { return append(valid(n), []byte(`{}`)...) }, "malformed"},
		{"not JSON", func(string) []byte { return []byte("active") }, "malformed"},
		{"one byte over the cap", func(n string) []byte { return pad(valid(n), MaxReply+1) }, "oversize"},
		{"far over the cap", func(string) []byte { return []byte(strings.Repeat("x", 1<<20)) }, "oversize"},
	} {
		f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) { w.Write(tc.body(got.Nonce)) })
		_, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
		wantFailure(t, err, CodeUnavailable, tc.reason)
	}
	f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) { w.Write(pad(valid(got.Nonce), MaxReply)) })
	if _, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t)); err != nil {
		t.Fatalf("a reply of exactly %d bytes was refused: %v", MaxReply, err)
	}
}

func TestIntrospectStatusAndRedirect(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	var followed atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
	defer target.Close()
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, got introspecttest.Request)
		reason string
	}{
		{"version refused", func(w http.ResponseWriter, _ introspecttest.Request) {
			w.WriteHeader(http.StatusBadRequest)
			introspecttest.WriteJSON(w, map[string]string{"code": "unsupported_version"})
		}, "version_rejected"},
		{"other 400", func(w http.ResponseWriter, _ introspecttest.Request) { w.WriteHeader(http.StatusBadRequest) }, "status"},
		{"server error", func(w http.ResponseWriter, got introspecttest.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			introspecttest.WriteJSON(w, activeReply(got.Nonce))
		}, "status"},
		{"redirect", func(w http.ResponseWriter, _ introspecttest.Request) {
			w.Header().Set("Location", target.URL+Path)
			w.WriteHeader(http.StatusTemporaryRedirect)
		}, "redirect"},
	} {
		f.SetAnswer(tc.answer)
		_, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
		wantFailure(t, err, CodeUnavailable, tc.reason)
	}
	if followed.Load() != 0 {
		t.Fatal("a redirect was followed")
	}
}

func TestIntrospectTLSTrust(t *testing.T) {
	f := newFake(t)
	other := newFake(t)
	c := newClient(t, f)
	// ca_dns: a CA the client does not trust.
	c.RootCAs = other.CA.Pool
	_, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
	wantFailure(t, err, CodeUnavailable, "tls_untrusted")
	// ca_dns: a trusted CA and a DNS name the certificate carries.
	c.RootCAs = f.CA.Pool
	p := peerOf(f, "ca_dns")
	p.Endpoint = strings.Replace(p.Endpoint, "127.0.0.1", "localhost", 1)
	p.TLSValue = "localhost"
	if _, err := c.Introspect(context.Background(), p, handle(t)); err != nil {
		t.Fatalf("the certificate names localhost: %v", err)
	}
	// spki_sha256: another server's pin. The system roots never rescue a pin.
	p = peerOf(f, "spki_sha256")
	p.TLSValue = other.Pin
	_, err = c.Introspect(context.Background(), p, handle(t))
	wantFailure(t, err, CodeUnavailable, "tls_untrusted")
	if f.Calls() != 1 {
		t.Fatalf("the untrusted peer received %d requests; only the trusted call may reach it", f.Calls())
	}
}

func TestIntrospectPeerRecordRefusedBeforeAnyCall(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	var plainCalls atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { plainCalls.Add(1) }))
	defer plain.Close()
	for name, edit := range map[string]func(p *Peer){
		"plain http":      func(p *Peer) { p.Endpoint = plain.URL + Path },
		"other path":      func(p *Peer) { p.Endpoint = f.Srv.URL + "/v1/other" },
		"base URL only":   func(p *Peer) { p.Endpoint = f.Srv.URL },
		"query":           func(p *Peer) { p.Endpoint += "?x=1" },
		"no trust":        func(p *Peer) { p.TLSMode = "" },
		"dns mismatch":    func(p *Peer) { p.TLSValue = "aicrew.example" },
		"pin malformed":   func(p *Peer) { p.TLSMode, p.TLSValue = "spki_sha256", "sha256-short" },
		"service missing": func(p *Peer) { p.ServiceID = "" },
	} {
		p := peerOf(f, "ca_dns")
		edit(&p)
		_, err := c.Introspect(context.Background(), p, handle(t))
		wantFailure(t, err, CodeUnavailable, "peer_record")
		if ok, _ := c.Operational(p); ok {
			t.Errorf("%s: reported operational", name)
		}
	}
	for _, h := range []string{"", "acs1_short", "amr1_" + strings.Repeat("A", 43), handle(t) + "x"} {
		_, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), h)
		wantFailure(t, err, CodeUnavailable, "handle_shape")
	}
	if f.Calls() != 0 || plainCalls.Load() != 0 {
		t.Fatalf("%d requests left the hub for an invalid peer or handle", f.Calls()+int(plainCalls.Load()))
	}
}

func TestIntrospectBudgetAndCancellation(t *testing.T) {
	f := newFake(t)
	release := make(chan struct{})
	defer close(release)
	f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	})
	c := newClient(t, f)
	start := time.Now()
	_, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
	wantFailure(t, err, CodeUnavailable, "timeout")
	if d := time.Since(start); d < Budget || d > Budget+time.Second {
		t.Fatalf("a silent peer was abandoned after %v; the budget is %v", d, Budget)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	start = time.Now()
	_, err = c.Introspect(ctx, peerOf(f, "ca_dns"), handle(t))
	wantFailure(t, err, CodeUnavailable, "cancelled")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancellation took %v to end the call", d)
	}
	// A peer that stalls midway through its reply is bounded too.
	f.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		w.Write([]byte(`{"nonce":`))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	})
	start = time.Now()
	_, err = c.Introspect(context.Background(), peerOf(f, "ca_dns"), handle(t))
	wantFailure(t, err, CodeUnavailable, "timeout")
	if d := time.Since(start); d > Budget+time.Second {
		t.Fatalf("a stalled reply held the call for %v", d)
	}
}

func TestIntrospectTransportFailure(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	p := peerOf(f, "ca_dns")
	f.Srv.Close()
	_, err := c.Introspect(context.Background(), p, handle(t))
	wantFailure(t, err, CodeUnavailable, "transport")
}

// TestIntrospectUsesNoProxy checks the transport directly: Go never proxies
// a loopback address, so a fake aicrew cannot show a proxy being used.
func TestIntrospectUsesNoProxy(t *testing.T) {
	tr, ok := newHTTPClient(&tls.Config{}).Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || !tr.DisableKeepAlives {
		t.Fatal("the introspection transport must not use a proxy or reuse connections")
	}
}

func TestIntrospectCredentialFile(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	p := peerOf(f, "ca_dns")
	if ok, why := c.Operational(p); !ok {
		t.Fatalf("not operational: %s", why)
	}
	// The file is read on every call: a replaced credential is used next.
	next := testBearer + "_ROTATED"
	os.Remove(c.TokenFile)
	fh, err := privatefile.Create(c.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(next + "\r\n")
	fh.Close()
	if _, err := c.Introspect(context.Background(), p, handle(t)); err != nil {
		t.Fatal(err)
	}
	if got := f.Last().Header.Get("Authorization"); got != "Bearer "+next {
		t.Fatalf("the replaced credential was not used: %q", got)
	}
	calls := f.Calls()
	exposed := credentialFile(t, testBearer)
	if err := privatefile.Expose(exposed); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		file, reason string
	}{
		"unset":          {"", "not_configured"},
		"missing":        {filepath.Join(t.TempDir(), "absent"), "credential_file"},
		"readable":       {exposed, "credential_file"},
		"empty":          {credentialFile(t, ""), "credential_file"},
		"two lines":      {credentialFile(t, testBearer+"\nsecond\n"), "credential_file"},
		"inner space":    {credentialFile(t, "aicrew token"), "credential_file"},
		"too large":      {credentialFile(t, strings.Repeat("a", maxCredential+1)), "credential_file"},
		"only a newline": {credentialFile(t, "\n"), "credential_file"},
	} {
		c := &Client{TokenFile: tc.file, RootCAs: f.CA.Pool}
		_, err := c.Introspect(context.Background(), p, handle(t))
		var fl *Failure
		if !errors.As(err, &fl) || fl.Reason != tc.reason {
			t.Errorf("%s: got %v, want %s", name, err, tc.reason)
		}
		if ok, why := c.Operational(p); ok || why != tc.reason {
			t.Errorf("%s: operational %v (%s)", name, ok, why)
		}
	}
	if f.Calls() != calls {
		t.Fatal("a request left the hub without a usable credential")
	}
}

func TestIntrospectFailuresCarryNoSecret(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	h := handle(t)
	var all []string
	for _, answer := range []func(w http.ResponseWriter, got introspecttest.Request){
		func(w http.ResponseWriter, got introspecttest.Request) { w.WriteHeader(http.StatusForbidden) },
		func(w http.ResponseWriter, got introspecttest.Request) { w.Write([]byte(got.Handle + testBearer)) },
		func(w http.ResponseWriter, got introspecttest.Request) {
			m := activeReply(got.Nonce)
			m["role"] = got.Handle
			introspecttest.WriteJSON(w, m)
		},
	} {
		f.SetAnswer(answer)
		_, err := c.Introspect(context.Background(), peerOf(f, "ca_dns"), h)
		if err == nil {
			t.Fatal("expected a failure")
		}
		all = append(all, err.Error())
	}
	for _, s := range all {
		if strings.Contains(s, h) || strings.Contains(s, testBearer) {
			t.Fatalf("a failure text carries a secret: %q", s)
		}
	}
}
