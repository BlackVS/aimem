// Package schema defines the versioned normalized event record that client
// adapters submit to the service. The schema is versioned so stored events
// outlive code changes; readers must tolerate unknown future fields.
package schema

import (
	"errors"
	"fmt"
	"regexp"
)

// Version is the current event schema version.
const Version = 1

// Event kinds.
const (
	KindTurn             = "turn"
	KindFailure          = "failure"
	KindCompactionMarker = "compaction-marker"
)

// Outcomes.
const (
	OutcomeOK            = "ok"
	OutcomeFailed        = "failed"
	OutcomePreCompaction = "pre-compaction"
)

// Event is one completed-turn checkpoint (or failure/compaction marker).
// All content fields are sanitized and size-capped by the service before
// persistence; adapters are expected to redact before sending as well.
type Event struct {
	SchemaVersion  int      `json:"schema_version"`
	IdempotencyKey string   `json:"idempotency_key"` // client:session:turn, stable across hook re-fires
	Client         string   `json:"client"`          // "opencode" | "claude-code" | future adapters
	SessionID      string   `json:"session_id"`
	TurnID         string   `json:"turn_id"`
	Kind           string   `json:"kind"`
	Outcome        string   `json:"outcome"`
	TS             string   `json:"ts"` // RFC3339, adapter-observed turn completion time
	UserRequest    string   `json:"user_request,omitempty"`
	AssistantReply string   `json:"assistant_response,omitempty"`
	ToolSummary    []string `json:"tool_summary,omitempty"`
	TouchedPaths   []string `json:"touched_paths,omitempty"`
	GitBranch      string   `json:"git_branch,omitempty"`
	GitStatus      string   `json:"git_status,omitempty"`
	HandoffPath    string   `json:"handoff_path,omitempty"`
	HandoffHash    string   `json:"handoff_hash,omitempty"`
	ParentEventID  string   `json:"parent_event_id,omitempty"`
}

var projectIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ValidProjectID reports whether id is safe to use as a directory name under
// the state root. Rejects path separators, dot-dot, and empty ids outright.
func ValidProjectID(id string) bool {
	return projectIDRe.MatchString(id) && id != "." && id != ".."
}

// Validate checks the fields the store cannot default.
func (e *Event) Validate() error {
	if e.SchemaVersion != Version {
		return fmt.Errorf("unsupported schema_version %d (want %d)", e.SchemaVersion, Version)
	}
	if e.IdempotencyKey == "" || e.Client == "" || e.SessionID == "" || e.TurnID == "" {
		return errors.New("idempotency_key, client, session_id, and turn_id are required")
	}
	switch e.Kind {
	case KindTurn, KindFailure, KindCompactionMarker:
	default:
		return fmt.Errorf("unknown kind %q", e.Kind)
	}
	switch e.Outcome {
	case OutcomeOK, OutcomeFailed, OutcomePreCompaction:
	default:
		return fmt.Errorf("unknown outcome %q", e.Outcome)
	}
	if e.TS == "" {
		return errors.New("ts is required")
	}
	return nil
}
