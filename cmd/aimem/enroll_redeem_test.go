package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aimem/internal/adapter"
)

const redeemPath = "/v1/identity/enrollments/redemptions"

// enrollRig is a real TLS hub that counts redemption requests and can lose
// the replies of the first ones (the hub commits, the client sees no answer).
type enrollRig struct {
	*identityCLIRig
	redeems      atomic.Int64
	lose         atomic.Int64
	identityDown atomic.Bool
}

func newEnrollRig(t *testing.T) *enrollRig {
	t.Helper()
	captureNotices(t)
	saved := enrollBackoff
	enrollBackoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { enrollBackoff = saved; enrollCrashAt = nil })
	e := &enrollRig{}
	e.identityCLIRig = newIdentityCLIRig(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/access/identity" && e.identityDown.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if r.URL.Path == redeemPath {
				e.redeems.Add(1)
				if e.lose.Load() > 0 {
					e.lose.Add(-1)
					h.ServeHTTP(httptest.NewRecorder(), r) // committed on the hub
					panic(http.ErrAbortHandler)            // the reply is lost
				}
			}
			h.ServeHTTP(w, r)
		})
	})
	return e
}

// issue runs the real `enroll issue` and returns its record line.
func (e *enrollRig) issue(t *testing.T) string {
	t.Helper()
	read := pipeSecretStdout(t)
	e.mustRun(t, "enroll", "issue", "--purpose", "new-user", "--output", "-")
	return read()
}

type redeemRun struct {
	code           int
	stdout, stderr string
}

func redeemWith(t *testing.T, root, record string, args ...string) redeemRun {
	t.Helper()
	if len(args) == 0 {
		args = []string{"--hub-name", "pilot", "--label", "member-laptop", "--json"}
	}
	var out, errb bytes.Buffer
	code := runEnrollRedeem(args, strings.NewReader(record), &out, &errb, root)
	return redeemRun{code, out.String(), errb.String()}
}

func (r redeemRun) result(t *testing.T) enrollResult {
	t.Helper()
	var res enrollResult
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatalf("result %q (stderr %q): %v", r.stdout, r.stderr, err)
	}
	return res
}

// noSecretOnDisk: the subcode is nowhere under the root, and the bearer is
// only in hub.json's credential slot.
func noSecretOnDisk(t *testing.T, root, subcode, bearer string) {
	t.Helper()
	filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), subcode) {
			t.Errorf("%s holds the subcode", path)
		}
		if bearer != "" && strings.Contains(string(raw), bearer) && filepath.Base(path) != "hub.json" {
			t.Errorf("%s holds the bearer", path)
		}
		return nil
	})
}

func recordSubcode(t *testing.T, record string) string {
	t.Helper()
	var rec struct {
		Subcode string `json:"subcode"`
	}
	if err := json.Unmarshal([]byte(record), &rec); err != nil {
		t.Fatal(err)
	}
	return rec.Subcode
}

// The pipeline end to end: issue on the hub, redeem on a clean
// installation, the credential stored and accepted, a rerun spends nothing.
func TestEnrollRedeemEndToEnd(t *testing.T) {
	e := newEnrollRig(t)
	record := e.issue(t)
	root := t.TempDir()
	run := redeemWith(t, root, record)
	if run.code != enrollExitOK {
		t.Fatalf("redeem: %d %s", run.code, run.stderr)
	}
	res := run.result(t)
	if res.Outcome != "enrolled" || res.Hub != "pilot" || res.UserID == "" {
		t.Fatalf("result: %+v", res)
	}
	hubs, def := adapter.LoadHubs(root)
	h := hubs["pilot"]
	if h == nil || def != "pilot" || h.URL != e.ts.URL || !strings.HasPrefix(h.TaskToken, "aimem_user_") || h.Token != "" || h.CAFile == "" {
		t.Fatalf("hub entry: %+v default %q", h, def)
	}
	if ca, err := os.ReadFile(h.CAFile); err != nil || !strings.Contains(string(ca), "CERTIFICATE") {
		t.Fatalf("the CA bundle beside the entry: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(root, "enroll", "*.json")); len(entries) != 0 {
		t.Fatalf("pending state left: %v", entries)
	}
	noSecretOnDisk(t, root, recordSubcode(t, record), h.TaskToken)
	if list := e.mustRun(t, "enroll", "list", "--state", "redeemed"); !strings.Contains(list, "redeemed: user "+res.UserID) {
		t.Fatalf("hub ledger: %q", list)
	}
	before := e.redeems.Load()
	again := redeemWith(t, root, record)
	if again.code != enrollExitOK || again.result(t).Outcome != "already_enrolled" || e.redeems.Load() != before {
		t.Fatalf("rerun: %d %s, %d redemption requests", again.code, again.stderr, e.redeems.Load()-before)
	}
}

// Every reply lost: the run ends with outcome unknown (5) and keeps its
// keys; the rerun replays the committed redemption.
func TestEnrollRedeemLostReplies(t *testing.T) {
	e := newEnrollRig(t)
	record := e.issue(t)
	root := t.TempDir()
	e.lose.Store(5)
	run := redeemWith(t, root, record)
	if run.code != enrollExitUnknown || !strings.Contains(run.stderr, "run the same command again") {
		t.Fatalf("lost replies: %d %s", run.code, run.stderr)
	}
	if hubs, _ := adapter.LoadHubs(root); hubs != nil {
		t.Fatalf("a credential was stored without a delivery: %+v", hubs)
	}
	again := redeemWith(t, root, record)
	if again.code != enrollExitOK || again.result(t).Outcome != "enrolled" {
		t.Fatalf("rerun: %d %s", again.code, again.stderr)
	}
	// One user, however many requests.
	if list := e.mustRun(t, "enroll", "list", "--state", "redeemed"); strings.Count(list, "redeemed: user") != 1 {
		t.Fatalf("ledger: %q", list)
	}
}

// A crash after the delivery was recorded, before the credential was
// stored: the rerun replays and stores it.
func TestEnrollRedeemCrashBeforeStorage(t *testing.T) {
	e := newEnrollRig(t)
	record := e.issue(t)
	root := t.TempDir()
	enrollCrashAt = func(phase string) bool { return phase == "delivered" }
	if run := redeemWith(t, root, record); run.code != enrollExitUnknown {
		t.Fatalf("crash: %d %s", run.code, run.stderr)
	}
	enrollCrashAt = nil
	if hubs, _ := adapter.LoadHubs(root); hubs != nil {
		t.Fatal("the credential was stored before the crash point")
	}
	again := redeemWith(t, root, record)
	if again.code != enrollExitOK || again.result(t).Outcome != "enrolled" {
		t.Fatalf("rerun: %d %s", again.code, again.stderr)
	}
}

// A crash after the credential was stored: the rerun verifies it against
// the recorded identity and never calls the redemption route.
func TestEnrollRedeemCrashAfterStorage(t *testing.T) {
	e := newEnrollRig(t)
	record := e.issue(t)
	root := t.TempDir()
	enrollCrashAt = func(phase string) bool { return phase == "stored" }
	first := redeemWith(t, root, record)
	if first.code != enrollExitUnknown {
		t.Fatalf("crash: %d %s", first.code, first.stderr)
	}
	enrollCrashAt = nil
	before := e.redeems.Load()
	again := redeemWith(t, root, record)
	if again.code != enrollExitOK || again.result(t).Outcome != "already_enrolled" || e.redeems.Load() != before {
		t.Fatalf("rerun: %d %s, %d redemption requests", again.code, again.stderr, e.redeems.Load()-before)
	}
	if entries, _ := filepath.Glob(filepath.Join(root, "enroll", "*.json")); len(entries) != 0 {
		t.Fatalf("pending state left: %v", entries)
	}
}

func TestEnrollRedeemRefusals(t *testing.T) {
	e := newEnrollRig(t)

	// A revoked bundle: final, nothing stored.
	revoked := e.issue(t)
	var rec map[string]any
	json.Unmarshal([]byte(revoked), &rec)
	e.mustRun(t, "enroll", "revoke", "--bundle-id", rec["bundle_id"].(string))
	root := t.TempDir()
	if run := redeemWith(t, root, revoked); run.code != enrollExitFinal || !strings.Contains(run.stderr, "enrollment_invalid") {
		t.Fatalf("revoked: %d %s", run.code, run.stderr)
	}
	if hubs, _ := adapter.LoadHubs(root); hubs != nil {
		t.Fatal("a refused redemption stored a hub entry")
	}

	// An expired record is refused before any request.
	live := e.issue(t)
	before := e.redeems.Load()
	json.Unmarshal([]byte(live), &rec)
	rec["expires_at"] = "2001-01-01T00:00:00Z"
	raw, _ := json.Marshal(rec)
	expired := string(raw) + "\n"
	if run := redeemWith(t, t.TempDir(), expired); run.code != enrollExitFinal || !strings.Contains(run.stderr, "expired") {
		t.Fatalf("expired: %d %s", run.code, run.stderr)
	}

	// A hub that fails the record's trust gets nothing.
	json.Unmarshal([]byte(live), &rec)
	rec["hub"].(map[string]any)["trust"] = map[string]any{"spki_sha256": "sha256-" + strings.Repeat("A", 43) + "="}
	raw, _ = json.Marshal(rec)
	if run := redeemWith(t, t.TempDir(), string(raw)); run.code != enrollExitFinal || !strings.Contains(run.stderr, "failed the record's trust check") {
		t.Fatalf("trust: %d %s", run.code, run.stderr)
	}
	if n := e.redeems.Load() - before; n != 0 {
		t.Fatalf("%d redemption requests for refused records", n)
	}

	// An oversized record, bad flags, and a hub name already bound elsewhere.
	if run := redeemWith(t, t.TempDir(), strings.Repeat("x", enrollRecordLimit+1)); run.code != enrollExitFinal {
		t.Fatalf("oversized: %d %s", run.code, run.stderr)
	}
	if run := redeemWith(t, t.TempDir(), live, "--hub-name", "pilot", "--label", "Not A Label"); run.code != enrollExitUsage {
		t.Fatalf("bad label: %d", run.code)
	}
	other := t.TempDir()
	if err := adapter.SaveHubs(other, map[string]*adapter.HubConfig{"pilot": {URL: "https://elsewhere.example.test:8443", Token: "t"}}, "pilot"); err != nil {
		t.Fatal(err)
	}
	if run := redeemWith(t, other, live); run.code != enrollExitFinal || !strings.Contains(run.stderr, "identity_mismatch") {
		t.Fatalf("bound elsewhere: %d %s", run.code, run.stderr)
	}
}

// A stored credential whose check cannot finish (the hub answers 503) is
// never taken as refused: nothing is spent and the credential stays. A
// credential the hub refuses (revoked) is replaced by a new enrollment.
func TestEnrollRedeemSpendsOnlyWhenTheStoredCredentialIsRefused(t *testing.T) {
	e := newEnrollRig(t)
	root := t.TempDir()
	if run := redeemWith(t, root, e.issue(t)); run.code != enrollExitOK {
		t.Fatalf("first enrollment: %d %s", run.code, run.stderr)
	}
	hubs, _ := adapter.LoadHubs(root)
	stored := hubs["pilot"].TaskToken
	second := e.issue(t)
	before := e.redeems.Load()
	e.identityDown.Store(true)
	run := redeemWith(t, root, second)
	e.identityDown.Store(false)
	if run.code != enrollExitRetry || !strings.Contains(run.stderr, "nothing was spent") || e.redeems.Load() != before {
		t.Fatalf("check unavailable: %d %s, %d redemption requests", run.code, run.stderr, e.redeems.Load()-before)
	}
	if hubs, _ := adapter.LoadHubs(root); hubs["pilot"].TaskToken != stored {
		t.Fatal("the stored credential was replaced")
	}
	// Revoke the stored credential on the hub: now the second code is spent.
	db, err := sql.Open("sqlite", filepath.Join(e.reg.Root(), "access.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE tokens SET revoked=1"); err != nil {
		t.Fatal(err)
	}
	again := redeemWith(t, root, second)
	if again.code != enrollExitOK || again.result(t).Outcome != "enrolled" {
		t.Fatalf("after revocation: %d %s", again.code, again.stderr)
	}
	if hubs, _ := adapter.LoadHubs(root); hubs["pilot"].TaskToken == stored {
		t.Fatal("the refused credential was kept")
	}
}

// Runs that start at once publish one pending state: every one gets the
// winner's keys, so a redemption that reached the hub stays recoverable.
func TestNewEnrollPendingPublishedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enroll", "01a10c90-0000-7000-8000-000000000001.json")
	type got struct {
		p   *enrollPending
		err error
	}
	out := make(chan got, 8)
	for range 8 {
		go func() {
			p, err := newEnrollPending(path, "01a10c90-0000-7000-8000-000000000001")
			out <- got{p, err}
		}()
	}
	var first *enrollPending
	for range 8 {
		g := <-out
		if g.err != nil {
			t.Fatal(g.err)
		}
		if first == nil {
			first = g.p
			continue
		}
		if g.p.RequestKey != first.RequestKey || g.p.PrivateKey != first.PrivateKey {
			t.Fatal("two runs got different keys for one bundle")
		}
	}
	stored, err := loadEnrollPending(path)
	if err != nil || stored == nil || stored.RequestKey != first.RequestKey {
		t.Fatalf("stored state: %+v %v", stored, err)
	}
	// An empty file (a run that stopped before writing) holds no keys.
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := loadEnrollPending(empty); err != nil || p != nil {
		t.Fatalf("empty state: %+v %v", p, err)
	}
}

func TestNewEnrollPendingAbandonedEmpty(t *testing.T) {
	bundle := "01a10c90-0000-7000-8000-000000000001"
	dir := filepath.Join(t.TempDir(), "enroll")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	empty := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// A creator that stopped before writing, long ago: the run recovers
	// with fresh keys.
	old := empty("old.json", 2*enrollAbandonedAfter)
	p, err := newEnrollPending(old, bundle)
	if err != nil {
		t.Fatalf("abandoned empty file not recovered: %v", err)
	}
	stored, err := loadEnrollPending(old)
	if err != nil || stored == nil || stored.RequestKey != p.RequestKey {
		t.Fatalf("stored state: %+v %v", stored, err)
	}

	// A young empty file is never touched, even under the lock: the run
	// asks to be repeated.
	young := empty("young.json", time.Second)
	if _, err := newEnrollPending(young, bundle); err == nil || !strings.Contains(err.Error(), "moments ago") {
		t.Fatalf("young empty file: %v", err)
	}
	if fi, err := os.Stat(young); err != nil || fi.Size() != 0 {
		t.Fatalf("young empty file was changed: %v %v", fi, err)
	}

	// Runs that start together from an abandoned file all resume with one
	// set of keys: no run recovers a file another has already replaced.
	shared := empty("shared.json", 2*enrollAbandonedAfter)
	out := make(chan *enrollPending, 8)
	for range 8 {
		go func() {
			p, err := newEnrollPending(shared, bundle)
			if err != nil {
				t.Error(err)
			}
			out <- p
		}()
	}
	var first *enrollPending
	for range 8 {
		p := <-out
		if p == nil {
			continue
		}
		if first == nil {
			first = p
		} else if p.RequestKey != first.RequestKey {
			t.Fatal("two runs recovered one abandoned file with different keys")
		}
	}
	if stored, err := loadEnrollPending(shared); err != nil || first == nil || stored.RequestKey != first.RequestKey {
		t.Fatalf("stored state: %+v %v", stored, err)
	}
}

func TestNewEnrollPendingStalledCreatorKeepsItsFile(t *testing.T) {
	// A creator stalled between the create and the write holds the creation
	// lock: another run never recovers its file, however old the file looks.
	path := filepath.Join(t.TempDir(), "enroll", "stalled.json")
	bundle := "01a10c90-0000-7000-8000-000000000001"
	var otherErr error
	enrollAfterCreate = func() {
		enrollAfterCreate = nil
		at := time.Now().Add(-2 * enrollAbandonedAfter)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Error(err)
			return
		}
		enrollLockWait = 0
		_, otherErr = newEnrollPending(path, bundle)
		enrollLockWait = 10 * time.Second
	}
	t.Cleanup(func() { enrollAfterCreate = nil; enrollLockWait = 10 * time.Second })
	p, err := newEnrollPending(path, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if otherErr == nil || !strings.Contains(otherErr.Error(), "another run is creating") {
		t.Fatalf("a run got past a live creator: %v", otherErr)
	}
	stored, err := loadEnrollPending(path)
	if err != nil || stored == nil || stored.RequestKey != p.RequestKey {
		t.Fatalf("the stalled creator's keys are not the published state: %+v %v", stored, err)
	}
}

// A first run stops retrying at the record's expiry with outcome unknown;
// the rerun is a recovery, which the hub replays past the expiry.
func TestEnrollRedeemFirstRunStopsAtExpiry(t *testing.T) {
	e := newEnrollRig(t)
	enrollBackoff = func(int) time.Duration { return 700 * time.Millisecond }
	read := pipeSecretStdout(t)
	e.mustRun(t, "enroll", "issue", "--purpose", "new-user", "--expires", "1s", "--output", "-")
	record := read()
	root := t.TempDir()
	e.lose.Store(5)
	run := redeemWith(t, root, record)
	if run.code != enrollExitUnknown || !strings.Contains(run.stderr, "expired while retrying") {
		t.Fatalf("first run: %d %s", run.code, run.stderr)
	}
	if n := e.redeems.Load(); n >= 5 {
		t.Fatalf("%d attempts: the first run retried past the expiry", n)
	}
	e.lose.Store(0)
	enrollBackoff = func(int) time.Duration { return 0 }
	again := redeemWith(t, root, record)
	if again.code != enrollExitOK || again.result(t).Outcome != "enrolled" {
		t.Fatalf("recovery: %d %s", again.code, again.stderr)
	}
}

// `aimem hub repair` rewrites hub.json owner-only and changes nothing else:
// with no default hub it keeps none, so routing is unchanged.
func TestHubRepairKeepsRouting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	hubs := map[string]*adapter.HubConfig{
		"work": {URL: "https://work.example.test", Token: "t1"},
		"home": {URL: "https://home.example.test", Token: "t2", TaskToken: "aimem_user_sample"},
	}
	if err := adapter.SaveHubs(root, hubs, ""); err != nil {
		t.Fatal(err)
	}
	if err := hubCmd([]string{"repair"}); err != nil {
		t.Fatal(err)
	}
	got, def := adapter.LoadHubs(root)
	if def != "" || len(got) != 2 || got["home"].TaskToken != "aimem_user_sample" || got["work"].Token != "t1" {
		t.Fatalf("after repair: %+v default %q", got, def)
	}
	if err := adapter.HubConfigPrivate(root); err != nil {
		t.Fatalf("not private after repair: %v", err)
	}
}

// Run B is overtaken: it starts, A completes the whole enrollment and
// removes its state, then B publishes fresh keys. B must end as
// already_enrolled without spending anything, and a state left by such a
// run must not wedge later reruns.
func TestEnrollRedeemOvertakenRunFinishesAsEnrolled(t *testing.T) {
	e := newEnrollRig(t)
	record := e.issue(t)
	root := t.TempDir()
	var calls atomic.Int64
	paused, release := make(chan struct{}), make(chan struct{})
	enrollBeforePublish = func() {
		if calls.Add(1) == 1 { // run B only
			close(paused)
			<-release
		}
	}
	t.Cleanup(func() { enrollBeforePublish = nil })
	bDone := make(chan redeemRun, 1)
	go func() { bDone <- redeemWith(t, root, record) }()
	<-paused
	a := redeemWith(t, root, record)
	if a.code != enrollExitOK || a.result(t).Outcome != "enrolled" {
		t.Fatalf("run A: %d %s", a.code, a.stderr)
	}
	hubs, _ := adapter.LoadHubs(root)
	stored := hubs["pilot"].TaskToken
	before := e.redeems.Load()
	close(release)
	b := <-bDone
	if b.code != enrollExitOK || b.result(t).Outcome != "already_enrolled" || e.redeems.Load() != before {
		t.Fatalf("run B: %d %s, %d redemption requests", b.code, b.stderr, e.redeems.Load()-before)
	}
	if hubs, _ := adapter.LoadHubs(root); hubs["pilot"].TaskToken != stored {
		t.Fatal("run B replaced run A's credential")
	}
	if entries, _ := filepath.Glob(filepath.Join(root, "enroll", "*.json")); len(entries) != 0 {
		t.Fatalf("pending state left: %v", entries)
	}

	// A state left by an overtaken run (fresh keys the hub never accepted):
	// the rerun meets the conflict, finds the stored credential, and cleans up.
	var rec struct {
		BundleID string `json:"bundle_id"`
	}
	json.Unmarshal([]byte(record), &rec)
	left := filepath.Join(root, "enroll", rec.BundleID+".json")
	if _, err := newEnrollPending(left, rec.BundleID); err != nil {
		t.Fatal(err)
	}
	enrollBeforePublish = nil
	again := redeemWith(t, root, record)
	if again.code != enrollExitOK || again.result(t).Outcome != "already_enrolled" {
		t.Fatalf("rerun over a stale state: %d %s", again.code, again.stderr)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("the stale state was kept: %v", err)
	}
	if list := e.mustRun(t, "enroll", "list", "--state", "redeemed"); strings.Count(list, "redeemed: user") != 1 {
		t.Fatalf("ledger: %q", list)
	}
}
