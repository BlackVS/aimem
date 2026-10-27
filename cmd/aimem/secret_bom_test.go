package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/hubtest"
	"aimem/internal/taskcred"
)

// bom is what Windows PowerShell 5.1 prepends to what it pipes under a
// UTF-8 console, and what Out-File writes at the start of a file.
const bom = "\uFEFF"

// Every reader of a secret from standard input or a token file drops one
// leading BOM; a second one, or one inside the token, is still refused.
func TestSecretReadersTolerateALeadingBOM(t *testing.T) {
	t.Run("task-token set", func(t *testing.T) {
		root, repo := t.TempDir(), t.TempDir()
		ts := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"scope":"project","task_write":true}`)) }))
		defer ts.Close()
		if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Pin: hubtest.Pin(ts)}}, "hub"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha"}`), 0600); err != nil {
			t.Fatal(err)
		}
		secret := "aimem_user_" + strings.Repeat("b", 64)
		var out bytes.Buffer
		if err := runTaskToken([]string{"set"}, repo, root, strings.NewReader(bom+secret+"\r\n"), &out); err != nil {
			t.Fatalf("a BOM-prefixed token was refused: %v", err)
		}
		if s, err := taskcred.Resolve(repo, root); err != nil || s.Token != secret {
			t.Fatalf("stored token: %v", err)
		}
		for name, in := range map[string]string{"two BOMs": bom + bom + secret, "a BOM inside": secret[:20] + bom + secret[20:]} {
			if err := runTaskToken([]string{"set"}, repo, root, strings.NewReader(in), &out); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})

	t.Run("hub token file and standard input", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "user.token")
		writePrivate(t, file, bom+"aimem_user_bom\r\n")
		if tok, err := readHubToken(file); err != nil || tok != "aimem_user_bom" {
			t.Fatalf("file: %q %v", tok, err)
		}
		defer func(r io.Reader) { hubStdin = r }(hubStdin)
		hubStdin = strings.NewReader(bom + "aimem_user_piped\r\n")
		if tok, err := readHubToken("-"); err != nil || tok != "aimem_user_piped" {
			t.Fatalf("stdin: %q %v", tok, err)
		}
		for name, in := range map[string]string{"two BOMs": bom + bom + "aimem_user_x\n", "a BOM inside": "aimem_user" + bom + "_x\n"} {
			hubStdin = strings.NewReader(in)
			if _, err := readHubToken("-"); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})

	t.Run("identity admin token file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "admin.token")
		writePrivate(t, file, bom+"hub-admin-secret\r\n")
		c, err := newIdentityClient("https://hub.example.test:8443", file, "", "sha256-"+strings.Repeat("A", 43)+"=")
		if err != nil || c.token != "hub-admin-secret" {
			t.Fatalf("admin token: %v", err)
		}
	})

	t.Run("enroll redeem record", func(t *testing.T) {
		e := newEnrollRig(t)
		record := e.issue(t)
		run := redeemWith(t, t.TempDir(), bom+record)
		if run.code != enrollExitOK || run.result(t).Outcome != "enrolled" {
			t.Fatalf("a BOM-prefixed record: %d %s", run.code, run.stderr)
		}
	})
}
