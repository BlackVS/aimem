package mcp

// Every MCP tool that stores free text a person or a later session reads
// carries one fixed sentence in its description, pointing to writing_rule
// (docs/WRITING-PERSISTED-TEXT.md). The description is what a model reads
// when it chooses the tool, so the rule is in front of it at the moment it
// writes, on every client and facade. writing_test.go classifies every
// listed tool, so a new write tool cannot ship without the sentence.

// keptTextNote is the sentence each write tool's description ends with.
const keptTextNote = "Write free text as complete, normally spaced sentences a reader new to this work can follow; keep code, paths and IDs exact (full rule: writing_rule)."

// keptTextTools store free text: task fields and comments, documents,
// memories, records, team messages, rationales, reasons, summaries and
// evidence.
var keptTextTools = map[string]bool{
	"remember": true, "update_doc": true, "put_record": true,
	"create_task": true, "update_task": true, "add_task_comment": true, "create_epic": true, "update_epic": true,
	"team_send": true, "team_offer": true, "team_decline": true, "team_withdraw": true, "team_block": true,
	"team_resume_work": true, "team_cancel": true, "team_stopped": true, "team_close_stop": true,
	"team_submit": true, "team_review": true, "team_handoff": true, "team_edit": true, "team_finalize": true,
	"task_reservation_claim": true, "task_reservation_transfer": true, "task_reservation_update": true,
	"task_reservation_release": true, "task_reservation_finalize": true,
}

func init() {
	for _, defs := range [][]map[string]any{toolDefs, taskToolDefs} {
		for _, d := range defs {
			if keptTextTools[d["name"].(string)] {
				d["description"] = d["description"].(string) + " " + keptTextNote
			}
		}
	}
}
