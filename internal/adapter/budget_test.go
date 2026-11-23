package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scanBack must hand back every line of a file, last first, with the
// offset it starts at: across chunk boundaries, without a trailing
// newline, with CRLF endings, and skipping only a line past maxLine.
func TestScanBackReturnsEveryLineWithItsOffset(t *testing.T) {
	var b bytes.Buffer
	var want []string
	var offs []int64
	for i := 0; b.Len() < 3*tailChunk; i++ {
		line := fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("x", i%5000))
		offs = append(offs, int64(b.Len()))
		want = append(want, line)
		b.WriteString(line)
		if i%7 == 0 {
			b.WriteString("\r\n")
		} else {
			b.WriteString("\n")
		}
	}
	offs = append(offs, int64(b.Len()))
	want = append(want, "last line without newline")
	b.WriteString("last line without newline")
	p := filepath.Join(t.TempDir(), "r.jsonl")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []string
	var gotOffs []int64
	if err := scanBack(f, time.Now().Add(time.Minute), func(line []byte, off int64) bool {
		got = append(got, strings.TrimSuffix(string(line), "\r"))
		gotOffs = append(gotOffs, off)
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		j := len(want) - 1 - i
		if got[i] != want[j] || gotOffs[i] != offs[j] {
			t.Fatalf("line %d: got %.40q at %d, want %.40q at %d", j, got[i], gotOffs[i], want[j], offs[j])
		}
	}
}

func TestScanBackSkipsAnOverlongLineAndStopsAtTheDeadline(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.jsonl")
	body := "first\n" + strings.Repeat("y", maxLine+tailChunk+3) + "\nlast\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(p)
	defer f.Close()
	var got []string
	if err := scanBack(f, time.Now().Add(time.Minute), func(line []byte, _ int64) bool {
		got = append(got, string(line))
		return false
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"last", "first"}) {
		t.Fatalf("lines: %q", got)
	}
	if err := scanBack(f, time.Now().Add(-time.Second), func([]byte, int64) bool { return false }); err != errShare {
		t.Fatalf("a past deadline: %v", err)
	}
}

// otherTurns is a rollout prefix of n complete turns of about size bytes
// in all, none of them the turn under test.
func otherTurns(n, size int) []string {
	var out []string
	pad := strings.Repeat("z", size/n)
	for i := range n {
		id := fmt.Sprintf("old-%d", i)
		out = append(out,
			`{"type":"event_msg","payload":{"type":"task_started","turn_id":"`+id+`"}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"`+id+`","item":{"type":"UserMessage","id":"x","content":[{"type":"text","text":"old ask"}]}}}`,
			`{"type":"response_item","payload":{"type":"function_call_output","call_id":"c","output":"`+pad+`"}}`,
			`{"type":"response_item","payload":{"type":"custom_tool_call","name":"old_tool","input":"x"}}`,
			`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"`+id+`","last_agent_message":"old reply"}}`,
		)
	}
	return out
}

// Reading from the turn's start gives what reading the whole rollout
// gave: the same prompt, reply, tools and turn, for a named turn, a
// retried one, and the last turn (PreCompact), whatever lies before it.
func TestCodexReadFromTheTurnStartMatchesAWholeRead(t *testing.T) {
	turns := [][]string{
		{ // a retried turn: tools before its second start are dropped
			`{"type":"event_msg","payload":{"type":"task_started","turn_id":"T"}}`,
			`{"type":"response_item","payload":{"type":"custom_tool_call","name":"abandoned","input":"x"}}`,
			`{"type":"event_msg","payload":{"type":"task_started","turn_id":"T"}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"T","item":{"type":"UserMessage","id":"i","content":[{"type":"text","text":"the ask"}]}}}`,
			`{"type":"response_item","payload":{"type":"function_call","name":"read_file","arguments":"{}"}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"T","item":{"type":"AgentMessage","id":"m","content":[{"type":"Text","text":"the reply"}]}}}`,
			`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"T","last_agent_message":"the reply"}}`,
		},
	}
	for _, turnID := range []string{"T", ""} {
		alone := writeTranscript(t, turns[0])
		after := writeTranscript(t, append(otherTurns(50, 2<<20), turns[0]...))
		dl := time.Now().Add(time.Minute)
		u1, r1, t1, l1, d1, e1 := parseRollout(alone, turnID, dl)
		u2, r2, t2, l2, d2, e2 := parseRollout(after, turnID, dl)
		if e1 != nil || e2 != nil {
			t.Fatal(e1, e2)
		}
		if u1 != u2 || r1 != r2 || !reflect.DeepEqual(t1, t2) || l1 != l2 || d1 != d2 {
			t.Fatalf("turn %q: alone %q %q %v %q %v, after other turns %q %q %v %q %v", turnID, u1, r1, t1, l1, d1, u2, r2, t2, l2, d2)
		}
		if u2 != "the ask" || r2 != "the reply" || !reflect.DeepEqual(t2, []string{"read_file"}) || l2 != "T" || !d2 {
			t.Fatalf("turn %q: %q %q %v %q %v", turnID, u2, r2, t2, l2, d2)
		}
	}
}

// The observed fault: a large rollout whose turn is not yet complete when
// Stop fires. The event is built inside the transcript share, from what
// could be read or from the payload alone, and says so; nothing is lost.
func TestCodexStopNeverLosesTheTurnToTheRollout(t *testing.T) {
	old := TranscriptShare
	TranscriptShare = 300 * time.Millisecond
	defer func() { TranscriptShare = old }()
	open := append(otherTurns(20, 8<<20),
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"T"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"T","item":{"type":"UserMessage","id":"i","content":[{"type":"text","text":"slow ask"}]}}}`,
	)
	tp := writeTranscript(t, open)
	start := time.Now()
	p := buildCodex(t, map[string]any{"session_id": "s", "turn_id": "T", "transcript_path": tp, "cwd": t.TempDir(),
		"hook_event_name": "Stop", "last_assistant_message": "the payload's reply"})
	if took := time.Since(start); took > TranscriptShare+2*time.Second {
		t.Fatalf("the event took %v, past its share of %v", took, TranscriptShare)
	}
	if p.Event.AssistantReply != "the payload's reply" || p.Event.IdempotencyKey != "codex:s:T" {
		t.Fatalf("event: %+v", p.Event)
	}
	if p.Degraded == "" {
		t.Fatal("a turn read past its share is not marked")
	}

	// A read that cannot even start in time: the payload alone.
	TranscriptShare = 0
	p = buildCodex(t, map[string]any{"session_id": "s", "turn_id": "T", "transcript_path": tp, "cwd": t.TempDir(),
		"hook_event_name": "Stop", "last_assistant_message": "only the payload"})
	if p.Event.AssistantReply != "only the payload" || p.Event.UserRequest != "" || p.Degraded == "" {
		t.Fatalf("payload-only event: %+v degraded=%q", p.Event, p.Degraded)
	}
}

func TestClaudeReadFromTheTurnStartMatchesAWholeRead(t *testing.T) {
	var before []string
	pad := strings.Repeat("q", 40000)
	for i := range 100 {
		before = append(before,
			fmt.Sprintf(`{"type":"user","uuid":"ou%d","message":{"role":"user","content":"old ask %d"}}`, i, i),
			fmt.Sprintf(`{"type":"assistant","uuid":"oa%d","message":{"role":"assistant","content":[{"type":"tool_use","name":"Old","input":{}},{"type":"text","text":"%s"}]}}`, i, pad),
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"r"}]}}`,
		)
	}
	cases := map[string][]string{
		"a complete turn": {
			`{"type":"user","uuid":"u1","message":{"role":"user","content":"the ask"}}`,
			`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}}`,
			`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
			`{"type":"assistant","uuid":"a2","message":{"role":"assistant","content":[{"type":"text","text":"the reply"}]}}`,
		},
		"a prompt with no reply yet (PreCompact)": {
			`{"type":"user","uuid":"u1","message":{"role":"user","content":"/compact"}}`,
		},
	}
	for name, turn := range cases {
		alone := writeTranscript(t, turn)
		after := writeTranscript(t, append(append([]string{}, before...), turn...))
		dl := time.Now().Add(time.Minute)
		u1, r1, t1, id1, la1, e1 := parseTranscript(alone, dl)
		u2, r2, t2, id2, la2, e2 := parseTranscript(after, dl)
		if e1 != nil || e2 != nil {
			t.Fatal(e1, e2)
		}
		if u1 != u2 || r1 != r2 || !reflect.DeepEqual(t1, t2) || id1 != id2 {
			t.Fatalf("%s: alone %q %q %v %q, after other turns %q %q %v %q", name, u1, r1, t1, id1, u2, r2, t2, id2)
		}
		if name == "a prompt with no reply yet (PreCompact)" {
			if la2 != "oa99" {
				t.Fatalf("%s: the previous assistant entry is %q, want oa99", name, la2)
			}
		} else if la1 != la2 {
			t.Fatalf("%s: last assistant %q vs %q", name, la1, la2)
		}
	}
}

// hubRoot is a state root whose default hub is url, and a payload bound
// to it.
func hubRoot(t *testing.T, url string) (string, *Payload) {
	t.Helper()
	root := t.TempDir()
	if err := SaveHubs(root, map[string]*HubConfig{"default": {URL: url, Token: "t"}}, "default"); err != nil {
		t.Fatal(err)
	}
	p := &Payload{ProjectID: "proj-b"}
	p.Event.Client, p.Event.SessionID, p.Event.TurnID, p.Event.Kind = "codex", "s1", "t1", "turn"
	p.Event.IdempotencyKey = "codex:s1:t1"
	return root, p
}

// The checkpoint is on disk, in a file of its own, before the hub is
// asked; a delivered one leaves neither that file nor a spool line.
func TestCheckpointIsKeptBeforeTheFirstHubRequest(t *testing.T) {
	var seen atomic.Int32
	var root string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		files, _ := filepath.Glob(filepath.Join(root, "spool", "hub.jsonl*"))
		for _, f := range files {
			if b, _ := os.ReadFile(f); bytes.Contains(b, []byte(`"codex:s1:t1"`)) {
				seen.Add(1)
			}
		}
		w.WriteHeader(200)
	}))
	defer hub.Close()
	root, p := hubRoot(t, hub.URL)
	c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
	c.Submit(p) // no local service here: the local copy is spooled
	if seen.Load() != 1 {
		t.Fatalf("at the hub request the checkpoint was on disk %d time(s), want 1", seen.Load())
	}
	if files, _ := filepath.Glob(filepath.Join(root, "spool", "hub.jsonl*")); len(files) != 0 {
		t.Fatalf("a delivered checkpoint left %v", files)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "spool", "pending.jsonl")); !bytes.Contains(b, []byte(`"codex:s1:t1"`)) {
		t.Fatal("the local copy is neither posted nor spooled")
	}
}

// keysHub is a hub that records every event's idempotency key and answers
// with status() (200 when nil).
func keysHub(t *testing.T, status func() int) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var keys []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			K string `json:"k"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		keys = append(keys, b.K)
		mu.Unlock()
		if status != nil {
			w.WriteHeader(status())
		}
	}))
	t.Cleanup(hub.Close)
	return hub, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), keys...)
	}
}

func rec(k string) []byte { return []byte(`{"k":"` + k + `"}`) }

// A run cut at any step of the hub delivery leaves the checkpoint for a
// later run: the state each cut leaves is set up here, and the next run
// delivers the record. A cut after the hub took the record, or after it
// was appended to the spool, delivers it twice; the hub keeps one event
// per idempotency key.
func TestACutAtAnyStepLeavesTheCheckpointForALaterRun(t *testing.T) {
	defer func(d time.Duration) { pendingStale = d }(pendingStale)
	pendingStale = 0
	for _, tc := range []struct {
		name              string
		onHub, inSpool    bool
		wantTotalOnTheHub int
	}{
		{"after the pending write, during the post, or after a failed post before the append", false, false, 1},
		{"after the hub took it, before the pending file is removed", true, false, 2},
		{"after the append, before the pending file is removed", false, true, 2},
	} {
		hub, keys := keysHub(t, nil)
		root, _ := hubRoot(t, hub.URL)
		spool := filepath.Join(root, "spool", "hub.jsonl")
		if _, err := writePending(spool, rec("cut")); err != nil {
			t.Fatal(err)
		}
		if tc.inSpool {
			appendLines(spool, rec("cut"))
		}
		before := 0
		if tc.onHub {
			before = 1
		}
		NewClient(root).WithDeadline(time.Now().Add(5*time.Second)).pushHub("", rec("next"))
		n := before
		for _, k := range keys() {
			if k == "cut" {
				n++
			}
		}
		if n != tc.wantTotalOnTheHub {
			t.Fatalf("%s: the hub got the record %d time(s), want %d (keys %v)", tc.name, n, tc.wantTotalOnTheHub, keys())
		}
		if left, _ := filepath.Glob(spool + "*"); len(left) != 0 {
			t.Fatalf("%s: left behind %v", tc.name, left)
		}
	}
	// A pending file younger than pendingStale belongs to a live run and
	// is left to it.
	pendingStale = time.Hour
	hub, keys := keysHub(t, nil)
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	live, _ := writePending(spool, rec("live"))
	NewClient(root).pushHub("", rec("next"))
	if _, err := os.Stat(live); err != nil || strings.Join(keys(), ",") != "next" {
		t.Fatalf("a live run's pending file was taken: %v, hub got %v", err, keys())
	}
}

// A hub that does not take the newest record leaves the backlog as it
// was, with the record appended in one write: the order on disk is the
// order of queueing, and a later run delivers its own record first and
// then the backlog in that order. A failing hub is asked once per run.
func TestAFailedPostKeepsTheQueueOrderAndAsksTheHubOnce(t *testing.T) {
	fail := false
	hub, keys := keysHub(t, func() int {
		if fail {
			return 503
		}
		return 200
	})
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	appendLines(spool, rec("A"))
	fail = true
	NewClient(root).pushHub("", rec("B"))
	if got := strings.Join(keys(), ","); got != "B" {
		t.Fatalf("the failing hub was asked %q, want only B", got)
	}
	if b, _ := os.ReadFile(spool); string(b) != string(rec("A"))+"\n"+string(rec("B"))+"\n" {
		t.Fatalf("the spool after a failed post: %q", b)
	}
	fail = false
	NewClient(root).pushHub("", rec("C"))
	if got := strings.Join(keys(), ","); got != "B,C,A,B" {
		t.Fatalf("delivery order %q, want B (refused), then C, A, B", got)
	}
	if left, _ := filepath.Glob(spool + "*"); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}

// Against a hub that does not answer, the run does no file work on the
// backlog: it is neither read nor rewritten, whatever its size.
func TestADownHubDoesNotTouchTheBacklog(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close() // refused from now on
	root, _ := hubRoot(t, url)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	var backlog bytes.Buffer
	for i := range 2000 {
		if i > 0 {
			backlog.WriteByte('\n')
		}
		backlog.Write(rec(fmt.Sprint(i)))
	}
	appendLines(spool, backlog.Bytes())
	// A second name for the same file: an append shows through it, a
	// claim by rename and a rewrite do not.
	link := spool + "-link"
	if err := os.Link(spool, link); err != nil {
		t.Skip("no hard links here:", err)
	}
	NewClient(root).WithDeadline(time.Now().Add(5*time.Second)).pushHub("", rec("new"))
	b, _ := os.ReadFile(spool)
	want := backlog.String() + "\n" + string(rec("new")) + "\n"
	if string(b) != want {
		t.Fatalf("the backlog changed: %d bytes, want %d", len(b), len(want))
	}
	appendLines(link, rec("marker")) // one file under two names, or two files?
	if b, _ := os.ReadFile(spool); !bytes.HasSuffix(b, append(rec("marker"), '\n')) {
		t.Fatal("the spool was replaced, not appended to")
	}
}

// TestHubPushHelperProcess is one hook run of
// TestConcurrentRunsKeepEveryRecordOnce in a process of its own.
func TestHubPushHelperProcess(t *testing.T) {
	root := os.Getenv("AIMEM_TEST_PUSH_ROOT")
	if root == "" {
		t.Skip("helper process only")
	}
	NewClient(root).WithDeadline(time.Now().Add(5*time.Second)).pushHub("", rec(os.Getenv("AIMEM_TEST_PUSH_KEY")))
}

// Two runs at once against a failing hub, ten times over: every record,
// the backlog's and each run's own, is queued exactly once.
func TestConcurrentRunsKeepEveryRecordOnce(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	root, _ := hubRoot(t, url)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	want := map[string]bool{}
	for i := range 200 {
		appendLines(spool, rec(fmt.Sprint("b", i)))
		want[fmt.Sprint("b", i)] = true
	}
	for round := range 10 {
		var wg sync.WaitGroup
		for j := range 2 {
			key := fmt.Sprintf("r%d-%d", round, j)
			want[key] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				cmd := exec.Command(os.Args[0], "-test.run=^TestHubPushHelperProcess$")
				cmd.Env = append(os.Environ(), "AIMEM_TEST_PUSH_ROOT="+root, "AIMEM_TEST_PUSH_KEY="+key)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("run %s: %v\n%s", key, err, out)
				}
			}()
		}
		wg.Wait()
	}
	got := map[string]int{}
	b, _ := os.ReadFile(spool)
	for line := range bytes.SplitSeq(b, []byte{'\n'}) {
		var r struct {
			K string `json:"k"`
		}
		if json.Unmarshal(line, &r) == nil {
			got[r.K]++
		}
	}
	for k := range want {
		if got[k] != 1 {
			t.Errorf("record %s is queued %d time(s), want 1", k, got[k])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d distinct records queued, want %d", len(got), len(want))
	}
	if left, _ := filepath.Glob(spool + ".*"); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

// projectWithTwoDocs is a project bound to proj-b with two changed bound
// documents.
func projectWithTwoDocs(t *testing.T) string {
	t.Helper()
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "docs"), 0o700)
	os.WriteFile(filepath.Join(proj, ".aimem.json"), []byte(`{"project":"proj-b","docs":["docs/NOTES.md"]}`), 0o600)
	os.WriteFile(filepath.Join(proj, "docs", "SESSION-STATE.md"), []byte("# state\n"), 0o600)
	os.WriteFile(filepath.Join(proj, "docs", "NOTES.md"), []byte("# notes\n"), 0o600)
	return proj
}

// A document PUT the hub fails with a server error stops the run's
// requests to that hub: the second document is not sent, both keep their
// old hashes, and the run's line names the one left.
func TestAFailedDocumentPutAsksTheHubNothingMore(t *testing.T) {
	var puts atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/docs/") {
			puts.Add(1)
			w.WriteHeader(503)
		}
	}))
	defer hub.Close()
	root, p := hubRoot(t, hub.URL)
	p.ProjectDir = projectWithTwoDocs(t)
	NewClient(root).WithDeadline(time.Now().Add(5 * time.Second)).Submit(p)
	if puts.Load() != 1 {
		t.Fatalf("the hub got %d document PUTs after a 503, want 1", puts.Load())
	}
	if st := loadDocSync(root, "proj-b"); len(st) != 0 {
		t.Fatalf("an unpublished document's hash moved: %v", st)
	}
	notes, _ := os.ReadFile(filepath.Join(root, "adapter.log"))
	if !bytes.Contains(notes, []byte("left work for a later run")) || !bytes.Contains(notes, []byte("the hub failed earlier in this run")) {
		t.Fatalf("adapter.log does not name the document left:\n%s", notes)
	}
}

// Outside a hook a timed-out PUT is the caller's error, and the document
// after it is reported as not published: `aimem docs push` never says
// "nothing to publish" for documents it could not send.
func TestAStandaloneDocsPushReportsATimeout(t *testing.T) {
	defer func(d time.Duration) { PushTimeout = d }(PushTimeout)
	PushTimeout = 300 * time.Millisecond
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer silent.Close()
	defer close(release)
	root, _ := hubRoot(t, silent.URL)
	res := NewClient(root).PublishDocs(projectWithTwoDocs(t), "proj-b", "", "test")
	if len(res) != 2 || res[0].Err == nil || res[1].Err == nil {
		t.Fatalf("results %+v, want an error for each document", res)
	}
}

// A run whose hub refuses the checkpoint names it in the run's line, with
// the event, the session and the turn, even when nothing ran out of time.
func TestARefusedHubIsNamedInTheRunsLine(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	root, p := hubRoot(t, url)
	p.ProjectDir = projectWithTwoDocs(t)
	NewClient(root).WithDeadline(time.Now().Add(5 * time.Second)).Submit(p)
	notes, _ := os.ReadFile(filepath.Join(root, "adapter.log"))
	for _, want := range []string{"turn event of session s1 turn t1 left work for a later run", "the hub delivery (", "publishing SESSION-STATE (the hub failed earlier in this run)"} {
		if !bytes.Contains(notes, []byte(want)) {
			t.Fatalf("adapter.log lacks %q:\n%s", want, notes)
		}
	}
}

// A hub that never answers costs the run no more than its deadline: the
// checkpoint stays queued, a changed bound document stays unpublished
// with its old hash, adapter.log says what was left, and the next run
// with the hub back delivers both.
func TestASilentHubIsBoundedByTheRunAndALaterRunDelivers(t *testing.T) {
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer silent.Close()
	defer close(release)
	root, p := hubRoot(t, silent.URL)
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "docs"), 0o700)
	os.WriteFile(filepath.Join(proj, ".aimem.json"), []byte(`{"project":"proj-b"}`), 0o600)
	os.WriteFile(filepath.Join(proj, "docs", "SESSION-STATE.md"), []byte("# state\n"), 0o600)
	p.ProjectDir = proj

	start := time.Now()
	c := NewClient(root).WithDeadline(start.Add(1500 * time.Millisecond))
	c.Submit(p)
	if took := time.Since(start); took > 4*time.Second { // the hub would hold it until the test ends
		t.Fatalf("the run took %v against a silent hub, past its deadline", took)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "spool", "hub.jsonl")); !bytes.Contains(b, []byte(`"codex:s1:t1"`)) {
		t.Fatalf("the checkpoint is not queued for the hub: %q", b)
	}
	notes, _ := os.ReadFile(filepath.Join(root, "adapter.log"))
	if !bytes.Contains(notes, []byte("left work for a later run")) || !bytes.Contains(notes, []byte("the hub delivery (out of time)")) {
		t.Fatalf("adapter.log does not say what was left:\n%s", notes)
	}
	if bytes.Contains(notes, []byte("# state")) {
		t.Fatal("adapter.log carries document content")
	}

	var events, docs atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/events":
			events.Add(1)
		case strings.Contains(r.URL.Path, "/docs/"):
			docs.Add(1)
			json.NewEncoder(w).Encode(HubDoc{Name: "SESSION-STATE", Rev: 1})
			return
		}
		w.WriteHeader(200)
	}))
	defer up.Close()
	if err := SaveHubs(root, map[string]*HubConfig{"default": {URL: up.URL, Token: "t"}}, "default"); err != nil {
		t.Fatal(err)
	}
	p.Event.TurnID, p.Event.IdempotencyKey = "t2", "codex:s1:t2"
	NewClient(root).WithDeadline(time.Now().Add(5 * time.Second)).Submit(p)
	if events.Load() != 2 {
		t.Fatalf("the next run delivered %d checkpoints, want the queued one and its own", events.Load())
	}
	if docs.Load() != 1 {
		t.Fatalf("the deferred document was published %d time(s) by the next run", docs.Load())
	}
}

// spoolLink gives a spool a second name: an append shows through it, a
// claim by rename or a rewrite does not. sameFile reports whether the two
// names still hold one file.
func spoolLink(t *testing.T, spool string) func() bool {
	t.Helper()
	link := spool + "-link"
	if err := os.Link(spool, link); err != nil {
		t.Skip("no hard links here:", err)
	}
	return func() bool {
		appendLines(link, rec("marker"))
		b, _ := os.ReadFile(spool)
		return bytes.HasSuffix(b, append(rec("marker"), '\n'))
	}
}

// resolved is the hub a state root resolves to.
func resolved(t *testing.T, root string) *HubConfig {
	t.Helper()
	_, h := ResolveHub(root, "")
	if h == nil {
		t.Fatal("no hub")
	}
	return h
}

// A run already out of time does not claim the local spool or the hub
// backlog: neither is renamed, read or rewritten.
func TestARunOutOfTimeClaimsNoSpool(t *testing.T) {
	hub, keys := keysHub(t, nil)
	root, _ := hubRoot(t, hub.URL)
	c := NewClient(root).WithDeadline(time.Now().Add(-time.Second))
	for i := range 5 {
		c.spool(rec(fmt.Sprint(i)))
	}
	local := spoolLink(t, c.spoolPath())
	if n, kept, err := c.ReplaySpool(); err != nil || n != 0 || kept != 0 {
		t.Fatalf("replay: %d sent, %d kept, %v", n, kept, err)
	}
	if !local() {
		t.Fatal("the local spool was claimed by a run out of time")
	}
	spool := filepath.Join(root, "spool", "hub.jsonl")
	appendLines(spool, rec("A"))
	backlog := spoolLink(t, spool)
	c.flushHubSpool(resolved(t, root), spool)
	if !backlog() || len(keys()) != 0 {
		t.Fatalf("the hub backlog was claimed by a run out of time; the hub got %v", keys())
	}
}

// A local replay that runs out of time partway writes back the records it
// kept and the untouched rest in one write, in their order, and removes
// its claim: nothing lost, nothing doubled.
func TestALocalReplayThatExpiresMidwayKeepsEverythingInOrder(t *testing.T) {
	root := t.TempDir()
	c := NewClient(root) // no local service here: every post fails at once
	var want bytes.Buffer
	for i := range 20000 {
		c.spool(rec(fmt.Sprint(i)))
		want.Write(rec(fmt.Sprint(i)))
		want.WriteByte('\n')
	}
	c.WithDeadline(time.Now().Add(20 * time.Millisecond))
	_, kept, err := c.ReplaySpool()
	if err != nil || kept != 20000 {
		t.Fatalf("replay: %d kept, %v", kept, err)
	}
	if !slices.Contains(c.deferredList, "the local spool replay (out of time)") {
		t.Skip("the replay finished before its deadline on this machine; the cut was not exercised")
	}
	if b, _ := os.ReadFile(c.spoolPath()); string(b) != want.String() {
		t.Fatalf("the spool after an expired replay differs: %d bytes, want %d", len(b), want.Len())
	}
	if left := spoolSiblings(c.spoolPath(), ".replay-"); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}

// The flush stops at its limit, and at a hub that slows past the run's
// deadline, and writes the record it stopped at and the rest back in
// order: what the hub got and what stays queued are the backlog, once.
func TestAFlushCutMidwayKeepsTheRestInOrder(t *testing.T) {
	for _, slow := range []bool{false, true} {
		var n atomic.Int32
		hub, keys := keysHub(t, func() int {
			if slow && n.Add(1) > 3 {
				time.Sleep(400 * time.Millisecond)
			}
			return 200
		})
		root, _ := hubRoot(t, hub.URL)
		spool := filepath.Join(root, "spool", "hub.jsonl")
		var all []string
		for i := range 150 {
			appendLines(spool, rec(fmt.Sprint(i)))
			all = append(all, fmt.Sprint(i))
		}
		c := NewClient(root)
		if slow {
			c.WithDeadline(time.Now().Add(1500 * time.Millisecond))
		}
		c.flushHubSpool(resolved(t, root), spool)
		got := keys()
		if !slow && len(got) != hubFlushLimit {
			t.Fatalf("the flush sent %d records, want its limit %d", len(got), hubFlushLimit)
		}
		var queued []string
		b, _ := os.ReadFile(spool)
		for line := range bytes.SplitSeq(bytes.TrimSpace(b), []byte{'\n'}) {
			var r struct {
				K string `json:"k"`
			}
			json.Unmarshal(line, &r)
			queued = append(queued, r.K)
		}
		// A request cut by the deadline may have reached the hub too.
		if d := len(got) + len(queued) - len(all); d < 0 || d > 1 || !reflect.DeepEqual(append(got[:len(got)-d:len(got)-d], queued...), all) {
			t.Fatalf("slow=%v: the hub got %d, %d stay queued; together they are not the backlog in order", slow, len(got), len(queued))
		}
		if left := spoolSiblings(spool, ".replay-"); len(left) != 0 {
			t.Fatalf("left behind %v", left)
		}
	}
}

// Without a pending file the record is still on disk before the hub is
// asked: appended to the spool, then posted. With nothing writable it is
// posted anyway, as its only chance, and the run says it was not kept.
func TestAFailedPendingWriteStillKeepsTheRecordFirst(t *testing.T) {
	defer func(f func(string, []byte) (string, error)) { pendingWrite = f }(pendingWrite)
	pendingWrite = func(string, []byte) (string, error) { return "", errors.New("no pending file") }

	// The spool writable, the hub up: posted from memory, and the flush
	// that follows delivers the spooled copy once more.
	var spoolAtRequest atomic.Int32
	var root string
	hub, keys := keysHub(t, func() int {
		if b, _ := os.ReadFile(filepath.Join(root, "spool", "hub.jsonl")); bytes.Contains(b, rec("x")) {
			spoolAtRequest.Add(1)
		}
		return 200
	})
	root, _ = hubRoot(t, hub.URL)
	NewClient(root).pushHub("", rec("x"))
	if spoolAtRequest.Load() == 0 || strings.Join(keys(), ",") != "x,x" {
		t.Fatalf("on disk at the request: %d; the hub got %v, want x twice", spoolAtRequest.Load(), keys())
	}

	// The spool writable, the hub down: queued once.
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	root2, _ := hubRoot(t, url)
	NewClient(root2).pushHub("", rec("y"))
	if b, _ := os.ReadFile(filepath.Join(root2, "spool", "hub.jsonl")); string(b) != string(rec("y"))+"\n" {
		t.Fatalf("the spool after a failed post: %q", b)
	}

	// Nothing writable (the spool path is a directory): posted directly,
	// and the run says the checkpoint is not kept on disk.
	hub3, keys3 := keysHub(t, nil)
	root3, _ := hubRoot(t, hub3.URL)
	os.MkdirAll(filepath.Join(root3, "spool", "hub.jsonl"), 0o700)
	NewClient(root3).pushHub("", rec("z"))
	notes, _ := os.ReadFile(filepath.Join(root3, "adapter.log"))
	if strings.Join(keys3(), ",") != "z" || !bytes.Contains(notes, []byte("not kept on disk")) {
		t.Fatalf("the hub got %v; adapter.log:\n%s", keys3(), notes)
	}
}

// The sweeps read the spool's directory literally: a state directory whose
// name holds pattern characters finds its own pending files and claims,
// and never another directory's.
func TestSweepsTakeTheStateDirectoryLiterally(t *testing.T) {
	defer func(d time.Duration) { pendingStale = d }(pendingStale)
	pendingStale = 0
	base := t.TempDir()
	hub, keys := keysHub(t, nil)
	mk := func(name string) string {
		root := filepath.Join(base, name)
		if err := SaveHubs(root, map[string]*HubConfig{"default": {URL: hub.URL, Token: "t"}}, "default"); err != nil {
			t.Fatal(err)
		}
		return root
	}
	odd, plain := mk("state[1]"), mk("state1")
	oddSpool := filepath.Join(odd, "spool", "hub.jsonl")
	plainSpool := filepath.Join(plain, "spool", "hub.jsonl")
	writePending(oddSpool, rec("odd"))
	writePending(plainSpool, rec("plain"))
	appendLines(oddSpool+".replay-99999999", rec("claimed"))
	NewClient(odd).pushHub("", rec("next"))
	got := strings.Join(keys(), ",")
	if !strings.Contains(got, "odd") || !strings.Contains(got, "claimed") || strings.Contains(got, "plain") {
		t.Fatalf("the hub got %q from the run in state[1]", got)
	}
	if left := spoolSiblings(plainSpool, ".pending-"); len(left) != 1 {
		t.Fatalf("the other directory's pending file was touched: %v", left)
	}
}

// A PUT whose answer is cut off mid-body is the hub's failure: the run
// sends no second document.
func TestACutOffPutAnswerAsksTheHubNothingMore(t *testing.T) {
	var puts atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/docs/") {
			puts.Add(1)
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(200)
			w.Write([]byte(`{"name":"SES`))
		}
	}))
	defer hub.Close()
	root, p := hubRoot(t, hub.URL)
	p.ProjectDir = projectWithTwoDocs(t)
	NewClient(root).WithDeadline(time.Now().Add(5 * time.Second)).Submit(p)
	if puts.Load() != 1 {
		t.Fatalf("the hub got %d PUTs after a cut-off answer, want 1", puts.Load())
	}
}

// A run whose only gap is one failed document, or a local post the
// service did not take, still writes the run's line naming it.
func TestOneFailedDocumentOrLocalPostIsNamed(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/docs/") {
			w.WriteHeader(503)
		}
	}))
	defer hub.Close()
	root, p := hubRoot(t, hub.URL)
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "docs"), 0o700)
	os.WriteFile(filepath.Join(proj, ".aimem.json"), []byte(`{"project":"proj-b"}`), 0o600)
	os.WriteFile(filepath.Join(proj, "docs", "SESSION-STATE.md"), []byte("# state\n"), 0o600)
	p.ProjectDir = proj
	NewClient(root).WithDeadline(time.Now().Add(5 * time.Second)).Submit(p) // no local service here
	notes, _ := os.ReadFile(filepath.Join(root, "adapter.log"))
	for _, want := range []string{"left work for a later run", "publishing SESSION-STATE (", "the local post ("} {
		if !bytes.Contains(notes, []byte(want)) {
			t.Fatalf("adapter.log lacks %q:\n%s", want, notes)
		}
	}
}

// A record of exactly maxLine bytes is kept by both readers, with or
// without its newline; one byte more is skipped. maxLine here is above
// the forward reader's buffer.
func TestARecordAtTheLineLimitIsKept(t *testing.T) {
	defer func(m int) { maxLine = m }(maxLine)
	maxLine = 2 << 20
	at := strings.Repeat("a", maxLine)
	past := strings.Repeat("b", maxLine+1)
	for _, body := range []string{"first\n" + at + "\n" + past + "\nlast\n", "first\n" + past + "\n" + at} {
		var fwd []string
		if err := forwardLines(strings.NewReader(body), time.Now().Add(time.Minute), func(line []byte) {
			fwd = append(fwd, string(line[:1]))
		}); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "r.jsonl")
		os.WriteFile(p, []byte(body), 0o600)
		f, _ := os.Open(p)
		var back []string
		err := scanBack(f, time.Now().Add(time.Minute), func(line []byte, _ int64) bool {
			back = append(back, string(line[:1]))
			return false
		})
		f.Close()
		if err != nil || !slices.Contains(fwd, "a") || slices.Contains(fwd, "b") || !slices.Contains(back, "a") || slices.Contains(back, "b") {
			t.Fatalf("forward %q, backward %q, %v", fwd, back, err)
		}
	}
}

// An overlong line is skipped whole by both readers, wherever the chunks
// of the backward read split it, and the forward read goes on after it.
func TestAnOverlongLineIsSkippedAtAnyChunkBoundary(t *testing.T) {
	defer func(m, c int) { maxLine, tailChunk = m, c }(maxLine, tailChunk)
	maxLine, tailChunk = 100, 64
	long := strings.Repeat("y", 101)
	for pad := range 200 {
		body := strings.Repeat("p", pad) + "\nfirst\n" + long + "\nlast\n"
		p := filepath.Join(t.TempDir(), "r.jsonl")
		os.WriteFile(p, []byte(body), 0o600)
		f, _ := os.Open(p)
		var back []string
		err := scanBack(f, time.Now().Add(time.Minute), func(line []byte, _ int64) bool {
			back = append(back, string(line))
			return false
		})
		f.Close()
		want := []string{"first", "last"}
		if pad > 0 {
			want = []string{strings.Repeat("p", pad), "first", "last"}
		}
		if pad > maxLine {
			want = want[1:]
		}
		if err != nil || !reflect.DeepEqual(back, reverse(want)) {
			t.Fatalf("pad %d: backward %q, %v", pad, back, err)
		}
		var fwd []string
		if err := forwardLines(strings.NewReader(body), time.Now().Add(time.Minute), func(line []byte) {
			fwd = append(fwd, string(line))
		}); err != nil || !reflect.DeepEqual(fwd, want) {
			t.Fatalf("pad %d: forward %q, %v", pad, fwd, err)
		}
	}
	// Past the forward reader's buffer, too.
	maxLine = 2 << 20
	body := "first\n" + strings.Repeat("z", 3<<20) + "\nlast\n"
	var fwd []string
	if err := forwardLines(strings.NewReader(body), time.Now().Add(time.Minute), func(line []byte) {
		fwd = append(fwd, string(line))
	}); err != nil || !reflect.DeepEqual(fwd, []string{"first", "last"}) {
		t.Fatalf("forward past the buffer: %.40q, %v", fwd, err)
	}
	if err := forwardLines(strings.NewReader("a\nb\n"), time.Now().Add(-time.Second), func([]byte) {}); err != errShare {
		t.Fatalf("a read past its share reports %v, want errShare", err)
	}
}

func reverse(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// An overlong line inside a Claude Code turn no longer ends the read: the
// turn's prompt and reply are still found.
func TestClaudeTurnWithAnOverlongLine(t *testing.T) {
	defer func(m int) { maxLine = m }(maxLine)
	maxLine = 4096
	lines := []string{
		`{"type":"user","message":{"content":"the prompt"}}`,
		`{"type":"assistant","uuid":"a1","message":{"content":[{"type":"text","text":"` + strings.Repeat("x", 5000) + `"}]}}`,
		`{"type":"assistant","uuid":"a2","message":{"content":[{"type":"text","text":"the reply"}]}}`,
	}
	p := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	u, r, _, id, _, err := parseTranscript(p, time.Now().Add(time.Minute))
	if err != nil || u != "the prompt" || r != "the reply" || id != "a2" {
		t.Fatalf("got %q %q %q, %v", u, r, id, err)
	}
}
