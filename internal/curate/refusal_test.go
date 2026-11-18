package curate

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"aimem/internal/llmrate"
)

// refusalRows are the upstream answers of the refusal tests: the ones
// waiting cannot cure, and the ones that stay a rate block.
var refusalRows = []struct {
	name    string
	status  int
	body    string
	refused bool
}{
	{"(a) insufficient_quota on 429", 429, `{"error":{"message":"You exceeded your current quota.","type":"insufficient_quota","code":"insufficient_quota"}}`, true},
	{"(a) proxy-wrapped insufficient_quota", 429, `{"error":{"message":"litellm.RateLimitError: {'type': 'insufficient_quota'}","type":"None"}}`, true},
	{"(b) payment required", 402, `{"error":{"message":"Insufficient credits"}}`, true},
	{"(c) key refused, 401", 401, `{"error":{"message":"Incorrect API key provided.","code":"invalid_api_key"}}`, true},
	{"(c) key refused, 403 with a JSON error", 403, `{"error":{"message":"key disabled"}}`, true},
	{"block: plain 429", 429, `{"error":{"message":"Rate limit reached","code":"rate_limit_exceeded"}}`, false},
	{"block: HTML 403", 403, `<!DOCTYPE html><html><title>Attention Required! | Cloudflare</title></html>`, false},
	{"block: 503", 503, `{"error":{"message":"overloaded"}}`, false},
}

// ladder runs the retry ladder with two retries and no waiting: the
// backoff is skipped, and the penalty each block earns is undone before
// the next attempt, so the test measures the requests, not the pacing.
func ladder(t *testing.T) func() {
	t.Helper()
	reset := func() {
		for i := 0; i < 20 && llmrate.Status()["penalty_s"].(float64) > 0; i++ {
			llmrate.Recover()
		}
	}
	t.Cleanup(llmrate.SetLadderForTest(2, func(time.Duration) { reset() }))
	t.Cleanup(reset)
	return reset
}

func blocks() int { return llmrate.Status()["blocks"].(int) }

// A completion refused for the account is asked once, is not counted as
// a rate block, and fails with the Refusal; a rate block takes the full
// ladder (three requests with two retries), as before.
func TestCompleteStopsAtARefusal(t *testing.T) {
	reset := ladder(t)
	for _, c := range refusalRows {
		t.Run(c.name, func(t *testing.T) {
			reset()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			b0 := blocks()
			_, _, err := (&OpenAIExtractor{BaseURL: srv.URL, Model: "m"}).Complete("p")
			_, refused := llmrate.AsRefusal(err)
			wantCalls, wantBlocks := int32(3), 3
			if c.refused {
				wantCalls, wantBlocks = 1, 0
			}
			if err == nil || refused != c.refused || calls.Load() != wantCalls || blocks()-b0 != wantBlocks {
				t.Fatalf("err %v (refused %v), %d requests, %d blocks; want refused %v, %d requests, %d blocks",
					err, refused, calls.Load(), blocks()-b0, c.refused, wantCalls, wantBlocks)
			}
		})
	}
}

// The claude CLI's answers: a spent credit balance is a Refusal and
// leaves the pacing alone; its usage limit stays a rate block.
func TestClaudeCLIRefusal(t *testing.T) {
	reset := ladder(t)
	for _, c := range []struct {
		result  string
		refused bool
	}{
		{"Credit balance is too low", true},
		{"Invalid API key · Please run /login", true},
		{"Claude AI usage limit reached|1763485200", false},
	} {
		reset()
		b0 := blocks()
		out := []byte(`{"type":"result","is_error":true,"result":"` + c.result + `"}`)
		_, _, err := (&ClaudeExtractor{}).finish(out)
		_, refused := llmrate.AsRefusal(err)
		wantBlocks := 1
		if c.refused {
			wantBlocks = 0
		}
		if err == nil || refused != c.refused || blocks()-b0 != wantBlocks {
			t.Errorf("%q: err %v, refused %v, %d blocks", c.result, err, refused, blocks()-b0)
		}
	}
}
