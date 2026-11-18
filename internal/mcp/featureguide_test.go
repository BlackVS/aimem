package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"aimem/internal/builtintext"
)

// The feature guide is read through team_context with section=features, on
// a local facade that has nothing else: no hub, credential or network.
func TestFeatureGuideThroughTeamContext(t *testing.T) {
	s := &srv{
		api:       &http.Client{Transport: failTransport{t}},
		taskState: taskStateEnabled,
		local:     &localCheckout{dir: t.TempDir(), root: t.TempDir()},
		taskSetup: func() (TaskCallFunc, error) { t.Fatal("task credential resolved"); return nil, nil },
	}
	var call struct {
		Result map[string]any `json:"result"`
	}
	json.Unmarshal(s.handle(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"team_context","arguments":{"section":"features"}}}`)), &call)
	text, isErr := toolText(call.Result)
	want, _, _ := builtintext.Section(builtintext.FeatureGuideSection, "")
	if isErr || text != want {
		t.Fatalf("team_context section=features: %v %.300s", isErr, text)
	}
	// The team unit's index names the read.
	json.Unmarshal(s.handle(t.Context(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"team_context","arguments":{}}}`)), &call)
	if index, _ := toolText(call.Result); !strings.Contains(index, "section=features returns the guide to a goal's features") {
		t.Fatalf("the index does not name the guide: %.300s", index)
	}
}

// Every configuration that lists a feature tool lists team_context too,
// except a team member's, whose pointer says the read is there only where
// it is listed; the three feature tools and team_context name the read.
func TestFeatureToolsPointToTheGuide(t *testing.T) {
	for _, c := range inventoryConfigs {
		listed := map[string]string{}
		for _, raw := range listedTools(t, c.srv(t)) {
			var d struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			listed[d.Name] = d.Description
		}
		feature := false
		for n := range featureTools {
			if desc, ok := listed[n]; ok {
				feature = true
				if !strings.Contains(desc, featureGuidePointer) {
					t.Errorf("%s: %s does not point to the guide", c.name, n)
				}
			}
		}
		desc, guide := listed["team_context"]
		if feature && !guide && c.name != "team member" {
			t.Errorf("%s lists a feature tool but not team_context", c.name)
		}
		if guide && !strings.Contains(desc, "section="+builtintext.FeatureGuideSection) {
			t.Errorf("%s: team_context does not name the guide", c.name)
		}
	}
	if !strings.Contains(featureGuidePointer, "team_context section="+builtintext.FeatureGuideSection) {
		t.Fatal("the pointer does not name the read")
	}
}
