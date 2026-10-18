package main

import (
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/server"
	"aimem/internal/store"
)

// projectService serves an isolated hub on the local socket of a temporary
// state root, so the CLI talks to it as the operator on the hub host.
func projectService(t *testing.T) *store.Registry {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	// A short socket path: a test's temporary directory can exceed the
	// platform's socket path limit.
	sockDir, err := os.MkdirTemp("", "aps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	t.Setenv("AIMEM_SOCKET", filepath.Join(sockDir, "s"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	reg, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	s := server.New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { s.Close() })
	ln, err := net.Listen("unix", server.SocketPath(root))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return reg
}

// stdoutOf runs fn and returns what it wrote to standard output.
func stdoutOf(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	err = fn()
	os.Stdout = old
	w.Close()
	return <-done, err
}

func TestProjectRepoSetShowClear(t *testing.T) {
	reg := projectService(t)
	if _, err := reg.Open("example"); err != nil {
		t.Fatal(err)
	}
	out, err := stdoutOf(t, func() error {
		return projectNamespaceCmd([]string{"repo", "set", "--project", "example", "--kind", "github", "--url", "https://github.com/example/example.git"})
	})
	if err != nil || !strings.Contains(out, "project.repository.set") || !strings.Contains(out, "previous: (none)") || !strings.Contains(out, "host github.com, access write") {
		t.Fatalf("set: %v\n%s", err, out)
	}
	// -p is the alias of --project.
	out, err = stdoutOf(t, func() error { return projectNamespaceCmd([]string{"show", "-p", "example"}) })
	if err != nil || !strings.Contains(out, "repository: github https://github.com/example/example.git") ||
		!strings.Contains(out, "process:    (none selected)") || !strings.Contains(out, "grants:     (none)") {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if _, err := stdoutOf(t, func() error {
		return projectNamespaceCmd([]string{"repo", "set", "--project", "example", "--kind", "github", "--url", "https://user:secret@github.com/example/example.git"})
	}); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("a URL with a password was accepted: %v", err)
	}
	out, err = stdoutOf(t, func() error { return projectNamespaceCmd([]string{"repo", "clear", "--project", "example"}) })
	if err != nil || !strings.Contains(out, "project.repository.clear") || !strings.Contains(out, "now:      (none)") {
		t.Fatalf("clear: %v\n%s", err, out)
	}
	if _, err := stdoutOf(t, func() error { return projectNamespaceCmd([]string{"repo", "clear", "--project", "example"}) }); err == nil || !strings.Contains(err.Error(), "no repository is set") {
		t.Fatalf("second clear: %v", err)
	}
	if _, err := stdoutOf(t, func() error { return projectNamespaceCmd([]string{"show", "--project", "nope"}) }); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("unknown project: %v", err)
	}
	for _, args := range [][]string{
		{"repo", "set", "--project", "example", "--kind", "github"},
		{"repo", "set", "--kind", "github", "--url", "https://github.com/x/y.git"},
		{"repo", "clear", "--project", "example", "--kind", "github"},
		{"repo", "show", "--project", "example"},
		{"show"},
		{"show", "--project", "example", "extra"},
		{"rename"},
	} {
		if _, err := stdoutOf(t, func() error { return projectNamespaceCmd(args) }); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// Tasks on with no process is an unfinished setup: `tasks on` says the
// project is not ready for work and names the selection command, with the
// three values explained while the hub uses no process and with the values
// of the processes it already uses once it does; `project show` and
// `project list` mark the project the same way. A project with a process,
// or with tasks off (a project that only journals), gets none of it.
func TestTasksOnWithoutProcessIsAnUnfinishedSetup(t *testing.T) {
	reg := projectService(t)
	for _, p := range []string{"example", "beta"} {
		if _, err := reg.Open(p); err != nil {
			t.Fatal(err)
		}
	}
	run := func(fn func() error) string {
		t.Helper()
		out, err := stdoutOf(t, fn)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	show := func(p string) string {
		return run(func() error { return projectNamespaceCmd([]string{"show", "-p", p}) })
	}
	list := func() string { return run(func() error { return projectNamespaceCmd([]string{"list"}) }) }
	tasks := func(v, p string) string { return run(func() error { return tasksCmd([]string{v, "-p", p}) }) }
	const (
		notReady = "not ready for work: tasks on, process not selected"
		example  = "aimem process select <repo> <commit> <manifest> [--ref <branch>] -p example"
		values   = "<commit>    the full 40-hex commit to follow, e.g. "
	)

	if out := show("example"); !strings.Contains(out, "tasks:      off") || strings.Contains(out, "warning") {
		t.Fatalf("tasks off, no process:\n%s", out)
	}
	if out := tasks("on", "example"); !strings.Contains(out, `tasks on for project "example"`) || !strings.Contains(out, notReady) || !strings.Contains(out, example) || !strings.Contains(out, values) {
		t.Fatalf("tasks on, no process anywhere:\n%s", out)
	}
	if out := show("example"); !strings.Contains(out, "tasks:      on") || !strings.Contains(out, "warning:    "+notReady) || !strings.Contains(out, example) {
		t.Fatalf("show, tasks on, no process:\n%s", out)
	}
	if out := list(); !strings.Contains(out, "example  "+notReady) || !strings.Contains(out, "    "+example) || !strings.Contains(out, "beta     tasks off") {
		t.Fatalf("list, tasks on, no process:\n%s", out)
	}

	commit := strings.Repeat("ab", 20)
	// The selection carries its set: here from an exported archive, as on
	// a hub host without access to the repository.
	archive, _ := exportFixture(t, "https://example.com/process.git", commit, "process/manifest.json")
	run(func() error {
		return processCmd([]string{"select", "https://example.com/process.git", commit, "process/manifest.json", "--ref", "main", "--from-archive", archive, "-p", "example"})
	})
	if out := tasks("on", "example"); strings.Contains(out, "next step") || strings.Contains(out, "not ready") {
		t.Fatalf("tasks on with a process selected:\n%s", out)
	}
	if out := show("example"); !strings.Contains(out, "process:    https://example.com/process.git "+commit) || strings.Contains(out, "warning") {
		t.Fatalf("show, tasks on, process selected:\n%s", out)
	}

	// A second project is offered the process the hub already uses, as a
	// complete command; turning its tasks on selects nothing.
	offered := "aimem process select https://example.com/process.git " + commit + " process/manifest.json --ref main -p beta"
	if out := tasks("on", "beta"); !strings.Contains(out, notReady) || !strings.Contains(out, "as example:") || !strings.Contains(out, offered) || strings.Contains(out, values) {
		t.Fatalf("tasks on, a process in use elsewhere:\n%s", out)
	}
	if out := show("beta"); !strings.Contains(out, "process:    (none selected)") || !strings.Contains(out, offered) {
		t.Fatalf("show, offered process:\n%s", out)
	}
	if out := list(); !strings.Contains(out, "beta     "+notReady) || !strings.Contains(out, offered) || !strings.Contains(out, "example  tasks on, process selected") {
		t.Fatalf("list, one ready and one not:\n%s", out)
	}

	if out := tasks("off", "beta"); strings.Contains(out, "next step") {
		t.Fatalf("tasks off:\n%s", out)
	}
	if out := show("beta"); !strings.Contains(out, "tasks:      off") || strings.Contains(out, "warning") {
		t.Fatalf("show, tasks off:\n%s", out)
	}
	if out := list(); strings.Contains(out, "not ready") || !strings.Contains(out, "beta     tasks off") {
		t.Fatalf("list, tasks off:\n%s", out)
	}
}

// Every command that takes a project registers --project with -p as its
// alias on the same variable.
func TestProjectFlagAlias(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	p := projectFlag(fs, "project id")
	if err := fs.Parse([]string{"-p", "alpha"}); err != nil || *p != "alpha" {
		t.Fatalf("-p: %q %v", *p, err)
	}
	fs = flag.NewFlagSet("x", flag.ContinueOnError)
	p = projectFlag(fs, "project id")
	if err := fs.Parse([]string{"--project", "beta"}); err != nil || *p != "beta" {
		t.Fatalf("--project: %q %v", *p, err)
	}
}
