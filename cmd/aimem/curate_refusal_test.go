package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"aimem/internal/llmrate"
	"aimem/internal/store"
)

// `aimem curate --all` stops at the first store the provider refuses for
// the account: it asks no further store, prints the one line, records the
// refusal for health, and exits with llmrate.ExitRefused. An ordinary
// failure still goes on to the next store and ends as a success.
func TestCurateAllStopsAtARefusal(t *testing.T) {
	for _, c := range []struct {
		name      string
		status    int
		body      string
		wantCalls int32
		wantExit  int
	}{
		{"refused for the account", 429, `{"error":{"message":"This account has no credits remaining.","type":"insufficient_quota"}}`, 1, llmrate.ExitRefused},
		{"an ordinary failure", 500, `{"error":{"message":"internal error"}}`, 2, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer ts.Close()
			state := t.TempDir()
			reg, err := store.NewRegistry(state)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{"store-one", "store-two"} {
				db, err := reg.Open(p)
				if err != nil {
					t.Fatal(err)
				}
				seedEvent(t, db, p+"-1")
			}
			reg.Close()
			env := []string{"AIMEM_CURATE_MODEL=fake", "AIMEM_OPENAI_BASE_URL=" + ts.URL, "AIMEM_OPENAI_API_KEY=k",
				"AIMEM_EMBED_MODEL=", "AIMEM_LLM_INTERVAL=0", "AIMEM_LLM_RETRIES=0"}
			_, errOut, err := runAimemEnv(t, t.TempDir(), state, "", env, "curate", "--backend", "openai", "--all")
			exit := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exit = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if exit != c.wantExit || calls.Load() != c.wantCalls {
				t.Fatalf("exit %d after %d requests, want exit %d after %d\n%s", exit, calls.Load(), c.wantExit, c.wantCalls, errOut)
			}
			const line = "aimem: provider refused the account (HTTP 429, insufficient_quota): This account has no credits remaining. Waiting will not cure this: the administrator must restore the provider account's credit or key.\n"
			raw, _ := os.ReadFile(filepath.Join(state, "llmrate.json"))
			if c.wantExit == llmrate.ExitRefused {
				if strings.Count(errOut, line) != 1 || strings.Contains(errOut, "retrying") {
					t.Fatalf("stderr does not carry the line once:\n%s", errOut)
				}
				if !strings.Contains(string(raw), `"last_refusal"`) {
					t.Fatalf("the refusal is not recorded for health: %s", raw)
				}
			} else if strings.Contains(errOut, "refused the account") || strings.Contains(string(raw), `"last_refusal"`) {
				t.Fatalf("an ordinary failure reads as a refusal:\n%s\n%s", errOut, raw)
			}
		})
	}
}
