package store

// Group-config sync records (DESIGN-hub-sync): the fill-only /
// newest-wins semantics used to live only in the CLI's ssh legs;
// they are shared here so the hub's /v1/sync/group-config endpoints
// and the CLI apply EXACTLY the same rules — two implementations of
// "fill-only" would eventually disagree about what counts as empty.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// GroupConfigKeys are the meta keys that travel between peers for
// group-* projects (plus "hub" for ordinary projects, and design_doc
// with its timestamp — see ExportGroupConfig).
var GroupConfigKeys = []string{"about", "policy", "chapters", "features"}

// ConfigRecord is one line of the group-config JSONL stream.
type ConfigRecord struct {
	Project string `json:"project"`
	Key     string `json:"key"`
	Value   string `json:"value"`
	TS      string `json:"ts,omitempty"`
}

// ExportGroupConfig writes the config records for the given projects.
// Ordinary projects ship exactly one key — their hub binding — because
// a synced copy that looks unbound would be curated as local by the
// receiving hub (duplicate facts, duplicate spend). Group projects
// ship the charter keys, plus the generated design doc with its
// timestamp so the import side can apply newest-wins.
func ExportGroupConfig(reg *Registry, ids []string, enc *json.Encoder) error {
	for _, id := range ids {
		if !strings.HasPrefix(id, "group-") {
			if db, err := reg.OpenExisting(id); err == nil {
				if v, _ := db.GetMeta("hub"); v != "" {
					if err := enc.Encode(ConfigRecord{Project: id, Key: "hub", Value: v}); err != nil {
						return err
					}
				}
			}
			continue
		}
		db, err := reg.OpenExisting(id)
		if err != nil {
			continue
		}
		for _, k := range GroupConfigKeys {
			if v, _ := db.GetMeta(k); v != "" {
				if err := enc.Encode(ConfigRecord{Project: id, Key: k, Value: v}); err != nil {
					return err
				}
			}
		}
		if doc, _ := db.GetMeta("design_doc"); doc != "" {
			ts, _ := db.GetMeta("design_doc_ts")
			if err := enc.Encode(ConfigRecord{Project: id, Key: "design_doc", Value: doc, TS: ts}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ImportGroupConfigRecord applies one record with the sync semantics:
// hub bindings and charter keys are FILL-ONLY (meta rows carry no
// timestamps, so a blind overwrite could undo a newer edit; divergence
// warns and keeps local), and the generated design doc is newest-wins
// by its companion timestamp. The returned message, when non-empty, is
// for the caller's log/stderr; applied reports whether anything was
// written.
func ImportGroupConfigRecord(reg *Registry, rec ConfigRecord) (msg string, applied bool) {
	if rec.Value == "" || rec.Project == "" {
		return "", false
	}
	if !strings.HasPrefix(rec.Project, "group-") {
		if rec.Key != "hub" {
			return "", false
		}
		db, err := reg.Open(rec.Project)
		if err != nil {
			return "", false
		}
		if cur, _ := db.GetMeta("hub"); cur == "" {
			if db.SetMeta("hub", rec.Value) == nil {
				return fmt.Sprintf("%s is bound to hub %q (adopted from peer)", rec.Project, rec.Value), true
			}
		}
		return "", false
	}
	if rec.Key == "design_doc" {
		db, err := reg.Open(rec.Project)
		if err != nil {
			return "", false
		}
		if cur, _ := db.GetMeta("design_doc_ts"); rec.TS > cur {
			if db.SetMeta("design_doc", rec.Value) == nil &&
				db.SetMeta("design_doc_ts", rec.TS) == nil {
				return fmt.Sprintf("design doc %s updated from peer (%s)", rec.Project, rec.TS), true
			}
		}
		return "", false
	}
	if !slices.Contains(GroupConfigKeys, rec.Key) {
		return "", false
	}
	db, err := reg.Open(rec.Project)
	if err != nil {
		return "", false
	}
	switch cur, _ := db.GetMeta(rec.Key); {
	case cur == "":
		if db.SetMeta(rec.Key, rec.Value) == nil {
			return fmt.Sprintf("group config %s/%s adopted from peer", rec.Project, rec.Key), true
		}
	case cur != rec.Value:
		return fmt.Sprintf("group config %s/%s differs from peer (kept local; re-save with `aimem group` to republish)",
			rec.Project, rec.Key), false
	}
	return "", false
}
