package teamstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadClearAndNoteLeave(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	repo, err := Canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := Path(root, repo)
	if st, err := Load(path); err != nil || st != nil {
		t.Fatalf("missing state: %v %v", st, err)
	}
	st := &State{Version: 1, Repo: repo, Project: "alpha", HubName: "hub", HubURL: "https://hub.invalid", TokenID: "t-1", Team: "Pilot", Role: "worker", SessionID: "sess-1", Generation: 2}
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil || back == nil || back.SessionID != "sess-1" || back.Generation != 2 {
		t.Fatalf("%+v %v", back, err)
	}
	// A leave of another session leaves the record; a leave of this one clears it.
	NoteLeave(dir, root, "sess-9")
	if back, _ := Load(path); back == nil {
		t.Fatal("foreign leave cleared the record")
	}
	NoteLeave(dir, root, "sess-1")
	if back, _ := Load(path); back != nil {
		t.Fatal("leave did not clear the record")
	}
	if err := Clear(path); err != nil {
		t.Fatalf("clearing a missing file: %v", err)
	}
	// An unreadable file is reported, not treated as absent.
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != ErrUnreadable {
		t.Fatalf("unreadable: %v", err)
	}
	if filepath.Base(path) == "" {
		t.Fatal("empty path")
	}
}
