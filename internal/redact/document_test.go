package redact

import (
	"slices"
	"testing"
)

// outcome is what ScanDocument says about RULE in TEXT: "refuse", "warn" or "".
func outcome(t *testing.T, text, rule string) string {
	t.Helper()
	warn, refuse := ScanDocument(text)
	switch {
	case slices.Contains(refuse, rule):
		return "refuse"
	case slices.Contains(warn, rule):
		return "warn"
	}
	return ""
}

func TestScanDocumentBearerHasThreeOutcomes(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		// A template placeholder is no finding.
		{"Authorization: Bearer <token>", ""},
		{"curl -H 'Authorization: Bearer ${AIMEM_TOKEN}'", ""},
		// A short prefix cut with dots shows a form; a long one is most of a secret.
		{fx("Bearer aimem_user_0", "123456789abcdef..."), "refuse"},
		// Lowercase words joined by - or _ with no digit: a stand-in that still warns.
		{"Bearer your-hub-admin-token-here-xx", ""}, // starts with a dummy word
		{"Bearer hub-admin-token-value", "warn"},
		{"Bearer hub_admin_token_value", "warn"},
		// Anything else of 16 or more token characters refuses.
		{fx("Bearer abcdefghij", "klmnop123456"), "refuse"},
		{fx("Bearer Hub-Admin-", "Token-Value"), "refuse"}, // mixed case is no stand-in
		{fx("Bearer hubadmintoken", "valuexx"), "refuse"},  // no separator is no stand-in
	} {
		if got := outcome(t, c.text, "bearer token"); got != c.want {
			t.Errorf("%q: bearer token %q, want %q", c.text, got, c.want)
		}
	}
}

func TestScanDocumentCredentialAssignment(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		// 20 or more characters mixing letters and digits refuse.
		{fx("api_key = a1b2c3d4e5", "f6g7h8i9j0"), "refuse"},
		// A value that only starts with a dummy word is not a dummy: real
		// secrets start with secret_.
		{fx("token: secret_a1b2c3d4", "e5f6g7h8i9"), "refuse"},
		// A letters-only value of that length stays a warning.
		{"password: correcthorsebatterystaple", "warn"},
		{"token=release_pipeline_name_value", "warn"},
		// Below 20 characters: a warning, also when it mixes letters and digits.
		{fx("password = hunter2", "again"), "warn"},
		{fx("api_key = a1b2c3d4e5", "f6g7h8i9j"), "warn"},    // 19
		{fx("api_key = a1b2c3d4e5", "f6g7h8i9j0"), "refuse"}, // 20
		// Placeholders are no finding.
		{"api_key: <your key here>", ""}, // the value regex stops at the space: "<your"
		{"api_key: <api-key>", ""},
		{"password: ${DB_PASSWORD}", ""},
		{"password = %{db_password}", ""},
		{"token = {{ .Token }}", ""},
		{"secret: [REDACTED]", ""},
		{"password: ********", ""},
		{"password: ***PASSWORD***", ""},
		{"token: xxxxxxxxxxxx", ""},
		{"api_key = your_api_key_goes_here", ""},
		{"api_key = example-0000-1111-2222", ""},
		{"password: changeme1234567890abc", ""},
		{"token: placeholder", ""},
		{"password = password", ""},
		{"token = token_token", "warn"}, // a dummy word counts only on equality
	} {
		if got := outcome(t, c.text, "credential assignment"); got != c.want {
			t.Errorf("%q: credential assignment %q, want %q", c.text, got, c.want)
		}
	}
}

func TestScanDocumentDotCutLength(t *testing.T) {
	// Fewer than 12 characters shown before the cut: an illustration.
	if got := outcome(t, fx("api_key = sk_live_ab", "c...")+" (truncated)", "credential assignment"); got != "" {
		t.Errorf("an 11-character prefix: %q, want no finding", got)
	}
	// 12 or more shown: most of a secret.
	if got := outcome(t, fx("api_key = sk_live_ab", "cd1234567890..."), "credential assignment"); got != "refuse" {
		t.Errorf("a 22-character prefix: %q, want refuse", got)
	}
	// 12 shown is a finding; below 20 characters it is a warning.
	if got := outcome(t, "api_key = abcdefghij12…", "credential assignment"); got != "warn" {
		t.Errorf("a 12-character prefix with an ellipsis: %q, want warn", got)
	}
	if got := outcome(t, "api_key = abcdefghij1…", "credential assignment"); got != "" {
		t.Errorf("an 11-character prefix with an ellipsis: %q, want no finding", got)
	}
}

func TestScanDocumentAuthorizationHeaderWarnsUnlessAPlaceholder(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"Authorization: Bearer <token>", ""},
		{"Authorization: <token>", ""},
		{"Authorization: Basic ${CREDENTIALS}", ""},
		{"send `Authorization: Bearer <token>`.", ""}, // a sentence ends after it
		{"(Authorization: <token>).", ""},
		{"Authorization: required", "warn"},
		{"Authorization: Basic dXNlcjpwYXNz", "warn"},
	} {
		if got := outcome(t, c.text, "authorization header"); got != c.want {
			t.Errorf("%q: authorization header %q, want %q", c.text, got, c.want)
		}
	}
}

func TestScanDocumentKeepsTheHighRulesAndReportsNoText(t *testing.T) {
	pat := fx("gh", "p_abcdefghijklmnopqrstuvwxyz123456789")
	warn, refuse := ScanDocument("runbook\n" + pat)
	if !slices.Contains(refuse, "GitHub PAT") {
		t.Fatalf("PAT not refused: warn=%v refuse=%v", warn, refuse)
	}
	for _, s := range append(warn, refuse...) {
		if s == pat || len(s) > 40 {
			t.Fatalf("a finding carries text: %q", s)
		}
	}
	// A rule that refuses is not also listed as a warning.
	warn, refuse = ScanDocument(fx("Bearer abcdefghij", "klmnop123456") + "\nBearer hub-admin-token-value")
	if !slices.Contains(refuse, "bearer token") || slices.Contains(warn, "bearer token") {
		t.Fatalf("bearer listed twice: warn=%v refuse=%v", warn, refuse)
	}
	if warn, refuse := ScanDocument("plain prose about tokens and passwords in general"); len(warn)+len(refuse) != 0 {
		t.Fatalf("clean text flagged: warn=%v refuse=%v", warn, refuse)
	}
}

// ScanAuthored, which records and task text use, is unchanged: the stricter
// set is a document policy only.
func TestScanAuthoredIsUnchangedByTheDocumentPolicy(t *testing.T) {
	for _, text := range []string{
		fx("Bearer abcdefghij", "klmnop123456"),
		fx("api_key = a1b2c3d4e5", "f6g7h8i9j0"),
		fx("token: secret_a1b2c3d4", "e5f6g7h8i9"),
	} {
		warn, refuse := ScanAuthored(text)
		if len(refuse) != 0 || len(warn) == 0 {
			t.Errorf("%q: ScanAuthored warn=%v refuse=%v, want a warning only", text, warn, refuse)
		}
	}
	// A placeholder still warns there: ScanAuthored has no placeholder notion.
	if warn, _ := ScanAuthored("Authorization: Bearer <token>"); len(warn) == 0 {
		t.Errorf("ScanAuthored stopped warning on an authorization header")
	}
}
