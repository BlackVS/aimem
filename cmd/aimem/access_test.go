package main

import (
	"testing"
)

func TestAccessCommandContracts(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		method, path string
	}{
		{[]string{"list"}, "GET", "/v1/access"},
		{[]string{"user-add", "Alice"}, "POST", "/v1/access/users"},
		{[]string{"user-set", "u", "Alice", "disabled"}, "PUT", "/v1/access/users/u"},
		{[]string{"group-add", "Developers"}, "POST", "/v1/access/groups"},
		{[]string{"member", "add", "g", "u"}, "PUT", "/v1/access/groups/g/members/u"},
		{[]string{"grant", "rm", "p", "user", "u"}, "DELETE", "/v1/projects/p/access/user/u"},
		{[]string{"grant-rm-instance", "instance", "group", "g"}, "DELETE", "/v1/access/grants/instance/group/g"},
		{[]string{"token-issue", "u", "agent", "-", "2027-01-01T00:00:00Z"}, "POST", "/v1/access/tokens"},
		{[]string{"token-revoke", "t"}, "DELETE", "/v1/access/tokens/t"},
	} {
		method, path, _, err := accessRequest(tc.args)
		if err != nil || method != tc.method || path != tc.path {
			t.Fatalf("%v: %s %s %v", tc.args, method, path, err)
		}
	}
	for _, args := range [][]string{{"token-issue", "u", "agent", "admin"}, {"token-issue", "u", "agent", "p", "invalid"}, {"grant", "add", "p", "admin", "u"}, {"user-set", "u", "Alice", "maybe"}, {"member", "promote", "g", "u"}} {
		if _, _, _, err := accessRequest(args); err == nil {
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
		method, path, body, err := accessRequest(tc.args)
		if err != nil || method != "POST" || path != "/v1/access/tokens" {
			t.Fatalf("request: %s %s %v", method, path, err)
		}
		b := body.(map[string]any)
		if scope, _ := b["scope"].(string); scope != tc.scope {
			t.Fatalf("scope %q", scope)
		}
		if project, _ := b["project"].(string); project != tc.project {
			t.Fatalf("project %q", project)
		}
	}
	for _, args := range [][]string{{"token-issue-user", "u", "label"}, {"token-issue-user", "u", "label", "bad"}, {"token-issue-user", "u", "label", "2027-01-01T00:00:00Z", "extra"}} {
		if _, _, _, err := accessRequest(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
