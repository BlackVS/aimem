package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/privatefile"
	"aimem/internal/teamsession"
)

func privateHub(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "hub-ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	return srv, ca, "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func storedHub(t *testing.T, name string) *adapter.HubConfig {
	t.Helper()
	hubs, _ := adapter.LoadHubs(stateRoot())
	return hubs[name]
}

// `aimem hub add --ca-file|--pin` records the hub's trust; a re-add without
// the flag keeps it, an explicit one replaces it, and a bad one is refused
// before anything is stored.
func TestHubAddTrust(t *testing.T) {
	t.Setenv("AIMEM_STATE_DIR", t.TempDir())
	srv, ca, pin := privateHub(t)
	if err := hubCmd([]string{"add", "pilot", srv.URL, "checkpoint", "--ca-file", ca}); err != nil {
		t.Fatal(err)
	}
	if h := storedHub(t, "pilot"); !filepath.IsAbs(h.CAFile) || h.CAFile != ca || h.Pin != "" {
		t.Fatalf("stored: %+v", h)
	}
	if err := hubCmd([]string{"add", "pilot", srv.URL, "rotated"}); err != nil {
		t.Fatal(err)
	}
	if h := storedHub(t, "pilot"); h.CAFile != ca || h.Token != "rotated" {
		t.Fatalf("a re-add without the flag must keep the trust: %+v", h)
	}
	if err := hubCmd([]string{"add", "pilot", srv.URL, "rotated", "--pin", pin}); err != nil {
		t.Fatal(err)
	}
	if h := storedHub(t, "pilot"); h.CAFile != "" || h.Pin != pin {
		t.Fatalf("an explicit --pin must replace the trust: %+v", h)
	}
	before := storedHub(t, "pilot")
	missing := filepath.Join(t.TempDir(), "absent.pem")
	notPEM := filepath.Join(t.TempDir(), "not.pem")
	os.WriteFile(notPEM, []byte("not a certificate"), 0o600)
	for args, want := range map[string]string{
		"--ca-file " + missing:              missing,
		"--ca-file " + notPEM:               notPEM + " holds no PEM certificate",
		"--ca-file " + ca + " --pin " + pin: "either --ca-file or --pin",
		"--ca-file " + ca + " --insecure":   "--insecure skips the verification",
		"--pin sha256-short":                "--pin",
	} {
		err := hubCmd(append([]string{"add", "pilot", srv.URL, "rotated"}, strings.Fields(args)...))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v (want %q)", args, err, want)
		}
	}
	if h := storedHub(t, "pilot"); *h != *before {
		t.Fatalf("a refused add changed the entry: %+v", h)
	}
}

// The other client paths to the hub use its recorded trust: the sync
// client, the team-session client (which still refuses a bare insecure
// hub) and, through HTTPClient, the MCP server, docs and the read clients.
func TestHubTrustClientPaths(t *testing.T) {
	srv, ca, pin := privateHub(t)
	call := func(c *http.Client) error {
		resp, err := c.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	for name, h := range map[string]*adapter.HubConfig{
		"ca_file": {URL: srv.URL, CAFile: ca},
		"pin":     {URL: srv.URL, Pin: pin},
	} {
		if err := call(syncHTTPClient(h)); err != nil {
			t.Fatalf("sync, %s: %v", name, err)
		}
		if err := call(h.HTTPClient()); err != nil {
			t.Fatalf("personal, %s: %v", name, err)
		}
		c, err := teamsession.HubClient(h)
		if err != nil {
			t.Fatalf("team session, %s: %v", name, err)
		}
		if err := call(c); err != nil {
			t.Fatalf("team session, %s: %v", name, err)
		}
	}
	if err := call(syncHTTPClient(&adapter.HubConfig{URL: srv.URL})); err == nil {
		t.Fatal("sync trusted a private CA without its ca_file")
	}
	if _, err := teamsession.HubClient(&adapter.HubConfig{URL: srv.URL, Insecure: true}); err == nil ||
		!strings.Contains(err.Error(), "--ca-file") {
		t.Fatalf("team session on a bare insecure hub: %v", err)
	}
	if _, err := teamsession.HubClient(&adapter.HubConfig{URL: srv.URL, CAFile: filepath.Join(t.TempDir(), "x.pem")}); err == nil ||
		!strings.Contains(err.Error(), "x.pem") {
		t.Fatalf("team session with an unreadable ca_file: %v", err)
	}
}

// writePrivate writes a file only its owner can read.
func writePrivate(t *testing.T, path, content string) {
	t.Helper()
	f, err := privatefile.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
}

// `hub add` and `hub task-token` take the token from a private file or
// standard input, so no secret goes on a command line: add, keep across a
// re-add, replace, and the refusals, none of which quotes the content.
func TestHubTokenFile(t *testing.T) {
	t.Setenv("AIMEM_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	cp := filepath.Join(dir, "checkpoint.token")
	writePrivate(t, cp, "checkpoint-secret\r\n") // a CRLF ending is dropped
	if err := hubCmd([]string{"add", "pilot", "https://hub.example", "--token-file", cp}); err != nil {
		t.Fatal(err)
	}
	if h := storedHub(t, "pilot"); h.Token != "checkpoint-secret" {
		t.Fatalf("stored token %q", h.Token)
	}
	user := filepath.Join(dir, "user.token")
	writePrivate(t, user, "aimem_user_first\n")
	if err := hubCmd([]string{"task-token", "pilot", "--token-file", user}); err != nil {
		t.Fatal(err)
	}
	// A same-host re-add, its token from standard input, keeps the task credential.
	defer func(r io.Reader) { hubStdin = r }(hubStdin)
	hubStdin = strings.NewReader("rotated-secret\n")
	if err := hubCmd([]string{"add", "pilot", "https://hub.example", "--token-file", "-"}); err != nil {
		t.Fatal(err)
	}
	if h := storedHub(t, "pilot"); h.Token != "rotated-secret" || h.TaskToken != "aimem_user_first" {
		t.Fatalf("after the re-add: %+v", h)
	}
	hubStdin = strings.NewReader("aimem_user_second")
	if err := hubCmd([]string{"task-token", "pilot", "--token-file", "-"}); err != nil {
		t.Fatal(err)
	}
	if h := storedHub(t, "pilot"); h.TaskToken != "aimem_user_second" {
		t.Fatalf("replaced: %+v", h)
	}

	before := *storedHub(t, "pilot")
	missing := filepath.Join(dir, "absent.token")
	wide := filepath.Join(dir, "wide.token")
	os.WriteFile(wide, []byte("aimem_user_wide\n"), 0o644)
	two := filepath.Join(dir, "two.token")
	writePrivate(t, two, "aimem_user_a aimem_user_b\n")
	empty := filepath.Join(dir, "empty.token")
	writePrivate(t, empty, "\n")
	for name, args := range map[string][]string{
		"missing file":  {"task-token", "pilot", "--token-file", missing},
		"not private":   {"task-token", "pilot", "--token-file", wide},
		"two tokens":    {"task-token", "pilot", "--token-file", two},
		"empty":         {"task-token", "pilot", "--token-file", empty},
		"both":          {"task-token", "pilot", "aimem_user_x", "--token-file", user},
		"add, both":     {"add", "pilot", "https://hub.example", "tok", "--token-file", cp},
		"add, missing":  {"add", "pilot", "https://hub.example", "--token-file", missing},
		"add, no token": {"add", "pilot", "https://hub.example"},
	} {
		err := hubCmd(args)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		for _, secret := range []string{"aimem_user_wide", "aimem_user_a", "checkpoint-secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s: the error quotes the token: %v", name, err)
			}
		}
		if name == "missing file" && !strings.Contains(err.Error(), missing) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if after := *storedHub(t, "pilot"); after != before {
		t.Fatalf("a refused command changed the entry: %+v", after)
	}
}
