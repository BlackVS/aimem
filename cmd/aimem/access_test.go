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
