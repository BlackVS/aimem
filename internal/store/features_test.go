package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// goalEpic creates an open epic to serve as a feature's goal.
func goalEpic(t *testing.T, db *DB, key string) string {
	t.Helper()
	e, err := db.CreateEpic(EpicContent{Title: "a goal", Objective: "the check"}, aliceActor, key)
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

func mandatory(goal string) FeatureLink {
	return FeatureLink{Goal: goal, Position: 1, Mark: "mandatory", Ground: "check"}
}

// featureOf turns task into a feature of goal, setting its epic first.
func featureOf(t *testing.T, db *DB, task Task, goal, key string) Task {
	t.Helper()
	c := task.TaskContent
	c.Epic = goal
	task, err := db.UpdateTask(task.ID, c, task.Revision, aliceActor, key+"-epic")
	if err != nil {
		t.Fatal(err)
	}
	link := mandatory(goal)
	f, err := db.SetFeature(task.ID, &link, false, task.Revision, aliceActor, key)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func mustCreateFeature(t *testing.T, db *DB, goal, title, key string) Task {
	t.Helper()
	c := content(title)
	c.Epic = goal
	f, err := db.CreateFeature(c, mandatory(goal), aliceActor, key)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A client that knows nothing of features sends a task's whole content;
// the kind and the link stay, because they are not part of the content.
func TestFeatureOlderClientUpdateKeepsKindAndLink(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	f := featureOf(t, db, mustCreate(t, db, "a feature", "c"), goal, "f")
	c := f.TaskContent
	c.Title, c.NextAction = "renamed by an older client", "next"
	got, err := db.UpdateTask(f.ID, c, f.Revision, aliceActor, "older")
	if err != nil {
		t.Fatal(err)
	}
	read, err := db.GetTask(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []Task{got, read} {
		if x.Kind != KindFeature || x.Feature == nil || *x.Feature != mandatory(goal) || x.Title != c.Title {
			t.Fatalf("kind or link lost: %+v", x)
		}
	}
	var kind string
	if err := db.sql.QueryRow(`SELECT kind FROM tasks WHERE id=?`, f.ID).Scan(&kind); err != nil || kind != KindFeature {
		t.Fatalf("column: %q, %v", kind, err)
	}
	other := goalEpic(t, db, "other")
	c.Epic = other
	if _, err := db.UpdateTask(f.ID, c, read.Revision, aliceActor, "move"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("a task write moved a feature off its goal: %v", err)
	}
	// A task's JSON is unchanged: no kind, no feature.
	plain := mustCreate(t, db, "plain", "plain")
	raw, _ := json.Marshal(plain)
	if strings.Contains(string(raw), `"kind"`) || strings.Contains(string(raw), `"feature"`) {
		t.Fatalf("task body gained feature fields: %s", raw)
	}
}

// A READY task turned into a feature closes in the feed; the feature's
// own state changes never enter it; a feature born as one never appears;
// removal reopens the record, and is refused while tasks are confirmed.
func TestFeatureFeed(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	c := content("ready work")
	c.State = "READY"
	task, err := db.CreateTask(c, aliceActor, "c")
	if err != nil {
		t.Fatal(err)
	}
	f := featureOf(t, db, task, goal, "f")
	c = f.TaskContent
	c.State = "IN_PROGRESS"
	f, err = db.UpdateTask(f.ID, c, f.Revision, aliceActor, "progress")
	if err != nil {
		t.Fatal(err)
	}
	born := mustCreateFeature(t, db, goal, "born a feature", "born")
	feed := feedOf(t, db)
	if len(feed) != 2 || feed[1].TaskID != task.ID || feed[1].From != "READY" || feed[1].To != FeatureFeedState || feed[1].Revision != f.Revision-1 {
		t.Fatalf("feed after conversion: %+v", feed)
	}
	for _, e := range feed {
		if e.TaskID == born.ID {
			t.Fatalf("a feature born as one entered the feed: %+v", e)
		}
	}
	other := mustCreate(t, db, "confirmed", "o")
	f, err = db.ConfirmFeatureTask(f.ID, other.ID, false, f.Revision, aliceActor, "confirm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetFeature(f.ID, nil, true, f.Revision, aliceActor, "remove-early"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("removal with confirmed tasks: %v", err)
	}
	f, err = db.ConfirmFeatureTask(f.ID, other.ID, true, f.Revision, aliceActor, "unconfirm")
	if err != nil || len(f.Dependencies) != 0 {
		t.Fatalf("take out: %+v, %v", f, err)
	}
	back, err := db.SetFeature(f.ID, nil, true, f.Revision, aliceActor, "remove")
	if err != nil || back.Kind != "" || back.Feature != nil {
		t.Fatalf("remove: %+v, %v", back, err)
	}
	feed = feedOf(t, db)
	last := feed[len(feed)-1]
	if last.TaskID != task.ID || last.From != FeatureFeedState || last.To != "IN_PROGRESS" || last.Revision != back.Revision {
		t.Fatalf("removal entry: %+v", last)
	}
}

// A claim across projects refuses a dependency that is a feature and names
// it; a claim of a feature itself is refused at both claim paths.
func TestFeatureClaimRefusals(t *testing.T) {
	r, ownerDB, depDB, owner, dep := dependencyFixture(t)
	goal := goalEpic(t, depDB, "goal")
	f := featureOf(t, depDB, dep, goal, "f")
	_, err := r.ClaimTaskReservation(context.Background(), claimInput(owner, "run"), aliceActor, testBinding(aliceActor), "claim", nil, allowDependencyRead)
	if !errors.Is(err, ErrFeatureNotWork) || !errors.Is(err, ErrDependencyUnresolved) || !strings.Contains(err.Error(), f.ID) || !strings.Contains(err.Error(), owner.ID) {
		t.Fatalf("cross-project feature dependency: %v", err)
	}
	ownerGoal := goalEpic(t, ownerDB, "owner-goal")
	c := content("ready feature")
	c.State, c.Epic = "READY", ownerGoal
	rf, err := ownerDB.CreateFeature(c, mandatory(ownerGoal), aliceActor, "rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTaskReservation(context.Background(), claimInput(rf, "run"), aliceActor, testBinding(aliceActor), "claim-f", nil, allowDependencyRead); !errors.Is(err, ErrFeatureNotWork) {
		t.Fatalf("claim of a feature: %v", err)
	}
	if _, err := ownerDB.ApplyTaskReservation(ReservationClaim, claimInput(rf, "run"), aliceActor, testBinding(aliceActor), "raw", nil, allowReservation); !errors.Is(err, ErrFeatureNotWork) {
		t.Fatalf("store-only claim of a feature: %v", err)
	}
}

// The walker checks again under the locks: a dependency that becomes a
// feature after the walk is refused by the locked validation, before its
// changed revision is even compared.
func TestFeatureClaimRefusedUnderLocks(t *testing.T) {
	r, ownerDB, depDB, owner, _ := dependencyFixture(t)
	nodes, err := r.locateDependencyGraph(context.Background(), owner, allowDependencyRead)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := depDB.GetTask(owner.Dependencies[0])
	if err != nil {
		t.Fatal(err)
	}
	featureOf(t, depDB, dep, goalEpic(t, depDB, "goal"), "f")
	// The walk itself stops at the feature, before any lock is taken.
	if _, err := r.locateDependencyGraph(context.Background(), owner, allowDependencyRead); !errors.Is(err, ErrFeatureNotWork) {
		t.Fatalf("walk: %v", err)
	}
	txs := map[string]*sql.Tx{}
	for _, db := range []*DB{ownerDB, depDB} {
		tx, err := db.sql.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		txs[db.projectID] = tx
	}
	if _, _, err := validateDependencyGraph(owner.ID, nodes, txs, allowDependencyRead, context.Background()); !errors.Is(err, ErrFeatureNotWork) {
		t.Fatalf("locked validation: %v", err)
	}
}

// In one project, no write makes a task depend on a feature: not a task
// write, not a conversion of a record a task depends on, and not a feature
// naming another feature. The offer refuses one that is there anyway.
func TestFeatureSameProjectDependents(t *testing.T) {
	_, d, team, a, _, offer := assignmentFixture(t)
	goal := goalEpic(t, d, "goal")
	f := mustCreateFeature(t, d, goal, "a feature", "f")
	task, err := d.GetTask(offer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	c := task.TaskContent
	c.Dependencies = []string{f.ID}
	if _, err := d.UpdateTask(task.ID, c, task.Revision, aliceActor, "dep"); !errors.Is(err, ErrFeatureNotWork) || !strings.Contains(err.Error(), f.ID) {
		t.Fatalf("task write naming a feature: %v", err)
	}
	if _, err := d.CreateTask(c, aliceActor, "dep-create"); !errors.Is(err, ErrFeatureNotWork) {
		t.Fatalf("create naming a feature: %v", err)
	}
	g := mustCreateFeature(t, d, goal, "another feature", "g")
	if _, err := d.ConfirmFeatureTask(g.ID, f.ID, false, g.Revision, aliceActor, "ff"); !errors.Is(err, ErrFeatureNotWork) {
		t.Fatalf("a feature confirmed on a feature: %v", err)
	}
	dependent := mustCreate(t, d, "depends", "depends")
	c = dependent.TaskContent
	c.Dependencies = []string{task.ID}
	if _, err := d.UpdateTask(dependent.ID, c, dependent.Revision, aliceActor, "dependent"); err != nil {
		t.Fatal(err)
	}
	c = task.TaskContent
	c.Epic = goal
	task, err = d.UpdateTask(task.ID, c, task.Revision, aliceActor, "epic")
	if err != nil {
		t.Fatal(err)
	}
	link := mandatory(goal)
	if _, err := d.SetFeature(task.ID, &link, false, task.Revision, aliceActor, "convert"); err == nil || !strings.Contains(err.Error(), dependent.ID) {
		t.Fatalf("conversion with a dependent: %v", err)
	}
	// Data the guards would not write (an older hub, a hand edit): the offer
	// still refuses it and names the feature.
	if _, err := d.sql.Exec(`UPDATE tasks SET body=json_set(body,'$.dependencies',json_array(?)) WHERE id=?`, f.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	offer.ExpectedRevision = task.Revision
	if _, err := d.OfferTeamAssignment(team.ID, offer, a, "offer", allowAssignmentWorker); !errors.Is(err, ErrFeatureNotWork) || !strings.Contains(err.Error(), f.ID) {
		t.Fatalf("offer of a task depending on a feature: %v", err)
	}
}

// A feature is offered to no one, so it is never team-managed; a reserved
// or managed task cannot become one.
func TestFeatureOfferAndManagementRefused(t *testing.T) {
	_, d, team, a, _, offer := assignmentFixture(t)
	task, err := d.GetTask(offer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	f := featureOf(t, d, task, goalEpic(t, d, "goal"), "f")
	offer.ExpectedRevision = f.Revision
	if _, err := d.OfferTeamAssignment(team.ID, offer, a, "offer", allowAssignmentWorker); !errors.Is(err, ErrFeatureNotWork) {
		t.Fatalf("offer of a feature: %v", err)
	}
	if n := countRows(t, d, "team_managed_tasks", "task_id=?", f.ID); n != 0 {
		t.Fatalf("a feature became managed: %d", n)
	}

	_, d2, team2, a2, _, offer2 := assignmentFixture(t)
	goal2 := goalEpic(t, d2, "goal")
	pre, err := d2.GetTask(offer2.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	c := pre.TaskContent
	c.Epic = goal2
	pre, err = d2.UpdateTask(pre.ID, c, pre.Revision, aliceActor, "epic")
	if err != nil {
		t.Fatal(err)
	}
	offer2.ExpectedRevision = pre.Revision
	mustOffer(t, d2, team2, a2, offer2, "offer")
	managed, err := d2.GetTask(offer2.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	link := mandatory(goal2)
	if _, err := d2.SetFeature(managed.ID, &link, false, managed.Revision, aliceActor, "convert"); !errors.Is(err, ErrManagedTask) {
		t.Fatalf("conversion of a managed task: %v", err)
	}

	_, db := taskDB(t)
	goal3 := goalEpic(t, db, "goal")
	held := readyReservationTask(t, db, "held")
	c = held.TaskContent
	c.Epic = goal3
	held, err = db.UpdateTask(held.ID, c, held.Revision, aliceActor, "epic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(held, "run"), aliceActor, testBinding(aliceActor), "claim", nil, allowReservation); err != nil {
		t.Fatal(err)
	}
	link = mandatory(goal3)
	if _, err := db.SetFeature(held.ID, &link, false, held.Revision, aliceActor, "convert"); !errors.Is(err, ErrTaskReserved) {
		t.Fatalf("conversion of a reserved task: %v", err)
	}
}

// A feature moves into DONE only with evidence; a record already DONE may
// be converted, and a later edit that keeps DONE is not a move into it.
func TestFeatureDoneNeedsEvidence(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	c := content("done at birth")
	c.State, c.Epic = "DONE", goal
	if _, err := db.CreateFeature(c, mandatory(goal), aliceActor, "born-done"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("born DONE without evidence: %v", err)
	}
	f := mustCreateFeature(t, db, goal, "open", "f")
	c = f.TaskContent
	c.State = "DONE"
	if _, err := db.UpdateTask(f.ID, c, f.Revision, aliceActor, "done"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("DONE without evidence: %v", err)
	}
	c.EvidenceRefs = []TaskRef{{Kind: "ci", Ref: "https://example.org/runs/1"}}
	if _, err := db.UpdateTask(f.ID, c, f.Revision, aliceActor, "done-evidence"); err != nil {
		t.Fatalf("DONE with evidence: %v", err)
	}
	c = content("already done")
	c.State = "DONE"
	done, err := db.CreateTask(c, aliceActor, "d")
	if err != nil {
		t.Fatal(err)
	}
	converted := featureOf(t, db, done, goal, "convert-done")
	c = converted.TaskContent
	c.NextAction = "kept DONE"
	if _, err := db.UpdateTask(converted.ID, c, converted.Revision, aliceActor, "edit-done"); err != nil {
		t.Fatalf("edit that keeps DONE: %v", err)
	}
}

// Lists and counts are of tasks by default, a search finds any record,
// and kind and feature narrow both.
func TestFeatureListsAndCounts(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	a := mustCreate(t, db, "alpha task", "a")
	mustCreate(t, db, "beta task", "b")
	f := mustCreateFeature(t, db, goal, "alpha feature", "f")
	f, err := db.ConfirmFeatureTask(f.ID, a.ID, false, f.Revision, aliceActor, "confirm")
	if err != nil {
		t.Fatal(err)
	}
	ids := func(p TaskPage) []string {
		var out []string
		for _, s := range p.Tasks {
			out = append(out, s.ID+"/"+s.Kind)
		}
		return out
	}
	for _, tc := range []struct {
		f    TaskFilter
		want int
	}{
		{TaskFilter{}, 2},
		{TaskFilter{Kind: "task"}, 2},
		{TaskFilter{Kind: "feature"}, 1},
		{TaskFilter{Kind: "any"}, 3},
		{TaskFilter{Feature: f.ID}, 1},
		{TaskFilter{Query: "alpha"}, 2},
		{TaskFilter{Query: "alpha", Kind: "task"}, 1},
	} {
		p, err := db.ListTasks(tc.f)
		if err != nil || len(p.Tasks) != tc.want {
			t.Fatalf("list %+v: %v, %v", tc.f, ids(p), err)
		}
		if tc.f.Kind == "feature" && p.Tasks[0].Kind != KindFeature {
			t.Fatalf("feature row without kind: %+v", p.Tasks[0])
		}
		if tc.f.Feature != "" && p.Tasks[0].ID != a.ID {
			t.Fatalf("feature filter: %v", ids(p))
		}
	}
	for kind, want := range map[string]int{"": 2, "task": 2, "feature": 1, "any": 3} {
		c, err := db.CountTasks(TaskFilter{Kind: kind})
		if err != nil || c.Total != want || c.Counts["BACKLOG"] != want {
			t.Fatalf("counts %q: %+v, %v", kind, c, err)
		}
	}
	if c, err := db.CountTasks(TaskFilter{Feature: f.ID}); err != nil || c.Total != 1 {
		t.Fatalf("counts of a feature: %+v, %v", c, err)
	}
	for _, bad := range []TaskFilter{{Kind: "epic"}, {Feature: a.ID}, {Feature: "not-an-id"}} {
		if _, err := db.ListTasks(bad); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("list %+v: %v", bad, err)
		}
		if _, err := db.CountTasks(bad); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("count %+v: %v", bad, err)
		}
	}
}

// The readers that see every record see a feature as they see a task:
// its history, its comments, its location, and the guard that keeps a
// project holding one from being dropped.
func TestFeatureUnchangedReaders(t *testing.T) {
	r, db := taskDB(t)
	f := mustCreateFeature(t, db, goalEpic(t, db, "goal"), "a feature", "f")
	if h, err := db.TaskHistory(f.ID, 0, 10); err != nil || len(h.Changes) != 1 || h.Changes[0].Task.Kind != KindFeature {
		t.Fatalf("history: %+v, %v", h, err)
	}
	if _, err := db.AddTaskComment(f.ID, "a note", aliceActor, "note"); err != nil {
		t.Fatal(err)
	}
	if p, err := db.TaskComments(f.ID, 0, 10); err != nil || len(p.Comments) != 1 {
		t.Fatalf("comments: %+v, %v", p, err)
	}
	if project, _, err := r.LocateTask(f.ID); err != nil || project != db.projectID {
		t.Fatalf("locate: %q, %v", project, err)
	}
	if has, err := db.HasTasks(); err != nil || !has {
		t.Fatalf("HasTasks: %t, %v", has, err)
	}
}

// Schema 26 adds the kind to an existing database: every record there is a
// task, and its body is untouched.
func TestFeatureMigration(t *testing.T) {
	r, db := taskDB(t)
	task := mustCreate(t, db, "before", "before")
	for _, q := range []string{`DROP INDEX idx_tasks_kind`, `ALTER TABLE tasks DROP COLUMN kind`, `UPDATE meta SET value='25' WHERE key='schema_version'`} {
		if _, err := db.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	root := r.Root()
	r.Close()
	r2, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r2.Close)
	db2, err := r2.Open(db.projectID)
	if err != nil {
		t.Fatal(err)
	}
	var kind, index string
	if err := db2.sql.QueryRow(`SELECT kind FROM tasks WHERE id=?`, task.ID).Scan(&kind); err != nil || kind != "task" {
		t.Fatalf("kind: %q, %v", kind, err)
	}
	if err := db2.sql.QueryRow(`SELECT name FROM sqlite_master WHERE name='idx_tasks_kind'`).Scan(&index); err != nil {
		t.Fatalf("index: %v", err)
	}
	if got, err := db2.GetTask(task.ID); err != nil || got.Kind != "" || got.Revision != task.Revision {
		t.Fatalf("read after migration: %+v, %v", got, err)
	}
	if _, err := db2.sql.Exec(`UPDATE tasks SET kind='epic' WHERE id=?`, task.ID); err == nil {
		t.Fatal("the column accepted a kind outside task and feature")
	}
}

// Confirming is a write of the feature alone; a second confirm writes
// nothing, a task not confirmed cannot be taken out, and a task's record
// is not a feature to confirm on.
func TestFeatureConfirm(t *testing.T) {
	_, db := taskDB(t)
	f := mustCreateFeature(t, db, goalEpic(t, db, "goal"), "a feature", "f")
	task := mustCreate(t, db, "work", "w")
	f1, err := db.ConfirmFeatureTask(f.ID, task.ID, false, f.Revision, aliceActor, "c1")
	if err != nil || f1.Revision != f.Revision+1 || len(f1.Dependencies) != 1 {
		t.Fatalf("confirm: %+v, %v", f1, err)
	}
	if again, err := db.ConfirmFeatureTask(f.ID, task.ID, false, f1.Revision, aliceActor, "c2"); err != nil || again.Revision != f1.Revision {
		t.Fatalf("second confirm wrote: %+v, %v", again, err)
	}
	other := mustCreate(t, db, "other", "o")
	if _, err := db.ConfirmFeatureTask(f.ID, other.ID, true, f1.Revision, aliceActor, "c3"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("take out a task not confirmed: %v", err)
	}
	if _, err := db.ConfirmFeatureTask(task.ID, other.ID, false, task.Revision, aliceActor, "c4"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("confirm on a task: %v", err)
	}
	if _, err := db.ConfirmFeatureTask(f.ID, task.ID, false, f.Revision, aliceActor, "c5"); err == nil {
		t.Fatal("stale revision accepted")
	}
}

// The feed's consumer reads every state with this shape and refuses a
// whole page when one entry fails it (aicrew internal/hubteams/hubteams.go,
// stateShape); the closing value must keep it and must not be a state.
func TestFeatureFeedStateShape(t *testing.T) {
	if !regexp.MustCompile(`^[A-Z][A-Z_]{0,31}$`).MatchString(FeatureFeedState) || slices.Contains(TaskStates, FeatureFeedState) {
		t.Fatalf("FeatureFeedState %q", FeatureFeedState)
	}
}

// A held claim whose dependency became a feature afterwards (another
// project's write, which this project cannot refuse) is reported changed,
// with the feature named; the hold stays.
func TestFeatureReconcileNamesFeature(t *testing.T) {
	r, ownerDB, depDB, owner, dep := dependencyFixture(t)
	got, err := r.ClaimTaskReservation(context.Background(), claimInput(owner, "run"), aliceActor, testBinding(aliceActor), "claim", nil, allowDependencyRead)
	if err != nil {
		t.Fatal(err)
	}
	featureOf(t, depDB, dep, goalEpic(t, depDB, "goal"), "f")
	changed, err := r.ReconcileReservationDependencies(context.Background(), owner.ID, allowDependencyRead)
	if !changed || !errors.Is(err, ErrFeatureNotWork) || !strings.Contains(err.Error(), dep.ID) {
		t.Fatalf("reconcile: %t, %v", changed, err)
	}
	if hold, err := ownerDB.GetTaskReservation(owner.ID); err != nil || hold.ID != got.Reservation.ID {
		t.Fatalf("hold: %+v, %v", hold, err)
	}
}

// Only a dependency a write adds is refused: a reserved record that names a
// feature anyway (data no guard wrote) can still be released.
func TestFeatureExistingDependencyDoesNotBlockRelease(t *testing.T) {
	_, db := taskDB(t)
	f := mustCreateFeature(t, db, goalEpic(t, db, "goal"), "a feature", "f")
	held := readyReservationTask(t, db, "held")
	out, err := db.ApplyTaskReservation(ReservationClaim, claimInput(held, "run"), aliceActor, testBinding(aliceActor), "claim", nil, allowReservation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE tasks SET body=json_set(body,'$.dependencies',json_array(?)) WHERE id=?`, f.ID, held.ID); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetTask(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := current.TaskContent
	in := TaskReservationInput{TaskID: held.ID, ExpectedRevision: current.Revision, ID: out.Reservation.ID, Fence: out.Reservation.Fence, Content: &c, Reason: "handing back"}
	if _, err := db.ApplyTaskReservation(ReservationRelease, in, aliceActor, testBinding(aliceActor), "release", nil, allowReservation); err != nil {
		t.Fatalf("release of a record naming a feature: %v", err)
	}
}

// Every rule of a feature's link refuses through both writes that take
// one, a creation and a conversion, with its own message: the epic rule of
// every write refuses some of the same links, so the message is what shows
// the link's own rule ran.
func TestFeatureLinkValidation(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	other := goalEpic(t, db, "other")
	for i, tc := range []struct {
		name string
		want string // the rule's own message
		epic string
		link FeatureLink
	}{
		{"position zero", "position must be", goal, FeatureLink{Goal: goal, Position: 0, Mark: "optional"}},
		{"position too large", "position must be", goal, FeatureLink{Goal: goal, Position: maxFeaturePosition + 1, Mark: "optional"}},
		{"mandatory without a ground", "needs a ground", goal, FeatureLink{Goal: goal, Position: 1, Mark: "mandatory"}},
		{"mandatory with an unknown ground", "needs a ground", goal, FeatureLink{Goal: goal, Position: 1, Mark: "mandatory", Ground: "vibes"}},
		{"optional with a ground", "has no ground", goal, FeatureLink{Goal: goal, Position: 1, Mark: "optional", Ground: "check"}},
		{"unknown mark", "mark must be", goal, FeatureLink{Goal: goal, Position: 1, Mark: "nice"}},
		{"goal differs from the epic", "epic as the goal", goal, FeatureLink{Goal: other, Position: 1, Mark: "optional"}},
		{"no epic", "set the record's epic first", "", FeatureLink{Goal: goal, Position: 1, Mark: "optional"}},
		{"ground note with a control character", "ground_note", goal, FeatureLink{Goal: goal, Position: 1, Mark: "optional", GroundNote: "a\x00b"}},
		{"alternative too long", "alternative", goal, FeatureLink{Goal: goal, Position: 1, Mark: "optional", Alternative: strings.Repeat("a", maxFeatureNoteBytes+1)}},
	} {
		c := content("feature " + tc.name)
		c.Epic = tc.epic
		if _, err := db.CreateFeature(c, tc.link, aliceActor, fmt.Sprintf("create-%d", i)); !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: create: %v", tc.name, err)
		}
		task, err := db.CreateTask(c, aliceActor, fmt.Sprintf("task-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		link := tc.link
		if _, err := db.SetFeature(task.ID, &link, false, task.Revision, aliceActor, fmt.Sprintf("convert-%d", i)); !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: convert: %v", tc.name, err)
		}
		if got, err := db.GetTask(task.ID); err != nil || got.Kind != "" || got.Revision != task.Revision {
			t.Errorf("%s: the refused conversion wrote: %+v, %v", tc.name, got, err)
		}
	}
	if p, err := db.ListTasks(TaskFilter{Kind: "feature"}); err != nil || len(p.Tasks) != 0 {
		t.Fatalf("a refused link left a feature: %+v, %v", p, err)
	}
}

// SetFeature's own input rules: a removal needs a feature, exactly one of
// a link and remove is given, and a stale revision is a conflict carrying
// the current record.
func TestFeatureSetInputs(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	c := content("plain")
	c.Epic = goal
	plain, err := db.CreateTask(c, aliceActor, "plain")
	if err != nil {
		t.Fatal(err)
	}
	before := len(feedOf(t, db))
	if _, err := db.SetFeature(plain.ID, nil, true, plain.Revision, aliceActor, "remove-plain"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("removal on a plain task: %v", err)
	}
	if got, _ := db.GetTask(plain.ID); got.Revision != plain.Revision || len(feedOf(t, db)) != before {
		t.Fatalf("the refused removal wrote: rev %d, feed %d", got.Revision, len(feedOf(t, db)))
	}
	link := mandatory(goal)
	if _, err := db.SetFeature(plain.ID, &link, true, plain.Revision, aliceActor, "both"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("both a link and remove: %v", err)
	}
	if _, err := db.SetFeature(plain.ID, nil, false, plain.Revision, aliceActor, "neither"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("neither a link nor remove: %v", err)
	}
	f, err := db.SetFeature(plain.ID, &link, false, plain.Revision, aliceActor, "convert")
	if err != nil {
		t.Fatal(err)
	}
	moved := FeatureLink{Goal: goal, Position: 2, Mark: "optional"}
	_, err = db.SetFeature(f.ID, &moved, false, plain.Revision, aliceActor, "stale")
	var conflict *TaskConflict
	if !errors.As(err, &conflict) || conflict.Current.Revision != f.Revision || *conflict.Current.Feature != link {
		t.Fatalf("stale revision: %v", err)
	}
	if got, _ := db.GetTask(f.ID); *got.Feature != link {
		t.Fatalf("a stale write changed the link: %+v", got.Feature)
	}
}
