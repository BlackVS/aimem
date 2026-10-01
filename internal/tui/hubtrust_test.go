package tui

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"aimem/internal/adapter"
)

// The dashboard's hub health uses the hub's recorded trust.
func TestTUIClientUsesHubTrust(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"projects": 3, "resources": {}}`))
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "hub-ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	if _, projs, _, ok := fetchHubHealth(srv.URL, "t", tuiClient(&adapter.HubConfig{URL: srv.URL, CAFile: ca})); !ok || projs != 3 {
		t.Fatalf("with the ca_file: ok %v, projects %d", ok, projs)
	}
	if _, _, _, ok := fetchHubHealth(srv.URL, "t", tuiClient(&adapter.HubConfig{URL: srv.URL})); ok {
		t.Fatal("a private CA was trusted without its ca_file")
	}
}
