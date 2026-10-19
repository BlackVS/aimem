package process

// Selection by tag: the admin names a tag (or a branch) and the command on
// the hub host resolves it to the commit it holds at that moment, through
// git ls-remote, never a clone. The selection then stores both, the ref for
// people and the commit as the pin: what a session sees stays exactly the
// reviewed commit even if the ref later moves.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultManifest is the manifest path select uses when none is named: the
// ready process set the aiskills repository publishes.
const DefaultManifest = "processes/aimem/manifest.json"

// IsCommit reports whether s is a full lowercase 40-hex commit id.
func IsCommit(s string) bool { return commitRE.MatchString(s) }

// CheckRefName checks that name is a plain branch or tag name.
func CheckRefName(name string) error {
	if !refRE.MatchString(name) || strings.Contains(name, "..") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") {
		return errors.New("ref must be a plain branch or tag name")
	}
	return nil
}

// Resolve returns the commit the tag or branch name holds in repo: a tag
// (an annotated one peeled to the commit it points at) or a branch of
// that exact name. A name that is both a tag and a branch, holding
// different commits, is refused as ambiguous.
func Resolve(ctx context.Context, repo, name string) (string, error) {
	if err := checkRepo(repo); err != nil {
		return "", err
	}
	if err := CheckRefName(name); err != nil {
		return "", err
	}
	return resolveNoValidate(ctx, repo, name)
}

// resolveNoValidate is Resolve after validation; tests drive it against a
// local repository, which checkRepo refuses for a hub selection.
func resolveNoValidate(ctx context.Context, repo, name string) (string, error) {
	// In a directory of its own, so git applies the same no-prompt ssh
	// setting as a fetch.
	tmp, err := os.MkdirTemp("", "aimem-resolve-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	tag, head := "refs/tags/"+name, "refs/heads/"+name
	out, err := git(ctx, tmp, "ls-remote", "--", repo, tag, tag+"^{}", head)
	if err != nil {
		return "", fmt.Errorf("git ls-remote: %s", firstLine(out, err))
	}
	return pickRef(string(out), name)
}

// pickRef reads ls-remote output for refs/tags/<name>, its peeled form and
// refs/heads/<name>, and returns the commit the name selects.
func pickRef(out, name string) (string, error) {
	tag, head := "refs/tags/"+name, "refs/heads/"+name
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && IsCommit(f[0]) {
			got[f[1]] = f[0]
		}
	}
	tagCommit := got[tag+"^{}"] // an annotated tag: the commit it points at
	if tagCommit == "" {
		tagCommit = got[tag] // a lightweight tag points at the commit itself
	}
	headCommit := got[head]
	switch {
	case tagCommit != "" && headCommit != "" && tagCommit != headCommit:
		return "", fmt.Errorf("%q is both a tag (%s) and a branch (%s) in the repository; select the commit instead", name, tagCommit[:12], headCommit[:12])
	case tagCommit != "":
		return tagCommit, nil
	case headCommit != "":
		return headCommit, nil
	}
	return "", fmt.Errorf("the repository has no tag or branch %q", name)
}
