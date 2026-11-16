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
	grp, err := r.Open("group-shared")
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
	res := ImportGroupConfigRecord(r, ConfigRecord{
		Project: "group-shared", Key: "origin_aliases", Value: string(val),
	})
	msg, applied := res.Msg, res.Applied
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
	if res := ImportGroupConfigRecord(r, ConfigRecord{
		Project: "group-shared", Key: "origin_aliases", Value: string(val),
	}); res.Applied {
		t.Fatalf("re-import must be a no-op, got applied=true (%s)", res.Msg)
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

// A differing charter key is kept and reported through Differs; the
// message names no command, because the hub logs it on its own host.
func TestImportGroupConfigDiffers(t *testing.T) {
	r := newTestRegistry(t)
	grp, _ := r.Open("group-x")
	if err := grp.SetMeta("about", "local"); err != nil {
		t.Fatal(err)
	}
	res := ImportGroupConfigRecord(r, ConfigRecord{Project: "group-x", Key: "about", Value: "peer"})
	if !res.Differs || res.Applied || res.Msg != "group config group-x/about differs from peer (kept local)" {
		t.Fatalf("differing key: %+v", res)
	}
	if v, _ := grp.GetMeta("about"); v != "local" {
		t.Fatalf("local value replaced: %q", v)
	}
	if res := ImportGroupConfigRecord(r, ConfigRecord{Project: "group-x", Key: "about", Value: "local"}); res.Differs || res.Msg != "" {
		t.Fatalf("equal key: %+v", res)
	}
	if res := ImportGroupConfigRecord(r, ConfigRecord{Project: "group-x", Key: "policy", Value: "all"}); res.Differs || !res.Applied {
		t.Fatalf("empty key: %+v", res)
	}
}
