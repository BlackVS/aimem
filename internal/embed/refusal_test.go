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

// A refusal stays recorded through a later failed call, even one whose
// error body decodes, and is cleared by the next embedding call that
// returns its vectors.
func TestEmbedClearsTheRefusalOnlyOnSuccess(t *testing.T) {
	t.Cleanup(llmrate.SetLadderForTest(0, func(time.Duration) {}))
	answers := []struct {
		status int
		body   string
	}{
		{429, `{"error":{"message":"no credits","type":"insufficient_quota"}}`},
		{400, `{"error":{"message":"invalid model","type":"invalid_request_error"}}`},
		{200, `{"error":{"message":"proxy says no"}}`},
		{200, `{"data":[{"index":0,"embedding":[1,0]}]}`},
	}
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := answers[n.Add(1)-1]
		w.WriteHeader(a.status)
		w.Write([]byte(a.body))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Model: "m"}
	for i, wantKept := range []bool{true, true, true, false} {
		_, _, err := c.Embed([]string{"a"})
		_, kept := llmrate.Status()["last_refusal"]
		if kept != wantKept || (i < 3) != (err != nil) {
			t.Fatalf("answer %d: err %v, refusal kept %v, want %v", i, err, kept, wantKept)
		}
	}
}
