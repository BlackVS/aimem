package installer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"aimem/internal/adapter"
)

const envToken = "env-admin-token-never-printed"

// runClientHint runs install-hub.sh's client-hint block with the given
// certificate and scheme, the env token set everywhere the installer has
// it: TOKEN, AIMEM_HTTP_TOKEN and the env file.
func runClientHint(t *testing.T, cert, scheme string) (stdout, stderr string) {
	t.Helper()
	envf := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envf, []byte("AIMEM_HTTP_TOKEN="+envToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "set -euo pipefail\nAIMEM_TLS_CERT=" + filepath.ToSlash(cert) + "\nSCHEME=" + scheme +
		"\nHOST=hub.test\nPORT=8440\nHUB_USER=sessiond\nENVF=" + filepath.ToSlash(envf) + "\nTOKEN=" + envToken + "\n" +
		extract(t, "install-hub.sh", "client-hint")
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "AIMEM_HTTP_TOKEN="+envToken)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("client hint: %v\n%s%s", err, out.String(), errb.String())
	}
	if strings.Contains(out.String()+errb.String(), envToken) {
		t.Fatalf("the client hint printed the env token:\n%s%s", out.String(), errb.String())
	}
	return out.String(), errb.String()
}

// selfSigned writes a self-signed certificate for hub.test, as the
// installer's openssl call makes one, and returns its key pair.
func selfSigned(t *testing.T, dir string) (string, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "hub.test"},
		DNSNames: []string{"hub.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "selfsigned.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

var printedPin = regexp.MustCompile(`--pin (sha256-[A-Za-z0-9+/=]+)`)

// A fresh install prints how to give each client its own writer token and
// the self-signed certificate's pin, never the env token; the printed pin
// is the certificate's, and a client configured with it connects.
func TestClientHintPrintsThePinNotTheEnvToken(t *testing.T) {
	shellOnly(t)
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl")
	}
	path, cert := selfSigned(t, t.TempDir())
	out, errOut := runClientHint(t, path, "https")
	for _, want := range []string{"aimem token add <machine>", `aimem hub add <name> https://hub.test:8440 "<writer-token>" --pin sha256-`,
		"admin credential: keep it on this host, never on a client", "--insecure would skip verification"} {
		if !strings.Contains(out, want) {
			t.Errorf("the hint lacks %q:\n%s", want, out)
		}
	}
	if errOut != "" {
		t.Errorf("a pinned https hub warns:\n%s", errOut)
	}
	m := printedPin.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no pin printed:\n%s", out)
	}
	sum := sha256.Sum256(func() []byte { c, _ := x509.ParseCertificate(cert.Certificate[0]); return c.RawSubjectPublicKeyInfo }())
	if want := "sha256-" + base64.StdEncoding.EncodeToString(sum[:]); m[1] != want {
		t.Fatalf("printed pin %s, the certificate's is %s", m[1], want)
	}

	// The printed command's trust connects to the hub serving that
	// certificate, and the same trust refuses another certificate.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()
	hub := &adapter.HubConfig{URL: srv.URL, Pin: m[1]}
	resp, err := hub.Client(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("a client with the printed pin cannot connect: %v", err)
	}
	resp.Body.Close()
	_, other := selfSigned(t, t.TempDir())
	srv2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv2.TLS = &tls.Config{Certificates: []tls.Certificate{other}}
	srv2.StartTLS()
	defer srv2.Close()
	if _, err := (&adapter.HubConfig{URL: srv2.URL, Pin: m[1]}).Client(5 * time.Second).Get(srv2.URL); err == nil {
		t.Fatal("the printed pin accepted another certificate")
	}
}

// A self-signed certificate whose pin cannot be computed gets the command
// to compute it, and a plain-http hub gets the clear-text warning; neither
// prints --insecure as the instruction.
func TestClientHintWarnsWithoutAPinAndOnPlainHTTP(t *testing.T) {
	shellOnly(t)
	missing := filepath.Join(t.TempDir(), "selfsigned.pem")
	out, errOut := runClientHint(t, missing, "https")
	if strings.Contains(out, "--pin sha256-") || strings.Contains(out, `"<writer-token>" --insecure`) {
		t.Errorf("a missing certificate printed a trust flag:\n%s", out)
	}
	if !strings.Contains(errOut, "could not compute the self-signed certificate's pin") || !strings.Contains(errOut, "openssl x509 -in "+filepath.ToSlash(missing)) {
		t.Errorf("no pin warning:\n%s", errOut)
	}
	// An operator's own certificate is trusted through the system roots or
	// --ca-file: no pin and no warning.
	selfCert, _ := selfSigned(t, t.TempDir())
	pemBytes, err := os.ReadFile(selfCert)
	if err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(t.TempDir(), "fullchain.pem")
	if err := os.WriteFile(owned, pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut = runClientHint(t, owned, "https")
	if !strings.Contains(out, `aimem hub add <name> https://hub.test:8440 "<writer-token>"`+"\n") || errOut != "" {
		t.Errorf("a real certificate got a trust flag or a warning:\n%s%s", out, errOut)
	}
	out, errOut = runClientHint(t, "", "http")
	if !strings.Contains(out, `aimem hub add <name> http://hub.test:8440 "<writer-token>"`+"\n") {
		t.Errorf("plain http hint:\n%s", out)
	}
	if !strings.Contains(errOut, "plain http, so every bearer token crosses the network in clear") {
		t.Errorf("no plain-http warning:\n%s", errOut)
	}
}
