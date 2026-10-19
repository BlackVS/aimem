package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aimem/internal/process"
)

// The member's side: read the index and a set from the hub under the
// checkout's credential, verify the digest, cache by it. 401 and 403 are
// a denial of this credential; 404 and 405 mean the hub has no such route
// (an older hub); a body that is cut or does not digest to its digest is
// never cached.

// Outcome classifies one read from the hub.
type Outcome int

const (
	OutcomeOK          Outcome = iota
	OutcomeDenied              // the hub refused the credential or the grant
	OutcomeUnsupported         // the hub has no such route (older hub) or no such set
	OutcomeFailed              // anything else; the error says what
)

// readTimeout bounds one read; bodyMax bounds its size (a set of
// MaxSetBytes, base64 in JSON).
const (
	readTimeout = process.FetchTimeout
	bodyMax     = 2 * MaxSetBytes
)

// Client reads the skills routes of one hub for one project.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Project string
}

// IndexAnswer is the hub's index for a project: the current import per
// source (nil when none), the kept imports per source, newest first, and
// the effective skills.
type IndexAnswer struct {
	Project   string             `json:"project"`
	Sources   map[string]*Entry  `json:"sources"`
	Kept      map[string][]Entry `json:"kept"`
	Effective []Skill            `json:"effective"`
}

func (c *Client) get(ctx context.Context, path string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bodyMax+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading the hub's answer: %w", err)
	}
	if len(raw) > bodyMax {
		return resp.StatusCode, nil, fmt.Errorf("the hub's answer is larger than %d bytes", bodyMax)
	}
	return resp.StatusCode, raw, nil
}

func classify(status int, raw []byte) (Outcome, error) {
	msg := func() string {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error != "" {
			return e.Error
		}
		return fmt.Sprintf("hub returned HTTP %d", status)
	}
	switch status {
	case http.StatusOK:
		return OutcomeOK, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return OutcomeDenied, errors.New(msg())
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return OutcomeUnsupported, errors.New(msg())
	}
	return OutcomeFailed, errors.New(msg())
}

// Index reads the project's skills index.
func (c *Client) Index(ctx context.Context) (*IndexAnswer, Outcome, error) {
	status, raw, err := c.get(ctx, "/v1/projects/"+url.PathEscape(c.Project)+"/skills")
	if err != nil {
		return nil, OutcomeFailed, err
	}
	if out, err := classify(status, raw); out != OutcomeOK {
		return nil, out, err
	}
	var ix IndexAnswer
	if err := json.Unmarshal(raw, &ix); err != nil {
		return nil, OutcomeFailed, fmt.Errorf("the hub's skills index is not readable: %w", err)
	}
	return &ix, OutcomeOK, nil
}

// Fetch reads the set with that digest (a current or a kept import),
// verifies that its files digest to it and stores it in the cache. A set
// already cached is read from there and the hub is not asked.
func (c *Client) Fetch(ctx context.Context, cache *Store, digest string) (map[string][]byte, Outcome, error) {
	if !ValidDigest(digest) {
		return nil, OutcomeFailed, errors.New("digest must be 64 lowercase hex characters")
	}
	if cache.Has(digest) {
		files, err := cache.Read(digest)
		if err == nil {
			return files, OutcomeOK, nil
		}
		// Damaged under its marker: out, so the verified answer replaces it.
		if err := cache.Remove(digest); err != nil {
			return nil, OutcomeFailed, fmt.Errorf("cached set %s is damaged and cannot be removed: %w", short(digest), err)
		}
	}
	status, raw, err := c.get(ctx, "/v1/projects/"+url.PathEscape(c.Project)+"/skills/set?digest="+digest)
	if err != nil {
		return nil, OutcomeFailed, err
	}
	if out, err := classify(status, raw); out != OutcomeOK {
		return nil, out, err
	}
	var body struct {
		Digest string            `json:"digest"`
		Files  map[string][]byte `json:"files"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, OutcomeFailed, fmt.Errorf("the hub's answer is not a complete set: %w", err)
	}
	if body.Digest != digest {
		return nil, OutcomeFailed, fmt.Errorf("the hub answered with set %s, not %s", short(body.Digest), short(digest))
	}
	if got := (&Set{Files: body.Files}).Digest(); got != digest {
		return nil, OutcomeFailed, fmt.Errorf("digest mismatch: the files digest to %s, the hub says %s; nothing cached", short(got), short(digest))
	}
	if _, err := cache.Put(body.Files, digest); err != nil {
		return nil, OutcomeFailed, fmt.Errorf("the hub's set does not validate: %w", err)
	}
	return body.Files, OutcomeOK, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Resolve picks, for each required skill, the import to install it from:
// an unpinned name from the effective set; a pin from its source's
// current import when the version matches, else from a kept import of
// that version. A skill the hub cannot serve is a problem, in words a
// member can act on (the admin imports the version, or the process pins
// the current one); none of them names where sets come from.
func Resolve(ix *IndexAnswer, pins []process.SkillPin) (picks []Skill, problems []string) {
	effective := map[string]Skill{}
	for _, s := range ix.Effective {
		effective[s.Name] = s
	}
	for _, p := range pins {
		if !p.Pinned() {
			s, ok := effective[p.Name]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: no skill of that name in the hub's sets", p.Name))
				continue
			}
			picks = append(picks, s)
			continue
		}
		var found *Entry
		cur := ix.Sources[p.Source]
		if cur != nil && cur.Version == p.Version && has(cur.Skills, p.Name) {
			found = cur
		}
		for i := range ix.Kept[p.Source] {
			k := &ix.Kept[p.Source][i]
			if found == nil && k.Version == p.Version && has(k.Skills, p.Name) {
				found = k
			}
		}
		if found == nil {
			state := "no " + p.Source + " set is imported"
			if cur != nil {
				state = "the current " + p.Source + " set is " + cur.Version
				if !has(cur.Skills, p.Name) {
					state += ", which has no skill " + p.Name
				}
			}
			problems = append(problems, fmt.Sprintf("%s %s (%s) is not on the hub: %s; the admin imports that version on the hub host, or the process pins the current one", p.Name, p.Version, p.Source, state))
			continue
		}
		picks = append(picks, Skill{Name: p.Name, Version: found.Version, Source: p.Source, Digest: found.Digest})
	}
	return picks, problems
}

func has(list []string, name string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

// Deadline is a context bounded for one install: the index plus at most
// one read per set.
func Deadline() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*readTimeout+time.Second)
}
