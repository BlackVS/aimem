package main

import (
	"encoding/json"
	"strings"
	"testing"

	"aimem/internal/adapter"
)

func TestRenderCollectionDeterministic(t *testing.T) {
	recs := []adapter.HubRecord{
		{ID: "messages/create", Body: json.RawMessage(`{"title":"Create a message","method":"POST","description":"Sends one.","shape":{"model":"string"}}`)},
		{ID: "messages/list", Body: json.RawMessage(`{"method":"GET"}`)},
		{ID: "models/get", Body: json.RawMessage(`{"method":"GET","beta":true}`)},
	}
	out := renderCollection("api", recs, "")
	if out != renderCollection("api", recs, "") {
		t.Fatal("renderer must be deterministic")
	}
	for _, want := range []string{
		"GENERATED from hub collection \"api\"",
		"# api",
		"## messages",          // branch heading from the shared prefix
		"### Create a message", // title field wins over the leaf segment
		"Sends one.",
		"- **method**: POST",
		"- **beta**: true",
		"```json", `"model": "string"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "messages") > strings.Index(out, "models") {
		t.Error("branches must render in id order")
	}
}

func TestOpenapiRecords(t *testing.T) {
	spec := `{"paths":{
		"/v1/projects/{p}/docs": {"get": {"summary": "list docs", "x-role": "writer"}},
		"/v1/projects/{p}/docs/{name}": {"get": {"summary": "read one doc", "x-role": "writer"},
		                                 "put": {"summary": "CAS write", "x-role": "writer"}},
		"/v1/health": {"get": {"summary": "liveness", "x-role": "writer"}}
	}}`
	recs, err := openapiRecords([]byte(spec))
	if err != nil || len(recs) != 4 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
	// Sorted by id; parameters and version prefix drop out of ids; a
	// listing/item GET collision disambiguates the ITEM op with -one,
	// and the uncontested put keeps its plain id.
	want := []string{"health/get", "projects/docs/get", "projects/docs/get-one", "projects/docs/put"}
	for i, w := range want {
		if recs[i].id != w {
			t.Fatalf("ids: got %q at %d, want %q", recs[i].id, i, w)
		}
	}
	if !strings.Contains(string(recs[1].body), "list docs") || !strings.Contains(string(recs[2].body), "read one doc") {
		t.Fatalf("collision bodies swapped: %s / %s", recs[1].body, recs[2].body)
	}
	if !strings.Contains(string(recs[3].body), `"PUT"`) || !strings.Contains(string(recs[3].body), "CAS write") {
		t.Fatalf("body: %s", recs[3].body)
	}
}
