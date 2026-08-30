package adapter

// Hub client for structured collections (docs/DESIGN-structured-docs.md).
// Records are hub-authoritative like documents; there is nothing to
// reconcile locally — generated markdown is a build artifact, so the
// client surface is plain CRUD plus the bulk fetch render uses.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"aimem/internal/store"
)

// HubRecord mirrors the hub's record representation.
type HubRecord struct {
	Collection string          `json:"collection,omitempty"`
	ID         string          `json:"id"`
	Body       json.RawMessage `json:"body,omitempty"`
	Rev        int64           `json:"rev"`
	UpdatedAt  string          `json:"updated_at"`
	UpdatedBy  string          `json:"updated_by"`
	Deleted    bool            `json:"deleted,omitempty"`
	Size       int             `json:"size"`
}

// ColSummary mirrors one collection in the hub's listing.
type ColSummary struct {
	Name      string `json:"name"`
	Records   int    `json:"records"`
	UpdatedAt string `json:"updated_at"`
}

// RecordConflictError is the client-side CAS refusal: the current record
// rides along whole, small enough to re-apply intent onto directly.
type RecordConflictError struct {
	Record HubRecord
}

func (e *RecordConflictError) Error() string {
	if e.Record.Deleted {
		return fmt.Sprintf("record deleted on the hub at rev %d by %s", e.Record.Rev, e.Record.UpdatedBy)
	}
	return fmt.Sprintf("stale base_rev: record is at rev %d (by %s at %s) — re-read and re-apply",
		e.Record.Rev, e.Record.UpdatedBy, e.Record.UpdatedAt)
}

func hubColURL(hub *HubConfig, projectID, collection, recordID string) string {
	u := strings.TrimRight(hub.URL, "/") + "/v1/projects/" + url.PathEscape(projectID) + "/collections"
	if collection != "" {
		u += "/" + url.PathEscape(collection) + "/records"
	}
	if recordID != "" {
		// Record ids are slash paths and the slashes are structural:
		// escape each segment, keep the separators.
		segs := strings.Split(recordID, "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		u += "/" + strings.Join(segs, "/")
	}
	return u
}

// ListHubCollections enumerates a project's (or group's) collections.
func (c *Client) ListHubCollections(hub *HubConfig, projectID string) ([]ColSummary, error) {
	var res struct {
		Collections []ColSummary `json:"collections"`
	}
	return res.Collections, c.hubColDo(hub, "GET", hubColURL(hub, projectID, "", ""), nil, &res)
}

// ListHubRecords lists a collection in id (= tree) order; withBodies is
// the render/export fetch.
func (c *Client) ListHubRecords(hub *HubConfig, projectID, collection string, withBodies bool) ([]HubRecord, error) {
	u := hubColURL(hub, projectID, collection, "")
	if withBodies {
		u += "?bodies=1"
	}
	var res struct {
		Records []HubRecord `json:"records"`
	}
	return res.Records, c.hubColDo(hub, "GET", u, nil, &res)
}

// GetHubRecord fetches one record (rev > 0 for a retained revision).
func (c *Client) GetHubRecord(hub *HubConfig, projectID, collection, id string, rev int64) (HubRecord, error) {
	u := hubColURL(hub, projectID, collection, id)
	if rev > 0 {
		u += fmt.Sprintf("?rev=%d", rev)
	}
	var rec HubRecord
	return rec, c.hubColDo(hub, "GET", u, nil, &rec)
}

// PutHubRecord is the CAS write; a *RecordConflictError carries the
// hub's current record.
func (c *Client) PutHubRecord(hub *HubConfig, projectID, collection, id string, body json.RawMessage, by string, baseRev int64) (HubRecord, error) {
	payload, _ := json.Marshal(map[string]any{"body": body, "base_rev": baseRev, "updated_by": by})
	var res HubRecord
	err := c.hubColDo(hub, "PUT", hubColURL(hub, projectID, collection, id), payload, &res)
	res.Collection, res.ID = collection, id
	return res, err
}

// DeleteHubRecord writes the tombstone.
func (c *Client) DeleteHubRecord(hub *HubConfig, projectID, collection, id, by string, baseRev int64) error {
	u := hubColURL(hub, projectID, collection, id) + fmt.Sprintf("?base_rev=%d&by=%s", baseRev, url.QueryEscape(by))
	return c.hubColDo(hub, "DELETE", u, nil, &struct{}{})
}

// hubRaw is the bare authenticated round-trip: body in, (body, status)
// out, transport errors wrapped.
func (c *Client) hubRaw(hub *HubConfig, method, u string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+hub.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hub.HTTPClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, store.MaxDocBytes*4))
	return raw, resp.StatusCode, nil
}

// hubColDo mirrors hubDocDo with the record-shaped 409.
func (c *Client) hubColDo(hub *HubConfig, method, u string, body []byte, into any) error {
	raw, status, err := c.hubRaw(hub, method, u, body)
	if err != nil {
		return err
	}
	if status == 409 {
		var rec HubRecord
		json.Unmarshal(raw, &rec)
		return &RecordConflictError{Record: rec}
	}
	if status != 200 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error != "" {
			return fmt.Errorf("hub: %s", e.Error)
		}
		return fmt.Errorf("hub returned HTTP %d", status)
	}
	return json.Unmarshal(raw, into)
}
