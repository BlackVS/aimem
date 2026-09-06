package store

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMergeOriginAliases(t *testing.T) {
	// Union with local precedence, chains re-pointed, cycles bounded.
	local := map[string]string{"a": "b"}
	peer := map[string]string{"a": "c", "x": "a"}
	m := MergeOriginAliases(local, peer)
	if m["a"] != "b" {
		t.Errorf("local precedence lost: a->%s", m["a"])
	}
	if m["x"] != "b" {
		t.Errorf("chain not re-pointed: x->%s (want b via a)", m["x"])
	}
	cyc := MergeOriginAliases(map[string]string{"p": "q"}, map[string]string{"q": "p"})
	if cyc["p"] == "" || cyc["q"] == "" {
		t.Errorf("cycle handling dropped entries: %v", cyc)
	}
}

func TestOriginAliasImportHealsGhost(t *testing.T) {
	r := newTestRegistry(t)
	grp, err := r.Open("group-oboro")
	if err != nil {
		t.Fatal(err)
	}
	// The ghost: a group fact citing a project id that was merged away
	// on ANOTHER machine — this DB never ran the relabel and has no alias.
	if _, _, err := grp.Remember("shared fact", "curator", RememberOpts{
		Kind: "fact", Sources: []string{"project:gitea-old-id"},
	}); err != nil {
		t.Fatal(err)
	}

	// The peer's alias record arrives via group-config sync.
	val, _ := json.Marshal(map[string]string{"gitea-old-id": "aimem"})
	msg, applied := ImportGroupConfigRecord(r, ConfigRecord{
		Project: "group-oboro", Key: "origin_aliases", Value: string(val),
	})
	if !applied || !strings.Contains(msg, "1 citation(s) relabeled") {
		t.Fatalf("import: applied=%v msg=%q", applied, msg)
	}
	mems, err := grp.Memories(false)
	if err != nil || len(mems) != 1 {
		t.Fatal(err, mems)
	}
	var cited string
	for _, s := range mems[0].Sources {
		if strings.HasPrefix(s, "project:") {
			cited = s
		}
	}
	if cited != "project:aimem" {
		t.Fatalf("ghost not healed: cites %q", cited)
	}
	// Future imports normalize through the adopted alias too.
	if al := grp.originAliases(); al["gitea-old-id"] != "aimem" {
		t.Fatalf("alias not recorded: %v", al)
	}
	// Idempotent: the same record again changes nothing and reports so.
	if msg, applied := ImportGroupConfigRecord(r, ConfigRecord{
		Project: "group-oboro", Key: "origin_aliases", Value: string(val),
	}); applied {
		t.Fatalf("re-import must be a no-op, got applied=true (%s)", msg)
	}
}

func TestExportGroupConfigCarriesAliases(t *testing.T) {
	r := newTestRegistry(t)
	grp, _ := r.Open("group-x")
	if _, _, err := grp.ApplyOriginAliases(map[string]string{"old-p": "new-p"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := ExportGroupConfig(r, []string{"group-x"}, json.NewEncoder(&buf)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"origin_aliases"`) || !strings.Contains(buf.String(), "new-p") {
		t.Fatalf("export missing alias record: %s", buf.String())
	}
}
