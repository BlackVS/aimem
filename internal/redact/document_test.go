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
		{"Authorization: Bearer MY_SECRET_TOKEN_HERE", "warn"},
		{"Bearer HUB-ADMIN-TOKEN-VALUE", "warn"},
		// Anything else of 16 or more token characters refuses.
		{fx("Bearer abcdefghij", "klmnop123456"), "refuse"},
		{fx("Bearer Hub-Admin-", "Token-Value"), "refuse"}, // mixed case is no stand-in
		{fx("Bearer hubadmintoken", "valuexx"), "refuse"},  // no separator is no stand-in
		// A wrapper around a generated value is that value (external review of 179ef84).
		{fx("Authorization: Bearer <a1b2c3d4e5", "f6g7h8i9j0k1>"), "refuse"},
		{fx("Bearer [a1b2c3d4e5", "f6g7h8i9j0k1]"), "refuse"},
		{fx("Bearer *a1b2c3d4e5", "f6g7h8i9j0k1*"), "refuse"},
		{fx("Bearer ${a1b2c3d4e5", "f6g7h8i9j0k1}"), "refuse"},
		{"Bearer <HUB_ADMIN_TOKEN>", ""},
		{"Bearer *hub-admin-token-value*", ""},
		// Prose and short words are no token, as before.
		{"the bearer of bad news", ""},
		{"Bearer abc123", ""},
		{"Bearer abcdefghijklmno.", "warn"}, // the soft shape, cut short by the full stop
	} {
		if got := outcome(t, c.text, "bearer token"); got != c.want {
			t.Errorf("%q: bearer token %q, want %q", c.text, got, c.want)
		}
	}
}

// TestScanDocumentInputsAgainstEveryRule judges each input by all three soft
// rules at once, so a fix to one rule cannot leave a hole beside it unseen
// (the external review of 179ef84 and the architect's review of 6b7c9a2 each
// found a form that only one rule's tests would have caught).
func TestScanDocumentInputsAgainstEveryRule(t *testing.T) {
	v := fx("Zx9Kq2Lm8Pv4", "Rt6Yw1Bn3Qd5") // 24 characters, mixed
	for _, c := range []struct{ text, auth, bearer, cred string }{
		// A token pasted with the text that follows it in a request or response.
		{`{"Authorization": "Bearer ` + v + `"}`, "", "refuse", ""},
		{`["Authorization: Bearer ` + v + `"]`, "warn", "refuse", ""},
		{`<code>Authorization: Bearer ` + v + `</code>`, "warn", "refuse", ""},
		{"Authorization: Bearer " + v + "|next", "warn", "refuse", ""},
		{"Bearer " + v + "&x=1", "", "refuse", ""},
		{"Bearer " + v + ".", "", "refuse", ""},
		// Wrapped: a generated value is the value, a name is a placeholder.
		{"Authorization: Bearer <" + v + ">", "warn", "refuse", ""},
		{"Authorization: Bearer *" + v + "*", "warn", "refuse", ""},
		{`{"Authorization": "Bearer <token>"}`, "", "", ""},
		// A long template name with the syntax that follows it (external
		// review of f46b26e), after either rule, is still a placeholder.
		{`{"Authorization": "Bearer ${CONFIG_VALUE_123}"}`, "", "", ""},
		{"Authorization: Bearer $CONFIG_VALUE_123|next", "warn", "", ""},
		{"Bearer <CONFIG_VALUE_123>.", "", "", ""},
		{"Bearer ${CONFIG_VALUE_123}.", "", "", ""},
		{"curl https://h.example/v1?api_key=${CONFIG_VALUE_123}&x=1", "", "", ""},
		{`{"api_key": "{{ .ConfigValue123 }}"}`, "", "", ""},
		// A wrapped generated value with syntax after it, and a value glued
		// onto a short placeholder, are values.
		{`{"Authorization": "Bearer <` + v + `>"}`, "", "refuse", ""},
		{"api_key=<" + v + ">&x=1", "", "", "refuse"},
		{"Bearer <x>" + v, "", "refuse", ""},
		{"api_key=<x>" + v, "", "", "refuse"},
		{`curl -H "Authorization: Bearer ${TOKEN}"`, "", "", ""},
		{"Authorization: Bearer <HUB_ADMIN_TOKEN>", "", "", ""},
		// Stand-ins and prose.
		{"Authorization: Bearer hub-admin-token-value", "warn", "warn", ""},
		{`"Bearer HUB_ADMIN_TOKEN_VALUE"}`, "", "warn", ""},
		{"the bearer of bad news", "", "", ""},
		// The credential rule beside the bearer rule.
		{"token: " + v, "", "", "refuse"},
		{"api_key = <" + v + ">", "", "", "refuse"},
		{`{"token": "` + v + `"}`, "", "", "refuse"},
		{"api_key = <api-key>", "", "", ""},
	} {
		for _, r := range []struct{ rule, want string }{
			{"authorization header", c.auth}, {"bearer token", c.bearer}, {"credential assignment", c.cred},
		} {
			if got := outcome(t, c.text, r.rule); got != r.want {
				t.Errorf("%q: %s %q, want %q", c.text, r.rule, got, r.want)
			}
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
		// A filesystem path names where a secret lives: a warning, never a
		// refusal. A URL is no path and keeps the rule.
		{"secret_key: /home/user1/keys/app2/secret.pem", "warn"},
		{"api_key = ./keys/app2/deploy-2024.pem", "warn"},
		{"api_key = ../keys/app2/id_ed25519", "warn"},
		{"api_key = ~/.ssh/id_ed25519", "warn"},
		{`api_key = C:\keys\app2\id_ed25519.pem`, "warn"},
		{"api_key = D:/keys/app2/service.json", "warn"},
		{fx("api_key = https://example.test/k/a1b2c3", "d4e5f6g7h8"), "refuse"},
		// Base64 may start with a slash: a generated run inside refuses.
		{fx("api_key = /9j4AbCd5Ef", "6Gh7Ij8Kl9Mn0"), "refuse"},
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
	// A cut is a form only when it ends the value: anything after the dots
	// is more of the value.
	for _, c := range []struct{ text, rule, want string }{
		{fx("token: ghp_Ab", "12..."), "credential assignment", ""},
		{fx("token: ghp_Ab", "12...") + ".", "credential assignment", ""},
		{"api_key = sk-...wxyz", "credential assignment", "warn"},
		{fx("token: ab...Zx9Kq2Lm", "8Pv4Rt6Yw1Bn3Qd5"), "credential assignment", "refuse"},
		{fx("api_key = sk...a1b2c3d4e5", "f6g7h8i9j0k1"), "credential assignment", "refuse"},
		{fx("Bearer sk...a1b2c3d4e5", "f6g7h8i9j0k1"), "bearer token", "refuse"},
	} {
		if got := outcome(t, c.text, c.rule); got != c.want {
			t.Errorf("%q: %s %q, want %q", c.text, c.rule, got, c.want)
		}
	}
}

// A wrapper names a value only when what it wraps is no generated-looking
// value: a secret in emphasis or brackets is still the secret.
func TestScanDocumentWrappedValues(t *testing.T) {
	value := fx("a1b2c3d4e5", "f6g7h8i9j0k1")
	for _, c := range []struct{ text, want string }{
		{"api_key = *" + value + "*", "refuse"},
		{"api_key = [" + value + "]", "refuse"},
		{"api_key = {" + value + "}", "refuse"},
		{"api_key = <" + value + ">", "refuse"},
		{"api_key = ${" + value + "}", "refuse"},
		// Names inside wrappers stay placeholders, digits included.
		{"api_key = <api-key-v2>", ""},
		{"api_key = <key2024>", ""}, // a short mixed run is a name
		{"password = ${DB_PASSWORD_2}", ""},
		{"token = [REDACTED]", ""},
		{"password = ***PASSWORD***", ""},
	} {
		if got := outcome(t, c.text, "credential assignment"); got != c.want {
			t.Errorf("%q: credential assignment %q, want %q", c.text, got, c.want)
		}
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
