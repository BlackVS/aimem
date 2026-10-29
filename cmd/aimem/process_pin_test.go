package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"aimem/internal/processctx"
)

// The JSON is the six fields an offer reads, under these names.
func TestPrintPin(t *testing.T) {
	pin := &processctx.Pin{Project: "alpha", Repository: "https://example.com/p.git", Commit: strings.Repeat("ab", 20),
		Manifest: "p/manifest.json", SetDigest: strings.Repeat("cd", 32), InstructionDigest: "sha256:" + strings.Repeat("ef", 32)}
	var b bytes.Buffer
	if err := printPin(&b, pin, true); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(b.Bytes(), &got); err != nil || len(got) != 6 || strings.Count(b.String(), "\n") != 1 {
		t.Fatalf("json: %v %q", err, b.String())
	}
	for k, v := range map[string]string{"project": pin.Project, "repository": pin.Repository, "commit": pin.Commit,
		"manifest": pin.Manifest, "set_digest": pin.SetDigest, "instruction_digest": pin.InstructionDigest} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	b.Reset()
	if err := printPin(&b, pin, false); err != nil || !strings.Contains(b.String(), "instruction_digest  sha256:") || !strings.Contains(b.String(), "commit              "+pin.Commit) {
		t.Fatalf("text: %v\n%s", err, b.String())
	}
}
