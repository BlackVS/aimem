package process

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// MaxForgeArchiveBytes bounds the download of a forge tarball: the whole
// repository at one commit, compressed, of which only the set is kept.
const MaxForgeArchiveBytes = 64 << 20

// ForgeArchiveURL is where a forge serves the tarball of commit for an
// https repository: GitHub and Gitea both answer
// <repository>/archive/<commit>.tar.gz. An ssh or scp-like repository has
// no such URL and gets "".
func ForgeArchiveURL(repo, commit string) string {
	if !strings.HasPrefix(repo, "https://") {
		return ""
	}
	base := strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
	return base + "/archive/" + commit + ".tar.gz"
}

// MaxRefAdvertisementBytes bounds the ref advertisement ResolveForge reads.
const MaxRefAdvertisementBytes = 8 << 20

// FetchForgeArchive reads the set at ref's commit from the forge's tarball
// of that commit, without Git: the archive must record the commit (git
// archive writes it in the pax global header, on GitHub and Gitea alike),
// and its files are read as an imported archive's are, so the set is the
// one Git would give. A non-empty token is sent as forgeGet sends it.
func FetchForgeArchive(ctx context.Context, client *http.Client, ref Ref, token string) (*Bundle, error) {
	u := ForgeArchiveURL(ref.Repo, ref.Commit)
	if u == "" {
		return nil, errors.New("the repository is not an https URL, so there is no forge tarball to read")
	}
	shown := redacted(u)
	ctx, cancel := context.WithTimeout(ctx, 3*FetchTimeout)
	defer cancel()
	data, err := forgeGet(ctx, client, u, token, MaxForgeArchiveBytes)
	if err != nil {
		return nil, err
	}
	got, err := ArchiveCommit(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", shown, err)
	}
	if got != ref.Commit {
		return nil, fmt.Errorf("%s records commit %q, not %s", shown, got, ref.Commit)
	}
	b, err := ReadArchive(bytes.NewReader(data), ref.Manifest)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", shown, err)
	}
	return b, nil
}

// ArchiveCommit is the commit a tar.gz made by git archive records: the
// comment of its pax global header, which comes first. An archive without
// one records no commit and gets "".
func ArchiveCommit(data []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("archive: not gzip: %w", err)
	}
	defer gz.Close()
	hdr, err := tar.NewReader(gz).Next()
	if err != nil {
		return "", fmt.Errorf("archive: %w", err)
	}
	if hdr.Typeflag != tar.TypeXGlobalHeader {
		return "", nil
	}
	return hdr.PAXRecords["comment"], nil
}

// ResolveForge is Resolve without Git, for an https repository: it reads
// the ref advertisement the forge serves to Git clients,
// <repository>/info/refs?service=git-upload-pack (what git ls-remote
// reads, on GitHub, Gitea and any smart-HTTP server), and picks the name
// from it as Resolve does. A non-empty token is sent as forgeGet sends it.
func ResolveForge(ctx context.Context, client *http.Client, repo, name, token string) (string, error) {
	if err := checkRepo(repo); err != nil {
		return "", err
	}
	if err := CheckRefName(name); err != nil {
		return "", err
	}
	if !strings.HasPrefix(repo, "https://") {
		return "", errors.New("the repository is not an https URL, so there is no ref advertisement to read")
	}
	u := strings.TrimSuffix(repo, "/") + "/info/refs?service=git-upload-pack"
	ctx, cancel := context.WithTimeout(ctx, 3*FetchTimeout)
	defer cancel()
	data, err := forgeGet(ctx, client, u, token, MaxRefAdvertisementBytes)
	if err != nil {
		return "", err
	}
	refs, err := advertisedRefs(data)
	if err != nil {
		return "", fmt.Errorf("%s: %w", redacted(u), err)
	}
	return pickRef(refs, name)
}

// advertisedRefs turns a smart-HTTP ref advertisement (pkt-lines: the
// "# service=git-upload-pack" line, a flush, then "<commit> <ref>" lines,
// the first carrying capabilities after a NUL) into the "<commit> <ref>"
// lines pickRef reads.
func advertisedRefs(data []byte) (string, error) {
	var out strings.Builder
	service := false
	for len(data) > 0 {
		if len(data) < 4 {
			return "", errors.New("not a Git ref advertisement: truncated pkt-line")
		}
		n, err := strconv.ParseUint(string(data[:4]), 16, 16)
		if err != nil || (n != 0 && n < 4) || int(n) > len(data) {
			return "", errors.New("not a Git ref advertisement: bad pkt-line length")
		}
		if n == 0 { // flush
			data = data[4:]
			continue
		}
		line := strings.TrimSuffix(string(data[4:n]), "\n")
		data = data[n:]
		if !service {
			if line != "# service=git-upload-pack" {
				return "", errors.New("not a Git ref advertisement: no service line")
			}
			service = true
			continue
		}
		line, _, _ = strings.Cut(line, "\x00")
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if !service {
		return "", errors.New("not a Git ref advertisement: empty")
	}
	return out.String(), nil
}

// forgeGet reads u, at most max bytes, from the repository's forge. A
// non-empty token goes as "Authorization: token <token>", the form GitHub
// and Gitea both accept, and only to u's own scheme and host: a redirect
// to another host (GitHub sends a tarball to codeload.github.com) is
// followed without it. Errors name u without its password and never carry
// the token.
func forgeGet(ctx context.Context, client *http.Client, u, token string, max int64) ([]byte, error) {
	shown := redacted(u)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("GET %s: not a valid URL", shown)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
		c := *client
		next := client.CheckRedirect
		c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
			if r.URL.Scheme != req.URL.Scheme || r.URL.Host != req.URL.Host {
				r.Header.Del("Authorization")
			}
			if next != nil {
				return next(r, via)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
		client = &c
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is already named, without its password
		}
		return nil, fmt.Errorf("GET %s: %v", shown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", shown, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %v", shown, err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", shown, max)
	}
	return data, nil
}

// redacted is u without its password, for messages.
func redacted(u string) string {
	if pu, err := url.Parse(u); err == nil {
		return pu.Redacted()
	}
	return u
}
