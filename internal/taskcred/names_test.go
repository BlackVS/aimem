package taskcred

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/hubtest"
)

// expiringHub answers the identity check with a token's label and expiry
// until expired is set, then refuses every credential (HTTP 401), as the
// hub does for an expired token.
func expiringHub(t *testing.T, root, repo string, entry adapter.HubConfig) *atomic.Bool {
	t.Helper()
	var expired atomic.Bool
	ts := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expired.Load() {
			http.Error(w, "bearer authentication required", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"scope":"project","task_write":true,"label":"laptop-alpha","expires_at":"2026-11-30T00:00:00Z"}`))
	}))
	t.Cleanup(ts.Close)
	entry.URL, entry.Pin = ts.URL, hubtest.Pin(ts)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": &entry}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha","hub":"hub"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return &expired
}

func refusal(t *testing.T, s *Selection) string {
	t.Helper()
	var rejected *Rejected
	err := error(s.Rejected(http.StatusUnauthorized))
	if s.Source == "project-local" {
		err = s.Validate(context.Background())
	}
	if !errors.As(err, &rejected) || rejected.Status != http.StatusUnauthorized {
		t.Fatalf("not a refusal: %v", err)
	}
	if strings.Contains(err.Error(), "aimem_user_") {
		t.Fatalf("the refusal carries a secret: %v", err)
	}
	return err.Error()
}

func wantAll(t *testing.T, kind, msg string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(msg, w) {
			t.Fatalf("%s: %q lacks %q", kind, msg, w)
		}
	}
}

// A refused credential names itself: the label, scope and expiry recorded
// when it was stored, where it is bound, and the step that replaces it.
func TestRefusalNamesTheCredential(t *testing.T) {
	t.Run("project-local", func(t *testing.T) {
		root, repo := t.TempDir(), t.TempDir()
		expired := expiringHub(t, root, repo, adapter.HubConfig{TaskToken: token("g")})
		if err := Set(context.Background(), repo, root, token("a")); err != nil {
			t.Fatal(err)
		}
		expired.Store(true)
		s, err := Resolve(repo, root)
		if err != nil {
			t.Fatal(err)
		}
		wantAll(t, "project-local", refusal(t, s),
			`task credential "laptop-alpha" (scope project, expires 2026-11-30T00:00:00Z)`,
			"bound project-locally to this checkout ("+s.Repo+", project alpha, hub hub)",
			"aimem access token-issue --project alpha", "aimem task-token set --token-file FILE")
	})
	t.Run("hub", func(t *testing.T) {
		root, repo := t.TempDir(), t.TempDir()
		meta := &adapter.TokenMeta{Label: "laptop", Scope: "user", ExpiresAt: "2026-10-01T00:00:00Z"}
		expiringHub(t, root, repo, adapter.HubConfig{TaskToken: token("g"), TaskTokenMeta: meta})
		s, err := Resolve(repo, root)
		if err != nil {
			t.Fatal(err)
		}
		wantAll(t, "hub", refusal(t, s),
			`task credential "laptop" (scope user, expires 2026-10-01T00:00:00Z) stored for hub hub`,
			"aimem access token-issue-user", "aimem hub task-token hub --token-file FILE")
	})
}

// A binding stored before labels were recorded still resolves; its refusal
// names the binding and the replacement step and says the label is unknown.
func TestRefusalOfAnOlderBinding(t *testing.T) {
	t.Run("project-local", func(t *testing.T) {
		root, repo := t.TempDir(), t.TempDir()
		expired := expiringHub(t, root, repo, adapter.HubConfig{})
		if err := Set(context.Background(), repo, root, token("a")); err != nil {
			t.Fatal(err)
		}
		s, err := Resolve(repo, root)
		if err != nil {
			t.Fatal(err)
		}
		// Rewrite the credential as an older release wrote it: no meta.
		path, err := credentialPath(root, s.Repo, false)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"version": 1, "repo": s.Repo, "project": s.Project, "hub": s.HubName, "url": s.Hub.URL, "token": token("a")})
		if raw, err = protect(raw); err != nil {
			t.Fatal(err)
		}
		if err := atomicWrite(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if s, err = Resolve(repo, root); err != nil || s.Meta != nil {
			t.Fatalf("older binding: %+v %v", s, err)
		}
		if err := s.Validate(context.Background()); err != nil {
			t.Fatalf("older binding no longer works: %v", err)
		}
		expired.Store(true)
		wantAll(t, "project-local", refusal(t, s), "label unknown", "bound project-locally to this checkout", "aimem task-token set")
	})
	t.Run("hub", func(t *testing.T) {
		root, repo := t.TempDir(), t.TempDir()
		expiringHub(t, root, repo, adapter.HubConfig{TaskToken: token("g")})
		s, err := Resolve(repo, root)
		if err != nil {
			t.Fatal(err)
		}
		wantAll(t, "hub", refusal(t, s), "label unknown", "stored for hub hub", "aimem hub task-token hub --token-file FILE")
	})
}

// A rejected token given to task-token set is not described as a binding:
// nothing was stored.
func TestSetOfARefusedToken(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	expired := expiringHub(t, root, repo, adapter.HubConfig{})
	expired.Store(true)
	err := Set(context.Background(), repo, root, token("a"))
	if err == nil || !strings.Contains(err.Error(), "the hub refused this token (HTTP 401); nothing was stored") {
		t.Fatalf("set: %v", err)
	}
}
