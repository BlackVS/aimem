package provider

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEM_OPENAI_API_KEY", "envkey")
	t.Setenv("AIMEM_OPENAI_BASE_URL", "https://env.example/v1")

	// No registry: env fallback.
	ep, ok := Resolve(root, "m1")
	if !ok || ep.Token != "envkey" || ep.BaseURL != "https://env.example/v1" || ep.Kind != "openai" {
		t.Fatalf("env fallback: %+v ok=%v", ep, ok)
	}

	// Registry binding wins over env.
	r := Load(root)
	r.Providers["g"] = Provider{Kind: "openai", BaseURL: "https://g.example/v1", Token: "gkey"}
	r.Providers["cl"] = Provider{Kind: "claude"}
	r.Bind("m1", "g", "")
	r.Bind("m2", "cl", "")
	if err := r.Save(root); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(Path(root)); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("registry mode: %v err=%v", fi.Mode(), err)
	}
	if ep, ok = Resolve(root, "m1"); !ok || ep.Token != "gkey" || ep.BaseURL != "https://g.example/v1" {
		t.Fatalf("registry binding: %+v ok=%v", ep, ok)
	}
	if ep, ok = Resolve(root, "m2"); !ok || ep.Kind != "claude" || ep.Token != "" {
		t.Fatalf("claude kind: %+v ok=%v", ep, ok)
	}
	// Unbound model still falls back to env.
	if ep, ok = Resolve(root, "other"); !ok || ep.Token != "envkey" {
		t.Fatalf("unbound fallback: %+v ok=%v", ep, ok)
	}
	// A BOUND model whose provider has no token must NOT fall back to
	// env: the binding is explicit, and serving it from the env endpoint
	// hid a misconfigured provider behind another vendor's errors.
	r.Providers["hollow"] = Provider{Kind: "openai"}
	r.Bind("m3", "hollow", "")
	r.Bind("m4", "vanished", "")
	if err := r.Save(root); err != nil {
		t.Fatal(err)
	}
	if ep, ok = Resolve(root, "m3"); ok {
		t.Fatalf("hollow binding must not resolve (got %+v)", ep)
	}
	if why := Explain(root, "m3"); !strings.Contains(why, `"hollow"`) || !strings.Contains(why, "no token") {
		t.Fatalf("explain hollow: %q", why)
	}
	if ep, ok = Resolve(root, "m4"); ok {
		t.Fatalf("binding to a missing provider must not resolve (got %+v)", ep)
	}
	if why := Explain(root, "m4"); !strings.Contains(why, "no longer exists") {
		t.Fatalf("explain vanished: %q", why)
	}
	// Explain is silent for models that resolve, and names the env gap
	// for unbound models only when the env pair is unset.
	if why := Explain(root, "m1"); why != "" {
		t.Fatalf("explain resolvable: %q", why)
	}
	if why := Explain(root, "other"); why != "" {
		t.Fatalf("explain env-served: %q", why)
	}
	t.Setenv("AIMEM_OPENAI_API_KEY", "")
	if why := Explain(root, "other"); !strings.Contains(why, "AIMEM_OPENAI_API_KEY") {
		t.Fatalf("explain unbound without env: %q", why)
	}
}

func TestResolveAlias(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEM_OPENAI_API_KEY", "")
	r := Load(root)
	r.Providers["a"] = Provider{Kind: "openai", BaseURL: "https://a.example/v1", Token: "ka"}
	r.Providers["b"] = Provider{Kind: "openai", BaseURL: "https://b.example/v1", Token: "kb"}
	// Same upstream model through two providers under distinct local names.
	r.Bind("gpt4o-a", "a", "gpt-4o")
	r.Bind("gpt4o-b", "b", "gpt-4o")
	r.Bind("plain", "a", "plain") // upstream == alias normalizes to ""
	if err := r.Save(root); err != nil {
		t.Fatal(err)
	}
	if b := Load(root).Models["plain"]; b.Model != "" {
		t.Fatalf("upstream==alias should store empty, got %q", b.Model)
	}
	ep, ok := Resolve(root, "gpt4o-a")
	if !ok || ep.Model != "gpt-4o" || ep.Token != "ka" {
		t.Fatalf("alias a: %+v ok=%v", ep, ok)
	}
	if ep, ok = Resolve(root, "gpt4o-b"); !ok || ep.Model != "gpt-4o" || ep.Token != "kb" {
		t.Fatalf("alias b: %+v ok=%v", ep, ok)
	}
	if ep, ok = Resolve(root, "plain"); !ok || ep.Model != "plain" {
		t.Fatalf("plain: %+v ok=%v", ep, ok)
	}
}

func TestResolveNothingConfigured(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEM_OPENAI_API_KEY", "")
	if _, ok := Resolve(root, "m"); ok {
		t.Fatal("expected ok=false with no registry and no env")
	}
}

func TestProviderDefaultBaseURL(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEM_OPENAI_API_KEY", "")
	r := Load(root)
	r.Providers["p"] = Provider{Kind: "openai", Token: "k"}
	r.Bind("m", "p", "")
	if err := r.Save(root); err != nil {
		t.Fatal(err)
	}
	if ep, ok := Resolve(root, "m"); !ok || ep.BaseURL != DefaultBaseURL {
		t.Fatalf("default base url: %+v ok=%v", ep, ok)
	}
}
