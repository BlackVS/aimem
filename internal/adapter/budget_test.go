package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// The checkpoint is on disk, queued for the hub, before the hub is asked:
// whatever moment a run is cut at, it is in the hub spool or on the hub.
func TestCheckpointIsQueuedForTheHubBeforeTheFirstHubRequest(t *testing.T) {
	var seen atomic.Int32
	var root string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, _ := filepath.Glob(filepath.Join(root, "spool", "hub.jsonl*"))
		for _, c := range claims {
			if b, _ := os.ReadFile(c); bytes.Contains(b, []byte(`"codex:s1:t1"`)) {
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
	if b, err := os.ReadFile(filepath.Join(root, "spool", "hub.jsonl")); err == nil && len(bytes.TrimSpace(b)) > 0 {
		t.Fatalf("a delivered checkpoint stayed queued: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "spool", "pending.jsonl")); !bytes.Contains(b, []byte(`"codex:s1:t1"`)) {
		t.Fatal("the local copy is neither posted nor spooled")
	}
}

// A hub that never answers costs the run no more than its deadline: the
// checkpoint stays queued, a changed bound document stays unpublished
// with its old hash, adapter.log says what was deferred, and the next run
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
	if !bytes.Contains(notes, []byte("ran short of time")) || !bytes.Contains(notes, []byte("the hub delivery")) {
		t.Fatalf("adapter.log does not say what was deferred:\n%s", notes)
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

// The flush sends the newest checkpoint first and asks a failing hub only
// once per run.
func TestHubFlushNewestFirstAndOneFailurePerRun(t *testing.T) {
	var order []string
	fail := false
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			N int `json:"n"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		order = append(order, fmt.Sprint(b.N))
		if fail {
			w.WriteHeader(503)
		}
	}))
	defer hub.Close()
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	c := NewClient(root)
	for i := 1; i <= 3; i++ {
		c.spoolTo(spool, []byte(fmt.Sprintf(`{"n":%d}`, i)))
	}
	c.pushHub("", []byte(`{"n":4}`))
	if strings.Join(order, ",") != "4,1,2,3" {
		t.Fatalf("delivery order %v", order)
	}
	order, fail = nil, true
	for i := 5; i <= 7; i++ {
		c.spoolTo(spool, []byte(fmt.Sprintf(`{"n":%d}`, i)))
	}
	c2 := NewClient(root)
	c2.pushHub("", []byte(`{"n":8}`))
	if len(order) != 1 {
		t.Fatalf("a failing hub was asked %d times in one run", len(order))
	}
	b, _ := os.ReadFile(spool)
	if n := bytes.Count(b, []byte("\n")); n != 4 {
		t.Fatalf("%d records stayed queued, want 4", n)
	}
}
