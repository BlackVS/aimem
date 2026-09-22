package server

import (
	"fmt"
	"testing"

	"aimem/internal/store"
)

// An export whose message stream is empty at the snapshot stays empty on every
// later page even after a message is accepted, and still completes.
func TestAuditHTTPExportEmptyMessageSnapshot(t *testing.T) {
	f := newMessageFixture(t)
	export := f.base + "/export"
	first := parseExport(t, taskReq(t, f.h, "GET", export+"?limit=1", f.admin, "", ""))
	snapshot := first.header["snapshot"].(map[string]any)
	if snapshot["message_sequence"].(float64) != 0 || len(first.messages) != 0 || len(first.events) != 1 || first.end["complete"] != false {
		t.Fatalf("first page %+v %d %d %+v", snapshot, len(first.events), len(first.messages), first.end)
	}
	d, err := f.reg.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	audit := store.TeamAuditContext{Actor: store.TaskActor{Kind: "user", UserID: f.aliceUser, TokenID: f.aliceTokenID, Name: "alice"}, RequestID: "late", ServerVersion: "test"}
	content := store.TeamMessageContent{Kind: "note", Recipient: store.TeamRecipient{Kind: "team"}, Payload: store.TeamMessagePayload{Text: "accepted after the snapshot"}}
	if _, err := d.SendTeamMessage(first.header["team_id"].(string), f.sender, content, audit, "late", store.MaxTeamMessages); err != nil {
		t.Fatal(err)
	}
	messages, end := 0, first.end
	for pages := 0; end["complete"] != true; pages++ {
		if pages > 50 {
			t.Fatal("export never completes")
		}
		next := end["next"].(map[string]any)
		path := fmt.Sprintf("%s?limit=1&after_events=%d&after_messages=%d&snapshot_events=%d&snapshot_messages=%d", export, int64(next["after_events"].(float64)), int64(next["after_messages"].(float64)), int64(next["snapshot_events"].(float64)), int64(next["snapshot_messages"].(float64)))
		page := parseExport(t, taskReq(t, f.h, "GET", path, f.admin, "", ""))
		messages += len(page.messages)
		end = page.end
	}
	if messages != 0 {
		t.Fatal("message accepted after the snapshot leaked into the export", messages)
	}
	if fresh := parseExport(t, taskReq(t, f.h, "GET", export, f.admin, "", "")); len(fresh.messages) != 1 || fresh.end["complete"] != true {
		t.Fatalf("fresh export %d %+v", len(fresh.messages), fresh.end)
	}
}
