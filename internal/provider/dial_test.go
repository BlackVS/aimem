package provider

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBlockedIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "fe80::1", "fd00::1", "0.0.0.0", "0.1.2.3", "::", "224.0.0.1"} {
		if !BlockedIP(net.ParseIP(s)) {
			t.Errorf("%s not blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if BlockedIP(net.ParseIP(s)) {
			t.Errorf("%s blocked", s)
		}
	}
}

// A provider call to a loopback server is refused at dial time without
// the flag, and served with it.
func TestHTTPClientRefusesPrivateUnlessAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()

	t.Setenv(AllowPrivateEnv, "")
	_, err := HTTPClient(5*time.Second, false).Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), AllowPrivateEnv) {
		t.Fatalf("loopback call without the flag: %v", err)
	}

	t.Setenv(AllowPrivateEnv, "1")
	resp, err := HTTPClient(5*time.Second, false).Get(srv.URL)
	if err != nil {
		t.Fatalf("loopback call with the flag: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// The host's own AIMEM_OPENAI_* pair is not an admin-settable endpoint:
// its calls dial without the guard, flag or not.
func TestHTTPClientLeavesTheEnvPairUnguarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	t.Setenv(AllowPrivateEnv, "")
	resp, err := HTTPClient(5*time.Second, true).Get(srv.URL)
	if err != nil {
		t.Fatalf("env-pair call to loopback: %v", err)
	}
	resp.Body.Close()
}
