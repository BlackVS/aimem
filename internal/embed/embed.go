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
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

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

// Embed returns one vector per input text, in order.
// Embed returns one vector per input plus the request's token usage (for
// spend metering).
func (c *Client) Embed(texts []string) ([][]float32, int64, error) {
	payload := map[string]any{"model": c.Model, "input": texts}
	if c.Dim > 0 {
		payload["dimensions"] = c.Dim
	}
	body, _ := json.Marshal(payload)
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
	defer resp.Body.Close()
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
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, fmt.Errorf("bad embeddings response (HTTP %d): %w", resp.StatusCode, err)
	}
	tokens := out.Usage.TotalTokens
	if tokens == 0 {
		tokens = out.Usage.PromptTokens
	}
	if out.Error != nil {
		return nil, tokens, fmt.Errorf("embeddings error (HTTP %d): %s", resp.StatusCode, out.Error.Message)
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
