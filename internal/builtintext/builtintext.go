// Package builtintext renders a document built into the aimem binary as one
// read: a header naming the text's unit, the build version, a SHA-256
// digest of its bytes and where the text comes from, the document as
// committed, and a terminator line, so a client that cut the result can
// tell. The writing rule (writing_rule) and the feature guide (team_context
// section=features) are served this way.
package builtintext

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"aimem/docs"
)

// Text is one built-in document.
type Text struct {
	// Unit names the text in its header and terminator.
	Unit string
	// Name is the text's name as the header's first words.
	Name string
	// From says where the text comes from, after the digest in the header.
	From string
	// About is the header's second line.
	About string
	// Body is the document as committed.
	Body []byte
	// MaxBytes bounds the document.
	MaxBytes int
}

// Digest is "sha256:<hex>" over the document's bytes; the build version is
// not part of it.
func (t Text) Digest() string {
	sum := sha256.Sum256(t.Body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Check validates the document; each text's package tests make a failure
// a failed build.
func (t Text) Check() error {
	b := t.Body
	switch {
	case !bytes.HasPrefix(b, []byte("# ")):
		return fmt.Errorf("%s: the file must start with its title heading", t.Unit)
	case bytes.IndexByte(b, '\r') >= 0:
		return fmt.Errorf("%s: the file contains a carriage return; it is LF-only so its digest is the same on every platform", t.Unit)
	case len(b) > t.MaxBytes:
		return fmt.Errorf("%s: %d bytes, over the %d-byte limit", t.Unit, len(b), t.MaxBytes)
	case b[len(b)-1] != '\n':
		return fmt.Errorf("%s: the file must end with a newline", t.Unit)
	}
	return nil
}

func orDev(version string) string {
	if version == "" {
		return "dev"
	}
	return version
}

// Terminator is the last line of the rendered text.
func (t Text) Terminator(version string) string {
	return fmt.Sprintf("=== end %s version %s digest %s ===", t.Unit, orDev(version), t.Digest())
}

// Render is the whole read: the header, the document as committed and the
// terminator.
func (t Text) Render(version string) (string, error) {
	if err := t.Check(); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s. Unit %s, version %s, digest %s, %s.\n", t.Name, t.Unit, orDev(version), t.Digest(), t.From)
	b.WriteString(t.About + "\n")
	fmt.Fprintf(&b, "Complete only if the last line is %q.\n\n", t.Terminator(version))
	b.Write(t.Body)
	b.WriteString(t.Terminator(version))
	b.WriteByte('\n')
	return b.String(), nil
}

// FeatureGuideSection is the team_context section id that returns the
// feature guide. It is answered beside the team guidance unit, not from
// it: the guide has its own digest, so an edit of it does not change the
// team guidance's.
const FeatureGuideSection = "features"

// FeatureGuide is the guide to a goal's features (docs/FEATURES.md). Its
// header says the text is the one built into this binary.
var FeatureGuide = Text{
	Unit:     "aimem-feature-guide",
	Name:     "aimem guide to a goal's features",
	From:     "source: built in, docs/" + docs.FeatureGuideFile + " as committed in this aimem build",
	About:    "How a session that plans and tracks a goal keeps the goal's features with the task tools (list_features, set_feature, confirm_feature_task). Public guidance; it grants no permission.",
	Body:     docs.FeatureGuide,
	MaxBytes: 16 << 10,
}

// Section answers a team_context section id that names a built-in text
// outside the team guidance unit; ok is false for any other id.
func Section(id, version string) (text string, ok bool, err error) {
	if id != FeatureGuideSection {
		return "", false, nil
	}
	text, err = FeatureGuide.Render(version)
	return text, true, err
}
