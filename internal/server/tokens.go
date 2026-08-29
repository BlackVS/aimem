package server

// Named hub tokens (DESIGN-hub-sync): <state-root>/tokens.json holds
// bearer secrets HASHED (sha256 hex) with a role each, so revoking one
// machine is deleting one line instead of re-keying the fleet, and doc
// writes become attributable to an authenticated name. The legacy
// AIMEM_HTTP_TOKEN keeps working as an implicit admin named "env" —
// zero-migration back-compat. Host-local like providers.json: never
// synced, never served.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// TokenEntry is one named token. Only the digest is ever on disk; the
// secret exists once, at creation, on the operator's terminal.
type TokenEntry struct {
	Name   string `json:"name"`
	Role   string `json:"role"`   // "writer" | "admin"
	SHA256 string `json:"sha256"` // hex sha256 of the secret
}

// TokensPath is the registry file location for a state root.
func TokensPath(root string) string { return filepath.Join(root, "tokens.json") }

// LoadTokens reads the token registry; a missing or unreadable file is
// an empty registry (the env token still works), never an error that
// could lock the operator out.
func LoadTokens(root string) []TokenEntry {
	raw, err := os.ReadFile(TokensPath(root))
	if err != nil {
		return nil
	}
	var f struct {
		Tokens []TokenEntry `json:"tokens"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	return f.Tokens
}

// SaveTokens writes the registry atomically, mode 0600.
func SaveTokens(root string, list []TokenEntry) error {
	b, err := json.MarshalIndent(struct {
		Tokens []TokenEntry `json:"tokens"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	p := TokensPath(root)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// NewTokenSecret generates a fresh secret and its stored digest.
func NewTokenSecret() (secret, digest string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	secret = hex.EncodeToString(b[:])
	return secret, HashToken(secret), nil
}

// HashToken is the digest stored and compared: sha256 hex.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Identity is who authenticated on the TCP listener. Requests over the
// local unix socket carry none and are trusted as the local operator.
type Identity struct {
	Name string
	Role string // "writer" | "admin"
}

type identityKey struct{}

func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom reports the authenticated identity, if the request came
// through the TCP auth wrapper.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// authenticate resolves a presented bearer secret to an identity:
// the env token (implicit admin, name "env") or a named entry from
// tokens.json. Every comparison is constant-time over digests of
// identical length, so timing reveals nothing about which token — or
// how much of one — matched.
func (s *Server) authenticate(envToken, presented string) (Identity, bool) {
	presentedDigest := []byte(HashToken(presented))
	ok := false
	id := Identity{}
	if envToken != "" {
		if subtle.ConstantTimeCompare(presentedDigest, []byte(HashToken(envToken))) == 1 {
			id, ok = Identity{Name: "env", Role: "admin"}, true
		}
	}
	for _, t := range LoadTokens(s.reg.Root()) {
		if len(t.SHA256) != sha256.Size*2 {
			continue
		}
		if subtle.ConstantTimeCompare(presentedDigest, []byte(t.SHA256)) == 1 && !ok {
			role := t.Role
			if role != "admin" {
				role = "writer"
			}
			id, ok = Identity{Name: t.Name, Role: role}, true
		}
	}
	return id, ok
}

// requireAdmin gates an admin-only route: a named writer token is
// refused; the env token, admin tokens, and the local unix socket (no
// identity) pass.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id, ok := IdentityFrom(r.Context()); ok && id.Role != "admin" {
			s.fail(w, http.StatusForbidden,
				fmt.Errorf("token %q has role %q; this route needs an admin token", id.Name, id.Role))
			return
		}
		next(w, r)
	}
}
