package server

// Task 01a1dab7-9c63: a grant carries its access. A read grant lets every
// token of its user read the project's tasks, knowledge and repository and
// write nothing; a write grant also lets user- and project-scoped tokens
// write; without a grant, or after its removal, neither.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
)

// grantRig is the knowledge rig with alpha's repository set and, per grant
// case, a user holding a user-, a project- and a read-only-scope token.
type grantRig struct {
	*knowledgeRig
	keys int
}

func newGrantRig(t *testing.T) *grantRig {
	t.Helper()
	g := &grantRig{knowledgeRig: newKnowledgeRig(t)}
	body := `{"kind":"github","url":"https://github.com/example/example.git","access":"write"}`
	if r := g.call(t, g.tls, "PUT", "/v1/projects/alpha/repository", g.env, nil, body, true); r.status != 200 {
		t.Fatalf("set the repository: %d %s", r.status, r.body)
	}
	// The process reference and its set, and a release skills set: the
	// other reads the project read rule guards.
	sel := withSet(t, `{"repo":"https://example.com/process.git","commit":"`+strings.Repeat("1", 40)+`","manifest":"m.json","expected_commit":""}`, "rules")
	if r := g.call(t, g.tls, "PUT", "/v1/projects/alpha/process", g.env, nil, sel, true); r.status != 200 {
		t.Fatalf("select the process: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "PUT", "/v1/skills/release", g.env, nil, skillsBody("1.0", map[string]string{"grant-skill/SKILL.md": "grant skill"}), true); r.status != 200 {
		t.Fatalf("import the skills: %d %s", r.status, r.body)
	}
	return g
}

// tokens creates a user and issues its three tokens; grant runs between,
// so a project-scoped token can be issued under the grant it is to test.
func (g *grantRig) tokens(t *testing.T, name string, grant func(user string)) map[string]string {
	t.Helper()
	u, err := g.db.CreateUser("admin", name)
	if err != nil {
		t.Fatal(err)
	}
	if grant != nil {
		grant(u.ID)
	}
	out := map[string]string{}
	exp := time.Now().Add(time.Hour)
	for scope, project := range map[access.TokenScope]string{access.ScopeUser: "", access.ScopeReadOnly: "", access.ScopeProject: g.alphaInstance} {
		if scope == access.ScopeProject && grant == nil {
			continue // issuance needs a grant on the project
		}
		_, secret, err := g.db.IssueScoped("admin", u.ID, name+"-"+string(scope), scope, project, exp)
		if err != nil {
			t.Fatalf("%s %s token: %v", name, scope, err)
		}
		g.secrets = append(g.secrets, secret)
		out[string(scope)] = secret
	}
	return out
}

// grantReads are alpha's task, knowledge, repository, process and skills
// reads; each answers 200 when allowed.
var grantReads = []string{
	"/v1/projects/alpha/tasks",
	"/v1/tasks/{task}",
	"/v1/projects/alpha/memories/recall?q=convention",
	"/v1/projects/alpha/docs",
	"/v1/projects/alpha/docs/RUNBOOK",
	"/v1/projects/alpha/repository",
	"/v1/projects/alpha/process",
	"/v1/projects/alpha/process/set",
	"/v1/projects/alpha/skills",
	"/v1/projects/alpha/skills/set?source=release",
}

// leaks are strings only an allowed read's answer carries.
var leaks = []string{"runbook", "example.git", "process.git", "grant skill", "grant-skill"}

func (g *grantRig) canRead(t *testing.T, bearer, path string) bool {
	t.Helper()
	r := g.personalGet(t, bearer, strings.ReplaceAll(path, "{task}", g.alphaTask))
	switch r.status {
	case 200:
		return true
	case 403, 404:
		for _, leak := range leaks {
			if strings.Contains(string(r.body), leak) {
				t.Errorf("%s: a refusal carries content: %s", path, r.body)
			}
		}
		return false
	}
	t.Fatalf("%s: %d %s", path, r.status, r.body)
	return false
}

// canWrite tries a task write and a comment in alpha.
func (g *grantRig) canWrite(t *testing.T, bearer string) (bool, bool) {
	t.Helper()
	g.keys++
	key := map[string]string{"Idempotency-Key": "grant-" + strconv.Itoa(g.keys), "Content-Type": "application/json"}
	task := g.call(t, g.tls, "POST", "/v1/projects/alpha/tasks", bearer, key, taskBody, true)
	g.keys++
	key["Idempotency-Key"] = "grant-" + strconv.Itoa(g.keys)
	comment := g.call(t, g.tls, "POST", "/v1/tasks/"+g.alphaTask+"/comments", bearer, key, `{"body":"hi"}`, true)
	for _, r := range []identityResp{task, comment} {
		if r.status != 200 && r.status != 201 && r.status != 403 {
			t.Fatalf("write: %d %s", r.status, r.body)
		}
	}
	return task.status != 403, comment.status != 403
}

func TestReadGrantReadsAndWritesNothing(t *testing.T) {
	g := newGrantRig(t)
	grant := func(a access.GrantAccess) func(string) {
		return func(user string) {
			if err := g.db.SetGrantAccess("admin", g.alphaInstance, "user", user, a); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name        string
		grant       func(string)
		after       func(user string) // applied after the tokens are issued
		read, write map[string]bool   // by token scope
	}{
		{name: "read grant", grant: grant(access.GrantRead),
			read:  map[string]bool{"user": true, "project": true, "read-only": true},
			write: map[string]bool{}},
		{name: "write grant", grant: grant(access.GrantWrite),
			read:  map[string]bool{"user": true, "project": true, "read-only": true},
			write: map[string]bool{"user": true, "project": true}},
		{name: "no grant", read: map[string]bool{}, write: map[string]bool{}},
		{name: "grant removed", grant: grant(access.GrantWrite), after: func(user string) {
			if err := g.db.SetGrant("admin", g.alphaInstance, "user", user, false); err != nil {
				t.Fatal(err)
			}
		}, read: map[string]bool{}, write: map[string]bool{}},
		{name: "write grant lowered to read", grant: grant(access.GrantWrite), after: grant(access.GrantRead),
			read:  map[string]bool{"user": true, "project": true, "read-only": true},
			write: map[string]bool{}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var uid string
			var toks map[string]string
			if c.grant == nil {
				// No grant: no project-scoped token can be issued, so the
				// user- and read-only-scope tokens are the ones there are.
				toks = g.tokens(t, "u"+strconv.Itoa(i), nil)
			} else {
				toks = g.tokens(t, "u"+strconv.Itoa(i), func(u string) { uid = u; c.grant(u) })
			}
			if c.after != nil {
				c.after(uid)
			}
			for scope, bearer := range toks {
				for _, path := range grantReads {
					if got := g.canRead(t, bearer, path); got != c.read[scope] {
						t.Errorf("%s token, %s: read %v, want %v", scope, path, got, c.read[scope])
					}
				}
				task, comment := g.canWrite(t, bearer)
				if task != c.write[scope] || comment != c.write[scope] {
					t.Errorf("%s token: task write %v, comment %v, want %v", scope, task, comment, c.write[scope])
				}
			}
		})
	}
}

// A project-scoped token issued to a user who holds only a read grant reads
// the project and cannot write it; raising the grant to write lets the same
// token write, since the grant decides at use.
func TestProjectTokenUnderAReadGrant(t *testing.T) {
	g := newGrantRig(t)
	var uid string
	toks := g.tokens(t, "auditor", func(u string) {
		uid = u
		if err := g.db.SetGrantAccess("admin", g.alphaInstance, "user", u, access.GrantRead); err != nil {
			t.Fatal(err)
		}
	})
	project := toks["project"]
	if project == "" {
		t.Fatal("no project-scoped token was issued under a read grant")
	}
	for _, path := range grantReads {
		if !g.canRead(t, project, path) {
			t.Errorf("%s: refused", path)
		}
	}
	if task, comment := g.canWrite(t, project); task || comment {
		t.Fatalf("wrote under a read grant: task %v, comment %v", task, comment)
	}
	if err := g.db.SetGrantAccess("admin", g.alphaInstance, "user", uid, access.GrantWrite); err != nil {
		t.Fatal(err)
	}
	if task, comment := g.canWrite(t, project); !task || !comment {
		t.Fatalf("refused under a write grant: task %v, comment %v", task, comment)
	}
}

// A group's read grant reads through membership as a user's does, and a
// write grant held through another group still writes.
func TestReadGrantThroughAGroup(t *testing.T) {
	g := newGrantRig(t)
	readers, err := g.db.CreateGroup("admin", "readers")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.db.SetGrantAccess("admin", g.alphaInstance, "group", readers.ID, access.GrantRead); err != nil {
		t.Fatal(err)
	}
	toks := g.tokens(t, "member", func(u string) {
		if err := g.db.SetMember("admin", readers.ID, u, true); err != nil {
			t.Fatal(err)
		}
	})
	user := toks["user"]
	for _, path := range grantReads {
		if !g.canRead(t, user, path) {
			t.Errorf("%s: refused through the group's read grant", path)
		}
	}
	if task, _ := g.canWrite(t, user); task {
		t.Fatal("wrote through a group's read grant")
	}
	writers, err := g.db.CreateGroup("admin", "writers")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.db.SetGrant("admin", g.alphaInstance, "group", writers.ID, true); err != nil {
		t.Fatal(err)
	}
	snap, _ := g.db.Snapshot()
	for _, u := range snap.Users {
		if u.Name == "member" {
			if err := g.db.SetMember("admin", writers.ID, u.ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	if task, _ := g.canWrite(t, user); !task {
		t.Fatal("a write grant through another group does not write")
	}
}

// The grant route takes ?access=read|write, says which it set, refuses
// another value, and the snapshot and the project's grant list show it.
func TestGrantRouteSetsTheAccess(t *testing.T) {
	g := newGrantRig(t)
	u, err := g.db.CreateUser("admin", "carol")
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/alpha/access/user/" + u.ID
	for _, c := range []struct{ query, want string }{{"?access=read", `"access":"read"`}, {"", `"access":"write"`}, {"?access=write", `"access":"write"`}} {
		r := g.call(t, g.tls, "PUT", path+c.query, g.env, nil, "", true)
		if r.status != 200 || !strings.Contains(string(r.body), c.want) {
			t.Fatalf("PUT%s: %d %s", c.query, r.status, r.body)
		}
		list := g.call(t, g.tls, "GET", "/v1/projects/alpha/grants", g.env, nil, "", true)
		if !strings.Contains(string(list.body), `"id":"`+u.ID+`","name":"carol",`+c.want) {
			t.Fatalf("grants after PUT%s: %s", c.query, list.body)
		}
	}
	if r := g.call(t, g.tls, "PUT", path+"?access=admin", g.env, nil, "", true); r.status != 400 {
		t.Fatalf("access=admin: %d %s", r.status, r.body)
	}
	snap := g.call(t, g.tls, "GET", "/v1/access", g.env, nil, "", true)
	if !strings.Contains(string(snap.body), `"subject":"`+u.ID+`","access":"write"`) {
		t.Fatalf("snapshot: %s", snap.body)
	}
}
