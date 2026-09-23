package teamguide

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"aimem/docs"
)

// canonical copies the embedded files into a writable map, so a test can
// change one byte and build again.
func canonical(t *testing.T) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	err := fs.WalkDir(docs.TeamGuidance, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(docs.TeamGuidance, p)
		m[p] = &fstest.MapFile{Data: b}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func build(t *testing.T, m fstest.MapFS) *Unit {
	t.Helper()
	u, err := Build(m)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func buildErr(t *testing.T, m fstest.MapFS, want string) {
	t.Helper()
	_, err := Build(m)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want an error containing %q, got %v", want, err)
	}
}

// A JSON template of exactly n bytes.
func template(n int) []byte {
	return []byte(`{"pad":"` + strings.Repeat("x", n-10) + `"}`)
}

func TestEmbeddedUnitIsValidAndComplete(t *testing.T) {
	u, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u.Digest, "sha256:") || len(u.Digest) != len("sha256:")+64 {
		t.Fatalf("digest %q", u.Digest)
	}
	for _, role := range Roles {
		req, err := u.Required(role)
		if err != nil {
			t.Fatal(err)
		}
		// Every prose section of the role, then every template those
		// sections link to: the set is closed over its own references.
		for _, id := range roleSections[role] {
			if !slices.Contains(req, id) {
				t.Errorf("%s misses %s", role, id)
			}
		}
		for _, r := range u.Manifest.References {
			if slices.Contains(req, r.From) && r.Section != "" && !slices.Contains(req, r.Section) {
				t.Errorf("%s: %s links %s, not in the required set", role, r.From, r.Section)
			}
		}
		text, err := u.Role(role, "v9.9.9")
		if err != nil {
			t.Fatal(err)
		}
		if len(text) > MaxRoleBytes {
			t.Errorf("%s renders to %d bytes", role, len(text))
		}
		if !strings.HasSuffix(text, u.Terminator("role "+role, "v9.9.9")+"\n") {
			t.Errorf("%s: no terminator at the end", role)
		}
		for _, id := range req {
			b, _ := u.Section(id)
			if len(b) == 0 || !strings.Contains(text, string(b)) {
				t.Errorf("%s: section %s missing or empty in the rendered set", role, id)
			}
		}
	}
	// The worker set carries the worker's templates, the coordinator's the offer.
	w, _ := u.Required("worker")
	c, _ := u.Required("coordinator")
	for _, id := range []string{"example/profile", "example/decline", "example/block", "example/submit", "example/escalation"} {
		if !slices.Contains(w, id) {
			t.Errorf("worker set misses %s: %v", id, w)
		}
	}
	if !slices.Contains(c, "example/offer") || slices.Contains(c, "worker") || slices.Contains(w, "coordinator") {
		t.Errorf("role sets: coordinator %v worker %v", c, w)
	}
}

// The embedded bytes are the committed files themselves: the playbook
// sections concatenate back to the file, and every template in the
// directory is a section with identical bytes. There is no second copy.
func TestEmbeddedMatchesCanonicalFiles(t *testing.T) {
	u, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(filepath.Join("..", "..", "docs", playbookFile))
	if err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for _, p := range playbookSections {
		b, _ := u.Section(p.id)
		joined = append(joined, b...)
	}
	if !bytes.Equal(joined, disk) {
		t.Fatal("playbook sections do not reassemble docs/TEAM-PLAYBOOKS.md")
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "examples", "team", "*"))
	if err != nil || len(files) == 0 {
		t.Fatal(files, err)
	}
	for _, f := range files {
		id := "example/" + strings.TrimSuffix(filepath.Base(f), ".json")
		want, _ := os.ReadFile(f)
		got, err := u.Section(id)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: embedded section differs from %s (%v)", id, f, err)
		}
	}
	if n := len(u.Manifest.Sections) - len(playbookSections); n != len(files) {
		t.Errorf("%d template sections, %d files", n, len(files))
	}
}

func TestDigestIsStableAndCoversContentAndRoleMap(t *testing.T) {
	emb, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	a, b := build(t, canonical(t)), build(t, canonical(t))
	if a.Digest != b.Digest || a.Digest != emb.Digest {
		t.Fatalf("same content, different digests: %s %s %s", a.Digest, b.Digest, emb.Digest)
	}

	m := canonical(t)
	m["examples/team/accept.json"].Data = bytes.Replace(m["examples/team/accept.json"].Data, []byte(`"generation": 1`), []byte(`"generation": 2`), 1)
	if c := build(t, m); c.Digest == a.Digest {
		t.Error("a changed template byte kept the digest")
	}

	m = canonical(t)
	m[playbookFile].Data = bytes.Replace(m[playbookFile].Data, []byte("Leave cleanly"), []byte("Leave  cleanly"), 1)
	if c := build(t, m); c.Digest == a.Digest {
		t.Error("a changed playbook byte kept the digest")
	}

	// Same bytes, different role mapping: the manifest is digested too.
	saved := roleSections["worker"]
	roleSections["worker"] = append(append([]string{}, saved...), "coordinator")
	c := build(t, canonical(t))
	roleSections["worker"] = saved
	if c.Digest == a.Digest {
		t.Error("a changed role set kept the digest")
	}
	// A CRLF copy (a checkout that bypassed .gitattributes) is refused, not
	// given a platform-specific digest.
	m = canonical(t)
	m["examples/team/accept.json"].Data = bytes.ReplaceAll(m["examples/team/accept.json"].Data, []byte("\n"), []byte("\r\n"))
	buildErr(t, m, "section example/accept contains a carriage return")
	// The build version is not content.
	ra, _ := a.Role("worker", "v1")
	rb, _ := a.Role("worker", "v2")
	if ra == rb || !strings.Contains(rb, a.Digest) {
		t.Error("the rendering must name the version and the unchanged digest")
	}
}

func TestLimitsAreEnforcedAtTheBoundary(t *testing.T) {
	m := canonical(t)
	m["examples/team/big.json"] = &fstest.MapFile{Data: template(MaxSectionBytes)}
	build(t, m) // exactly at the limit, and not required by any role

	m["examples/team/big.json"].Data = template(MaxSectionBytes + 1)
	buildErr(t, m, "section example/big is 16385 bytes, over the 16384-byte section limit")

	// Two large templates linked from the worker section push the worker's
	// required set past its limit; nothing is returned in part.
	m = canonical(t)
	m["examples/team/big-a.json"] = &fstest.MapFile{Data: template(15 << 10)}
	m["examples/team/big-b.json"] = &fstest.MapFile{Data: template(15 << 10)}
	m[playbookFile].Data = bytes.Replace(m[playbookFile].Data, []byte("## Worker playbook\n"),
		[]byte("## Worker playbook\n\nSee [a](examples/team/big-a.json) and [b](examples/team/big-b.json).\n"), 1)
	buildErr(t, m, "role worker required set renders to")

	m = canonical(t)
	for i := range 9 {
		m["examples/team/pad-"+string(rune('a'+i))+".json"] = &fstest.MapFile{Data: template(15 << 10)}
	}
	buildErr(t, m, "over the 131072-byte unit limit")
}

func TestReferencesResolveOrAreInformative(t *testing.T) {
	u, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range u.Manifest.References {
		if (r.Section == "") == (r.Informative == "") {
			t.Errorf("reference %+v must either resolve or be informative", r)
		}
	}

	with := func(extra string) fstest.MapFS {
		m := canonical(t)
		m[playbookFile].Data = bytes.Replace(m[playbookFile].Data, []byte("## Human escalation\n"), []byte("## Human escalation\n\n"+extra+"\n"), 1)
		return m
	}
	buildErr(t, with("[runbook](RECOVERY-RUNBOOK.md)"), "links RECOVERY-RUNBOOK.md, which neither resolves")
	buildErr(t, with("[gone](examples/team/gone.json)"), "links examples/team/gone.json, which is not in the unit")
	buildErr(t, with("[up](../README.md)"), "neither resolves")
	buildErr(t, with("[nowhere](#no-such-heading)"), "neither resolves")
	buildErr(t, with(`[titled](RECOVERY-RUNBOOK.md "the runbook")`), "links RECOVERY-RUNBOOK.md, which neither resolves")
	buildErr(t, with("[angled](<RECOVERY-RUNBOOK.md>)"), "links RECOVERY-RUNBOOK.md, which neither resolves")
	buildErr(t, with("See [the runbook][rb].\n\n[rb]: RECOVERY-RUNBOOK.md"), "links RECOVERY-RUNBOOK.md, which neither resolves")

	u = build(t, with("[back](#worker-playbook) and [web](https://example.com/x.md) and [q](TEAM-AGENT-QUICKSTART.md#one-command-onboarding)"))
	found := map[string]Reference{}
	for _, r := range u.Manifest.References {
		if r.From == "escalation" {
			found[r.Target] = r
		}
	}
	if found["#worker-playbook"].Section != "worker" {
		t.Errorf("anchor: %+v", found["#worker-playbook"])
	}
	if found["TEAM-AGENT-QUICKSTART.md#one-command-onboarding"].Informative == "" {
		t.Errorf("informative: %+v", found)
	}
	if _, ok := found["https://example.com/x.md"]; ok {
		t.Error("an absolute URL is not a unit reference")
	}
}

func TestHeadingsNeedStableIDs(t *testing.T) {
	m := canonical(t)
	m[playbookFile].Data = bytes.Replace(m[playbookFile].Data, []byte("## Worker playbook"), []byte("## Worker guide"), 1)
	buildErr(t, m, `is "Worker guide", the id table expects "Worker playbook"`)

	m = canonical(t)
	m[playbookFile].Data = append(m[playbookFile].Data, []byte("\n## Appendix\n\nmore\n")...)
	buildErr(t, m, "give every second-level heading a stable id")

	m = canonical(t)
	delete(m, "examples/team/offer.json")
	buildErr(t, m, "links examples/team/offer.json, which is not in the unit")

	m = canonical(t)
	m["examples/team/Bad Name.json"] = &fstest.MapFile{Data: []byte("{}")}
	buildErr(t, m, `unexpected template entry "Bad Name.json"`)

	m = canonical(t)
	m["examples/team/broken.json"] = &fstest.MapFile{Data: []byte("{")}
	buildErr(t, m, "template broken.json is not valid JSON")
}

func TestReadsAcceptOnlyKnownIdentifiers(t *testing.T) {
	u, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../TEAM-PLAYBOOKS.md", "docs/TEAM-PLAYBOOKS.md", "https://example.com/x", "Worker", "example/../worker", "", strings.Repeat("a", 65)} {
		if _, err := u.SectionText(id, ""); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
	_, err = u.SectionText("example/nope", "")
	if err == nil || !strings.Contains(err.Error(), "unknown section \"example/nope\"") || !strings.Contains(err.Error(), "example/offer") {
		t.Errorf("unknown id error must name the valid ids: %v", err)
	}
	_, err = u.Role(strings.Repeat("r", 500), "")
	if err == nil || len(err.Error()) > 200 || !strings.Contains(err.Error(), "coordinator or worker") {
		t.Errorf("unknown role error must be bounded and useful: %v", err)
	}
	text, err := u.SectionText("example/offer", "v1")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := u.Section("example/offer")
	if !strings.Contains(text, string(want)) || !strings.HasSuffix(text, u.Terminator("section example/offer", "v1")+"\n") {
		t.Errorf("section text:\n%s", text)
	}
}

// Every read ends with its terminator, the index included, and the index
// payload before it is the manifest JSON.
func TestEveryReadEndsWithItsTerminator(t *testing.T) {
	u, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := u.Index("v1")
	if err != nil {
		t.Fatal(err)
	}
	payload, found := strings.CutSuffix(idx, "\n"+u.Terminator("index", "v1")+"\n")
	if !found || !json.Valid([]byte(payload)) {
		t.Fatalf("index must be the manifest JSON then its terminator:\n%s", idx[max(0, len(idx)-300):])
	}
	role, _ := u.Role("worker", "v1")
	section, _ := u.SectionText("worker", "v1")
	for what, text := range map[string]string{"role worker": role, "section worker": section} {
		if !strings.HasSuffix(text, u.Terminator(what, "v1")+"\n") {
			t.Errorf("%s: no terminator at the end", what)
		}
	}
}
