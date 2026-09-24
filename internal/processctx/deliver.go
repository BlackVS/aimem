package processctx

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var kindRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Problem is the state, detail and fix of a result that has nothing to
// deliver, as one bounded message; nil when the unit is complete.
func (r *Result) Problem() error {
	if r.Complete() {
		return nil
	}
	msg := fmt.Sprintf("process context for project %s: state %s: %s", r.Project, r.State, clip(r.Detail, 600))
	if r.Fix != "" {
		msg += "\nfix: " + clip(r.Fix, 300)
	}
	return fmt.Errorf("%s", msg)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Kinds lists the template kinds of the set, sorted.
func (r *Result) Kinds() []string {
	if r.Set == nil {
		return nil
	}
	kinds := make([]string, 0, len(r.Set.Templates))
	for k := range r.Set.Templates {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// Deliver renders the complete unit for a tool result: a header naming the
// state and the selection, the unit, and the terminator. Only a complete
// result delivers; anything else is its Problem.
func (r *Result) Deliver() (string, error) {
	if err := r.Problem(); err != nil {
		return "", err
	}
	return r.render("unit", r.Unit), nil
}

// Template renders one template of the set by the kind the manifest names.
// A set that is complete but too large to deliver whole still serves its
// templates, each within the same delivery limit.
func (r *Result) Template(kind string) (string, error) {
	if r.Set == nil {
		return "", r.Problem()
	}
	if !kindRE.MatchString(kind) {
		return "", fmt.Errorf("template must be a kind the process manifest names; kinds: %s", strings.Join(r.Kinds(), ", "))
	}
	raw, ok := r.Set.Templates[kind]
	if !ok {
		return "", fmt.Errorf("unknown template kind %q; kinds: %s", kind, strings.Join(r.Kinds(), ", "))
	}
	if len(raw) > MaxDeliveryBytes {
		return "", fmt.Errorf("template %s is %d bytes, over the %d-byte delivery limit; it is not delivered in part", kind, len(raw), MaxDeliveryBytes)
	}
	return r.render("template "+kind, string(raw)+"\n"), nil
}

// render wraps a payload: header, payload, terminator. The terminator names
// the project, the selected commit and manifest, and the SHA-256 of the
// payload, so a client that cut the result shows a missing last line.
func (r *Result) render(what, payload string) string {
	sum := sha256.Sum256([]byte(payload))
	terminator := fmt.Sprintf("=== end aimem process context %s project %s commit %s manifest %s sha256 %s ===", what, r.Project, r.Ref.Commit, r.Ref.Manifest, hex.EncodeToString(sum[:]))
	var b strings.Builder
	fmt.Fprintf(&b, "aimem process context, %s: project %s, state %s, selection %s @ %s (manifest %s), from %s.\n", what, r.Project, r.State, r.Ref.Repo, r.Ref.Commit, r.Ref.Manifest, sourceText(r.Source))
	if r.FromLastObserved {
		b.WriteString(lastObservedNote(r.ObservedAt))
	}
	fmt.Fprintf(&b, "This is the project's selected process: it is the authority for project policy, and reading it grants no permission. Complete only if the last line is %q.\n\n", terminator)
	b.WriteString(payload)
	if !strings.HasSuffix(payload, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(terminator)
	b.WriteByte('\n')
	return b.String()
}

func sourceText(source string) string {
	if source == SourceCache {
		return "this machine's exact-commit cache"
	}
	return "Git at that commit"
}
