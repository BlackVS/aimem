package introspect

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"aimem/internal/introspect/introspecttest"
)

func proof(t *testing.T) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "acp1_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// stoppedFact is a valid stopped fact for tests to edit.
func stoppedFact() map[string]any {
	return map[string]any{
		"kind": "stopped", "operation": "release", "task_id": "01a0e62c-0000-7000-8000-00000000c601",
		"request_key_digest": "k1_" + strings.Repeat("A", 43),
		"member": map[string]any{"user_id": "user-1", "agent_id": "agent-1", "team_id": "team-1", "role": "worker",
			"session_id": "sess-1", "generation": "4"},
		"attempt_ref": "aicrew-attempt-7",
		"expires_at":  time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}
}

func testPin() map[string]any {
	return map[string]any{"repo": "https://git.example/team/process.git", "commit": strings.Repeat("3f", 20), "manifest": "process/manifest.json"}
}

// offerFact is a valid offer: it names its worker and carries the pin.
func offerFact(fact map[string]any) {
	fact["kind"], fact["operation"] = "offer", "claim"
	delete(fact, "attempt_ref")
	fact["offer_ref"] = "aicrew-offer-7"
	fact["intended_worker"] = map[string]any{"user_id": "user-2", "agent_id": "agent-2"}
	fact["process"] = testPin()
}

func answerFact(f *fake, edit func(reply, fact map[string]any)) {
	f.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		fact := stoppedFact()
		reply := introspecttest.FactReply(got.Nonce, testService, testHub, fact)
		if edit != nil {
			edit(reply, fact)
		}
		introspecttest.WriteJSON(w, reply)
	})
}

func TestCoordinateActiveFact(t *testing.T) {
	f := newFake(t)
	answerFact(f, nil)
	for _, mode := range []string{"ca_dns", "spki_sha256"} {
		p := proof(t)
		got, err := newClient(t, f).Coordinate(context.Background(), peerOf(f, mode), p)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if got.Kind != "stopped" || got.Operation != "release" || got.AttemptRef != "aicrew-attempt-7" || got.Member.SessionID != "sess-1" ||
			got.ServiceID != testService || got.OfferRef != "" || got.IntendedWorker != nil {
			t.Fatalf("%s fact: %+v", mode, got)
		}
		last := f.Last()
		if last.Path != introspecttest.CoordinationPath || last.Header.Get(CoordinationVersionHeader) != "1" ||
			last.Header.Get("Authorization") != "Bearer "+testBearer || last.Body.Proof != p || last.Body.Handle != "" ||
			last.Body.Version != 1 || last.Body.HubID != testHub {
			t.Fatalf("%s request: %+v", mode, last)
		}
	}
	// The offer fact carries its intended worker and its process pin.
	answerFact(f, func(_, fact map[string]any) { offerFact(fact) })
	got, err := newClient(t, f).Coordinate(context.Background(), peerOf(f, "ca_dns"), proof(t))
	want := Process{Repo: "https://git.example/team/process.git", Commit: strings.Repeat("3f", 20), Manifest: "process/manifest.json"}
	if err != nil || got.IntendedWorker == nil || *got.IntendedWorker != (Worker{"user-2", "agent-2"}) || got.OfferRef != "aicrew-offer-7" ||
		got.Process == nil || *got.Process != want {
		t.Fatalf("offer fact: %+v %v", got, err)
	}
	// So does an independent claim; a stop carries none.
	answerFact(f, func(_, fact map[string]any) {
		fact["kind"], fact["operation"] = "independent_claim", "claim"
		fact["member"].(map[string]any)["role"] = "independent"
		fact["process"] = testPin()
	})
	if got, err := newClient(t, f).Coordinate(context.Background(), peerOf(f, "ca_dns"), proof(t)); err != nil || got.Process == nil || *got.Process != want {
		t.Fatalf("independent claim: %+v %v", got, err)
	}
}

// A missing, malformed or misplaced pin, or one with another field, is a
// wrong-shaped reply (C5-w2).
func TestCoordinateProcessPinShape(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	setPin := func(k string, v any) func(reply, fact map[string]any) {
		return func(_, fact map[string]any) {
			offerFact(fact)
			pin := fact["process"].(map[string]any)
			if v == nil {
				delete(pin, k)
			} else {
				pin[k] = v
			}
		}
	}
	for name, edit := range map[string]func(reply, fact map[string]any){
		"offer without a pin": func(_, fact map[string]any) { offerFact(fact); delete(fact, "process") },
		"stop with a pin":     func(_, fact map[string]any) { fact["process"] = testPin() },
		"no commit":           setPin("commit", nil),
		"short commit":        setPin("commit", "3f2a9c1"),
		"uppercase commit":    setPin("commit", strings.Repeat("3F", 20)),
		"plain http repo":     setPin("repo", "http://git.example/team/process.git"),
		"absolute manifest":   setPin("manifest", "/process/manifest.json"),
		"escaping manifest":   setPin("manifest", "../manifest.json"),
		"a branch":            setPin("ref", "main"),
		"a digest":            setPin("digest", "sha256:"+strings.Repeat("0", 64)),
		"a number for commit": setPin("commit", 7),
		"the pin is a string": func(_, fact map[string]any) {
			offerFact(fact)
			fact["process"] = "https://git.example/team/process.git"
		},
	} {
		answerFact(f, edit)
		_, err := c.Coordinate(context.Background(), peerOf(f, "ca_dns"), proof(t))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantFailure(t, err, CodeUnavailable, "malformed")
	}
}

func TestCoordinateInactiveAndExpiredAreRejected(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	// Unscripted: the fake answers inactive.
	_, err := c.Coordinate(context.Background(), peerOf(f, "ca_dns"), proof(t))
	wantFailure(t, err, CodeRejected, "inactive")
	answerFact(f, func(_, fact map[string]any) {
		fact["expires_at"] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	})
	_, err = c.Coordinate(context.Background(), peerOf(f, "ca_dns"), proof(t))
	wantFailure(t, err, CodeRejected, "expired")
	_, err = c.Coordinate(context.Background(), peerOf(f, "ca_dns"), "acp1_short")
	wantFailure(t, err, CodeRejected, "proof_shape")
	// An inactive reply that says anything more is malformed, not a reason.
	f.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, map[string]any{"nonce": got.Nonce, "active": false, "service_id": testService})
	})
	_, err = c.Coordinate(context.Background(), peerOf(f, "ca_dns"), proof(t))
	wantFailure(t, err, CodeUnavailable, "malformed")
}

func TestCoordinateReplyChecks(t *testing.T) {
	f := newFake(t)
	c := newClient(t, f)
	for name, c2 := range map[string]struct {
		edit   func(reply, fact map[string]any)
		reason string
	}{
		"nonce":             {func(r, _ map[string]any) { r["nonce"] = "n-00000000000000000000000000000000" }, "nonce_mismatch"},
		"service":           {func(r, _ map[string]any) { r["service_id"] = "other" }, "wrong_service"},
		"hub":               {func(r, _ map[string]any) { r["hub_id"] = "other" }, "wrong_hub"},
		"unknown field":     {func(r, _ map[string]any) { r["reason"] = "x" }, "malformed"},
		"unknown kind":      {func(_, f map[string]any) { f["kind"] = "vibes" }, "malformed"},
		"kind vs operation": {func(_, f map[string]any) { f["operation"] = "finalize" }, "malformed"},
		"key digest":        {func(_, f map[string]any) { f["request_key_digest"] = "raw-key" }, "malformed"},
		"role":              {func(_, f map[string]any) { f["member"].(map[string]any)["role"] = "boss" }, "malformed"},
		"generation":        {func(_, f map[string]any) { f["member"].(map[string]any)["generation"] = "0" }, "malformed"},
		"missing ref":       {func(_, f map[string]any) { delete(f, "attempt_ref") }, "malformed"},
		"extra ref":         {func(_, f map[string]any) { f["offer_ref"] = "o" }, "malformed"},
		"extra worker":      {func(_, f map[string]any) { f["intended_worker"] = map[string]any{"user_id": "u", "agent_id": "a"} }, "malformed"},
		"no member":         {func(_, f map[string]any) { delete(f, "member") }, "malformed"},
	} {
		answerFact(f, c2.edit)
		_, err := c.Coordinate(context.Background(), peerOf(f, "ca_dns"), proof(t))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		wantFailure(t, err, CodeUnavailable, c2.reason)
	}
	// The version is refused before evaluation: context_unavailable.
	f.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		w.WriteHeader(http.StatusBadRequest)
		introspecttest.WriteJSON(w, map[string]any{"code": "unsupported_version"})
	})
	_, err := c.Coordinate(context.Background(), peerOf(f, "ca_dns"), proof(t))
	wantFailure(t, err, CodeUnavailable, "version_rejected")
}
