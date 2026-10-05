package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// pipeSecretStdout points `--output -` at a pipe for the test and returns a
// reader for what was written.
func pipeSecretStdout(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := secretStdout
	secretStdout = w
	t.Cleanup(func() { secretStdout = saved })
	return func() string {
		w.Close()
		b, _ := io.ReadAll(r)
		return string(b)
	}
}

type testRecord struct {
	Kind      string    `json:"kind"`
	Version   int       `json:"version"`
	BundleID  string    `json:"bundle_id"`
	Purpose   string    `json:"purpose"`
	Subcode   string    `json:"subcode"`
	ExpiresAt time.Time `json:"expires_at"`
	Hub       struct {
		HubID string         `json:"hub_id"`
		URL   string         `json:"url"`
		Trust map[string]any `json:"trust"`
	} `json:"hub"`
}

// enroll issue writes exactly one line, the subcode record, to the pipe; the
// bundle ID and a runnable revoke command go to standard error first; the
// subcode appears nowhere else.
func TestIdentityEnrollIssueWritesOnlyTheRecord(t *testing.T) {
	notices := captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	read := pipeSecretStdout(t)
	before := time.Now()
	out := g.mustRun(t, "enroll", "issue", "--purpose", "new-user", "--expires", "2h", "--output", "-")
	piped := read()
	if out != "" || strings.Count(piped, "\n") != 1 {
		t.Fatalf("stdout %q, pipe %q", out, piped)
	}
	var rec testRecord
	if err := json.Unmarshal([]byte(piped), &rec); err != nil {
		t.Fatal(err)
	}
	ca, _ := os.ReadFile(g.caFile)
	if rec.Kind != "aimem-enrollment" || rec.Version != 1 || rec.Purpose != "new_user" ||
		!regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(rec.BundleID) ||
		!regexp.MustCompile(`^aes1_[A-Za-z0-9_-]{43}$`).MatchString(rec.Subcode) ||
		rec.Hub.HubID == "" || rec.Hub.URL != g.ts.URL || rec.Hub.Trust["ca_pem"] != string(ca) ||
		rec.ExpiresAt.Before(before.Add(2*time.Hour-time.Minute)) || rec.ExpiresAt.After(time.Now().Add(2*time.Hour)) {
		t.Fatalf("record: %+v", rec)
	}
	if !strings.Contains(notices.String(), rec.BundleID) || len(printedLines(notices.String())) == 0 {
		t.Fatalf("notices: %q", notices)
	}
	list := g.mustRun(t, "enroll", "list")
	if !strings.Contains(list, rec.BundleID+"  new_user  issued") {
		t.Fatalf("list: %q", list)
	}
	// The printed revoke command runs as printed.
	if err := g.runPrinted(t, shellParse(t, printedLines(notices.String())[0])); err != nil {
		t.Fatalf("printed revoke: %v", err)
	}
	if list := g.mustRun(t, "enroll", "list", "--state", "revoked"); !strings.Contains(list, rec.BundleID) {
		t.Fatalf("after revoke: %q", list)
	}
	for _, o := range append(g.outputs, notices.String()) {
		if strings.Contains(o, rec.Subcode) {
			t.Fatalf("the subcode leaked: %.120q", o)
		}
	}
	g.assertNoSecrets(t)
}

// The record's trust is the trust the issuing command used.
func TestIdentityEnrollRecordCarriesThePin(t *testing.T) {
	captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	read := pipeSecretStdout(t)
	if _, err := g.runRaw(t, "enroll", "issue", "--purpose", "new-user", "--output", "-",
		"--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--hub-pin", g.pin); err != nil {
		t.Fatal(err)
	}
	var rec testRecord
	if err := json.Unmarshal([]byte(read()), &rec); err != nil || rec.Hub.Trust["spki_sha256"] != g.pin || len(rec.Hub.Trust) != 1 {
		t.Fatalf("pin trust: %+v %v", rec.Hub.Trust, err)
	}
	if time.Until(rec.ExpiresAt) < 23*time.Hour || time.Until(rec.ExpiresAt) > 24*time.Hour {
		t.Fatalf("default expiry: %v", rec.ExpiresAt)
	}
}

// Bad arguments, a file destination and a terminal are refused before the
// hub is asked anything.
func TestIdentityEnrollRefusesBeforeTheHub(t *testing.T) {
	captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	dev, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	saved := secretStdout
	secretStdout = dev
	defer func() { secretStdout = saved }()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"enroll", "issue", "--purpose", "new-user", "--output", "-"}, "standard output is a terminal"},
		{[]string{"enroll", "issue", "--purpose", "new-user", "--output", g.secretPath("record")}, "only into a pipe"},
		{[]string{"enroll", "issue", "--output", "-"}, "--purpose new-user"},
		{[]string{"enroll", "issue", "--purpose", "existing-user", "--output", "-"}, "--purpose new-user"},
		{[]string{"enroll", "issue", "--purpose", "new-user", "--expires", "73h", "--output", "-"}, "at most 72h"},
		{[]string{"enroll", "issue", "--purpose", "new-user", "--expires", "3d", "--output", "-"}, "at most 72h"},
		{[]string{"enroll", "revoke"}, "--bundle-id"},
		{[]string{"enroll", "revoke", "--bundle-id", "x", "--state", "issued"}, "does not apply"},
		{[]string{"enroll", "list", "--purpose", "new-user"}, "does not apply"},
		{[]string{"enroll", "redeem"}, "usage"},
	} {
		if _, err := g.run(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	if n := g.requests.Load(); n != 0 {
		t.Fatalf("%d requests reached the hub", n)
	}
}

// Revoking a redeemed bundle names what it issued and the commands that undo
// it.
func TestIdentityEnrollRevokeRedeemed(t *testing.T) {
	captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	read := pipeSecretStdout(t)
	g.mustRun(t, "enroll", "issue", "--purpose", "new-user", "--output", "-")
	var rec testRecord
	if err := json.Unmarshal([]byte(read()), &rec); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(g.reg.Root(), "access.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE enrollments SET redeemed_at=?, redeemed_user_id='u-1', redeemed_token_id='t-1' WHERE bundle_id=?", time.Now().Unix(), rec.BundleID); err != nil {
		t.Fatal(err)
	}
	_, err = g.run(t, "enroll", "revoke", "--bundle-id", rec.BundleID)
	if err == nil || !strings.Contains(err.Error(), "already redeemed: user u-1, token t-1") ||
		!strings.Contains(err.Error(), "aimem access token-revoke --token-id t-1") {
		t.Fatalf("revoke redeemed: %v", err)
	}
	if list := g.mustRun(t, "enroll", "list", "--state", "redeemed"); !strings.Contains(list, "redeemed: user u-1, token t-1") {
		t.Fatalf("list redeemed: %q", list)
	}
}

// A standard output redirected to a regular file is refused like a
// terminal: the record exists only in a pipe. Nothing reaches the hub.
func TestIdentityEnrollRefusesAFileOnStandardOutput(t *testing.T) {
	captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	f, err := os.Create(g.secretPath("redirected"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	saved := secretStdout
	secretStdout = f
	defer func() { secretStdout = saved }()
	if _, err := g.run(t, "enroll", "issue", "--purpose", "new-user", "--output", "-"); err == nil || !strings.Contains(err.Error(), "not a pipe") {
		t.Fatalf("regular file: %v", err)
	}
	if fi, _ := f.Stat(); fi.Size() != 0 {
		t.Fatalf("the redirected file got %d bytes", fi.Size())
	}
	if n := g.requests.Load(); n != 0 {
		t.Fatalf("%d requests reached the hub", n)
	}
}

// A record that would exceed enrollment.v1's 16,384 bytes (here, from a
// large CA bundle) is refused before the hub is asked; a record just under
// the bound is written.
func TestIdentityEnrollRecordSizeBound(t *testing.T) {
	captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	ca, err := os.ReadFile(g.caFile)
	if err != nil {
		t.Fatal(err)
	}
	big := g.secretPath("big-ca.pem")
	if err := os.WriteFile(big, []byte(strings.Repeat(string(ca), enrollRecordMax/len(ca)+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	pipeSecretStdout(t)
	_, err = g.runRaw(t, "enroll", "issue", "--purpose", "new-user", "--output", "-",
		"--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--hub-ca-file", big)
	if err == nil || !strings.Contains(err.Error(), "more than enrollment.v1's 16384") || !strings.Contains(err.Error(), "nothing was issued") {
		t.Fatalf("oversized record: %v", err)
	}
	if n := g.requests.Load(); n != 0 {
		t.Fatalf("%d requests reached the hub", n)
	}
	// The bound itself: the estimate covers the record actually written.
	read := pipeSecretStdout(t)
	g.mustRun(t, "enroll", "issue", "--purpose", "new-user", "--output", "-")
	written := read()
	var rec testRecord
	if err := json.Unmarshal([]byte(written), &rec); err != nil {
		t.Fatal(err)
	}
	trust := map[string]any{"ca_pem": string(ca)}
	if est := enrollRecordSize("https://"+strings.TrimPrefix(g.ts.URL, "https://"), trust); est < len(written) {
		t.Fatalf("estimate %d is below the written record's %d bytes", est, len(written))
	}
}

// A pipe whose reader has gone (aicrew's console refused) gets nothing, and
// the bundle is revoked, so its subcode can never be redeemed.
func TestIdentityEnrollBrokenPipeRevokes(t *testing.T) {
	captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	saved := secretStdout
	secretStdout = w
	defer func() { secretStdout = saved }()
	_, err = g.run(t, "enroll", "issue", "--purpose", "new-user", "--output", "-")
	if err == nil || !strings.Contains(err.Error(), "could not be written") || !strings.Contains(err.Error(), "was revoked") {
		t.Fatalf("broken pipe: %v", err)
	}
	if list := g.mustRun(t, "enroll", "list", "--state", "revoked"); strings.Count(list, "revoked") != 1 {
		t.Fatalf("the bundle is not revoked: %q", list)
	}
}
