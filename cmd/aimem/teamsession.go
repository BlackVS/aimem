package main

// Client-side commands of an aicrew team session (task E5a). They are run by
// the aicrew client on the agent's machine, never by a model:
//   - `aimem identity proof` asks the hub for a single-use proof receipt with
//     the installation's individual credential and writes it only into a pipe
//     the aicrew client reads (decision D3): never onto a terminal, into a
//     file, a model's context or a transcript;
//   - `aimem team-session open|refresh|close|status` keeps the private
//     session file that binds one conversation to one verified team session.
//     The aimem-scoped handle arrives on stdin only (D2), never in argv.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/introspect"
	"aimem/internal/teamsession"
)

const proofUsage = `usage: aimem identity proof --peer SERVICE --hub-id HUB_ID --challenge ID [--hub-name NAME]

Client-only: the aicrew client runs this at session entry, never a model or a
person. It asks the hub for a single-use proof receipt (valid 60 s) with this
installation's individual credential and writes the receipt to stdout only
when stdout is a pipe the client reads. A terminal, a file or any other
destination is refused before anything is requested, so the receipt never
reaches a screen, a model's context or a transcript.`

const teamSessionUsage = `usage: aimem team-session open --service SERVICE --team TEAM --session ID [--hub-name NAME]  < handle
       aimem team-session refresh ID                                                      < handle
       aimem team-session close ID
       aimem team-session status ID

Client-only: the aicrew client binds one agent conversation to one verified
team session. open and refresh read the aimem-scoped handle from stdin (never
from the command line), verify it online with the hub and write a private
session file under the aimem state root; open prints its path. The aicrew
client then starts the agent with AIMEM_TEAM_SESSION=<path>, and that
conversation's aimem mcp serves only the team's reads. The file never holds
the individual credential or aicrew's own token. close removes it.`

// receiptSink refuses every stdout but a pipe: a terminal would show the
// receipt on screen and in scrollback, and a file would keep it.
func receiptSink(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("cannot inspect stdout (%v); the proof receipt is written only into a pipe", err)
	}
	switch m := fi.Mode(); {
	case m&os.ModeNamedPipe != 0:
		return nil
	case m&os.ModeCharDevice != 0:
		return errors.New("stdout is a terminal: aimem identity proof writes the proof receipt only into a pipe read by the aicrew client, never onto a screen or into a transcript. It is a client command, not for a model or a person")
	default:
		return errors.New("stdout is not a pipe (a file or another destination): aimem identity proof writes the proof receipt only into a pipe read by the aicrew client, never into a file")
	}
}

// teamHub resolves the named (or default) hub with the installation's
// individual credential and a verified-TLS client.
func teamHub(root, name string) (string, *adapter.HubConfig, string, *http.Client, error) {
	resolved, h := adapter.ResolveHub(root, name)
	if h == nil {
		if name == "" {
			return "", nil, "", nil, errors.New("no hub is configured on this machine (aimem hub add)")
		}
		return "", nil, "", nil, fmt.Errorf("hub %q is not configured on this machine", name)
	}
	cred, err := teamsession.Credential(h)
	if err != nil {
		return "", nil, "", nil, err
	}
	client, err := teamsession.HubClient(h)
	if err != nil {
		return "", nil, "", nil, err
	}
	return resolved, h, cred, client, nil
}

func identityProofCmd(args []string, stdout *os.File, root string) error {
	fs := flag.NewFlagSet("aimem identity proof", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	peer := fs.String("peer", "", "")
	hubID := fs.String("hub-id", "", "")
	challenge := fs.String("challenge", "", "")
	hubName := fs.String("hub-name", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *peer == "" || *hubID == "" || *challenge == "" {
		return errors.New(proofUsage)
	}
	// The destination is checked before anything else: nothing is asked of
	// the hub for a receipt that could not be delivered privately.
	if err := receiptSink(stdout); err != nil {
		return err
	}
	_, h, cred, client, err := teamHub(root, *hubName)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"peer_service_id": *peer, "hub_id": *hubID, "challenge_id": *challenge})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(h.URL, "/")+"/v1/identity/proofs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cred)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Aimem-Identity-Version", "1")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("hub unreachable: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		if r := teamsession.ParseRefusal(resp.StatusCode, raw); r != nil {
			return fmt.Errorf("the hub refused the proof: %v", r)
		}
		return fmt.Errorf("the hub answered %d to the proof request", resp.StatusCode)
	}
	var out struct {
		Receipt string `json:"receipt"`
	}
	if json.Unmarshal(raw, &out) != nil || !strings.HasPrefix(out.Receipt, "amr1_") {
		return errors.New("the hub's proof answer holds no receipt")
	}
	_, err = fmt.Fprintln(stdout, out.Receipt)
	return err
}

// readHandle takes one aimem-scoped handle from stdin.
func readHandle(stdin io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(stdin, 4096))
	if err != nil {
		return "", err
	}
	h := strings.TrimSpace(string(raw))
	if !introspect.ValidHandle(h) {
		return "", errors.New("stdin holds no aimem-scoped handle (acs1_...); the handle is accepted only on stdin")
	}
	return h, nil
}

// verifiedFile verifies handle with the hub and returns the session file
// it would write, checked against the expected service, team and session.
func verifiedFile(root, hubName, handle, service, team, session string) (teamsession.File, error) {
	name, h, cred, client, err := teamHub(root, hubName)
	if err != nil {
		return teamsession.File{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rep, _, err := teamsession.Verify(ctx, client, h.URL, cred, handle)
	if err != nil {
		return teamsession.File{}, fmt.Errorf("the handle was not verified: %w", err)
	}
	if rep.Team.ServiceID != service || rep.Team.TeamID != team || rep.Team.SessionID != session {
		return teamsession.File{}, errors.New("the hub reports this handle for another service, team or session; nothing was written")
	}
	return teamsession.File{Version: 1, Hub: name, URL: strings.TrimRight(h.URL, "/"), UserID: rep.UserID, TokenID: rep.TokenID,
		ServiceID: service, TeamID: team, SessionID: session, Generation: rep.Team.Generation, Handle: handle,
		HandleExpiresAt: rep.Team.HandleExpiresAt, UpdatedAt: time.Now().UTC()}, nil
}

func aicrewSessionCmd(args []string) error {
	return runAicrewSession(args, os.Stdin, os.Stdout, stateRoot())
}

func runAicrewSession(args []string, stdin io.Reader, out io.Writer, root string) error {
	usage := errors.New(teamSessionUsage)
	if len(args) == 0 {
		return usage
	}
	switch verb, rest := args[0], args[1:]; verb {
	case "open":
		fs := flag.NewFlagSet("aimem team-session open", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		service := fs.String("service", "", "")
		team := fs.String("team", "", "")
		session := fs.String("session", "", "")
		hubName := fs.String("hub-name", "", "")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || *service == "" || *team == "" || !teamsession.ValidSessionID(*session) {
			return usage
		}
		path := teamsession.PathFor(root, *session)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("team session %s is already open; use refresh, or close it first", *session)
		}
		handle, err := readHandle(stdin)
		if err != nil {
			return err
		}
		f, err := verifiedFile(root, *hubName, handle, *service, *team, *session)
		if err != nil {
			return err
		}
		err = teamsession.Locked(root, func() error {
			if _, err := os.Lstat(path); err == nil {
				return fmt.Errorf("team session %s was opened by another command meanwhile; nothing was written", *session)
			}
			return teamsession.Save(path, f)
		})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, path)
		return nil
	case "refresh":
		if len(rest) != 1 || !teamsession.ValidSessionID(rest[0]) {
			return usage
		}
		path := teamsession.PathFor(root, rest[0])
		old, err := teamsession.Load(path)
		if err != nil {
			return fmt.Errorf("team session %s is not open here: %w", rest[0], err)
		}
		handle, err := readHandle(stdin)
		if err != nil {
			return err
		}
		f, err := verifiedFile(root, old.Hub, handle, old.ServiceID, old.TeamID, old.SessionID)
		if err != nil {
			return err
		}
		if f.Binding() != old.Binding() {
			return errors.New("the refreshed handle belongs to another hub or user; the session file was left as it was")
		}
		err = teamsession.Locked(root, func() error {
			if cur, err := loadSettled(path); err != nil || cur.Handle != old.Handle {
				return fmt.Errorf("team session %s was closed or refreshed by another command meanwhile; nothing was written", rest[0])
			}
			return teamsession.Save(path, f)
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "team session %s refreshed (generation %s)\n", f.SessionID, f.Generation)
		return nil
	case "close":
		if len(rest) != 1 || !teamsession.ValidSessionID(rest[0]) {
			return usage
		}
		path := teamsession.PathFor(root, rest[0])
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(out, "team session %s is not open here\n", rest[0])
			return nil
		}
		f, loadErr := loadSettled(path)
		if loadErr == nil {
			// Local state is dropped only once the hub says the session has
			// ended: an active session, or a hub that cannot answer, keeps it.
			if err := confirmEnded(root, f); err != nil {
				return fmt.Errorf("team session %s was kept: %w", rest[0], err)
			}
		}
		// A file that cannot be loaded can bind no conversation, and one
		// readable by other accounts should not keep its handle; either way
		// it goes. What goes is only the file this command judged: one that
		// a refresh replaced meanwhile is kept.
		err := teamsession.Locked(root, func() error {
			cur, err := loadSettled(path)
			switch {
			case errors.Is(err, os.ErrNotExist):
				return nil
			case (err == nil) != (loadErr == nil) || (err == nil && cur.Handle != f.Handle):
				return fmt.Errorf("team session %s was kept: another command changed it meanwhile; run close again", rest[0])
			}
			return os.Remove(path)
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "team session %s closed\n", rest[0])
		return nil
	case "status":
		if len(rest) != 1 || !teamsession.ValidSessionID(rest[0]) {
			return usage
		}
		f, err := teamsession.Load(teamsession.PathFor(root, rest[0]))
		if err != nil {
			return fmt.Errorf("team session %s is not open here: %w", rest[0], err)
		}
		view, _ := json.MarshalIndent(map[string]any{
			"hub": f.Hub, "url": f.URL, "user_id": f.UserID, "service_id": f.ServiceID, "team_id": f.TeamID,
			"session_id": f.SessionID, "generation": f.Generation, "handle_expires_at": f.HandleExpiresAt,
			"updated_at": f.UpdatedAt, "path": teamsession.PathFor(root, rest[0]),
		}, "", "  ")
		fmt.Fprintln(out, string(view))
		return nil
	}
	return usage
}

// loadSettled loads a session file, retrying briefly: a conversation
// re-reading the file can make a load fail for an instant on Windows. A
// missing file is reported at once, as os.ErrNotExist.
func loadSettled(path string) (teamsession.File, error) {
	if _, err := os.Lstat(path); err != nil {
		return teamsession.File{}, err
	}
	f, err := teamsession.Load(path)
	for i := 0; err != nil && i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		f, err = teamsession.Load(path)
	}
	return f, err
}

// confirmEnded asks the hub whether the session file's handle is still
// active. Only a context_stale answer, which aicrew gives once a session has
// ended, left or expired, confirms that the file may go.
func confirmEnded(root string, f teamsession.File) error {
	_, h, cred, client, err := teamHub(root, f.Hub)
	if err != nil {
		return fmt.Errorf("the hub cannot be asked whether it has ended (%v)", err)
	}
	if strings.TrimRight(h.URL, "/") != f.URL {
		return errors.New("the hub's configured URL changed since the session was opened; its end cannot be confirmed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, err = teamsession.Verify(ctx, client, f.URL, cred, f.Handle)
	var r *teamsession.Refusal
	switch {
	case err == nil:
		return errors.New("the hub still reports it active; leave it through aicrew first")
	case errors.As(err, &r) && r.Code == "context_stale":
		return nil
	}
	return fmt.Errorf("the hub could not confirm it has ended (%v); try again once it answers", err)
}
