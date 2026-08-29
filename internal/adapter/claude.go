// Claude Code adapter: converts a Stop/StopFailure hook payload plus the
// session transcript JSONL into one normalized turn event. Reads the
// transcript file only — never Claude Code's internal state.
package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"aimem/internal/ident"
	"aimem/internal/schema"
)

// ClaudeHookPayload is the subset of the hook stdin JSON the adapter needs.
type ClaudeHookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Trigger        string `json:"trigger"` // PreCompact: "manual" | "auto"
}

// transcriptEntry is one line of the session JSONL (lenient subset).
type transcriptEntry struct {
	Type    string `json:"type"`
	UUID    string `json:"uuid"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"` // tool_use
}

// BuildClaudeEvent parses the hook payload and transcript into a Payload.
func BuildClaudeEvent(raw []byte) (*Payload, error) {
	// PowerShell 5.1 pipes prepend a UTF-8 BOM; tolerate it so manual
	// hook invocations on Windows work the same as real ones.
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var hp ClaudeHookPayload
	if err := json.Unmarshal(raw, &hp); err != nil {
		return nil, fmt.Errorf("bad hook payload: %w", err)
	}
	if hp.SessionID == "" || hp.TranscriptPath == "" {
		return nil, errors.New("hook payload missing session_id or transcript_path")
	}
	// The Stop hook can fire before Claude Code flushes the final assistant
	// entry to the transcript; retry briefly until it appears.
	var userReq, reply string
	var tools []string
	var turnID, lastAssistant string
	var err error
	for attempt := range 6 {
		if attempt > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		userReq, reply, tools, turnID, lastAssistant, err = parseTranscript(hp.TranscriptPath)
		if err != nil {
			return nil, err
		}
		if turnID != "" {
			// On tool-using turns the tool_use entries provide turnID
			// before the final text block is flushed; a Stop hook must
			// also wait for the reply or it journals an empty response.
			// (Failed or compacting turns may legitimately have none.)
			if reply != "" || hp.HookEventName != "Stop" {
				break
			}
			continue
		}
		// PreCompact can fire right after a user prompt (e.g. /compact)
		// opens a turn with no assistant reply yet; anchor to the previous
		// assistant message instead of waiting for one that never comes.
		if hp.HookEventName == "PreCompact" && lastAssistant != "" {
			turnID = lastAssistant
			break
		}
	}
	if turnID == "" {
		turnID = fmt.Sprintf("no-assistant-%d", time.Now().Unix())
	}
	outcome := schema.OutcomeOK
	kind := schema.KindTurn
	switch hp.HookEventName {
	case "StopFailure":
		outcome, kind = schema.OutcomeFailed, schema.KindFailure
	case "PreCompact":
		// Compaction marker: anchored to the last assistant turn so a
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
			IdempotencyKey: "claude-code:" + hp.SessionID + ":" + turnID,
			Client:         "claude-code",
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

// parseTranscript scans the JSONL for the last completed turn: the last real
// user prompt (string content or text items — tool_result carriers are not
// prompts), the assistant text after it, and the tools used in between. The
// last assistant entry's uuid becomes the stable turn id.
func parseTranscript(path string) (userReq, reply string, tools []string, turnID, lastAssistant string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", nil, "", "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // transcript lines can be huge
	type turnState struct {
		user   string
		reply  string
		tools  []string
		turnID string
	}
	var cur turnState
	for sc.Scan() {
		var e transcriptEntry
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // tolerate unknown/corrupt lines
		}
		switch e.Type {
		case "user":
			if text, isPrompt := promptText(e.Message.Content); isPrompt {
				cur = turnState{user: text} // new turn begins
			}
		case "assistant":
			var items []contentItem
			if json.Unmarshal(e.Message.Content, &items) != nil {
				continue
			}
			for _, it := range items {
				switch it.Type {
				case "text":
					if it.Text != "" {
						cur.reply = it.Text // keep last text block of the turn
					}
				case "tool_use":
					if it.Name != "" {
						cur.tools = append(cur.tools, it.Name)
					}
				}
			}
			if e.UUID != "" {
				cur.turnID = e.UUID
				lastAssistant = e.UUID
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", nil, "", "", err
	}
	return cur.user, cur.reply, cur.tools, cur.turnID, lastAssistant, nil
}

// promptText extracts a human prompt from a user entry's content, reporting
// false for tool_result carrier entries.
func promptText(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, s != ""
	}
	var items []contentItem
	if json.Unmarshal(raw, &items) != nil {
		return "", false
	}
	text := ""
	for _, it := range items {
		switch it.Type {
		case "tool_result":
			return "", false
		case "text":
			if text != "" {
				text += "\n"
			}
			text += it.Text
		}
	}
	return text, text != ""
}
