package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"aimem/internal/store"
)

// Epic routes are project-scoped (no cross-project lookup: an epic is
// addressed by the project it belongs to) and sit on the ordinary
// surface; every write passes the same authorization and enablement gate
// as a task write.

type epicResponse struct {
	store.Epic
	Project string `json:"project"`
	Links   struct {
		Self    string `json:"self"`
		Project string `json:"project"`
	} `json:"links"`
}

func epicView(project string, e store.Epic) epicResponse {
	out := epicResponse{Epic: e, Project: project}
	out.Links.Self = "/v1/projects/" + project + "/epics/" + e.ID
	out.Links.Project = "/v1/projects/" + project
	return out
}

func (s *Server) listEpics(w http.ResponseWriter, r *http.Request) {
	project, db := s.taskProject(w, r)
	if db == nil {
		return
	}
	includeRetired := false
	switch r.URL.Query().Get("include_retired") {
	case "", "false", "0":
	case "true", "1":
		includeRetired = true
	default:
		s.fail(w, http.StatusBadRequest, errors.New("include_retired must be true or false"))
		return
	}
	epics, err := db.ListEpics(includeRetired)
	if err != nil {
		s.taskError(w, err)
		return
	}
	views := make([]epicResponse, 0, len(epics))
	for _, e := range epics {
		views = append(views, epicView(project, e))
	}
	s.ok(w, map[string]any{"project": project, "epics": views})
}

func (s *Server) createEpic(w http.ResponseWriter, r *http.Request) {
	project, db := s.taskProject(w, r)
	if db == nil {
		return
	}
	if !s.authorizeTaskWrite(w, r, project) {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var content store.EpicContent
	if !s.decodeTaskBody(w, r, &content) {
		return
	}
	e, err := db.CreateEpic(content, taskActor(r), key)
	if err != nil {
		s.taskError(w, err)
		return
	}
	s.log.Info("epic created", "project", project, "epic", e.ID, "actor", taskActor(r).Name)
	s.created(w, epicView(project, e))
}

func (s *Server) getEpic(w http.ResponseWriter, r *http.Request) {
	project, db := s.taskProject(w, r)
	if db == nil {
		return
	}
	e, err := db.GetEpic(r.PathValue("e"))
	if err != nil {
		s.taskError(w, err)
		return
	}
	s.ok(w, epicView(project, e))
}

type epicUpdateBody struct {
	store.EpicContent
	ExpectedRevision int64 `json:"expected_revision"`
}

func (s *Server) updateEpic(w http.ResponseWriter, r *http.Request) {
	project, db := s.taskProject(w, r)
	if db == nil {
		return
	}
	if !s.authorizeTaskWrite(w, r, project) {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var body epicUpdateBody
	if !s.decodeTaskBody(w, r, &body) {
		return
	}
	e, err := db.UpdateEpic(r.PathValue("e"), body.EpicContent, body.ExpectedRevision, taskActor(r), key)
	if err != nil {
		var conflict *store.EpicConflict
		if errors.As(err, &conflict) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "current": epicView(project, conflict.Current)})
			return
		}
		s.taskError(w, err)
		return
	}
	s.log.Info("epic updated", "project", project, "epic", e.ID, "revision", e.Revision, "state", e.State, "actor", taskActor(r).Name)
	s.ok(w, epicView(project, e))
}

// accessDirectory is the identity directory the board labels assignees
// and authors with: for every user and group the access store holds,
// exactly id, kind, name and whether it is enabled — no tokens, no
// grants, no project lists. On the ordinary surface, pinned by the gate
// matrix and by TestAccessDirectory (docs/DESIGN-kanban-docs.md).
func (s *Server) accessDirectory(w http.ResponseWriter, r *http.Request) {
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	snap, err := db.Snapshot()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, errors.New("cannot read the directory"))
		return
	}
	type entry struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	out := make([]entry, 0, len(snap.Users)+len(snap.Groups))
	for _, u := range snap.Users {
		out = append(out, entry{ID: u.ID, Kind: "user", Name: u.Name, Enabled: !u.Disabled})
	}
	for _, g := range snap.Groups {
		out = append(out, entry{ID: g.ID, Kind: "group", Name: g.Name, Enabled: true})
	}
	s.ok(w, map[string]any{"identities": out})
}
