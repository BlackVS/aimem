package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"aimem/internal/uuidv7"
)

type pausedCommitDriver struct {
	base    driver.Driver
	entered chan struct{}
	release chan struct{}
}

func (d *pausedCommitDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &pausedCommitConn{Conn: conn, entered: d.entered, release: d.release}, nil
}

type pausedCommitConn struct {
	driver.Conn
	entered chan struct{}
	release chan struct{}
}

func (c *pausedCommitConn) Begin() (driver.Tx, error) {
	return nil, errors.New("paused commit test requires BeginTx")
}

func (c *pausedCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &pausedCommitTx{Tx: tx, entered: c.entered, release: c.release}, nil
}

type pausedCommitTx struct {
	driver.Tx
	entered chan struct{}
	release chan struct{}
}

func (t *pausedCommitTx) Commit() error {
	close(t.entered)
	<-t.release
	return t.Tx.Commit()
}

func dependencyFixture(t *testing.T) (*Registry, *DB, *DB, Task, Task) {
	t.Helper()
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	ownerDB, err := r.Open("owner")
	if err != nil {
		t.Fatal(err)
	}
	depDB, err := r.Open("dependency")
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range []*DB{ownerDB, depDB} {
		if err := db.SetMeta(TasksMetaKey, "on"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ProjectAccessID(db.projectID); err != nil {
			t.Fatal(err)
		}
	}
	dep, err := depDB.CreateTask(TaskContent{Title: "finished", State: "DONE", Archived: true},
		aliceActor, "dep-create")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := ownerDB.CreateTask(TaskContent{Title: "claim me", State: "READY",
		Dependencies: []string{dep.ID}}, aliceActor, "owner-create")
	if err != nil {
		t.Fatal(err)
	}
	return r, ownerDB, depDB, owner, dep
}

func allowDependencyRead(context.Context, string, string) error { return nil }

func TestDependencyClaimAndReconcile(t *testing.T) {
	r, ownerDB, depDB, owner, dep := dependencyFixture(t)
	in := claimInput(owner, "run")
	if _, err := ownerDB.ApplyTaskReservation(ReservationClaim, in, aliceActor, "raw"); !errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("raw claim must refuse dependencies: %v", err)
	}
	if _, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, "missing-verifier", nil); !errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("missing verifier: %v", err)
	}
	if _, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, "denied",
		func(_ context.Context, project, _ string) error {
			if project == "dependency" {
				return errors.New("denied")
			}
			return nil
		}); !errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("denied read: %v", err)
	}
	got, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, "claim", allowDependencyRead)
	if err != nil || got.Reservation.ID == "" {
		t.Fatalf("claim: %+v, %v", got, err)
	}
	replayed, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, "claim", allowDependencyRead)
	if err != nil || replayed.Reservation != got.Reservation {
		t.Fatalf("replay: %+v, %v", replayed, err)
	}
	if changed, err := r.ReconcileReservationDependencies(context.Background(), owner.ID, allowDependencyRead); err != nil || changed {
		t.Fatalf("unchanged proof: %t, %v", changed, err)
	}
	peer, err := NewRegistry(r.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerDB, err := peer.OpenExisting(depDB.projectID)
	if err != nil {
		t.Fatal(err)
	}
	content := dep.TaskContent
	content.State, content.Archived = "READY", false
	if _, err := peerDB.UpdateTask(dep.ID, content, dep.Revision, aliceActor, "reopen"); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.ReconcileReservationDependencies(context.Background(), owner.ID, allowDependencyRead); !changed || !errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("reopen must surface: %t, %v", changed, err)
	}
	if hold, err := ownerDB.GetTaskReservation(owner.ID); err != nil || hold.ID != got.Reservation.ID {
		t.Fatalf("reopen released hold: %+v, %v", hold, err)
	}
	if again, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, "claim", allowDependencyRead); err != nil ||
		again.Reservation != got.Reservation {
		t.Fatalf("replay after reopen changed recorded result: %+v, %v", again, err)
	}
}

func TestDependencyClaimRefusals(t *testing.T) {
	r, ownerDB, depDB, owner, dep := dependencyFixture(t)
	if err := depDB.SetMeta(TasksMetaKey, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTaskReservation(context.Background(), claimInput(owner, "disabled"), aliceActor,
		"disabled", allowDependencyRead); !errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("disabled dependency: %v", err)
	}
	if err := depDB.SetMeta(TasksMetaKey, "on"); err != nil {
		t.Fatal(err)
	}
	missing, err := ownerDB.CreateTask(TaskContent{Title: "missing", State: "READY",
		Dependencies: []string{uuidv7.New()}}, aliceActor, "missing-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTaskReservation(context.Background(), claimInput(missing, "missing"), aliceActor,
		"missing-claim", allowDependencyRead); !errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("missing dependency: %v", err)
	}
	content := dep.TaskContent
	content.Dependencies = []string{owner.ID}
	if _, err := depDB.UpdateTask(dep.ID, content, dep.Revision, aliceActor, "cycle"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTaskReservation(context.Background(), claimInput(owner, "cycle"), aliceActor,
		"cycle-claim", allowDependencyRead); !errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("dependency cycle: %v", err)
	}
}

func TestDependencyClaimDuplicateSelfAndUnavailable(t *testing.T) {
	r, ownerDB, depDB, _, dep := dependencyFixture(t)
	duplicate, err := ownerDB.CreateTask(TaskContent{Title: "duplicate", State: "READY",
		Dependencies: []string{dep.ID, dep.ID}}, aliceActor, "duplicate-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTaskReservation(context.Background(), claimInput(duplicate, "duplicate"),
		aliceActor, "duplicate-claim", allowDependencyRead); err != nil {
		t.Fatalf("duplicate IDs should count once: %v", err)
	}
	self, err := ownerDB.CreateTask(TaskContent{Title: "self", State: "READY"}, aliceActor, "self-create")
	if err != nil {
		t.Fatal(err)
	}
	content := self.TaskContent
	content.Dependencies = []string{self.ID}
	self, err = ownerDB.UpdateTask(self.ID, content, self.Revision, aliceActor, "self-update")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTaskReservation(context.Background(), claimInput(self, "self"),
		aliceActor, "self-claim", allowDependencyRead); !errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("self dependency: %v", err)
	}
	if err := depDB.sql.Close(); err != nil {
		t.Fatal(err)
	}
	unavailable, err := ownerDB.CreateTask(TaskContent{Title: "unavailable", State: "READY",
		Dependencies: []string{dep.ID}}, aliceActor, "unavailable-create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClaimTaskReservation(context.Background(), claimInput(unavailable, "unavailable"),
		aliceActor, "unavailable-claim", allowDependencyRead); !errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("unavailable partition counted as DONE: %v", err)
	}
}

func TestDependencyReopenBeforeLockedValidation(t *testing.T) {
	r, ownerDB, depDB, owner, dep := dependencyFixture(t)
	peer, err := NewRegistry(r.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerDB, err := peer.OpenExisting(depDB.projectID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := peerDB.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result := make(chan error, 1)
	go func() {
		_, err := r.ClaimTaskReservation(context.Background(), claimInput(owner, "racing"),
			aliceActor, "racing-claim", allowDependencyRead)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("claim passed held dependency write lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	reopened := dep
	reopened.State, reopened.Archived = "READY", false
	reopened.Revision++
	reopened.UpdatedAt = nowUTC()
	if err := writeTask(tx, reopened, aliceActor, false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrDependencyUnresolved) {
			t.Fatalf("stale dependency allowed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("claim did not finish after dependency commit")
	}
	if hold, err := ownerDB.GetTaskReservation(owner.ID); err != nil || hold.ID != "" {
		t.Fatalf("refused claim left hold: %+v, %v", hold, err)
	}
}

func TestDependencyClaimCancellationAndRetry(t *testing.T) {
	r, ownerDB, depDB, owner, _ := dependencyFixture(t)
	peer, err := NewRegistry(r.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerDB, err := peer.OpenExisting(depDB.projectID)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := peerDB.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = r.ClaimTaskReservation(ctx, claimInput(owner, "cancel"), aliceActor, "retry", allowDependencyRead)
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("contended claim did not honor cancellation: %v", err)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if hold, err := ownerDB.GetTaskReservation(owner.ID); err != nil || hold.ID != "" {
		t.Fatalf("cancelled claim left hold: %+v, %v", hold, err)
	}
	first, err := r.ClaimTaskReservation(context.Background(), claimInput(owner, "cancel"),
		aliceActor, "retry", allowDependencyRead)
	if err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
	if first.Reservation.ID == "" {
		t.Fatal("retry did not commit a hold")
	}
}

func TestDependencyClaimLifecycleWaitCancels(t *testing.T) {
	r, _, _, owner, _ := dependencyFixture(t)
	r.teamMu.Lock()
	defer r.teamMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := r.ClaimTaskReservation(ctx, claimInput(owner, "lifecycle-wait"),
		aliceActor, "lifecycle-wait", allowDependencyRead)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lifecycle wait did not cancel: %v", err)
	}
}

func TestDependencyLocksSurviveCancellationDuringOwnerCommit(t *testing.T) {
	r, ownerDB, depDB, _, _ := dependencyFixture(t)
	peer, err := NewRegistry(r.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerDB, err := peer.OpenExisting(depDB.projectID)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	name := "sqlite-c3-paused-" + uuidv7.New()
	sql.Register(name, &pausedCommitDriver{base: &sqlite.Driver{}, entered: entered, release: release})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	depHandle, depTx, err := beginClaimTx(ctx, depDB)
	if err != nil {
		t.Fatal(err)
	}
	defer depHandle.Close()
	defer depTx.Rollback()
	ownerHandle, ownerTx, err := beginClaimTxDriver(ctx, ownerDB, name)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerHandle.Close()
	defer ownerTx.Rollback()
	if _, err := ownerTx.Exec(`INSERT INTO meta(key,value) VALUES('c3_commit_pause','written')`); err != nil {
		t.Fatal(err)
	}
	commitResult := make(chan error, 1)
	go func() { commitResult <- ownerTx.Commit() }()
	<-entered
	cancel()
	peerResult := make(chan error, 1)
	go func() { peerResult <- peerDB.SetMeta("c3_peer", "written") }()
	select {
	case err := <-peerResult:
		t.Fatalf("dependency lock released during owner commit: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-commitResult; err != nil {
		t.Fatalf("owner commit: %v", err)
	}
	if err := depTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-peerResult:
		if err != nil {
			t.Fatalf("peer write after owner commit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dependency peer stayed blocked after rollback")
	}
}

func TestDependencyClaimPrecommitVerifierRollback(t *testing.T) {
	r, ownerDB, _, owner, _ := dependencyFixture(t)
	in := claimInput(owner, "precommit")
	calls := 0
	_, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, "precommit-claim",
		func(context.Context, string, string) error {
			calls++
			if calls >= 6 {
				return errors.New("grant revoked")
			}
			return nil
		})
	if !errors.Is(err, ErrDependencyUnresolved) || calls < 6 {
		t.Fatalf("precommit verifier did not refuse: calls=%d err=%v", calls, err)
	}
	if hold, err := ownerDB.GetTaskReservation(owner.ID); err != nil || hold.ID != "" {
		t.Fatalf("failed verification left hold: %+v, %v", hold, err)
	}
	if _, found, err := ownerDB.GetTaskReservationReceipt(ReservationClaim, in, aliceActor, "precommit-claim"); err != nil || found {
		t.Fatalf("failed verification left receipt: found=%t err=%v", found, err)
	}
	if countRows(t, ownerDB, "task_reservation_events", "task_id=?", owner.ID) != 0 {
		t.Fatal("failed verification left a claim event")
	}
	if _, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, "precommit-claim", allowDependencyRead); err != nil {
		t.Fatalf("same-key retry after rollback: %v", err)
	}
}

func TestDependencyOwnerRevisionReread(t *testing.T) {
	r, _, _, owner, _ := dependencyFixture(t)
	peer, err := NewRegistry(r.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerDB, err := peer.OpenExisting("owner")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := peerDB.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	result := make(chan error, 1)
	go func() {
		_, err := r.ClaimTaskReservation(context.Background(), claimInput(owner, "owner-race"),
			aliceActor, "owner-race", allowDependencyRead)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("claim passed held owner write lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	changed := owner
	changed.Revision++
	changed.Title = "changed while claim waits"
	changed.UpdatedAt = nowUTC()
	if err := writeTask(tx, changed, aliceActor, false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		var conflict *TaskConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("owner revision was not reread: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("claim did not finish after owner update")
	}
}

func TestDependencyClaimRenameAndLifecycleLock(t *testing.T) {
	r, ownerDB, depDB, owner, _ := dependencyFixture(t)
	if err := r.Rename("dependency", "renamed"); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	claimResult := make(chan error, 1)
	renamedChecks := 0
	go func() {
		_, err := r.ClaimTaskReservation(context.Background(), claimInput(owner, "rename"),
			aliceActor, "rename-claim", func(ctx context.Context, project, accessID string) error {
				if project == "renamed" {
					renamedChecks++
					if renamedChecks == 3 {
						close(entered)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
				return nil
			})
		claimResult <- err
	}()
	<-entered
	renameResult := make(chan error, 1)
	go func() { renameResult <- r.Rename("renamed", "renamed-again") }()
	select {
	case err := <-renameResult:
		t.Fatalf("rename bypassed claim lifecycle lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-claimResult; err != nil {
		t.Fatalf("claim after initial rename: %v", err)
	}
	if err := <-renameResult; err != nil {
		t.Fatal(err)
	}
	if changed, err := r.ReconcileReservationDependencies(context.Background(), owner.ID, allowDependencyRead); err != nil || changed {
		t.Fatalf("stable access ID should survive rename: %t, %v", changed, err)
	}
	if hold, err := ownerDB.GetTaskReservation(owner.ID); err != nil || hold.ID == "" {
		t.Fatalf("rename lost hold: %+v, %v", hold, err)
	}
	if err := r.Drop("renamed-again"); !errors.Is(err, ErrProjectHasTasks) {
		t.Fatalf("task-bearing dependency project was dropped: %v", err)
	}
	_ = depDB // rename invalidates its cached handle; claim used the locator.
}

func TestDependencyClaimReceiptAfterRestart(t *testing.T) {
	r, _, _, owner, _ := dependencyFixture(t)
	in := claimInput(owner, "restart")
	first, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, "restart-claim", allowDependencyRead)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	reopened, err := NewRegistry(r.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.ClaimTaskReservation(context.Background(), in, aliceActor, "restart-claim", allowDependencyRead)
	if err != nil || got.Reservation != first.Reservation {
		t.Fatalf("receipt after restart: %+v, %v", got, err)
	}
	if changed, err := reopened.ReconcileReservationDependencies(context.Background(), owner.ID, allowDependencyRead); err != nil || changed {
		t.Fatalf("proof after restart: %t, %v", changed, err)
	}
}

func TestDependencyReconciliationRequiresVerifiedClaimProof(t *testing.T) {
	r, ownerDB, _, _, dep := dependencyFixture(t)
	verified := readyReservationTask(t, ownerDB, "empty-verified")
	hold, err := r.ClaimTaskReservation(context.Background(), claimInput(verified, "empty"),
		aliceActor, "empty-claim", allowDependencyRead)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := r.ReconcileReservationDependencies(context.Background(), verified.ID, allowDependencyRead); err != nil || changed {
		t.Fatalf("verified empty dependencies: %t, %v", changed, err)
	}
	content := verified.TaskContent
	content.Dependencies = []string{dep.ID}
	if _, err := ownerDB.ApplyTaskReservation(ReservationUpdate, TaskReservationInput{
		TaskID: verified.ID, ID: hold.Reservation.ID, Fence: hold.Reservation.Fence,
		ExpectedRevision: verified.Revision, Content: &content,
	}, aliceActor, "holder-changed-dependencies"); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.ReconcileReservationDependencies(context.Background(), verified.ID, allowDependencyRead); !changed ||
		!errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("new dependency lacked claim proof: %t, %v", changed, err)
	}
	storeOnly := readyReservationTask(t, ownerDB, "empty-store-only")
	if _, err := ownerDB.ApplyTaskReservation(ReservationClaim, claimInput(storeOnly, "store-only"),
		aliceActor, "store-only-claim"); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.ReconcileReservationDependencies(context.Background(), storeOnly.ID, allowDependencyRead); !changed ||
		!errors.Is(err, ErrDependencyUnresolved) {
		t.Fatalf("store-only claim misrepresented as verified: %t, %v", changed, err)
	}
}
