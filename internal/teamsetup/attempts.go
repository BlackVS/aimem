package teamsetup

// A worker that is not ready and holds an attempt (docs/DESIGN-portable-team-
// context.md, decision 4): the attempt is handled by its actual state, only
// through the existing worker operations, and never beyond them.
//
//   OFFERED        decline with the readiness reason (the hub's decline: the
//                  offer returns to the coordinator; it was never accepted)
//   RUNNING        block with the missing context (the attempt stays
//                  reserved; the reason becomes the task blocker)
//   BLOCKED        preserved; no second block
//   STOP_REQUESTED nothing sent: only the worker can establish that its local
//                  execution stopped, and nothing here claims it did
//   others         nothing sent
//
// Accept, resume-work, stopped and leave are never sent here. Each write is
// recorded (with its retry key and exact content) before it is sent, so an
// uncertain outcome is retried as the identical request, and only while the
// attempt is still where the write found it.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"aimem/internal/teamstate"
	"aimem/internal/uuidv7"
)

// sourceState is the attempt state a not-ready write applies to.
var sourceState = map[string]string{"decline": "OFFERED", "block": "RUNNING"}

// handleUnreadyAttempt applies the not-ready rules to the reserved attempt,
// after first reconciling a write an earlier run could not confirm.
func (s *setup) handleUnreadyAttempt(me *Session) {
	rd, r, st := s.report.Readiness, s.report.Reserved, s.state
	if st == nil {
		return
	}
	if w := st.PendingWork; w != nil {
		stale := ""
		switch {
		case rd.ReadyForWork:
			stale = "this session is ready now"
		case r == nil || r.ID != w.Attempt:
			stale = "the attempt is no longer reserved for this session"
		case r.State != sourceState[w.Op]:
			stale = "the attempt is " + r.State + " now"
		case w.Generation != me.Generation:
			stale = "the session generation changed"
		}
		if stale != "" {
			// It landed, or something else moved the attempt on: either way
			// the recorded request no longer applies and is not sent again.
			st.PendingWork = nil
			s.saveState()
			s.check("attempt", "ok", fmt.Sprintf("the earlier unconfirmed %s of attempt %s is not repeated: %s", w.Op, w.Attempt, stale), "")
		}
	}
	if rd.ReadyForWork || r == nil {
		return
	}
	switch r.State {
	case "OFFERED":
		s.attemptWrite(me, r, "decline")
	case "RUNNING":
		s.attemptWrite(me, r, "block")
	case "BLOCKED":
		s.check("attempt", "ok", "attempt "+r.ID+" is already BLOCKED: preserved as it is, no second block", "")
	case "STOP_REQUESTED":
		s.check("attempt", "warn", "attempt "+r.ID+" has a stop requested: nothing was sent; whether your local work has stopped is yours to establish before team_stopped", "")
	}
}

// attemptWrite sends one decline or block for the reserved attempt: the
// recorded request when an earlier run left one for this attempt, otherwise
// a new one recorded before it is sent.
func (s *setup) attemptWrite(me *Session, r *Reserved, op string) {
	st := s.state
	w := st.PendingWork
	if w == nil {
		w = &teamstate.PendingWork{Op: op, Attempt: r.ID, Generation: me.Generation, Key: uuidv7.New()}
		switch op {
		case "decline":
			w.Reason = "Declined automatically: this session is not ready for work (" + s.report.Readiness.notReady() + "). Nothing was done on the task. Offer it again once the member reports ready_for_work, or to another member."
		case "block":
			rev, err := s.taskRevision(r.TaskID)
			if err != nil {
				s.check("attempt", "warn", "attempt "+r.ID+" is RUNNING but was not blocked: "+err.Error(), "block it yourself with team_block and the missing context")
				return
			}
			w.ExpectedRevision = rev
			w.Reason = "Blocked automatically: this session is not ready for work (" + s.report.Readiness.notReady() + "). Work on the attempt stops until team_continue reports ready_for_work; the attempt stays reserved to this worker."
		}
		st.PendingWork = w
		if err := s.saveState(); err != nil {
			st.PendingWork = nil
			s.check("attempt", "warn", "attempt "+r.ID+" was not "+past(op)+": the retry key could not be recorded before sending ("+err.Error()+")", "make the state root writable and re-run; nothing was sent")
			return
		}
	}
	args := map[string]any{"team": me.TeamID, "session_id": me.ID, "generation": me.Generation, "attempt": r.ID, "reason": w.Reason, "idempotency_key": w.Key}
	name := "team_decline"
	if op == "block" {
		name = "team_block"
		args["expected_revision"] = w.ExpectedRevision
	}
	status, body, err := s.teamCall(name, args)
	var res struct {
		Protocol   int `json:"protocol_version"`
		Assignment struct {
			State string `json:"state"`
		} `json:"assignment"`
	}
	switch {
	case err != nil || (status == http.StatusOK && (json.Unmarshal(body, &res) != nil || res.Protocol != 1 || res.Assignment.State == "")):
		// The hub may or may not have applied it: the record stays, and the
		// next run replays the identical request while the attempt is still
		// where this one found it.
		detail := "unreadable reply"
		if err != nil {
			detail = err.Error()
		}
		s.check("attempt", "warn", fmt.Sprintf("the %s of attempt %s was sent but its outcome is unknown (%s); the retry key is kept and the next run repeats the identical request only while the attempt is still %s", op, r.ID, detail, r.State), "re-run team_continue")
	case status == http.StatusOK:
		st.PendingWork = nil
		s.saveState()
		r.State = res.Assignment.State
		s.check("attempt", "ok", fmt.Sprintf("attempt %s %s: %s", r.ID, past(op), w.Reason), "")
	default:
		// A definite refusal: nothing was applied; the record is dropped and
		// the next run decides again from the attempt's state.
		st.PendingWork = nil
		s.saveState()
		s.check("attempt", "warn", fmt.Sprintf("the %s of attempt %s was refused (%s); nothing changed on the hub", op, r.ID, hubOutcome(status, body, nil)), "re-run team_continue; if the attempt is still "+r.State+", "+op+" it yourself with the readiness reason")
	}
}

func past(op string) string {
	if op == "decline" {
		return "declined"
	}
	return "blocked"
}

// taskRevision reads the task's current revision, the one a block must
// name; it is never guessed.
func (s *setup) taskRevision(id string) (int64, error) {
	if s.env.Task == nil {
		return 0, fmt.Errorf("this entry point cannot read the task revision a block needs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), HubTimeout)
	defer cancel()
	status, body, err := s.env.Task(ctx, id)
	if err != nil || status != http.StatusOK {
		return 0, fmt.Errorf("task %s not read: %s", id, hubOutcome(status, body, err))
	}
	var t struct {
		Revision int64 `json:"revision"`
	}
	if json.Unmarshal(body, &t) != nil || t.Revision < 1 {
		return 0, fmt.Errorf("task %s: no revision in the hub's reply", id)
	}
	return t.Revision, nil
}

// reservedNext writes the guidance for the reserved attempt, from its state
// after the handling above: a member that is not ready is never told to
// accept, continue or resume.
func (s *setup) reservedNext() {
	r := s.report.Reserved
	if r == nil {
		return
	}
	if !s.report.Readiness.ReadyForWork {
		r.Next = unreadyNext(r.State)
	}
	s.report.Next = append(s.report.Next, "reserved attempt "+r.ID+" ("+r.State+"): "+r.Next)
	switch r.State {
	case "RUNNING", "BLOCKED", "STOP_REQUESTED":
		s.report.Next = append(s.report.Next, s.reconcileLine())
	}
}

// unreadyNext is the worker's step for an attempt while it is not ready.
func unreadyNext(state string) string {
	switch state {
	case "OFFERED":
		return "not ready for work: do not accept it; decline it with the readiness reason (team_decline), which this run could not do (see the attempt check)"
	case "DECLINED":
		return "declined because this session is not ready for work; the coordinator can offer the task again; nothing further here"
	case "RUNNING":
		return "not ready for work: stop working on it and block it with the missing context (team_block), which this run could not do (see the attempt check); do not continue the work"
	case "BLOCKED":
		return "not ready for work: keep it blocked; do not resume it (team_resume_work) until team_continue reports ready_for_work; read the inbox for the coordinator's decision"
	case "STOP_REQUESTED":
		return "the coordinator asked you to stop: reconcile your local execution yourself and send team_stopped only once you have established that the work stopped; nothing in this report acknowledges a stop"
	}
	return AttemptNext(state)
}
