package curate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"aimem/internal/llmrate"
	"aimem/internal/provider"
)

// Curation's completion call goes through the provider dial guard: a
// model server on loopback is refused without the flag, before any
// request is sent, and served with it.
func TestCompleteRefusesPrivateUnlessAllowed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"choices":[{"message":{"content":"fine"}}]}`))
	}))
	defer srv.Close()
	x := &OpenAIExtractor{BaseURL: srv.URL, Model: "m"}

	t.Setenv(provider.AllowPrivateEnv, "")
	if _, _, err := x.Complete("p"); err == nil || !strings.Contains(err.Error(), provider.AllowPrivateEnv) {
		t.Fatalf("loopback completion without the flag: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d requests reached the server without the flag", n)
	}

	t.Setenv(provider.AllowPrivateEnv, "1")
	if got, _, err := x.Complete("p"); err != nil || got != "fine" {
		t.Fatalf("loopback completion with the flag: %q, %v", got, err)
	}

	// The host's own AIMEM_OPENAI_* pair is not guarded.
	t.Setenv(provider.AllowPrivateEnv, "")
	env := &OpenAIExtractor{BaseURL: srv.URL, Model: "m", FromEnv: true}
	if got, _, err := env.Complete("p"); err != nil || got != "fine" {
		t.Fatalf("env-pair completion without the flag: %q, %v", got, err)
	}
}

// A 200 completion whose content mentions Cloudflare is an answer, not a
// block page: it is returned at once, with no retry and no widened
// spacing. Curating a project that discusses its proxy produces exactly
// such answers.
func TestCompleteAnswerMentioningCloudflareIsNotABlock(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"choices":[{"message":{"content":"Attention Required pages come from the Cloudflare proxy in front of <html> routes."}}],` +
			`"usage":{"prompt_tokens":5,"completion_tokens":9}}`))
	}))
	defer srv.Close()
	before := llmrate.Status()["blocks"]
	got, _, err := (&OpenAIExtractor{BaseURL: srv.URL, Model: "m"}).Complete("p")
	if err != nil || !strings.Contains(got, "Cloudflare proxy") {
		t.Fatalf("Complete = %q, %v; want the answer", got, err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d requests; a successful answer must not be retried", n)
	}
	if after := llmrate.Status()["blocks"]; after != before {
		t.Errorf("blocks %v -> %v; a successful answer must not be penalised", before, after)
	}
}

// The same words in a failed response, or an HTML page where the JSON
// belongs, are still blocks.
func TestCompleteBlockPagesStillBlock(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"html page on 200", 200, `<!DOCTYPE html><html><title>Attention Required! | Cloudflare</title></html>`},
		{"proxy error carrying a block page", 400, `{"error":{"message":"upstream: <html>Attention Required! | Cloudflare</html>"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			if llmrate.Retries() != 0 {
				t.Fatalf("llmrate retries = %d; TestMain pins AIMEM_LLM_RETRIES=0", llmrate.Retries())
			}
			// The pacer is process-wide: undo this block's penalty, so later
			// tests are not spaced out by it.
			t.Cleanup(func() {
				for i := 0; i < 10 && llmrate.Status()["penalty_s"].(float64) > 0; i++ {
					llmrate.Recover()
				}
			})
			before := llmrate.Status()["blocks"].(int)
			_, _, err := (&OpenAIExtractor{BaseURL: srv.URL, Model: "m"}).Complete("p")
			if err == nil || !strings.Contains(err.Error(), "blocked upstream") {
				t.Fatalf("Complete err = %v; want a block", err)
			}
			if after := llmrate.Status()["blocks"].(int); after != before+1 {
				t.Errorf("blocks %d -> %d; a block page must be penalised once", before, after)
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("%d requests with retries pinned to 0; want 1", n)
			}
		})
	}
}
