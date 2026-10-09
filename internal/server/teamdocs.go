package server

// A member's report as a hub document (aicrew's control-plane design, A0c,
// 8.4). In team mode, PUT /v1/projects/{p}/docs/{name} writes a document
// whose name starts with report-, in a project the team profile is granted,
// with the same compare-and-swap revisions as any document write. Every
// member role may deliver a report. The revision names the member and its
// verified team context, and the write is audited. The prefix keeps a team
// member from rewriting the project's other documents, which other sessions
// read as context. Deleting, merging and the log stay off in team mode.

import (
	"fmt"
	"net/http"
	"strings"

	"aimem/internal/store"
)

// teamReportPrefix is the name prefix of the documents team mode may write.
const teamReportPrefix = "report-"

// teamDocWriteDenied applies team mode's rules to a document write and
// answers the refusal when they deny it: a report- name, an ordinary
// project, and the profile's live grant on it.
func (s *Server) teamDocWriteDenied(w http.ResponseWriter, r *http.Request, tc teamContext) bool {
	id, _ := IdentityFrom(r.Context())
	p, name := r.PathValue("p"), r.PathValue("name")
	detail := teamAuditDetail(tc.Context, r) + fmt.Sprintf(" project=%q doc=%q", p, name)
	switch {
	case !strings.HasPrefix(name, teamReportPrefix):
		s.teamDeny(w, r, tc.Role, id, "invalid_request", detail+" reason=report_name", tc.CorrelationID)
		return true
	case store.IsReservedProject(p):
		s.teamDeny(w, r, tc.Role, id, "grant_denied", detail+" reason=reserved", tc.CorrelationID)
		return true
	}
	instance, err := s.knowledgeInstance(p)
	switch {
	case err != nil:
		s.teamDeny(w, r, tc.Role, id, "identity_unavailable", detail+" reason=grant_store", tc.CorrelationID)
		return true
	case instance == "":
		// An unknown project reads the same as one without a grant.
		s.teamDeny(w, r, tc.Role, id, "grant_denied", detail, tc.CorrelationID)
		return true
	}
	return s.teamGrantDenied(w, r, p)
}

// teamWriter is a team-mode revision's updated_by: the member's token name,
// its verified team and role, then the client's own label.
func teamWriter(r *http.Request, tc teamContext, by string) string {
	id, _ := IdentityFrom(r.Context())
	out := fmt.Sprintf("%s/team:%s/%s", id.Name, tc.TeamID, tc.Role)
	if by != "" {
		out += "/" + by
	}
	return out
}

// auditTeamDocWrite records a team member's document write under the
// member, with the verified context and the revision written. An audit that
// cannot be written is logged; the write has already committed.
func (s *Server) auditTeamDocWrite(r *http.Request, tc teamContext, doc store.Doc) {
	id, _ := IdentityFrom(r.Context())
	subject := teamAuditDetail(tc.Context, r) + fmt.Sprintf(" project=%q doc=%q rev=%d correlation=%s", r.PathValue("p"), doc.Name, doc.Rev, tc.CorrelationID)
	db, err := s.openAccess(false)
	if err == nil {
		err = db.RecordTeamRequest(teamAuditActor(id), "team.doc.write", subject)
	}
	if err != nil {
		s.log.Error("team document audit", "project", r.PathValue("p"), "doc", doc.Name, "err", err)
	}
}
