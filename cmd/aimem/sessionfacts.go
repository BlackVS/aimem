package main

// Session-start knowledge injection (FEATURE-PROPOSALS #6): opt-in via
// .aimem.json {"session_facts": <tokenBudget>}, a budgeted slice of
// recalled facts rides into context with the handoff — so conventions
// reach the agent BEFORE the first mistake instead of waiting for it
// to think of recall_memory. Everything stays mechanical and local:
// the query is the previous session's recent requests, recall is the
// service's hybrid pipeline (BM25-only when no embeddings are
// configured — zero egress), and every step fails open to silence:
// session start gains no failure mode.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"aimem/internal/ident"
	"aimem/internal/store"
)

func sessionFactsNotice() string {
	budget := ident.SessionFactsBudget(".")
	if budget <= 0 {
		return ""
	}
	id, err := ident.ProjectID(".")
	if err != nil {
		return ""
	}
	q := previousSessionQuery(id)
	if q == "" {
		return ""
	}
	// Project knowledge first, then declared groups, then the user's
	// cross-project preferences — the same visibility recall grants.
	scopes := []string{id}
	if gs, err := ident.ProjectGroups("."); err == nil {
		scopes = append(scopes, gs...)
	}
	scopes = append(scopes, "user")
	type hit struct {
		store.Memory
		scope string
	}
	var hits []hit
	for _, sc := range scopes {
		var res struct {
			Memories []store.Memory `json:"memories"`
		}
		if err := localGetJSON(fmt.Sprintf("/v1/projects/%s/memories/recall?q=%s&budget=%d",
			url.PathEscape(sc), url.QueryEscape(q), budget), &res); err != nil {
			continue
		}
		for _, m := range res.Memories {
			hits = append(hits, hit{m, sc})
		}
	}
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, h := range hits {
		label := h.Kind
		if h.scope != id {
			label += ", " + strings.TrimPrefix(h.scope, "group-")
		}
		line := fmt.Sprintf("- [%s] %s\n", label, h.Text)
		t := len(line)/4 + 1 // the usual rough token estimate
		if used+t > budget {
			break
		}
		b.WriteString(line)
		used += t
	}
	if used == 0 {
		return ""
	}
	return "\n\n--- Possibly relevant knowledge (matched against the previous session's activity; " +
		"verify before relying on it, and use recall_memory to dig deeper) ---\n" + b.String()
}

// previousSessionQuery builds the recall query mechanically from the
// most recent session's last few requests — "what was being worked on",
// with no LLM in the loop.
func previousSessionQuery(id string) string {
	var sres struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
	}
	if err := localGetJSON("/v1/projects/"+url.PathEscape(id)+"/sessions", &sres); err != nil ||
		len(sres.Sessions) == 0 {
		return ""
	}
	var tres struct {
		Events []store.StoredEvent `json:"events"`
	}
	if err := localGetJSON(fmt.Sprintf("/v1/projects/%s/sessions/%s/timeline",
		url.PathEscape(id), url.PathEscape(sres.Sessions[0].SessionID)), &tres); err != nil {
		return ""
	}
	evs := tres.Events
	if len(evs) > 4 {
		evs = evs[len(evs)-4:]
	}
	var parts []string
	for _, e := range evs {
		if r := strings.TrimSpace(e.UserRequest); r != "" {
			parts = append(parts, r)
		}
	}
	q := strings.Join(parts, " ")
	if len(q) > 600 {
		q = q[:600]
	}
	return q
}

func localGetJSON(path string, into any) error {
	resp, err := client().Get("http://aimem" + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
