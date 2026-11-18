package llmrate

import (
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

// A refusal is recorded for health with its time and message, leaves the
// spacing and the block count alone, survives into the next process, and
// is cleared by the next clean success.
func TestRefuseIsShownUntilASuccess(t *testing.T) {
	dir := t.TempDir()
	SetStateDir(dir)
	t.Cleanup(func() { SetStateDir("") })
	before := Status()
	Refuse(Refused(429, []byte(`{"error":{"message":"no credits","type":"insufficient_quota"}}`)))
	st := Status()
	if st["penalty_s"] != before["penalty_s"] || st["blocks"] != before["blocks"] {
		t.Fatalf("a refusal changed the pacing: %v -> %v", before, st)
	}
	lr, ok := st["last_refusal"].(map[string]string)
	if !ok || lr["at"] == "" || lr["message"] != "provider refused the account (HTTP 429, insufficient_quota): no credits" {
		t.Fatalf("last_refusal = %#v", st["last_refusal"])
	}
	if len(lr) != 2 {
		t.Fatalf("last_refusal carries more than its time and message: %#v", lr)
	}
	// Another process reads it from the state file.
	mu.Lock()
	lastRefusal = nil
	mu.Unlock()
	SetStateDir(dir)
	if _, ok := Status()["last_refusal"]; !ok {
		t.Fatal("the refusal was not persisted")
	}
	Recover()
	if _, ok := Status()["last_refusal"]; ok {
		t.Fatal("a clean success did not clear the refusal")
	}
}
