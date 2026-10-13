package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"aimem/internal/ident"
	"aimem/internal/redact"
	"aimem/internal/store"
	"aimem/internal/teamsession"
)

const (
	// failureRecallHits is the most memories one failure brings back.
	failureRecallHits = 3
	// failureRecallHitBytes bounds each memory's text, and
	// failureRecallMaxBytes the whole additional context.
	failureRecallHitBytes = 400
	failureRecallMaxBytes = 1500
	// failureQueryMaxTerms and failureQueryBytes bound the recall query.
	failureQueryMaxTerms = 20
	failureQueryBytes    = 240
	// failureRecallTimeout bounds the whole lookup: the hook must never
	// hold the agent up.
	failureRecallTimeout = 2 * time.Second
)

// failureRecallCmd is Claude Code's PostToolUseFailure hook for the Bash
// tool. A command that exited non-zero fires it with the exit line and the
// command's output in "error". It recalls the project's and the user's
// memories with a short, normalised form of that error and returns at most
// failureRecallHits of them as additionalContext, so the agent sees a known
// remedy (an SSH flag, a credential's location) before it reports the
// failure as a blocker. It is silent, and always exits 0, when the input is
// not a failed Bash command, when nothing relevant matches, when the local
// service is not running, and in a team conversation.
func failureRecallCmd() error {
	if teamsession.Active() {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return nil
	}
	b = []byte(strings.TrimPrefix(string(b), "\xef\xbb\xbf"))
	var ev struct {
		Event       string `json:"hook_event_name"`
		Tool        string `json:"tool_name"`
		Error       string `json:"error"`
		IsInterrupt bool   `json:"is_interrupt"`
		Cwd         string `json:"cwd"`
	}
	if json.Unmarshal(b, &ev) != nil || ev.Event != "PostToolUseFailure" || ev.Tool != "Bash" || ev.IsInterrupt {
		return nil
	}
	terms := failureQueryTerms(ev.Error)
	if len(terms) == 0 {
		return nil
	}
	dir := ev.Cwd
	if dir == "" {
		dir = "."
	}
	var scopes []string
	if p, err := ident.ProjectID(dir); err == nil {
		scopes = append(scopes, p)
	}
	scopes = append(scopes, store.UserScopeProject)
	ctx := failureRecallContext(terms, recallScopes(scopes, strings.Join(terms, " ")))
	if ctx == "" {
		return nil
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "PostToolUseFailure",
		"additionalContext": ctx,
	}})
	fmt.Println(string(out))
	return nil
}

// recallScopes asks the local service to recall q in each scope, within
// failureRecallTimeout overall, and returns the hits in scope order. A scope
// that fails contributes nothing.
func recallScopes(scopes []string, q string) []store.Memory {
	c := client()
	deadline := time.Now().Add(failureRecallTimeout)
	var all []store.Memory
	for _, p := range scopes {
		if time.Until(deadline) <= 0 {
			break
		}
		c.Timeout = time.Until(deadline)
		resp, err := c.Get("http://aimem/v1/projects/" + url.PathEscape(p) + "/memories/recall?budget=1000&q=" + url.QueryEscape(q))
		if err != nil {
			continue
		}
		var body struct {
			Memories []store.Memory `json:"memories"`
		}
		if resp.StatusCode == 200 {
			json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body)
		}
		resp.Body.Close()
		all = append(all, body.Memories...)
	}
	return all
}

var (
	failureExitLine = regexp.MustCompile(`^Exit code -?\d+$`)
	failureWord     = regexp.MustCompile(`[a-z][a-z0-9_]*(?:[.-][a-z0-9_]+)*`)
	failureHexish   = regexp.MustCompile(`^[0-9a-f]{7,}$`)
	// Words that occur in nearly every failure, or carry no remedy: the
	// recall OR-combines its terms, so these would match everything.
	failureStopwords = map[string]bool{}
)

func init() {
	for _, w := range strings.Fields(`the and for with not this that from are was were you your has have had can
		could cannot did does doesn into onto over use used using see run running try then than when will would
		should more info error errors failed failure fail fatal exit code line lines file files warning warnings
		command usage such any all but its it's out may might must also there here what which who how why
		been being get got set now one two yes off only just via per redacted stderr stdout bash sh`) {
		failureStopwords[w] = true
	}
}

// failureQueryTerms reduces a failed command's error text to at most
// failureQueryMaxTerms distinctive lower-case words: the "Exit code N" line
// dropped, secrets redacted, paths reduced to their last component, URLs to
// their host, and numbers, hashes and common words left out.
func failureQueryTerms(errText string) []string {
	text, _ := redact.String(errText, 64<<10)
	seen := map[string]bool{}
	var terms []string
	size := 0
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if i == 0 && failureExitLine.MatchString(line) {
			continue
		}
		for _, f := range strings.Fields(line) {
			f = strings.Trim(f, `"'()[]{}<>,;`)
			if j := strings.Index(f, "://"); j >= 0 {
				f = strings.SplitN(f[j+3:], "/", 2)[0]
				if k := strings.LastIndex(f, "@"); k >= 0 {
					f = f[k+1:]
				}
			} else if strings.ContainsAny(f, `/\`) {
				f = f[strings.LastIndexAny(strings.TrimRight(f, `/\:`), `/\`)+1:]
			}
			for _, w := range failureWord.FindAllString(strings.ToLower(f), -1) {
				if len(w) < 3 || failureStopwords[w] || failureHexish.MatchString(w) || seen[w] {
					continue
				}
				if size+len(w)+1 > failureQueryBytes {
					return terms
				}
				seen[w] = true
				terms = append(terms, w)
				size += len(w) + 1
				if len(terms) == failureQueryMaxTerms {
					return terms
				}
			}
		}
	}
	return terms
}

// failureRecallOverlap is how many query terms a memory must share to come
// back: two, or three once the query has 8 terms or more, since a long error
// shares a couple of ordinary words ("agent", "operation") with many
// unrelated memories; never more than the query has.
func failureRecallOverlap(terms int) int {
	if terms >= 8 {
		return 3
	}
	return min(2, terms)
}

// failureRecallContext keeps the hits that share enough query terms
// (failureRecallOverlap), best overlap first and recall order within it,
// drops duplicates, and renders at most failureRecallHits of them, each
// redacted and bounded, as the hook's additional context. It is "" when
// nothing qualifies.
func failureRecallContext(terms []string, hits []store.Memory) string {
	need := failureRecallOverlap(len(terms))
	type scored struct {
		m       store.Memory
		overlap int
	}
	var keep []scored
	seen := map[string]bool{}
	for _, m := range hits {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		words := map[string]bool{}
		for _, w := range failureWord.FindAllString(strings.ToLower(m.Text), -1) {
			words[w] = true
			for _, part := range strings.FieldsFunc(w, func(r rune) bool { return r == '.' || r == '-' }) {
				words[part] = true
			}
		}
		n := 0
		for _, t := range terms {
			if words[t] {
				n++
			}
		}
		if n >= need {
			keep = append(keep, scored{m, n})
		}
	}
	for i := 1; i < len(keep); i++ {
		for j := i; j > 0 && keep[j].overlap > keep[j-1].overlap; j-- {
			keep[j], keep[j-1] = keep[j-1], keep[j]
		}
	}
	if len(keep) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("aimem recall for this failure (stored memories that share its terms; check that one applies before acting on it):\n")
	for i, s := range keep {
		if i == failureRecallHits {
			break
		}
		text, _ := redact.String(strings.Join(strings.Fields(s.m.Text), " "), 64<<10)
		if len(text) > failureRecallHitBytes {
			text = strings.ToValidUTF8(text[:failureRecallHitBytes], "") + "…"
		}
		line := "- " + text
		if s.m.Kind != "" {
			line = "- [" + s.m.Kind + "] " + text
		}
		line += " (memory " + s.m.ID + ")\n"
		if b.Len()+len(line) > failureRecallMaxBytes {
			break
		}
		b.WriteString(line)
	}
	if !strings.Contains(b.String(), "\n- ") {
		return ""
	}
	return b.String()
}
