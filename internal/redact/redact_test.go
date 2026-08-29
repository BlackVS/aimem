package redact

import (
	"strings"
	"testing"
)

// Fixtures are assembled at runtime so no secret-shaped literal exists
// in this file. The redaction regexes operate on the joined strings, so
// the tests exercise exactly the same inputs - but secret scanners
// (GitGuardian, GitHub secret scanning, trufflehog) read source, and a
// convincing fake token in a test file is indistinguishable from a real
// leak to them. Every fixture is split mid-token for that reason;
// keeping them scanner-invisible is part of this file's contract.
func fx(parts ...string) string { return strings.Join(parts, "") }

func TestPatterns(t *testing.T) {
	bearer := fx("Authorization: Bearer abcdefghij", "klmnop123456")
	cases := []string{
		bearer,
		fx("authorization=Basic dXNlcjpw", "YXNz12345678"),
		fx("api_key = sk_", "live_abcdefghij1234567890"),
		fx(`"apiKey": "AI`, `zaSyD-1234567890abcdefghijk"`),
		fx("export PASSWORD=hunter2", "hunter2"),
		fx("token: gh", "p_abcdefghijklmnopqrstuvwxyz123456789"),
		fx("AKIAIOSFODNN7", "EXAMPLE"),
		fx("-----BEGIN RSA PRIVATE ", "KEY-----\nMIIEow\nsecretbody\n-----END RSA PRIVATE ", "KEY-----"),
		fx("key sk-", "proj-abcdefghijklmnopqrstuvwxyz"),
		fx("slack xox", "b-1234567890-abcdefghij"),
		// split inside each "eyJ" marker: a JWT detector keys on that
		// prefix, so no fragment may carry it whole
		fx("jwt ey", "JhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.ey",
			"JzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKK", "F2QT4fwpMeJf36POk6yJV"),
	}
	for _, c := range cases {
		got, _ := String(c, 0)
		if !strings.Contains(got, Placeholder) {
			t.Errorf("not redacted: %q -> %q", c, got)
		}
	}
	// Secret-bearing substrings must not survive.
	got, _ := String(bearer, 0)
	if strings.Contains(got, fx("abcdefghij", "klmnop123456")) {
		t.Errorf("secret survived redaction: %q", got)
	}
}

func TestPlainTextUntouched(t *testing.T) {
	in := "refactored the auth module and ran go test ./... — all green"
	got, trunc := String(in, 0)
	if got != in || trunc {
		t.Errorf("plain text modified: %q -> %q (trunc=%v)", in, got, trunc)
	}
}

func TestEnvValueScrub(t *testing.T) {
	t.Setenv("MY_TEST_SECRET", "supersecretvalue42")
	t.Setenv("SESSIOND_SECRET_ENV", "MY_TEST_SECRET")
	got, _ := String("the value supersecretvalue42 appeared in output", 0)
	if strings.Contains(got, "supersecretvalue42") {
		t.Errorf("env secret survived: %q", got)
	}
}

func TestTruncation(t *testing.T) {
	in := strings.Repeat("a", 1000)
	got, trunc := String(in, 100)
	if !trunc {
		t.Fatal("expected truncation")
	}
	if !strings.Contains(got, "[TRUNCATED sha256=") || !strings.Contains(got, "orig_bytes=1000") {
		t.Errorf("missing truncation marker: %q", got[len(got)-120:])
	}
}

func TestScanAuthored(t *testing.T) {
	// A recognised vendor token shape refuses; the assignment around it
	// only warns.
	warn, refuse := ScanAuthored("runbook\n" + fx("token: gh", "p_abcdefghijklmnopqrstuvwxyz123456789"))
	if len(refuse) != 1 || refuse[0] != "GitHub PAT" {
		t.Fatalf("PAT not refused: warn=%v refuse=%v", warn, refuse)
	}
	if _, refuse := ScanAuthored(fx("-----BEGIN RSA PRIVATE ", "KEY-----\nx\n-----END RSA PRIVATE ", "KEY-----")); len(refuse) != 1 || refuse[0] != "private key block" {
		t.Fatalf("private key not refused: %v", refuse)
	}
	// Soft shapes warn, never refuse: prose mentions credentials all the
	// time and a refusal would block legitimate handoffs.
	warn, refuse = ScanAuthored(fx("password = hunter2", "hunter2again"))
	if len(refuse) != 0 || len(warn) != 1 {
		t.Fatalf("soft shape misclassified: warn=%v refuse=%v", warn, refuse)
	}
	if warn, refuse := ScanAuthored("plain prose about tokens in general"); len(warn)+len(refuse) != 0 {
		t.Fatalf("clean text flagged: warn=%v refuse=%v", warn, refuse)
	}
}
