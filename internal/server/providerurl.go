package server

// A provider's stored token belongs to the endpoint it was saved with
// (audit A-7): a save that moves base_url must send the token again.

import (
	"strings"

	"aimem/internal/provider"
)

// sameEndpoint compares base URLs the way the callers use them: blank
// means the default, and a trailing slash is trimmed before a path is
// appended.
func sameEndpoint(a, b string) bool {
	norm := func(u string) string {
		if u == "" {
			u = provider.DefaultBaseURL
		}
		return strings.TrimRight(u, "/")
	}
	return norm(a) == norm(b)
}
