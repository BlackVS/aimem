// Package embed is the semantic-recall backend (proposal "Planned: semantic
// recall"): embeddings from an OpenAI-compatible /embeddings endpoint —
// typically the lab's LiteLLM proxy — stored as float32 blobs in the same
// per-project SQLite. Like every LLM touchpoint in aimem it is optional and
// fail-open: no key configured means BM25-only recall, and an embedding
// failure at query time silently degrades to BM25.
package embed

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"aimem/internal/llmrate"
	"aimem/internal/provider"
)

// Client calls an OpenAI-compatible embeddings endpoint.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	// Dim truncates the embedding (OpenAI "dimensions"; honoured by
	// text-embedding-3-* and Google's compat endpoint). 0 = provider
	// default. A 3072-dim vector is 12KB, and recall, curation dedup and
	// the nightly sweep all scan every vector, so this is the cheapest
	// lever on a knowledge base heading past ~10^3 facts/project.
	Dim int
}

// Key identifies the vector space these embeddings live in. Vectors of
// different dimensions are not comparable (Cosine returns 0), so the
// dimension is part of the identity: changing it makes NeedingEmbedding
// see every fact as unembedded and re-embed it, instead of silently
// mixing two incompatible spaces under one name.
func (c *Client) Key() string {
	if c == nil {
		return ""
	}
	if c.Dim > 0 {
		return fmt.Sprintf("%s@%d", c.Model, c.Dim)
	}
	return c.Model
}

// ForRoot returns a configured client, or nil when embedding is not
// enabled on this machine. Opt-in is AIMEM_EMBED_MODEL plus a resolvable
// endpoint — the model's providers.json binding, or the legacy
// AIMEM_OPENAI_API_KEY env pair. Machines that must not egress simply
// configure neither.
func ForRoot(root string) *Client {
	return ForModel(root, os.Getenv("AIMEM_EMBED_MODEL"))
}

// ForModel resolves model through the provider registry (env fallback)
// into a client; nil when no endpoint serves it.
func ForModel(root, model string) *Client {
	if model == "" {
		return nil
	}
	ep, ok := provider.Resolve(root, model)
	if !ok || ep.Kind != "openai" {
		return nil
	}
	dim, _ := strconv.Atoi(os.Getenv("AIMEM_EMBED_DIM"))
	// ep.Model is the payload name (an alias binding's upstream); vectors
	// are keyed by it too, consistently on the write and query paths.
	return &Client{BaseURL: ep.BaseURL, APIKey: ep.Token, Model: ep.Model, Dim: dim}
}

// Embed returns one vector per input text, in order, plus the request's
// token usage (for spend metering). Calls are PACED process-wide and
// transient upstream blocks retry with backoff (llmrate) — a curation
// sweep must trickle, not burst, or Cloudflare-fronted provider chains
// block the whole batch (2026-09-04 incident).
func (c *Client) Embed(texts []string) ([][]float32, int64, error) {
	payload := map[string]any{"model": c.Model, "input": texts}
	if c.Dim > 0 {
		payload["dimensions"] = c.Dim
	}
	body, _ := json.Marshal(payload)
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	var status int
	for attempt := 0; ; attempt++ {
		llmrate.Wait()
		req, err := http.NewRequest("POST", strings.TrimRight(c.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			return nil, 0, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		status = resp.StatusCode
		out.Data, out.Error = nil, nil
		jsonErr := json.Unmarshal(raw, &out)
		reason := ""
		if llmrate.Blocked(status, string(raw)) {
			reason = fmt.Sprintf("embeddings HTTP %d", status)
		} else if out.Error != nil && llmrate.BlockedMessage(out.Error.Message) {
			reason = "embeddings proxy error: " + llmrate.Clip(out.Error.Message, 120)
		}
		if reason != "" {
			llmrate.Penalize(reason)
			if attempt < llmrate.Retries() {
				d := llmrate.RetryDelay(attempt)
				fmt.Fprintf(os.Stderr, "aimem llmrate: retrying embeddings in %s (attempt %d/%d)\n",
					d.Round(time.Second), attempt+1, llmrate.Retries())
				time.Sleep(d)
				continue
			}
			return nil, 0, fmt.Errorf("embeddings blocked upstream after %d attempts (HTTP %d): %s",
				attempt+1, status, llmrate.Clip(string(raw), 200))
		}
		if jsonErr != nil {
			return nil, 0, fmt.Errorf("bad embeddings response (HTTP %d): %s", status, llmrate.Clip(string(raw), 200))
		}
		llmrate.Recover()
		break
	}
	tokens := out.Usage.TotalTokens
	if tokens == 0 {
		tokens = out.Usage.PromptTokens
	}
	if out.Error != nil {
		return nil, tokens, fmt.Errorf("embeddings error (HTTP %d): %s", status, llmrate.Clip(out.Error.Message, 200))
	}
	if len(out.Data) != len(texts) {
		return nil, tokens, fmt.Errorf("embeddings returned %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, tokens, fmt.Errorf("embeddings returned out-of-range index %d", d.Index)
		}
		// A provider that ignores "dimensions" would hand back full-width
		// vectors that then get stored under the truncated key — two
		// incompatible spaces under one name. Refuse instead.
		if c.Dim > 0 && len(d.Embedding) != c.Dim {
			return nil, tokens, fmt.Errorf("provider ignored dimensions=%d and returned %d (model %q)",
				c.Dim, len(d.Embedding), c.Model)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, tokens, nil
}

// Encode packs a vector as little-endian float32 for BLOB storage.
func Encode(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// Decode unpacks an Encode blob.
func Decode(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// Cosine similarity; 0 on dimension mismatch or zero vectors, so a stray
// model change can never rank anything.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
