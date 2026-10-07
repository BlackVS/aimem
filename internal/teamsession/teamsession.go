// Package teamsession binds one local conversation to one verified aicrew
// team session (task E5a; docs/DESIGN-AIFORGE-CONTEXT.md, "Selecting and
// verifying context").
//
// Aicrew launches a team conversation with AIMEM_TEAM_SESSION naming a
// private session file under the aimem state root. `aimem team-session open`
// writes that file after verifying the aimem-scoped handle online; `refresh`
// replaces the handle atomically; `close` removes the file. The file holds
// the handle and the IDs the hub reported, never the individual aimem token
// and never aicrew's own session token. A process pins the binding (hub,
// user, service, team, session) when it starts; only the handle, its expiry,
// the token ID and the generation may change after that.
package teamsession

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
	"regexp"
	"strings"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/filelock"
	"aimem/internal/introspect"
	"aimem/internal/privatefile"
)

const (
	// EnvVar names the session file of the conversation's team context. Its
	// presence puts the process in team mode; it is set by the aicrew
	// launcher for one agent process tree, never host-wide.
	EnvVar = "AIMEM_TEAM_SESSION"
	// Header carries the aimem-scoped handle on every team-mode hub call.
	Header = "X-Aimem-Team-Context"
	// maxFile bounds a session file.
	maxFile = 16 << 10
)

var idShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// File is one session file.
type File struct {
	Version         int       `json:"version"`
	Hub             string    `json:"hub"` // the hub.json name
	URL             string    `json:"url"`
	UserID          string    `json:"user_id"`
	TokenID         string    `json:"token_id"`
	ServiceID       string    `json:"service_id"`
	TeamID          string    `json:"team_id"`
	SessionID       string    `json:"session_id"`
	Generation      string    `json:"generation"`
	Handle          string    `json:"handle"`
	HandleExpiresAt string    `json:"handle_expires_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Binding is what a process pins at start. The hub is pinned by its local
// name and URL: the hub itself checks that the handle belongs to this hub.
type Binding struct {
	Hub, URL, UserID, ServiceID, TeamID, SessionID string
}

func (f File) Binding() Binding {
	return Binding{f.Hub, f.URL, f.UserID, f.ServiceID, f.TeamID, f.SessionID}
}

func (f File) validate() error {
	if f.Version != 1 {
		return fmt.Errorf("unsupported session file version %d", f.Version)
	}
	for name, v := range map[string]string{"user_id": f.UserID, "token_id": f.TokenID, "service_id": f.ServiceID,
		"team_id": f.TeamID, "session_id": f.SessionID} {
		if !idShape.MatchString(v) {
			return fmt.Errorf("session file field %s is missing or malformed", name)
		}
	}
	if f.Hub == "" || !strings.HasPrefix(f.URL, "https://") {
		return errors.New("session file names no https hub")
	}
	if !introspect.ValidHandle(f.Handle) {
		return errors.New("session file holds no valid handle")
	}
	return nil
}

// ValidSessionID reports whether id can name a session.
func ValidSessionID(id string) bool { return idShape.MatchString(id) }

// Active reports whether this process belongs to a team conversation: the
// aicrew launcher set EnvVar for its process tree. Hooks consult it to keep
// knowledge capture and recall off (E5b, D4).
func Active() bool { return os.Getenv(EnvVar) != "" }

// Dir is where aicrew session files live under the state root. It is not the
// legacy team feature's directory (internal/teamstate uses team-sessions/):
// the two kinds of state never share a directory.
func Dir(root string) string { return filepath.Join(root, "aicrew-sessions") }

// Locked runs fn while holding the state root's aicrew-session lock, which
// every lifecycle command takes for its final check-and-write. fn must not
// wait on the network: the lock only makes "the file still holds what I
// verified" and the write that follows one step. The OS releases the lock
// if the process dies, so it is never stale.
func Locked(root string, fn func() error) error {
	if err := os.MkdirAll(Dir(root), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(Dir(root), ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ok, err := filelock.TryLock(lf)
		if err != nil {
			return err
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("another aimem team-session command held the session lock too long; try again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer filelock.Unlock(lf)
	return fn()
}

// PathFor is the session file for sessionID under the state root. The name
// is a digest of the ID, so any valid ID is a safe file name on every OS.
func PathFor(root, sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(Dir(root), hex.EncodeToString(sum[:16])+".json")
}

// Load reads a session file, which must be private (privatefile.Check).
// Errors never quote the file's content.
func Load(path string) (File, error) {
	if err := privatefile.Check(path); err != nil {
		return File{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return File{}, err
	}
	if len(raw) > maxFile {
		return File{}, errors.New("the session file is too large")
	}
	var out File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&out) != nil {
		return File{}, errors.New("the session file is not valid")
	}
	if err := out.validate(); err != nil {
		return File{}, err
	}
	return out, nil
}

// Save writes f to path atomically: a new private file in the same
// directory, then a rename over path. A reader sees the old file or the new
// one, never a partial write.
func Save(path string, f File) error {
	if err := f.validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	var suffix [8]byte
	sum := sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano(), path)))
	copy(suffix[:], sum[:])
	tmp := path + ".tmp-" + hex.EncodeToString(suffix[:])
	out, err := privatefile.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := out.Write(raw); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// On Windows a rename over a file another process has open for an
	// instant (a conversation re-reading it) fails transiently; retry
	// briefly rather than fail a refresh.
	for attempt := 0; ; attempt++ {
		err = os.Rename(tmp, path)
		if err == nil {
			return nil
		}
		if attempt == 20 {
			os.Remove(tmp)
			return err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// HubClient is the team-mode client for a hub: https only, with the
// certificate verified against the hub's ca_file or the system roots.
// A hub configured with insecure (verification skipped) is refused: a
// team-mode request carries the individual bearer and a session handle.
// No proxy, no redirect.
func HubClient(h *adapter.HubConfig) (*http.Client, error) {
	u, err := url.Parse(h.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("team mode needs an https hub URL; the hub's own TLS carries the handle")
	}
	if h.Insecure {
		return nil, errors.New("team mode refuses a hub configured with insecure (certificate verification skipped); " +
			"record its CA or pin instead (aimem hub add NAME URL TOKEN --ca-file PATH or --pin sha256-BASE64)")
	}
	cfg, err := h.TLSConfig() // the hub's ca_file or pin; never skips verification here
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{Proxy: nil, TLSClientConfig: cfg},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// Credential returns the installation's individual credential for the hub:
// its user-scoped task token. A checkout's project-scoped credential is
// never used in team mode.
func Credential(h *adapter.HubConfig) (string, error) {
	if !strings.HasPrefix(h.TaskToken, "aimem_user_") {
		return "", errors.New("the hub has no individual credential (aimem hub task-token); team mode uses only the installation's user-scoped credential")
	}
	return h.TaskToken, nil
}

// Refusal is the hub's refusal envelope for a team-mode request.
type Refusal struct {
	Status     int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	ActiveMode string `json:"active_mode,omitempty"`
	// DeniedAction and ActiveRole name the caller's own refused operation
	// and verified role, when the hub reports them.
	DeniedAction  string `json:"denied_action,omitempty"`
	ActiveRole    string `json:"active_role,omitempty"`
	Retryable     bool   `json:"retryable"`
	NextAction    string `json:"next_action"`
	CorrelationID string `json:"correlation_id"`
}

func (r *Refusal) Error() string {
	msg := r.Code + ": " + r.Message
	if r.NextAction != "" {
		msg += " Next: " + r.NextAction
	}
	if r.CorrelationID != "" {
		msg += " (correlation " + r.CorrelationID + ")"
	}
	return msg
}

// ParseRefusal reads a refusal envelope from a non-2xx body, or nil.
func ParseRefusal(status int, body []byte) *Refusal {
	var r Refusal
	if json.Unmarshal(body, &r) != nil || r.Code == "" {
		return nil
	}
	r.Status = status
	return &r
}

// Report is the hub's team-mode context report.
type Report struct {
	Mode    string `json:"mode"`
	UserID  string `json:"user_id"`
	TokenID string `json:"token_id"`
	Team    struct {
		ServiceID       string `json:"service_id"`
		TeamID          string `json:"team_id"`
		AgentID         string `json:"agent_id"`
		Role            string `json:"role"`
		SessionID       string `json:"session_id"`
		Generation      string `json:"generation"`
		HandleExpiresAt string `json:"handle_expires_at"`
	} `json:"team"`
	Projects  []string `json:"projects"`
	Knowledge string   `json:"knowledge"`
}

// Verify asks the hub for the team-mode context report of handle, with the
// individual credential. It returns the report, or a *Refusal when the hub
// refused, or another error when the hub could not be asked.
func Verify(ctx context.Context, client *http.Client, hubURL, credential, handle string) (Report, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hubURL, "/")+"/v1/access/identity", nil)
	if err != nil {
		return Report{}, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set(Header, handle)
	resp, err := client.Do(req)
	if err != nil {
		return Report{}, nil, fmt.Errorf("hub unreachable: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Report{}, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if r := ParseRefusal(resp.StatusCode, body); r != nil {
			return Report{}, nil, r
		}
		return Report{}, nil, fmt.Errorf("the hub answered %d to the context report", resp.StatusCode)
	}
	var rep Report
	if json.Unmarshal(body, &rep) != nil || rep.Mode != "team" {
		return Report{}, nil, errors.New("the hub did not answer with a team context report")
	}
	return rep, body, nil
}

// Matches reports whether a verified report belongs to the binding.
func (b Binding) Matches(rep Report) bool {
	return rep.UserID == b.UserID && rep.Team.ServiceID == b.ServiceID &&
		rep.Team.TeamID == b.TeamID && rep.Team.SessionID == b.SessionID
}
