package embed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aimem/internal/provider"
)

// fakeEmbedServer answers /embeddings with vectors of the given width,
// ignoring or honouring the request's "dimensions" as configured.
func fakeEmbedServer(t *testing.T, width int, honourDim bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		n := width
		if honourDim && req.Dimensions > 0 {
			n = req.Dimensions
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i := range req.Input {
			data = append(data, item{Index: i, Embedding: make([]float32, n)})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data":  data,
			"usage": map[string]int64{"total_tokens": 7},
		})
	}))
}

func TestEmbedWidthGuard(t *testing.T) {
	// A provider that ignores "dimensions" hands back full-width vectors
	// that would be stored under the truncated key - two incompatible
	// spaces under one name. Embed must refuse.
	srv := fakeEmbedServer(t, 3072, false)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Model: "m", Dim: 768}
	if _, _, err := c.Embed([]string{"x"}); err == nil ||
		!strings.Contains(err.Error(), "ignored dimensions") {
		t.Fatalf("width mismatch not refused: %v", err)
	}
}

func TestEmbedHonouredDimensions(t *testing.T) {
	srv := fakeEmbedServer(t, 3072, true)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Model: "m", Dim: 768}
	vecs, tokens, err := c.Embed([]string{"a", "b"})
	if err != nil || len(vecs) != 2 || len(vecs[0]) != 768 || tokens != 7 {
		t.Fatalf("honoured dims: vecs=%d w=%d tokens=%d err=%v",
			len(vecs), len(vecs[0]), tokens, err)
	}
}

func TestKeyCarriesDimension(t *testing.T) {
	if k := (&Client{Model: "m", Dim: 768}).Key(); k != "m@768" {
		t.Fatalf("key with dim: %q", k)
	}
	if k := (&Client{Model: "m"}).Key(); k != "m" {
		t.Fatalf("key without dim: %q", k)
	}
	var nilClient *Client
	if k := nilClient.Key(); k != "" {
		t.Fatalf("nil key: %q", k)
	}
}

func TestEncodeDecodeCosine(t *testing.T) {
	v := []float32{0.25, -1, 3.5}
	got := Decode(Encode(v))
	if len(got) != 3 || got[0] != 0.25 || got[1] != -1 || got[2] != 3.5 {
		t.Fatalf("roundtrip: %v", got)
	}
	if c := Cosine(v, v); c < 0.9999 {
		t.Fatalf("self-cosine: %f", c)
	}
	// Dimension mismatch and zero vectors rank nothing, by contract.
	if c := Cosine(v, []float32{1, 2}); c != 0 {
		t.Fatalf("mismatched dims ranked: %f", c)
	}
	if c := Cosine([]float32{0, 0}, []float32{0, 0}); c != 0 {
		t.Fatalf("zero vectors ranked: %f", c)
	}
}

// The embed path must not go dark silently under the bound-only
// resolution rule: a configured model whose provider cannot serve it
// yields no client AND a reason naming the provider.
func TestForModelReportsWhy(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEM_OPENAI_API_KEY", "envkey") // env is available and must NOT be used
	r := provider.Load(root)
	r.Providers["hollow"] = provider.Provider{Kind: "openai"}
	r.Providers["ok"] = provider.Provider{Kind: "openai", BaseURL: "https://ok.example/v1", Token: "k"}
	r.Providers["cl"] = provider.Provider{Kind: "claude"}
	r.Bind("hollow/e", "hollow", "e")
	r.Bind("ok/e", "ok", "e")
	r.Bind("cl/e", "cl", "e")
	if err := r.Save(root); err != nil {
		t.Fatal(err)
	}
	if c := ForModel(root, "hollow/e"); c != nil {
		t.Fatalf("tokenless binding produced a client: %+v", c)
	}
	t.Setenv("AIMEM_EMBED_MODEL", "hollow/e")
	if why := Why(root); !strings.Contains(why, `provider "hollow"`) || !strings.Contains(why, "no token") {
		t.Fatalf("why: %q", why)
	}
	if c := ForModel(root, "cl/e"); c != nil {
		t.Fatalf("claude binding cannot embed but produced a client: %+v", c)
	}
	t.Setenv("AIMEM_EMBED_MODEL", "cl/e")
	if why := Why(root); !strings.Contains(why, "claude") {
		t.Fatalf("why for claude binding: %q", why)
	}
	if c := ForModel(root, "ok/e"); c == nil || c.Model != "e" || c.APIKey != "k" {
		t.Fatalf("healthy binding: %+v", c)
	}
	t.Setenv("AIMEM_EMBED_MODEL", "")
	if why := Why(root); !strings.Contains(why, "AIMEM_EMBED_MODEL is unset") {
		t.Fatalf("why unset: %q", why)
	}
}
