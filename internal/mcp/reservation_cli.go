package mcp

// The member reservation CLI's engine (task C6c; coordination wire §4). The
// `aimem reservation` commands run one reservation tool exactly as the stdio
// facade would: with AIMEM_TEAM_SESSION, as that verified team conversation
// (the same pinned binding, context header and online verification, and no
// personal caller to fall back to); without it, with the checkout's personal
// credential. The answer is one JSON document: the outcome, or the typed
// refusal envelope. The proof, read from standard input, is never printed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"

	"aimem/internal/teamsession"
)

// Exit codes of the reservation CLI (coordination wire §4).
const (
	ExitCommitted = 0 // committed, or a replayed committed outcome
	ExitUsage     = 2
	ExitFinal     = 3 // a final refusal
	ExitRetryable = 4 // a retryable refusal
	ExitUnknown   = 5 // sent, but no answer: reconcile with receipt and the same key
)

// ReservationRequest is one CLI command: an operation (claim, transfer,
// update, release, finalize, receipt or status), the task, the request key,
// the receipt's operation, and a mutation's body from standard input.
type ReservationRequest struct {
	Command   string
	TaskID    string
	Key       string
	Operation string
	Body      map[string]json.RawMessage
}

// reservationCLIServer is the facade the CLI runs its tool on, chosen as
// Serve chooses it.
var reservationCLIServer = func() *srv {
	if path := os.Getenv(teamsession.EnvVar); path != "" {
		return newTeamSrv(path, mcpStateRoot(), "")
	}
	return &srv{taskSetup: localTaskCaller}
}

// ReservationCommand runs one CLI command and returns the JSON document to
// print and the exit code.
func ReservationCommand(ctx context.Context, req ReservationRequest) (string, int) {
	return reservationCommand(ctx, reservationCLIServer(), req)
}

func reservationCommand(ctx context.Context, s *srv, req ReservationRequest) (string, int) {
	fail := func(err error) (string, int) {
		r := reservationToolError(err)
		return r.envelope, reservationExit(r)
	}
	project, err := s.reservationProject(ctx, req.TaskID)
	if err != nil {
		return fail(err)
	}
	args := map[string]any{"version": 1, "project": project, "task_id": req.TaskID}
	name := "task_reservation_" + req.Command
	switch req.Command {
	case "status":
	case "receipt":
		args["operation"], args["request_key"] = req.Operation, req.Key
	default:
		for k, v := range req.Body {
			args[k] = v
		}
		args["request_key"] = req.Key
	}
	raw, _ := json.Marshal(args)
	text, err := s.taskTool(ctx, name, raw)
	if err != nil {
		return fail(err)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(text), "", "  ") != nil {
		return fail(newReservationRefusal("context_unavailable", "The hub's answer is not readable JSON.", "", true, ""))
	}
	return pretty.String(), ExitCommitted
}

// reservationProject finds the task's current project, which the member
// routes carry in their path: a task read, over the same connection. In team
// mode it first verifies the team context, as the facade does before a
// reservation tool; the calls then go through the team caller only.
func (s *srv) reservationProject(ctx context.Context, taskID string) (string, error) {
	if s.team != nil {
		if err := s.reservationTeamReady(ctx); err != nil {
			return "", err
		}
	}
	raw, _ := json.Marshal(map[string]string{"id": taskID})
	text, err := s.taskTool(ctx, "get_task", raw)
	if err != nil {
		var ref *teamsession.Refusal
		switch {
		case errors.As(err, &ref):
			return "", err
		case hubTransportError(err):
			return "", newReservationRefusal("context_unavailable", "The hub did not answer.", "", true, "")
		}
		return "", newReservationRefusal("task_unavailable", "The task could not be read: "+err.Error(), "", false, "")
	}
	var t struct {
		Project string `json:"project"`
	}
	if json.Unmarshal([]byte(text), &t) != nil || t.Project == "" {
		return "", newReservationRefusal("task_unavailable", "The task read named no project.", "", false, "")
	}
	return t.Project, nil
}

// hubTransportError reports a failure to reach the hub or to read its
// answer, whichever layer wrapped it (the caller, the local credential's
// check): nothing the hub decided, so the same command may be retried.
func hubTransportError(err error) bool {
	var ne net.Error // an *url.Error from the HTTP client is one too
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded)
}

// reservationExit maps a typed refusal to the CLI's exit code.
func reservationExit(r *reservationToolRefusal) int {
	if r.unknownOutcome {
		return ExitUnknown
	}
	var e struct {
		Retryable bool `json:"retryable"`
	}
	if json.Unmarshal([]byte(r.envelope), &e) == nil && e.Retryable {
		return ExitRetryable
	}
	return ExitFinal
}
