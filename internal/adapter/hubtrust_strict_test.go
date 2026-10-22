package adapter

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func strictGet(h *HubConfig) error {
	c, err := h.StrictClient(5 * time.Second)
	if err != nil {
		return err
	}
	resp, err := c.Get(h.URL)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// The strict client reaches a hub only over verified https: a private CA
// or a pin is trusted; plain http (loopback included), insecure (even with
// a pin beside it), a wrong pin and the system roots are not. An entry it
// cannot use (http, insecure, a trust that does not load) is refused with
// ErrUntrustedHub before any client exists.
func TestStrictClientTrust(t *testing.T) {
	srv, ca, pin := trustServer(t)
	for name, h := range map[string]*HubConfig{
		"ca_file": {URL: srv.URL, CAFile: ca},
		"pin":     {URL: srv.URL, Pin: pin},
	} {
		if err := strictGet(h); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	for name, tc := range map[string]struct {
		h    *HubConfig
		want string
	}{
		"plain http on loopback": {&HubConfig{URL: plain.URL, Pin: pin}, "only over https"},
		"no host":                {&HubConfig{URL: "https://"}, "only over https"},
		"insecure":               {&HubConfig{URL: srv.URL, Insecure: true}, "configured with insecure"},
		"insecure with a pin":    {&HubConfig{URL: srv.URL, Insecure: true, Pin: pin}, "configured with insecure"},
	} {
		_, err := tc.h.StrictClient(5 * time.Second)
		if !errors.Is(err, ErrUntrustedHub) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "aimem hub list") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	missing := filepath.Join(t.TempDir(), "absent.pem")
	if _, err := (&HubConfig{URL: srv.URL, CAFile: missing}).StrictClient(5 * time.Second); !errors.Is(err, ErrUntrustedHub) ||
		!errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "absent.pem") {
		t.Fatalf("unreadable ca_file: %v", err)
	}
	wrong := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	for name, h := range map[string]*HubConfig{
		"wrong pin":    {URL: srv.URL, Pin: wrong},
		"system roots": {URL: srv.URL},
	} {
		if err := strictGet(h); err == nil || errors.Is(err, ErrUntrustedHub) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// No proxy and no redirect: the credential goes to the recorded URL or
// nowhere. The proxy is checked on the transport itself, since net/http
// never proxies a loopback test server and reads the environment once.
func TestStrictClientNoProxyNoRedirect(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer target.Close()
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer hub.Close()
	_, _, pin := trustServer(t)
	c, err := (&HubConfig{URL: hub.URL, Pin: pin}).StrictClient(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(hub.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || reached.Load() != 0 {
		t.Fatalf("redirect followed: status %d, target reached %d", resp.StatusCode, reached.Load())
	}
	if tr, ok := c.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("the strict transport takes a proxy")
	}
}
