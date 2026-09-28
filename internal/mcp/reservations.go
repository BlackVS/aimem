package mcp

// The member reservation tools (task C6a; docs/DESIGN-AIFORGE-RESERVATION-
// WIRE.md): task_reservation_<operation>, task_reservation_status and
// task_reservation_receipt, one per hub route. They carry the same bodies,
// and a refusal comes back as the hub's reservation envelope, typed as the
// wire requires: code, message, retryable, next_action, correlation_id. In a
// team conversation the team session's caller adds the context handle.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"aimem/internal/teamsession"
)

// reservationTools are the seven member tools; team mode serves them too.
var reservationTools = map[string]bool{
	"task_reservation_claim": true, "task_reservation_transfer": true, "task_reservation_update": true,
	"task_reservation_release": true, "task_reservation_finalize": true,
	"task_reservation_status": true, "task_reservation_receipt": true,
}

func reservationCommonProps() map[string]any {
	return map[string]any{
		"version": map[string]any{"type": "integer", "enum": []any{1}, "description": "the reservation protocol version: 1"},
		"project": prop("string", "the task's project (defaults to the current project)"),
		"task_id": prop("string", "task id"),
	}
}

func reservationMutationDef(op, description string, required ...string) map[string]any {
	p := reservationCommonProps()
	p["request_key"] = prop("string", "your request key: reuse it to retry or reconcile this exact step, never a fresh one")
	p["expected_revision"] = prop("integer", "the task revision you read")
	p["reservation_id"] = prop("string", "the current reservation id (every operation but claim)")
	p["fence"] = prop("string", "the current fence, as a decimal string (every operation but claim)")
	p["holder"] = map[string]any{"type": "object", "additionalProperties": false, "required": []any{"mode", "work_ref"},
		"description": "claim and transfer: standalone with your own work reference, or external with aicrew's offer or attempt reference",
		"properties":  map[string]any{"mode": map[string]any{"type": "string", "enum": []any{"standalone", "external"}}, "work_ref": prop("string", "")}}
	p["intent"] = propEnum("update only: block, submit or resume", "block", "submit", "resume")
	p["content"] = map[string]any{"type": "object", "description": "the complete task content (update, release, finalize)"}
	p["reason"] = prop("string", "release and finalize: why")
	p["terminal_evidence"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"},
		"description": "finalize: reviewed-delivery and human-merge evidence; required for DONE"}
	p["coordination_proof"] = prop("string", "team mode: the acp1_ proof aicrew issued for this step (every operation but update); never shown back")
	return map[string]any{"name": "task_reservation_" + op, "description": description,
		"inputSchema": objSchema(p, append([]string{"version", "task_id", "request_key", "expected_revision"}, required...)...)}
}

var reservationToolDefs = []map[string]any{
	reservationMutationDef("claim", "Reserve a READY task: in personal mode a standalone hold on your own work reference; "+
		"in a team conversation an external hold on aicrew's offer or independent attempt, with its coordination_proof.", "holder"),
	reservationMutationDef("transfer", "Team mode: take an offer's hold as its intended worker, onto your accepted attempt, with the accepted-attempt coordination_proof.",
		"reservation_id", "fence", "holder", "coordination_proof"),
	reservationMutationDef("update", "Update the task you hold (complete content), fenced by your reservation; optional intent block, submit or resume.",
		"reservation_id", "fence", "content"),
	reservationMutationDef("release", "Release your hold, leaving the task READY or BLOCKED, with a reason (in team mode with its coordination_proof).",
		"reservation_id", "fence", "content", "reason"),
	reservationMutationDef("finalize", "Finalize the task DONE (with terminal_evidence of reviewed delivery and human merge) or CANCELLED, closing the hold.",
		"reservation_id", "fence", "content", "reason"),
	{
		"name":        "task_reservation_status",
		"description": "Your own reservation on a task: held with its id and fence, or none. Another holder's hold reads as none.",
		"inputSchema": objSchema(reservationCommonProps(), "version", "task_id"),
	},
	{
		"name": "task_reservation_receipt",
		"description": "Reconcile a step whose reply was lost, by its operation and request key: committed with the recorded outcome, " +
			"or not_committed (retry the same key). Never retry with a fresh key.",
		"inputSchema": objSchema(func() map[string]any {
			p := reservationCommonProps()
			p["operation"] = propEnum("the step's operation", "claim", "transfer", "update", "release", "finalize")
			p["request_key"] = prop("string", "the step's request key")
			return p
		}(), "version", "task_id", "operation", "request_key"),
	},
}

type reservationArgs struct {
	Version           int             `json:"version"`
	Project           string          `json:"project"`
	TaskID            string          `json:"task_id"`
	RequestKey        string          `json:"request_key"`
	Operation         string          `json:"operation"`
	ExpectedRevision  int64           `json:"expected_revision"`
	ReservationID     string          `json:"reservation_id"`
	Fence             string          `json:"fence"`
	Holder            json.RawMessage `json:"holder"`
	Intent            string          `json:"intent"`
	Content           json.RawMessage `json:"content"`
	Reason            string          `json:"reason"`
	TerminalEvidence  []string        `json:"terminal_evidence"`
	CoordinationProof string          `json:"coordination_proof"`
}

// reservationRefusal is a hub refusal passed to the tool's caller as the
// wire's typed envelope, so a client acts on its code and retryable flag.
type reservationToolRefusal struct{ envelope string }

func (r *reservationToolRefusal) Error() string { return r.envelope }

func callReservationTool(ctx context.Context, tasks TaskCallFunc, defaultProject, name string, raw json.RawMessage) (string, error) {
	var a reservationArgs
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return "", fmt.Errorf("arguments: %w", err)
		}
	}
	project := a.Project
	if project == "" {
		project = defaultProject
	}
	if project == "" || a.TaskID == "" {
		return "", errors.New("project and task_id are required")
	}
	base := "/v1/projects/" + url.PathEscape(project) + "/tasks/" + url.PathEscape(a.TaskID) + "/reservation"
	// The hub refuses a missing or unsupported version itself; the tool
	// passes what it was given.
	headers := map[string]string{"X-Aimem-Reservation-Version": strconv.Itoa(a.Version)}
	var method, path string
	var body []byte
	switch name {
	case "task_reservation_status":
		method, path = "GET", base
	case "task_reservation_receipt":
		if a.Operation == "" || a.RequestKey == "" {
			return "", errors.New("operation and request_key are required")
		}
		method, path = "GET", base+"/receipts/"+url.PathEscape(a.Operation)+"/"+url.PathEscape(a.RequestKey)
	default:
		if a.RequestKey == "" {
			return "", errors.New("request_key is required")
		}
		headers["Idempotency-Key"] = a.RequestKey
		m := map[string]any{"expected_revision": a.ExpectedRevision}
		for k, v := range map[string]string{"reservation_id": a.ReservationID, "fence": a.Fence, "intent": a.Intent,
			"reason": a.Reason, "coordination_proof": a.CoordinationProof} {
			if v != "" {
				m[k] = v
			}
		}
		if len(a.Holder) > 0 {
			m["holder"] = a.Holder
		}
		if len(a.Content) > 0 {
			m["content"] = a.Content
		}
		if a.TerminalEvidence != nil {
			m["terminal_evidence"] = a.TerminalEvidence
		}
		body, _ = json.Marshal(m)
		method, path = "POST", base+"/"+strings.TrimPrefix(name, "task_reservation_")
	}
	status, resp, err := tasks(ctx, method, path, headers, body)
	if err != nil {
		// A transport failure after sending leaves the outcome unknown:
		// reconcile with task_reservation_receipt and the same key.
		return "", fmt.Errorf("%w; the outcome is unknown: reconcile with task_reservation_receipt and the same request_key", err)
	}
	if status/100 != 2 {
		if teamsession.ParseRefusal(status, resp) != nil {
			return "", &reservationToolRefusal{envelope: strings.TrimSpace(string(resp))}
		}
		return "", fmt.Errorf("HTTP %d", status)
	}
	return strings.TrimSpace(string(resp)), nil
}
