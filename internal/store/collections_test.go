package store

import (
	"errors"
	"strings"
	"testing"
)

func TestRecordLifecycle(t *testing.T) {
	db := openDocDB(t)

	// Create at base 0.
	r, err := db.PutRecord("api", "messages/create", []byte(`{"method":"POST"}`), "alice", 0, false)
	if err != nil || r.Rev != 1 {
		t.Fatalf("create: %+v err=%v", r, err)
	}
	// Idempotent retry: same body, any base — current rev, no error.
	r2, err := db.PutRecord("api", "messages/create", []byte(`{"method":"POST"}`), "alice", 99, false)
	if err != nil || r2.Rev != 1 {
		t.Fatalf("idempotent retry: %+v err=%v", r2, err)
	}
	// CAS advance.
	r3, err := db.PutRecord("api", "messages/create", []byte(`{"method":"POST","beta":true}`), "bob", 1, false)
	if err != nil || r3.Rev != 2 {
		t.Fatalf("advance: %+v err=%v", r3, err)
	}
	// Stale base → RecordConflict carrying current.
	_, err = db.PutRecord("api", "messages/create", []byte(`{"method":"PUT"}`), "carol", 1, false)
	var conflict *RecordConflict
	if !errors.As(err, &conflict) || conflict.Current.Rev != 2 || conflict.Current.UpdatedBy != "bob" {
		t.Fatalf("want conflict at rev 2 by bob, got %v", err)
	}
	// A second record in another branch never conflicts.
	if _, err := db.PutRecord("api", "models/get", []byte(`{"method":"GET"}`), "carol", 0, false); err != nil {
		t.Fatalf("disjoint record: %v", err)
	}

	// History: rev 1 retained.
	old, err := db.GetRecord("api", "messages/create", 1)
	if err != nil || strings.Contains(string(old.Body), "beta") {
		t.Fatalf("history rev 1: %+v err=%v", old, err)
	}

	// Listing: id order, no bodies; with bodies for render.
	list, err := db.ListRecords("api", false)
	if err != nil || len(list) != 2 || list[0].ID != "messages/create" || list[0].Body != nil {
		t.Fatalf("list: %+v err=%v", list, err)
	}
	full, err := db.ListRecords("api", true)
	if err != nil || len(full) != 2 || !strings.Contains(string(full[1].Body), "GET") {
		t.Fatalf("list with bodies: %+v err=%v", full, err)
	}
	cols, err := db.ListCollections()
	if err != nil || len(cols) != 1 || cols[0].Name != "api" || cols[0].Records != 2 {
		t.Fatalf("collections: %+v err=%v", cols, err)
	}

	// Tombstone needs the right base and keeps the row.
	if _, err := db.PutRecord("api", "models/get", nil, "carol", 1, true); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	cols, _ = db.ListCollections()
	if cols[0].Records != 1 {
		t.Fatalf("tombstone should not count as live: %+v", cols)
	}
}

func TestRecordValidation(t *testing.T) {
	db := openDocDB(t)

	cases := []struct {
		col, id, body string
	}{
		{"api", "a//b", `{}`},                        // empty segment
		{"api", "../etc", `{}`},                      // traversal shape
		{"api", strings.Repeat("a/", 9) + "x", `{}`}, // too deep
		{"api", "ok", `[1,2]`},                       // not an object
		{"api", "ok", `not json`},                    // not JSON
		{"bad name!", "ok", `{}`},                    // collection name
	}
	for _, c := range cases {
		if _, err := db.PutRecord(c.col, c.id, []byte(c.body), "x", 0, false); err == nil {
			t.Errorf("PutRecord(%q,%q,%q) should refuse", c.col, c.id, c.body)
		}
	}
	// Secret-shaped content refuses.
	if _, err := db.PutRecord("api", "ok", []byte(`{"k":"-----BEGIN OPENSSH PRIVATE KEY-----"}`), "x", 0, false); err == nil {
		t.Error("secret-shaped record should refuse")
	}
}
