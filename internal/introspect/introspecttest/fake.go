// Package introspecttest provides a fake aicrew introspection endpoint for
// tests: a TLS server with its own throwaway CA (never httptest's shared
// certificate, so a wrong-CA or wrong-pin case is really wrong) whose answer
// each test scripts.
package introspecttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Path is aicrew's introspection route.
const Path = "/v1/crew/introspect"

// CA is a throwaway certificate authority.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	Pool *x509.CertPool
}

func NewCA(t testing.TB) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake aicrew CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{cert, key, pool}
}

// Leaf issues a server certificate for 127.0.0.1 and localhost, and returns
// it with its SPKI pin in the registered form (sha256-BASE64).
func (ca *CA) Leaf(t testing.TB) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "fake aicrew"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

// Request is the body aimem sends.
type Request struct {
	Version int    `json:"version"`
	HubID   string `json:"hub_id"`
	Nonce   string `json:"nonce"`
	Handle  string `json:"handle"`
}

// Seen is one request as the fake received it.
type Seen struct {
	Header http.Header
	Path   string
	Body   Request
}

// Answer writes the fake's reply to one request.
type Answer func(w http.ResponseWriter, got Request)

// Fake is an aicrew introspection endpoint over TLS.
type Fake struct {
	Srv *httptest.Server
	CA  *CA
	Pin string

	calls     atomic.Int32
	mu        sync.Mutex
	answer    Answer
	last      *Seen
	abandoned chan struct{}
}

// New starts a fake that, until SetAnswer, answers every request as an
// active worker session of service on hub.
func New(t testing.TB, service, hub string) *Fake {
	t.Helper()
	f := &Fake{CA: NewCA(t)}
	var cert tls.Certificate
	cert, f.Pin = f.CA.Leaf(t)
	f.answer = func(w http.ResponseWriter, got Request) { WriteJSON(w, ActiveReply(got.Nonce, service, hub)) }
	f.Srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var got Request
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		f.mu.Lock()
		f.last = &Seen{r.Header.Clone(), r.URL.Path, got}
		answer, abandoned := f.answer, f.abandoned
		f.abandoned = nil
		f.mu.Unlock()
		if abandoned != nil {
			<-r.Context().Done()
			close(abandoned)
			return
		}
		answer(w, got)
	}))
	f.Srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	f.Srv.StartTLS()
	t.Cleanup(f.Srv.Close)
	return f
}

// SetAnswer scripts every later reply.
func (f *Fake) SetAnswer(a Answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = a
}

// StallUntilAbandoned makes the next request hang until the caller gives up
// on it; the returned channel closes at that moment.
func (f *Fake) StallUntilAbandoned() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abandoned = make(chan struct{})
	return f.abandoned
}

// Calls counts the requests that reached the fake.
func (f *Fake) Calls() int { return int(f.calls.Load()) }

// Last returns the most recent request, or nil.
func (f *Fake) Last() *Seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// Endpoint is the full introspection URL, as a peer registers it.
func (f *Fake) Endpoint() string { return f.Srv.URL + Path }

func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ActiveReply is a valid active answer to nonce, expiring in ten minutes.
func ActiveReply(nonce, service, hub string) map[string]any {
	return map[string]any{
		"nonce": nonce, "active": true, "service_id": service, "hub_id": hub,
		"identity": map[string]any{"user_id": "user-1", "token_id": "tok-1"},
		"agent_id": "agent-1", "team_id": "team-1", "role": "worker", "session_id": "sess-1",
		"generation": "4", "handle_expires_at": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}
}

// InactiveReply is the only inactive answer: the nonce and active false.
func InactiveReply(nonce string) map[string]any {
	return map[string]any{"nonce": nonce, "active": false}
}
