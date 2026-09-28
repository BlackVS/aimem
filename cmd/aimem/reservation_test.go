package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"aimem/internal/store"
)

// heldTask is a READY task in project alpha held by a team member bound to
// service aicrew-example, as a coordination-backed claim leaves it.
func heldTask(t *testing.T, g *identityCLIRig) (store.Task, store.TaskReservationOutcome) {
	t.Helper()
	db, err := g.reg.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(store.TasksMetaKey, "on"); err != nil {
		t.Fatal(err)
	}
	task, err := db.CreateTask(store.TaskContent{Title: "stuck work", State: "READY"}, store.TaskActor{Kind: "admin", Name: "admin"}, "create-stuck")
	if err != nil {
		t.Fatal(err)
	}
	actor := store.TaskActor{Kind: "user", UserID: "01a0e62c-0000-7000-8000-0000000000a1", TokenID: "01a0e62c-0000-7000-8000-0000000000a2", Name: "worker"}
	b := store.ReservationBinding{UserID: actor.UserID, Mode: "team", ServiceID: "aicrew-example", ProfileID: "profile-1", TeamID: "team-1",
		Role: "worker", SessionID: "sess-1", Generation: "4"}
	held, err := db.ApplyTaskReservation(store.ReservationClaim, store.TaskReservationInput{TaskID: task.ID, ExpectedRevision: task.Revision,
		Holder: store.ReservationHolder{Mode: "external", Ref: "aicrew-attempt-1"}}, actor, b, "claim-stuck", func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return task, held
}

func (g *identityCLIRig) recover(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	full := append(append([]string{"recover"}, args...), "--hub", g.ts.URL, "--admin-token-file", g.tokenFile, "--hub-ca-file", g.caFile)
	var out bytes.Buffer
	err := runReservation(full, strings.NewReader(stdin), &out)
	g.outputs = append(g.outputs, out.String())
	if err != nil {
		g.outputs = append(g.outputs, err.Error())
	}
	return out.String(), err
}

func TestReservationRecoverCLI(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	task, held := heldTask(t, g)
	out, err := g.recover(t, "", "status", "--task", task.ID)
	if err != nil || !strings.Contains(out, `"state": "held"`) || !strings.Contains(out, held.Reservation.ID) {
		t.Fatalf("status: %v %s", err, out)
	}
	body, _ := json.Marshal(map[string]any{
		"reservation_id": held.Reservation.ID, "fence": strconv.FormatInt(held.Reservation.Fence, 10),
		"expected_revision": held.Task.Revision, "reason": "the worker's machine is gone",
		"content":  map[string]any{"title": task.Title, "state": "READY"},
		"evidence": map[string]any{"kind": "attestation", "attestation_id": "ticket-9", "statement": "worker machine decommissioned; attempt abandoned"},
	})
	out, err = g.recover(t, string(body), "release", "--task", task.ID, "--key", "cli-rec-1")
	if err != nil || !strings.Contains(out, `"task_state": "READY"`) || !strings.Contains(out, `"principal": "recovery/env"`) {
		t.Fatalf("release: %v %s", err, out)
	}
	if strings.Contains(out, "decommissioned") {
		t.Fatal("the answer quotes the attestation")
	}
	out, err = g.recover(t, "", "receipt", "recovery_release", "--task", task.ID, "--key-digest", store.RequestKeyDigest("cli-rec-1"))
	if err != nil || !strings.Contains(out, `"principal": "recovery/env"`) {
		t.Fatalf("receipt: %v %s", err, out)
	}
	// DONE is refused with the hub's code.
	task2, held2 := func() (store.Task, store.TaskReservationOutcome) {
		db, _ := g.reg.Open("alpha")
		tk, _ := db.CreateTask(store.TaskContent{Title: "second", State: "READY"}, store.TaskActor{Kind: "admin", Name: "admin"}, "create-2")
		b := store.ReservationBinding{UserID: "01a0e62c-0000-7000-8000-0000000000a1", Mode: "personal"}
		h, err := db.ApplyTaskReservation(store.ReservationClaim, store.TaskReservationInput{TaskID: tk.ID, ExpectedRevision: tk.Revision,
			Holder: store.ReservationHolder{Mode: "standalone", Ref: "w"}}, store.TaskActor{Kind: "user", UserID: b.UserID,
			TokenID: "01a0e62c-0000-7000-8000-0000000000a2", Name: "p"}, b, "c2", func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return tk, h
	}()
	done, _ := json.Marshal(map[string]any{
		"reservation_id": held2.Reservation.ID, "fence": strconv.FormatInt(held2.Reservation.Fence, 10),
		"expected_revision": held2.Task.Revision, "reason": "x",
		"content":  map[string]any{"title": task2.Title, "state": "DONE"},
		"evidence": map[string]any{"kind": "attestation", "attestation_id": "t", "statement": "worker machine decommissioned; attempt abandoned"},
	})
	if _, err := g.recover(t, string(done), "cancel", "--task", task2.ID, "--key", "cli-done"); err == nil || !strings.Contains(err.Error(), "invalid_request") {
		t.Fatalf("cancel to DONE: %v", err)
	}
	// Usage: the body only on stdin, a key for mutations, a digest for receipts.
	for name, c := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"no body":          {"", []string{"release", "--task", task2.ID, "--key", "k"}, "standard input"},
		"not an object":    {"[1]", []string{"release", "--task", task2.ID, "--key", "k"}, "standard input"},
		"no key":           {string(done), []string{"release", "--task", task2.ID}, "--key"},
		"raw key digest":   {"", []string{"receipt", "claim", "--task", task2.ID, "--key-digest", "cli-rec-1"}, "--key-digest"},
		"proof as an argv": {"", []string{"release", "--task", task2.ID, "--key", "k", "--proof", "acp1_x"}, "flag provided but not defined"},
	} {
		if _, err := g.recover(t, c.stdin, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Plain HTTP is refused before any request.
	var out2 bytes.Buffer
	if err := runReservation([]string{"recover", "status", "--task", task.ID, "--hub", "http://127.0.0.1:1", "--admin-token-file", g.tokenFile},
		strings.NewReader(""), &out2); err == nil {
		t.Fatal("plain HTTP accepted")
	}
	g.assertNoSecrets(t)
}
