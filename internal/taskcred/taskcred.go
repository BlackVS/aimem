// Package taskcred resolves required local task credentials without fallback.
package taskcred

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"aimem/internal/adapter"
	"aimem/internal/ident"
)

type Selection struct {
	Source  string             `json:"source"`
	Repo    string             `json:"repo"`
	Project string             `json:"project"`
	HubName string             `json:"hub"`
	Hub     *adapter.HubConfig `json:"-"`
	Token   string             `json:"-"`
}

type credential struct {
	Version int    `json:"version"`
	Repo    string `json:"repo"`
	Project string `json:"project"`
	Hub     string `json:"hub"`
	URL     string `json:"url"`
	Token   string `json:"token"`
}

func canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func config(dir string) (map[string]json.RawMessage, bool, error) {
	path := filepath.Join(dir, ".aimem.json")
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		fi, err = os.Stat(path)
		if err != nil {
			return nil, false, err
		}
	}
	if !fi.Mode().IsRegular() {
		return nil, false, errors.New("task config must resolve to a regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &fields) != nil || fields == nil {
		return nil, false, errors.New("task tools refused: invalid .aimem.json")
	}
	value, exists := fields["task_credential"]
	if exists {
		var mode string
		if json.Unmarshal(value, &mode) != nil || mode != "local" {
			return nil, false, errors.New("task_credential must be local when configured")
		}
	}
	return fields, exists, nil
}

// LocalRequired lets read-only bootstrap paths honor an explicit requirement
// while retaining their existing behavior when no override is configured.
func LocalRequired(dir string) (bool, error) {
	_, local, err := config(dir)
	return local, err
}

func binding(dir, root string) (*Selection, bool, error) {
	repo, err := canonical(dir)
	if err != nil {
		return nil, false, err
	}
	_, local, err := config(repo)
	if err != nil {
		return nil, false, err
	}
	name, err := ident.ProjectHubNameStrict(repo)
	if err != nil {
		return nil, false, err
	}
	project, err := ident.ProjectID(repo)
	if err != nil {
		return nil, false, err
	}
	name, hub := adapter.ResolveHub(root, name)
	if hub == nil {
		return nil, false, fmt.Errorf("no hub configured for this project (binding %q); configure it with aimem hub add", name)
	}
	return &Selection{Repo: repo, Project: project, HubName: name, Hub: hub}, local, nil
}

// Resolve reads local configuration only. A configured override is mandatory.
func Resolve(dir, root string) (*Selection, error) {
	s, local, err := binding(dir, root)
	if err != nil {
		return nil, err
	}
	if !local {
		s.Source, s.Token = "user-hub", s.Hub.TaskToken
		if s.Token == "" {
			return nil, fmt.Errorf("hub %q has no task credential: aimem hub task-token %s <ordinary-token>", s.HubName, s.HubName)
		}
		return s, nil
	}
	s.Source = "project-local"
	path, err := credentialPath(root, s.Repo, false)
	if err != nil {
		return nil, fmt.Errorf("local task credential required: %w", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("local task credential required: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("local task credential must be a regular file")
	}
	if err := checkPrivate(fi); err != nil {
		return nil, err
	}
	if fi.Size() > 16384 {
		return nil, errors.New("local task credential is too large")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read required local task credential")
	}
	raw, err = unprotect(raw)
	if err != nil {
		return nil, errors.New("cannot decrypt required local task credential")
	}
	var c credential
	if json.Unmarshal(raw, &c) != nil || c.Version != 1 || !validToken(c.Token) {
		return nil, errors.New("malformed local task credential; run aimem task-token set")
	}
	if c.Repo != s.Repo || c.Project != s.Project || c.Hub != s.HubName || c.URL != s.Hub.URL {
		return nil, errors.New("local task credential binding changed; run aimem task-token set")
	}
	s.Token = c.Token
	return s, nil
}

func validToken(token string) bool {
	raw, ok := strings.CutPrefix(token, "aimem_user_")
	if !ok || len(raw) != 64 {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil
}

// Client refuses redirects: a local override is bound to one exact hub URL.
func (s *Selection) Client() *http.Client {
	c := *s.Hub.HTTPClient()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// Validate uses only the selected credential. Legacy per-hub credentials
// retain their existing behavior.
func (s *Selection) Validate(ctx context.Context) error {
	if s.Source != "project-local" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(s.Hub.URL, "/")+"/v1/access/identity?project="+url.QueryEscape(s.Project), nil)
	if err != nil {
		return errors.New("invalid hub URL for local task credential")
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := s.Client().Do(req)
	if err != nil {
		return fmt.Errorf("cannot validate local task credential: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("local task credential rejected (HTTP %d); no fallback", resp.StatusCode)
	}
	var id struct {
		Scope     string `json:"scope"`
		TaskWrite bool   `json:"task_write"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&id) != nil || id.Scope != "project" || !id.TaskWrite {
		return errors.New("local task credential must be project-scoped with current write access to this project; no fallback")
	}
	return nil
}

func credentialPath(root, repo string, create bool) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if create {
		if err := os.MkdirAll(abs, 0700); err != nil {
			return "", err
		}
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(repo, abs)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("task credential state directory must be outside the checkout")
	}
	dir := filepath.Join(abs, "task-credentials")
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("task credential directory must not be a symlink")
	}
	if err := checkPrivate(fi); err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(repo))
	return filepath.Join(dir, hex.EncodeToString(key[:])+".json"), nil
}

// Set validates first, writes the protected secret, then the nonsecret
// requirement. An unsuccessful rotation never truncates the previous secret.
func Set(ctx context.Context, dir, root, token string) error {
	if !validToken(token) {
		return errors.New("expected a complete ordinary project token on stdin")
	}
	s, _, err := binding(dir, root)
	if err != nil {
		return err
	}
	s.Source, s.Token = "project-local", token
	if err := s.Validate(ctx); err != nil {
		return err
	}
	path, err := credentialPath(root, s.Repo, true)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(credential{Version: 1, Repo: s.Repo, Project: s.Project, Hub: s.HubName, URL: s.Hub.URL, Token: token})
	if err != nil {
		return err
	}
	raw, err = protect(raw)
	if err != nil {
		return errors.New("cannot protect local task credential")
	}
	if err := atomicWrite(path, raw, 0600); err != nil {
		return err
	}
	fields, _, err := config(s.Repo)
	if err != nil {
		return err
	}
	fields["task_credential"] = json.RawMessage(`"local"`)
	return writeConfig(s.Repo, fields)
}

func Clear(dir, root string) error {
	repo, err := canonical(dir)
	if err != nil {
		return err
	}
	fields, local, err := config(repo)
	if err != nil {
		return err
	}
	if !local {
		return nil
	}
	path, err := credentialPath(root, repo, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Remove the requirement only on this explicit command, never on read error.
	delete(fields, "task_credential")
	if err := writeConfig(repo, fields); err != nil {
		return err
	}
	if path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func writeConfig(repo string, fields map[string]json.RawMessage) error {
	raw, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if fi, err := os.Lstat(filepath.Join(repo, ".aimem.json")); err == nil {
		mode = fi.Mode().Perm()
	}
	return atomicWrite(filepath.Join(repo, ".aimem.json"), append(raw, '\n'), mode)
}

func atomicWrite(path string, raw []byte, mode os.FileMode) error {
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return errors.New("refusing to replace a non-regular credential/config file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".task-credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
