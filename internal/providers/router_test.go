package providers

import (
	"context"
	"net/http"
	"testing"

	"github.com/mp2209/pitwall/internal/config"
)

func testRouter(t *testing.T) *Router {
	t.Helper()
	t.Setenv("TEST_ANTHROPIC_KEY", "sk-ant-real")
	c, err := config.Parse([]byte(`
providers:
  - {name: anthropic, kind: anthropic, base_url: "https://api.anthropic.test", api_key_env: TEST_ANTHROPIC_KEY}
  - {name: ollama, kind: openai, base_url: "http://ollama:11434"}
models:
  - {match: cheap, provider: ollama, upstream: "qwen2.5-coder:7b"}
  - {match: "claude-*", provider: anthropic}
  - {match: "ollama/*", provider: ollama, strip_prefix: "ollama/"}
`))
	if err != nil {
		t.Fatal(err)
	}
	return NewRouter(c)
}

func TestResolve(t *testing.T) {
	r := testRouter(t)
	cases := map[string][2]string{
		"cheap":                  {"ollama", "qwen2.5-coder:7b"},
		"claude-sonnet-4-5":      {"anthropic", "claude-sonnet-4-5"},
		"ollama/llama3.2:latest": {"ollama", "llama3.2:latest"},
	}
	for model, want := range cases {
		route, ok := r.Resolve(model)
		if !ok || route.Upstream.Name != want[0] || route.Model != want[1] {
			t.Errorf("%s -> %+v %v, want %v", model, route, ok, want)
		}
	}
	if _, ok := r.Resolve("gpt-4o"); ok {
		t.Error("unconfigured model should not resolve")
	}
	if got := r.Models(); len(got) != 1 || got[0].ID != "cheap" {
		t.Errorf("listed models = %+v", got)
	}
}

func TestNewRequestUsesProviderKeyNotClientKey(t *testing.T) {
	r := testRouter(t)
	route, _ := r.Resolve("claude-x")
	client := http.Header{}
	client.Set("x-api-key", "pw-client-key")
	client.Set("Authorization", "Bearer pw-client-key")
	client.Set("anthropic-beta", "prompt-caching-2024-07-31")
	req, err := route.Upstream.NewRequest(context.Background(), "/v1/messages", "beta=true", []byte(`{}`), client)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "https://api.anthropic.test/v1/messages?beta=true" {
		t.Errorf("url = %s", req.URL)
	}
	if req.Header.Get("x-api-key") != "sk-ant-real" || req.Header.Get("Authorization") != "" {
		t.Errorf("auth headers = %v", req.Header)
	}
	if req.Header.Get("anthropic-version") == "" || req.Header.Get("anthropic-beta") != "prompt-caching-2024-07-31" {
		t.Errorf("anthropic headers = %v", req.Header)
	}

	ollama, _ := r.Resolve("cheap")
	req, _ = ollama.Upstream.NewRequest(context.Background(), "/v1/chat/completions", "", nil, client)
	if req.Header.Get("Authorization") != "" || req.Header.Get("anthropic-beta") != "" {
		t.Errorf("ollama should get no client or anthropic headers: %v", req.Header)
	}
}
