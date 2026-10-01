package adapter

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The trust every client path uses for one hub: the system roots, the
// hub's CA bundle (ca_file), the SHA-256 pin of its certificate's public
// key (pin), or, in a hub's self-signed phase, none (insecure). A trust that
// cannot be loaded fails every request with an error naming it; it never
// falls back to the system roots or to skipping verification.

// ParsePin decodes a pin written sha256-BASE64.
func ParsePin(pin string) ([]byte, error) {
	b64, ok := strings.CutPrefix(pin, "sha256-")
	want, err := base64.StdEncoding.DecodeString(b64)
	if !ok || err != nil || len(want) != sha256.Size {
		return nil, errors.New("a pin is sha256- followed by the base64 SHA-256 of the hub certificate's public key")
	}
	return want, nil
}

// TLSConfig is the hub's trust. A ca_file or pin is verification and wins
// over insecure; both together are refused.
func (h *HubConfig) TLSConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case h.CAFile != "" && h.Pin != "":
		return nil, errors.New("the hub sets both ca_file and pin: keep one (aimem hub add … --ca-file or --pin)")
	case h.CAFile != "":
		pem, err := os.ReadFile(h.CAFile)
		if err != nil {
			return nil, fmt.Errorf("the hub's ca_file %s cannot be read: %w", h.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the hub's ca_file %s holds no PEM certificate", h.CAFile)
		}
		cfg.RootCAs = pool
	case h.Pin != "":
		want, err := ParsePin(h.Pin)
		if err != nil {
			return nil, fmt.Errorf("the hub's pin: %w", err)
		}
		// The chain check is replaced by the pin, never dropped: the
		// connection is refused unless the leaf's public key hashes to it.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the hub presented no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if subtle.ConstantTimeCompare(sum[:], want) != 1 {
				return errors.New("the hub certificate does not match the hub's pin")
			}
			return nil
		}
	case h.Insecure:
		cfg.InsecureSkipVerify = true
	}
	return cfg, nil
}

// failTransport fails every request with the trust's error.
type failTransport struct{ err error }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// Client is an HTTP client for the hub with the given timeout and the hub's
// trust.
func (h *HubConfig) Client(timeout time.Duration) *http.Client {
	cfg, err := h.TLSConfig()
	if err != nil {
		return &http.Client{Timeout: timeout, Transport: failTransport{err}}
	}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: cfg,
		DialContext:     (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		IdleConnTimeout: 90 * time.Second,
	}}
}

// hubClients caches the push client per trust, so frequent checkpoint
// pushes reuse connections; a trust is read once per process.
var hubClients sync.Map // trustKey -> *http.Client

type trustKey struct {
	insecure bool
	caFile   string
	pin      string
}

// cachedClient is the push client of a trust that loaded; a trust that
// failed is not cached, so a fixed ca_file is picked up by the next call.
func (h *HubConfig) cachedClient() *http.Client {
	k := trustKey{h.Insecure, h.CAFile, h.Pin}
	if c, ok := hubClients.Load(k); ok {
		return c.(*http.Client)
	}
	if _, err := h.TLSConfig(); err != nil {
		return h.Client(5 * time.Second)
	}
	c, _ := hubClients.LoadOrStore(k, h.Client(5*time.Second))
	return c.(*http.Client)
}
