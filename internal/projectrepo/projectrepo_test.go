package projectrepo

import "testing"

func TestHost(t *testing.T) {
	good := map[string]string{
		"https://github.com/example/example.git":       "github.com",
		"https://Forge.Example.org:8443/team/repo":     "forge.example.org:8443",
		"https://forge.example.org:3000/team/repo":     "forge.example.org:3000",
		"ssh://git@gitlab.example.org:2222/team/x.git": "gitlab.example.org:2222",
		"ssh://gitlab.example.org/team/x.git":          "gitlab.example.org",
		"git@github.com:example/example.git":           "github.com",
		"gitea@forge.example.org:team/repo.git":        "forge.example.org",
	}
	for in, want := range good {
		if got, err := Host(in); err != nil || got != want {
			t.Errorf("Host(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "-https://x/y", "https://x/y z", "http://example.org/x/y", "ftp://example.org/x",
		"https://user:pw@example.org/x", "https://user@example.org/x", "ssh://git:pw@example.org/x",
		"https://example.org/x?y=1", "https://example.org/x#y", "https://example.org", "https://example.org/",
		"https://exa_mple.org/x", "https://example.org:/x", "https://example.org:0/x", "https://example.org:65536/x", "https://example.org:0443/x", "https://example.org:x/x", "example.org:x/y", "git@:x/y", "git@host:", "git@host://x",
		"a:b@host:x", `https://example.org/x\y`, "file:///tmp/x",
	} {
		if h, err := Host(bad); err == nil {
			t.Errorf("Host(%q) accepted as %q", bad, h)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := Repository{Kind: "gitea", URL: "https://forge.example.org/team/repo.git", Access: "read"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Repository{
		{Kind: "", URL: ok.URL, Access: "read"},
		{Kind: "GitHub", URL: ok.URL, Access: "read"},
		{Kind: "gitea", URL: ok.URL, Access: ""},
		{Kind: "gitea", URL: ok.URL, Access: "admin"},
		{Kind: "gitea", URL: "nope", Access: "read"},
	} {
		if r.Validate() == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}
