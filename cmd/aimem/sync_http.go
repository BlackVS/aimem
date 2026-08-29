package main

// Anti-entropy sync over the hub API (DESIGN-hub-sync): the same six
// legs as the ssh transport — events push/pull with cursors, memories
// both ways, group config both ways — but over the bearer+TLS channel
// the real-time push already uses. No shell account, no authorized
// keys, no remote binary path, and it works on Windows.

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

// errNoSyncTransport: the hub entry has neither an API URL nor an ssh
// destination — nothing to sync over.
var errNoSyncTransport = errors.New("no sync transport")

// errNoSyncRoutes: the hub answered but predates the /v1/sync routes.
var errNoSyncRoutes = errors.New("hub has no sync routes (older release)")

// syncHub picks the transport for one hub: the API when a URL+token is
// configured (destOverride forces ssh, preserving `sync --hub X <dest>`),
// falling back to the ssh legs when the hub predates the sync routes.
func syncHub(name string, h *adapter.HubConfig, def, destOverride string) error {
	if destOverride != "" {
		return syncOne(destOverride, name, def)
	}
	if h.URL != "" && h.Token != "" {
		err := syncOneHTTP(name, h, def)
		if !errors.Is(err, errNoSyncRoutes) {
			return err
		}
		if h.Sync == "" {
			return fmt.Errorf("%w and no ssh destination to fall back to — upgrade the hub", errNoSyncRoutes)
		}
		fmt.Fprintf(os.Stderr, "aimem: hub %q predates API sync — falling back to ssh (upgrade the hub to retire the key)\n", name)
	}
	if h.Sync == "" {
		return errNoSyncTransport
	}
	return syncOne(h.Sync, name, def)
}

// syncHTTPClient allows minutes-long streams (a first sync moves whole
// journals); the adapter's 5s client is for checkpoint pushes only.
func syncHTTPClient(h *adapter.HubConfig) *http.Client {
	c := &http.Client{Timeout: 15 * time.Minute}
	if h.Insecure {
		c.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return c
}

func syncDo(h *adapter.HubConfig, method, path string, q url.Values, body io.Reader) (*http.Response, error) {
	u := strings.TrimRight(h.URL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-ndjson")
	}
	resp, err := syncHTTPClient(h).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, errNoSyncRoutes
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("hub HTTP %d on %s", resp.StatusCode, path)
	}
	return resp, nil
}

// syncPost streams a locally-produced JSONL dump to a sync route.
func syncPost(h *adapter.HubConfig, path string, q url.Values, produce func(w io.Writer) error) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(produce(pw)) }()
	resp, err := syncDo(h, "POST", path, q, pr)
	if err != nil {
		pr.CloseWithError(err) // unblock the producer if the POST died early
		return err
	}
	defer resp.Body.Close()
	var counts map[string]any
	if json.NewDecoder(resp.Body).Decode(&counts) == nil && len(counts) > 0 {
		b, _ := json.Marshal(counts)
		fmt.Fprintf(os.Stderr, "aimem: hub accepted %s\n", b)
	}
	return nil
}

// syncOneHTTP merges journals, memories, and group config with one hub
// over its API. Both directions carry the projects filter: a hub holds
// many machines' projects, and this machine syncs only its bound ones,
// the user DB, and its declared groups — the recall rule applied to
// sync.
func syncOneHTTP(name string, h *adapter.HubConfig, def string) error {
	reg, err := store.NewRegistry(stateRoot())
	if err != nil {
		return err
	}
	ids, err := hubProjects(reg, name, def)
	if err != nil {
		reg.Close()
		return err
	}
	projects := url.Values{"projects": {strings.Join(ids, ",")}}

	// Cursors are keyed per hub (hashed, like ssh destinations). The 1h
	// overlap window and idempotent import make clock skew free, exactly
	// as on the ssh path.
	cursorKey := "hub:" + name
	pushCur := readCursor(cursorKey, "push")
	pullCur := readCursor(cursorKey, "pull")
	const overlap = time.Hour

	fmt.Fprintf(os.Stderr, "aimem: pushing local events to hub %q\n", name)
	since := uuidv7.ShiftBack(pushCur, overlap)
	err = syncPost(h, "/v1/sync/events", nil, func(w io.Writer) error {
		for _, id := range ids {
			db, err := reg.OpenExisting(id)
			if err != nil {
				continue
			}
			if err := db.DumpSince(w, since); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		reg.Close()
		return fmt.Errorf("push failed: %w", err)
	}
	if m, err := localMaxEventID(); err == nil && m != "" {
		writeCursor(cursorKey, "push", m)
	}

	fmt.Fprintf(os.Stderr, "aimem: pulling events from hub %q\n", name)
	q := url.Values{"projects": projects["projects"], "since": {uuidv7.ShiftBack(pullCur, overlap)}}
	resp, err := syncDo(h, "GET", "/v1/sync/events", q, nil)
	if err != nil {
		reg.Close()
		return fmt.Errorf("pull failed: %w", err)
	}
	submitted, spooled, failed, err := importEventsFrom(resp.Body)
	resp.Body.Close()
	if err != nil {
		reg.Close()
		return fmt.Errorf("pull failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "aimem: pulled %d event(s) (%d spooled, %d failed)\n", submitted, spooled, failed)
	if m, err := localMaxEventID(); err == nil && m != "" {
		writeCursor(cursorKey, "pull", m)
	}

	fmt.Fprintf(os.Stderr, "aimem: syncing memories with hub %q\n", name)
	err = syncPost(h, "/v1/sync/memories", nil, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for _, id := range ids {
			db, err := reg.OpenExisting(id)
			if err != nil {
				continue
			}
			if err := db.DumpMemories(enc); err != nil {
				return err
			}
			runs, err := db.CurateRuns()
			if err != nil {
				continue
			}
			for i := range runs {
				if err := enc.Encode(map[string]any{"project_id": id, "curate_run": runs[i]}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		reg.Close()
		return fmt.Errorf("memory push failed: %w", err)
	}
	resp, err = syncDo(h, "GET", "/v1/sync/memories", projects, nil)
	if err != nil {
		reg.Close()
		return fmt.Errorf("memory pull failed: %w", err)
	}
	imported, mfailed, err := importMemoriesFrom(bufio.NewReaderSize(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		reg.Close()
		return fmt.Errorf("memory pull failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "aimem: merged %d memory record(s) (%d failed)\n", imported, mfailed)

	// Group config is best-effort in both directions, like the ssh legs:
	// an older hub or a partial failure must not fail the whole sync.
	if err := syncPost(h, "/v1/sync/group-config", nil, func(w io.Writer) error {
		return store.ExportGroupConfig(reg, ids, json.NewEncoder(w))
	}); err != nil {
		fmt.Fprintln(os.Stderr, "aimem: group config push skipped:", err)
	}
	if resp, err := syncDo(h, "GET", "/v1/sync/group-config", projects, nil); err != nil {
		fmt.Fprintln(os.Stderr, "aimem: group config pull skipped:", err)
	} else {
		err := importGroupConfigFrom(reg, resp.Body)
		resp.Body.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "aimem: group config pull skipped:", err)
		}
	}
	reg.Close()
	postSyncEmbed()
	return nil
}
