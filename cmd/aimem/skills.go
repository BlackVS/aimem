package main

// The hub-served skills set (docs/DESIGN-portable-team-context.md,
// increment 1e). `aimem skills import` runs on the hub host as the admin
// and sends a skills release, or the instance's own skills, from an
// archive verified by its SHA256SUMS to the hub, which stores it with its
// digest; `aimem skills export` writes that archive pair back from the
// hub. `aimem skills install` runs on a member machine, in a project
// checkout: it reads the skills the project's process requires, at the
// pinned versions, from the hub under the checkout's own credential (in a
// team conversation, AIMEM_TEAM_SESSION, as that conversation under the
// team's grant) and writes them where the agent clients on this machine
// read skills. It
// reads nothing from the internet. `aimem skills list` prints the hub's
// effective set (instance over release by name) with each skill's source.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aimem/internal/process"
	"aimem/internal/processctx"
	"aimem/internal/skills"
	"aimem/internal/teamsession"
	"aimem/internal/wiring"
)

const skillsUsage = `usage: aimem skills import --from-archive <file> [--sums <file>] [--source release|instance] [--version <v>]
       aimem skills export [--source release|instance] [--out <dir>]
       aimem skills list [-p <project>] [--hub]
       aimem skills install [-p <project>] [--user] [--tool <claude,codex,opencode,openhands>] [--all]

import and export run on the hub host (admin). import reads the skills of
a tar.gz (skills/<name>/… under one leading directory, as a skills release
archive is laid out, with its VERSION file; --version names it otherwise)
verified against the SHA256SUMS beside it (--sums names another file),
and stores them on the hub as the release set (default) or the instance
set; the hub keeps the last 5 imports per source. export writes the
current set of a source and its SHA256SUMS line. list and install run on
a member machine in a project checkout (or with -p): list prints the
effective set, instance over release by name, with each skill's version
and source (--hub prints the hub host's own index over the local socket);
install writes the skills the project's process requires, at the pinned
versions, into the skills directories of the agent clients found on this
machine (--tool names them; --user installs for the user instead of the
checkout; --all installs every effective skill) and records each one's
version and source beside them. With AIMEM_TEAM_SESSION set, list and
install read the hub as that team conversation, under the team's grant,
and never with the checkout's credential.`

func skillsCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", skillsUsage)
	}
	switch args[0] {
	case "import":
		fs := flag.NewFlagSet("skills import", flag.ExitOnError)
		archive := fs.String("from-archive", "", "the tar.gz to import")
		sums := fs.String("sums", "", "the SHA256SUMS file that names the archive (default: SHA256SUMS beside it)")
		source := fs.String("source", skills.SourceRelease, "release or instance")
		version := fs.String("version", "", "the set's version (default: the archive's VERSION file)")
		fs.Parse(args[1:])
		if *archive == "" {
			return fmt.Errorf("%s", skillsUsage)
		}
		return skillsImport(*archive, *sums, *source, *version)
	case "export":
		fs := flag.NewFlagSet("skills export", flag.ExitOnError)
		source := fs.String("source", skills.SourceRelease, "release or instance")
		out := fs.String("out", ".", "the directory to write the archive and SHA256SUMS into")
		fs.Parse(args[1:])
		return skillsExport(*source, *out)
	case "list":
		fs := flag.NewFlagSet("skills list", flag.ExitOnError)
		p := projectFlag(fs, "project id (default: current directory's project)")
		hub := fs.Bool("hub", false, "print the hub host's own index over the local socket (admin)")
		fs.Parse(args[1:])
		if *hub {
			return getJSON("/v1/skills")
		}
		return skillsList(".", *p)
	case "install":
		fs := flag.NewFlagSet("skills install", flag.ExitOnError)
		p := projectFlag(fs, "project id (default: current directory's project)")
		user := fs.Bool("user", false, "install for the user (the clients' user-level skills directories) instead of the checkout")
		tool := fs.String("tool", "", "comma-separated clients to install for (default: the clients found on PATH)")
		all := fs.Bool("all", false, "install every effective skill, not only the ones the process requires")
		fs.Parse(args[1:])
		return skillsInstall(".", *p, *user, *tool, *all)
	}
	return fmt.Errorf("%s", skillsUsage)
}

// skillsImport verifies and reads the archive on this machine and sends
// the set to the hub over the local socket.
func skillsImport(archive, sums, source, version string) error {
	if !skills.ValidSource(source) {
		return errors.New("--source is release or instance")
	}
	if sums == "" {
		sums = filepath.Join(filepath.Dir(archive), "SHA256SUMS")
	}
	set, err := skills.ReadArchiveFile(archive, sums, source, version)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "importing %s set %s (%d skills, %d files, %d bytes): %s\n", source, set.Version, len(set.Names()), len(set.Files), set.Bytes(), strings.Join(set.Names(), ", "))
	raw, _ := json.Marshal(map[string]any{"version": set.Version, "files": set.Files})
	req, err := http.NewRequest(http.MethodPut, "http://aimem/v1/skills/"+source, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client().Do(req)
	if err != nil {
		return fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	fmt.Println(strings.TrimSpace(string(data)))
	return nil
}

// skillsExport writes the hub's current set of a source as the archive
// import reads, with its SHA256SUMS line.
func skillsExport(source, out string) error {
	if !skills.ValidSource(source) {
		return errors.New("--source is release or instance")
	}
	resp, err := client().Get("http://aimem/v1/skills/" + source)
	if err != nil {
		return fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 3*skills.MaxSetBytes))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var body struct {
		Version string            `json:"version"`
		Digest  string            `json:"digest"`
		Files   map[string][]byte `json:"files"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return err
	}
	set := &skills.Set{Source: source, Version: body.Version, Files: body.Files}
	if set.Digest() != body.Digest {
		return errors.New("the service's answer does not digest to the digest it states")
	}
	archivePath, sumsPath, err := writeSkillsExport(set, out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s and its line in %s\n", archivePath, sumsPath)
	return nil
}

// writeSkillsExport writes skills-<source>-<version>.tar.gz and its line
// in SHA256SUMS (replacing an earlier line for the same name).
func writeSkillsExport(set *skills.Set, out string) (archivePath, sumsPath string, err error) {
	var buf bytes.Buffer
	if err := skills.WriteArchive(&buf, set); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", "", err
	}
	archivePath = filepath.Join(out, "skills-"+set.Source+"-"+set.Version+".tar.gz")
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		return "", "", err
	}
	sumsPath = filepath.Join(out, "SHA256SUMS")
	line := skills.SumsLine(buf.Bytes(), archivePath)
	existing, _ := os.ReadFile(sumsPath)
	var kept []string
	for _, l := range strings.Split(string(existing), "\n") {
		if f := strings.Fields(l); len(f) == 2 && strings.TrimPrefix(f[1], "*") != filepath.Base(archivePath) {
			kept = append(kept, l)
		}
	}
	kept = append(kept, strings.TrimRight(line, "\n"))
	if err := os.WriteFile(sumsPath, []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return archivePath, sumsPath, nil
}

// skillsConn is where the skills commands read the hub: as the team
// conversation AIMEM_TEAM_SESSION names, under the team's grant, or with
// the checkout's own credential. In a team conversation nothing falls back
// to the checkout's credential.
type skillsConn struct {
	client *skills.Client
	set    *process.Set // the project's process, when asked for
	// grant is the next step when the hub refuses a skills read.
	grant string
	// who names the team conversation a refusal came in; "" in personal mode.
	who string
}

const (
	personalSkillsGrant = "check the checkout's credential and its grant on the project"
	teamSkillsGrant     = processctx.TeamGrantFix
)

// skillsConnect connects for the project in dir (or projectID); with
// withProcess it also reads the project's process, whose manifest pins
// the skills.
func skillsConnect(dir, projectID string, withProcess bool) (*skillsConn, error) {
	if path := os.Getenv(teamsession.EnvVar); path != "" {
		set, c, who, err := processctx.TeamSkills(path, dir, stateRoot(), projectID, withProcess)
		if err != nil {
			return nil, err
		}
		return &skillsConn{client: c, set: set, grant: teamSkillsGrant, who: who + ": "}, nil
	}
	conn := &skillsConn{grant: personalSkillsGrant}
	if withProcess {
		r := processctx.Load(dir, stateRoot(), projectID)
		if r.Set == nil {
			return nil, fmt.Errorf("the project's process is not available (%s): %s; %s", r.State, r.Detail, r.Fix)
		}
		conn.set = r.Set
	}
	c, r := processctx.SkillsClient(dir, stateRoot(), projectID)
	if c == nil {
		return nil, fmt.Errorf("%s: %s (%s)", r.State, r.Detail, r.Fix)
	}
	conn.client = c
	return conn, nil
}

// index reads the project's skills index and turns a refusal into words.
func (conn *skillsConn) index() (*skills.IndexAnswer, error) {
	c := conn.client
	ctx, cancel := skills.Deadline()
	defer cancel()
	ix, outcome, err := c.Index(ctx)
	switch outcome {
	case skills.OutcomeDenied:
		return nil, fmt.Errorf("%sthe hub refused the skills read for project %s: %v; %s (a hub from before hub-served skills refuses this route the same way: compare the hub's version)", conn.who, c.Project, err, conn.grant)
	case skills.OutcomeUnsupported:
		return nil, fmt.Errorf("this hub does not serve skills (%v): upgrade the hub, or install the skills with the installers for now", err)
	case skills.OutcomeFailed:
		return nil, err
	}
	return ix, nil
}

// skillsList prints the effective set with each skill's version, source
// and what this machine has of it.
func skillsList(dir, projectID string) error {
	conn, err := skillsConnect(dir, projectID, false)
	if err != nil {
		return err
	}
	ix, err := conn.index()
	if err != nil {
		return err
	}
	for _, src := range skills.Sources {
		if e := ix.Sources[src]; e != nil {
			fmt.Printf("%s set: %s (%d skills, imported %s)\n", src, e.Version, len(e.Skills), e.ImportedAt)
		} else {
			fmt.Printf("%s set: none imported\n", src)
		}
	}
	if len(ix.Effective) == 0 {
		fmt.Println("effective skills: none")
		return nil
	}
	home, _ := os.UserHomeDir()
	state := process.SkillStateOn(dir, home)
	fmt.Println("effective skills (instance over release by name):")
	for _, s := range ix.Effective {
		st := state(s.Name)
		here := "not installed here"
		switch {
		case st.Installed && st.Version == "":
			here = "installed here, version unknown"
		case st.Installed && st.Version == s.Version:
			here = "installed here"
		case st.Installed:
			here = "installed here at " + st.Version
		}
		fmt.Printf("  %s %s (%s; %s)\n", s.Name, s.Version, s.Source, here)
	}
	return nil
}

// skillsInstall installs the required (or every effective) skill from the
// hub into the clients' skills directories.
func skillsInstall(dir, projectID string, user bool, tool string, all bool) error {
	conn, err := skillsConnect(dir, projectID, !all)
	if err != nil {
		return err
	}
	var pins []process.SkillPin
	if !all {
		pins = conn.set.Manifest.Skills
		if len(pins) == 0 {
			fmt.Println("the selected process requires no skills; nothing to install (--all installs every effective skill)")
			return nil
		}
	}
	c := conn.client
	ix, err := conn.index()
	if err != nil {
		return err
	}
	var picks []skills.Skill
	if all {
		picks = ix.Effective
	} else {
		var problems []string
		picks, problems = skills.Resolve(ix, pins)
		if len(problems) > 0 {
			return errors.New("not every required skill can be installed from the hub:\n  " + strings.Join(problems, "\n  "))
		}
	}
	if len(picks) == 0 {
		fmt.Println("no skills to install: the hub has none imported")
		return nil
	}
	home, _ := os.UserHomeDir()
	targets, err := skillsTargets(dir, home, user, tool)
	if err != nil {
		return err
	}
	cache := skills.NewStore(stateRoot())
	ctx, cancel := skills.Deadline()
	defer cancel()
	fetched := map[string]map[string][]byte{}
	for _, p := range picks {
		if _, ok := fetched[p.Digest]; ok {
			continue
		}
		files, outcome, err := c.Fetch(ctx, cache, p.Digest)
		switch outcome {
		case skills.OutcomeDenied:
			return fmt.Errorf("%sthe hub refused the skills set read: %v; %s", conn.who, err, conn.grant)
		case skills.OutcomeUnsupported, skills.OutcomeFailed:
			return fmt.Errorf("set %s: %v", p.Digest[:12], err)
		}
		fetched[p.Digest] = files
	}
	cache.Trim(10)
	done, err := skills.Install(targets, picks, func(d string) (map[string][]byte, error) {
		files, ok := fetched[d]
		if !ok {
			return nil, fmt.Errorf("set %s was not fetched", d[:12])
		}
		return files, nil
	}, time.Now())
	for _, d := range done {
		verb := "installed"
		if d.Replaced {
			verb = "replaced"
		}
		fmt.Printf("%s %s %s (%s) for %s: %s\n", verb, d.Name, d.Version, d.Source, d.Client, d.Dir)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%d skill(s) into %d location(s); each location's %s records the versions and sources\n", len(picks), len(targets), process.InstalledRecordName)
	return nil
}

// skillsTargets lists the directories to install into: one per client
// named by --tool, or per client found on PATH.
func skillsTargets(dir, home string, user bool, tool string) ([]skills.Target, error) {
	var names []string
	if tool != "" {
		for _, n := range strings.Split(tool, ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if _, ok := wiring.InstallDir(dir, home, n, false); !ok {
				return nil, fmt.Errorf("unknown client %q in --tool (known: %s)", n, strings.Join(wiring.InstallClients, ", "))
			}
			names = append(names, n)
		}
	} else {
		for _, c := range wiring.DetectClients() {
			names = append(names, c.Name)
		}
		sort.Strings(names)
	}
	if len(names) == 0 {
		return nil, errors.New("no agent client found on PATH; name the clients to install for with --tool (" + strings.Join(wiring.InstallClients, ", ") + ")")
	}
	var out []skills.Target
	seen := map[string]bool{}
	for _, n := range names {
		d, ok := wiring.InstallDir(dir, home, n, user)
		if !ok {
			return nil, fmt.Errorf("no %s skills directory for %s on this machine", map[bool]string{true: "user-level", false: "project"}[user], n)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		t := skills.Target{Client: n, Dir: d}
		if !user {
			t.Root = dir // a project directory is never written through a link
		}
		out = append(out, t)
	}
	return out, nil
}
