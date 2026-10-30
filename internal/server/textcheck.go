package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"

	"aimem/internal/store"
	"aimem/internal/textcheck"
	"aimem/internal/writingrule"
)

// textJudge is the readability check's second stage; nil means the filter
// decides alone.
type textJudge interface {
	Readable(ctx context.Context, span, around string) (bool, error)
}

// setupTextCheck resolves the check once, at start, like the embedding
// model: whether the filter runs, and the second stage.
func (s *Server) setupTextCheck() {
	on, err := textcheck.FilterOn()
	if err != nil {
		s.log.Warn("text check switch not understood", "err", err)
	}
	s.textOff = !on
	if !on {
		s.log.Warn("text check off: task text is stored unchecked", "key", textcheck.FilterKey)
	}
	j, why := textcheck.ForRoot(s.reg.Root())
	s.textCheck, s.textCheckWhy = j, why
	if j != nil {
		s.judge = j
		return
	}
	// A model the operator named that cannot serve is a misconfiguration,
	// and this line is the only place it would show.
	if on && os.Getenv(textcheck.ModelKey) != "" {
		s.log.Warn("text check second stage off", "why", why)
	}
}

// Stages named in a refusal.
const (
	stageFilter      = "filter"
	stageFilterModel = "filter_and_model"
)

// textRefusal is a write refused because a field is not readable prose.
type textRefusal struct {
	Field   string
	Stage   string
	Finding *writingrule.Finding
}

// checkText runs the filter over one field and, for flagged text only,
// asks the second stage, which can only relax the verdict.
func (s *Server) checkText(ctx context.Context, field, text string) *textRefusal {
	if s.textOff {
		return nil
	}
	f := writingrule.Unreadable(text)
	if f == nil {
		return nil
	}
	stage := stageFilter
	if s.judge != nil {
		readable, err := s.judge.Readable(ctx, f.Span, writingrule.Context(text, f.Span, textcheck.ContextBytes))
		switch {
		case err != nil:
			s.log.Warn("text check second stage failed; the filter's verdict stands", "field", field, "err", err)
		case readable:
			s.log.Info("text check: the model found flagged text readable", "field", field, "span", f.Span)
			return nil
		default:
			stage = stageFilterModel
		}
	}
	return &textRefusal{Field: field, Stage: stage, Finding: f}
}

// checkTaskFields checks the task's free-text fields; when prev is not
// nil, only the fields this write changes, so text stored before the
// check existed never blocks an unrelated update.
func (s *Server) checkTaskFields(ctx context.Context, c store.TaskContent, prev *store.TaskContent) *textRefusal {
	var was [][2]string
	if prev != nil {
		was = prev.TextFields()
	}
	for i, f := range c.TextFields() {
		if was != nil && was[i][1] == f[1] {
			continue
		}
		if r := s.checkText(ctx, f[0], f[1]); r != nil {
			return r
		}
	}
	return nil
}

// checkTaskChange checks a write that replaces task id's content: only the
// fields it changes. A retry of a stored write (replay) is not checked: it
// returns its receipt, and the task may have moved on since. A task that
// cannot be read is left to the store, which answers for it.
func (s *Server) checkTaskChange(ctx context.Context, db *store.DB, id string, c store.TaskContent, replay bool) *textRefusal {
	if replay {
		return nil
	}
	cur, err := db.GetTask(id)
	if err != nil {
		return nil
	}
	return s.checkTaskFields(ctx, c, &cur.TaskContent)
}

// refuseText answers 422 in the hub's refusal envelope (code, message,
// next_action), which the MCP tools render as "code: message Next: fix";
// error carries the same whole sentence for readers of the plain shape.
func (s *Server) refuseText(w http.ResponseWriter, r *textRefusal) {
	decided := "the filter alone"
	if r.Stage == stageFilterModel {
		decided = "the filter, confirmed by the model"
	}
	msg := r.Field + ": " + r.Finding.Message() + " Decided by " + decided + "."
	s.log.Info("task write refused: unreadable text", "field", r.Field, "span", r.Finding.Span, "stage", r.Stage)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	json.NewEncoder(w).Encode(map[string]any{
		"error": "unreadable_text: " + msg + " Next: " + r.Finding.Fix, "code": "unreadable_text", "message": msg,
		"next_action": r.Finding.Fix, "retryable": false,
		"field": r.Field, "span": r.Finding.Span, "reason": r.Finding.Reason, "section": r.Finding.Section, "stage": r.Stage,
	})
}
