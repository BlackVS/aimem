package builtintext

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"aimem/docs"
	"aimem/internal/teamguide"
)

func TestFeatureGuideIsValid(t *testing.T) {
	if err := FeatureGuide.Check(); err != nil {
		t.Fatal(err)
	}
}

// The served guide is the committed file, whole, between the header and
// the terminator; the header names its own unit, the version, the digest
// and that the text is the built-in one.
func TestFeatureGuideIsTheCommittedFileWhole(t *testing.T) {
	disk, err := os.ReadFile(filepath.Join("..", "..", "docs", docs.FeatureGuideFile))
	if err != nil {
		t.Fatal(err)
	}
	text, ok, err := Section(FeatureGuideSection, "v9.9.9")
	if !ok || err != nil {
		t.Fatalf("section %q: %v %v", FeatureGuideSection, ok, err)
	}
	if !strings.Contains(text, "\n\n"+string(disk)+FeatureGuide.Terminator("v9.9.9")+"\n") {
		t.Fatal("the served text does not hold docs/" + docs.FeatureGuideFile + " whole before the terminator")
	}
	if !strings.HasSuffix(text, "=== end aimem-feature-guide version v9.9.9 digest "+FeatureGuide.Digest()+" ===\n") {
		t.Fatalf("terminator: %q", text[len(text)-120:])
	}
	header, _, _ := strings.Cut(text, "\n")
	for _, want := range []string{"Unit aimem-feature-guide,", "version v9.9.9,", "digest " + FeatureGuide.Digest(), "source: built in, docs/" + docs.FeatureGuideFile} {
		if !strings.Contains(header, want) {
			t.Errorf("the header does not name %q: %s", want, header)
		}
	}
}

// A session without a checkout of aimem cannot follow a relative link, so
// the served guide holds none: every Markdown link is absolute.
func TestFeatureGuideHasNoBareRelativeLink(t *testing.T) {
	text, _, err := Section(FeatureGuideSection, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`\]\(([^)\s]+)`).FindAllStringSubmatch(text, -1) {
		if !strings.HasPrefix(m[1], "https://") {
			t.Errorf("a relative link in the served guide: %s", m[1])
		}
	}
}

// Section answers the guide's id only; every other id is the team unit's.
func TestSectionAnswersOnlyTheGuide(t *testing.T) {
	for _, id := range []string{"worker", "writing", "example/submit", "", "FEATURES", "features/"} {
		if _, ok, _ := Section(id, ""); ok {
			t.Errorf("section %q answered as a built-in text", id)
		}
	}
	u, err := teamguide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range u.IDs() {
		if id == FeatureGuideSection {
			t.Fatalf("the team unit has a section %q, which the guide shadows", id)
		}
	}
}

// The guide is not part of the team guidance unit, so an edit of it never
// moves the team guidance's digest that every member is told about.
func TestTeamGuidanceDigestDoesNotMoveWithTheGuide(t *testing.T) {
	if _, err := fs.Stat(docs.TeamGuidance, docs.FeatureGuideFile); err == nil {
		t.Fatal("the team guidance embeds " + docs.FeatureGuideFile)
	}
	u, err := teamguide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, guide := range []string{string(docs.FeatureGuide), "# Features of a goal\n\nan edited guide\n"} {
		fsys := fstest.MapFS{docs.FeatureGuideFile: {Data: []byte(guide)}}
		err := fs.WalkDir(docs.TeamGuidance, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(docs.TeamGuidance, p)
			fsys[p] = &fstest.MapFile{Data: b}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		built, err := teamguide.Build(fsys)
		if err != nil {
			t.Fatal(err)
		}
		if built.Digest != u.Digest {
			t.Fatalf("the team guidance digest moved with the guide: %s, want %s", built.Digest, u.Digest)
		}
	}
}
