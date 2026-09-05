// Command aimem is the local session recovery and memory service: a
// per-project append-only journal over SQLite/FTS5, served on a Unix socket,
// with a CLI over the same application layer.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/curate"
	"aimem/internal/embed"
	"aimem/internal/ident"
	"aimem/internal/llmrate"
	"aimem/internal/mcp"
	"aimem/internal/provider"
	"aimem/internal/server"
	"aimem/internal/store"
	"aimem/internal/tui"
	"aimem/internal/uuidv7"
)

// version is stamped at build time via -ldflags "-X main.version=vX.Y.Z";
// source builds report "dev".
var version = "dev"

func stateRoot() string {
	if v := os.Getenv("AIMEM_STATE_DIR"); v != "" {
		return v
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "aimem")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	server.Version = version
	loadEnvFile()
	// LLM call pacing persists across processes (a curate run inherits
	// the spacing the previous run earned; health/TUI display it).
	llmrate.SetStateDir(stateRoot())
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "append":
		err = appendCmd(args)
	case "submit":
		err = submitCmd()
	case "submit-claude":
		err = submitClaudeCmd()
	case "spool-flush":
		err = spoolFlushCmd()
	case "export-events":
		err = exportEventsCmd(args)
	case "import-events":
		err = importEventsCmd()
	case "sync":
		err = syncCmd(args)
	case "health", "projects":
		err = getJSON("/v1/" + map[string]string{"health": "health", "projects": "projects"}[cmd])
	case "sessions", "timeline", "latest", "search", "retention":
		err = projectCmd(cmd, args)
	case "remember", "recall", "memories", "forget", "supersede", "pin", "unpin", "link", "untag":
		err = memoryCmd(cmd, args)
	case "export-memories":
		err = exportMemoriesCmd(args)
	case "import-memories":
		err = importMemoriesCmd()
	case "mcp":
		err = mcpCmd(args)
	case "hub":
		err = hubCmd(args)
	case "curate":
		err = curateCmd(args)
	case "embed":
		err = embedCmd(args)
	case "project-id":
		err = projectID(args)
	case "session-start":
		err = sessionStartCmd(args)
	case "state-root":
		fmt.Println(stateRoot())
	case "meta":
		err = metaCmd(args)
	case "group":
		err = groupCmd(args)
	case "drop-project":
		err = dropProjectCmd(args)
	case "dedup":
		err = dedupCmd(args)
	case "docs":
		err = docsCmd(args)
	case "col":
		err = colCmd(args)
	case "token":
		err = tokenCmd(args)
	case "review":
		err = reviewCmd(args)
	case "logs":
		err = logsCmd(args)
	case "doc":
		err = docCmd(args)
	case "export-group-config":
		err = exportGroupConfigCmd(args)
	case "import-group-config":
		err = importGroupConfigCmd()
	case "budget":
		err = budgetCmd(args)
	case "tui":
		err = tui.Run(stateRoot())
	case "version", "--version", "-v":
		fmt.Println("aimem", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "aimem:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: aimem <command> [flags]

  serve                      run the service on the Unix socket
  append                     read {"project_id","event"} JSON on stdin, submit (no spool)
  submit                     like append but redacts adapter-side and spools
                             when the service is down (for adapters/plugins)
  submit-claude              Claude Code Stop/StopFailure hook adapter:
                             reads the hook payload on stdin
  spool-flush                replay spooled checkpoints into the service
  export-events [-p <proj>]  dump journal events as JSONL (all projects
                             unless -p); for backup and cross-machine sync
  import-events              read JSONL events on stdin, submit idempotently
                             (duplicates drop out; spools if service down)
  sync <ssh-destination>     merge journals with another machine over ssh
                             (both directions; requires aimem on PATH there);
                             --hub <name> syncs one hub's bound projects,
                             --all-hubs every hub with a sync destination
  health                     service health
  projects                   list project IDs
  sessions   -p <project>    list sessions in a project
  timeline   -p -s [-n]      session event timeline
  latest     -p -s           latest session checkpoint
  search     -p -q [-n]      FTS search within a project
  retention  -p [--max-age-days N] [--max-bytes N]
  remember   -p|-u <text>    store a curated memory (project or user scope)
  recall     -p|-u -q [-n]   search curated memories (token budget -n)
  memories   -p|-u [-a]      list curated memories (-a includes stale)
  forget     -p|-u --id      retire a memory (bi-temporal; audit preserved)
  supersede  -p|-u --id <new text>   replace a memory, keeping lineage
  link       -p|-u --id --to [--rel]  relate two memories
  untag      -p|-u --id --tag <tag>   detach a tag (re-file a fact's chapter)
  pin|unpin  -p|-u --id      protect a memory / release it
  export-memories [-p]       dump curated memories as JSONL
  import-memories            merge JSONL memories from stdin (idempotent)
  mcp                        run the MCP recall facade on stdio (register in
                             your agent client; project = cwd)
  hub [<url> <token>]        configure (or show) hubs for real-time push;
                             add/rm/default manage named hubs — projects
                             bind via .aimem.json {"hub":"<name>"}
  curate [-p] [--dry-run]    distill recent journal events into curated
                             knowledge; --backend claude (headless CLI,
                             subscription) or openai (LiteLLM proxy, small
                             fast models — Mem0-style economics)
  embed [-p|--all]           backfill embeddings for curated memories so
                             recall gains a semantic leg (BM25 + cosine,
                             RRF-merged); needs AIMEM_EMBED_MODEL +
                             AIMEM_OPENAI_API_KEY
  project-id [dir]           compute stable project identity for a directory
  session-start [file]       Claude Code SessionStart hook adapter: emit the
                             handoff (docs/SESSION-STATE.md) as hook JSON;
                             portable (no jq/bash), silent if file missing
  state-root                 print the state root path
  version                    print the binary version
  tui                        interactive dashboard (q quits)
  meta       [-p] <key>      print a project meta value
  dedup      [-p|--all] [--sim 0.90] [--dry-run]
                             fold near-identical memories onto one survivor
                             (pinned wins, else newest; tags/sources merged,
                             twin retired with audit)
  docs <list|push|pull|diff|merge|log|rm>  shared documents (handoff, runbooks)
                             on the project's hub; CAS-versioned,
                             docs/SESSION-STATE.md bound by default; merge =
                             three-way vs the last-synced revision
                             (DESIGN-shared-docs.md)
  drop-project -p <id> --yes  delete a project DB entirely (journal + memories);
                             for stale duplicates, e.g. a path-derived id after
                             pinning {"project": ...} in .aimem.json
  group      <name> [--about <charter>] [--policy all|domain]
             [--chapter "<name>: <desc>"]... [--drop-chapter <name>]...
             [--enable <feature>]... [--disable <feature>]...
                             configure a knowledge group: charter steers the
                             curator's domain routing; policy all mirrors every
                             curated member fact into the group; chapters are
                             knowledge-base sections the curator files facts into;
                             optional features (e.g. doc) switch per group
  doc        <group>|-p <id> [--all] [--force] [--show]
                             synthesize the KB's design document from its facts
                             (chapters become sections, corroboration weights
                             certainty); --all covers groups with feature doc
  budget     [-p] [--daily V] [--weekly V] [--monthly V] [--unlimited]
                             [--reset]  show/set curation spend caps
                             (V: "500k", "in:2M,out:300k", or "$5"); global by
                             default, -p for a per-project override
  token      add <name> [--role writer|admin] | list | rm <name>
                             named hub tokens (run on the hub host);
                             secret printed once, only hashes stored
  logs       [-n 40] [-q filter]  local diagnostics: client-side warnings
                             (spooled checkpoints, orphaned hub bindings,
                             doc conflicts) + the service log ring
  review     [-p] [--days N] [--max-corroboration N]  stale-fact queue:
                             old, thinly-corroborated, unpinned facts;
                             verdicts = review confirm / supersede / forget

env: AIMEM_STATE_DIR overrides the state root
     AIMEM_SECRET_ENV: comma-separated env var names whose values are scrubbed
`)
}

func serve() error {
	// Tee logs into a bounded ring so the admin GUI's Log tab can show
	// recent events/errors; journald (stderr) stays the durable log.
	ring := server.NewLogRing(500)
	log := slog.New(server.NewRingHandler(slog.NewJSONHandler(os.Stderr, nil), ring))
	root := stateRoot()
	reg, err := store.NewRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()
	srv := server.New(reg, log).WithLogRing(ring)
	httpSrv, _, err := srv.ListenAndServe(root)
	if err != nil {
		return err
	}
	// Optional hub-mode TCP listener with bearer auth and the MCP endpoint.
	var tcpSrv *http.Server
	if listen := os.Getenv("AIMEM_HTTP_LISTEN"); listen != "" {
		tcpSrv, err = srv.ListenTCP(listen,
			os.Getenv("AIMEM_HTTP_TOKEN"),
			os.Getenv("AIMEM_TLS_CERT"), os.Getenv("AIMEM_TLS_KEY"),
			map[string]http.Handler{"/mcp": mcp.NewHTTPHandler(client())})
		if err != nil {
			return err
		}
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if tcpSrv != nil {
		tcpSrv.Shutdown(ctx)
	}
	httpSrv.Shutdown(ctx)
	reg.Close()
	if err := server.WriteSentinel(root); err != nil {
		return err
	}
	log.Info("clean shutdown")
	return nil
}

// client returns an HTTP client dialing the service's Unix socket.
func client() *http.Client {
	sock := server.SocketPath(stateRoot())
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

func getJSON(path string) error {
	resp, err := client().Get("http://aimem" + path)
	if err != nil {
		return fmt.Errorf("%w (is `aimem serve` running?)", err)
	}
	defer resp.Body.Close()
	return printBody(resp)
}

func postJSON(path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := client().Post("http://aimem"+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return fmt.Errorf("%w (is `aimem serve` running?)", err)
	}
	defer resp.Body.Close()
	return printBody(resp)
}

func printBody(resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	os.Stdout.Write(b)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func appendCmd(_ []string) error {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return err
	}
	var payload json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("stdin is not valid JSON: %w", err)
	}
	resp, err := client().Post("http://aimem/v1/events", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		return fmt.Errorf("%w (is `aimem serve` running?)", err)
	}
	defer resp.Body.Close()
	return printBody(resp)
}

func projectCmd(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	p := fs.String("p", "", "project id")
	s := fs.String("s", "", "session id")
	q := fs.String("q", "", "search query")
	n := fs.Int("n", 0, "limit")
	maxAge := fs.Int("max-age-days", 0, "delete events older than N days")
	maxBytes := fs.Int64("max-bytes", 0, "shrink database to at most N bytes")
	fs.Parse(args)
	if *p == "" {
		return fmt.Errorf("-p <project> is required")
	}
	base := "/v1/projects/" + url.PathEscape(*p)
	switch cmd {
	case "sessions":
		return getJSON(base + "/sessions")
	case "timeline", "latest":
		if *s == "" {
			return fmt.Errorf("-s <session> is required")
		}
		path := base + "/sessions/" + url.PathEscape(*s) + "/" + cmd
		if cmd == "timeline" && *n > 0 {
			path += fmt.Sprintf("?limit=%d", *n)
		}
		return getJSON(path)
	case "search":
		if *q == "" {
			return fmt.Errorf("-q <query> is required")
		}
		return getJSON(base + fmt.Sprintf("/search?q=%s&limit=%d", url.QueryEscape(*q), *n))
	case "retention":
		return postJSON(base+"/retention", map[string]any{
			"max_age_days": *maxAge, "max_bytes": *maxBytes,
		})
	}
	return nil
}

// projectID prints the stable project identity for a directory.
func projectID(args []string) error {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
	}
	id, err := ident.ProjectID(dir)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

// sessionStartCmd is the Claude Code SessionStart hook: it emits the project
// handoff as additionalContext. Portable replacement for the jq/bash hook so
// the same hook command works on Windows (cmd.exe) and Linux. A missing
// handoff contributes nothing and never breaks session start.
func sessionStartCmd(args []string) error {
	p := filepath.Join("docs", "SESSION-STATE.md")
	if len(args) > 0 {
		p = args[0]
	}
	ctx := ""
	if b, err := os.ReadFile(p); err == nil {
		ctx = "Canonical session handoff (" + p +
			") — treat all claims as unverified until re-checked against git/tests:\n\n" + string(b)
		ctx += hubHandoffNotice(string(b))
	}
	// Opt-in recalled knowledge rides along (FEATURE-PROPOSALS #6);
	// empty unless .aimem.json sets "session_facts".
	ctx += sessionFactsNotice()
	ctx += mergePreviewNotice()
	if ctx == "" {
		return nil
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": ctx,
		},
	})
}

// mergePreviewNotice is the offline conflict beacon (DESIGN-doc-collab):
// a <file>.merge preview left by sync's reconcile means hub and this
// machine both changed a bound doc and the overlap needs judgment.
// Purely local — session start gains no network dependency.
func mergePreviewNotice() string {
	var b strings.Builder
	for _, rel := range ident.ProjectDocs(".") {
		if _, err := os.Stat(filepath.FromSlash(rel) + ".merge"); err == nil {
			fmt.Fprintf(&b, "\n\nATTENTION: %s.merge holds an unresolved shared-document merge preview — the hub and this machine both changed %s. Run `aimem docs merge %s`, resolve any <<<<<<< markers in the bound file, and the next checkpoint publishes it (the .merge preview is removed by a completed merge).",
				rel, rel, ident.DocName(rel))
		}
	}
	return b.String()
}

// hubHandoffNotice checks — under a hard short timeout, failing open —
// whether the project's hub holds a NEWER revision of the handoff than
// this machine last published or pulled, and if so says who wrote it and
// includes a bounded excerpt plus the pull command. Never a whole large
// document into session context, never a blocked session start
// (docs/DESIGN-shared-docs.md section 6).
func hubHandoffNotice(localBody string) string {
	id, err := ident.ProjectID(".")
	if err != nil {
		return ""
	}
	hubName, err := ident.ProjectHubName(".")
	if err != nil {
		return "" // invalid binding: skip hub contact (session start never blocks, and never touches the default hub)
	}
	_, hub := adapter.ResolveHub(stateRoot(), hubName)
	if hub == nil {
		return ""
	}
	c := adapter.NewClient(stateRoot())
	done := make(chan string, 1)
	go func() {
		doc, err := c.GetHubDoc(hub, id, "SESSION-STATE", 0)
		if err != nil || doc.Deleted {
			done <- ""
			return
		}
		if doc.Rev <= c.DocSyncRev(id, "SESSION-STATE") || doc.Body == localBody {
			done <- ""
			return
		}
		excerpt := doc.Body
		if len(excerpt) > 2048 {
			excerpt = excerpt[:2048] + "\n[truncated]"
		}
		done <- fmt.Sprintf("\n\n--- The hub holds a NEWER handoff (rev %d, %s by %s) than this machine last saw. "+
			"Reconcile deliberately before overwriting: run `aimem docs pull SESSION-STATE` (or `aimem docs diff SESSION-STATE`). Hub version begins:\n%s",
			doc.Rev, doc.UpdatedAt, doc.UpdatedBy, excerpt)
	}()
	select {
	case s := <-done:
		return s
	case <-time.After(1200 * time.Millisecond):
		return "" // an unreachable or slow hub degrades to local-only, silently
	}
}

// metaCmd prints a project meta value (ops/debug; e.g. `meta -p <id> groups`).
func metaCmd(args []string) error {
	fs := flag.NewFlagSet("meta", flag.ExitOnError)
	p := fs.String("p", "", "project id (default: current directory's project)")
	fs.Parse(args)
	key := fs.Arg(0)
	if key == "" {
		return fmt.Errorf("usage: aimem meta [-p <project>] <key>")
	}
	proj := *p
	if proj == "" {
		id, err := ident.ProjectID(".")
		if err != nil {
			return err
		}
		proj = id
	}
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	defer reg.Close()
	db, err := reg.Open(proj)
	if err != nil {
		return err
	}
	v, err := db.GetMeta(key)
	if err != nil {
		return err
	}
	fmt.Println(v)
	return nil
}

// groupCmd configures a knowledge group: its charter ("about", handed to
// the curator for domain routing) and promotion policy ("domain" routes by
// charter; "all" mirrors every curated member fact into the group — the
// meta-project case). Config lives in the group's own DB, so sync carries
// it to every machine and the hub.
func groupCmd(args []string) error {
	usage := "usage: aimem group <name> [--about <charter>] [--policy all|domain]" +
		" [--chapter \"<name>: <which facts belong here>\"]... [--drop-chapter <name>]"
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("%s", usage)
	}
	name, rest := args[0], args[1:]
	fs := flag.NewFlagSet("group", flag.ExitOnError)
	about := fs.String("about", "", "group charter: what this group's shared domain is")
	policy := fs.String("policy", "", "promotion policy: all | domain")
	var chapters, drops, enable, disable multiFlag
	fs.Var(&chapters, "chapter", "add/update a knowledge-base chapter: \"<name>: <description>\" (repeatable)")
	fs.Var(&drops, "drop-chapter", "remove a knowledge-base chapter by name (repeatable)")
	fs.Var(&enable, "enable", "switch on an optional feature: "+strings.Join(curate.KnownFeatures, ", ")+" (repeatable)")
	fs.Var(&disable, "disable", "switch off an optional feature (repeatable)")
	fs.Parse(rest)
	gid, err := ident.GroupProject(name)
	if err != nil {
		return err
	}
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	defer reg.Close()
	db, err := reg.Open(gid)
	if err != nil {
		return err
	}
	if *policy != "" && *policy != "all" && *policy != "domain" {
		return fmt.Errorf("%s", usage)
	}
	if *about != "" {
		if err := db.SetMeta("about", *about); err != nil {
			return err
		}
	}
	if *policy != "" {
		if err := db.SetMeta("policy", *policy); err != nil {
			return err
		}
	}
	if len(chapters) > 0 || len(drops) > 0 {
		var cur []curate.Chapter
		if raw, _ := db.GetMeta("chapters"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &cur)
		}
		for _, spec := range chapters {
			cname, cabout, ok := strings.Cut(spec, ":")
			cname = strings.TrimSpace(cname)
			cabout = strings.TrimSpace(cabout)
			if !ok || cname == "" || cabout == "" {
				return fmt.Errorf("--chapter wants \"<name>: <description>\", got %q", spec)
			}
			replaced := false
			for i := range cur {
				if cur[i].Name == cname {
					cur[i].About, replaced = cabout, true
				}
			}
			if !replaced {
				cur = append(cur, curate.Chapter{Name: cname, About: cabout})
			}
		}
		for _, d := range drops {
			cur = slices.DeleteFunc(cur, func(c curate.Chapter) bool { return c.Name == strings.TrimSpace(d) })
		}
		b, err := json.Marshal(cur)
		if err != nil {
			return err
		}
		if err := db.SetMeta("chapters", string(b)); err != nil {
			return err
		}
	}
	if len(enable) > 0 || len(disable) > 0 {
		var feats []string
		if raw, _ := db.GetMeta("features"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &feats)
		}
		for _, f := range enable {
			if !slices.Contains(curate.KnownFeatures, f) {
				return fmt.Errorf("unknown feature %q (known: %s)", f, strings.Join(curate.KnownFeatures, ", "))
			}
			if !slices.Contains(feats, f) {
				feats = append(feats, f)
			}
		}
		for _, f := range disable {
			feats = slices.DeleteFunc(feats, func(x string) bool { return x == f })
		}
		b, _ := json.Marshal(feats)
		if err := db.SetMeta("features", string(b)); err != nil {
			return err
		}
	}
	curAbout, _ := db.GetMeta("about")
	curPolicy, _ := db.GetMeta("policy")
	if curPolicy == "" {
		curPolicy = "domain"
	}
	var curChapters []curate.Chapter
	rawChapters, _ := db.GetMeta("chapters")
	if rawChapters != "" {
		_ = json.Unmarshal([]byte(rawChapters), &curChapters)
	}
	var curFeatures []string
	rawFeatures, _ := db.GetMeta("features")
	if rawFeatures != "" {
		_ = json.Unmarshal([]byte(rawFeatures), &curFeatures)
	}
	// Distribute edits to the hub right away: nightly curation runs there
	// and must see current routing rules, not last sync's. Best-effort —
	// the local write already succeeded.
	if *about != "" || *policy != "" || len(chapters) > 0 || len(drops) > 0 ||
		len(enable) > 0 || len(disable) > 0 {
		for _, hn := range hubNamesForGroup(reg, gid) {
			for k, v := range map[string]string{
				"about": curAbout, "policy": curPolicy, "chapters": rawChapters, "features": rawFeatures,
			} {
				if err := adapter.PushMeta(stateRoot(), hn, gid, k, v); err != nil {
					fmt.Fprintf(os.Stderr, "aimem: hub push of %s/%s failed: %v (sync will retry)\n", gid, k, err)
					break
				}
			}
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"group": name, "about": curAbout, "policy": curPolicy,
		"chapters": curChapters, "features": curFeatures,
	})
}

// loadEnvFile folds ~/.config/aimem/env into the process environment for
// AIMEM_* keys not already set. systemd units get the file via
// EnvironmentFile=; interactive invocations (aimem embed, curate, tui)
// historically saw none of it, so e.g. manual embedding failed unless the
// user exported the key by hand. Process env always wins.
func loadEnvFile() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "aimem", "env"))
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(k, "AIMEM_") || os.Getenv(k) != "" {
			continue
		}
		os.Setenv(k, strings.Trim(v, `"'`))
	}
}

// filterProjects narrows a project-id list to a comma-separated allowlist
// ("" = no filtering). Used by the export commands so a hub-partitioned
// sync only ships the projects bound to that hub.
func filterProjects(ids []string, only string) []string {
	if only == "" {
		return ids
	}
	allow := map[string]bool{}
	for _, id := range strings.Split(only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			allow[id] = true
		}
	}
	var out []string
	for _, id := range ids {
		if allow[id] {
			out = append(out, id)
		}
	}
	return out
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// dropProjectCmd deletes a whole project DB — via the running service so
// its open handle is released first; direct registry removal only when no
// service is up. Destructive: demands the exact id plus --yes.
func dropProjectCmd(args []string) error {
	fs := flag.NewFlagSet("drop-project", flag.ExitOnError)
	p := fs.String("p", "", "project id to delete (exact)")
	yes := fs.Bool("yes", false, "confirm deletion")
	fs.Parse(args)
	if *p == "" || !*yes {
		return fmt.Errorf("usage: aimem drop-project -p <project-id> --yes (deletes journal AND memories)")
	}
	req, err := http.NewRequest("DELETE", "http://aimem/v1/projects/"+url.PathEscape(*p), nil)
	if err != nil {
		return err
	}
	resp, err := client().Do(req)
	if err == nil {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("service refused: %s", strings.TrimSpace(string(body)))
		}
		fmt.Println(strings.TrimSpace(string(body)))
		return nil
	}
	// No service running: drop directly.
	reg, rerr := store.NewRegistry(stateRoot())
	if rerr != nil {
		return rerr
	}
	defer reg.Close()
	if err := reg.Drop(*p); err != nil {
		return err
	}
	os.Remove(filepath.Join(stateRoot(), "curate", *p+".cursor"))
	fmt.Printf("{\"dropped\":%q}\n", *p)
	return nil
}

// docCmd synthesizes (or prints) a knowledge base's design document.
// Explicit runs work on any KB; --all regenerates only groups that opted
// in (feature "doc") and whose facts changed since the stored doc.
func docCmd(args []string) error {
	fs := flag.NewFlagSet("doc", flag.ExitOnError)
	p := fs.String("p", "", "KB project id (or pass a bare group name as the first argument)")
	all := fs.Bool("all", false, "regenerate every group KB with feature \"doc\" enabled")
	force := fs.Bool("force", false, "regenerate even when the doc is newer than every fact")
	show := fs.Bool("show", false, "print the stored document instead of regenerating")
	backend := fs.String("backend", envOr("AIMEM_CURATE_BACKEND", "claude"),
		"synthesis backend: claude | openai")
	model := fs.String("model", "", "synthesis model (default: haiku for claude, AIMEM_CURATE_MODEL for openai)")
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if gid, err := ident.GroupProject(args[0]); err == nil {
			*p = gid
		}
		args = args[1:]
	}
	fs.Parse(args)
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	defer reg.Close()
	var targets []string
	switch {
	case *all:
		ids, err := reg.Projects()
		if err != nil {
			return err
		}
		for _, id := range ids {
			if !strings.HasPrefix(id, "group-") {
				continue
			}
			if db, err := reg.Open(id); err == nil && curate.FeatureEnabled(db, "doc") {
				targets = append(targets, id)
			}
		}
	case *p != "":
		targets = []string{*p}
	default:
		return fmt.Errorf("usage: aimem doc <group>|-p <kb-project> [--all] [--force] [--show]")
	}
	if *show {
		for _, id := range targets {
			db, err := reg.Open(id)
			if err != nil {
				return err
			}
			doc, _ := db.GetMeta("design_doc")
			if doc == "" {
				fmt.Fprintf(os.Stderr, "aimem: %s has no stored document yet (run `aimem doc`)\n", id)
				continue
			}
			fmt.Println(doc)
		}
		return nil
	}
	var syn curate.Synthesizer
	runModel := *model
	switch *backend {
	case "openai":
		if runModel == "" {
			runModel = os.Getenv("AIMEM_CURATE_MODEL")
		}
		if runModel == "" {
			return fmt.Errorf("openai backend needs --model or AIMEM_CURATE_MODEL")
		}
		ep, ok := provider.Resolve(stateRoot(), runModel)
		if !ok || ep.Kind != "openai" {
			return fmt.Errorf("no openai endpoint for model %q: bind it in providers.json or set AIMEM_OPENAI_API_KEY", runModel)
		}
		syn = &curate.OpenAIExtractor{BaseURL: ep.BaseURL, APIKey: ep.Token, Model: ep.Model}
	case "claude":
		workDir := filepath.Join(stateRoot(), "curator-workdir")
		os.MkdirAll(workDir, 0o700)
		if runModel == "" {
			runModel = "haiku"
		}
		syn = &curate.ClaudeExtractor{Model: runModel, WorkDir: workDir}
	default:
		return fmt.Errorf("unknown backend %q (claude|openai)", *backend)
	}
	host, _ := os.Hostname()
	for _, id := range targets {
		db, err := reg.Open(id)
		if err != nil {
			return err
		}
		rep, err := curate.GenerateDoc(db, strings.TrimPrefix(id, "group-"), syn, *force)
		if err != nil {
			if !*all {
				return fmt.Errorf("doc %s: %w", id, err)
			}
			fmt.Fprintf(os.Stderr, "aimem: doc %s: %v\n", id, err)
			continue
		}
		if rep.Usage.InputTokens+rep.Usage.OutputTokens > 0 {
			_ = db.AddCurateRun(&store.CurateRun{
				TS: time.Now().UTC().Format(time.RFC3339), Host: host, Model: runModel,
				InputTokens: rep.Usage.InputTokens, OutputTokens: rep.Usage.OutputTokens,
				CostUSD: rep.Usage.CostUSD,
			})
		}
		out, _ := json.Marshal(rep)
		fmt.Println(string(out))
	}
	return nil
}

// dedupCmd retroactively folds near-identical memories (write-time dedup
// only guards NEW facts; twins from before v0.1.33 stay until swept).
func dedupCmd(args []string) error {
	fs := flag.NewFlagSet("dedup", flag.ExitOnError)
	p := fs.String("p", "", "project id (default: current directory's project)")
	all := fs.Bool("all", false, "sweep every project, including user/group memory DBs")
	sim := fs.Float64("sim", 0.90, "cosine similarity threshold")
	dry := fs.Bool("dry-run", false, "report pairs without folding")
	fs.Parse(args)
	// Vectors are compared per (model, dimension), so the sweep must use
	// the same key the writers used — not the bare model name.
	model := embed.ForRoot(stateRoot()).Key()
	if model == "" {
		return fmt.Errorf("dedup needs AIMEM_EMBED_MODEL (vectors are compared per model)")
	}
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	defer reg.Close()
	var projects []string
	switch {
	case *all:
		if projects, err = reg.Projects(); err != nil {
			return err
		}
	case *p != "":
		projects = []string{*p}
	default:
		id, err := ident.ProjectID(".")
		if err != nil {
			return err
		}
		projects = []string{id}
	}
	for _, proj := range projects {
		db, err := reg.Open(proj)
		if err != nil {
			return err
		}
		res, err := curate.DedupProject(db, model, *sim, *dry)
		if err != nil {
			return fmt.Errorf("dedup %s: %w", proj, err)
		}
		if *all && res.Folded == 0 {
			continue
		}
		fmt.Printf("== %s\n", proj)
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
	}
	return nil
}

// Group config (charter/policy/chapters/features) travels with sync as
// JSONL so every machine and the hub converge on the same routing rules.
// groupConfigKeys moved to store.GroupConfigKeys so the hub's sync
// endpoints and these CLI legs share ONE implementation of the
// fill-only / newest-wins rules (DESIGN-hub-sync).

func exportGroupConfigCmd(args []string) error {
	fs := flag.NewFlagSet("export-group-config", flag.ExitOnError)
	only := fs.String("projects", "", "comma-separated project ids to export (hub-partitioned sync)")
	fs.Parse(args)
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	defer reg.Close()
	ids, err := reg.Projects()
	if err != nil {
		return err
	}
	return store.ExportGroupConfig(reg, filterProjects(ids, *only), json.NewEncoder(os.Stdout))
}

// importGroupConfigCmd fills empty local keys from the peer and warns on
// divergence (meta has no timestamps, so a blind overwrite could undo a
// newer edit; the hub-push-on-write path keeps the hub current anyway).
func importGroupConfigCmd() error {
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	defer reg.Close()
	return importGroupConfigFrom(reg, os.Stdin)
}

// importGroupConfigFrom applies a config stream with the shared
// semantics (store.ImportGroupConfigRecord); messages go to stderr
// exactly as the ssh legs always printed them.
func importGroupConfigFrom(reg *store.Registry, r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec store.ConfigRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if msg, _ := store.ImportGroupConfigRecord(reg, rec); msg != "" {
			fmt.Fprintf(os.Stderr, "aimem: %s\n", msg)
		}
	}
	return sc.Err()
}

// parseTokenCount accepts "500k", "2M", "1500000".
func parseTokenCount(v string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "k"):
		mult, v = 1_000, strings.TrimSuffix(v, "k")
	case strings.HasSuffix(v, "m"):
		mult, v = 1_000_000, strings.TrimSuffix(v, "m")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad token count %q", v)
	}
	return n * mult, nil
}

// parseCapValue accepts combined tokens ("500k", "2M"), separate
// directions ("in:2M", "out:300k", "in:2M,out:300k" — in/out prices and
// limits usually differ), or USD ("$5" / "5usd"). Empty/0 = clear.
func parseCapValue(v string) (*curate.Cap, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" || v == "0" || v == "unlimited" {
		return nil, nil
	}
	if strings.HasPrefix(v, "$") || strings.HasSuffix(v, "usd") {
		f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(v, "$"), "usd"), 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("bad USD cap %q", v)
		}
		return &curate.Cap{USD: f}, nil
	}
	c := &curate.Cap{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "in:"):
			n, err := parseTokenCount(strings.TrimPrefix(part, "in:"))
			if err != nil {
				return nil, err
			}
			c.TokensIn = n
		case strings.HasPrefix(part, "out:"):
			n, err := parseTokenCount(strings.TrimPrefix(part, "out:"))
			if err != nil {
				return nil, err
			}
			c.TokensOut = n
		default:
			n, err := parseTokenCount(part)
			if err != nil {
				return nil, err
			}
			c.Tokens = n
		}
	}
	return c, nil
}

// budgetCmd: show or edit curation spend limits. Default scope is the
// GLOBAL budget (user DB, counts all projects); -p makes a per-project
// override that counts only that project.
func budgetCmd(args []string) error {
	fs := flag.NewFlagSet("budget", flag.ExitOnError)
	p := fs.String("p", "", "project id: set/show a per-project override (default: global)")
	daily := fs.String("daily", "", "daily cap: tokens (500k) or USD ($2); 0=clear")
	weekly := fs.String("weekly", "", "weekly cap")
	monthly := fs.String("monthly", "", "monthly cap")
	unlimited := fs.Bool("unlimited", false, "remove the budget entirely")
	reset := fs.Bool("reset", false, "restart the current windows (history kept)")
	fs.Parse(args)

	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	defer reg.Close()
	scope := store.UserScopeProject
	if *p != "" {
		scope = *p
	}
	db, err := reg.Open(scope)
	if err != nil {
		return err
	}
	// Load current (for edit-in-place semantics).
	var b curate.Budget
	if raw, _ := db.GetMeta("budget"); raw != "" {
		json.Unmarshal([]byte(raw), &b)
	}
	changed := false
	if *unlimited {
		if err := curate.SaveBudget(db, nil); err != nil {
			return err
		}
		fmt.Printf("budget removed (%s)\n", scope)
		return nil
	}
	for _, wc := range []struct {
		val string
		dst **curate.Cap
	}{{*daily, &b.Daily}, {*weekly, &b.Weekly}, {*monthly, &b.Monthly}} {
		if wc.val == "" {
			continue
		}
		cap, err := parseCapValue(wc.val)
		if err != nil {
			return err
		}
		*wc.dst = cap
		changed = true
	}
	if *reset {
		b.Epoch = time.Now().UTC().Format(time.RFC3339)
		changed = true
	}
	if changed {
		if err := curate.SaveBudget(db, &b); err != nil {
			return err
		}
	}
	// Show: caps and current window usage.
	global := *p == ""
	target := db
	if !global {
		target, _ = reg.Open(*p)
	}
	fmt.Printf("budget scope: %s%s\n", scope, map[bool]string{true: " (global, counts all projects)", false: ""}[global])
	if b.Epoch != "" {
		fmt.Printf("epoch (last reset): %s\n", b.Epoch)
	}
	now := time.Now()
	for _, wc := range []struct {
		name string
		cap  *curate.Cap
	}{{"daily", b.Daily}, {"weekly", b.Weekly}, {"monthly", b.Monthly}} {
		inTok, outTok, usd, err := curate.WindowUsage(reg, target, global, wc.name, now, b.Epoch)
		if err != nil {
			return err
		}
		lim := "unlimited"
		if wc.cap != nil {
			var parts []string
			if wc.cap.Tokens > 0 {
				parts = append(parts, fmt.Sprintf("%d tokens", wc.cap.Tokens))
			}
			if wc.cap.TokensIn > 0 {
				parts = append(parts, fmt.Sprintf("in %d", wc.cap.TokensIn))
			}
			if wc.cap.TokensOut > 0 {
				parts = append(parts, fmt.Sprintf("out %d", wc.cap.TokensOut))
			}
			if wc.cap.USD > 0 {
				parts = append(parts, fmt.Sprintf("$%.2f", wc.cap.USD))
			}
			if len(parts) > 0 {
				lim = strings.Join(parts, ", ")
			}
		}
		fmt.Printf("%-8s used %8d in / %7d out / $%.4f   limit: %s\n",
			wc.name, inTok, outTok, usd, lim)
	}
	return nil
}

// submitCmd reads a {"project_id","event"} payload on stdin and submits it
// through the spool-backed adapter path. Always exits 0 on spool fallback
// (fail-open for the coding client).
func submitCmd() error {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return err
	}
	var p adapter.Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("stdin is not a valid payload: %w", err)
	}
	if err := p.ResolveProjectID(); err != nil {
		return err
	}
	_, err = adapter.NewClient(stateRoot()).Submit(&p)
	return err
}

// submitClaudeCmd is the Stop/StopFailure hook entrypoint.
func submitClaudeCmd() error {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return err
	}
	p, err := adapter.BuildClaudeEvent(raw)
	if err != nil {
		return err
	}
	_, err = adapter.NewClient(stateRoot()).Submit(p)
	return err
}

// exportEventsCmd dumps journal events as JSONL payloads. It reads the
// databases directly (WAL allows concurrent readers), so it works whether or
// not the service is running.
func exportEventsCmd(args []string) error {
	fs := flag.NewFlagSet("export-events", flag.ExitOnError)
	p := fs.String("p", "", "project id (default: all projects)")
	only := fs.String("projects", "", "comma-separated project ids to export (hub-partitioned sync)")
	since := fs.String("since", "", "only events with id greater than this cursor")
	fs.Parse(args)
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	defer reg.Close()
	projects := []string{*p}
	if *p == "" {
		if projects, err = reg.Projects(); err != nil {
			return err
		}
		projects = filterProjects(projects, *only)
	}
	for _, id := range projects {
		db, err := reg.Open(id)
		if err != nil {
			return err
		}
		if err := db.DumpSince(os.Stdout, *since); err != nil {
			return err
		}
	}
	return nil
}

// importEventsCmd submits JSONL payloads from stdin through the spool-backed
// adapter path. Idempotency keys make re-imports and overlapping dumps safe.
func importEventsCmd() error {
	submitted, spooled, failed, _, err := importEventsFrom(os.Stdin)
	if err != nil {
		return err
	}
	fmt.Printf(`{"submitted":%d,"spooled":%d,"failed":%d}`+"\n", submitted, spooled, failed)
	if failed > 0 {
		return fmt.Errorf("%d record(s) failed", failed)
	}
	return nil
}

// importEventsFrom lands a JSONL event stream locally — the shared core
// of `import-events` (ssh legs) and the HTTP sync pull.
func importEventsFrom(r io.Reader) (submitted, spooled, failed int, end *int, err error) {
	c := adapter.NewClient(stateRoot())
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// Stream terminator (hub v0.3.24+, requested with ?end=1): carries
		// the count of records the hub actually sent, so the sync path can
		// tell a complete stream from a truncated one. Not an event — never
		// imported. Absent in plain files, harmlessly recognized if present.
		if strings.Contains(line, `"sync_end"`) {
			var t struct {
				SyncEnd bool `json:"sync_end"`
				Events  int  `json:"events"`
			}
			if json.Unmarshal([]byte(line), &t) == nil && t.SyncEnd {
				end = &t.Events
				continue
			}
		}
		var p adapter.Payload
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "aimem: skipping bad line: %v\n", err)
			continue
		}
		// Anti-entropy import: store locally, never re-broadcast. See
		// adapter.SubmitLocal — pushing here would ship a peer's events
		// to THIS machine's default hub and break hub partitioning.
		sp, err := c.SubmitLocal(&p)
		switch {
		case err != nil:
			failed++
			fmt.Fprintf(os.Stderr, "aimem: %v\n", err)
		case sp:
			spooled++
		default:
			submitted++
		}
	}
	return submitted, spooled, failed, end, sc.Err()
}

// syncCmd merges journals with a remote machine over ssh, both directions.
// Events are append-only with globally stable idempotency keys, so each
// direction is a pure union; running sync repeatedly is safe.
func syncCmd(args []string) error {
	usage := `usage: aimem sync <ssh-destination>        sync everything with one peer
       aimem sync --hub <name> [dest]      sync only that hub's projects (dest from hub config)
       aimem sync --all-hubs               sync every hub that has a sync destination`
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	hubName := fs.String("hub", "", "sync only the named hub's bound projects")
	allHubs := fs.Bool("all-hubs", false, "sync every configured hub with a sync destination")
	fs.Parse(args)
	rest := fs.Args()
	root := stateRoot()
	switch {
	case *allHubs:
		hubs, def := adapter.LoadHubs(root)
		if hubs == nil {
			return fmt.Errorf("no hub configured")
		}
		warnOrphanBindings(hubs)
		var errs []string
		for _, name := range slices.Sorted(maps.Keys(hubs)) {
			if err := syncHub(name, hubs[name], def, ""); err != nil {
				if errors.Is(err, errNoSyncTransport) {
					fmt.Fprintf(os.Stderr, "aimem: hub %q has no sync transport — skipped\n", name)
					continue
				}
				errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("sync failed: %s", strings.Join(errs, "; "))
		}
		return nil
	case *hubName != "":
		hubs, def := adapter.LoadHubs(root)
		h, ok := hubs[*hubName]
		if !ok {
			return fmt.Errorf("no hub named %q", *hubName)
		}
		warnOrphanBindings(hubs)
		dest := ""
		if len(rest) > 0 {
			dest = rest[0]
		}
		if err := syncHub(*hubName, h, def, dest); err != nil {
			if errors.Is(err, errNoSyncTransport) {
				return fmt.Errorf("hub %q has neither an API URL nor an ssh sync destination (aimem hub add %s <url> <token> [--sync <ssh-dest>])", *hubName, *hubName)
			}
			return err
		}
		return nil
	case len(rest) >= 1:
		// Legacy whole-registry sync. With several hubs configured this
		// would cross-pollinate their data — refuse and point at --hub.
		if hubs, _ := adapter.LoadHubs(root); len(hubs) > 1 {
			return fmt.Errorf("multiple hubs configured — use `aimem sync --hub <name>` (or --all-hubs) so each hub only receives its own projects")
		}
		return syncOne(rest[0], "", "")
	}
	return fmt.Errorf("%s", usage)
}

// syncOne merges journals, memories, and group config with one ssh peer.
// A non-empty hubName restricts the push legs to the projects bound to
// that hub (see hubProjects) — the partition that keeps separate hubs'
// data separate. Pull legs need no filter: the peer only holds its own.
func syncOne(dest, hubName, def string) error {
	// AIMEM_SSH_OPTS supplies identity/config flags for unattended sync,
	// e.g. "-i ~/.ssh/aimem_hub -o BatchMode=yes".
	sshOpts := os.Getenv("AIMEM_SSH_OPTS")
	remoteBin := os.Getenv("AIMEM_REMOTE_BIN")
	if remoteBin == "" {
		remoteBin = "$HOME/.local/bin/aimem"
	}
	filter := ""
	if hubName != "" {
		reg, err := store.NewRegistry(stateRoot())
		if err != nil {
			return err
		}
		ids, err := hubProjects(reg, hubName, def)
		reg.Close()
		if err != nil {
			return err
		}
		filter = fmt.Sprintf(" --projects %q", strings.Join(ids, ","))
	}
	// Incremental cursors per peer: only events newer than the last synced
	// id cross the wire, minus a 1h overlap window so cross-machine clock
	// skew cannot hide events (idempotent import makes the overlap free).
	pushCur := readCursor(dest, "push")
	pullCur := readCursor(dest, "pull")
	const overlap = time.Hour
	fmt.Fprintf(os.Stderr, "aimem: pushing local events to %s\n", dest)
	// The remote command is single-quoted so $HOME expands on the remote
	// side, not in the local pipeline shell.
	push := exec.Command("bash", "-c",
		fmt.Sprintf(`"%s" export-events --since %q%s | ssh %s %q '%s import-events'`,
			selfPath(), uuidv7.ShiftBack(pushCur, overlap), filter, sshOpts, dest, remoteBin))
	push.Stdout, push.Stderr = os.Stdout, os.Stderr
	if err := push.Run(); err != nil {
		return fmt.Errorf("push failed: %w", err)
	}
	if m, err := localMaxEventID(); err == nil && m != "" {
		writeCursor(dest, "push", m)
	}
	fmt.Fprintf(os.Stderr, "aimem: pulling remote events from %s\n", dest)
	pull := exec.Command("bash", "-c",
		fmt.Sprintf(`ssh %s %q '%s export-events --since "%s"' | "%s" import-events`,
			sshOpts, dest, remoteBin, uuidv7.ShiftBack(pullCur, overlap), selfPath()))
	pull.Stdout, pull.Stderr = os.Stdout, os.Stderr
	if err := pull.Run(); err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}
	// UUIDv7 ids are globally time-ordered, so after a successful pull the
	// local max bounds everything we have seen from this peer too.
	if m, err := localMaxEventID(); err == nil && m != "" {
		writeCursor(dest, "pull", m)
	}
	// Curated memories sync the same way; the import side merges staleness
	// (expiry/supersession wins) so forgotten facts never resurrect.
	fmt.Fprintf(os.Stderr, "aimem: syncing memories with %s\n", dest)
	memPush := exec.Command("bash", "-c",
		fmt.Sprintf(`"%s" export-memories%s | ssh %s %q '%s import-memories'`, selfPath(), filter, sshOpts, dest, remoteBin))
	memPush.Stdout, memPush.Stderr = os.Stdout, os.Stderr
	if err := memPush.Run(); err != nil {
		return fmt.Errorf("memory push failed: %w", err)
	}
	memPull := exec.Command("bash", "-c",
		fmt.Sprintf(`ssh %s %q '%s export-memories' | "%s" import-memories`, sshOpts, dest, remoteBin, selfPath()))
	memPull.Stdout, memPull.Stderr = os.Stdout, os.Stderr
	if err := memPull.Run(); err != nil {
		return fmt.Errorf("memory pull failed: %w", err)
	}
	// Group config (charter/policy/chapters) rides along so routing rules
	// converge everywhere. Best-effort: an old peer binary without these
	// subcommands must not fail the whole sync.
	cfgPush := exec.Command("bash", "-c",
		fmt.Sprintf(`"%s" export-group-config%s | ssh %s %q '%s import-group-config'`, selfPath(), filter, sshOpts, dest, remoteBin))
	cfgPush.Stdout, cfgPush.Stderr = os.Stdout, os.Stderr
	if err := cfgPush.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "aimem: group config push skipped:", err)
	}
	cfgPull := exec.Command("bash", "-c",
		fmt.Sprintf(`ssh %s %q '%s export-group-config' | "%s" import-group-config`, sshOpts, dest, remoteBin, selfPath()))
	cfgPull.Stdout, cfgPull.Stderr = os.Stdout, os.Stderr
	if err := cfgPull.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "aimem: group config pull skipped:", err)
	}
	postSyncEmbed()
	return nil
}

// postSyncEmbed backfills embeddings after a successful sync of either
// transport: freshly pulled facts are BM25-only until embedded, and
// waiting for a manual `aimem embed` would leave hybrid recall blind to
// them. Best-effort — the sync itself already succeeded.
func postSyncEmbed() {
	c := embed.ForRoot(stateRoot())
	if c == nil {
		return
	}
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return
	}
	defer reg.Close()
	if projects, err := reg.Projects(); err == nil {
		if err := embedProjects(reg, c, projects, 64); err != nil {
			fmt.Fprintln(os.Stderr, "aimem: post-sync embed:", err)
		}
	}
}

func cursorPath(dest, dir string) string {
	sum := sha256.Sum256([]byte(dest))
	return filepath.Join(stateRoot(), "sync",
		fmt.Sprintf("%s-%s.cursor", hex.EncodeToString(sum[:6]), dir))
}

func readCursor(dest, dir string) string {
	b, err := os.ReadFile(cursorPath(dest, dir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeCursor(dest, dir, val string) {
	p := cursorPath(dest, dir)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(val+"\n"), 0o600)
}

// localMaxEventID scans all project journals for the newest event id.
func localMaxEventID() (string, error) {
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return "", err
	}
	defer reg.Close()
	ids, err := reg.Projects()
	if err != nil {
		return "", err
	}
	max := ""
	for _, id := range ids {
		db, err := reg.Open(id)
		if err != nil {
			continue
		}
		if m, err := db.MaxEventID(); err == nil && m > max {
			max = m
		}
	}
	return max, nil
}

func selfPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "aimem"
}

// memoryScope resolves -p/-u flags to a project id; -u means the reserved
// cross-project user scope, and a bare command defaults to the cwd project.
func memoryScope(p string, user bool) (string, error) {
	if user {
		return store.UserScopeProject, nil
	}
	if p != "" {
		return p, nil
	}
	return ident.ProjectID(".")
}

func memoryCmd(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	p := fs.String("p", "", "project id (default: current directory's project)")
	u := fs.Bool("u", false, "user scope (cross-project)")
	q := fs.String("q", "", "recall query")
	id := fs.String("id", "", "memory id")
	n := fs.Int("n", 0, "token budget / limit")
	all := fs.Bool("a", false, "include stale memories")
	kind := fs.String("kind", "", "memory kind (fact|decision|convention|preference|solution|reference)")
	tag := fs.String("tag", "", "recall: filter by tag")
	tags := fs.String("tags", "", "remember: comma-separated tags")
	to := fs.String("to", "", "link: target memory id")
	rel := fs.String("rel", "related", "link: relation")
	fs.Parse(args)
	proj, err := memoryScope(*p, *u)
	if err != nil {
		return err
	}
	base := "/v1/projects/" + url.PathEscape(proj) + "/memories"
	text := strings.Join(fs.Args(), " ")
	switch cmd {
	case "remember":
		if text == "" {
			return fmt.Errorf("usage: aimem remember [-p|-u] <text>")
		}
		var tagList []string
		if *tags != "" {
			tagList = strings.Split(*tags, ",")
		}
		return postJSON(base, map[string]any{
			"text": text, "actor": "cli", "kind": *kind, "tags": tagList,
		})
	case "recall":
		if *q == "" {
			return fmt.Errorf("-q <query> is required")
		}
		return getJSON(base + fmt.Sprintf("/recall?q=%s&budget=%d&tag=%s&kind=%s",
			url.QueryEscape(*q), *n, url.QueryEscape(*tag), url.QueryEscape(*kind)))
	case "memories":
		suffix := ""
		if *all {
			suffix = "?stale=1"
		}
		return getJSON(base + suffix)
	case "forget":
		if *id == "" {
			return fmt.Errorf("--id is required")
		}
		return postJSON(base+"/"+url.PathEscape(*id)+"/forget", map[string]any{"actor": "cli"})
	case "supersede":
		if *id == "" || text == "" {
			return fmt.Errorf("usage: aimem supersede [-p|-u] --id <id> <new text>")
		}
		return postJSON(base+"/"+url.PathEscape(*id)+"/supersede",
			map[string]any{"text": text, "actor": "cli"})
	case "link":
		if *id == "" || *to == "" {
			return fmt.Errorf("--id and --to are required")
		}
		return postJSON(base+"/"+url.PathEscape(*id)+"/link",
			map[string]any{"to": *to, "rel": *rel, "actor": "cli"})
	case "pin", "unpin":
		if *id == "" {
			return fmt.Errorf("--id is required")
		}
		return postJSON(base+"/"+url.PathEscape(*id)+"/pin",
			map[string]any{"pinned": cmd == "pin", "actor": "cli"})
	case "untag":
		if *id == "" || *tag == "" {
			return fmt.Errorf("usage: aimem untag [-p|-u] --id <id> --tag <tag>")
		}
		return postJSON(base+"/"+url.PathEscape(*id)+"/untag", map[string]any{"tag": *tag})
	}
	return nil
}

func exportMemoriesCmd(args []string) error {
	fs := flag.NewFlagSet("export-memories", flag.ExitOnError)
	p := fs.String("p", "", "project id (default: all projects)")
	only := fs.String("projects", "", "comma-separated project ids to export (hub-partitioned sync)")
	fs.Parse(args)
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	defer reg.Close()
	projects := []string{*p}
	if *p == "" {
		if projects, err = reg.Projects(); err != nil {
			return err
		}
		projects = filterProjects(projects, *only)
	}
	enc := json.NewEncoder(os.Stdout)
	for _, id := range projects {
		db, err := reg.Open(id)
		if err != nil {
			return err
		}
		if err := db.DumpMemories(enc); err != nil {
			return err
		}
		// Curation run history syncs alongside memories (id-idempotent),
		// so every machine's dashboard sees hub-side maintenance cost.
		runs, err := db.CurateRuns()
		if err != nil {
			return err
		}
		for i := range runs {
			if err := enc.Encode(map[string]any{"project_id": id, "curate_run": runs[i]}); err != nil {
				return err
			}
		}
	}
	return nil
}

func importMemoriesCmd() error {
	imported, failed, err := importMemoriesFrom(os.Stdin)
	if err != nil {
		return err
	}
	fmt.Printf(`{"imported":%d,"failed":%d}`+"\n", imported, failed)
	if failed > 0 {
		return fmt.Errorf("%d memory record(s) failed", failed)
	}
	return nil
}

// importMemoriesFrom merges a JSONL memory/curate-run stream through
// the local service — shared by `import-memories` and the HTTP sync
// pull.
func importMemoriesFrom(r io.Reader) (imported, failed int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			ProjectID string          `json:"project_id"`
			Memory    json.RawMessage `json:"memory"`
			CurateRun json.RawMessage `json:"curate_run"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			failed++
			continue
		}
		var err error
		if len(rec.CurateRun) > 0 {
			err = postJSONQuiet("/v1/projects/"+url.PathEscape(rec.ProjectID)+"/curate-runs/import",
				map[string]any{"run": json.RawMessage(rec.CurateRun)})
		} else {
			err = postJSONQuiet("/v1/projects/"+url.PathEscape(rec.ProjectID)+"/memories/import",
				map[string]any{"memory": json.RawMessage(rec.Memory)})
		}
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "aimem: %v\n", err)
			continue
		}
		imported++
	}
	return imported, failed, sc.Err()
}

func postJSONQuiet(path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := client().Post("http://aimem"+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d on %s", resp.StatusCode, path)
	}
	return nil
}

func mcpCmd(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	p := fs.String("p", "", "project id (default: derived from cwd)")
	fs.Parse(args)
	proj := *p
	var groups []string
	if proj == "" {
		var err error
		if proj, groups, err = mcp.DefaultProject(); err != nil {
			return err
		}
	}
	return mcp.Serve(client(), proj, groups)
}

// curateCmd runs one knowledge-curation pass (async extraction, Phase 5b).
// clipLine keeps a conflict line readable in a terminal log.
func clipLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func curateCmd(args []string) error {
	fs := flag.NewFlagSet("curate", flag.ExitOnError)
	p := fs.String("p", "", "project id (default: current directory's project)")
	dry := fs.Bool("dry-run", false, "print proposals without writing or advancing the cursor")
	maxFacts := fs.Int("max", 10, "max facts per run")
	maxEvents := fs.Int("events", 50, "max journal events consumed per run")
	force := fs.Bool("force", false, "bypass budget caps for this run")
	rounds := fs.Int("rounds", 10, "max passes per project while the journal backlog lasts")
	backend := fs.String("backend", envOr("AIMEM_CURATE_BACKEND", "claude"),
		"extraction backend: claude (headless subscription CLI) or openai (LiteLLM/OpenAI-compatible API)")
	model := fs.String("model", "", "extraction model (default: haiku for claude backend, AIMEM_CURATE_MODEL for openai)")
	all := fs.Bool("all", false, "curate every project journal in the registry (skips user/group memory DBs)")
	fs.Parse(args)
	root := stateRoot()
	reg, err := store.NewRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()
	var projects []string
	switch {
	case *all:
		ids, err := reg.Projects()
		if err != nil {
			return err
		}
		for _, id := range ids {
			// memory-only DBs hold no journal events to distill; the curator's
			// scratch workdir journals its own extraction turns — recursing on
			// those just re-curates the curator
			if id == store.UserScopeProject || strings.HasPrefix(id, "group-") ||
				strings.HasPrefix(id, "curator-workdir-") {
				continue
			}
			// Partitioning is the feature: a copy that arrived here by sync
			// but belongs to another hub must not be curated here too.
			// Only an explicit binding on BOTH sides suppresses a project,
			// so unnamed hubs and unbound projects behave exactly as before.
			if me := os.Getenv("AIMEM_HUB_NAME"); me != "" {
				if db, err := reg.Open(id); err == nil {
					if owner, _ := db.GetMeta("hub"); owner != "" && owner != me {
						fmt.Fprintf(os.Stderr, "aimem: curate skip %s (bound to hub %q, this hub is %q)\n", id, owner, me)
						continue
					}
				}
			}
			projects = append(projects, id)
		}
	case *p != "":
		projects = []string{*p}
	default:
		id, err := ident.ProjectID(".")
		if err != nil {
			return err
		}
		projects = []string{id}
	}
	var ex curate.Extractor
	var runModel string
	// Run the extraction turn from a scratch workdir so the curator's own
	// claude session doesn't checkpoint into a real project's journal.
	claudeEx := func(m string) curate.Extractor {
		workDir := filepath.Join(root, "curator-workdir")
		os.MkdirAll(workDir, 0o700)
		return &curate.ClaudeExtractor{Model: m, WorkDir: workDir}
	}
	m := *model
	if m == "" {
		m = os.Getenv("AIMEM_CURATE_MODEL")
	}
	// A registry binding decides the backend by its provider's kind (so
	// the GUI can select the claude CLI: bind e.g. claude/haiku and pick
	// it as curate model); --backend / AIMEM_CURATE_BACKEND applies only
	// to unbound models. Payloads use the resolved upstream name; run
	// history keeps the local alias for attribution.
	if ep, bound := provider.ResolveBound(root, m); bound {
		if ep.Kind == "claude" {
			ex = claudeEx(ep.Model)
		} else {
			ex = &curate.OpenAIExtractor{BaseURL: ep.BaseURL, APIKey: ep.Token, Model: ep.Model}
		}
		runModel = m
	} else {
		switch *backend {
		case "openai":
			if m == "" {
				return fmt.Errorf("openai backend needs --model or AIMEM_CURATE_MODEL")
			}
			ep, ok := provider.Resolve(root, m)
			if !ok || ep.Kind != "openai" {
				return fmt.Errorf("no openai endpoint for model %q: bind it in providers.json or set AIMEM_OPENAI_API_KEY", m)
			}
			ex = &curate.OpenAIExtractor{BaseURL: ep.BaseURL, APIKey: ep.Token, Model: ep.Model}
			runModel = m
		case "claude":
			if m == "" {
				m = "haiku"
			}
			ex = claudeEx(m)
			runModel = m
		default:
			return fmt.Errorf("unknown backend %q (claude|openai)", *backend)
		}
	}
	// Semantic dedup rides on the embedding config when present; disable
	// or tune with AIMEM_DEDUP_SIM (0 = off, default 0.90).
	dedupSim := 0.90
	if v, err := strconv.ParseFloat(os.Getenv("AIMEM_DEDUP_SIM"), 64); err == nil {
		dedupSim = v
	}
	var embedder curate.Embedder
	embedModel := ""
	if c := embed.ForRoot(root); c != nil && dedupSim > 0 {
		embedder, embedModel = c, c.Key()
	}
	for _, proj := range projects {
		// Active development outruns a single capped pass (one nightly run
		// x 10 facts starves busy projects), so drain the backlog: keep
		// running while full batches keep coming, bounded by --rounds and
		// still subject to the budget gate on every round.
		for round := 1; ; round++ {
			rep, err := curate.Run(reg, root, proj, ex,
				curate.RunOpts{DryRun: *dry, MaxFacts: *maxFacts, MaxEvents: *maxEvents, Model: runModel, Force: *force,
					Embedder: embedder, EmbedModel: embedModel, DedupSim: dedupSim})
			if err != nil {
				if !*all {
					return err
				}
				fmt.Fprintf(os.Stderr, "aimem: curate %s: %v\n", proj, err)
				break
			}
			out, _ := json.MarshalIndent(rep, "", "  ")
			if *all || round > 1 {
				fmt.Printf("== %s (round %d)\n", proj, round)
			}
			fmt.Println(string(out))
			// Loud, not automatic: a guardrail's clean [] and a legitimate
			// "nothing qualifies" look identical, so flag for a human and
			// never rewind the cursor on our own.
			// Every automatic rewrite of an existing fact is announced:
			// a supersession triggered by a similarity heuristic must
			// never happen out of sight.
			for _, c := range rep.Conflicts {
				fmt.Fprintf(os.Stderr, "aimem: curate %s: %s %s (sim %.3f)\n  old: %s\n  new: %s\n",
					proj, c.Action, c.OldID, c.Sim, clipLine(c.OldText), clipLine(c.NewText))
			}
			if !*dry && rep.ZeroYield() {
				fmt.Fprintf(os.Stderr, "aimem: curate %s: ZERO-YIELD run — consumed %d events (%s..%s), wrote nothing; if suspicious, re-curate that window\n",
					proj, rep.EventsRead, rep.FirstEvent, rep.LastEvent)
			}
			// A short batch means the cursor caught up with the journal;
			// dry runs never advance the cursor, so they get one round.
			if *dry || rep.EventsRead < *maxEvents || round >= *rounds {
				break
			}
		}
	}
	return nil
}

// embedCmd backfills memory embeddings — the async, egress-gated half of
// semantic recall (the query is embedded at recall time by the server).
func embedCmd(args []string) error {
	fs := flag.NewFlagSet("embed", flag.ExitOnError)
	p := fs.String("p", "", "project id (default: current directory's project)")
	all := fs.Bool("all", false, "embed every project, including user/group memory DBs")
	batch := fs.Int("batch", 64, "texts per embeddings request")
	fs.Parse(args)
	root := stateRoot()
	c := embed.ForRoot(root)
	if c == nil {
		return fmt.Errorf("embedding not configured: set AIMEM_EMBED_MODEL and bind the model in providers.json or set AIMEM_OPENAI_API_KEY")
	}
	reg, err := store.NewRegistry(root)
	if err != nil {
		return err
	}
	defer reg.Close()
	var projects []string
	switch {
	case *all:
		if projects, err = reg.Projects(); err != nil {
			return err
		}
	case *p != "":
		projects = []string{*p}
	default:
		id, err := ident.ProjectID(".")
		if err != nil {
			return err
		}
		projects = []string{id}
	}
	return embedProjects(reg, c, projects, *batch)
}

// embedProjects backfills pending memory embeddings for the given
// projects; shared by `embed` and the post-sync backfill. Spend is
// budget-gated per batch and recorded into the run history under the
// embedding model's name.
func embedProjects(reg *store.Registry, c *embed.Client, projects []string, batch int) error {
	host, _ := os.Hostname()
	for _, proj := range projects {
		db, err := reg.Open(proj)
		if err != nil {
			return err
		}
		total, tokens := 0, int64(0)
		for {
			targets, err := db.NeedingEmbedding(c.Key(), batch)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				break
			}
			// Budget gate: worst-case ~150 tokens per fact sentence. USD
			// caps charge these at AIMEM_PRICE_IN (chat input price) —
			// above real embedding prices, so never an undercount.
			if budget, owned, err := curate.LoadBudget(reg, db); err != nil {
				return err
			} else if !budget.Empty() {
				proj := curate.Projection{In: int64(len(targets)) * 150}
				if pin, perr := strconv.ParseFloat(os.Getenv("AIMEM_PRICE_IN"), 64); perr == nil {
					proj.USD, proj.Priced = float64(proj.In)/1e6*pin, true
				}
				if err := curate.CheckBudget(reg, db, budget, !owned, proj, time.Now()); err != nil {
					return err
				}
			}
			texts := make([]string, len(targets))
			for i, t := range targets {
				texts[i] = t.Text
			}
			vecs, used, err := c.Embed(texts)
			tokens += used
			if err != nil {
				return fmt.Errorf("embed %s: %w", proj, err)
			}
			for i, t := range targets {
				if err := db.SetEmbedding(t.ID, c.Key(), len(vecs[i]), embed.Encode(vecs[i])); err != nil {
					return err
				}
			}
			total += len(targets)
		}
		if total > 0 {
			_ = db.AddCurateRun(&store.CurateRun{
				TS: time.Now().UTC().Format(time.RFC3339), Host: host,
				Model: c.Model, Written: total, InputTokens: tokens,
			})
		}
		fmt.Printf("%s: %d embedded (model %q)\n", proj, total, c.Key())
	}
	return nil
}

func hubCmd(args []string) error {
	root := stateRoot()
	// `hub list` is what both humans and agents guess first — alias it.
	if len(args) == 1 && (args[0] == "list" || args[0] == "ls") {
		args = nil
	}
	if len(args) == 0 {
		hubs, def := adapter.LoadHubs(root)
		if hubs == nil {
			fmt.Println("no hub configured")
			return nil
		}
		for _, name := range slices.Sorted(maps.Keys(hubs)) {
			h := hubs[name]
			mark := " "
			if name == def {
				mark = "*"
			}
			line := fmt.Sprintf("%s %-12s %s  token:%s...", mark, name, h.URL, h.Token[:min(6, len(h.Token))])
			if h.Sync != "" {
				line += "  sync:" + h.Sync
			}
			if h.Insecure {
				line += "  [insecure: self-signed]"
			}
			fmt.Println(line)
		}
		if len(hubs) > 1 {
			fmt.Println("\nprojects bind to a named hub via .aimem.json {\"hub\":\"<name>\"}; unbound projects use the default (*)")
		}
		return nil
	}
	usage := `usage: aimem hub <url> <token>                      set/replace the default hub
       aimem hub add <name> <url> <token> [--sync <ssh-dest>] [--default]
       aimem hub rm <name>
       aimem hub default <name>`
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("hub add", flag.ExitOnError)
		syncDest := fs.String("sync", "", "ssh destination for `aimem sync --hub <name>` (e.g. aimem@hub.example.com)")
		makeDefault := fs.Bool("default", false, "make this hub the default for unbound projects")
		insecure := fs.Bool("insecure", false, "skip TLS verification (hub still on its self-signed cert)")
		rest := args[1:]
		var pos []string
		// flag package stops at the first non-flag arg; accept flags after
		// the three positionals too.
		for len(rest) > 0 {
			if strings.HasPrefix(rest[0], "-") {
				fs.Parse(rest)
				rest = fs.Args()
				continue
			}
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
		if len(pos) != 3 {
			return fmt.Errorf("%s", usage)
		}
		name := pos[0]
		if _, err := ident.GroupProject(name); err != nil {
			return fmt.Errorf("invalid hub name %q (want lowercase letters, digits, dashes)", name)
		}
		hubs, def := adapter.LoadHubs(root)
		if hubs == nil {
			hubs = map[string]*adapter.HubConfig{}
		}
		hubs[name] = &adapter.HubConfig{URL: strings.TrimRight(pos[1], "/"), Token: pos[2], Sync: *syncDest, Insecure: *insecure}
		if *makeDefault || def == "" {
			def = name
		}
		if err := adapter.SaveHubs(root, hubs, def); err != nil {
			return err
		}
		fmt.Printf("hub %q configured (default: %s)\n", name, def)
		return nil
	case "rm":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		hubs, def := adapter.LoadHubs(root)
		if _, ok := hubs[args[1]]; !ok {
			return fmt.Errorf("no hub named %q", args[1])
		}
		delete(hubs, args[1])
		if def == args[1] {
			def = ""
			if len(hubs) == 1 {
				for k := range hubs {
					def = k
				}
			}
		}
		if err := adapter.SaveHubs(root, hubs, def); err != nil {
			return err
		}
		fmt.Printf("hub %q removed\n", args[1])
		if len(hubs) > 1 && def == "" {
			fmt.Println("WARNING: no default hub set — unbound projects will not push; run `aimem hub default <name>`")
		}
		return nil
	case "default":
		if len(args) != 2 {
			return fmt.Errorf("%s", usage)
		}
		hubs, _ := adapter.LoadHubs(root)
		if _, ok := hubs[args[1]]; !ok {
			return fmt.Errorf("no hub named %q", args[1])
		}
		return adapter.SaveHubs(root, hubs, args[1])
	}
	if len(args) != 2 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("%s", usage)
	}
	if err := adapter.SaveHub(root, &adapter.HubConfig{
		URL: strings.TrimRight(args[0], "/"), Token: args[1],
	}); err != nil {
		return err
	}
	fmt.Println("hub configured; checkpoints will push in real time")
	return nil
}

// hubNamesForGroup returns the distinct hub bindings ("" = default hub)
// of a group's member projects, so group-config edits reach every hub
// that will curate this group. No members known locally → default hub.
func hubNamesForGroup(reg *store.Registry, gid string) []string {
	ids, err := reg.Projects()
	if err != nil {
		return []string{""}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "user" || strings.HasPrefix(id, "group-") {
			continue
		}
		db, err := reg.OpenExisting(id)
		if err != nil {
			continue
		}
		raw, _ := db.GetMeta("groups")
		var gs []string
		if raw == "" || json.Unmarshal([]byte(raw), &gs) != nil || !slices.Contains(gs, gid) {
			continue
		}
		h, _ := db.GetMeta("hub")
		seen[h] = true
	}
	if len(seen) == 0 {
		return []string{""}
	}
	return slices.Sorted(maps.Keys(seen))
}

// hubProjects partitions the registry for one hub: the projects bound to
// hubName (meta "hub"; unbound projects belong to the default hub), plus
// the user DB and the group DBs those projects declare. This is the sync
// boundary that keeps different hubs' data physically separate.
func hubProjects(reg *store.Registry, hubName, def string) ([]string, error) {
	ids, err := reg.Projects()
	if err != nil {
		return nil, err
	}
	var out []string
	groups := map[string]bool{}
	for _, id := range ids {
		if id == "user" || strings.HasPrefix(id, "group-") {
			continue
		}
		db, err := reg.OpenExisting(id)
		if err != nil {
			continue
		}
		mh, _ := db.GetMeta("hub")
		if !(mh == hubName || (mh == "" && hubName == def)) {
			continue
		}
		out = append(out, id)
		raw, _ := db.GetMeta("groups")
		var gs []string
		if raw != "" && json.Unmarshal([]byte(raw), &gs) == nil {
			for _, g := range gs {
				groups[g] = true
			}
		}
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "group-") && groups[id] {
			out = append(out, id)
		}
	}
	out = append(out, "user")
	slices.Sort(out)
	return out, nil
}

func spoolFlushCmd() error {
	replayed, requeued, err := adapter.NewClient(stateRoot()).ReplaySpool()
	if err != nil {
		return err
	}
	fmt.Printf(`{"replayed":%d,"requeued":%d}`+"\n", replayed, requeued)
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
