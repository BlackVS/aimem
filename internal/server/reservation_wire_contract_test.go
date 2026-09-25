package server

// These tests check the C4 design fixtures as a fake consumer. They do not
// implement reservation authorization or exercise an endpoint; C5 and C6 own
// those gates.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readReservationFixture(t *testing.T, name string, out any) {
	t.Helper()
	p := filepath.Join("..", "..", "docs", "fixtures", "reservation-v1", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

func TestReservationV1FixtureAndProposedSurface(t *testing.T) {
	var examples struct {
		Version   int    `json:"version"`
		Status    string `json:"status"`
		Mutations []struct {
			Operation  string                     `json:"operation"`
			MCPTool    string                     `json:"mcp_tool"`
			ActorCase  string                     `json:"actor_case"`
			RequestKey string                     `json:"request_key"`
			Request    map[string]json.RawMessage `json:"request"`
			Response   struct {
				Receipt struct {
					ID           string `json:"id"`
					State        string `json:"state"`
					Operation    string `json:"operation"`
					RequestKey   string `json:"request_key"`
					ActorID      string `json:"actor_id"`
					VerifiedMode string `json:"verified_mode"`
				} `json:"receipt"`
				Reservation struct {
					ID         string `json:"id"`
					Fence      string `json:"fence"`
					Active     bool   `json:"active"`
					HolderMode string `json:"holder_mode"`
					OwnWorkRef string `json:"own_work_ref"`
				} `json:"reservation"`
				TaskRevision int64 `json:"task_revision"`
			} `json:"response"`
		} `json:"mutations"`
		Reconciliation []struct {
			Case          string `json:"case"`
			ReceiptState  string `json:"receipt_state"`
			NextAction    string `json:"next_action"`
			SameReceiptID string `json:"same_receipt_id"`
		} `json:"reconciliation"`
		StatusExample struct {
			State      string `json:"state"`
			Fence      string `json:"fence"`
			OwnWorkRef string `json:"own_work_ref"`
		} `json:"status_example"`
		RefusalEnvelope struct {
			Code          string `json:"code"`
			Message       string `json:"message"`
			ActiveMode    string `json:"active_mode"`
			Retryable     bool   `json:"retryable"`
			NextAction    string `json:"next_action"`
			CorrelationID string `json:"correlation_id"`
		} `json:"refusal_envelope_example"`
		Refusals []struct {
			Case       string `json:"case"`
			Code       string `json:"code"`
			HTTPStatus int    `json:"http_status"`
			Retryable  bool   `json:"retryable"`
			NextAction string `json:"next_action"`
		} `json:"refusals"`
	}
	readReservationFixture(t, "examples.json", &examples)
	if examples.Version != 1 || examples.Status != "proposal_only" {
		t.Fatalf("unversioned or live fixture: %d %q", examples.Version, examples.Status)
	}
	type proposedOperation struct {
		Operation string                     `json:"x-operation"`
		MCPTool   string                     `json:"x-mcp-tool"`
		Responses map[string]json.RawMessage `json:"responses"`
	}
	var spec struct {
		OpenAPI       string                                `json:"openapi"`
		Version       string                                `json:"x-contract-version"`
		SecurityGate  string                                `json:"x-security-gate"`
		Parity        map[string]string                     `json:"x-mcp-parity"`
		RefusalStatus map[string]json.RawMessage            `json:"x-refusal-status"`
		Paths         map[string]map[string]json.RawMessage `json:"paths"`
	}
	readReservationFixture(t, "openapi-proposal.json", &spec)
	if spec.OpenAPI != "3.1.0" || spec.Version != "reservation.v1" {
		t.Fatalf("unexpected proposed OpenAPI version: %q %q", spec.OpenAPI, spec.Version)
	}
	if !strings.Contains(spec.SecurityGate, "C5") || !strings.Contains(spec.SecurityGate, "C6") {
		t.Fatal("proposal must gate route registration on C5 authorization")
	}
	base := "/v1/projects/{project_id}/tasks/{task_id}/reservation"
	operations := []string{"claim", "transfer", "update", "release", "finalize"}
	for _, op := range operations {
		tool := "task_reservation_" + op
		if spec.Parity[op] != tool {
			t.Errorf("%s MCP parity: %q", op, spec.Parity[op])
		}
		var entry proposedOperation
		err := json.Unmarshal(spec.Paths[base+"/"+op]["post"], &entry)
		if err != nil || entry.Operation != op || entry.MCPTool != tool {
			t.Errorf("%s missing from proposed HTTP/MCP map", op)
		}
		var headers struct {
			Parameters []struct {
				Ref string `json:"$ref"`
			} `json:"parameters"`
		}
		_ = json.Unmarshal(spec.Paths[base+"/"+op]["post"], &headers)
		if len(headers.Parameters) != 1 || headers.Parameters[0].Ref != "#/components/parameters/IdempotencyKey" {
			t.Errorf("%s missing required idempotency header", op)
		}
		for _, status := range []string{"200", "400", "401", "403", "404", "409", "503"} {
			if _, ok := entry.Responses[status]; !ok {
				t.Errorf("%s missing status %s", op, status)
			}
		}
	}
	for _, op := range []string{"status", "receipt"} {
		if spec.Parity[op] != "task_reservation_"+op {
			t.Errorf("%s MCP parity missing", op)
		}
	}
	var statusOp, receiptOp proposedOperation
	_ = json.Unmarshal(spec.Paths[base]["get"], &statusOp)
	_ = json.Unmarshal(spec.Paths[base+"/receipts/{operation}/{request_key}"]["get"], &receiptOp)
	if statusOp.Operation != "status" || receiptOp.Operation != "receipt" {
		t.Error("status or receipt proposal missing")
	}
	for path, fields := range spec.Paths {
		var parameters []struct {
			Ref string `json:"$ref"`
		}
		if err := json.Unmarshal(fields["parameters"], &parameters); err != nil {
			t.Errorf("%s has no path/version parameters: %v", path, err)
			continue
		}
		version := false
		for _, param := range parameters {
			version = version || param.Ref == "#/components/parameters/ReservationVersion"
		}
		if !version {
			t.Errorf("%s missing reservation version header", path)
		}
	}
	positive := regexp.MustCompile(`^[1-9][0-9]*$`)
	seen := map[string]bool{}
	byCase := map[string]struct {
		Fence     string
		Revision  int64
		ReceiptID string
	}{}
	for _, ex := range examples.Mutations {
		if !strings.HasPrefix(ex.MCPTool, "task_reservation_") || ex.MCPTool != spec.Parity[ex.Operation] {
			t.Errorf("%s has no HTTP/MCP parity", ex.ActorCase)
		}
		seen[ex.Operation] = true
		byCase[ex.ActorCase] = struct {
			Fence     string
			Revision  int64
			ReceiptID string
		}{ex.Response.Reservation.Fence, ex.Response.TaskRevision, ex.Response.Receipt.ID}
		if ex.RequestKey == "" || ex.Response.Receipt.ID == "" || ex.Response.Receipt.State != "committed" || ex.Response.Receipt.Operation != ex.Operation || ex.Response.Receipt.RequestKey != ex.RequestKey {
			t.Errorf("%s receipt does not bind original operation/key", ex.ActorCase)
		}
		if ex.Response.Receipt.ActorID == "" || (ex.Response.Receipt.VerifiedMode != "personal" && ex.Response.Receipt.VerifiedMode != "team") {
			t.Errorf("%s receipt lacks server-derived actor/context", ex.ActorCase)
		}
		if !positive.MatchString(ex.Response.Reservation.Fence) || len(ex.Request["expected_revision"]) == 0 {
			t.Errorf("%s missing positive fence or expected revision", ex.ActorCase)
		}
		if ex.Operation == "release" || ex.Operation == "finalize" {
			if ex.Response.Reservation.Active || ex.Response.Reservation.ID != "" || ex.Response.Reservation.OwnWorkRef != "" {
				t.Errorf("%s closed outcome exposes an active holder", ex.ActorCase)
			}
		} else if !ex.Response.Reservation.Active || ex.Response.Reservation.ID == "" || ex.Response.Reservation.HolderMode == "" {
			t.Errorf("%s committed hold missing active identity", ex.ActorCase)
		}
		if ex.Operation != "claim" {
			if len(ex.Request["reservation_id"]) == 0 || len(ex.Request["fence"]) == 0 {
				t.Errorf("%s missing current reservation fence", ex.ActorCase)
			}
		}
		if ex.Operation == "update" || ex.Operation == "release" || ex.Operation == "finalize" {
			var content map[string]json.RawMessage
			if err := json.Unmarshal(ex.Request["content"], &content); err != nil {
				t.Errorf("%s invalid full task content: %v", ex.ActorCase, err)
			} else {
				for _, field := range []string{"title", "objective", "acceptance_criteria", "non_goals", "state", "blocker", "dependencies", "candidate_refs", "evidence_refs", "next_action", "archived"} {
					if _, ok := content[field]; !ok {
						t.Errorf("%s content missing %s", ex.ActorCase, field)
					}
				}
			}
		}
		if ex.ActorCase == "independent_worker_external_attempt" {
			var holder struct {
				Mode    string `json:"mode"`
				WorkRef string `json:"work_ref"`
			}
			if err := json.Unmarshal(ex.Request["holder"], &holder); err != nil || holder.Mode != "external" || holder.WorkRef == "" || len(ex.Request["coordination_proof"]) == 0 {
				t.Error("independent worker must claim an external attempt with verified coordination")
			}
		}
		if ex.ActorCase == "successor_coordinator_unaccepted_offer" && len(ex.Request["coordination_proof"]) == 0 {
			t.Error("successor coordinator release lacks unaccepted-offer proof")
		}
		if ex.ActorCase == "verified_reviewing_coordinator" && (len(ex.Request["coordination_proof"]) == 0 || len(ex.Request["terminal_evidence"]) == 0) {
			t.Error("coordinator finalize lacks acceptance proof or delivery evidence")
		}
		for _, forbidden := range []string{"actor_id", "profile_id", "role", "session_handle", "token"} {
			if _, ok := ex.Request[forbidden]; ok {
				t.Errorf("%s request asserts %s", ex.ActorCase, forbidden)
			}
		}
	}
	for _, op := range operations {
		if !seen[op] {
			t.Errorf("no %s success example", op)
		}
	}
	// Fake-consumer ordering for the offer path. Release forks before transfer;
	// accepted work advances the fence at transfer and terminal closure.
	offer := byCase["coordinator_offer"]
	transfer := byCase["accepted_offer_to_worker"]
	update := byCase["worker_submit"]
	finalize := byCase["verified_reviewing_coordinator"]
	release := byCase["successor_coordinator_unaccepted_offer"]
	if offer.Fence != "1" || transfer.Fence != "2" || update.Fence != "2" || finalize.Fence != "3" || release.Fence != "2" ||
		offer.Revision != 3 || transfer.Revision != 3 || update.Revision != 4 || finalize.Revision != 5 || release.Revision != 4 {
		t.Error("offer, accepted-work, terminal and decline fixture ordering disagree")
	}
	if !seen["claim"] || len(examples.Reconciliation) < 3 {
		t.Error("claim/reconciliation examples incomplete")
	}
	if examples.StatusExample.State != "held" || !positive.MatchString(examples.StatusExample.Fence) || examples.StatusExample.OwnWorkRef == "" {
		t.Error("authorized own-status example incomplete")
	}
	if examples.RefusalEnvelope.Code == "" || examples.RefusalEnvelope.Message == "" || examples.RefusalEnvelope.ActiveMode != "team" || !examples.RefusalEnvelope.Retryable || examples.RefusalEnvelope.NextAction == "" || examples.RefusalEnvelope.CorrelationID == "" {
		t.Error("safe refusal envelope example incomplete")
	}
	states := map[string]bool{}
	for _, ex := range examples.Reconciliation {
		states[ex.ReceiptState] = true
		if ex.NextAction == "" {
			t.Errorf("%s missing reconciliation instruction", ex.Case)
		}
		if ex.Case == "identical_replay" && ex.SameReceiptID != byCase["personal_standalone"].ReceiptID {
			t.Error("identical replay does not refer to original committed receipt")
		}
	}
	for _, state := range []string{"committed", "not_committed", "unresolved"} {
		if !states[state] {
			t.Errorf("missing %s reconciliation state", state)
		}
	}
	refusals := map[string]bool{}
	refusalCodes := map[string]bool{}
	for _, ex := range examples.Refusals {
		refusals[ex.Case] = true
		refusalCodes[ex.Code] = true
		if ex.Code == "" || ex.NextAction == "" || strings.Contains(strings.ToLower(ex.NextAction), "personal credential") {
			t.Errorf("%s unsafe or incomplete refusal", ex.Case)
		}
		mapped := false
		var one int
		if err := json.Unmarshal(spec.RefusalStatus[ex.Code], &one); err == nil && one == ex.HTTPStatus {
			mapped = true
		}
		var many []int
		if err := json.Unmarshal(spec.RefusalStatus[ex.Code], &many); err == nil {
			for _, status := range many {
				mapped = mapped || status == ex.HTTPStatus
			}
		}
		if !mapped {
			t.Errorf("%s refusal status %d disagrees with proposed OpenAPI", ex.Case, ex.HTTPStatus)
		}
		if ex.Retryable && ex.HTTPStatus != 503 {
			t.Errorf("%s retryable refusal should use 503", ex.Case)
		}
	}
	for code := range spec.RefusalStatus {
		if !refusalCodes[code] {
			t.Errorf("proposed refusal code %s lacks an example", code)
		}
	}
	for _, name := range []string{"revoked_token", "stale_session", "introspection_outage", "role_downgrade", "dependency_denied", "dependency_outage", "competing_hold", "stale_worker", "changed_replay", "unknown_receipt"} {
		if !refusals[name] {
			t.Errorf("missing %s refusal", name)
		}
	}
	// C6 alone registers the route. A fixture must not accidentally become a
	// production surface through a copied route or embedded OpenAPI entry.
	s, _ := testServer(t)
	for _, route := range s.Routes() {
		if strings.Contains(route.Pattern, "/reservation") {
			t.Errorf("C4 unexpectedly registered route %s", route.Pattern)
		}
	}
	var live struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &live); err != nil {
		t.Fatal(err)
	}
	for path := range live.Paths {
		if strings.Contains(path, "/reservation") {
			t.Errorf("C4 unexpectedly changed live OpenAPI path %s", path)
		}
	}
}
