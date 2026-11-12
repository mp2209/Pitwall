// Package providers sends requests to model providers and decides which
// provider serves each model.
//
// Pitwall has one adapter per wire format rather than per vendor: OpenAI and
// Ollama both speak the OpenAI Chat Completions format, Anthropic speaks the
// Messages format. Translation between the two lives in internal/translate.
package providers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/internal/providers/anthropic"
)

// Upstream is one configured provider.
type Upstream struct {
	Name           string
	Kind           string // config.KindOpenAI or config.KindAnthropic
	BaseURL        string
	APIKey         string
	MaxTokensParam string
}

// Route is the result of resolving a requested model name.
type Route struct {
	Upstream *Upstream
	// Model is the name to send to the provider.
	Model string
}

// Router maps model names to providers.
type Router struct {
	upstreams map[string]*Upstream
	rules     []config.Model
}

// NewRouter builds a router, reading provider keys from the environment.
func NewRouter(c *config.Config) *Router {
	r := &Router{upstreams: map[string]*Upstream{}, rules: c.Models}
	for _, p := range c.Providers {
		u := &Upstream{
			Name:           p.Name,
			Kind:           p.Kind,
			BaseURL:        p.BaseURL,
			MaxTokensParam: p.MaxTokensParam,
		}
		if p.APIKeyEnv != "" {
			u.APIKey = os.Getenv(p.APIKeyEnv)
		}
		r.upstreams[p.Name] = u
	}
	return r
}

// Resolve finds the provider for a model, or returns false.
func (r *Router) Resolve(model string) (Route, bool) {
	for _, rule := range r.rules {
		if ok, _ := path.Match(rule.Match, model); !ok {
			continue
		}
		upstream := model
		switch {
		case rule.Upstream != "":
			upstream = rule.Upstream
		case rule.StripPrefix != "":
			upstream = strings.TrimPrefix(model, rule.StripPrefix)
		}
		return Route{Upstream: r.upstreams[rule.Provider], Model: upstream}, true
	}
	return Route{}, false
}

// ListedModel is a model name the gateway advertises on /v1/models.
type ListedModel struct {
	ID       string
	Provider string
}

// Models lists the rules that name one exact model (no wildcards).
func (r *Router) Models() []ListedModel {
	var out []ListedModel
	for _, rule := range r.rules {
		if strings.ContainsAny(rule.Match, "*?[\\") {
			continue
		}
		out = append(out, ListedModel{ID: rule.Match, Provider: rule.Provider})
	}
	return out
}

// Upstreams returns every configured provider.
func (r *Router) Upstreams() []*Upstream {
	out := make([]*Upstream, 0, len(r.upstreams))
	for _, u := range r.upstreams {
		out = append(out, u)
	}
	return out
}

// Headers a client may send that are forwarded to an Anthropic provider.
var anthropicPassHeaders = []string{"anthropic-version", "anthropic-beta"}

// NewRequest builds a POST to the provider. path includes the /v1 prefix.
// Client headers are only consulted for Anthropic protocol headers; the
// client's own credentials are never forwarded.
func (u *Upstream) NewRequest(ctx context.Context, path, rawQuery string, body []byte, client http.Header) (*http.Request, error) {
	url := u.BaseURL + path
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	switch u.Kind {
	case config.KindOpenAI:
		if u.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+u.APIKey)
		}
	case config.KindAnthropic:
		if u.APIKey != "" {
			req.Header.Set("x-api-key", u.APIKey)
		}
		for _, h := range anthropicPassHeaders {
			if v := client.Values(h); len(v) > 0 {
				req.Header[http.CanonicalHeaderKey(h)] = v
			}
		}
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", anthropic.DefaultVersion)
		}
	}
	return req, nil
}

// NewHTTPClient returns the client used for provider calls. It has no overall
// timeout because streams can run for minutes; cancellation comes from the
// request context, which ends when the agent disconnects.
func NewHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 128
	t.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: t}
}
