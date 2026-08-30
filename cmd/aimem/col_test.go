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
		"/v1/projects/{p}/docs/{name}": {"put": {"summary": "CAS write", "x-role": "writer"}},
		"/v1/health": {"get": {"summary": "liveness", "x-role": "writer"}}
	}}`
	recs, err := openapiRecords([]byte(spec))
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
	// Sorted by id; parameters and version prefix drop out of ids.
	if recs[0].id != "health/get" || recs[1].id != "projects/docs/put" {
		t.Fatalf("ids: %q %q", recs[0].id, recs[1].id)
	}
	if !strings.Contains(string(recs[1].body), `"PUT"`) || !strings.Contains(string(recs[1].body), "CAS write") {
		t.Fatalf("body: %s", recs[1].body)
	}
}
