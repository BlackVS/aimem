package writingrule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/docs"
)

func TestUnreadableFlagsCompressedText(t *testing.T) {
	for _, tc := range []struct {
		text, span, reason string
	}{
		// The operator's examples, in the sentences they came from.
		{"CI409 SUCCESS99s, merged.", "CI409", ReasonGlued},
		{"Checkpoint74files written before the restart.", "Checkpoint74files", ReasonGlued},
		{"Waiting on approval573 from the operator.", "approval573", ReasonGlued},
		{"The task is BLOCKEDr4 until the hub is upgraded.", "BLOCKEDr4", ReasonGlued},
		{"All11containersremoved and the host is clean.", "All11containersremoved", ReasonGlued},
		{"Run SUCCESS99s on the second attempt.", "SUCCESS99s", ReasonGlued},
		// The rule's own compressed example.
		{"rootcause=cachekey/missing tenant;fixed+tests,deploy pending(ops)", "tenant;fixed+tests,deploy", ReasonPunctuation},
		// Invented ones.
		{"Deployed.Tests green.Next the release notes.Then the tag.", "Deployed.Tests", ReasonPunctuation},
		{"fixedtheretrylooponthehubandaddedatestforthetimeoutpath", "fixedtheretrylooponthehubandaddedatestforthetimeoutpath", ReasonSpacing},
		{"PR282 merged, verify pending.", "PR282", ReasonGlued},
	} {
		f := Unreadable(tc.text)
		if f == nil {
			t.Errorf("%q passed; want it flagged at %q", tc.text, tc.span)
			continue
		}
		if f.Span != tc.span || f.Reason != tc.reason {
			t.Errorf("%q: flagged %q (%s); want %q (%s)", tc.text, f.Span, f.Reason, tc.span, tc.reason)
		}
		if f.Section != RuleSection || f.Fix == "" {
			t.Errorf("%q: finding %+v lacks the section or the fix", tc.text, f)
		}
		if msg := f.Message(); !strings.Contains(msg, tc.span) || !strings.Contains(msg, tc.reason) || !strings.Contains(msg, RuleSection) {
			t.Errorf("message %q does not name the span, the reason and the section", msg)
		}
	}
}

func TestUnreadablePassesReadableText(t *testing.T) {
	for _, text := range []string{
		"Done.",
		"Ok.",
		"",
		// A technical comment: ids, hashes, paths, a code span, versions,
		// URLs, flags, environment keys and technical names with digits.
		"Merged as d2baf13286 into master; CI run 40089890103 passed on ubuntu-latest and macos-14. " +
			"The fix is in `internal/wiring/project.go` and cmd/aimem/wire.go, released in v0.10.13. " +
			"Task 01a16a1a-a4a2-7000-b960-6777418edfd4 tracks the follow-up; see https://github.com/BlackVS/aimem/pull/282. " +
			"The sha256 digest and the ed25519 key are unchanged, AIMEM_TEXT_CHECK_MODEL stays unset, and `go test ./...` passes with -count=1.",
		"Run `aimem tasks text-check -p demo --since 2026-10-01` on the hub host.\n\n```\nCI409 SUCCESS99s Checkpoint74files\n```\n",
		"The worker pushed the fix to the branch. The reviewer found no blocker, so the operator can merge it.",
		"Windows 11 and Ubuntu 24.04 both pass; the amd64 and arm64 builds are signed, and RFC9110 covers the status codes.",
		"Version 1.2.3 is out. See README.md, CHANGELOG.md and docs/ADMIN-MANUAL.md for details.",
		"i18n and k8s are common numeronyms, and win32api is a library name.",
		"The task 01a119cc-0a7c has eleven comments; the hash 9f86d081884c7d659a2feaa0c55ad015 matches.",
		"Calls go through store.DB and os.Getenv, as before.",
		"Comment seq201 records the decision; UUIDv7 ids and ChaCha20-Poly1305 are unchanged.",
		// A list of identifiers, one per line.
		"Changed files:\n- internal/writingrule/readable.go\n- internal/server/tasks.go\n- docs/ADMIN-MANUAL.md\n",
	} {
		if f := Unreadable(text); f != nil {
			t.Errorf("%q flagged at %q (%s); it is readable", text, f.Span, f.Reason)
		}
	}
}

func TestContextIsBoundedAroundTheSpan(t *testing.T) {
	text := strings.Repeat("a", 500) + " CI409 " + strings.Repeat("é", 500)
	c := Context(text, "CI409", 100)
	if !strings.Contains(c, "CI409") || len(c) > 100+len("CI409")+100+2 {
		t.Fatalf("context %d bytes: %q", len(c), c)
	}
	if !strings.HasSuffix(c, "é") && !strings.HasSuffix(c, " ") {
		t.Fatalf("context cut inside a rune: %q", c[len(c)-4:])
	}
	if got := Context("short", "missing", 10); got != "short" {
		t.Fatalf("context of a missing span = %q", got)
	}
}

// The repository's own prose is technical and readable: every paragraph of
// the documentation, the changelog and the README passes, except the
// compressed examples the rule quotes on purpose. A false positive the
// filter grows shows up here first.
func TestRepositoryProsePasses(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no documentation found: %v", err)
	}
	files = append(files, filepath.Join("..", "..", "CHANGELOG.md"), filepath.Join("..", "..", "README.md"))
	paragraphs := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for p := range strings.SplitSeq(string(raw), "\n\n") {
			p = strings.TrimSpace(p)
			if p == "" || strings.HasPrefix(p, "> ") && filepath.Base(f) == docs.WritingRuleFile {
				continue
			}
			paragraphs++
			if fd := Unreadable(p); fd != nil {
				t.Errorf("%s: flagged %q (%s) in %q", filepath.Base(f), fd.Span, fd.Reason, Context(p, fd.Span, 60))
			}
		}
	}
	if paragraphs < 1000 {
		t.Fatalf("only %d paragraphs read; the corpus is missing", paragraphs)
	}
}
