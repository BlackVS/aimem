package main

import (
	"strings"
	"testing"
)

// fakeDirectory resolves names the way the hub's directory would.
type fakeDirectory map[string]map[string][]string

func (d fakeDirectory) resolve(kind, name string) (string, error) {
	return lookupName(kind, name, d[kind])
}

var testDirectory = fakeDirectory{
	"user":  {"Alice": {"u1"}, "Bob": {"u2"}, "Twin": {"u3", "u4"}},
	"group": {"reviewers": {"g1"}},
}

// The named forms: entities by --*-name or --*-id, names resolved through
// the directory.
func TestAccessNamedForms(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		method, path string
		output       string
	}{
		{[]string{"list"}, "GET", "/v1/access", ""},
		{[]string{"user-add", "--user-name", "Alice"}, "POST", "/v1/access/users", ""},
		{[]string{"user-set", "--user-id", "u1", "--user-name", "Alice B", "--state", "disabled"}, "PUT", "/v1/access/users/u1", ""},
		{[]string{"group-add", "--group-name", "Developers"}, "POST", "/v1/access/groups", ""},
		{[]string{"member", "add", "--group-name", "reviewers", "--user-name", "Bob"}, "PUT", "/v1/access/groups/g1/members/u2", ""},
		{[]string{"member", "rm", "--group-id", "g9", "--user-id", "u9"}, "DELETE", "/v1/access/groups/g9/members/u9", ""},
		{[]string{"grant", "add", "--project", "example", "--group-name", "reviewers"}, "PUT", "/v1/projects/example/access/group/g1", ""},
		{[]string{"grant", "rm", "-p", "example", "--user-name", "Alice"}, "DELETE", "/v1/projects/example/access/user/u1", ""},
		{[]string{"grant-rm-instance", "--instance", "i1", "--group-id", "g1"}, "DELETE", "/v1/access/grants/i1/group/g1", ""},
		{[]string{"token-issue", "--user-name", "Alice", "--label", "l", "--project", "example", "--expires", "2027-01-01T00:00:00Z", "--output", "t.token"}, "POST", "/v1/access/tokens", "t.token"},
		{[]string{"token-issue", "--user-id", "u1", "--label", "l", "--read-only", "--expires", "2027-01-01T00:00:00Z", "--output", "-"}, "POST", "/v1/access/tokens", "-"},
		{[]string{"token-issue-user", "--user-name", "Bob", "--label", "l", "--expires", "2027-01-01T00:00:00Z", "--output", "-"}, "POST", "/v1/access/tokens", "-"},
		{[]string{"token-revoke", "--token-id", "t1"}, "DELETE", "/v1/access/tokens/t1", ""},
	} {
		call, err := accessRequest(tc.args, testDirectory)
		if err != nil || call.method != tc.method || call.path != tc.path || call.output != tc.output || call.newForm != "" {
			t.Fatalf("%v: %+v %v", tc.args, call, err)
		}
	}
	call, _ := accessRequest([]string{"token-issue-user", "--user-name", "Bob", "--label", "l", "--expires", "2027-01-01T00:00:00Z", "--output", "-"}, testDirectory)
	if b := call.body.(map[string]any); b["user_id"] != "u2" || b["scope"] != "user" {
		t.Fatalf("token-issue-user body: %v", b)
	}
	call, _ = accessRequest([]string{"token-issue", "--user-id", "u1", "--label", "l", "--read-only", "--expires", "2027-01-01T00:00:00Z", "--output", "x"}, testDirectory)
	if b := call.body.(map[string]any); b["project"] != "" || b["scope"] != nil {
		t.Fatalf("read-only body: %v", b)
	}
}

// The selector rule and each command's own flags: exactly one of name or
// ID, an unknown name refused with the names that exist, an ambiguous name
// refused with its IDs, no secret issued without --output.
func TestAccessNamedFormsRefuse(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"grant", "add", "--project", "p", "--user-name", "Carol"}, "unknown user name \"Carol\"; the users are: Alice, Bob, Twin"},
		{[]string{"grant", "add", "--project", "p", "--user-name", "Twin"}, "held by 2 users (u3, u4); name one with --user-id"},
		{[]string{"grant", "add", "--project", "p", "--user-name", "Alice", "--user-id", "u1"}, "exactly one of"},
		{[]string{"grant", "add", "--project", "p", "--user-name", "Alice", "--group-id", "g1"}, "exactly one of"},
		{[]string{"grant", "add", "--user-id", "u1"}, "--project is required"},
		{[]string{"member", "add", "--group-name", "nope", "--user-id", "u1"}, "unknown group name \"nope\"; the groups are: reviewers"},
		{[]string{"member", "add", "--user-id", "u1"}, "exactly one of --group-name, --group-id"},
		{[]string{"user-add", "--user-id", "u1"}, "--user-id does not apply here"},
		{[]string{"user-set", "--user-id", "u1", "--user-name", "A"}, "--state enabled|disabled"},
		{[]string{"token-issue-user", "--user-id", "u1", "--label", "l", "--expires", "2027-01-01T00:00:00Z"}, "needs --label, --expires and --output"},
		{[]string{"token-issue-user", "--group-id", "g1", "--label", "l", "--expires", "2027-01-01T00:00:00Z", "--output", "-"}, "--group-id does not apply here"},
		{[]string{"token-issue", "--user-id", "u1", "--label", "l", "--expires", "2027-01-01T00:00:00Z", "--output", "-"}, "exactly one of --project or --read-only"},
		{[]string{"token-issue", "--user-id", "u1", "--label", "l", "--project", "p", "--read-only", "--expires", "2027-01-01T00:00:00Z", "--output", "-"}, "exactly one of --project or --read-only"},
		{[]string{"token-issue", "--user-id", "u1", "--label", "l", "--project", "p", "--expires", "soon", "--output", "-"}, "RFC 3339"},
		{[]string{"token-revoke"}, "--token-id is required"},
		{[]string{"grant", "promote", "--user-id", "u1"}, "usage: aimem access"},
		{[]string{"rename", "--user-id", "u1"}, "usage: aimem access"},
	} {
		_, err := accessRequest(tc.args, testDirectory)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
}

// The positional forms of earlier releases keep working for one release and
// name the form that replaces them.
func TestAccessLegacyForms(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		method, path string
		newForm      string
	}{
		{[]string{"user-add", "Alice"}, "POST", "/v1/access/users", "aimem access user-add --user-name Alice"},
		{[]string{"user-set", "u", "Alice B", "disabled"}, "PUT", "/v1/access/users/u", "aimem access user-set --user-id u --user-name 'Alice B' --state disabled"},
		{[]string{"group-add", "Developers"}, "POST", "/v1/access/groups", "aimem access group-add --group-name Developers"},
		{[]string{"member", "add", "g", "u"}, "PUT", "/v1/access/groups/g/members/u", "aimem access member add --group-id g --user-id u"},
		{[]string{"grant", "rm", "p", "user", "u"}, "DELETE", "/v1/projects/p/access/user/u", "aimem access grant rm --project p --user-id u"},
		{[]string{"grant-rm-instance", "instance", "group", "g"}, "DELETE", "/v1/access/grants/instance/group/g", "aimem access grant-rm-instance --instance instance --group-id g"},
		{[]string{"token-issue", "u", "agent", "-", "2027-01-01T00:00:00Z"}, "POST", "/v1/access/tokens", "aimem access token-issue --user-id u --label agent --read-only --expires 2027-01-01T00:00:00Z --output FILE"},
		{[]string{"token-issue", "u", "agent", "alpha", "2027-01-01T00:00:00Z"}, "POST", "/v1/access/tokens", "aimem access token-issue --user-id u --label agent --project alpha --expires 2027-01-01T00:00:00Z --output FILE"},
		{[]string{"token-issue-user", "u", "laptop", "2027-01-01T00:00:00Z"}, "POST", "/v1/access/tokens", "aimem access token-issue-user --user-id u --label laptop --expires 2027-01-01T00:00:00Z --output FILE"},
		{[]string{"token-revoke", "t"}, "DELETE", "/v1/access/tokens/t", "aimem access token-revoke --token-id t"},
	} {
		call, err := accessRequest(tc.args, nil)
		if err != nil || call.method != tc.method || call.path != tc.path || call.newForm != tc.newForm || call.output != "" {
			t.Fatalf("%v: %+v %v", tc.args, call, err)
		}
	}
	for _, args := range [][]string{{"token-issue", "u", "agent", "admin"}, {"token-issue", "u", "agent", "p", "invalid"}, {"grant", "add", "p", "admin", "u"}, {"user-set", "u", "Alice", "maybe"}, {"member", "promote", "g", "u"}, {"token-issue-user", "u", "label"}, {"token-issue-user", "u", "label", "bad"}, {"token-issue-user", "u", "label", "2027-01-01T00:00:00Z", "extra"}} {
		if _, err := accessRequest(args, nil); err == nil {
			t.Fatalf("accepted invalid args: %v", args)
		}
	}
}

func TestTokenIssueScopes(t *testing.T) {
	for _, tc := range []struct {
		args           []string
		scope, project string
	}{
		{[]string{"token-issue-user", "u", "token-laptop", "2027-01-01T00:00:00Z"}, "user", ""},
		{[]string{"token-issue", "u", "token-project", "alpha", "2027-01-01T00:00:00Z"}, "", "alpha"},
		{[]string{"token-issue", "u", "token-reader", "-", "2027-01-01T00:00:00Z"}, "", ""},
	} {
		call, err := accessRequest(tc.args, nil)
		if err != nil || call.method != "POST" || call.path != "/v1/access/tokens" {
			t.Fatalf("request: %+v %v", call, err)
		}
		b := call.body.(map[string]any)
		if scope, _ := b["scope"].(string); scope != tc.scope {
			t.Fatalf("scope %q", scope)
		}
		if project, _ := b["project"].(string); project != tc.project {
			t.Fatalf("project %q", project)
		}
	}
}
