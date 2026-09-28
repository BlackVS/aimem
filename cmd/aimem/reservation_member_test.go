package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"aimem/internal/mcp"
)

// A usage error exits 2 with nothing run; a valid command passes its
// request through, prints the one document and returns the engine's code.
func TestMemberReservationArgs(t *testing.T) {
	const proof = "acp1_MEMBERPROOFMEMBERPROOFMEMBERPROOF"
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  *mcp.ReservationRequest
	}{
		{"no command", nil, "", nil},
		{"unknown command", []string{"grab", "--task", "t", "--key", "k"}, "{}", nil},
		{"no task", []string{"status"}, "", nil},
		{"status with a key", []string{"status", "--task", "t", "--key", "k"}, "", nil},
		{"mutation without a key", []string{"claim", "--task", "t"}, "{}", nil},
		{"receipt without an operation", []string{"receipt", "--task", "t", "--key", "k"}, "", nil},
		{"receipt of a read", []string{"receipt", "status", "--task", "t", "--key", "k"}, "", nil},
		{"a body as an argument", []string{"claim", "--task", "t", "--key", "k", `{"fence":"1"}`}, "{}", nil},
		{"an unknown flag", []string{"claim", "--task", "t", "--key", "k", "--proof", proof}, "{}", nil},
		{"no body", []string{"claim", "--task", "t", "--key", "k"}, "", nil},
		{"a non-object body", []string{"claim", "--task", "t", "--key", "k"}, "[1]", nil},
		{"two documents", []string{"claim", "--task", "t", "--key", "k"}, `{} {}`, nil},
		{"a reserved field", []string{"claim", "--task", "t", "--key", "k"}, `{"request_key":"other"}`, nil},
		{"a body one byte over the bound", []string{"claim", "--task", "t", "--key", "k"},
			`{"reason":"` + strings.Repeat("x", 1<<18+1-len(`{"reason":""}`)) + `"}`, nil},
		{"a body at the bound", []string{"claim", "--task", "t", "--key", "k"},
			`{"reason":"` + strings.Repeat("x", 1<<18-len(`{"reason":""}`)) + `"}`, &mcp.ReservationRequest{Command: "claim", TaskID: "t", Key: "k"}},
		{"status", []string{"status", "--task", "t"}, "", &mcp.ReservationRequest{Command: "status", TaskID: "t"}},
		{"receipt", []string{"receipt", "finalize", "--task", "t", "--key", "k"}, "",
			&mcp.ReservationRequest{Command: "receipt", TaskID: "t", Key: "k", Operation: "finalize"}},
		{"claim", []string{"claim", "--task", "t", "--key", "k"}, `{"fence":"1","coordination_proof":"` + proof + `"}` + "\n",
			&mcp.ReservationRequest{Command: "claim", TaskID: "t", Key: "k"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got *mcp.ReservationRequest
			run := func(_ context.Context, req mcp.ReservationRequest) (string, int) {
				got = &req
				return `{"ok":true}`, 4
			}
			var out, errOut bytes.Buffer
			code := runMemberReservation(c.args, strings.NewReader(c.stdin), &out, &errOut, run)
			if strings.Contains(out.String()+errOut.String(), proof) {
				t.Fatal("the proof was printed")
			}
			if c.want == nil {
				if code != mcp.ExitUsage || got != nil || out.Len() != 0 || !strings.Contains(errOut.String(), "usage:") {
					t.Fatalf("want a usage error: code %d, ran %v, out %q", code, got != nil, out.String())
				}
				return
			}
			if code != 4 || out.String() != "{\"ok\":true}\n" || got == nil {
				t.Fatalf("want the engine's answer: code %d, out %q", code, out.String())
			}
			if got.Command != c.want.Command || got.TaskID != c.want.TaskID || got.Key != c.want.Key || got.Operation != c.want.Operation {
				t.Fatalf("request %+v, want %+v", *got, *c.want)
			}
			if c.want.Command == "claim" && c.name == "claim" {
				if string(got.Body["fence"]) != `"1"` || string(got.Body["coordination_proof"]) != `"`+proof+`"` {
					t.Fatalf("the body did not pass through: %v", got.Body)
				}
			} else if (got.Body != nil) != (c.want.Command == "claim") {
				t.Fatalf("a %s carries no body", got.Command)
			}
		})
	}
}
