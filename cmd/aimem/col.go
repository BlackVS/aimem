package main

// `aimem col` — structured collections (docs/DESIGN-structured-docs.md):
// live trees of small JSON records on the hub, edited record-at-a-time by
// many concurrent writers, with markdown strictly a GENERATED artifact.
// Named `col` because `aimem collections` would collide with nothing but
// patience.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"aimem/internal/adapter"
	"aimem/internal/ident"
	"aimem/internal/redact"
)

func colCmd(args []string) error {
	// Flags are scanned manually (like docsCmd's --force) so they work in
	// the natural trailing position — package flag stops at the first
	// positional argument, which here is always the subcommand.
	scopeF, outF := "", ""
	baseRevF := int64(-1)
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func(name string) (string, bool) {
			if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
				return v, true
			}
			if a == "--"+name && i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		if v, ok := val("scope"); ok {
			scopeF = v
		} else if v, ok := val("out"); ok {
			outF = v
		} else if v, ok := val("base-rev"); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("--base-rev wants a number, got %q", v)
			}
			baseRevF = n
		} else {
			rest = append(rest, a)
		}
	}
	scope, out, baseRev := &scopeF, &outF, &baseRevF
	if len(rest) == 0 {
		return fmt.Errorf(`usage: aimem col <list|get|put|rm|render|import> ...

  aimem col list                     collections on the hub (this project + bound groups)
  aimem col list <collection>        records of one collection, in tree order
  aimem col get <collection> <id>    one record's JSON (note the rev)
  aimem col put <collection> <id> [file.json]   CAS write (stdin without file;
                                     --base-rev required to update, 0/absent creates)
  aimem col rm  <collection> <id>    tombstone (--base-rev required)
  aimem col log <collection> <id>    recent revisions of one record
  aimem col render <collection>      generate markdown (--out or the binding's render path)
  aimem col import <collection> <openapi.json>  seed records from an OpenAPI spec

Records are small JSON objects with slash-path ids (api/messages/create).
The CAS unit is the record: writers touching different records never
conflict. Declare bindings in .aimem.json {"collections":[{"name","scope","render"}]}.`)
	}
	sub := rest[0]
	dir := "."
	projID, err := ident.ProjectID(dir)
	if err != nil {
		return err
	}
	hubName, _ := ident.ProjectHubName(dir)
	_, hub := adapter.ResolveHub(stateRoot(), hubName)
	if hub == nil {
		return fmt.Errorf("no hub configured (aimem hub add ...) — collections live on the hub")
	}
	c := adapter.NewClient(stateRoot())
	host, _ := os.Hostname()
	by := host + "/cli"

	bindings := ident.ProjectCollections(dir)
	binding := func(name string) *ident.ColBinding {
		for i := range bindings {
			if bindings[i].Name == name {
				return &bindings[i]
			}
		}
		return nil
	}
	// The scope a collection is addressed under: explicit --scope wins,
	// then the binding's declared scope, then this project's partition.
	scopeProject := func(name string) (string, error) {
		sc := *scope
		if sc == "" {
			if b := binding(name); b != nil {
				sc = b.Scope
			}
		}
		if g, ok := strings.CutPrefix(sc, "group:"); ok {
			return ident.GroupProject(g)
		}
		if sc != "" {
			return "", fmt.Errorf("invalid scope %q (want group:<name>)", sc)
		}
		return projID, nil
	}

	switch sub {
	case "list", "ls":
		if len(rest) >= 2 {
			p, err := scopeProject(rest[1])
			if err != nil {
				return err
			}
			recs, err := c.ListHubRecords(hub, p, rest[1], false)
			if err != nil {
				return err
			}
			if len(recs) == 0 {
				fmt.Println("no records yet — create one with: aimem col put", rest[1], "<id>")
				return nil
			}
			for _, r := range recs {
				mark := ""
				if r.Deleted {
					mark = " [deleted]"
				}
				fmt.Printf("%-40s rev %-4d %s  by %-24s %4dB%s\n", r.ID, r.Rev, r.UpdatedAt, r.UpdatedBy, r.Size, mark)
			}
			return nil
		}
		// All collections visible from here: project scope plus every
		// distinct group named by a binding.
		seen := map[string]bool{}
		scopes := []string{projID}
		for _, b := range bindings {
			if g, ok := strings.CutPrefix(b.Scope, "group:"); ok {
				if id, err := ident.GroupProject(g); err == nil && !seen[id] {
					seen[id], scopes = true, append(scopes, id)
				}
			}
		}
		total := 0
		for _, p := range scopes {
			cols, err := c.ListHubCollections(hub, p)
			if err != nil {
				if p == projID {
					return err
				}
				continue // a group with no hub presence yet is not an error
			}
			for _, col := range cols {
				note := ""
				if p != projID {
					note = "  [group:" + strings.TrimPrefix(p, "group-") + "]"
				}
				fmt.Printf("%-24s %4d records  updated %s%s\n", col.Name, col.Records, col.UpdatedAt, note)
			}
			total += len(cols)
		}
		if total == 0 {
			fmt.Println("no collections yet — create the first record with: aimem col put <collection> <id>")
		}
		return nil

	case "get":
		if len(rest) != 3 {
			return fmt.Errorf("usage: aimem col get <collection> <id>")
		}
		p, err := scopeProject(rest[1])
		if err != nil {
			return err
		}
		rec, err := c.GetHubRecord(hub, p, rest[1], rest[2], 0)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "# %s/%s rev %d by %s at %s\n", rest[1], rec.ID, rec.Rev, rec.UpdatedBy, rec.UpdatedAt)
		var pretty map[string]any
		if json.Unmarshal(rec.Body, &pretty) == nil {
			b, _ := json.MarshalIndent(pretty, "", "  ")
			fmt.Println(string(b))
		} else {
			fmt.Println(string(rec.Body))
		}
		return nil

	case "put":
		if len(rest) < 3 || len(rest) > 4 {
			return fmt.Errorf("usage: aimem col put <collection> <id> [file.json]  (stdin when no file)")
		}
		p, err := scopeProject(rest[1])
		if err != nil {
			return err
		}
		var body []byte
		if len(rest) == 4 {
			body, err = os.ReadFile(rest[3])
		} else {
			body, err = io.ReadAll(os.Stdin)
		}
		if err != nil {
			return err
		}
		// Same softer-tier warning documents get: the hub refuses the
		// unambiguous secret shapes itself, but a softer match publishes
		// as written to everyone on the project — say so (arch review A3).
		if warn, _ := redact.ScanAuthored(string(body)); len(warn) > 0 {
			fmt.Fprintf(os.Stderr, "aimem: record has secret-shaped content (%s) — it is stored as written on the hub\n",
				strings.Join(warn, ", "))
		}
		base := *baseRev
		if base < 0 {
			// CAS discipline for humans: creating is free, updating
			// requires having read the record (its rev names the base).
			if cur, err := c.GetHubRecord(hub, p, rest[1], rest[2], 0); err == nil {
				return fmt.Errorf("%s/%s exists at rev %d (by %s) — read it, then update with --base-rev %d",
					rest[1], cur.ID, cur.Rev, cur.UpdatedBy, cur.Rev)
			}
			base = 0
		}
		rec, err := c.PutHubRecord(hub, p, rest[1], rest[2], body, by, base)
		if err != nil {
			return err
		}
		fmt.Printf("%s/%s -> rev %d\n", rest[1], rec.ID, rec.Rev)
		return nil

	case "log":
		if len(rest) != 3 {
			return fmt.Errorf("usage: aimem col log <collection> <id>")
		}
		p, err := scopeProject(rest[1])
		if err != nil {
			return err
		}
		revs, err := c.RecordHubLog(hub, p, rest[1], rest[2])
		if err != nil {
			return err
		}
		if len(revs) == 0 {
			fmt.Println("no retained revisions")
			return nil
		}
		for _, r := range revs {
			mark := ""
			if r.Deleted {
				mark = " [deleted]"
			}
			fmt.Printf("rev %-4d %s  by %-24s %4dB%s\n", r.Rev, r.UpdatedAt, r.UpdatedBy, r.Size, mark)
		}
		return nil

	case "rm":
		if len(rest) != 3 {
			return fmt.Errorf("usage: aimem col rm <collection> <id> --base-rev <rev>")
		}
		if *baseRev < 0 {
			return fmt.Errorf("rm requires --base-rev (read the record first; its rev names the base)")
		}
		p, err := scopeProject(rest[1])
		if err != nil {
			return err
		}
		if err := c.DeleteHubRecord(hub, p, rest[1], rest[2], by, *baseRev); err != nil {
			return err
		}
		fmt.Printf("%s/%s tombstoned\n", rest[1], rest[2])
		return nil

	case "render":
		if len(rest) != 2 {
			return fmt.Errorf("usage: aimem col render <collection> [--out <file.md|dir/>]")
		}
		name := rest[1]
		p, err := scopeProject(name)
		if err != nil {
			return err
		}
		target := *out
		if target == "" {
			if b := binding(name); b != nil {
				target = b.Render
			}
		}
		recs, err := c.ListHubRecords(hub, p, name, true)
		if err != nil {
			return err
		}
		live := recs[:0]
		for _, r := range recs {
			if !r.Deleted {
				live = append(live, r)
			}
		}
		if len(live) == 0 {
			return fmt.Errorf("collection %q has no live records", name)
		}
		if target == "" || strings.HasSuffix(target, ".md") {
			doc := renderCollection(name, live, "")
			if target == "" {
				fmt.Print(doc)
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(target, []byte(doc), 0o644); err != nil {
				return err
			}
			fmt.Printf("%s: %d records -> %s\n", name, len(live), target)
			return nil
		}
		// Directory mode: one file per top-level branch.
		branches := map[string][]adapter.HubRecord{}
		var order []string
		for _, r := range live {
			top, _, _ := strings.Cut(r.ID, "/")
			if _, ok := branches[top]; !ok {
				order = append(order, top)
			}
			branches[top] = append(branches[top], r)
		}
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		for _, top := range order {
			doc := renderCollection(name, branches[top], top)
			if err := os.WriteFile(filepath.Join(target, top+".md"), []byte(doc), 0o644); err != nil {
				return err
			}
		}
		fmt.Printf("%s: %d records -> %s (%d files)\n", name, len(live), target, len(order))
		return nil

	case "import":
		if len(rest) != 3 {
			return fmt.Errorf("usage: aimem col import <collection> <openapi.json>")
		}
		p, err := scopeProject(rest[1])
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(rest[2])
		if err != nil {
			return err
		}
		recs, err := openapiRecords(raw)
		if err != nil {
			return err
		}
		// Writes at base 0 either create, land as idempotent no-ops on an
		// identical existing record, or conflict on a record a writer has
		// since changed — which the importer must never overwrite.
		applied, diverged := 0, 0
		for _, r := range recs {
			if _, err := c.PutHubRecord(hub, p, rest[1], r.id, r.body, by, 0); err != nil {
				diverged++
				continue
			}
			applied++
		}
		fmt.Printf("%s: %d records applied (created or already identical), %d diverged and left alone — the importer never overwrites\n",
			rest[1], applied, diverged)
		return nil
	}
	return fmt.Errorf("unknown subcommand %q (aimem col with no args shows usage)", sub)
}

// renderCollection is the deterministic built-in renderer: records in id
// order, headings from path segments, scalar fields as a definition list,
// nested shapes as fenced JSON. Same records in, same bytes out — the
// file is a build artifact and says so.
func renderCollection(name string, recs []adapter.HubRecord, branch string) string {
	var b strings.Builder
	title := name
	if branch != "" {
		title = name + "/" + branch
	}
	fmt.Fprintf(&b, "<!-- GENERATED from hub collection %q — do not edit; regenerate with `aimem col render %s` -->\n\n# %s\n",
		name, name, title)
	emitted := map[string]bool{}
	for _, r := range recs {
		segs := strings.Split(r.ID, "/")
		// Ancestor headings for each new prefix (skip the leaf).
		for i := 1; i < len(segs); i++ {
			prefix := strings.Join(segs[:i], "/")
			if !emitted[prefix] {
				emitted[prefix] = true
				fmt.Fprintf(&b, "\n%s %s\n", strings.Repeat("#", min(i+1, 6)), segs[i-1])
			}
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(r.Body, &fields); err != nil {
			continue
		}
		leafDepth := min(len(segs)+1, 6)
		heading := segs[len(segs)-1]
		if t, ok := stringField(fields, "title"); ok {
			heading = t
		}
		fmt.Fprintf(&b, "\n%s %s\n\n", strings.Repeat("#", leafDepth), heading)
		if d, ok := stringField(fields, "description"); ok {
			fmt.Fprintf(&b, "%s\n\n", d)
		}
		keys := make([]string, 0, len(fields))
		for k := range fields {
			if k == "title" || k == "description" {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var nested []string
		for _, k := range keys {
			var s string
			if json.Unmarshal(fields[k], &s) == nil {
				fmt.Fprintf(&b, "- **%s**: %s\n", k, s)
				continue
			}
			var n float64
			var t bool
			if json.Unmarshal(fields[k], &n) == nil || json.Unmarshal(fields[k], &t) == nil {
				fmt.Fprintf(&b, "- **%s**: %s\n", k, string(fields[k]))
				continue
			}
			nested = append(nested, k)
		}
		for _, k := range nested {
			var v any
			json.Unmarshal(fields[k], &v)
			pretty, _ := json.MarshalIndent(v, "", "  ")
			fmt.Fprintf(&b, "\n**%s**:\n\n```json\n%s\n```\n", k, pretty)
		}
	}
	return b.String()
}

func stringField(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := fields[key]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return "", false
	}
	return s, true
}

type importRecord struct {
	id   string
	body json.RawMessage
}

// openapiRecords turns an OpenAPI spec's paths into records: one per
// operation, id derived from the URL path plus the method — the dogfood
// importer that seeds aimem's own API wiki from openapi.json.
func openapiRecords(raw []byte) ([]importRecord, error) {
	var spec struct {
		Paths map[string]map[string]struct {
			Summary string `json:"summary"`
			XRole   string `json:"x-role"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("not an OpenAPI JSON spec: %w", err)
	}
	if len(spec.Paths) == 0 {
		return nil, fmt.Errorf("spec has no paths")
	}
	type entry struct {
		importRecord
		paramFinal bool // the raw path ends in a parameter: an item op
		method     string
	}
	var all []entry
	counts := map[string]int{}
	for path, ops := range spec.Paths {
		segs := strings.Split(strings.Trim(path, "/"), "/")
		paramFinal := len(segs) > 0 && strings.HasPrefix(segs[len(segs)-1], "{")
		for method, op := range ops {
			id := recordIDFromPath(path, method)
			if id == "" {
				continue
			}
			body, _ := json.Marshal(map[string]any{
				"method": strings.ToUpper(method), "path": path,
				"summary": op.Summary, "role": op.XRole,
			})
			all = append(all, entry{importRecord{id: id, body: body}, paramFinal, method})
			counts[id]++
		}
	}
	// Parameter stripping can collapse a listing and its item op onto one
	// id ("/docs" GET vs "/docs/{name}" GET). Disambiguate only actual
	// collisions: the item op (parameter-final path) gets "-one".
	var out []importRecord
	for _, e := range all {
		if counts[e.id] > 1 && e.paramFinal {
			e.id += "-one"
		}
		out = append(out, e.importRecord)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// recordIDFromPath maps "/v1/projects/{p}/docs/{name}" + "put" to
// "projects/docs/put": version prefix and parameters drop out, structure
// stays.
func recordIDFromPath(path, method string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var keep []string
	for _, s := range segs {
		if s == "" || strings.HasPrefix(s, "{") || s == "v1" {
			continue
		}
		keep = append(keep, s)
	}
	if len(keep) == 0 {
		keep = []string{"root"}
	}
	return strings.Join(keep, "/") + "/" + strings.ToLower(method)
}
