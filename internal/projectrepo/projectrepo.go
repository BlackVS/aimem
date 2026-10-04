// Package projectrepo is a project's one repository: the hub property that
// says where a project's work happens (docs/DESIGN-AIFORGE-PILOT-1.md §1).
// The hub stores it as given and never fetches it. It holds no secret: the
// host of the clone URL names the credential a member needs, and the
// credential itself stays on the member's machine.
package projectrepo

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Kinds are the forge API dialects a credential is verified against.
var Kinds = []string{"github", "gitea", "gitlab"}

// Accesses are what members need: write (branches and pull requests) or
// read (a project members only consult).
var Accesses = []string{"write", "read"}

// Repository is the stored property. There is no default branch: the
// forge owns it and the coordinator reads it at offer time.
type Repository struct {
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Access string `json:"access"`
	SetAt  string `json:"set_at,omitempty"`
	SetBy  string `json:"set_by,omitempty"`
}

// Change is one audited set or clear, kept in the project's bounded
// repository history with the values before and after.
type Change struct {
	Action string      `json:"action"`
	At     string      `json:"at"`
	By     string      `json:"by"`
	Old    *Repository `json:"old"`
	New    *Repository `json:"new"`
}

// The audited actions of a change.
const (
	ActionSet   = "project.repository.set"
	ActionClear = "project.repository.clear"
)

// Validate checks kind, access and the clone URL's form. An empty access
// is the caller's to default; Validate refuses it.
func (r Repository) Validate() error {
	if !slices.Contains(Kinds, r.Kind) {
		return fmt.Errorf("kind must be one of %s", strings.Join(Kinds, ", "))
	}
	if !slices.Contains(Accesses, r.Access) {
		return fmt.Errorf("access must be one of %s", strings.Join(Accesses, ", "))
	}
	_, err := Host(r.URL)
	return err
}

// Host is the lowercase host of a clone URL, with its port when the URL
// names one (forge.example.org:3000): two forge instances on one host name
// two credentials. The URL forms are https://host[:port]/path,
// ssh://[user@]host[:port]/path or the scp form user@host:path. It refuses
// anything else, and any URL that could carry a secret (a password, or a
// user name on an https URL), a query or a fragment.
func Host(raw string) (string, error) {
	if raw == "" || len(raw) > 512 || strings.ContainsAny(raw, " \t\r\n\"'\\") || strings.HasPrefix(raw, "-") {
		return "", errors.New("url must be a clone URL (at most 512 bytes, no whitespace, quotes or backslashes)")
	}
	if !strings.Contains(raw, "://") {
		// The scp form: user@host:path, where the path does not start with
		// a slash pair (that would be a scheme-less URL, not scp).
		at := strings.IndexByte(raw, '@')
		colon := strings.IndexByte(raw, ':')
		if at <= 0 || colon < at+2 || colon == len(raw)-1 || strings.HasPrefix(raw[colon+1:], "//") {
			return "", errors.New("url must be https://host/path, ssh://host/path or user@host:path")
		}
		host := raw[at+1 : colon]
		if strings.ContainsAny(raw[:at], ":/") || !validHost(host) {
			return "", errors.New("url has an invalid user or host")
		}
		return strings.ToLower(host), nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("url: %w", err)
	}
	switch u.Scheme {
	case "https":
		if u.User != nil {
			return "", errors.New("an https url must not carry a user name or password; the credential stays on the member's machine")
		}
	case "ssh":
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", errors.New("url must not carry a password")
		}
	default:
		return "", errors.New("url scheme must be https or ssh")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("url must not carry a query or fragment")
	}
	if u.Path == "" || u.Path == "/" {
		return "", errors.New("url must name a repository path")
	}
	if !validHost(u.Hostname()) || !validPort(u.Port(), u.Host) {
		return "", errors.New("url has an invalid host")
	}
	return strings.ToLower(u.Host), nil
}

func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
		default:
			return false
		}
	}
	return !strings.HasPrefix(h, "-") && !strings.HasPrefix(h, ".")
}

// validPort accepts no port, or 1 to 65535 in decimal without a leading
// zero; host is the URL's host[:port] as given, so "host:" is refused.
func validPort(port, host string) bool {
	if port == "" {
		return !strings.HasSuffix(host, ":")
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == port
}
