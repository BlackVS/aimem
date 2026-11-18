package llmrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each answer the provider gives when waiting cannot cure the failure is
// a Refusal; each answer that is a rate block, or no failure, is not.
func TestRefusedTellsTheAccountFromARateBlock(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
		kind   string // "" = not a Refusal
	}{
		{"insufficient_quota type on 429", 429, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`, "insufficient_quota"},
		{"insufficient type, other code", 429, `{"error":{"message":"This account has no credits remaining.","type":"insufficient_credits","code":null}}`, "insufficient_credits"},
		{"insufficient code only", 429, `{"error":{"message":"no quota","type":"requests","code":"insufficient_quota"}}`, "insufficient_quota"},
		{"proxy wraps insufficient_quota in its message", 429, `{"error":{"message":"litellm.RateLimitError: OpenAIException - {'error': {'type': 'insufficient_quota'}}","type":"None","code":"429"}}`, "insufficient_quota"},
		{"payment required", 402, `{"error":{"message":"Insufficient credits"}}`, "payment required"},
		{"payment required without a body", 402, ``, "payment required"},
		{"key refused", 401, `{"error":{"message":"Incorrect API key provided.","type":"invalid_request_error","code":"invalid_api_key"}}`, "key refused"},
		{"key refused, bare string error", 401, `{"error":"invalid key"}`, "key refused"},
		{"forbidden with a JSON error", 403, `{"error":{"message":"Your key is disabled."}}`, "key refused"},

		{"plain rate limit", 429, `{"error":{"message":"Rate limit reached for requests","type":"requests","code":"rate_limit_exceeded"}}`, ""},
		{"429 without a body", 429, ``, ""},
		{"block page on 403", 403, `<!DOCTYPE html><html><title>Attention Required! | Cloudflare</title></html>`, ""},
		{"server error", 503, `{"error":{"message":"overloaded"}}`, ""},
		{"budget of a proxy key", 400, `{"error":{"message":"Budget has been exceeded!","type":"budget_exceeded"}}`, ""},
		{"an answer about quotas", 200, `{"choices":[{"message":{"content":"insufficient_quota is OpenAI's error type"}}]}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := Refused(c.status, []byte(c.body))
			switch {
			case c.kind == "" && r != nil:
				t.Fatalf("a Refusal: %v", r)
			case c.kind != "" && r == nil:
				t.Fatal("not a Refusal")
			case r != nil && (r.Kind != c.kind || r.Status != c.status):
				t.Fatalf("got %q at %d, want %q", r.Kind, r.Status, c.kind)
			}
		})
	}
}

// The claude CLI's usage limit resets with the subscription window, so
// it stays a rate block; a spent credit balance or a refused key do not.
func TestRefusedCLI(t *testing.T) {
	for msg, want := range map[string]bool{
		"completion turn errored: Credit balance is too low": true,
		"API Error: 401 Invalid API key · Please run /login": true,
		"Claude AI usage limit reached|1763485200":           false,
		"API Error: 429 rate_limit_error":                    false,
	} {
		if got := RefusedCLI(msg) != nil; got != want {
			t.Errorf("%q: refused %v, want %v", msg, got, want)
		}
	}
}

// The line names the provider's answer, says what it means, and stays
// one line however the provider formats its message.
func TestRefusalLine(t *testing.T) {
	r := Refused(429, []byte(`{"error":{"message":"no credits\nremaining   for this account","type":"insufficient_quota"}}`))
	line := r.Line()
	want := "aimem: provider refused the account (HTTP 429, insufficient_quota): no credits remaining for this account. Waiting will not cure this: the administrator must restore the provider account's credit or key."
	if line != want {
		t.Fatalf("line:\n%s\nwant:\n%s", line, want)
	}
	long := Refused(402, []byte(`{"error":{"message":"`+strings.Repeat("x", 500)+`"}}`))
	if len(long.Message) > 220 || strings.Contains(long.Line(), "\n") {
		t.Fatalf("the message is not clipped to one short line: %d bytes", len(long.Message))
	}
}

// refusalsByKind reads health's records as kind -> message.
func refusalsByKind(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	rs, _ := Status()["last_refusal"].([]map[string]string)
	for _, r := range rs {
		if len(r) != 3 || r["at"] == "" {
			t.Fatalf("a record carries more or less than its kind, time and message: %#v", r)
		}
		out[r["kind"]] = r["message"]
	}
	return out
}

// A refusal is recorded for health under its kind of call, with its time
// and message; it leaves the spacing and the block count alone, survives
// into the next process, and is cleared only by a success of the same
// kind: neither the pacing recovery, which also follows a decoded error
// answer, nor a success of another kind clears it.
func TestRefuseIsShownUntilASuccessOfItsKind(t *testing.T) {
	dir := t.TempDir()
	SetStateDir(dir)
	t.Cleanup(func() { SetStateDir("") })
	before := Status()
	Refuse(CallCompletions, Refused(429, []byte(`{"error":{"message":"no credits","type":"insufficient_quota"}}`)))
	st := Status()
	if st["penalty_s"] != before["penalty_s"] || st["blocks"] != before["blocks"] {
		t.Fatalf("a refusal changed the pacing: %v -> %v", before, st)
	}
	if got := refusalsByKind(t); len(got) != 1 || got[CallCompletions] != "provider refused the account (HTTP 429, insufficient_quota): no credits" {
		t.Fatalf("records = %#v", got)
	}
	// Another process reads it from the state file.
	mu.Lock()
	refusals = nil
	mu.Unlock()
	SetStateDir(dir)
	if _, ok := refusalsByKind(t)[CallCompletions]; !ok {
		t.Fatal("the refusal was not persisted")
	}
	Recover()
	Succeeded(CallEmbeddings)
	if _, ok := refusalsByKind(t)[CallCompletions]; !ok {
		t.Fatal("the pacing recovery or a success of another kind cleared the refusal")
	}
	Succeeded(CallCompletions)
	if got := refusalsByKind(t); len(got) != 0 {
		t.Fatalf("a success of its kind did not clear the refusal: %#v", got)
	}
}

// A refused key keeps only its status and kind in the record: the
// provider's message can quote part of the key. The line printed to the
// journal keeps the provider's text.
func TestAKeyRefusalRecordsNoProviderText(t *testing.T) {
	SetStateDir(t.TempDir())
	t.Cleanup(func() { SetStateDir(""); Succeeded(CallEmbeddings); Succeeded(CallClaudeCLI) })
	r := Refused(401, []byte(`{"error":{"message":"Incorrect API key provided: sk-proj-****9XYZ.","code":"invalid_api_key"}}`))
	Refuse(CallEmbeddings, r)
	Refuse(CallClaudeCLI, RefusedCLI("Invalid API key sk-ant-****1234 · Please run /login"))
	got := refusalsByKind(t)
	if got[CallEmbeddings] != "provider refused the account (HTTP 401, key refused)" ||
		got[CallClaudeCLI] != "provider refused the account (claude CLI, invalid api key)" {
		t.Fatalf("records = %#v", got)
	}
	if raw, _ := os.ReadFile(filepath.Join(stateDir, "llmrate.json")); strings.Contains(string(raw), "9XYZ") || strings.Contains(string(raw), "1234") {
		t.Fatalf("the state file carries the key's text: %s", raw)
	}
	if !strings.Contains(r.Line(), "sk-proj-****9XYZ") {
		t.Fatalf("the journal line lost the provider's text: %s", r.Line())
	}
}

// otherProcess writes the state file as another aimem process would: it
// changes the file without touching this process's memory.
func otherProcess(t *testing.T, change func(*state)) {
	t.Helper()
	path := filepath.Join(stateDir, "llmrate.json")
	var s state
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
	}
	change(&s)
	raw, _ := json.Marshal(s)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A save by one process never drops a record another process wrote to
// the shared state file since, and never brings back one it cleared.
// Each of the four savers is checked against both cases.
func TestASaveKeepsWhatAnotherProcessChanged(t *testing.T) {
	saves := map[string]func(){
		"Penalize":            func() { Penalize("test") },
		"Recover":             func() { Penalize("test"); Recover() },
		"Refuse another kind": func() { Refuse(CallTextCheck, Refused(402, nil)) },
		"Succeeded another":   func() { Refuse(CallTextCheck, Refused(402, nil)); Succeeded(CallTextCheck) },
	}
	for name, save := range saves {
		t.Run(name, func(t *testing.T) {
			SetStateDir(t.TempDir())
			t.Cleanup(func() {
				SetStateDir("")
				mu.Lock()
				refusals, penalty = nil, 0
				mu.Unlock()
			})
			// The other process records a refusal after this one read the file.
			otherProcess(t, func(s *state) {
				s.Refusals = map[string]*refusalRecord{CallCompletions: {At: "2026-11-18T10:00:00Z", Message: "refused"}}
			})
			save()
			if _, ok := refusalsByKind(t)[CallCompletions]; !ok {
				t.Fatal("the save dropped the other process's record")
			}
			// The mirror case: the other process clears it, this one saves again.
			otherProcess(t, func(s *state) { delete(s.Refusals, CallCompletions) })
			save()
			if _, ok := refusalsByKind(t)[CallCompletions]; ok {
				t.Fatal("the save brought back the record the other process cleared")
			}
		})
	}
}
