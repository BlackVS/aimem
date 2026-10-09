package server

// aicrewd's board feed, board.read (aicrew's control-plane design, A0):
// the task state changes of every project granted to the calling peer's
// enabled teams, read with one call per tick for all of them. Each project
// keeps its own sequence of changes (store.TaskStateChanges); the cursor
// carries the position reached in each project, so a page never skips or
// repeats a change, and the same cursor reads the same page again while
// nothing new is written. Entries carry no task content.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

const (
	boardReadDefaultLimit = 100
	boardReadMaxLimit     = 500
	boardCursorPrefix     = "b1."
)

// boardCursor is the decoded cursor: the peer it was issued to and, per
// project instance, the last sequence read. Positions of projects no
// longer granted are carried forward, so a grant given back resumes where
// the peer stopped.
type boardCursor struct {
	Service   string           `json:"service"`
	Positions map[string]int64 `json:"positions"`
}

func (c boardCursor) encode() string {
	b, _ := json.Marshal(c) // map keys marshal sorted: one cursor per state
	return boardCursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

var errBoardCursor = errors.New("invalid board cursor")

func decodeBoardCursor(raw, service string) (boardCursor, error) {
	c := boardCursor{Service: service, Positions: map[string]int64{}}
	if raw == "" {
		return c, nil
	}
	enc, ok := strings.CutPrefix(raw, boardCursorPrefix)
	if !ok {
		return c, errBoardCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return c, errBoardCursor
	}
	var got boardCursor
	if json.Unmarshal(b, &got) != nil || got.Service != service {
		return c, errBoardCursor
	}
	for inst, pos := range got.Positions {
		if inst == "" || pos < 1 {
			return c, errBoardCursor
		}
	}
	if got.Positions != nil {
		c.Positions = got.Positions
	}
	return c, nil
}

// boardChangeView is one feed entry.
type boardChangeView struct {
	Project  string `json:"project"`
	TaskID   string `json:"task_id"`
	Revision int64  `json:"revision"`
	From     string `json:"from"`
	To       string `json:"to"`
	At       string `json:"at"`
}

type boardReadView struct {
	Changes []boardChangeView `json:"changes"`
	Cursor  string            `json:"cursor"`
	More    bool              `json:"more"`
}

// readBoard is GET /v1/identity/peers/{service_id}/board-changes. Projects
// are read in the order of their instance IDs; a page that fills up
// leaves the remaining projects at their positions and says more.
func (s *Server) readBoard(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.teamPeer(w, r)
	if !ok {
		return
	}
	limit := boardReadDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > boardReadMaxLimit {
			s.teamOpRefuse(w, r, peer, "invalid_request")
			return
		}
		limit = n
	}
	cursor, err := decodeBoardCursor(r.URL.Query().Get("cursor"), peer.ServiceID)
	if err != nil {
		s.teamOpRefuse(w, r, peer, "invalid_cursor")
		return
	}
	db, err := s.openAccess(false)
	if err != nil {
		s.identityRefuse(w, "identity_unavailable")
		return
	}
	profiles, err := db.ListTeamProfiles(peer.ServiceID)
	if err != nil {
		s.teamReadFailed(w, err)
		return
	}
	granted := map[string]bool{}
	for _, p := range profiles {
		if p.Disabled {
			continue
		}
		instances, err := db.TeamGrantInstances(p.ID)
		if err != nil {
			s.teamReadFailed(w, err)
			return
		}
		for _, inst := range instances {
			granted[inst] = true
		}
	}
	names, err := s.projectNamesByInstance()
	if err != nil {
		s.teamReadFailed(w, err)
		return
	}
	out := boardReadView{Changes: []boardChangeView{}}
	next := boardCursor{Service: peer.ServiceID, Positions: maps.Clone(cursor.Positions)}
	for _, inst := range slices.Sorted(maps.Keys(granted)) {
		project := names[inst]
		if project == "" {
			continue // a grant whose project is gone
		}
		pdb, err := s.reg.OpenExisting(project)
		if err != nil {
			s.teamReadFailed(w, err)
			return
		}
		pos := cursor.Positions[inst]
		room := limit - len(out.Changes)
		changes, last, err := pdb.TaskStateChanges(pos, max(room, 1))
		if err != nil {
			s.teamReadFailed(w, err)
			return
		}
		if pos > last {
			s.teamOpRefuse(w, r, peer, "cursor_ahead")
			return
		}
		if room > 0 {
			for _, c := range changes {
				out.Changes = append(out.Changes, boardChangeView{project, c.TaskID, c.Revision, c.From, c.To, c.At})
				pos = c.Sequence
			}
			if pos > 0 {
				next.Positions[inst] = pos
			}
		}
		out.More = out.More || last > pos
	}
	out.Cursor = next.encode()
	if err := db.RecordTeamRequest("peer:"+peer.ServiceID, "board.read", fmt.Sprintf("service=%s changes=%d", peer.ServiceID, len(out.Changes))); err != nil {
		s.teamReadFailed(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, out)
}
