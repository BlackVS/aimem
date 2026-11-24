package server

// Feature routes (docs/DESIGN-feature-record.md, section 6): the link of a
// feature to its goal, the confirmation of a task on a feature, and the
// goal's features in order. They sit beside the task routes and are guarded
// as those are: a read needs a grant on the project, a write the task write
// authorization, the idempotency key and the expected revision.

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"

	"aimem/internal/store"
)

// maxGoalFeatures caps list_features: a goal has 10 to 15 features, so the
// list has no paging.
const maxGoalFeatures = 100

// featureBody is set_feature's body: the link's fields, or remove.
type featureBody struct {
	Goal             string `json:"goal"`
	Position         int    `json:"position"`
	Mark             string `json:"mark"`
	Ground           string `json:"ground"`
	GroundNote       string `json:"ground_note"`
	Alternative      string `json:"alternative"`
	Remove           bool   `json:"remove"`
	ExpectedRevision int64  `json:"expected_revision"`
}

func (b featureBody) link() store.FeatureLink {
	return store.FeatureLink{Goal: b.Goal, Position: b.Position, Mark: b.Mark, Ground: b.Ground, GroundNote: b.GroundNote, Alternative: b.Alternative}
}

// checkFeatureText checks the link's two prose fields that this write
// changes, as a task write checks its own.
func (s *Server) checkFeatureText(ctx context.Context, l store.FeatureLink, prev *store.FeatureLink) *textRefusal {
	var was store.FeatureLink
	if prev != nil {
		was = *prev
	}
	for _, f := range [][3]string{{"ground_note", l.GroundNote, was.GroundNote}, {"alternative", l.Alternative, was.Alternative}} {
		if f[1] == f[2] {
			continue
		}
		if r := s.checkText(ctx, f[0], f[1]); r != nil {
			return r
		}
	}
	return nil
}

// setFeature is PUT /v1/tasks/{id}/feature.
func (s *Server) setFeature(w http.ResponseWriter, r *http.Request) {
	project, db, ok := s.locateTask(w, r)
	if !ok || !s.authorizeTaskWrite(w, r, project) {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var body featureBody
	if !s.decodeTaskBody(w, r, &body) {
		return
	}
	var link *store.FeatureLink
	if body.Remove {
		if body.link() != (store.FeatureLink{}) {
			s.fail(w, http.StatusBadRequest, errors.New("remove takes no link fields: it turns the feature back into a task"))
			return
		}
	} else {
		l := body.link()
		link = &l
	}
	// A retry of a stored write returns its receipt unchecked: its text was
	// accepted when it was first written.
	if link != nil && !db.SetFeatureReplays(r.PathValue("id"), link, false, body.ExpectedRevision, taskActor(r), key) {
		l := *link
		var prev *store.FeatureLink
		if cur, err := db.GetTask(r.PathValue("id")); err == nil {
			prev = cur.Feature
		}
		if ref := s.checkFeatureText(r.Context(), l, prev); ref != nil {
			s.refuseText(w, ref)
			return
		}
	}
	task, err := db.SetFeature(r.PathValue("id"), link, body.Remove, body.ExpectedRevision, taskActor(r), key)
	if err != nil {
		var conflict *store.TaskConflict
		if errors.As(err, &conflict) {
			s.taskConflict(w, project, err, conflict)
			return
		}
		s.taskError(w, err)
		return
	}
	s.log.Info("feature set", "project", project, "task", task.ID, "revision", task.Revision, "kind", task.Kind, "actor", taskActor(r).Name)
	s.ok(w, taskView(project, task))
}

// errConfirmTaskUnknown answers a task to confirm that does not exist or
// that the caller cannot read: one answer for both.
var errConfirmTaskUnknown = errors.New("task: no task this credential can read has that id")

// confirmFeatureTask is POST /v1/tasks/{id}/feature/tasks. The store sees
// only the feature's own project, so the route checks the task: it exists,
// the caller may read it, and it is not a feature of any project.
func (s *Server) confirmFeatureTask(w http.ResponseWriter, r *http.Request) {
	project, db, ok := s.locateTask(w, r)
	if !ok || !s.authorizeTaskWrite(w, r, project) {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Task             string `json:"task"`
		Remove           bool   `json:"remove"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if !s.decodeTaskBody(w, r, &body) {
		return
	}
	taskID := body.Task
	// Taking a task out needs only its id, which the feature holds: the
	// task may have been dropped or become unreadable since.
	if !body.Remove || !store.FullTaskID(taskID) {
		replays := func(id string) bool {
			return db.ConfirmFeatureTaskReplays(r.PathValue("id"), id, body.Remove, body.ExpectedRevision, taskActor(r), key)
		}
		id, ok := s.confirmableTask(w, r, taskID, replays)
		if !ok {
			return
		}
		taskID = id
	}
	task, err := db.ConfirmFeatureTask(r.PathValue("id"), taskID, body.Remove, body.ExpectedRevision, taskActor(r), key)
	if err != nil {
		var conflict *store.TaskConflict
		if errors.As(err, &conflict) {
			s.taskConflict(w, project, err, conflict)
			return
		}
		s.taskError(w, err)
		return
	}
	s.log.Info("feature task confirmed", "project", project, "feature", task.ID, "task", taskID, "remove", body.Remove, "revision", task.Revision, "actor", taskActor(r).Name)
	s.ok(w, taskView(project, task))
}

// confirmableTask resolves the task argument among the tasks the caller may
// read, in any project, and refuses a feature. A retry of a stored write
// (replays) returns its receipt: the task was accepted when it was first
// written, and may have changed since.
func (s *Server) confirmableTask(w http.ResponseWriter, r *http.Request, arg string, replays func(id string) bool) (string, bool) {
	if arg == "" {
		s.fail(w, http.StatusBadRequest, errors.New("task is required: the id of the task to confirm"))
		return "", false
	}
	readable := func(p string) (bool, error) { return s.canReadTasks(r, p) }
	id, _, err := s.reg.ResolveReadableTaskID(arg, readable)
	var amb *store.AmbiguousIDError
	switch {
	case errors.As(err, &amb):
		s.taskIDError(w, r, err, false)
		return "", false
	case err != nil:
		s.fail(w, http.StatusBadRequest, errConfirmTaskUnknown)
		return "", false
	}
	p, pdb, err := s.reg.LocateTask(id)
	if err != nil {
		if !errors.Is(err, store.ErrTaskNotFound) {
			s.taskError(w, err)
			return "", false
		}
		s.fail(w, http.StatusBadRequest, errConfirmTaskUnknown)
		return "", false
	}
	if ok, err := readable(p); err != nil || !ok {
		s.fail(w, http.StatusBadRequest, errConfirmTaskUnknown)
		return "", false
	}
	if replays(id) {
		return id, true
	}
	t, err := pdb.GetTask(id)
	if err != nil {
		s.taskError(w, err)
		return "", false
	}
	if t.Kind == store.KindFeature {
		s.fail(w, http.StatusBadRequest, errors.New("task: "+t.ID+" is a feature, and a feature confirms tasks, not features"))
		return "", false
	}
	return t.ID, true
}

// featureRow is one row of list_features.
type featureRow struct {
	ID       string              `json:"id"`
	Title    string              `json:"title"`
	State    string              `json:"state"`
	Assignee *store.TaskAssignee `json:"assignee,omitempty"`
	Archived bool                `json:"archived"`
	Revision int64               `json:"revision"`
	Feature  *store.FeatureLink  `json:"feature"`
	// Confirmed counts the ids the feature confirms, of any project,
	// readable or not: the feature's own record lists them.
	Confirmed int `json:"confirmed_tasks"`
	// Ready is the ready-to-close mark (readyToClose).
	Ready bool `json:"ready_to_close"`
	// Tie marks a position another feature of the goal also holds; the
	// hub does not renumber, and the rows keep id order within it.
	Tie   bool `json:"tie,omitempty"`
	Links struct {
		Self string `json:"self"`
	} `json:"links"`
}

// featureRows builds the goal's rows from the features of one read. A
// record that is not a feature of goal is left out, as a guard against data
// no write path produces. finished reports, for each confirmed task id,
// whether the caller may read it and it is DONE or CANCELLED.
func featureRows(goal string, features []store.Task, finished func(id string) bool) []featureRow {
	rows := make([]featureRow, 0, len(features))
	for _, t := range features {
		if t.Feature == nil || t.Epic != goal {
			continue
		}
		row := featureRow{ID: t.ID, Title: t.Title, State: t.State, Assignee: t.Assignee, Archived: t.Archived, Revision: t.Revision, Feature: t.Feature, Confirmed: len(t.Dependencies)}
		row.Ready = readyToClose(t, finished)
		row.Links.Self = "/v1/tasks/" + t.ID
		rows = append(rows, row)
	}
	return rows
}

// readyToClose is the mark: the feature is open, has at least one confirmed
// task, and every confirmed task is one the caller may read that is DONE or
// CANCELLED. A task the caller may not read, or that is gone, withholds the
// mark exactly as an unfinished one does.
func readyToClose(t store.Task, finished func(id string) bool) bool {
	if t.State == "DONE" || t.State == "CANCELLED" || len(t.Dependencies) == 0 {
		return false
	}
	for _, id := range t.Dependencies {
		if !finished(id) {
			return false
		}
	}
	return true
}

// confirmedFinished answers readyToClose's question from the goal's view:
// a record of the goal's project from its snapshot, which the caller may
// read since it reads the listing; a record of another project once per id
// per listing, and only when the caller may read that project. Locating a
// task asks only which project holds its id; the state and kind used here
// come from the one read of the record, made after the grant. A record that
// is gone, unreadable or now a feature withholds the mark exactly as an
// unfinished task does.
func (s *Server) confirmedFinished(r *http.Request, local map[string]store.Confirmed) func(id string) bool {
	seen := map[string]bool{}
	return func(id string) bool {
		if c, ok := local[id]; ok {
			return c.Finished()
		}
		if v, ok := seen[id]; ok {
			return v
		}
		v := false
		if p, pdb, err := s.reg.LocateTask(id); err == nil {
			if ok, err := s.canReadTasks(r, p); err == nil && ok {
				if t, err := pdb.GetTask(id); err == nil {
					v = store.Confirmed{State: t.State, Kind: t.Kind}.Finished()
				}
			}
		}
		seen[id] = v
		return v
	}
}

// listFeatures is GET /v1/projects/{p}/epics/{e}/features: the goal's
// features in position order, each with its ready-to-close mark, and the
// count of the goal's tasks no feature of this project confirms. The goal's
// project is read in one read-only transaction (store.ReadGoalView), so a
// write between its reads changes none of the answer; only a confirmed task
// of another project is read after it, from that project's current state.
func (s *Server) listFeatures(w http.ResponseWriter, r *http.Request) {
	project, db := s.taskReadProject(w, r)
	if db == nil {
		return
	}
	includeArchived := false
	switch r.URL.Query().Get("include_archived") {
	case "", "false", "0":
	case "true", "1":
		includeArchived = true
	default:
		s.fail(w, http.StatusBadRequest, errors.New("include_archived must be true or false"))
		return
	}
	view, err := db.ReadGoalView(r.PathValue("e"), includeArchived, maxGoalFeatures)
	if err != nil {
		s.taskError(w, err)
		return
	}
	rows := featureRows(view.Goal.ID, view.Features, s.confirmedFinished(r, view.Local))
	slices.SortStableFunc(rows, func(a, b featureRow) int {
		return cmp.Or(cmp.Compare(a.Feature.Position, b.Feature.Position), cmp.Compare(a.ID, b.ID))
	})
	for i := range rows {
		if i > 0 && rows[i].Feature.Position == rows[i-1].Feature.Position {
			rows[i].Tie, rows[i-1].Tie = true, true
		}
	}
	s.ok(w, struct {
		Project          string       `json:"project"`
		Goal             string       `json:"goal"`
		Features         []featureRow `json:"features"`
		TasksOnNoFeature int          `json:"tasks_on_no_feature"`
		Truncated        bool         `json:"truncated,omitempty"`
	}{project, view.Goal.ID, rows, view.Unconfirmed, view.Truncated})
}
