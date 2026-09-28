package main

// aimem reservation claim|transfer|update|release|finalize|receipt|status:
// the member reservation CLI (task C6c; coordination wire §4), which aicrew's
// client drives deterministically. It runs as the process's team session
// (AIMEM_TEAM_SESSION) or, without it, in personal mode, and never falls back
// from team to personal. A mutation's body, including its coordination
// proof, is read from standard input only. The answer is one JSON document on
// standard output, and the exit code says what to do next.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"aimem/internal/mcp"
)

const memberReservationUsage = `usage: aimem reservation claim|transfer|update|release|finalize --task TASK_ID --key REQUEST_KEY < body.json
       aimem reservation receipt OPERATION --task TASK_ID --key REQUEST_KEY
       aimem reservation status --task TASK_ID

A member's reservation step on a task, over this process's connection: the
team session named by AIMEM_TEAM_SESSION, or personal mode without it (never a
fallback from team to personal).

A mutation's body is one JSON object on standard input, never an argument:
  {"expected_revision": N, "reservation_id": "...", "fence": "N",
   "holder": {"mode": "standalone|external", "work_ref": "..."},
   "content": {complete task content}, "intent": "block|submit|resume",
   "reason": "...", "terminal_evidence": ["..."], "coordination_proof": "acp1_..."}
Each operation takes the fields the reservation wire requires. REQUEST_KEY is
your request key: repeat it to retry or reconcile, never switch it.

Output: one JSON document, the outcome or the refusal envelope. It never
contains the proof.
Exit: 0 committed (or a replay of a committed step); 3 final refusal;
4 retryable refusal; 5 outcome unknown (the step was sent and no answer came:
run "aimem reservation receipt OPERATION" with the same key); 2 usage.

Operator recovery is "aimem reservation recover" (run it alone for its usage).`

var memberMutations = map[string]bool{"claim": true, "transfer": true, "update": true, "release": true, "finalize": true}

// memberReserved are the fields the CLI sets from its arguments; a body
// cannot carry them.
var memberReserved = map[string]bool{"version": true, "project": true, "task_id": true, "request_key": true, "operation": true}

// runMemberReservation runs one member command and returns its exit code.
func runMemberReservation(args []string, stdin io.Reader, out, errOut io.Writer,
	run func(context.Context, mcp.ReservationRequest) (string, int)) int {
	usage := func(msg string) int {
		fmt.Fprintf(errOut, "aimem reservation: %s\n\n%s\n", msg, memberReservationUsage)
		return mcp.ExitUsage
	}
	if len(args) == 0 {
		return usage("a command is required")
	}
	req := mcp.ReservationRequest{Command: args[0]}
	rest := args[1:]
	switch {
	case memberMutations[req.Command], req.Command == "status":
	case req.Command == "receipt":
		if len(rest) == 0 || !memberMutations[rest[0]] {
			return usage("receipt needs the step's operation: claim, transfer, update, release or finalize")
		}
		req.Operation, rest = rest[0], rest[1:]
	default:
		return usage("unknown command " + req.Command)
	}
	fs := flag.NewFlagSet("aimem reservation "+req.Command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&req.TaskID, "task", "", "")
	fs.StringVar(&req.Key, "key", "", "")
	if err := fs.Parse(rest); err != nil {
		return usage(err.Error())
	}
	switch {
	case fs.NArg() != 0:
		return usage("unexpected arguments; a body goes on standard input")
	case req.TaskID == "":
		return usage("--task is required")
	case req.Command == "status" && req.Key != "":
		return usage("status takes no --key")
	case req.Command != "status" && req.Key == "":
		return usage("--key is required")
	}
	if memberMutations[req.Command] {
		body, err := readMemberBody(stdin)
		if err != nil {
			return usage(err.Error())
		}
		req.Body = body
	}
	text, code := run(context.Background(), req)
	fmt.Fprintln(out, text)
	return code
}

// readMemberBody reads exactly one bounded JSON object from standard input.
func readMemberBody(stdin io.Reader) (map[string]json.RawMessage, error) {
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<18+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<18 {
		return nil, fmt.Errorf("the body on standard input exceeds %d bytes", 1<<18)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var body map[string]json.RawMessage
	if err := dec.Decode(&body); err != nil || body == nil || dec.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("the body must be one JSON object on standard input")
	}
	for k := range body {
		if memberReserved[k] {
			return nil, fmt.Errorf("the body cannot set %s; it comes from the arguments", k)
		}
	}
	return body, nil
}
