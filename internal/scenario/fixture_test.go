//go:build scenario

package scenario

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"aimem/internal/process"
)

// bin is the aimem binary under test, built once by TestMain.
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aimem-sc-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	bin = filepath.Join(dir, "aimem"+exe)
	b := exec.Command("go", "build", "-o", bin, "aimem/cmd/aimem")
	b.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := b.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build aimem: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const (
	// processRepo is the URL the cards print; the git fixture answers it.
	processRepo = "https://github.com/BlackVS/aimem.git"
	processTag  = "v0.0.0-scenario"
	skillsVer   = "0.0.0-scenario"
	// stopWait bounds a serve's exit after the stop signal.
	stopWait = 10 * time.Second
	// readyWait bounds a serve's start until it answers.
	readyWait = 30 * time.Second
)

// world is one scenario's machines: a hub and a workstation, each with its
// own state root and home, and the fixtures the cards name through vars.
type world struct {
	t    *testing.T
	root string
	hub  *machine
	ws   *machine
	vars map[string]string
}

// machine is one side of a scenario: a state root, the environment every
// command on that side gets, and its running `aimem serve`.
type machine struct {
	name  string
	state string
	dir   string // where its commands run
	env   []string
	serve *exec.Cmd
	log   string
}

// newWorld starts a hub (TLS on loopback) and a workstation and stops both
// when the test ends. The root is a short directory under the system
// temporary directory, not t.TempDir(): the service socket lives in each
// state root and a Unix socket path is limited to 103 bytes, which
// t.TempDir()'s test-named directories exceed on Windows.
func newWorld(t *testing.T) *world {
	t.Helper()
	root, err := os.MkdirTemp("", "aimem-sc")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, root: root}
	t.Cleanup(func() { os.RemoveAll(root) })

	fx := filepath.Join(root, "fx")
	if err := os.MkdirAll(fx, 0o700); err != nil {
		t.Fatal(err)
	}
	caFile, certFile, keyFile := writeCerts(t, fx)
	bare := gitFixture(t, fx)
	archive := skillsArchive(t, fx)
	adminToken := randomHex(t)
	port := freePort(t)

	// git inherits the environment: map the documented URL to the bare
	// fixture, so `process select` reads it without the network while the
	// card keeps printing the real URL.
	gitEnv := []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url." + fileURL(bare) + ".insteadOf",
		"GIT_CONFIG_VALUE_0=" + processRepo,
	}
	w.hub = w.newMachine("hub", gitEnv)
	w.ws = w.newMachine("workstation", gitEnv)
	// The workstation's commands run in the project's checkout, as a
	// person runs them from the project directory.
	w.ws.dir = filepath.Join(root, "project")
	if err := os.MkdirAll(w.ws.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w.vars = map[string]string{
		"hub-url":            fmt.Sprintf("https://127.0.0.1:%d", port),
		"ca-file":            caFile,
		"token-file":         filepath.Join(fx, "writer.token"),
		"task-token-file":    filepath.Join(fx, "task.token"),
		"project-token-file": filepath.Join(fx, "project.token"),
		"project":            "scenario-project",
		"project-2":          "scenario-project-2",
		"hub-name":           "team",
		"machine":            "scenario-machine",
		"user":               "scenario-agent",
		"repo-url":           "https://github.com/example/scenario-project.git",
		"expires":            time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
		"tag":                processTag,
		"skills-archive":     archive,
		"skills-version":     skillsVer,
	}
	w.start(w.hub, "AIMEM_HTTP_LISTEN=127.0.0.1:"+fmt.Sprint(port), "AIMEM_HTTP_TOKEN="+adminToken,
		"AIMEM_TLS_CERT="+certFile, "AIMEM_TLS_KEY="+keyFile)
	w.waitHTTPS(caFile, w.vars["hub-url"], adminToken)
	w.writerToken(w.vars["token-file"])
	w.start(w.ws)
	return w
}

// newMachine prepares one side's state root and home and its environment: the
// inherited one without any AIMEM_ setting, XDG_RUNTIME_DIR or home, so no
// command reaches the developer's real service or files.
func (w *world) newMachine(name string, extra []string) *machine {
	m := &machine{name: name, state: filepath.Join(w.root, name)}
	m.dir = m.state
	home := filepath.Join(m.state, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		w.t.Fatal(err)
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(strings.ToUpper(k), "AIMEM_"), strings.HasPrefix(strings.ToUpper(k), "GIT_CONFIG"):
		case strings.EqualFold(k, "XDG_RUNTIME_DIR"), strings.EqualFold(k, "HOME"), strings.EqualFold(k, "USERPROFILE"):
		default:
			m.env = append(m.env, kv)
		}
	}
	m.env = append(m.env, "AIMEM_STATE_DIR="+m.state, "HOME="+home, "USERPROFILE="+home)
	m.env = append(m.env, extra...)
	return m
}

// start runs `aimem serve` on m with serveEnv added, waits until `aimem
// health` answers on that side, and registers the stop.
func (w *world) start(m *machine, serveEnv ...string) {
	t := w.t
	m.log = filepath.Join(w.root, m.name+".serve.log")
	lf, err := os.Create(m.log)
	if err != nil {
		t.Fatal(err)
	}
	m.serve = exec.Command(bin, "serve")
	m.serve.Env = append(append([]string{}, m.env...), serveEnv...)
	m.serve.Stdout, m.serve.Stderr = lf, lf
	if err := m.serve.Start(); err != nil {
		lf.Close()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- m.serve.Wait(); lf.Close() }()
	t.Cleanup(func() { w.stop(m, exited) })

	deadline := time.Now().Add(readyWait)
	for {
		select {
		case err := <-exited:
			exited <- err
			t.Fatalf("%s: aimem serve exited before it answered (%v):\n%s", m.name, err, w.logTail(m))
		default:
		}
		if _, err := w.exec(m, "health"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: aimem health did not answer within %s:\n%s", m.name, readyWait, w.logTail(m))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop ends m's serve: an interrupt where the platform has one (Windows
// has none to send to another process, so it is killed), then a kill and a
// test failure if it does not exit within stopWait.
func (w *world) stop(m *machine, exited chan error) {
	select {
	case <-exited:
		return
	default:
	}
	if runtime.GOOS == "windows" {
		m.serve.Process.Kill()
	} else {
		m.serve.Process.Signal(os.Interrupt)
	}
	select {
	case <-exited:
	case <-time.After(stopWait):
		m.serve.Process.Kill()
		<-exited
		w.t.Errorf("%s: aimem serve did not exit within %s of the stop:\n%s", m.name, stopWait, w.logTail(m))
	}
}

// waitHTTPS waits until the hub's TCP listener answers /v1/health over TLS
// verified against the fixture CA.
func (w *world) waitHTTPS(caFile, url, token string) {
	t := w.t
	c, err := hubClient(caFile, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(readyWait)
	for {
		req, _ := http.NewRequest("GET", url+"/v1/health", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("status %s", resp.Status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub: %s/v1/health did not answer within %s (%v):\n%s", url, readyWait, err, w.logTail(w.hub))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// exec runs the binary on m with args and returns its combined output.
func (w *world) exec(m *machine, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = m.env
	cmd.Dir = m.dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (w *world) logTail(m *machine) string {
	b, _ := os.ReadFile(m.log)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

// run runs every step of the card at path and returns the first failure,
// which names the card file, the step and the card line.
func (w *world) run(path string) error {
	c, err := ParseCard(path, w.vars)
	if err != nil {
		return err
	}
	for _, st := range c.Steps {
		m := w.hub
		if st.Side == "workstation" {
			m = w.ws
		}
		var out string
		var err error
		if st.HTTP != nil {
			out, err = w.send(st.HTTP)
		} else {
			out, err = w.exec(m, st.Args[1:]...)
		}
		if err != nil {
			return fmt.Errorf("%s:%d: step %d (%s): %v:\n%s", c.Path, st.Line, st.N, st.Title, err, out)
		}
		for _, e := range st.Expect {
			if !strings.Contains(out, e.Text) {
				return fmt.Errorf("%s:%d: step %d (%s): expected line %q not in output:\n%s", c.Path, e.Line, st.N, st.Title, e.Text, out)
			}
		}
	}
	return nil
}

// send sends an http step's request to the hub over TLS verified against
// the fixture CA, with the token from its Token-File as the bearer. The
// output is the status line, then the response body; a status other than
// 2xx is an error, as a non-zero exit is for a command.
func (w *world) send(r *Request) (string, error) {
	tok, err := os.ReadFile(r.TokenFile)
	if err != nil {
		return "", err
	}
	c, err := hubClient(w.vars["ca-file"], 30*time.Second)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(r.Method, w.vars["hub-url"]+r.Path, strings.NewReader(r.Body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	if r.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range r.Header {
		req.Header.Set(h[0], h[1])
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	out := resp.Status + "\n" + string(body)
	if err != nil {
		return out, err
	}
	if resp.StatusCode/100 != 2 {
		return out, fmt.Errorf("the hub answered %s", resp.Status)
	}
	return out, nil
}

// hubClient is an HTTPS client that trusts only the CA in caFile and keeps
// no idle connection to the hub after a request.
func hubClient(caFile string, timeout time.Duration) (*http.Client, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s holds no PEM certificate", caFile)
	}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, DisableKeepAlives: true}}, nil
}

// writeCerts writes a CA and a server certificate for 127.0.0.1 and
// localhost signed by it.
func writeCerts(t *testing.T, dir string) (caFile, certFile, keyFile string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aimem scenario CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	caFile, certFile, keyFile = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "hub.pem"), filepath.Join(dir, "hub.key")
	for f, blk := range map[string]*pem.Block{
		caFile:   {Type: "CERTIFICATE", Bytes: caDER},
		certFile: {Type: "CERTIFICATE", Bytes: leafDER},
		keyFile:  {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(f, pem.EncodeToMemory(blk), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return caFile, certFile, keyFile
}

// gitFixture makes a bare repository holding this checkout's processes/
// directory, tagged processTag, and returns its path.
func gitFixture(t *testing.T, dir string) string {
	t.Helper()
	work, bare := filepath.Join(dir, "process-work"), filepath.Join(dir, "process.git")
	if err := os.CopyFS(filepath.Join(work, "processes"), os.DirFS("../../processes")); err != nil {
		t.Fatal(err)
	}
	git := func(d string, args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=scenario", "-c", "user.email=scenario@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "init.defaultBranch=master"}, args...)...)
		cmd.Dir = d
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git(dir, "init", "-q", "--bare", bare)
	git(work, "init", "-q")
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "process set for the scenarios")
	git(work, "tag", processTag)
	git(work, "push", "-q", bare, "master", processTag)
	return bare
}

// skillsArchive writes a skills release archive (aiskills-V/VERSION and
// aiskills-V/skills/<name>/...) holding the skill the process set needs,
// with its SHA256SUMS beside it, and returns the archive's path.
func skillsArchive(t *testing.T, dir string) string {
	t.Helper()
	top := "aiskills-" + skillsVer + "/"
	files := map[string][]byte{
		top + "VERSION":                        []byte(skillsVer + "\n"),
		top + "skills/oh-code-review/SKILL.md": []byte("---\nname: oh-code-review\ndescription: scenario stand-in\n---\nReview.\n"),
	}
	var buf bytes.Buffer
	if err := process.WriteArchive(&buf, &process.Bundle{Files: files}); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "aiskills-"+skillsVer+".tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  "+filepath.Base(archive)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return archive
}

// writerToken issues a writer token on the hub, as an administrator does
// with `aimem token add`, and stores its secret owner-only in path: the
// admin token the hub was started with stays inside the fixture.
func (w *world) writerToken(path string) {
	t := w.t
	out, err := w.exec(w.hub, "token", "add", "scenario-workstation")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err != nil || len(lines) < 2 || !strings.Contains(lines[0], "created") {
		t.Fatalf("hub: token add: %v\n%s", err, out)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(lines[1])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ownerOnly(t, path)
}

// ownerOnly restricts a secret file to the current account, as the CLI
// requires of a token file: mode 0600 does that on Unix; on Windows the
// file inherits the temporary directory's ACL, so it gets the icacls
// command the documentation gives.
func ownerOnly(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		return
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", path, "/inheritance:r", "/grant:r", u.Username+":F").CombinedOutput(); err != nil {
		t.Fatalf("icacls %s: %v\n%s", path, err, out)
	}
}

// freePort returns a loopback port that was free a moment ago. A hub that
// then cannot bind exits, and start fails with its log.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// fileURL is the file:// URL of a local path, with forward slashes on
// Windows (file:///C:/...).
func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}
