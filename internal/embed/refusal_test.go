package embed

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"aimem/internal/llmrate"
)

// An embedding call refused for the account is asked once, is not
// counted as a rate block, and fails with the Refusal; a rate block takes
// the full ladder (three requests with two retries), as before.
func TestEmbedStopsAtARefusal(t *testing.T) {
	reset := func() {
		for i := 0; i < 20 && llmrate.Status()["penalty_s"].(float64) > 0; i++ {
			llmrate.Recover()
		}
	}
	t.Cleanup(llmrate.SetLadderForTest(2, func(time.Duration) { reset() }))
	t.Cleanup(reset)
	blocks := func() int { return llmrate.Status()["blocks"].(int) }
	for _, c := range []struct {
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
	} {
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
			_, _, err := (&Client{BaseURL: srv.URL, Model: "m"}).Embed([]string{"a"})
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
