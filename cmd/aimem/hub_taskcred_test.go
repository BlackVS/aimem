package main

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/hubtest"
)

// `aimem hub task-token` stores only an ordinary token, on a known hub,
// and the credential survives a same-host re-add but not a re-point.
func TestHubTaskTokenCommand(t *testing.T) {
	t.Setenv("AIMEM_STATE_DIR", t.TempDir())
	if err := hubCmd([]string{"add", "home", "https://hub.example", "checkpoint-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := hubCmd([]string{"task-token", "home", "checkpoint-secret"}); err == nil || !strings.Contains(err.Error(), "ordinary token") {
		t.Fatalf("checkpoint token accepted as task credential: %v", err)
	}
	if err := hubCmd([]string{"task-token", "nope", "aimem_user_abc"}); err == nil || !strings.Contains(err.Error(), "no hub named") {
		t.Fatalf("unknown hub: %v", err)
	}
	if err := hubCmd([]string{"task-token", "home"}); err == nil {
		t.Fatal("missing token accepted")
	}
	if err := hubCmd([]string{"task-token", "home", "aimem_user_abc"}); err != nil {
		t.Fatal(err)
	}
	hubs, _ := adapter.LoadHubs(stateRoot())
	if hubs["home"].TaskToken != "aimem_user_abc" || hubs["home"].Token != "checkpoint-secret" {
		t.Fatalf("stored: %+v", hubs["home"])
	}
	if err := hubCmd([]string{"add", "home", "https://hub.example", "rotated"}); err != nil {
		t.Fatal(err)
	}
	hubs, _ = adapter.LoadHubs(stateRoot())
	if hubs["home"].TaskToken != "aimem_user_abc" || hubs["home"].Token != "rotated" {
		t.Fatalf("same-host re-add must keep the task credential: %+v", hubs["home"])
	}
	if err := hubCmd([]string{"add", "home", "https://other.example", "rotated"}); err != nil {
		t.Fatal(err)
	}
	hubs, _ = adapter.LoadHubs(stateRoot())
	if hubs["home"].TaskToken != "" {
		t.Fatalf("re-pointed hub must not carry the credential: %+v", hubs["home"])
	}
}

// A task credential stored on an entry the strict client refuses is kept,
// with a warning that it will not be sent there; a verified entry gets
// none. Nothing is sent to the hub either way.
func TestHubTaskTokenWarnsOnAnUntrustedEntry(t *testing.T) {
	var warned bytes.Buffer
	saved := hubWarnOut
	hubWarnOut = &warned
	t.Cleanup(func() { hubWarnOut = saved })
	state := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", state)
	srv := hubtest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("hub task-token reached the hub") }))
	defer srv.Close()
	missing := filepath.Join(t.TempDir(), "absent.pem")
	for name, tc := range map[string]struct {
		h    *adapter.HubConfig
		want string
	}{
		"http":     {&adapter.HubConfig{URL: "http://hub.example:8440", Token: "c"}, "aimem hub list"},
		"insecure": {&adapter.HubConfig{URL: srv.URL, Token: "c", Insecure: true}, "configured with insecure"},
		"ca_file":  {&adapter.HubConfig{URL: srv.URL, Token: "c", CAFile: missing}, "absent.pem"},
		"pinned":   {&adapter.HubConfig{URL: srv.URL, Token: "c", Pin: hubtest.Pin(srv)}, ""},
	} {
		warned.Reset()
		if err := adapter.SaveHubs(state, map[string]*adapter.HubConfig{"home": tc.h}, "home"); err != nil {
			t.Fatal(err)
		}
		if err := hubCmd([]string{"task-token", "home", "aimem_user_abc"}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if hubs, _ := adapter.LoadHubs(state); hubs["home"].TaskToken != "aimem_user_abc" {
			t.Fatalf("%s: not stored: %+v", name, hubs["home"])
		}
		got := warned.String()
		if tc.want == "" {
			if got != "" {
				t.Fatalf("%s: warned: %q", name, got)
			}
			continue
		}
		if !strings.HasPrefix(got, "WARNING: hub home: the task credential is stored, but aimem will not send it to this entry: ") ||
			!strings.Contains(got, tc.want) || strings.Contains(got, "aimem_user_abc") {
			t.Fatalf("%s: %q", name, got)
		}
	}
}
