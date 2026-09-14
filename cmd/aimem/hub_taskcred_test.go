package main

import (
	"strings"
	"testing"

	"aimem/internal/adapter"
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
