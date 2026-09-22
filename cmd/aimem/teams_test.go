package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTeamsRequest(t *testing.T) {
	f := filepath.Join(t.TempDir(), "team.json")
	if err := os.WriteFile(f, []byte(`{"name":"team"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args         []string
		method, path string
	}{{[]string{"list", "alpha"}, "GET", "/v1/projects/alpha/teams"}, {[]string{"show", "alpha", "id"}, "GET", "/v1/projects/alpha/teams/id"}, {[]string{"events", "alpha", "id"}, "GET", "/v1/projects/alpha/teams/id/events"}, {[]string{"create", "alpha", f, "retry"}, "POST", "/v1/projects/alpha/teams"}, {[]string{"configure", "alpha", "id", f, "retry"}, "PUT", "/v1/projects/alpha/teams/id"},
		{[]string{"recover", "alpha", "id", "att", f, "retry"}, "POST", "/v1/projects/alpha/teams/id/assignments/att/recover"},
		{[]string{"unmanage", "alpha", "id", "task", f, "retry"}, "POST", "/v1/projects/alpha/teams/id/tasks/task/unmanage"},
		{[]string{"rebind-token", "alpha", "id", "sess", f, "retry"}, "POST", "/v1/projects/alpha/teams/id/sessions/sess/rebind-token"}} {
		m, p, _, k, e := teamsRequest(tc.args)
		if e != nil || m != tc.method || p != tc.path {
			t.Fatalf("%v: %s %s %v", tc.args, m, p, e)
		}
		if m != "GET" && k != "retry" {
			t.Fatalf("lost key %s", k)
		}
	}
	for _, args := range [][]string{nil, {"create", "alpha"}, {"configure", "alpha", "id"}, {"list", "alpha", "extra"}, {"recover", "alpha", "id", "att"}, {"unmanage", "alpha", "id"}, {"rebind-token", "alpha", "id", "sess"}} {
		if _, _, _, _, err := teamsRequest(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
