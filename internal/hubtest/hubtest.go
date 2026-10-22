// Package hubtest gives tests a hub that individual credentials may reach:
// a TLS test server and the pin of its certificate's public key, as
// `aimem hub add … --pin` records it.
package hubtest

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
)

// NewServer starts a TLS test server for h; the caller closes it.
func NewServer(h http.Handler) *httptest.Server { return httptest.NewTLSServer(h) }

// Pin is srv's pin, sha256-BASE64 of its leaf's public key.
func Pin(srv *httptest.Server) string {
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}
