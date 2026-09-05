package main

// `aimem docs` — shared documents over the hub (docs/DESIGN-shared-docs.md).
// Named `docs` (plural) because `aimem doc` is the design-document
// synthesizer and hub timers already call it; recorded as a proposal
// correction. The hub is the authority; the local file is the working
// copy; the checkpoint path publishes changed bound files automatically,
// so push/pull here are for deliberate moments — resolving a conflict,
// bringing a fresh clone current, retiring a doc.

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aimem/internal/adapter"
	"aimem/internal/diff3"
	"aimem/internal/ident"
	"aimem/internal/store"
)

func docsCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`usage: aimem docs <list|sync|push|pull|diff|merge|log|rm> [name] [--force]

Shared documents: whole files (the handoff, runbooks) versioned on the
project's hub with compare-and-swap. Bound files (docs/SESSION-STATE.md
by default, plus .aimem.json "docs" entries) publish automatically on
every checkpoint; these commands are for deliberate moments. "sync"
runs the periodic reconcile (pull / clean-merge / push) right now.`)
	}
	sub, rest := args[0], args[1:]
	force := false
	var names []string
	for _, a := range rest {
		if a == "--force" || a == "-f" {
			force = true
		} else {
			names = append(names, a)
		}
	}

	dir := "."
	projID, err := ident.ProjectID(dir)
	if err != nil {
		return err
	}
	hubName, err := ident.ProjectHubName(dir)
	if err != nil {
		return err // invalid binding must fail loudly, never route to the default hub
	}
	_, hub := adapter.ResolveHub(stateRoot(), hubName)
	if hub == nil {
		return fmt.Errorf("no hub configured (aimem hub add ...) — shared docs live on the hub")
	}
	c := adapter.NewClient(stateRoot())
	host, _ := os.Hostname()
	by := host + "/cli"

	one := func() (string, error) {
		if len(names) != 1 {
			return "", fmt.Errorf("usage: aimem docs %s <name>", sub)
		}
		return names[0], nil
	}
	boundPath := func(name string) string {
		for _, rel := range ident.ProjectDocs(dir) {
			if ident.DocName(rel) == name {
				return rel
			}
		}
		return ""
	}

	switch sub {
	case "sync":
		// The doc-collab reconcile (DESIGN-doc-collab), on demand instead
		// of waiting for the periodic sync: fast-forward pulls, clean
		// merges, conflict previews — each action prints as it happens —
		// then publish whatever is locally changed.
		c.ReconcileDocsIn(hub, projID, dir)
		res := c.PublishDocs(dir, projID, hubName, by)
		if len(res) == 0 {
			fmt.Println("docs in sync (nothing to publish)")
		}
		for _, r := range res {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "aimem: %s: %v\n", r.Name, r.Err)
			} else {
				fmt.Printf("%s -> rev %d\n", r.Name, r.Rev)
			}
		}
		return nil

	case "list", "ls":
		docs, err := c.ListHubDocs(hub, projID)
		if err != nil {
			return err
		}
		if len(docs) == 0 {
			fmt.Println("no shared documents yet — bound files publish on the next checkpoint, or: aimem docs push <name>")
			return nil
		}
		bound := map[string]string{}
		for _, rel := range ident.ProjectDocs(dir) {
			bound[ident.DocName(rel)] = rel
		}
		for _, d := range docs {
			state := "hub-only"
			if rel, ok := bound[d.Name]; ok {
				state = rel
				// Compare the file against the sidecar's last-synced hash:
				// free, and avoids one full-body hub fetch per doc.
				if body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
					if last := c.DocSyncHash(projID, d.Name); last != "" && last != hashHex(body) {
						state += " (local differs)"
					}
				}
			}
			mark := ""
			if d.Deleted {
				mark = " [deleted]"
			}
			fmt.Printf("%-24s rev %-4d %s  by %-24s %s%s\n", d.Name, d.Rev, d.UpdatedAt, d.UpdatedBy, state, mark)
		}
		return nil

	case "push":
		if len(names) == 0 { // push everything bound
			res := c.PublishDocs(dir, projID, hubName, by)
			if len(res) == 0 {
				fmt.Println("nothing to publish (all bound docs unchanged)")
			}
			for _, r := range res {
				if r.Err != nil {
					fmt.Fprintf(os.Stderr, "aimem: %s: %v\n", r.Name, r.Err)
				} else {
					fmt.Printf("%s -> rev %d\n", r.Name, r.Rev)
				}
			}
			return nil
		}
		name, err := one()
		if err != nil {
			return err
		}
		rel := boundPath(name)
		if rel == "" {
			return fmt.Errorf("%q is not a bound file (docs/SESSION-STATE.md is bound by default; add others to .aimem.json \"docs\")", name)
		}
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		if len(body) > store.WarnDocBytes {
			fmt.Fprintf(os.Stderr, "aimem: %s is %dKB — shared docs should stay small\n", name, len(body)/1024)
		}
		baseRev := c.DocSyncRev(projID, name)
		if force {
			if cur, err := c.GetHubDoc(hub, projID, name, 0); err == nil {
				baseRev = cur.Rev
			}
		}
		doc, err := c.PutHubDoc(hub, projID, name, string(body), by, baseRev)
		if err != nil {
			return err
		}
		c.DocSyncRecord(projID, name, doc.Rev, hashHex(body))
		fmt.Printf("%s -> rev %d\n", name, doc.Rev)
		return nil

	case "pull":
		name, err := one()
		if err != nil {
			return err
		}
		doc, err := c.GetHubDoc(hub, projID, name, 0)
		if err != nil {
			return err
		}
		if doc.Deleted {
			return fmt.Errorf("%s is deleted on the hub (rev %d by %s)", name, doc.Rev, doc.UpdatedBy)
		}
		rel := boundPath(name)
		if rel == "" {
			rel = "docs/" + name + ".md"
		}
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		// Never clobber unpublished local work: losing an uncommitted
		// handoff to a pull would be this feature's worst possible bug.
		if cur, err := os.ReadFile(abs); err == nil && !force {
			if hashHex(cur) != c.DocSyncHash(projID, name) && string(cur) != doc.Body {
				return fmt.Errorf("%s has local changes not on the hub — `aimem docs diff %s` to compare, --force to overwrite with rev %d", rel, name, doc.Rev)
			}
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(doc.Body), 0o644); err != nil {
			return err
		}
		c.DocSyncRecord(projID, name, doc.Rev, hashHex([]byte(doc.Body)))
		fmt.Printf("%s <- rev %d (%s by %s)\n", rel, doc.Rev, doc.UpdatedAt, doc.UpdatedBy)
		return nil

	case "merge":
		// Three-way merge (FEATURE-PROPOSALS #3): base = the revision this
		// machine last synced (sidecar), fetched from the hub's retained
		// history; local = the bound file; other = the hub's current body.
		// Non-overlapping edits apply silently, overlaps become conflict
		// markers in the file. Local edits can never be lost: they end up
		// merged or inside a marker block.
		name, err := one()
		if err != nil {
			return err
		}
		rel := boundPath(name)
		if rel == "" {
			return fmt.Errorf("%q has no bound local file — merge needs one (read_doc/update_doc is the unbound flow)", name)
		}
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		local, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("no local file to merge (%v) — `aimem docs pull %s` instead", err, name)
		}
		cur, err := c.GetHubDoc(hub, projID, name, 0)
		if err != nil {
			return err
		}
		if cur.Deleted {
			return fmt.Errorf("%s is deleted on the hub (rev %d by %s) — nothing to merge with", name, cur.Rev, cur.UpdatedBy)
		}
		if string(local) == cur.Body {
			c.DocSyncRecord(projID, name, cur.Rev, hashHex(local))
			fmt.Printf("%s: local file already matches hub rev %d — nothing to merge\n", name, cur.Rev)
			return nil
		}
		baseRev := c.DocSyncRev(projID, name)
		if baseRev == cur.Rev {
			return fmt.Errorf("%s: local edits are based on the hub's current rev %d — no divergence; just `aimem docs push %s`", name, cur.Rev, name)
		}
		base := ""
		if baseRev > 0 {
			if bd, err := c.GetHubDoc(hub, projID, name, baseRev); err == nil && !bd.Deleted {
				base = bd.Body
			} else {
				fmt.Fprintf(os.Stderr, "aimem: base rev %d no longer retained on the hub — two-way merge (expect more conflicts)\n", baseRev)
			}
		}
		merged, conflicts, err := diff3.MergeText(base, string(local),
			cur.Body, "local ("+rel+")", fmt.Sprintf("hub rev %d by %s", cur.Rev, cur.UpdatedBy))
		if err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(merged), 0o644); err != nil {
			return err
		}
		// A completed merge consumes any reconcile preview beacon.
		os.Remove(abs + ".merge")
		if conflicts == 0 {
			// Rebase the sidecar onto the hub's current rev, with the HUB
			// body's hash: the merged file then reads as a pending local
			// change, so the next checkpoint auto-publishes it (or push
			// now). CAS is satisfied either way.
			c.DocSyncRecord(projID, name, cur.Rev, hashHex([]byte(cur.Body)))
			fmt.Printf("%s: merged cleanly into %s — review it; the next checkpoint publishes it (or `aimem docs push %s` now)\n",
				name, rel, name)
			return nil
		}
		// Conflicted: record the WRITTEN file's hash, so the auto-publisher
		// stays quiet until a human resolves the markers (any edit changes
		// the hash and the resolved file then pushes against the current
		// rev). Conflict markers must never reach the hub on their own.
		c.DocSyncRecord(projID, name, cur.Rev, hashHex([]byte(merged)))
		fmt.Printf("%s: %d conflict(s) written to %s — resolve the <<<<<<< blocks, then `aimem docs push %s` (auto-publish waits for your edit)\n",
			name, conflicts, rel, name)
		return nil

	case "diff":
		name, err := one()
		if err != nil {
			return err
		}
		doc, err := c.GetHubDoc(hub, projID, name, 0)
		if err != nil {
			return err
		}
		rel := boundPath(name)
		if rel == "" {
			return fmt.Errorf("%q has no bound local file to diff against", name)
		}
		local, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		if string(local) == doc.Body {
			fmt.Printf("%s: local file matches hub rev %d\n", name, doc.Rev)
			return nil
		}
		fmt.Printf("--- hub %s rev %d (%s by %s)\n+++ local %s\n", name, doc.Rev, doc.UpdatedAt, doc.UpdatedBy, rel)
		printUnifiedish(doc.Body, string(local))
		return nil

	case "log":
		name, err := one()
		if err != nil {
			return err
		}
		var res struct {
			Revisions []adapter.HubDoc `json:"revisions"`
		}
		if err := c.HubDocGetJSON(hub, projID, name, "/log", &res); err != nil {
			return err
		}
		for _, d := range res.Revisions {
			mark := ""
			if d.Deleted {
				mark = "  [deleted]"
			}
			fmt.Printf("rev %-4d %s  by %s%s\n", d.Rev, d.UpdatedAt, d.UpdatedBy, mark)
		}
		return nil

	case "rm":
		name, err := one()
		if err != nil {
			return err
		}
		cur, err := c.GetHubDoc(hub, projID, name, 0)
		if err != nil {
			return err
		}
		if !force {
			return fmt.Errorf("retire %s (rev %d, %d bytes)? re-run with --force — this tombstones the doc for every machine", name, cur.Rev, len(cur.Body))
		}
		if err := c.DeleteHubDoc(hub, projID, name, by, cur.Rev); err != nil {
			return err
		}
		fmt.Printf("%s retired (tombstone at rev %d)\n", name, cur.Rev+1)
		return nil
	}
	return fmt.Errorf("unknown subcommand %q (list|push|pull|diff|log|rm)", sub)
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// printUnifiedish prints a minimal line diff — enough to see what
// diverged without shipping a diff library for two small text files.
func printUnifiedish(hubBody, local string) {
	h, l := strings.Split(hubBody, "\n"), strings.Split(local, "\n")
	seen := map[string]bool{}
	for _, line := range h {
		seen[line] = true
	}
	inLocal := map[string]bool{}
	for _, line := range l {
		inLocal[line] = true
	}
	for _, line := range h {
		if !inLocal[line] {
			fmt.Println("-" + line)
		}
	}
	for _, line := range l {
		if !seen[line] {
			fmt.Println("+" + line)
		}
	}
}

var _ = flag.ErrHelp
