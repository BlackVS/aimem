package main

import (
	"bytes"
	"errors"
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
	}{{[]string{"list", "alpha"}, "GET", "/v1/projects/alpha/teams"}, {[]string{"show", "alpha", "id"}, "GET", "/v1/projects/alpha/teams/id"}, {[]string{"events", "alpha", "id"}, "GET", "/v1/projects/alpha/teams/id/events"},
		{[]string{"events", "alpha", "id", "session_id=s1", "operation=team.assignment.", "limit=5"}, "GET", "/v1/projects/alpha/teams/id/events?limit=5&operation=team.assignment.&session_id=s1"},
		{[]string{"export", "alpha", "id"}, "GET", "/v1/projects/alpha/teams/id/export"},
		{[]string{"export", "alpha", "id", "include_bodies=true", "since=2026-09-22T00:00:00Z"}, "GET", "/v1/projects/alpha/teams/id/export?include_bodies=true&since=2026-09-22T00%3A00%3A00Z"}, {[]string{"create", "alpha", f, "retry"}, "POST", "/v1/projects/alpha/teams"}, {[]string{"configure", "alpha", "id", f, "retry"}, "PUT", "/v1/projects/alpha/teams/id"},
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
	for _, args := range [][]string{nil, {"create", "alpha"}, {"configure", "alpha", "id"}, {"list", "alpha", "extra"}, {"recover", "alpha", "id", "att"}, {"unmanage", "alpha", "id"}, {"rebind-token", "alpha", "id", "sess"},
		{"events", "alpha", "id", "foo=1"}, {"events", "alpha", "id", "session_id"}, {"events", "alpha", "id", "session_id="}, {"events", "alpha", "id", "limit=1", "limit=2"}, {"events", "alpha", "id", "include_bodies=true"}, {"export", "alpha", "id", "after=1"}} {
		if _, _, _, _, err := teamsRequest(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestExportPagesFollowsOneSnapshot(t *testing.T) {
	pages := map[string]string{
		"/x/export?limit=1": "{\"record\":\"header\",\"first_page\":true}\n{\"record\":\"event\",\"sequence\":1}\n{\"record\":\"message\",\"sequence\":1}\n{\"record\":\"end\",\"complete\":false,\"next\":{\"after_events\":1,\"after_messages\":1,\"snapshot_events\":2,\"snapshot_messages\":1}}\n",
		"/x/export?after_events=1&after_messages=1&limit=1&snapshot_events=2&snapshot_messages=1": "{\"record\":\"header\",\"first_page\":false}\n{\"record\":\"event\",\"sequence\":2}\n{\"record\":\"end\",\"complete\":true,\"next\":{\"after_events\":2,\"after_messages\":1,\"snapshot_events\":2,\"snapshot_messages\":1}}\n",
	}
	var requested []string
	get := func(path string) ([]byte, error) {
		requested = append(requested, path)
		body, ok := pages[path]
		if !ok {
			return nil, errors.New("unexpected " + path)
		}
		return []byte(body), nil
	}
	var out bytes.Buffer
	if err := exportPages(get, "/x/export?limit=1", &out); err != nil {
		t.Fatal(err, requested)
	}
	want := "{\"record\":\"header\",\"first_page\":true}\n{\"record\":\"event\",\"sequence\":1}\n{\"record\":\"message\",\"sequence\":1}\n{\"record\":\"event\",\"sequence\":2}\n{\"record\":\"end\",\"complete\":true,\"next\":{\"after_events\":2,\"after_messages\":1,\"snapshot_events\":2,\"snapshot_messages\":1}}\n"
	if out.String() != want || len(requested) != 2 {
		t.Fatalf("%q %v", out.String(), requested)
	}
	// A page whose cursor does not advance, or without an end record, stops the loop.
	stuck := func(string) ([]byte, error) {
		return []byte("{\"record\":\"end\",\"complete\":false,\"next\":{\"after_events\":0,\"after_messages\":0,\"snapshot_events\":0,\"snapshot_messages\":0}}"), nil
	}
	if err := exportPages(stuck, "/x/export?after_events=0&after_messages=0&snapshot_events=0&snapshot_messages=0", &out); err == nil {
		t.Fatal("stuck export accepted")
	}
	if err := exportPages(func(string) ([]byte, error) { return []byte("{\"record\":\"header\"}"), nil }, "/x/export", &out); err == nil {
		t.Fatal("missing end accepted")
	}
}
