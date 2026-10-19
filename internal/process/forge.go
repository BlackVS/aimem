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

// FetchForgeArchive reads the set at ref's commit from the forge's tarball
// of that commit, without Git: the archive must record the commit (git
// archive writes it in the pax global header, on GitHub and Gitea alike),
// and its files are read as an imported archive's are, so the set is the
// one Git would give.
func FetchForgeArchive(ctx context.Context, client *http.Client, ref Ref) (*Bundle, error) {
	u := ForgeArchiveURL(ref.Repo, ref.Commit)
	if u == "" {
		return nil, errors.New("the repository is not an https URL, so there is no forge tarball to read")
	}
	shown := u
	if pu, err := url.Parse(u); err == nil {
		shown = pu.Redacted()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxForgeArchiveBytes+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %v", shown, err)
	}
	if len(data) > MaxForgeArchiveBytes {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", shown, MaxForgeArchiveBytes)
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
