package llmrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExitRefused is the exit status of an aimem command that stopped at a
// Refusal. No other aimem command uses 6, so the hub's curation script
// can tell this failure from any other.
const ExitRefused = 6

// Refusal is a provider answer that waiting cannot cure: the account has
// no credit or quota left, or the provider refuses the key. It is not a
// rate block, so it is never retried and never widens the call spacing;
// the administrator has to act on the provider account.
type Refusal struct {
	Status  int    // the HTTP status; 0 for the claude CLI
	Kind    string // the provider's error type or code, or what was recognised
	Message string // the provider's message, clipped
}

func (r *Refusal) Error() string {
	where := "claude CLI"
	if r.Status != 0 {
		where = fmt.Sprintf("HTTP %d", r.Status)
	}
	return fmt.Sprintf("provider refused the account (%s, %s): %s", where, r.Kind, r.Message)
}

// Line is what a command prints when it stops at the refusal.
func (r *Refusal) Line() string {
	return "aimem: " + strings.TrimRight(r.Error(), ". ") + ". Waiting will not cure this: the administrator must restore the provider account's credit or key."
}

// AsRefusal finds a Refusal in err's chain.
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	ok := errors.As(err, &r)
	return r, ok
}

// Refused reads an OpenAI-compatible answer and returns the Refusal it
// carries, or nil. A 2xx answer is never one. Recognised:
//   - a JSON error whose type or code begins "insufficient" (OpenAI's
//     insufficient_quota) or whose message names insufficient_quota (the
//     form a LiteLLM proxy wraps it in), at any status, 429 included;
//   - HTTP 402, payment required;
//   - HTTP 401, and HTTP 403 with a JSON error: the key is refused. A 403
//     without one is a block page and stays a rate block.
func Refused(status int, body []byte) *Refusal {
	if status/100 == 2 {
		return nil
	}
	var out struct {
		Error json.RawMessage `json:"error"`
	}
	var e struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	}
	jsonErr := false
	if json.Unmarshal(body, &out) == nil && len(out.Error) > 0 && string(out.Error) != "null" {
		jsonErr = true
		if json.Unmarshal(out.Error, &e) != nil {
			// Some proxies send the error as a bare string.
			_ = json.Unmarshal(out.Error, &e.Message)
		}
	}
	var code string
	_ = json.Unmarshal(e.Code, &code)
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	refusal := func(kind string) *Refusal {
		return &Refusal{Status: status, Kind: kind, Message: Clip(oneLine(msg), 200)}
	}
	for _, k := range []string{e.Type, code} {
		if strings.HasPrefix(strings.ToLower(k), "insufficient") {
			return refusal(k)
		}
	}
	switch {
	case jsonErr && strings.Contains(strings.ToLower(e.Message), "insufficient_quota"):
		return refusal("insufficient_quota")
	case status == 402:
		return refusal("payment required")
	case status == 401, status == 403 && jsonErr:
		return refusal("key refused")
	}
	return nil
}

// RefusedCLI reads a claude CLI failure. Its usage limit resets with the
// subscription window, so waiting cures it and it stays a rate block;
// an exhausted credit balance or a refused key do not.
func RefusedCLI(diag string) *Refusal {
	low := strings.ToLower(diag)
	for _, s := range []string{"credit balance is too low", "invalid api key"} {
		if strings.Contains(low, s) {
			return &Refusal{Kind: s, Message: Clip(oneLine(diag), 200)}
		}
	}
	return nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// The kinds of call a refusal is recorded for. Each kind keeps its own
// record, because one provider account can refuse completions and still
// serve embeddings: a success of one kind never clears another's.
const (
	CallCompletions = "completions"
	CallEmbeddings  = "embeddings"
	CallTextCheck   = "text_check"
	CallClaudeCLI   = "claude_cli"
)

// keyRefused reports a refusal of the key itself. A provider's message
// for it can quote part of the key, so the record keeps only the status
// and the kind; the journal line keeps the provider's text.
func (r *Refusal) keyRefused() bool {
	return r.Kind == "key refused" || r.Kind == "invalid api key"
}

// summary is what the record for health says: the status, the kind and,
// unless the key was refused, the provider's clipped message.
func (r *Refusal) summary() string {
	if !r.keyRefused() {
		return r.Error()
	}
	where := "claude CLI"
	if r.Status != 0 {
		where = fmt.Sprintf("HTTP %d", r.Status)
	}
	return fmt.Sprintf("provider refused the account (%s, %s)", where, r.Kind)
}

// Refuse records the refusal for health under its kind of call: its time
// and summary, nothing of the request. The call spacing is left as it
// is. The next call of the same kind that succeeds clears it.
func Refuse(call string, r *Refusal) {
	mu.Lock()
	defer mu.Unlock()
	loadLocked()
	if refusals == nil {
		refusals = map[string]*refusalRecord{}
	}
	refusals[call] = &refusalRecord{At: time.Now().UTC().Format(time.RFC3339), Message: r.summary()}
	saveLocked()
}

// Succeeded clears the refusal recorded for a kind of call. A client
// calls it only once a call has returned its answer: an error object, an
// empty answer or a failed decode is no success, even when it is not a
// rate block.
func Succeeded(call string) {
	mu.Lock()
	defer mu.Unlock()
	loadLocked()
	if _, ok := refusals[call]; ok {
		delete(refusals, call)
		saveLocked()
	}
}

type refusalRecord struct {
	At      string `json:"at"`
	Message string `json:"message"`
}

// refusalStatus lists the records for health, one per kind, in a fixed
// order. The caller holds mu.
func refusalStatus() []map[string]string {
	var out []map[string]string
	for _, k := range []string{CallCompletions, CallEmbeddings, CallTextCheck, CallClaudeCLI} {
		if r := refusals[k]; r != nil {
			out = append(out, map[string]string{"kind": k, "at": r.At, "message": r.Message})
		}
	}
	return out
}
