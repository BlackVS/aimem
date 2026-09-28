package server

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

// reservationRig is the E4 team rig (a real TLS hub, the fake aicrew and a
// team profile granted on alpha) plus a second user, Bob, with a personal
// grant on alpha.
type reservationRig struct {
	*teamRig
	alpha          *store.DB
	bobID, bobTok  string
	aliceIdentity  Identity
	bobIdentity    Identity
	createdCounter int
}

func newReservationRig(t *testing.T) *reservationRig {
	t.Helper()
	g := &reservationRig{teamRig: newTeamRig(t)}
	// Contexts the rig verifies are verified at or after this instant, so
	// they never age; a test that needs an old one sets its VerifiedAt.
	pinned := time.Now()
	reservationClock = func() time.Time { return pinned }
	t.Cleanup(func() { reservationClock = time.Now })
	var err error
	if g.alpha, err = g.s.reg.Open("alpha"); err != nil {
		t.Fatal(err)
	}
	bob, err := g.db.CreateUser("admin", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := g.db.IssueScoped("admin", bob.ID, "agent", access.ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.db.SetGrant("admin", g.alphaInstance, "user", bob.ID, true); err != nil {
		t.Fatal(err)
	}
	g.bobID, g.bobTok = bob.ID, tok.ID
	g.aliceIdentity = Identity{Name: "agent", Role: "user", UserID: g.aliceID, TokenID: g.aliceTok, Scope: access.ScopeUser}
	g.bobIdentity = Identity{Name: "agent", Role: "user", UserID: bob.ID, TokenID: tok.ID, Scope: access.ScopeUser}
	return g
}

func (g *reservationRig) personal(id Identity) context.Context {
	return withIdentity(context.Background(), id)
}

// team verifies a team context for id through the real E4 verifier and the
// fake aicrew, whose answer edit adjusts, and returns a context carrying it.
func (g *reservationRig) team(t *testing.T, id Identity, edit func(m map[string]any)) context.Context {
	t.Helper()
	g.answerActive(func(m map[string]any) {
		m["identity"] = map[string]any{"user_id": id.UserID, "token_id": id.TokenID}
		if edit != nil {
			edit(m)
		}
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/access/identity", nil)
	tc, ok := g.s.verifyTeamContext(rec, req, id, validHandle(t), uuidv7.New())
	if !ok {
		t.Fatalf("team context not verified: %d %s", rec.Code, rec.Body.String())
	}
	return context.WithValue(withIdentity(context.Background(), id), teamContextKey{}, tc)
}

func (g *reservationRig) readyTask(t *testing.T, db *store.DB) store.Task {
	t.Helper()
	g.createdCounter++
	task, err := db.CreateTask(store.TaskContent{Title: "reserved work", State: "READY"},
		store.TaskActor{Kind: "admin", Name: "admin"}, "ready-"+uuidv7.New())
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func refusalCode(err error) string {
	var r *reservationRefusal
	if errors.As(err, &r) {
		return r.Code
	}
	if err == nil {
		return ""
	}
	return "error: " + err.Error()
}

func expectRefusal(t *testing.T, what string, err error, code string) {
	t.Helper()
	if got := refusalCode(err); got != code {
		t.Fatalf("%s: got %q, want %q", what, got, code)
	}
}

func claimOf(task store.Task) store.TaskReservationInput {
	return store.TaskReservationInput{TaskID: task.ID, ExpectedRevision: task.Revision,
		Holder: store.ReservationHolder{Mode: "standalone", Ref: "own-work"}}
}

func nextOf(task store.Task, out store.TaskReservationOutcome, state string) store.TaskReservationInput {
	in := store.TaskReservationInput{TaskID: task.ID, ID: out.Reservation.ID, Fence: out.Reservation.Fence,
		ExpectedRevision: out.Task.Revision, Content: &store.TaskContent{Title: task.Title, State: state}}
	if state == "READY" || state == "BLOCKED" || state == "DONE" || state == "CANCELLED" {
		in.Reason = "work reached " + state
	}
	return in
}

// held asserts the task's stored hold is exactly want.
func (g *reservationRig) held(t *testing.T, taskID string, want store.TaskReservation) {
	t.Helper()
	got, err := g.alpha.GetTaskReservation(taskID)
	if err != nil || got.ID != want.ID || got.Fence != want.Fence || got.Binding != want.Binding {
		t.Fatalf("hold changed: %+v, want %+v (%v)", got, want, err)
	}
}

// seedTeamHold binds a hold directly in the store to the team context in
// ctx, as C5b's coordination-backed claim will. C5a authorizes no team claim.
func (g *reservationRig) seedTeamHold(t *testing.T, ctx context.Context, task store.Task) store.TaskReservationOutcome {
	t.Helper()
	c, err := reservationCallerFrom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := claimOf(task)
	in.Holder = store.ReservationHolder{Mode: "external", Ref: "attempt-1"}
	out, err := g.alpha.ApplyTaskReservation(store.ReservationClaim, in, c.actor, c.binding, "seed-"+task.ID, nil, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReservationPersonalStandaloneLifecycle(t *testing.T) {
	g := newReservationRig(t)
	alice := g.personal(g.aliceIdentity)
	task := g.readyTask(t, g.alpha)
	claimed, err := g.s.reserve(alice, store.ReservationClaim, claimOf(task), "claim", "")
	if err != nil {
		t.Fatal(err)
	}
	want := store.ReservationBinding{UserID: g.aliceID, Mode: "personal"}
	if claimed.Reservation.Binding != want || claimed.Binding != want {
		t.Fatalf("claim binding: %+v", claimed)
	}
	if st, err := g.s.reservationStatus(alice, task.ID); err != nil || st.State != "held" || st.Reservation.ID != claimed.Reservation.ID {
		t.Fatalf("own status: %+v, %v", st, err)
	}
	updated, err := g.s.reserve(alice, store.ReservationUpdate, nextOf(task, claimed, "IN_PROGRESS"), "update", "")
	if err != nil {
		t.Fatal(err)
	}
	released, err := g.s.reserve(alice, store.ReservationRelease, nextOf(task, updated, "READY"), "release", "")
	if err != nil || released.Reservation.ID != "" {
		t.Fatalf("release: %+v, %v", released, err)
	}
	again, err := g.s.reserve(alice, store.ReservationClaim, claimOf(released.Task), "claim-again", "")
	if err != nil {
		t.Fatal(err)
	}
	final, err := g.s.reserve(alice, store.ReservationFinalize, nextOf(released.Task, again, "CANCELLED"), "finalize", "")
	if err != nil || final.Task.State != "CANCELLED" || final.Reservation.ID != "" {
		t.Fatalf("finalize: %+v, %v", final, err)
	}
	// The receipt of each transition is the caller's, with its binding.
	if out, found, err := g.s.reservationReceipt(alice, store.ReservationClaim, task.ID, "claim"); err != nil || !found || out.Binding != want {
		t.Fatalf("own receipt: %+v %v %v", out, found, err)
	}
	// A personal claim holds in standalone mode only.
	other := g.readyTask(t, g.alpha)
	ext := claimOf(other)
	ext.Holder.Mode = "external"
	_, err = g.s.reserve(alice, store.ReservationClaim, ext, "claim-external", "")
	expectRefusal(t, "personal external claim", err, "invalid_request")
}

// The actor and holder come only from the authenticated connection.
func TestReservationActorIsDerivedNeverSupplied(t *testing.T) {
	g := newReservationRig(t)
	task := g.readyTask(t, g.alpha)
	_, err := g.s.reserve(context.Background(), store.ReservationClaim, claimOf(task), "anon", "")
	expectRefusal(t, "no identity", err, "invalid_credential")
	admin := withIdentity(context.Background(), Identity{Name: "admin", Role: "admin"})
	_, err = g.s.reserve(admin, store.ReservationClaim, claimOf(task), "admin", "")
	expectRefusal(t, "admin credential", err, "grant_denied")
	// The input a caller controls has no field that names an actor or context.
	forbidden := []string{"actor", "user", "profile", "role", "session", "generation", "token", "team", "binding", "mode"}
	for _, f := range reflect.VisibleFields(reflect.TypeOf(store.TaskReservationInput{})) {
		for _, word := range forbidden {
			if strings.Contains(strings.ToLower(f.Name), word) {
				t.Fatalf("reservation input field %s could name the %s", f.Name, word)
			}
		}
	}
	// A team context verified for another credential is refused.
	ctx := g.team(t, g.aliceIdentity, nil)
	forged := withIdentity(ctx, g.bobIdentity)
	_, err = g.s.reserve(forged, store.ReservationUpdate, nextOf(task, store.TaskReservationOutcome{Task: task,
		Reservation: store.TaskReservation{ID: uuidv7.New(), Fence: 1}}, "IN_PROGRESS"), "forged", "")
	expectRefusal(t, "team context of another credential", err, "identity_mismatch")

	// Alice's hold: Bob cannot act on it, and his status reads none.
	alice, bob := g.personal(g.aliceIdentity), g.personal(g.bobIdentity)
	claimed, err := g.s.reserve(alice, store.ReservationClaim, claimOf(task), "alice-claim", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.s.reserve(bob, store.ReservationUpdate, nextOf(task, claimed, "IN_PROGRESS"), "bob-update", "")
	expectRefusal(t, "another user's update", err, "reservation_conflict")
	_, err = g.s.reserve(bob, store.ReservationRelease, nextOf(task, claimed, "READY"), "bob-release", "")
	expectRefusal(t, "another user's release", err, "reservation_conflict")
	if st, err := g.s.reservationStatus(bob, task.ID); err != nil || st.State != "none" || st.Reservation != nil {
		t.Fatalf("another user's status disclosed the hold: %+v, %v", st, err)
	}
	if _, found, err := g.s.reservationReceipt(bob, store.ReservationClaim, task.ID, "alice-claim"); err != nil || found {
		t.Fatalf("another user read the receipt: %v %v", found, err)
	}
	g.held(t, task.ID, claimed.Reservation)
}

// A personal context never acts on a team hold, and a team context never
// on a personal one.
func TestReservationModesNeverCross(t *testing.T) {
	g := newReservationRig(t)
	alice := g.personal(g.aliceIdentity)
	personalTask := g.readyTask(t, g.alpha)
	personalHold, err := g.s.reserve(alice, store.ReservationClaim, claimOf(personalTask), "p-claim", "")
	if err != nil {
		t.Fatal(err)
	}
	team := g.team(t, g.aliceIdentity, nil)
	_, err = g.s.reserve(team, store.ReservationUpdate, nextOf(personalTask, personalHold, "IN_PROGRESS"), "t-on-p", "")
	expectRefusal(t, "team update of a personal hold", err, "reservation_conflict")
	if st, err := g.s.reservationStatus(team, personalTask.ID); err != nil || st.State != "none" {
		t.Fatalf("team status of a personal hold: %+v, %v", st, err)
	}
	teamTask := g.readyTask(t, g.alpha)
	teamHold := g.seedTeamHold(t, team, teamTask)
	_, err = g.s.reserve(alice, store.ReservationUpdate, nextOf(teamTask, teamHold, "IN_PROGRESS"), "p-on-t", "")
	expectRefusal(t, "personal update of a team hold", err, "reservation_conflict")
	_, err = g.s.reserve(alice, store.ReservationRelease, nextOf(teamTask, teamHold, "READY"), "p-release-t", "")
	expectRefusal(t, "personal release of a team hold", err, "reservation_conflict")
	if st, err := g.s.reservationStatus(alice, teamTask.ID); err != nil || st.State != "none" {
		t.Fatalf("personal status of a team hold: %+v, %v", st, err)
	}
	g.held(t, personalTask.ID, personalHold.Reservation)
	g.held(t, teamTask.ID, teamHold.Reservation)
}

func TestReservationTeamHolderUpdate(t *testing.T) {
	g := newReservationRig(t)
	worker := g.team(t, g.aliceIdentity, nil) // the fake's default role is worker
	task := g.readyTask(t, g.alpha)
	hold := g.seedTeamHold(t, worker, task)
	out, err := g.s.reserve(worker, store.ReservationUpdate, nextOf(task, hold, "IN_PROGRESS"), "update-1", "")
	if err != nil {
		t.Fatal(err)
	}
	// A resumed session (new session and generation) of the same member keeps
	// its hold.
	resumed := g.team(t, g.aliceIdentity, func(m map[string]any) { m["session_id"], m["generation"] = "sess-2", "5" })
	if out, err = g.s.reserve(resumed, store.ReservationUpdate, nextOf(task, out, "BLOCKED"), "update-2", ""); err != nil {
		t.Fatalf("resumed session: %v", err)
	}
	// Team claim, release, finalize and transfer need aicrew coordination
	// (C5b): a worker never claims, and every other step needs its proof.
	for op, c := range map[store.ReservationOperation]struct {
		in   store.TaskReservationInput
		want string
	}{
		store.ReservationClaim:    {claimOf(g.readyTask(t, g.alpha)), "role_forbidden"},
		store.ReservationRelease:  {nextOf(task, out, "READY"), "invalid_request"},
		store.ReservationFinalize: {nextOf(task, out, "DONE"), "invalid_request"},
		store.ReservationTransfer: {store.TaskReservationInput{TaskID: task.ID, ID: out.Reservation.ID, Fence: out.Reservation.Fence, ExpectedRevision: out.Task.Revision,
			Holder: store.ReservationHolder{Mode: "external", Ref: "attempt-2"}}, "invalid_request"},
	} {
		_, err := g.s.reserve(worker, op, c.in, "team-"+string(op), "")
		expectRefusal(t, "team "+string(op)+" without a proof", err, c.want)
	}
	// An update carries no proof.
	_, err = g.s.reserve(worker, store.ReservationUpdate, nextOf(task, out, "IN_PROGRESS"), "update-proof", testProof(t))
	expectRefusal(t, "update with a proof", err, "invalid_request")
	// A coordinator does not update work.
	coordinator := g.team(t, g.aliceIdentity, func(m map[string]any) { m["role"] = "coordinator" })
	_, err = g.s.reserve(coordinator, store.ReservationUpdate, nextOf(task, out, "IN_PROGRESS"), "coord", "")
	expectRefusal(t, "coordinator update", err, "role_forbidden")
	// A role other than the bound one is another holder.
	independent := g.team(t, g.aliceIdentity, func(m map[string]any) { m["role"] = "independent" })
	_, err = g.s.reserve(independent, store.ReservationUpdate, nextOf(task, out, "IN_PROGRESS"), "indep", "")
	expectRefusal(t, "changed role", err, "reservation_conflict")
	// Wrong project: beta is not granted to the profile.
	beta, err := g.s.reg.Open("beta")
	if err != nil {
		t.Fatal(err)
	}
	betaTask := g.readyTask(t, beta)
	_, err = g.s.reserve(worker, store.ReservationUpdate, store.TaskReservationInput{TaskID: betaTask.ID, ID: uuidv7.New(), Fence: 1,
		ExpectedRevision: betaTask.Revision, Content: &store.TaskContent{Title: "x", State: "IN_PROGRESS"}}, "beta", "")
	expectRefusal(t, "ungranted project", err, "grant_denied")
	// An online answer older than the bound is re-verified first.
	stale := g.team(t, g.aliceIdentity, nil)
	tc, _ := teamContextFrom(stale)
	tc.VerifiedAt = reservationClock().Add(-2 * reservationVerificationAge)
	_, err = g.s.reserve(context.WithValue(stale, teamContextKey{}, tc), store.ReservationUpdate, nextOf(task, out, "IN_PROGRESS"), "stale", "")
	expectRefusal(t, "old verification", err, "context_unavailable")
	// Losing the profile or its grant refuses new effects and keeps the hold.
	fresh := g.team(t, g.aliceIdentity, nil)
	if err := g.db.SetTeamGrant("admin", g.alphaInstance, g.profile.ID, false); err != nil {
		t.Fatal(err)
	}
	_, err = g.s.reserve(fresh, store.ReservationUpdate, nextOf(task, out, "IN_PROGRESS"), "no-grant", "")
	expectRefusal(t, "revoked team grant", err, "grant_denied")
	if err := g.db.SetTeamProfileDisabled("admin", g.profile.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err = g.s.reserve(fresh, store.ReservationUpdate, nextOf(task, out, "IN_PROGRESS"), "disabled", "")
	expectRefusal(t, "disabled profile", err, "context_stale")
	g.held(t, task.ID, out.Reservation)
}

// Two sessions of one member contend on one fence: one update wins.
func TestReservationSimultaneousSessionsOfOneMember(t *testing.T) {
	g := newReservationRig(t)
	first := g.team(t, g.aliceIdentity, nil)
	second := g.team(t, g.aliceIdentity, func(m map[string]any) { m["session_id"] = "sess-2" })
	task := g.readyTask(t, g.alpha)
	hold := g.seedTeamHold(t, first, task)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, ctx := range []context.Context{first, second} {
		wg.Add(1)
		go func(i int, ctx context.Context) {
			defer wg.Done()
			_, errs[i] = g.s.reserve(ctx, store.ReservationUpdate, nextOf(task, hold, "IN_PROGRESS"), "race-"+string(rune('a'+i)), "")
		}(i, ctx)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch refusalCode(err) {
		case "":
			won++
		case "stale_fence", "revision_conflict":
		default:
			t.Fatalf("race loser: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d updates won: %v", won, errs)
	}
}

// Revocation refuses new effects and never releases the hold; a rotated
// credential of the same user carries on.
func TestReservationRevocationAndRotation(t *testing.T) {
	g := newReservationRig(t)
	alice := g.personal(g.aliceIdentity)
	task := g.readyTask(t, g.alpha)
	hold, err := g.s.reserve(alice, store.ReservationClaim, claimOf(task), "claim", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.db.SetGrant("admin", g.alphaInstance, "user", g.aliceID, false); err != nil {
		t.Fatal(err)
	}
	_, err = g.s.reserve(alice, store.ReservationUpdate, nextOf(task, hold, "IN_PROGRESS"), "no-grant", "")
	expectRefusal(t, "revoked grant", err, "grant_denied")
	_, _, err = g.s.reservationReceipt(alice, store.ReservationClaim, task.ID, "claim")
	expectRefusal(t, "receipt after the grant was revoked", err, "grant_denied")
	_, err = g.s.reservationStatus(alice, task.ID)
	expectRefusal(t, "status after the grant was revoked", err, "grant_denied")
	_, err = g.s.reserve(alice, store.ReservationClaim, claimOf(task), "claim", "")
	expectRefusal(t, "claim replay after the grant was revoked", err, "grant_denied")
	g.held(t, task.ID, hold.Reservation)
	if err := g.db.SetGrant("admin", g.alphaInstance, "user", g.aliceID, true); err != nil {
		t.Fatal(err)
	}
	if err := g.db.Revoke("admin", g.aliceTok); err != nil {
		t.Fatal(err)
	}
	_, err = g.s.reserve(alice, store.ReservationUpdate, nextOf(task, hold, "IN_PROGRESS"), "revoked-token", "")
	expectRefusal(t, "revoked credential", err, "invalid_credential")
	g.held(t, task.ID, hold.Reservation)
	// Rotation: a new credential of the same user continues the hold.
	tok, _, err := g.db.IssueScoped("admin", g.aliceID, "agent", access.ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rotated := g.aliceIdentity
	rotated.TokenID = tok.ID
	out, err := g.s.reserve(g.personal(rotated), store.ReservationUpdate, nextOf(task, hold, "IN_PROGRESS"), "rotated", "")
	if err != nil || out.Reservation.ID != hold.Reservation.ID {
		t.Fatalf("rotated credential: %+v, %v", out, err)
	}
	if out, found, err := g.s.reservationReceipt(g.personal(rotated), store.ReservationClaim, task.ID, "claim"); err != nil || !found {
		t.Fatalf("rotated credential's receipt: %+v %v %v", out, found, err)
	}
}

// A revocation landing between authorization and commit refuses the commit.
func TestReservationPrecommitRecheck(t *testing.T) {
	g := newReservationRig(t)
	alice := g.personal(g.aliceIdentity)
	task := g.readyTask(t, g.alpha)
	hold, err := g.s.reserve(alice, store.ReservationClaim, claimOf(task), "claim", "")
	if err != nil {
		t.Fatal(err)
	}
	revoke := func() {
		if err := g.db.SetGrant("admin", g.alphaInstance, "user", g.aliceID, false); err != nil {
			t.Error(err)
		}
	}
	beforeReservationRecheck = revoke
	t.Cleanup(func() { beforeReservationRecheck = nil })
	_, err = g.s.reserve(alice, store.ReservationUpdate, nextOf(task, hold, "IN_PROGRESS"), "raced", "")
	expectRefusal(t, "revoked before commit", err, "grant_denied")
	beforeReservationRecheck = nil
	g.held(t, task.ID, hold.Reservation)
	if got, err := g.alpha.GetTask(task.ID); err != nil || got.Revision != task.Revision {
		t.Fatalf("a refused commit changed the task: %+v, %v", got, err)
	}
	// A claim rechecks inside the ledger too, and the refusal keeps its code.
	if err := g.db.SetGrant("admin", g.alphaInstance, "user", g.aliceID, true); err != nil {
		t.Fatal(err)
	}
	other := g.readyTask(t, g.alpha)
	beforeReservationRecheck = revoke
	_, err = g.s.reserve(alice, store.ReservationClaim, claimOf(other), "raced-claim", "")
	expectRefusal(t, "claim revoked inside the ledger", err, "grant_denied")
	beforeReservationRecheck = nil
	if got, err := g.alpha.GetTaskReservation(other.ID); err != nil || got.ID != "" || got.Fence != 0 {
		t.Fatalf("a refused claim left a hold: %+v, %v", got, err)
	}
}

// The member authorizer and the admin recovery path (C5c) are the ledger's
// only production callers.
func TestReservationLedgerHasOneCaller(t *testing.T) {
	ledger := map[string]bool{"ApplyTaskReservation": true, "ClaimTaskReservation": true,
		"GetTaskReservation": true, "GetTaskReservationReceipt": true,
		"RecoverTaskReservation": true, "ServiceHoldStatus": true, "RecoveryReceipts": true, "RecoveryReplay": true,
		"TaskReservationReceiptByKey": true}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{filepath.Join("internal", "server", "reservations.go"): true,
		filepath.Join("internal", "server", "recovery.go"): true}
	var callers []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".work" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if filepath.Dir(rel) == filepath.Join("internal", "store") || allowed[rel] {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && ledger[sel.Sel.Name] {
				callers = append(callers, rel+": "+sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 0 {
		t.Fatalf("the reservation ledger is reached outside the authorizer: %v", callers)
	}
	// And the authorizer does reach it, so the walk is not vacuous.
	src, err := os.ReadFile("reservations.go")
	if err != nil || !strings.Contains(string(src), ".ApplyTaskReservation(") || !strings.Contains(string(src), ".ClaimTaskReservation(") {
		t.Fatal("the authorizer does not call the ledger")
	}
}

// A claim proves its dependencies through the caller's own read authority;
// a dependency the caller cannot read, or that is not done, is unresolved.
func TestReservationClaimDependencies(t *testing.T) {
	g := newReservationRig(t)
	alice := g.personal(g.aliceIdentity)
	beta, err := g.s.reg.Open("beta")
	if err != nil {
		t.Fatal(err)
	}
	admin := store.TaskActor{Kind: "admin", Name: "admin"}
	dep, err := beta.CreateTask(store.TaskContent{Title: "dependency", State: "READY"}, admin, "dep")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := g.alpha.CreateTask(store.TaskContent{Title: "owner", State: "READY", Dependencies: []string{dep.ID}}, admin, "owner")
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.s.reserve(alice, store.ReservationClaim, claimOf(owner), "not-done", "")
	expectRefusal(t, "dependency not done", err, "dependency_unresolved")
	dep.TaskContent.State = "DONE"
	if _, err := beta.UpdateTask(dep.ID, dep.TaskContent, dep.Revision, admin, "dep-done"); err != nil {
		t.Fatal(err)
	}
	// A credential scoped to alpha alone cannot read beta: unresolved, not a
	// refusal about beta.
	tok, _, err := g.db.IssueScoped("admin", g.aliceID, "agent", access.ScopeProject, g.alphaInstance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	scoped := g.aliceIdentity
	scoped.TokenID, scoped.Scope, scoped.Project = tok.ID, access.ScopeProject, g.alphaInstance
	_, err = g.s.reserve(g.personal(scoped), store.ReservationClaim, claimOf(owner), "scoped", "")
	expectRefusal(t, "unreadable dependency project", err, "dependency_unresolved")
	if out, err := g.s.reserve(alice, store.ReservationClaim, claimOf(owner), "done", ""); err != nil || out.Reservation.ID == "" {
		t.Fatalf("claim with a done dependency: %+v, %v", out, err)
	}
}

// A project lifecycle operation that starts while a transition is at its
// final recheck waits for the transition, and both finish: the recheck reads
// the registry, and Drop, rename and merge hold the registry while they wait
// for the project's connection.
func TestReservationTransitionAndProjectDropBothFinish(t *testing.T) {
	g := newReservationRig(t)
	alice := g.personal(g.aliceIdentity)
	task := g.readyTask(t, g.alpha)
	hold, err := g.s.reserve(alice, store.ReservationClaim, claimOf(task), "claim", "")
	if err != nil {
		t.Fatal(err)
	}
	dropped := make(chan error, 1)
	beforeReservationRecheck = func() {
		beforeReservationRecheck = nil
		go func() { dropped <- g.s.reg.Drop("alpha") }()
		time.Sleep(200 * time.Millisecond) // let Drop reach its locks
	}
	t.Cleanup(func() { beforeReservationRecheck = nil })
	updated := make(chan error, 1)
	go func() {
		_, err := g.s.reserve(alice, store.ReservationUpdate, nextOf(task, hold, "IN_PROGRESS"), "update", "")
		updated <- err
	}()
	deadline := time.After(20 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case err := <-updated:
			if err != nil {
				t.Fatalf("update beside a drop: %v", err)
			}
		case err := <-dropped:
			if !errors.Is(err, store.ErrProjectHasTasks) {
				t.Fatalf("drop of a project with tasks: %v", err)
			}
		case <-deadline:
			panic("a reservation transition and a project drop deadlocked")
		}
	}
}
