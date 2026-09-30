package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/adapter"
)

// `aimem hub credential` reports whether the hub's individual credential
// is set and the hub's answer for it (scope, state, IDs), against a real
// hub over verified TLS, and never prints any part of a secret.
func TestHubCredentialCommand(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	db, err := access.Open(g.reg.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	u, err := db.CreateUser("admin", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	tok, secret, err := db.IssueScoped("admin", u.ID, "agent", access.ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", state)
	const checkpoint = "checkpoint-secret-value"
	save := func(h *adapter.HubConfig) {
		t.Helper()
		if err := adapter.SaveHubs(state, map[string]*adapter.HubConfig{"hub": h}, "hub"); err != nil {
			t.Fatal(err)
		}
	}
	report := func(args ...string) (hubCredentialStatus, string) {
		t.Helper()
		var js, txt bytes.Buffer
		if err := hubCredentialCmd(append(append([]string{}, args...), "--json"), &js); err != nil {
			t.Fatalf("credential --json: %v", err)
		}
		if err := hubCredentialCmd(args, &txt); err != nil {
			t.Fatalf("credential: %v", err)
		}
		var st hubCredentialStatus
		if err := json.Unmarshal(js.Bytes(), &st); err != nil {
			t.Fatalf("json: %s", js.String())
		}
		// No window of either secret appears: every 8 characters of the
		// credential past its public prefix, and of the checkpoint token.
		var windows []string
		for _, sec := range []string{strings.TrimPrefix(secret, "aimem_user_"), checkpoint} {
			for i := 0; i+8 <= len(sec); i++ {
				windows = append(windows, sec[i:i+8])
			}
		}
		for _, out := range []string{js.String(), txt.String()} {
			for _, w := range windows {
				if strings.Contains(out, w) {
					t.Fatalf("the output carries part of a secret (%q): %s", w, out)
				}
			}
		}
		return st, txt.String()
	}

	// No credential: none, and the hub is not asked.
	save(&adapter.HubConfig{URL: g.ts.URL, Token: checkpoint, CAFile: g.caFile})
	before := g.requests.Load()
	if st, txt := report("hub"); st.Credential != "none" || st.State != "absent" || !strings.Contains(txt, "aimem hub task-token hub") {
		t.Fatalf("none: %+v %q", st, txt)
	}
	if g.requests.Load() != before {
		t.Fatal("the hub was asked about an absent credential")
	}

	// The individual credential, active: its scope and IDs as the hub
	// reports them. The default hub is used without a name.
	save(&adapter.HubConfig{URL: g.ts.URL, Token: checkpoint, TaskToken: secret, CAFile: g.caFile})
	for _, args := range [][]string{{"hub"}, nil} {
		st, txt := report(args...)
		if st.Credential != "set" || st.State != "active" || st.Scope != "user" || st.UserID != u.ID || st.TokenID != tok.ID ||
			!strings.Contains(txt, "active, scope user") {
			t.Fatalf("active %v: %+v %q", args, st, txt)
		}
	}

	// Revoked: the hub refuses it.
	if err := db.Revoke("admin", tok.ID); err != nil {
		t.Fatal(err)
	}
	if st, txt := report("hub"); st.Credential != "set" || st.State != "refused" || st.Scope != "" || !strings.Contains(txt, "refused") {
		t.Fatalf("revoked: %+v %q", st, txt)
	}

	// A stored task credential that is not an individual one.
	save(&adapter.HubConfig{URL: g.ts.URL, Token: checkpoint, TaskToken: "legacy-token", CAFile: g.caFile})
	if st, _ := report("hub"); st.Credential != "other" || st.State != "absent" {
		t.Fatalf("other: %+v", st)
	}

	// No answer: unreachable, the credential still reported as set.
	save(&adapter.HubConfig{URL: "https://127.0.0.1:1", Token: checkpoint, TaskToken: secret, CAFile: g.caFile})
	if st, txt := report("hub"); st.Credential != "set" || st.State != "unreachable" || st.Detail == "" || !strings.Contains(txt, "unreachable") {
		t.Fatalf("unreachable: %+v %q", st, txt)
	}

	// Usage and unknown hubs.
	var out bytes.Buffer
	if err := hubCredentialCmd([]string{"nope"}, &out); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unknown hub: %v", err)
	}
	if err := hubCredentialCmd([]string{"a", "b"}, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("two names: %v", err)
	}
}
