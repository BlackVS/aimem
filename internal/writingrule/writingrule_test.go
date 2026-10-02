package writingrule

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/docs"
)

func TestEmbeddedRuleIsValid(t *testing.T) {
	if err := Check(); err != nil {
		t.Fatal(err)
	}
}

// The served text is the committed file, whole, between the header and the
// terminator; there is no second copy.
func TestTextIsTheCommittedFileWhole(t *testing.T) {
	disk, err := os.ReadFile(filepath.Join("..", "..", "docs", docs.WritingRuleFile))
	if err != nil {
		t.Fatal(err)
	}
	text, err := Text("v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "\n\n"+string(disk)+Terminator("v9.9.9")+"\n") {
		t.Fatal("the served text does not hold docs/" + docs.WritingRuleFile + " whole before the terminator")
	}
	if !strings.HasSuffix(text, "=== end aimem-writing-rule version v9.9.9 digest "+Digest()+" ===\n") {
		t.Fatalf("terminator: %q", text[len(text)-120:])
	}
	if !strings.Contains(text, "version v9.9.9, digest "+Digest()) || !strings.Contains(text, "from docs/"+docs.WritingRuleFile) {
		t.Fatal("the header does not name the version, the digest and the source")
	}
	if dev, _ := Text(""); !strings.Contains(dev, "version dev,") {
		t.Fatal("an unstamped build is not named dev")
	}
}

// The digest covers the bytes only, so every build of the same text has the
// same digest.
func TestDigestIsStable(t *testing.T) {
	disk, err := os.ReadFile(filepath.Join("..", "..", "docs", docs.WritingRuleFile))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(disk)
	if want := "sha256:" + hex.EncodeToString(sum[:]); Digest() != want {
		t.Fatalf("digest %q, want the SHA-256 of the committed file %q", Digest(), want)
	}
	a, _ := Text("v1")
	b, _ := Text("v2")
	if !strings.Contains(a, Digest()) || !strings.Contains(b, Digest()) {
		t.Fatal("the digest changed with the version")
	}
}
