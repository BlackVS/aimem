package adapter

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aimem/internal/hubtest"
)

// An entry marked Strict carries an individual credential: when the strict
// client cannot be built for it, HTTPClient does not fall back to the
// ordinary client, the call fails and nothing reaches the hub.
func TestStrictEntryFailsClosed(t *testing.T) {
	var reached atomic.Int32
	count := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); w.Write([]byte(`{"docs":[]}`)) })
	plain := httptest.NewServer(count)
	defer plain.Close()
	tls := hubtest.NewServer(count)
	defer tls.Close()
	cases := map[string]*HubConfig{
		"plain http":       {URL: plain.URL, Token: "INDIVIDUAL", Strict: true},
		"insecure":         {URL: tls.URL, Token: "INDIVIDUAL", Insecure: true, Strict: true},
		"malformed pin":    {URL: tls.URL, Token: "INDIVIDUAL", Pin: "sha256-not-a-pin", Strict: true},
		"missing ca file":  {URL: tls.URL, Token: "INDIVIDUAL", CAFile: t.TempDir() + "/none.pem", Strict: true},
		"wrong pin (TLS)":  {URL: tls.URL, Token: "INDIVIDUAL", Pin: "sha256-" + strings.Repeat("A", 43) + "=", Strict: true},
		"no trust (roots)": {URL: tls.URL, Token: "INDIVIDUAL", Strict: true},
	}
	c := NewClient(t.TempDir())
	for name, hub := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.ListHubDocs(hub, "projQ"); err == nil {
				t.Fatal("the call succeeded")
			}
			if _, err := c.GetHubDoc(hub, "projQ", "RUNBOOK", 0); err == nil {
				t.Fatal("the call succeeded")
			}
			if n := reached.Load(); n != 0 {
				t.Fatalf("%d requests reached the hub", n)
			}
		})
	}
	// The same entries without the mark are the ordinary client's business:
	// the plain one is reached, which is what Strict exists to stop.
	if _, err := c.ListHubDocs(&HubConfig{URL: plain.URL, Token: "WRITER"}, "projQ"); err != nil || reached.Load() != 1 {
		t.Fatalf("the unmarked control: %v, %d requests", err, reached.Load())
	}
}

// A proxy set in the environment never sees a Strict entry's request: the
// strict client uses no proxy.
func TestStrictEntryUsesNoProxy(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	hub := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"docs":[]}`)) }))
	defer hub.Close()
	// A name the system resolver does not know: only a proxy could reach it,
	// and loopback addresses are never proxied, so the hub is named.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(hub.URL, "https://"))
	entry := &HubConfig{URL: "https://hub.invalid:" + port, Token: "INDIVIDUAL", Pin: hubtest.Pin(hub), Strict: true}
	if _, err := NewClient(t.TempDir()).ListHubDocs(entry, "projQ"); err == nil {
		t.Fatal("the request reached a hub by a name nothing resolves")
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("the proxy saw %d requests", n)
	}
}
