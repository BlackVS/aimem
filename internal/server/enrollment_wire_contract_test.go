package server

// A fake consumer of enrollment.v1 (docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md):
// the fixtures, the contract's refusal table and the served code must agree.
// It proves coverage and shape, not authorization; enrollment_routes_test.go
// exercises the routes.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aimem/internal/access"
)

type enrollmentFixture struct {
	SampleSecrets map[string]string `json:"sample_secrets"`
	Bounds        map[string]int    `json:"bounds"`
	Exchanges     []struct {
		Case     string          `json:"case"`
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
		Record   json.RawMessage `json:"record"`
		Audit    json.RawMessage `json:"audit"`
	} `json:"exchanges"`
	Refusals []struct {
		Code   string `json:"code"`
		Status int    `json:"http_status"`
	} `json:"refusals"`
}

func readEnrollmentContract(t *testing.T) (enrollmentFixture, map[string]int, string) {
	t.Helper()
	dir := filepath.Join("..", "..", "docs")
	raw, err := os.ReadFile(filepath.Join(dir, "fixtures", "enrollment-v1", "examples.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx enrollmentFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "fixtures", "enrollment-v1", "openapi-proposal.json"))
	if err != nil {
		t.Fatal(err)
	}
	var proposal struct {
		Status map[string]int `json:"x-refusal-status"`
	}
	if err := json.Unmarshal(raw, &proposal); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(dir, "DESIGN-AIFORGE-ENROLLMENT-WIRE.md"))
	if err != nil {
		t.Fatal(err)
	}
	return fx, proposal.Status, string(text)
}

// The hub's refusal codes agree three ways: the contract's table, the
// examples and the proposed OpenAPI, code by code and status by status. The
// codes the admin routes serve today have those statuses.
func TestEnrollmentContractRefusalsAgree(t *testing.T) {
	fx, proposal, text := readEnrollmentContract(t)
	table := map[string]int{}
	row := regexp.MustCompile("(?m)^\\| (`[a-z_]+`(?:, `[a-z_]+`)*) \\| (\\d+|—) \\| (yes|no) \\| (hub[^|]*|client) \\|")
	for _, m := range row.FindAllStringSubmatch(text, -1) {
		if m[4] == "client" {
			continue
		}
		status := 0
		for _, c := range m[2] {
			status = status*10 + int(c-'0')
		}
		for _, code := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(m[1], -1) {
			table[code[1]] = status
		}
	}
	if len(table) < 10 {
		t.Fatalf("parsed only %d hub refusals from the contract's table", len(table))
	}
	examples := map[string]int{}
	for _, r := range fx.Refusals {
		examples[r.Code] = r.Status
	}
	for name, got := range map[string]map[string]int{"examples.json": examples, "openapi-proposal.json": proposal} {
		for code, status := range table {
			if got[code] != status {
				t.Errorf("%s: %s is %d, the contract says %d", name, code, got[code], status)
			}
		}
		for code := range got {
			if _, ok := table[code]; !ok {
				t.Errorf("%s names %s, which the contract's table does not", name, code)
			}
		}
	}
	for code, ref := range enrollmentRefusals {
		if want, ok := table[code]; !ok || ref.status != want {
			t.Errorf("served %s: status %d, contract %d (listed %v)", code, ref.status, want, ok)
		}
	}
}

// The bounds the hub enforces are the contract's.
func TestEnrollmentContractBounds(t *testing.T) {
	fx, _, _ := readEnrollmentContract(t)
	if got := fx.Bounds["subcode_max_lifetime_seconds"]; got != int(access.EnrollmentMaxLife.Seconds()) {
		t.Errorf("subcode cap: fixture %d s, hub %v", got, access.EnrollmentMaxLife)
	}
	if fx.Bounds["subcode_default_lifetime_seconds"] != 24*3600 {
		t.Errorf("default lifetime: %d", fx.Bounds["subcode_default_lifetime_seconds"])
	}
}

// The sample subcode appears only where the contract permits it: the issue
// answer, the subcode record and a redemption request; never in another
// response or an audit example.
func TestEnrollmentContractSecretLocations(t *testing.T) {
	fx, _, _ := readEnrollmentContract(t)
	subcode := fx.SampleSecrets["subcode"]
	if !regexp.MustCompile(`^aes1_[A-Za-z0-9_-]{43}$`).MatchString(subcode) {
		t.Fatalf("sample subcode shape: %q", subcode)
	}
	seen := false
	for _, ex := range fx.Exchanges {
		if strings.Contains(string(ex.Audit), subcode) {
			t.Errorf("%s: an audit example carries the subcode", ex.Case)
		}
		if strings.Contains(string(ex.Response), subcode) {
			if ex.Case != "issue" {
				t.Errorf("%s: a response carries the subcode", ex.Case)
			}
			seen = true
		}
		for _, secret := range []string{fx.SampleSecrets["bearer"], fx.SampleSecrets["delivery_private_key"]} {
			for _, part := range []json.RawMessage{ex.Request, ex.Response, ex.Record, ex.Audit} {
				if strings.Contains(string(part), secret) {
					t.Errorf("%s carries a secret that never travels in clear", ex.Case)
				}
			}
		}
	}
	if !seen {
		t.Error("the issue exchange does not show the subcode it returns")
	}
}

// The three admin routes are served and described; redemption is not served
// until D1-b.
func TestEnrollmentContractRoutesServed(t *testing.T) {
	s, _ := testServer(t)
	served := map[string]bool{}
	for _, rt := range s.Routes() {
		served[rt.Method+" "+rt.Pattern] = rt.Admin
	}
	for _, p := range []string{enrollIssuePattern, enrollListPattern, enrollRevokePattern} {
		if admin, ok := served[p]; !ok || !admin {
			t.Errorf("%s: served %v, admin %v", p, ok, admin)
		}
	}
	for route := range served {
		if strings.Contains(route, "/redemptions") && strings.Contains(route, "enrollments") {
			t.Errorf("%s is served before D1-b", route)
		}
	}
}
