package textcheck

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"aimem/internal/provider"
)

// The pacer's base spacing is two seconds; these tests measure answers,
// not spacing.
func TestMain(m *testing.M) {
	os.Setenv("AIMEM_LLM_INTERVAL", "0")
	os.Setenv(FilterKey, "")
	os.Exit(m.Run())
}

// fakeModel serves chat completions with a fixed answer and status, and
// records what it was asked.
func fakeModel(t *testing.T, status int, reply string) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var calls atomic.Int32
	var asked atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			asked.Store(req.Model + "\n" + req.Messages[0].Content)
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": reply}}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &asked
}

func judgeFor(t *testing.T, url string) *Judge {
	t.Helper()
	t.Setenv("AIMEM_OPENAI_API_KEY", "test-key")
	t.Setenv("AIMEM_OPENAI_BASE_URL", url)
	t.Setenv(ModelKey, "small-model")
	j, why := ForRoot(t.TempDir())
	if j == nil {
		t.Fatalf("stage off: %s", why)
	}
	return j
}

// A model bound in the provider registry is reached through the provider
// dial guard: on loopback it is refused without the flag, before any
// request, and served with it.
func TestRegistryModelGoesThroughTheDialGuard(t *testing.T) {
	srv, calls, _ := fakeModel(t, 200, "READABLE")
	root := t.TempDir()
	reg, _ := json.Marshal(provider.Registry{
		Providers: map[string]provider.Provider{"local": {Kind: "openai", BaseURL: srv.URL, Token: "t"}},
		Models:    map[string]provider.Binding{"small-model": {Provider: "local"}},
	})
	if err := os.WriteFile(filepath.Join(root, "providers.json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ModelKey, "small-model")
	j, why := ForRoot(root)
	if j == nil {
		t.Fatalf("stage off: %s", why)
	}

	t.Setenv(provider.AllowPrivateEnv, "")
	if _, err := j.Readable(context.Background(), "CI409", "The run CI409 passed."); err == nil || !strings.Contains(err.Error(), provider.AllowPrivateEnv) {
		t.Fatalf("loopback registry model without the flag: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d requests reached the model without the flag", n)
	}

	t.Setenv(provider.AllowPrivateEnv, "1")
	if ok, err := j.Readable(context.Background(), "CI409", "The run CI409 passed."); err != nil || !ok {
		t.Fatalf("loopback registry model with the flag: %v, %v", ok, err)
	}
}

func TestReadableTakesOnlyAClearAnswer(t *testing.T) {
	for _, tc := range []struct {
		reply   string
		want    bool
		wantErr bool
	}{
		{"READABLE", true, false},
		{" readable.\n", true, false},
		{"SHORTHAND", false, false},
		{"**Shorthand**", false, false},
		{"It is mostly readable, I think.", false, true},
		{"", false, true},
	} {
		srv, _, asked := fakeModel(t, 200, tc.reply)
		got, err := judgeFor(t, srv.URL).Readable(context.Background(), "CI409", "The run CI409 passed.")
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("reply %q: readable=%v err=%v; want %v, error %v", tc.reply, got, err, tc.want, tc.wantErr)
		}
		q, _ := asked.Load().(string)
		if !strings.HasPrefix(q, "small-model\n") || !strings.Contains(q, "The flagged part: CI409") || !strings.Contains(q, "The run CI409 passed.") {
			t.Errorf("the question did not carry the model, the span and the context: %q", q)
		}
	}
}

func TestDailyCapStopsCalls(t *testing.T) {
	srv, calls, _ := fakeModel(t, 200, "READABLE")
	t.Setenv(CapKey, "2")
	j := judgeFor(t, srv.URL)
	for i := range 2 {
		if ok, err := j.Readable(context.Background(), "x", "x"); !ok || err != nil {
			t.Fatalf("call %d: readable=%v err=%v", i, ok, err)
		}
	}
	if _, err := j.Readable(context.Background(), "x", "x"); !errors.Is(err, ErrCapReached) {
		t.Fatalf("third call: err=%v; want the cap", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("the model was called %d times under a cap of 2", calls.Load())
	}
	if c := Describe(j, ""); c.CallsToday != 2 || c.DailyCalls != 2 || c.State != "on" {
		t.Fatalf("describe = %+v", c)
	}
}

func TestStageResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEM_OPENAI_API_KEY", "test-key")
	t.Setenv("AIMEM_OPENAI_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("AIMEM_CURATE_MODEL", "curate-model")

	// Unset or empty: off, whatever curation uses. The stage is off by
	// default, so no flagged text leaves the hub until the operator says.
	os.Unsetenv(ModelKey)
	if j, why := ForRoot(root); j != nil || why != ModelKey+" is not set" {
		t.Fatalf("unset key: judge %+v, why %q", j, why)
	}
	t.Setenv(ModelKey, " ")
	if j, why := ForRoot(root); j != nil || why != ModelKey+" is not set" {
		t.Fatalf("empty key: judge %+v, why %q", j, why)
	}
	// Its own model, separate from curation's.
	t.Setenv(ModelKey, "check-model")
	if j, _ := ForRoot(root); j == nil || j.Model != "check-model" {
		t.Fatalf("own key: judge %+v", j)
	}
	// A cap of zero is off; a malformed cap is off with the reason.
	t.Setenv(CapKey, "0")
	if j, why := ForRoot(root); j != nil || !strings.Contains(why, CapKey) {
		t.Fatalf("zero cap: judge %+v, why %q", j, why)
	}
	t.Setenv(CapKey, "lots")
	if j, why := ForRoot(root); j != nil || !strings.Contains(why, "lots") {
		t.Fatalf("bad cap: judge %+v, why %q", j, why)
	}
	t.Setenv(CapKey, "")
	// A model no endpoint serves is off, with the registry's reason.
	t.Setenv("AIMEM_OPENAI_API_KEY", "")
	if j, why := ForRoot(root); j != nil || !strings.Contains(why, "AIMEM_OPENAI_API_KEY") || !strings.Contains(why, "subscription CLI") {
		t.Fatalf("unresolved model: judge %+v, why %q", j, why)
	}
	if c := Describe(nil, "why"); c.State != "off" || c.Why != "why" || c.DailyCalls != DefaultDailyCalls {
		t.Fatalf("describe off = %+v", c)
	}
}

func TestFilterSwitch(t *testing.T) {
	for v, want := range map[string]bool{"": true, "on": true, " Off ": false, "off": false, "no": true} {
		t.Setenv(FilterKey, v)
		on, err := FilterOn()
		if on != want || (err != nil) != (v == "no") {
			t.Errorf("%s=%q: on %v, err %v", FilterKey, v, on, err)
		}
	}
	// Off turns the model stage off with it, whatever model is named.
	t.Setenv(FilterKey, "off")
	t.Setenv(ModelKey, "check-model")
	if j, why := ForRoot(t.TempDir()); j != nil || !strings.Contains(why, FilterKey) {
		t.Fatalf("filter off: judge %+v, why %q", j, why)
	}
	if c := Describe(nil, "why"); c.Filter != "off" || c.State != "off" {
		t.Fatalf("describe with the filter off = %+v", c)
	}
}

// Last in the file: a blocked answer widens the process-wide pacer, which
// would hold the tests after it.
func TestReadableFailsOnAnUpstreamError(t *testing.T) {
	for _, status := range []int{400, 401, 500} {
		srv, _, _ := fakeModel(t, status, "READABLE")
		if ok, err := judgeFor(t, srv.URL).Readable(context.Background(), "x", "x"); ok || err == nil {
			t.Errorf("HTTP %d: readable=%v err=%v; want an error", status, ok, err)
		}
	}
}
