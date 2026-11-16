package config

import (
	"strings"
	"testing"
)

func TestParseDefaultsAndEnvOverride(t *testing.T) {
	t.Setenv("TEST_BASE", "http://override:1234/")
	c, err := Parse([]byte(`
providers:
  - {name: a, kind: openai, base_url: "http://x/", base_url_env: TEST_BASE}
models:
  - {match: "m-*", provider: a}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.DefaultMaxTokens != 8192 || c.MaxRequestBytes == 0 {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.Providers[0].BaseURL != "http://override:1234" {
		t.Errorf("base url = %q", c.Providers[0].BaseURL)
	}
	if c.Providers[0].MaxTokensParam != "max_tokens" {
		t.Errorf("max tokens param = %q", c.Providers[0].MaxTokensParam)
	}
}

func TestParseRejectsBadConfig(t *testing.T) {
	_, err := Parse([]byte(`
providers:
  - {name: a, kind: grpc, base_url: "ftp://x"}
  - {name: a, kind: openai, base_url: "http://x"}
models:
  - {match: "[", provider: a}
  - {match: "x", provider: missing}
`))
	if err == nil {
		t.Fatal("want errors")
	}
	for _, want := range []string{"kind must be", "base_url must start", "defined twice", "bad pattern", "unknown provider"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte("listn: ':1'\n")); err == nil {
		t.Fatal("typo in a field name should fail")
	}
}

func TestShippedConfigIsValid(t *testing.T) {
	if _, err := Load("../../config/pitwall.yaml"); err != nil {
		t.Fatal(err)
	}
}
