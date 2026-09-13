package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestProjectAccessIdentityLifecycle(t *testing.T) {
	r := newTestRegistry(t)
	if _, err := r.ProjectAccessID("missing"); err == nil {
		t.Fatal("created identity for missing project")
	}
	if _, err := r.Open("original"); err != nil {
		t.Fatal(err)
	}
	id, err := r.ProjectAccessID("original")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Rename("original", "renamed"); err != nil {
		t.Fatal(err)
	}
	renamed, err := r.ProjectAccessID("renamed")
	if err != nil || renamed != id {
		t.Fatalf("rename lost identity: %s %v", renamed, err)
	}
	if err := r.Drop("renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open("renamed"); err != nil {
		t.Fatal(err)
	}
	recreated, err := r.ProjectAccessID("renamed")
	if err != nil || recreated == id {
		t.Fatalf("recreated project inherited access: %s %v", recreated, err)
	}
	if _, err := r.ProjectAccessID("group-test"); err == nil {
		t.Fatal("knowledge group became access project")
	}
}

func TestProjectAccessConcurrentCreators(t *testing.T) {
	r := newTestRegistry(t)
	if _, err := r.Open("shared"); err != nil {
		t.Fatal(err)
	}
	const workers = 16
	ids := make([]string, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			other, err := NewRegistry(r.Root())
			if err != nil {
				errs[i] = err
				return
			}
			defer other.Close()
			ids[i], errs[i] = other.ProjectAccessID("shared")
		})
	}
	wg.Wait()
	for i := range workers {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("creator %d: %q %v", i, ids[i], errs[i])
		}
	}
	leftovers, err := filepath.Glob(filepath.Join(r.Root(), "projects", "shared", ".access-id-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v %v", leftovers, err)
	}
	// Never silently replace a damaged existing identity and orphan its grants.
	path := filepath.Join(r.Root(), "projects", "shared", "access-id")
	if err := os.WriteFile(path, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProjectAccessID("shared"); err == nil {
		t.Fatal("damaged identity was accepted")
	}
	if got, _ := os.ReadFile(path); string(got) != "partial" {
		t.Fatal("damaged identity was replaced")
	}
}
