package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aimem/internal/adapter"
)

// refusingHub answers every request with status and body, and counts
// the PUTs it receives.
func refusingHub(t *testing.T, status int, body string) (*adapter.HubConfig, *atomic.Int32) {
	t.Helper()
	var puts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return &adapter.HubConfig{URL: ts.URL, Token: "t"}, &puts
}

// Only the no_such_project code means "the hub holds no such group". A
// wrong token's 401 and a 400 without the code are reported as what they
// are on all three paths, and publish does not go on to the PUT.
func TestGroupHubRefusalIsNotMissing(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
		status           int
	}{
		{"wrong token", "", "hub HTTP 401", http.StatusUnauthorized},
		{"400 without the code", "{\"error\":\"bad\\u001b[2J request\"}", `hub HTTP 400 on /v1/projects/group-shared/meta/about: "bad\x1b[2J request"`, http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			hub, puts := refusingHub(t, c.status, c.body)
			groupMachine(t, hub, map[string]string{"about": "local", "chapters": chaptersA})
			for _, args := range [][]string{{"shared", "--adopt"}, {"shared", "--hub", "primary"}, {"shared", "--publish"}} {
				out, _, err := captureOut(t, func() error { return groupCmd(args) })
				all := out
				if err != nil {
					all += err.Error()
				}
				if err == nil || strings.Contains(all, "holds no group") || !strings.Contains(all, c.want) {
					t.Fatalf("%v: err %v\n%s", args, err, out)
				}
				if strings.Contains(all, "\x1b") {
					t.Fatalf("%v printed the hub's control character raw:\n%q", args, all)
				}
			}
			if n := puts.Load(); n != 0 {
				t.Fatalf("publish sent %d PUT(s) after the hub refused the read", n)
			}
		})
	}
}

// A hub that is down, or accepts the request and never answers, stops
// the loop at the first key: the rest are not tried, each waits no
// longer than the meta timeout, and the command fails.
func TestGroupStopsWhenHubUnreachable(t *testing.T) {
	old := groupMetaTimeout
	groupMetaTimeout = 300 * time.Millisecond
	t.Cleanup(func() { groupMetaTimeout = old })

	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(hang.Close)

	for name, url := range map[string]string{"down": down.URL, "hanging": hang.URL} {
		t.Run(name, func(t *testing.T) {
			groupMachine(t, &adapter.HubConfig{URL: url, Token: "t"}, map[string]string{
				"about": "a", "policy": "all", "chapters": chaptersA, "features": `["f1"]`,
			})
			for _, mode := range []string{"--publish", "--adopt"} {
				start := time.Now()
				out, _, err := captureOut(t, func() error { return groupCmd([]string{"shared", mode}) })
				if err == nil || !strings.Contains(err.Error(), "4 of 4 keys failed") {
					t.Fatalf("%s: err %v\n%s", mode, err, out)
				}
				if strings.Count(out, "this and the remaining keys were not tried") != 1 || strings.Contains(out, "policy:") {
					t.Fatalf("%s did not stop at the first key:\n%s", mode, out)
				}
				if d := time.Since(start); d > 5*time.Second {
					t.Fatalf("%s took %v", mode, d)
				}
			}
			// The push after an edit stops the same way and reports once.
			_, stderr, err := captureOut(t, func() error { return groupCmd([]string{"shared", "--about", "b"}) })
			if err != nil || strings.Count(stderr, "not tried") != 1 || !strings.Contains(stderr, "4 key(s) did not reach hub") {
				t.Fatalf("edit against an unreachable hub: %v\n%s", err, stderr)
			}
		})
	}
}

// Values that came from the hub reach the terminal quoted: a control
// character in a hub value is shown escaped by publish and by adopt.
func TestGroupHubValuesPrintQuoted(t *testing.T) {
	hostile := "[\"x\x1b[2J\"]"
	_, hub := groupHub(t, map[string]string{"features": hostile})
	groupMachine(t, hub, map[string]string{"features": `["f1"]`})
	out, _, err := captureOut(t, func() error { return groupCmd([]string{"shared", "--publish"}) })
	if err != nil || strings.Contains(out, "\x1b") || !strings.Contains(out, `features: replaced on the hub ("[\"x\x1b[2J\"]" -> "[\"f1\"]")`) {
		t.Fatalf("publish: %v\n%q", err, out)
	}
	// Adopt refuses both (a raw control character is not valid JSON, and
	// the policy is no policy) and quotes the policy it shows.
	_, hub2 := groupHub(t, map[string]string{"features": hostile, "policy": "all\x1b[2J"})
	reg := groupMachine(t, hub2, map[string]string{"features": `["f1"]`, "policy": "domain"})
	out, _, err = captureOut(t, func() error { return groupCmd([]string{"shared", "--adopt"}) })
	if err == nil || strings.Contains(out, "\x1b") || !strings.Contains(out, `the hub holds "all\x1b[2J"`) ||
		!strings.Contains(out, "features: refused") {
		t.Fatalf("adopt: %v\n%q", err, out)
	}
	if got := getMeta(t, reg, "group-shared", "features"); got != `["f1"]` {
		t.Fatalf("adopt stored %q", got)
	}
}
