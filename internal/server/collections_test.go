package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRecordEndpoints: the CAS unit is the record — disjoint writers
// never conflict, a stale write 409s with the whole current record, and
// slash-path ids route through the {id...} wildcard intact.
func TestRecordEndpoints(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()

	// Create two records in different branches.
	w := req(t, h, "PUT", "/v1/projects/proj-col/collections/api/records/messages/create",
		`{"body":{"method":"POST","path":"/v1/messages"},"base_rev":0,"updated_by":"a1"}`)
	if w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	w = req(t, h, "PUT", "/v1/projects/proj-col/collections/api/records/models/get",
		`{"body":{"method":"GET"},"base_rev":0,"updated_by":"a2"}`)
	if w.Code != 200 {
		t.Fatalf("disjoint create: %d %s", w.Code, w.Body)
	}

	// Read back through the wildcard path.
	w = req(t, h, "GET", "/v1/projects/proj-col/collections/api/records/messages/create", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/v1/messages") {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}

	// Stale write 409s with the current record body.
	w = req(t, h, "PUT", "/v1/projects/proj-col/collections/api/records/messages/create",
		`{"body":{"method":"PUT"},"base_rev":0,"updated_by":"late"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"method":"POST"`) {
		t.Fatalf("stale write: %d %s", w.Code, w.Body)
	}

	// Listing: id order, sizes not bodies; ?bodies=1 for render.
	w = req(t, h, "GET", "/v1/projects/proj-col/collections/api/records", "")
	var list struct {
		Records []struct {
			ID   string          `json:"id"`
			Body json.RawMessage `json:"body"`
		} `json:"records"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Records) != 2 {
		t.Fatalf("list: %s err=%v", w.Body, err)
	}
	if list.Records[0].ID != "messages/create" || len(list.Records[0].Body) > 0 {
		t.Fatalf("list order/bodies: %+v", list.Records)
	}
	w = req(t, h, "GET", "/v1/projects/proj-col/collections/api/records?bodies=1", "")
	if !strings.Contains(w.Body.String(), `"method":"GET"`) {
		t.Fatalf("bodies=1: %s", w.Body)
	}

	// Collections listing.
	w = req(t, h, "GET", "/v1/projects/proj-col/collections", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"api"`) {
		t.Fatalf("collections: %d %s", w.Code, w.Body)
	}

	// Tombstone with CAS.
	w = req(t, h, "DELETE", "/v1/projects/proj-col/collections/api/records/models/get?base_rev=1&by=a2", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"deleted":true`) {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}

	// A non-object body refuses.
	w = req(t, h, "PUT", "/v1/projects/proj-col/collections/api/records/bad",
		`{"body":[1,2],"base_rev":0}`)
	if w.Code != 400 {
		t.Fatalf("non-object body: %d %s", w.Code, w.Body)
	}
}
