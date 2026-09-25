package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/store"
)

type assignmentFixture struct {
	*messageFixture
	task  store.Task
	offer store.TeamOffer
}

func assignmentJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newAssignmentFixture(t *testing.T) *assignmentFixture {
	t.Helper()
	f := &assignmentFixture{messageFixture: newMessageFixture(t)}
	w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "task", taskBody)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &f.task); err != nil {
		t.Fatal(err)
	}
	f.offer = store.TeamOffer{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, TaskID: f.task.ID, ExpectedRevision: f.task.Revision, Worker: f.recipient, SuitabilityRationale: "S task; Go capability and model fit confirmed by operator", CostRationale: "Least costly suitable available member"}
	return f
}

func assignmentResult(t *testing.T, w *httptest.ResponseRecorder, status int) store.TeamAssignment {
	t.Helper()
	if w.Code != status {
		t.Fatal(w.Code, w.Body)
	}
	var out struct {
		Version    int                  `json:"protocol_version"`
		Assignment store.TeamAssignment `json:"assignment"`
		Ready      *bool                `json:"workflow_ready"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Version != 1 || out.Ready == nil || !*out.Ready || out.Assignment.ID == "" {
		t.Fatal(w.Body, err)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Request-ID") == "" {
		t.Fatal(w.Header())
	}
	for _, field := range []string{`"token_id"`, `"user_id"`, `"actor"`} {
		if strings.Contains(w.Body.String(), field) {
			t.Fatal("internal identity in response", field)
		}
	}
	return out.Assignment
}

func (f *assignmentFixture) offerRequest(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	return taskReq(t, f.h, "POST", f.base+"/assignments", f.alice, key, assignmentJSON(t, f.offer))
}
func (f *assignmentFixture) attemptURL(id string, h store.TeamSessionHandle) string {
	return fmt.Sprintf("%s/assignments/%s?session_id=%s&generation=%d", f.base, id, h.SessionID, h.Generation)
}
func (f *assignmentFixture) command(t *testing.T, id, op, token, key string, c store.TeamAssignmentCommand) *httptest.ResponseRecorder {
	t.Helper()
	return taskReq(t, f.h, "POST", f.base+"/assignments/"+id+"/"+op, token, key, assignmentJSON(t, c))
}

func TestAssignmentHTTPExchange(t *testing.T) {
	f := newAssignmentFixture(t)
	offer := assignmentResult(t, f.offerRequest(t, "offer"), 201)
	if offer.State != "OFFERED" || offer.TaskRevision != f.task.Revision || offer.Worker != f.recipient || offer.Coordinator != f.sender || offer.ProfileRevision != 1 || offer.Requirements.Title != f.task.Title {
		t.Fatal(offer)
	}
	if retry := assignmentResult(t, f.offerRequest(t, "offer"), 201); retry.ID != offer.ID {
		t.Fatal("duplicate offer")
	}
	for _, who := range []struct {
		token string
		h     store.TeamSessionHandle
	}{{f.alice, f.sender}, {f.peer, f.recipient}} {
		if read := assignmentResult(t, taskReq(t, f.h, "GET", f.attemptURL(offer.ID, who.h), who.token, "", ""), 200); read.ID != offer.ID {
			t.Fatal(read)
		}
	}
	cmd := store.TeamAssignmentCommand{TeamSessionHandle: f.recipient}
	for range 2 {
		if run := assignmentResult(t, f.command(t, offer.ID, "accept", f.peer, "accept", cmd), 200); run.State != "RUNNING" {
			t.Fatal(run)
		}
	}
	w := taskReq(t, f.h, "GET", "/v1/tasks/"+f.task.ID, f.peer, "", "")
	var task store.Task
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil || task.State != "IN_PROGRESS" || task.Revision != 2 || task.Coordination == nil || task.Coordination.AttemptID != offer.ID {
		t.Fatal(task, err)
	}
	withdraw := store.TeamAssignmentCommand{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, Reason: "stop"}
	if w := f.command(t, offer.ID, "withdraw", f.alice, "withdraw-running", withdraw); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	// Changed coordinator generation fences new commands; retries preserve results.
	w = taskReq(t, f.h, "POST", f.base+"/resume", f.alice, "resume", assignmentJSON(t, f.sender))
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if retry := assignmentResult(t, f.offerRequest(t, "offer"), 201); retry.ID != offer.ID {
		t.Fatal(retry)
	}
	if w := f.offerRequest(t, "stale"); w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	for _, state := range []string{"decline", "withdraw"} {
		g := newAssignmentFixture(t)
		o := assignmentResult(t, g.offerRequest(t, "offer"), 201)
		c := store.TeamAssignmentCommand{TeamSessionHandle: g.recipient, Reason: "reassess"}
		token := g.peer
		if state == "withdraw" {
			c.TeamSessionHandle = g.sender
			c.CoordinatorGeneration = 1
			token = g.alice
		}
		for range 2 {
			closed := assignmentResult(t, g.command(t, o.ID, state, token, state, c), 200)
			if closed.State == "OFFERED" {
				t.Fatal(closed)
			}
		}
		if next := assignmentResult(t, g.offerRequest(t, "again"), 201); next.ID == o.ID {
			t.Fatal("closed attempt reused")
		}
	}
}

func TestAssignmentOfferRefusesActiveTaskReservation(t *testing.T) {
	f := newAssignmentFixture(t)
	db, err := f.reg.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	hold, err := db.ApplyTaskReservation(store.ReservationClaim, store.TaskReservationInput{TaskID: f.task.ID,
		ExpectedRevision: f.task.Revision, Holder: store.ReservationHolder{Mode: "standalone", Ref: "private-work"}},
		store.TaskActor{Kind: "user", Name: "Alice", UserID: f.aliceUser, TokenID: f.aliceTokenID}, "offer-held-claim")
	if err != nil {
		t.Fatal(err)
	}
	w := f.offerRequest(t, "offer-held")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "active reservation") || strings.Contains(w.Body.String(), "private-work") {
		t.Fatalf("legacy offer bypass or holder disclosure: %d %s", w.Code, w.Body)
	}
	if got, err := db.GetTask(f.task.ID); err != nil || got.Coordination != nil || got.Revision != f.task.Revision {
		t.Fatalf("refused offer changed task: %+v, %v", got, err)
	}
	content := f.task.TaskContent
	released, err := db.ApplyTaskReservation(store.ReservationRelease, store.TaskReservationInput{TaskID: f.task.ID,
		ID: hold.Reservation.ID, Fence: hold.Reservation.Fence, ExpectedRevision: f.task.Revision,
		Content: &content, Reason: "standalone stopped"},
		store.TaskActor{Kind: "user", Name: "Alice", UserID: f.aliceUser, TokenID: f.aliceTokenID}, "offer-held-release")
	if err != nil {
		t.Fatal(err)
	}
	f.offer.ExpectedRevision = released.Task.Revision
	if w := f.offerRequest(t, "offer-after-release"); w.Code != http.StatusCreated {
		t.Fatalf("legacy offer after release: %d %s", w.Code, w.Body)
	}
}

func TestAssignmentHTTPBoundary(t *testing.T) {
	f := newAssignmentFixture(t)
	out := assignmentResult(t, f.offerRequest(t, "offer"), 201)
	read := f.attemptURL(out.ID, f.recipient)
	for _, token := range []string{f.admin, f.env, f.writer, f.bob, ""} {
		for _, route := range []struct{ method, path, body string }{{"POST", f.base + "/assignments", assignmentJSON(t, f.offer)}, {"GET", read, ""}, {"POST", f.base + "/assignments/" + out.ID + "/accept", assignmentJSON(t, f.recipient)}, {"POST", f.base + "/assignments/" + out.ID + "/decline", `{}`}, {"POST", f.base + "/assignments/" + out.ID + "/withdraw", `{}`}} {
			w := taskReq(t, f.h, route.method, route.path, token, "denied", route.body)
			want := 403
			if token == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(route.path, w.Code, w.Body)
			}
		}
	}
	if w := taskReq(t, f.s.Handler(), "POST", f.base+"/assignments", "", "socket", assignmentJSON(t, f.offer)); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := taskReq(t, f.h, "GET", read, f.alice, "", ""); w.Code != 403 {
		t.Fatal("borrowed worker handle", w.Code)
	}
	if w := f.command(t, out.ID, "accept", f.alice, "wrong-worker", store.TeamAssignmentCommand{TeamSessionHandle: f.sender}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	workerOffer := f.offer
	workerOffer.TeamSessionHandle = f.recipient
	if w := taskReq(t, f.h, "POST", f.base+"/assignments", f.peer, "self", assignmentJSON(t, workerOffer)); w.Code != 409 {
		t.Fatal("worker self offer", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "POST", strings.Replace(f.base, "alpha", "beta", 1)+"/assignments", f.alice, "cross-project", assignmentJSON(t, f.offer)); w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, suffix := range []string{"&generation=1", "&actor=x", "&limit=1", "&generation=bad", "&session_id=x"} {
		if w := taskReq(t, f.h, "GET", read+suffix, f.peer, "", ""); w.Code != 400 {
			t.Fatal(suffix, w.Code)
		}
	}
	for _, q := range []string{"", "?generation=1", "?session_id=x&generation=0", "?session_id=x&generation=9223372036854775808"} {
		if w := taskReq(t, f.h, "GET", f.base+"/assignments/"+out.ID+q, f.peer, "", ""); w.Code != 400 {
			t.Fatal(q, w.Code)
		}
	}
	if w := f.offerRequest(t, ""); w.Code != 400 {
		t.Fatal("missing retry key", w.Code)
	}
	body := assignmentJSON(t, f.offer)
	for _, bad := range []string{body + ` {}`, strings.Replace(body, `"task_id":`, `"actor":"forged","task_id":`, 1), strings.Replace(body, `"worker":{`, `"worker":{"token_id":"forged",`, 1), strings.Replace(body, `"task_id":`, `"backend_session_id":"fake","task_id":`, 1), strings.Repeat(" ", 65<<10) + body} {
		if w := taskReq(t, f.h, "POST", f.base+"/assignments", f.alice, "invalid", bad); w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	for _, bad := range []string{`{"session_id":"x","generation":1,"worker":{}}`, `{"session_id":"x","generation":1,"reason":"ignored"}`} {
		if w := taskReq(t, f.h, "POST", f.base+"/assignments/"+out.ID+"/accept", f.peer, "invalid", bad); w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if w := taskReq(t, f.h, "GET", strings.Replace(read, out.ID, "00000000-0000-7000-8000-000000000000", 1), f.peer, "", ""); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestAssignmentHTTPLiveWorkerAuthority(t *testing.T) {
	for _, change := range []string{"grant", "revocation", "disabled", "scope", "enrollment", "availability", "generation"} {
		t.Run(change, func(t *testing.T) {
			f := newAssignmentFixture(t)
			out := assignmentResult(t, f.offerRequest(t, "offer"), 201)
			acc, err := f.s.openAccess(false)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := acc.Authenticate(f.peer)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "grant":
				err = acc.SetGrant("admin", f.alphaInstance, "user", f.bobUser, false)
			case "revocation":
				err = acc.Revoke("admin", identity.TokenID)
			case "disabled":
				err = acc.SetUser("admin", f.bobUser, "Bob", true)
			case "scope":
				// Same enrolled access user and live grant, but another token owns no
				// write scope here. A different session cannot borrow the offered one.
				_, secret, e := acc.Issue("admin", f.bobUser, "read-only", "", time.Now().Add(time.Hour))
				if e != nil {
					t.Fatal(e)
				}
				f.peer = secret
			case "enrollment":
				w := taskReq(t, f.h, "PUT", f.base, f.admin, "remove", fmt.Sprintf(`{"name":"Workers","expected_revision":1,"enrollment":[{"user_id":%q,"coordinator":true}]}`, f.aliceUser))
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
			case "availability":
				w := taskReq(t, f.h, "POST", f.base+"/heartbeat", f.peer, "away", fmt.Sprintf(`{"session_id":%q,"generation":1,"availability":"unavailable"}`, f.recipient.SessionID))
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
			case "generation":
				w := taskReq(t, f.h, "POST", f.base+"/resume", f.peer, "resume", assignmentJSON(t, f.recipient))
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			w := f.command(t, out.ID, "accept", f.peer, "accept", store.TeamAssignmentCommand{TeamSessionHandle: f.recipient})
			if w.Code != 401 && w.Code != 403 && w.Code != 409 {
				t.Fatal("accepted changed authority", w.Code, w.Body)
			}
			// The coordinator remains authorized to withdraw an unaccepted offer.
			assignmentResult(t, f.command(t, out.ID, "withdraw", f.alice, "withdraw", store.TeamAssignmentCommand{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, Reason: "worker unavailable"}), 200)
			if change != "scope" {
				w = f.offerRequest(t, "new-offer")
				if w.Code != 403 && w.Code != 409 {
					t.Fatal("new effect skipped live worker check", w.Code, w.Body)
				}
			}
		})
	}
}

func TestAssignmentHTTPCallerRevocationAndConflicts(t *testing.T) {
	f := newAssignmentFixture(t)
	f.offer.ExpectedRevision = 2
	w := f.offerRequest(t, "stale")
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body)
	}
	var conflict struct {
		Current taskResponse `json:"current"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &conflict); err != nil || conflict.Current.ID != f.task.ID || conflict.Current.Project != "alpha" || conflict.Current.Revision != 1 {
		t.Fatal(w.Body, err)
	}
	f.offer.ExpectedRevision = 1
	out := assignmentResult(t, f.offerRequest(t, "offer"), 201)
	acc, err := f.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = acc.SetGrant("admin", f.alphaInstance, "user", f.aliceUser, false); err != nil {
		t.Fatal(err)
	}
	if w := f.offerRequest(t, "offer"); w.Code != 403 {
		t.Fatal("receipt granted authority", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", f.attemptURL(out.ID, f.sender), f.alice, "", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestAssignmentHTTPAcceptWithdrawRace(t *testing.T) {
	f := newAssignmentFixture(t)
	out := assignmentResult(t, f.offerRequest(t, "offer"), 201)
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	var wg sync.WaitGroup
	codes := make([]int, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, op := range []string{"accept", "withdraw"} {
		wg.Add(1)
		go func(i int, op string) {
			defer wg.Done()
			<-start
			token := f.peer
			cmd := store.TeamAssignmentCommand{TeamSessionHandle: f.recipient}
			if op == "withdraw" {
				token = f.alice
				cmd = store.TeamAssignmentCommand{TeamSessionHandle: f.sender, CoordinatorGeneration: 1, Reason: "withdraw"}
			}
			b, err := json.Marshal(cmd)
			if err != nil {
				errs[i] = err
				return
			}
			req, err := http.NewRequest("POST", ts.URL+f.base+"/assignments/"+out.ID+"/"+op, strings.NewReader(string(b)))
			if err != nil {
				errs[i] = err
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Idempotency-Key", op)
			resp, err := ts.Client().Do(req)
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			codes[i] = resp.StatusCode
		}(i, op)
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || !((codes[0] == 200 && codes[1] == 409) || (codes[0] == 409 && codes[1] == 200)) {
		t.Fatal(codes, errs)
	}
}
