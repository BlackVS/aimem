// Codex CLI adapter: converts a Stop/PreCompact hook payload plus the
// session rollout JSONL into one normalized turn event. Codex's hook
// wire format deliberately mirrors Claude Code's (same stdin JSON, same
// SessionStart additionalContext shape — verified live against
// codex-cli 0.153), so this adapter mirrors claude.go: read the rollout
// file only, never Codex's internal state.
package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"aimem/internal/ident"
	"aimem/internal/schema"
)

// CodexHookPayload is the subset of the hook stdin JSON the adapter
// needs. Unlike Claude Code, Codex hands us the turn id directly, so
// the rollout is only mined for the prompt, the reply, and tool names.
type CodexHookPayload struct {
	SessionID            string `json:"session_id"`
	TurnID               string `json:"turn_id"`
	TranscriptPath       string `json:"transcript_path"`
	CWD                  string `json:"cwd"`
	HookEventName        string `json:"hook_event_name"`
	LastAssistantMessage string `json:"last_assistant_message"`
	Trigger              string `json:"trigger"` // PreCompact, when Codex sends one
}

// rolloutLine is one line of the session rollout JSONL (lenient subset).
type rolloutLine struct {
	Type    string          `json:"type"` // session_meta | event_msg | response_item | ...
	Payload json.RawMessage `json:"payload"`
}

// rolloutEvent covers the event_msg payloads we care about.
type rolloutEvent struct {
	Type             string `json:"type"` // task_started | item_completed | task_complete
	TurnID           string `json:"turn_id"`
	LastAgentMessage string `json:"last_agent_message"` // task_complete
	Item             struct {
		Type    string `json:"type"` // UserMessage | AgentMessage | ...
		Content []struct {
			Type string `json:"type"` // "text" (user) / "Text" (agent)
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item"`
}

// rolloutResponseItem covers the response_item payloads that name tools.
type rolloutResponseItem struct {
	Type string `json:"type"` // function_call | custom_tool_call | local_shell_call | web_search_call
	Name string `json:"name"`
}

// BuildCodexEvent parses the hook payload and rollout into a Payload.
func BuildCodexEvent(raw []byte) (*Payload, error) {
	// Same BOM tolerance as the Claude adapter: PowerShell 5.1 pipes
	// prepend one, and manual replays on Windows should still work.
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var hp CodexHookPayload
	if err := json.Unmarshal(raw, &hp); err != nil {
		return nil, fmt.Errorf("bad hook payload: %w", err)
	}
	if hp.SessionID == "" {
		return nil, errors.New("hook payload missing session_id")
	}
	// The Stop hook can fire before Codex flushes the turn's trailing
	// rollout lines (tool response_items, the final AgentMessage,
	// task_complete); retry briefly until the turn's task_complete
	// appears — the one line that marks the turn fully written. A
	// rollout that stays unreadable or incomplete DEGRADES the event
	// to what the payload alone carries instead of losing the turn:
	// the hook payload has session, turn, and the final reply, and the
	// journal's fail-open contract prefers a thin event over a hole.
	// transcript_path is nullable in Codex's wire format; without it the
	// payload-only degradation below is all there is.
	var userReq, reply string
	var tools []string
	var lastTurnID string
	var turnDone bool
	for attempt := 0; hp.TranscriptPath != "" && attempt < 6; attempt++ {
		if attempt > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		var perr error
		userReq, reply, tools, lastTurnID, turnDone, perr = parseRollout(hp.TranscriptPath, hp.TurnID)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "aimem submit-codex: rollout %s: %v (journaling from hook payload only)\n",
				hp.TranscriptPath, perr)
			userReq, reply, tools, lastTurnID = "", "", nil, ""
			break
		}
		if turnDone || hp.HookEventName != "Stop" {
			break
		}
	}
	// The payload's own last_assistant_message is authoritative for the
	// reply — the rollout may not have flushed the final AgentMessage yet.
	if hp.LastAssistantMessage != "" {
		reply = hp.LastAssistantMessage
	}
	turnID := hp.TurnID
	if turnID == "" {
		// PreCompact (and future events) may omit turn_id; anchor to the
		// rollout's last known turn so the marker stays idempotent.
		turnID = lastTurnID
	}
	if turnID == "" {
		// Still nothing (empty or unreadable rollout): a CONSTANT
		// fallback keeps the idempotency key stable across hook
		// re-fires — a clock-derived id would mint a fresh key each
		// time and defeat dedup exactly on the re-fire path.
		turnID = "no-turn"
	}
	outcome := schema.OutcomeOK
	kind := schema.KindTurn
	if hp.HookEventName == "PreCompact" {
		// Compaction marker: anchored to the compaction point's turn so a
		// re-fired hook stays idempotent (one marker per compaction point).
		outcome, kind = schema.OutcomePreCompaction, schema.KindCompactionMarker
		turnID += "-compact"
		userReq = "compaction trigger: " + hp.Trigger
		reply = ""
		tools = nil
	}
	dir := hp.CWD
	if dir == "" {
		dir = "."
	}
	pid, err := ident.ProjectID(dir)
	if err != nil {
		return nil, err
	}
	return &Payload{
		ProjectID:  pid,
		ProjectDir: dir,
		Event: schema.Event{
			SchemaVersion:  schema.Version,
			IdempotencyKey: "codex:" + hp.SessionID + ":" + turnID,
			Client:         "codex",
			SessionID:      hp.SessionID,
			TurnID:         turnID,
			Kind:           kind,
			Outcome:        outcome,
			TS:             time.Now().UTC().Format(time.RFC3339),
			UserRequest:    userReq,
			AssistantReply: reply,
			ToolSummary:    tools,
			GitBranch:      ident.GitBranch(dir),
		},
	}, nil
}

// parseRollout scans the rollout JSONL for the turn named by turnID (or,
// with an empty turnID, the last turn present): the user prompt and agent
// reply come from item_completed events, which carry the turn id; tool
// names come from the response_item lines between that turn's
// task_started and the next one, because response_item lines carry no
// turn id of their own. turnDone reports whether the wanted turn's
// task_complete was seen — the signal that its lines are fully flushed.
func parseRollout(path, turnID string) (userReq, reply string, tools []string, lastTurnID string, turnDone bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", nil, "", false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // rollout lines can be huge
	inTurn := false                                  // between the wanted turn's task_started and the next task_started
	var curTools []string
	for sc.Scan() {
		var line rolloutLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue // tolerate unknown/corrupt lines
		}
		switch line.Type {
		case "event_msg":
			var ev rolloutEvent
			if json.Unmarshal(line.Payload, &ev) != nil {
				continue
			}
			switch ev.Type {
			case "task_started":
				if ev.TurnID != "" {
					lastTurnID = ev.TurnID
				}
				wanted := turnID == "" || ev.TurnID == turnID
				if wanted {
					curTools = nil // a retried turn restarts its tool list
				}
				inTurn = wanted
			case "item_completed":
				if turnID != "" && ev.TurnID != turnID {
					continue
				}
				text := ""
				for _, c := range ev.Item.Content {
					// User items use "text", agent items "Text".
					if strings.EqualFold(c.Type, "text") && c.Text != "" {
						if text != "" {
							text += "\n"
						}
						text += c.Text
					}
				}
				switch ev.Item.Type {
				case "UserMessage":
					if text != "" {
						userReq = text
					}
				case "AgentMessage":
					if text != "" {
						reply = text // keep the turn's last agent message
					}
				}
			case "task_complete":
				if turnID == "" || ev.TurnID == turnID {
					turnDone = true
					if ev.LastAgentMessage != "" {
						reply = ev.LastAgentMessage
					}
				}
			}
		case "response_item":
			if !inTurn {
				continue
			}
			var ri rolloutResponseItem
			if json.Unmarshal(line.Payload, &ri) != nil {
				continue
			}
			switch ri.Type {
			case "function_call", "custom_tool_call", "local_shell_call", "web_search_call":
				name := ri.Name
				if name == "" {
					name = ri.Type
				}
				curTools = append(curTools, name)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", nil, "", false, err
	}
	return userReq, reply, curTools, lastTurnID, turnDone, nil
}
