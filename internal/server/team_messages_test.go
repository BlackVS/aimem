package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aimem/internal/store"
)

type messageFixture struct {
	*taskFixture
	base, peer        string
	sender, recipient store.TeamSessionHandle
}

func newMessageFixture(t *testing.T) *messageFixture {
	t.Helper()
	f := newTaskFixture(t)
	acc, err := f.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = acc.SetGrant("admin", f.alphaInstance, "user", f.bobUser, true); err != nil {
		t.Fatal(err)
	}
	_, peer, err := acc.Issue("admin", f.bobUser, "peer", f.alphaInstance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	w := taskReq(t, f.h, "POST", "/v1/projects/alpha/teams", f.admin, "setup", fmt.Sprintf(`{"name":"Workers","enrollment":[{"user_id":%q,"coordinator":true},{"user_id":%q}]}`, f.aliceUser, f.bobUser))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var team store.Team
	if err := json.Unmarshal(w.Body.Bytes(), &team); err != nil {
		t.Fatal(err)
	}
	m := &messageFixture{taskFixture: f, base: "/v1/projects/alpha/teams/" + team.ID, peer: peer}
	join := func(token, role, key string) store.TeamSessionHandle {
		w := taskReq(t, f.h, "POST", m.base+"/join", token, key, fmt.Sprintf(`{"role":%q,"profile":{"label":"agent","platform":"fixture","platform_version":"1"}}`, role))
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body)
		}
		var out struct {
			Session teamMemberView `json:"session"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return store.TeamSessionHandle{SessionID: out.Session.ID, Generation: out.Session.Generation}
	}
	m.sender = join(f.alice, "coordinator", "join-a")
	m.recipient = join(peer, "worker", "join-b")
	return m
}

func (f *messageFixture) send(t *testing.T, key string) store.TeamMessage {
	t.Helper()
	body := fmt.Sprintf(`{"session_id":%q,"generation":1,"recipient":{"kind":"member","id":%q},"kind":"question","payload":{"text":"Which test should run?"}}`, f.sender.SessionID, f.recipient.SessionID)
	w := taskReq(t, f.h, "POST", f.base+"/messages", f.alice, key, body)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Message store.TeamMessage `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Message
}

func (f *messageFixture) inbox() string {
	return fmt.Sprintf("%s/inbox?session_id=%s&generation=1", f.base, f.recipient.SessionID)
}

func messagePage(t *testing.T, w *httptest.ResponseRecorder) store.TeamMessagePage {
	t.Helper()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Version int `json:"protocol_version"`
		store.TeamMessagePage
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Version != 1 {
		t.Fatal(err, w.Body)
	}
	return out.TeamMessagePage
}

func TestMessageHTTPExchange(t *testing.T) {
	f := newMessageFixture(t)
	m := f.send(t, "send-one")
	if retry := f.send(t, "send-one"); retry.ID != m.ID || retry.Sequence != m.Sequence {
		t.Fatal("send duplicated")
	}
	if m.SenderID != f.sender.SessionID || m.SenderGeneration != 1 {
		t.Fatal("sender not derived from session")
	}
	ack := fmt.Sprintf(`{"session_id":%q,"generation":1,"message_ids":[%q]}`, f.recipient.SessionID, m.ID)
	if w := taskReq(t, f.h, "POST", f.base+"/ack", f.peer, "ack", ack); w.Code != 409 {
		t.Fatal("unseen ack", w.Code)
	}
	// History is team-visible even when routed directly; reading history is not delivery.
	page := messagePage(t, taskReq(t, f.h, "GET", strings.Replace(f.inbox(), "/inbox?", "/messages?", 1), f.peer, "", ""))
	if len(page.Messages) != 1 {
		t.Fatal(page)
	}
	if w := taskReq(t, f.h, "POST", f.base+"/ack", f.peer, "ack", ack); w.Code != 409 {
		t.Fatal("history implied delivery", w.Code)
	}
	for range 2 {
		page = messagePage(t, taskReq(t, f.h, "GET", f.inbox(), f.peer, "", ""))
		if len(page.Messages) != 1 || page.Messages[0].ID != m.ID {
			t.Fatal("lost-response retry", page)
		}
	}
	f.send(t, "send-two")
	page = messagePage(t, taskReq(t, f.h, "GET", f.inbox()+"&limit=1", f.peer, "", ""))
	if !page.HasMore || page.Next != m.Sequence {
		t.Fatal("pagination", page)
	}
	next := messagePage(t, taskReq(t, f.h, "GET", f.inbox()+fmt.Sprintf("&after=%d", page.Next), f.peer, "", ""))
	if len(next.Messages) != 1 || next.Messages[0].ID == m.ID {
		t.Fatal(next)
	}
	reply := fmt.Sprintf(`{"session_id":%q,"generation":1,"recipient":{"kind":"member","id":%q},"kind":"answer","reply_to":%q,"payload":{"text":"Run the focused suite."}}`, f.recipient.SessionID, f.sender.SessionID, m.ID)
	if w := taskReq(t, f.h, "POST", f.base+"/messages", f.peer, "answer", reply); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	for range 2 {
		if w := taskReq(t, f.h, "POST", f.base+"/ack", f.peer, "ack", ack); w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	page = messagePage(t, taskReq(t, f.h, "GET", f.inbox(), f.peer, "", ""))
	if len(page.Messages) != 1 || page.Messages[0].ID == m.ID {
		t.Fatal("ack cleared wrong messages", page)
	}
	for _, token := range []string{f.admin, f.env, f.writer, f.bob, f.alice} {
		if w := taskReq(t, f.h, "GET", f.inbox(), token, "", ""); w.Code != 403 {
			t.Fatal("wrong credential", w.Code)
		}
	}
	if w := taskReq(t, f.s.Handler(), "GET", f.inbox(), "", "", ""); w.Code != 403 {
		t.Fatal("operator session", w.Code)
	}
	for _, suffix := range []string{"&wait_seconds=26", "&wait_seconds=-1", "&limit=0", "&after=-1", "&generation=1", "&actor=x", "&after=no"} {
		if w := taskReq(t, f.h, "GET", f.inbox()+suffix, f.peer, "", ""); w.Code != 400 {
			t.Fatal(suffix, w.Code)
		}
	}
	for _, body := range []string{`null`, reply + ` {}`, strings.Replace(reply, `"kind":"answer"`, `"sender_id":"forged","kind":"answer"`, 1), strings.Replace(reply, `"text":"Run the focused suite."`, `"text":"x","extra":true`, 1)} {
		if w := taskReq(t, f.h, "POST", f.base+"/messages", f.peer, "invalid", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	// Session generation changes invalidate new reads, not accepted send receipts.
	handle := fmt.Sprintf(`{"session_id":%q,"generation":1}`, f.recipient.SessionID)
	if w := taskReq(t, f.h, "POST", f.base+"/resume", f.peer, "resume", handle); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := taskReq(t, f.h, "GET", f.inbox(), f.peer, "", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
}

func TestMessageHTTPQuotaAndRetry(t *testing.T) {
	t.Setenv("AIMEM_TEAM_MAX_MESSAGES", "1")
	f := newMessageFixture(t)
	m := f.send(t, "one")
	if f.send(t, "one").ID != m.ID {
		t.Fatal("receipt failed at quota")
	}
	body := fmt.Sprintf(`{"session_id":%q,"generation":1,"recipient":{"kind":"team"},"kind":"note","payload":{"text":"next"}}`, f.sender.SessionID)
	if w := taskReq(t, f.h, "POST", f.base+"/messages", f.alice, "two", body); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "POST", f.base+"/messages", f.alice, "one", body); w.Code != 409 {
		t.Fatal("changed retry", w.Code)
	}
}

func TestMessageHTTPWait(t *testing.T) {
	for _, mode := range []string{"delivery", "timeout", "grant", "token", "enrollment", "generation", "disabled", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newMessageFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("GET", f.inbox()+"&wait_seconds=2", nil).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+f.peer)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			start := time.Now()
			go func() { f.h.ServeHTTP(w, req); close(done) }()
			select {
			case <-done:
				t.Fatal("wait returned immediately", w.Code, w.Body)
			case <-time.After(150 * time.Millisecond):
			}
			acc, err := f.s.openAccess(false)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "delivery":
				f.send(t, "wake")
			case "grant":
				if err := acc.SetGrant("admin", f.alphaInstance, "user", f.bobUser, false); err != nil {
					t.Fatal(err)
				}
			case "token":
				id, err := acc.Authenticate(f.peer)
				if err != nil {
					t.Fatal(err)
				}
				if err = acc.Revoke("admin", id.TokenID); err != nil {
					t.Fatal(err)
				}
			case "enrollment":
				body := fmt.Sprintf(`{"name":"Workers","expected_revision":1,"enrollment":[{"user_id":%q,"coordinator":true}]}`, f.aliceUser)
				if out := taskReq(t, f.h, "PUT", f.base, f.admin, "revoke", body); out.Code != 200 {
					t.Fatal(out.Body)
				}
			case "generation":
				body := fmt.Sprintf(`{"session_id":%q,"generation":1}`, f.recipient.SessionID)
				if out := taskReq(t, f.h, "POST", f.base+"/resume", f.peer, "resume", body); out.Code != 200 {
					t.Fatal(out.Body)
				}
			case "disabled":
				db, err := f.reg.OpenExisting("alpha")
				if err != nil {
					t.Fatal(err)
				}
				if err = db.SetMeta(store.TasksMetaKey, "off"); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("wait did not finish")
			}
			switch mode {
			case "delivery":
				if len(messagePage(t, w).Messages) != 1 {
					t.Fatal("delivery missing")
				}
			case "timeout":
				if len(messagePage(t, w).Messages) != 0 || time.Since(start) < 2*time.Second {
					t.Fatal("timeout contract")
				}
			case "cancel":
				if time.Since(start) > time.Second || w.Body.Len() != 0 {
					t.Fatal("cancellation ignored")
				}
			case "generation":
				if w.Code != 409 {
					t.Fatal(w.Code, w.Body)
				}
			default:
				if w.Code != http.StatusForbidden {
					t.Fatal(w.Code, w.Body)
				}
			}
		})
	}
}
