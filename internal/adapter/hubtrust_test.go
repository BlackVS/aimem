package adapter

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trustServer is a TLS hub with a private (self-signed) certificate: its
// CA file and its pin.
func trustServer(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "hub-ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	return srv, ca, "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func get(h *HubConfig) error {
	resp, err := h.HTTPClient().Get(h.URL)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// Every trust the hub entry can hold, through the client every personal
// path uses (HTTPClient): a private CA and a pin are trusted; the system
// roots, a wrong pin, an unusable CA file and two trusts at once are not,
// and an unusable trust names itself instead of falling back.
func TestHubTrust(t *testing.T) {
	srv, ca, pin := trustServer(t)
	ok := map[string]*HubConfig{
		"ca_file":          {URL: srv.URL, CAFile: ca},
		"pin":              {URL: srv.URL, Pin: pin},
		"insecure":         {URL: srv.URL, Insecure: true},
		"ca_file+insecure": {URL: srv.URL, CAFile: ca, Insecure: true},
	}
	for name, h := range ok {
		if err := get(h); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "absent.pem")
	notPEM := filepath.Join(t.TempDir(), "not.pem")
	os.WriteFile(notPEM, []byte("not a certificate"), 0o600)
	wrong := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	refused := map[string]struct {
		h    *HubConfig
		want string
	}{
		"system roots":       {&HubConfig{URL: srv.URL}, "certificate"},
		"wrong pin":          {&HubConfig{URL: srv.URL, Pin: wrong}, "does not match the hub's pin"},
		"bad pin":            {&HubConfig{URL: srv.URL, Pin: "sha1-x"}, "the hub's pin"},
		"unreadable ca_file": {&HubConfig{URL: srv.URL, CAFile: missing}, missing},
		"ca_file not PEM":    {&HubConfig{URL: srv.URL, CAFile: notPEM}, notPEM + " holds no PEM"},
		"both":               {&HubConfig{URL: srv.URL, CAFile: ca, Pin: pin}, "both ca_file and pin"},
		// A broken trust never falls back to skipping verification.
		"unreadable ca_file, insecure": {&HubConfig{URL: srv.URL, CAFile: missing, Insecure: true}, missing},
	}
	for name, c := range refused {
		if err := get(c.h); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v (want %q)", name, err, c.want)
		}
	}
}

// A ca_file fixed after a failure is picked up: failures are not cached.
func TestHubTrustFailureNotCached(t *testing.T) {
	srv, ca, _ := trustServer(t)
	later := filepath.Join(t.TempDir(), "later.pem")
	h := &HubConfig{URL: srv.URL, CAFile: later}
	if err := get(h); err == nil {
		t.Fatal("an absent ca_file was trusted")
	}
	raw, _ := os.ReadFile(ca)
	os.WriteFile(later, raw, 0o600)
	if err := get(h); err != nil {
		t.Fatalf("the fixed ca_file: %v", err)
	}
}

// A re-add of the same host keeps the recorded trust unless a new one is
// given; a re-pointed hub inherits none.
func TestHubTrustOver(t *testing.T) {
	prev := &HubConfig{URL: "https://hub.example", Token: "a", CAFile: "/ca.pem"}
	if got := (&HubConfig{URL: "https://hub.example", Token: "b"}).Over(prev); got.CAFile != "/ca.pem" || got.Pin != "" {
		t.Fatalf("kept: %+v", got)
	}
	if got := (&HubConfig{URL: "https://hub.example", Pin: "sha256-x"}).Over(prev); got.CAFile != "" || got.Pin != "sha256-x" {
		t.Fatalf("replaced: %+v", got)
	}
	if got := (&HubConfig{URL: "https://other.example"}).Over(prev); got.CAFile != "" || got.Pin != "" {
		t.Fatalf("re-pointed: %+v", got)
	}
}
