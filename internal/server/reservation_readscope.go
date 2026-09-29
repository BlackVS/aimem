package server

// Aicrew's read scope (task C6b; coordination wire §2): three HTTP-only
// reads, over TLS this hub terminated, for a reservation.read peer
// credential. The bearer gate confines that credential to exactly these
// routes. Each answers only about records under the credential's own
// service's proofs; everything else, whether missing or out of scope, is
// none. No answer carries task content, another holder, a request key or a
// proof.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"aimem/internal/store"
)

// readScopePerMinute bounds reads per credential (coordination wire bounds).
const readScopePerMinute = 60

// readScopeLimiter counts one credential's reads in fixed one-minute
// windows. now is the clock; a test pins it.
type readScopeLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	windows map[string]readScopeWindow
}

type readScopeWindow struct {
	start time.Time
	count int
}

// allow records one read by credentialID and reports whether it is within
// the bound.
func (l *readScopeLimiter) allow(credentialID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.windows == nil {
		l.windows = map[string]readScopeWindow{}
	}
	w := l.windows[credentialID]
	if now.Sub(w.start) >= time.Minute {
		for id, old := range l.windows {
			if now.Sub(old.start) >= time.Minute {
				delete(l.windows, id)
			}
		}
		w = readScopeWindow{start: now}
	}
	w.count++
	l.windows[credentialID] = w
	return w.count <= readScopePerMinute
}

// readScope applies the checks every read shares: TLS, the version, a
// reservation.read credential of the path's service, and the rate bound. It
// returns the service on success; otherwise it has answered the refusal.
func (s *Server) readScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !identityTLS(r) {
		s.identityRefuse(w, "tls_required")
		return "", false
	}
	if r.Header.Get(reservationVersionHeader) != "1" {
		s.identityRefuse(w, "unsupported_version")
		return "", false
	}
	id, ok := IdentityFrom(r.Context())
	switch {
	case !ok || id.Role != "peer":
		s.identityRefuse(w, "peer_unauthenticated")
		return "", false
	case !peerRouteAllowed(r, id.Peer), r.PathValue("service_id") != id.Peer.ServiceID:
		s.identityRefuse(w, "peer_forbidden")
		return "", false
	case !s.readLimiter().allow(id.Peer.CredentialID):
		s.identityRefuse(w, "rate_limited")
		return "", false
	}
	return id.Peer.ServiceID, true
}

func (s *Server) readLimiter() *readScopeLimiter {
	s.readOnce.Do(func() {
		if s.readLimit == nil {
			s.readLimit = &readScopeLimiter{now: time.Now}
		}
	})
	return s.readLimit
}

// boundedRead runs one read's lookups within the request's deadline, which
// the bearer gate set for the wire routes. The store calls it makes take the
// context, and a lookup that does not (finding the task's project) still
// cannot hold the answer past the deadline: the read then fails, and
// readScopeFailed answers the retryable refusal. The abandoned lookup is
// read-only and ends when the store frees.
func boundedRead(ctx context.Context, lookup func(context.Context) error) error {
	done := make(chan error, 1)
	go func() { done <- lookup(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// readScopeFailed answers a store that could not give a reliable answer:
// busy past the deadline, or a project that could not be read. A none it
// cannot vouch for is never answered.
func (s *Server) readScopeFailed(w http.ResponseWriter, what string, err error) {
	s.log.Error("reservation read scope", "read", what, "err", err)
	s.identityRefuse(w, "request_in_progress")
}

type readScopeReceipt struct {
	ID               string `json:"id"`
	Operation        string `json:"operation"`
	TaskID           string `json:"task_id"`
	RequestKeyDigest string `json:"request_key_digest"`
	ReservationID    string `json:"reservation_id"`
	Fence            string `json:"fence"`
	TaskRevision     int64  `json:"task_revision"`
	MemberUserID     string `json:"member_user_id"`
	VerifiedMode     string `json:"verified_mode"`
	CommittedAt      string `json:"committed_at"`
}

type readScopeReceiptAnswer struct {
	State   string            `json:"state"`
	Receipt *readScopeReceipt `json:"receipt,omitempty"`
}

func (s *Server) readScopeReceiptOK(w http.ResponseWriter, rc store.ServiceReceipt, found bool) {
	w.Header().Set("Cache-Control", "no-store")
	if !found {
		s.ok(w, readScopeReceiptAnswer{State: "none"})
		return
	}
	s.ok(w, readScopeReceiptAnswer{State: "committed", Receipt: &readScopeReceipt{
		ID: rc.ID, Operation: string(rc.Operation), TaskID: rc.TaskID, RequestKeyDigest: rc.RequestKeyDigest,
		ReservationID: rc.ReservationID, Fence: strconv.FormatInt(rc.Fence, 10), TaskRevision: rc.TaskRevision,
		MemberUserID: rc.MemberUserID, VerifiedMode: rc.VerifiedMode, CommittedAt: rc.CommittedAt,
	}})
}

// readReceiptByProof answers the transition committed under a proof this
// service issued.
func (s *Server) readReceiptByProof(w http.ResponseWriter, r *http.Request) {
	service, ok := s.readScope(w, r)
	if !ok {
		return
	}
	var rc store.ServiceReceipt
	var found bool
	err := boundedRead(r.Context(), func(ctx context.Context) (err error) {
		rc, found, err = s.reg.ServiceReceiptByProof(ctx, service, r.PathValue("proof_digest"))
		return err
	})
	if err != nil {
		s.readScopeFailed(w, "receipt by proof", err)
		return
	}
	s.readScopeReceiptOK(w, rc, found)
}

// readReceiptByKey answers a member transition made on a reservation this
// service's proof established, by operation and request-key digest.
func (s *Server) readReceiptByKey(w http.ResponseWriter, r *http.Request) {
	service, ok := s.readScope(w, r)
	if !ok {
		return
	}
	taskID := r.PathValue("task_id")
	var rc store.ServiceReceipt
	var found bool
	err := boundedRead(r.Context(), func(ctx context.Context) error {
		_, db, err := s.reg.LocateTask(taskID)
		if err != nil {
			return err
		}
		rc, found, err = db.ServiceReceiptByKey(ctx, service, taskID, store.ReservationOperation(r.PathValue("operation")), r.PathValue("request_key_digest"))
		return err
	})
	switch {
	case errors.Is(err, store.ErrTaskNotFound):
		s.readScopeReceiptOK(w, store.ServiceReceipt{}, false)
		return
	case err != nil:
		s.readScopeFailed(w, "receipt by key", err)
		return
	}
	s.readScopeReceiptOK(w, rc, found)
}

type readScopeHold struct {
	State         string `json:"state"`
	ReservationID string `json:"reservation_id,omitempty"`
	Fence         string `json:"fence,omitempty"`
	HolderMode    string `json:"holder_mode,omitempty"`
	OwnWorkRef    string `json:"own_work_ref,omitempty"`
	ClosingFence  string `json:"closing_fence,omitempty"`
	ClosedBy      string `json:"closed_by,omitempty"`
	ClosedAt      string `json:"closed_at,omitempty"`
	TaskRevision  int64  `json:"task_revision,omitempty"`
}

// readHold answers this service's hold on a task: held under its proof, its
// most recent reservation closed (closure evidence, C5c-w), or none.
func (s *Server) readHold(w http.ResponseWriter, r *http.Request) {
	service, ok := s.readScope(w, r)
	if !ok {
		return
	}
	taskID := r.PathValue("task_id")
	w.Header().Set("Cache-Control", "no-store")
	var h store.ServiceHold
	err := boundedRead(r.Context(), func(ctx context.Context) error {
		_, db, err := s.reg.LocateTask(taskID)
		if err != nil {
			return err
		}
		h, err = db.ServiceHoldStatus(ctx, taskID, service)
		return err
	})
	switch {
	case errors.Is(err, store.ErrTaskNotFound):
		s.ok(w, readScopeHold{State: "none"})
		return
	case err != nil:
		s.readScopeFailed(w, "hold status", err)
		return
	}
	out := readScopeHold{State: h.State}
	switch h.State {
	case "held":
		out.ReservationID, out.Fence, out.HolderMode, out.OwnWorkRef, out.TaskRevision =
			h.ReservationID, strconv.FormatInt(h.Fence, 10), h.HolderMode, h.OwnWorkRef, h.TaskRevision
	case "closed":
		out.ReservationID, out.ClosingFence, out.ClosedBy, out.ClosedAt, out.TaskRevision =
			h.ReservationID, strconv.FormatInt(h.ClosingFence, 10), h.ClosedBy, h.ClosedAt, h.TaskRevision
	}
	s.ok(w, out)
}
