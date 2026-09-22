package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"aimem/internal/uuidv7"
)

func teamFixture(t *testing.T) (*Registry, *DB, TeamAuditContext) {
	t.Helper()
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	d, err := r.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SetMeta(TasksMetaKey, "on"); err != nil {
		t.Fatal(err)
	}
	return r, d, TeamAuditContext{Actor: TaskActor{Kind: "admin", Name: "operator"}, RequestID: uuidv7.New(), ServerVersion: "test"}
}

func TestTeamConfigurationAtomicity(t *testing.T) {
	r, d, a := teamFixture(t)
	c := TeamContent{Name: "Kernel team", Enrollment: []TeamEnrollment{{UserID: uuidv7.New(), Coordinator: true}}}
	first, err := r.ConfigureTeam("alpha", "", 0, c, a, "create")
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.ConfigureTeam("alpha", "", 0, c, a, "create")
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err = r.ConfigureTeam("alpha", "", 0, c, a, "duplicate"); !errors.Is(err, ErrTeamNameTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	c.Description = "changed"
	if _, err = r.ConfigureTeam("alpha", "", 0, c, a, "create"); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatalf("retry conflict: %v", err)
	}
	if _, err = d.sql.Exec(`CREATE TRIGGER reject_team_event BEFORE INSERT ON team_events BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ConfigureTeam("alpha", first.ID, 1, c, a, "update"); err == nil {
		t.Fatal("audit failure accepted")
	}
	got, err := d.GetTeam(first.ID)
	if err != nil || got.Revision != 1 || got.Description != "" {
		t.Fatalf("rollback: %+v %v", got, err)
	}
	if _, err = d.sql.Exec(`DROP TRIGGER reject_team_event`); err != nil {
		t.Fatal(err)
	}
	up, err := r.ConfigureTeam("alpha", first.ID, 1, c, a, "update")
	if err != nil || up.Revision != 2 {
		t.Fatalf("retry rollback: %+v %v", up, err)
	}
	var conflict *TeamConflict
	if _, err = r.ConfigureTeam("alpha", first.ID, 1, c, a, "stale"); !errors.As(err, &conflict) || conflict.Current.Revision != 2 {
		t.Fatalf("CAS: %v", err)
	}
	es, err := d.TeamEvents(first.ID, 0, 100)
	if err != nil || len(es) != 2 || es[0].Team.Revision != 1 || es[1].PreviousRevision != 1 || es[0].TeamAuditContext != a {
		t.Fatalf("audit: %+v %v", es, err)
	}
	d.SetMeta(TasksMetaKey, "off")
	if _, err = r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "disabled"}, a, "off"); err == nil {
		t.Fatal("disabled write")
	}
	if _, err = r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "Kernel team", Enrollment: first.Enrollment}, a, "create"); err == nil {
		t.Fatal("disabled receipt replay")
	}
}

func TestTeamConcurrentConfiguration(t *testing.T) {
	r, d, a := teamFixture(t)
	var wg sync.WaitGroup
	results := make([]Team, 12)
	errs := make([]error, 12)
	for i := range results {
		wg.Go(func() { results[i], errs[i] = r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "shared"}, a, "same") })
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].ID != results[0].ID {
			t.Fatalf("retry %d: %v", i, errs[i])
		}
	}
	for i := range results {
		wg.Go(func() {
			_, errs[i] = r.ConfigureTeam("alpha", results[0].ID, 1, TeamContent{Name: "shared", Description: fmt.Sprint(i)}, a, fmt.Sprint(i))
		})
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else {
			var conflict *TeamConflict
			if !errors.As(err, &conflict) {
				t.Fatal(err)
			}
		}
	}
	if wins != 1 {
		t.Fatalf("winners=%d", wins)
	}
	es, err := d.TeamEvents(results[0].ID, 0, 100)
	if err != nil || len(es) != 2 {
		t.Fatalf("events=%d err=%v", len(es), err)
	}
}

func TestTeamLifecycleAndBackup(t *testing.T) {
	r, _, a := teamFixture(t)
	team, err := r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "team"}, a, "c")
	if err != nil {
		t.Fatal(err)
	}
	r.Open("beta")
	if _, _, _, _, err = r.MergeProject("alpha", "beta"); !errors.Is(err, ErrProjectHasTeams) {
		t.Fatalf("merge source: %v", err)
	}
	if _, _, _, _, err = r.MergeProject("beta", "alpha"); !errors.Is(err, ErrProjectHasTeams) {
		t.Fatalf("merge target: %v", err)
	}
	if err = r.Drop("alpha"); !errors.Is(err, ErrProjectHasTeams) {
		t.Fatalf("live drop: %v", err)
	}
	if err = r.Rename("alpha", "renamed"); err != nil {
		t.Fatal(err)
	}
	d, err := r.OpenExisting("renamed")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetTeam(team.ID)
	if err != nil || got.ProjectInstance != team.ProjectInstance {
		t.Fatalf("rename: %+v %v", got, err)
	}
	r.Close()
	if err = r.Drop("renamed"); !errors.Is(err, ErrProjectHasTeams) {
		t.Fatalf("closed drop: %v", err)
	}
	backup := t.TempDir()
	dir := filepath.Join(backup, "projects", "renamed")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"journal.db", "access-id"} {
		b, e := os.ReadFile(filepath.Join(r.Root(), "projects", "renamed", name))
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	restored, err := NewRegistry(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	rd, err := restored.OpenExisting("renamed")
	if err != nil {
		t.Fatal(err)
	}
	es, err := rd.TeamEvents(team.ID, 0, 100)
	if err != nil || len(es) != 1 || es[0].Team.ProjectInstance != team.ProjectInstance {
		t.Fatalf("backup: %+v %v", es, err)
	}
}

func TestTeamMigrationAndValidation(t *testing.T) {
	r, d, a := teamFixture(t)
	for _, q := range []string{`DROP TABLE team_assignments`, `DROP TABLE team_managed_tasks`, `DROP TABLE team_deliveries`, `DROP TABLE team_messages`, `DROP TABLE team_sessions`, `DROP TABLE team_session_control`, `DROP TABLE team_events`, `DROP TABLE teams`, `UPDATE meta SET value='13' WHERE key='schema_version'`} {
		if _, err := d.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	d, err := r.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []TeamContent{{Name: ""}, {Name: " space"}, {Name: "line\nbreak"}, {Name: "x", Enrollment: []TeamEnrollment{{UserID: "bad"}}}} {
		if _, err := r.ConfigureTeam("alpha", "", 0, c, a, "bad"); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	if _, err := r.ConfigureTeam(UserScopeProject, "", 0, TeamContent{Name: "reserved"}, a, "reserved"); err == nil {
		t.Fatal("reserved scope")
	}
	if _, err := r.ConfigureTeam("missing", "", 0, TeamContent{Name: "missing"}, a, "missing"); !errors.Is(err, ErrNoSuchProject) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "ok"}, a, "ok"); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.GetMeta("schema_version"); v != fmt.Sprint(currentSchema) {
		t.Fatalf("version %s", v)
	}
}

func TestTeamCreateVersusDrop(t *testing.T) {
	for range 10 {
		r, _, a := teamFixture(t)
		var team Team
		var createErr, dropErr error
		var wg sync.WaitGroup
		wg.Go(func() { team, createErr = r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "racing"}, a, "race") })
		wg.Go(func() { dropErr = r.Drop("alpha") })
		wg.Wait()
		if createErr == nil {
			if !errors.Is(dropErr, ErrProjectHasTeams) {
				t.Fatalf("created %s but drop=%v", team.ID, dropErr)
			}
			d, err := r.OpenExisting("alpha")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.GetTeam(team.ID); err != nil {
				t.Fatal(err)
			}
		} else if dropErr != nil || !errors.Is(createErr, ErrNoSuchProject) {
			t.Fatalf("create=%v drop=%v", createErr, dropErr)
		}
	}
}
