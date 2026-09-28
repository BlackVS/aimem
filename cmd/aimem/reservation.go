package main

// aimem reservation recover: the operator's reservation recovery (task C5c)
// over the hub's TLS listener, with the same hub trust and admin-token rules
// as aimem identity. A recovery body, which may carry a stop proof, is read
// from standard input only.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"aimem/internal/store"
)

const reservationUsage = `usage: aimem reservation recover release --task TASK_ID --key KEY [hub flags] < body.json
       aimem reservation recover cancel  --task TASK_ID --key KEY [hub flags] < body.json
       aimem reservation recover status  --task TASK_ID           [hub flags]
       aimem reservation recover receipt OPERATION --task TASK_ID --key-digest k1_... [hub flags]

Operator recovery of a task reservation whose holder cannot close it. release
leaves the task READY or BLOCKED; cancel leaves it CANCELLED; recovery never
completes a task. status and receipt are the recovery reader: any hold, and
any actor's receipt for an operation and request-key digest.

The body of release and cancel is JSON on standard input, never an argument:
  {"reservation_id": "...", "fence": "N", "expected_revision": N,
   "content": {complete task content}, "reason": "...",
   "evidence": {"kind": "attestation", "attestation_id": "...", "statement": "..."}
            or {"kind": "stop_evidence", "proof": "acp1_..."}}
A stop proof must be bound to this --key. KEY is your request key: repeat it
to retry, and never switch keys to force a new attempt.

Hub flags (after the other arguments), as for aimem identity:
  --hub https://HOST:PORT    the hub's TLS listener (required; http is refused)
  --admin-token-file PATH    a hub-admin bearer on one line, in a file only you
                             can read (required)
  --hub-ca-file PATH         trust only this CA bundle for the hub
  --hub-pin sha256-BASE64    trust only a hub certificate with this SPKI SHA-256
  Without --hub-ca-file or --hub-pin the system roots are used. There is no
  insecure mode.`

var cliKeyDigest = regexp.MustCompile(`^k1_[A-Za-z0-9_-]{43}$`)

func reservationCmd(args []string) error {
	return runReservation(args, os.Stdin, os.Stdout)
}

func runReservation(args []string, stdin io.Reader, out io.Writer) error {
	usage := fmt.Errorf("%s", reservationUsage)
	if len(args) < 2 || args[0] != "recover" {
		return usage
	}
	verb, rest := args[1], args[2:]
	var operation string
	if verb == "receipt" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return usage
		}
		operation, rest = rest[0], rest[1:]
	}
	fs := flag.NewFlagSet("aimem reservation recover "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hub := fs.String("hub", "", "")
	tokenFile := fs.String("admin-token-file", "", "")
	caFile := fs.String("hub-ca-file", "", "")
	pin := fs.String("hub-pin", "", "")
	task := fs.String("task", "", "")
	key := fs.String("key", "", "")
	digest := fs.String("key-digest", "", "")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%v\n\n%s", err, reservationUsage)
	}
	if fs.NArg() != 0 || *task == "" {
		return usage
	}
	var body []byte
	switch verb {
	case "release", "cancel":
		if *key == "" || *digest != "" {
			return fmt.Errorf("recover %s needs --key and takes no --key-digest", verb)
		}
		raw, err := io.ReadAll(io.LimitReader(stdin, 1<<18+1))
		if err != nil {
			return err
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) > 1<<18 || len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
			return errors.New("the recovery body must be one JSON object on standard input")
		}
		body = raw
	case "status":
		if *key != "" || *digest != "" {
			return usage
		}
	case "receipt":
		if *key != "" || !cliKeyDigest.MatchString(*digest) {
			return errors.New("recover receipt needs --key-digest k1_... (the digest, never the key)")
		}
	default:
		return usage
	}
	c, err := newIdentityClient(*hub, *tokenFile, *caFile, *pin)
	if err != nil {
		return err
	}
	path := "/v1/admin/reservations/" + url.PathEscape(*task) + "/recovery"
	var status int
	var data []byte
	switch verb {
	case "release", "cancel":
		status, data, err = c.doKeyed(http.MethodPost, path+"/"+verb, body, *key)
		if errors.Is(err, errIdentityTransport) {
			return fmt.Errorf("%w; the outcome is unknown: run aimem reservation recover receipt recovery_%s --task %s --key-digest %s before retrying with the same --key",
				err, verb, *task, store.RequestKeyDigest(*key))
		}
	case "status":
		status, data, err = c.doKeyed(http.MethodGet, path, nil, "")
	case "receipt":
		status, data, err = c.doKeyed(http.MethodGet, path+"/receipts/"+url.PathEscape(operation)+"/"+url.PathEscape(*digest), nil, "")
	}
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return c.hubRefusal(status, data)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, bytes.TrimSpace(data), "", "  ") != nil {
		return errors.New("unreadable answer from the hub")
	}
	fmt.Fprintln(out, c.scrub(pretty.String()))
	return nil
}

// doKeyed sends raw JSON with an optional Idempotency-Key.
func (c *identityClient) doKeyed(method, path string, raw []byte, key string) (int, []byte, error) {
	var rd io.Reader
	if raw != nil {
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", errIdentityTransport, c.scrub(err.Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("%w: reading the answer: %v", errIdentityTransport, c.scrub(err.Error()))
	}
	return resp.StatusCode, data, nil
}
