package teamsetup

// An accepted attempt keeps the process version it was accepted under
// (docs/DESIGN-portable-team-context.md, decision 6). The checkout's accept
// path records that version before it sends the accept (mcp.AcceptWithRecord);
// here, for a reserved attempt in an accepted state, the project process
// part is that exact version, recovered with Load's live checks. A newer
// project selection is reported, never delivered as the attempt's rules; a
// missing, foreign or malformed record is not ready, so the attempt is
// handled like any other missing context (a RUNNING attempt is blocked).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"aimem/internal/process"
	"aimem/internal/processctx"
	"aimem/internal/teamstate"
	"aimem/internal/uuidv7"
)

// acceptedStates are the states an attempt reaches only through an accept.
var acceptedStates = map[string]bool{"RUNNING": true, "BLOCKED": true, "STOP_REQUESTED": true, "STOPPED": true, "SUBMITTED": true}

func availabilityFor(rd *Readiness) string {
	if rd.ReadyForWork {
		return "available"
	}
	return "unavailable"
}

// announce sends the availability readiness calls for and records it as the
// availability check; a refusal fails the run, keeping the membership.
func (s *setup) announce(me *Session) (string, bool) {
	rd := s.report.Readiness
	availability := availabilityFor(rd)
	status, body, err := s.teamCall("team_heartbeat", map[string]any{"team": me.TeamID, "session_id": me.ID, "generation": me.Generation, "availability": availability, "idempotency_key": uuidv7.New()})
	if err != nil || status != http.StatusOK {
		if !rd.ReadyForWork {
			s.report.Next = append(s.report.Next, "NOT ready for work ("+rd.notReady()+") and the unavailable announcement was not accepted: the hub may still offer you work (a join starts available). Accept nothing and decline any offer with this reason")
		}
		s.dropCheck("availability")
		return availability, s.fail("availability", "heartbeat ("+availability+") not accepted: "+hubOutcome(status, body, err), "membership is saved, nothing was left or released; the hub still shows the availability it last accepted. Re-run to verify it")
	}
	s.dropCheck("availability")
	if rd.ReadyForWork {
		s.check("availability", "ok", "announced available", "")
	} else {
		s.check("availability", "warn", "announced unavailable: not ready for work ("+rd.notReady()+"), so the hub will neither offer work to this session nor let it accept any", "")
	}
	return availability, true
}

// dropCheck removes earlier checks of that name: a re-announcement replaces
// the first one instead of contradicting it.
func (s *setup) dropCheck(name string) {
	kept := s.report.Checks[:0]
	for _, c := range s.report.Checks {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	s.report.Checks = kept
}

// pinAccepted makes the project process part the version the reserved
// attempt was accepted under, when the attempt is in an accepted state, and
// drops a record whose attempt the hub no longer reserves. It reports
// whether readiness changed.
func (s *setup) pinAccepted(me *Session) bool {
	rd, r, st := s.report.Readiness, s.report.Reserved, s.state
	if st == nil {
		return false
	}
	rec := st.Accepted
	if rec != nil && (r == nil || r.ID != rec.Attempt) {
		st.Accepted = nil
		s.saveState()
		s.check("accepted version", "ok", "the recorded process version of attempt "+rec.Attempt+" is dropped: the hub no longer reserves that attempt for this session", "")
		rec = nil
	}
	if r == nil || !acceptedStates[r.State] {
		return false
	}
	before := rd.ReadyForWork
	part := s.recoverAccepted(me, r.ID, rec)
	rd.ProjectProcess = part
	rd.ReadyForWork = rd.Membership.State == memberActive && rd.RoleContext.State == "delivered" && part.State == string(processctx.Ready)
	return rd.ReadyForWork != before
}

// recoverAccepted resolves the attempt's recorded version and replaces the
// project process delivery with it; the current selection's delivery is
// removed either way, because it is not this attempt's rules.
func (s *setup) recoverAccepted(me *Session, attempt string, rec *teamstate.AcceptedAttempt) Part {
	s.dropDelivery("project_process")
	current := s.report.Readiness.ProjectProcess.Version
	notRecorded := func(why string) Part {
		return Part{State: "not_recorded",
			Detail: "attempt " + attempt + " was accepted, but the process version it was accepted under is not known here: " + why + "; no version is assumed, and the project's current selection is not this attempt's rules",
			Fix:    "the coordinator decides how the attempt continues (for example a stop and a new offer); do not continue it under a guessed version"}
	}
	switch {
	case rec == nil:
		return notRecorded("this checkout holds no record of that accept (accepted from another checkout, through the hub endpoint, by an aimem before this record existed, or the record was lost)")
	case rec.SessionID != me.ID:
		return notRecorded("the record belongs to session " + rec.SessionID + ", not " + me.ID)
	}
	ref := process.Ref{Repo: rec.Repo, Commit: rec.Commit, Manifest: rec.Manifest}
	if err := ref.Validate(); err != nil {
		return notRecorded("the record is malformed (" + err.Error() + ")")
	}
	if s.env.ProcessAt == nil {
		return Part{State: "not_checked", Version: rec.Commit, Detail: "this entry point cannot recover the recorded process version of attempt " + attempt}
	}
	res := s.env.ProcessAt(s.env.Dir, s.sel.Project, ref)
	res.PinnedFor = "accepted attempt " + attempt
	part := Part{State: string(res.State), Detail: res.Detail, Fix: res.Fix, Version: rec.Commit}
	newer := ""
	if res.Current != nil && res.Current.Commit != rec.Commit {
		newer = res.Current.Commit
	} else if res.Current == nil && current != "" && current != rec.Commit {
		newer = current
	}
	if newer != "" {
		s.check("accepted version", "ok", fmt.Sprintf("attempt %s keeps the process version it was accepted under (%s); the project now selects %s, which applies to new work only", attempt, rec.Commit, newer), "")
	}
	if !res.Complete() {
		part.Detail = "the process version attempt " + attempt + " was accepted under (" + rec.Commit + ") is not recoverable here: " + res.Detail
		return part
	}
	text, err := res.Deliver()
	if err != nil {
		return Part{State: "unavailable", Version: rec.Commit, Detail: err.Error()}
	}
	sum := sha256.Sum256([]byte(res.Unit))
	part.Digest = "sha256:" + hex.EncodeToString(sum[:])
	s.report.Delivered = append(s.report.Delivered, Delivery{Kind: "project_process", Text: text})
	if res.State == processctx.Ready {
		// The commit is the version. The unit's own digest is not compared
		// with the one recorded at accept: its text names where the files
		// came from (Git or this machine's cache) and which skills are
		// installed here, so it differs across reads of the same commit.
		part.Detail = "the process version attempt " + attempt + " was accepted under follows this report, pinned; it stays that attempt's rules even when the project selects a newer one"
	} else {
		part.Detail = "the pinned version of attempt " + attempt + " is delivered with its notice but is not ready: " + res.Detail
	}
	return part
}

// attemptUnknown is a worker run that could not establish its reserved
// attempt (the heartbeat or the reserved read failed): which process
// version applies is unknown, because an accepted attempt keeps its own.
// The current selection is withheld and the worker is not ready. It
// reports whether readiness changed.
func (s *setup) attemptUnknown() bool {
	rd := s.report.Readiness
	before := rd.ReadyForWork
	s.dropDelivery("project_process")
	rd.ProjectProcess = Part{State: "not_evaluated", Version: "",
		Detail: "the reserved attempt could not be read, so which process version applies (an accepted attempt keeps the one it was accepted under) is unknown; nothing is delivered as the process",
		Fix:    "re-run team_continue when the hub answers"}
	rd.ReadyForWork = false
	return rd.ReadyForWork != before
}

func (s *setup) dropDelivery(kind string) {
	kept := s.report.Delivered[:0]
	for _, d := range s.report.Delivered {
		if d.Kind != kind {
			kept = append(kept, d)
		}
	}
	s.report.Delivered = kept
}
