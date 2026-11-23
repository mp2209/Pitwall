// Package config loads the gateway's YAML configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mp2209/pitwall/internal/guard"
)

// Wire formats a provider can speak.
const (
	KindOpenAI    = "openai"
	KindAnthropic = "anthropic"
)

type Config struct {
	Listen string `yaml:"listen"`
	// DevKeys is a Phase 1 stand-in for virtual keys: if non-empty, callers
	// must send one of these. Empty means no auth, for local use only.
	DevKeys []string `yaml:"dev_keys"`
	// DefaultMaxTokens fills max_tokens when an OpenAI-format request is sent
	// to an Anthropic-format provider without one; Anthropic requires it.
	DefaultMaxTokens int `yaml:"default_max_tokens"`
	// MaxRequestBytes caps request bodies.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
	// PricesFile is the reviewed price table, relative to this config
	// file. With a database, its rows are copied into model_prices at
	// startup (existing rows are never changed).
	PricesFile string `yaml:"prices_file"`
	// BudgetFailOpen lets calls through when Redis is unreachable, logging
	// the failure, instead of refusing them. Default true.
	BudgetFailOpen *bool `yaml:"budget_fail_open"`
	// CacheOptimizerDefault turns the prompt-cache optimizer on in dev mode,
	// where there are no keys. With keys, each key's cache_optimizer_on
	// decides.
	CacheOptimizerDefault bool `yaml:"cache_optimizer_default"`
	// Guard sets the runaway-agent guard's limits; anything left out keeps
	// its default (see guard.Defaults).
	Guard GuardConfig `yaml:"guard"`
	// TaskIdleGap closes an inferred task: a call after this long without
	// one starts a new task. Default 5m.
	TaskIdleGap time.Duration `yaml:"task_idle_gap"`
	// MetricsListen serves Prometheus metrics on a separate address, kept
	// off the public port. Empty disables metrics. Default ":9464".
	MetricsListen *string    `yaml:"metrics_listen"`
	Providers     []Provider `yaml:"providers"`
	Models        []Model    `yaml:"models"`

	dir string // directory of the config file, for relative paths
}

// GuardConfig is the guard section of the config file.
type GuardConfig struct {
	Enabled          *bool        `yaml:"enabled"`
	RepeatedExchange *guard.Limit `yaml:"repeated_exchange"`
	RepeatedToolCall *guard.Limit `yaml:"repeated_tool_call"`
	ContextTokens    *guard.Limit `yaml:"context_tokens"`
	TaskCostUSD      *guard.Limit `yaml:"task_cost_usd"`
}

// GuardSettings merges the file's guard section over the defaults.
func (c *Config) GuardSettings() guard.Config {
	g := guard.Defaults()
	if c.Guard.Enabled != nil {
		g.Enabled = *c.Guard.Enabled
	}
	for _, f := range []struct {
		from *guard.Limit
		to   *guard.Limit
	}{
		{c.Guard.RepeatedExchange, &g.RepeatedExchange}, {c.Guard.RepeatedToolCall, &g.RepeatedToolCall},
		{c.Guard.ContextTokens, &g.ContextTokens}, {c.Guard.TaskCostUSD, &g.TaskCostUSD},
	} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	return g
}

// FailOpen reports whether calls go through when budgets can't be checked.
func (c *Config) FailOpen() bool { return c.BudgetFailOpen == nil || *c.BudgetFailOpen }

// PricesPath resolves PricesFile against the config file's directory.
func (c *Config) PricesPath() string {
	if c.PricesFile == "" || filepath.IsAbs(c.PricesFile) {
		return c.PricesFile
	}
	return filepath.Join(c.dir, c.PricesFile)
}

type Provider struct {
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"` // openai or anthropic
	BaseURL string `yaml:"base_url"`
	// BaseURLEnv, if set and the variable is non-empty, overrides BaseURL.
	BaseURLEnv string `yaml:"base_url_env"`
	// APIKeyEnv names the environment variable holding the real provider
	// key. Keys are never written in the config file.
	APIKeyEnv string `yaml:"api_key_env"`
	// MaxTokensParam is the field an Anthropic-format request's max_tokens
	// becomes when sent to an OpenAI-format provider: max_tokens (default,
	// what Ollama accepts) or max_completion_tokens (what OpenAI's newer
	// models require).
	MaxTokensParam string `yaml:"max_tokens_param"`
	// Free marks a provider whose calls cost nothing (a local model), so
	// they are recorded at $0 instead of flagged as unpriced.
	Free bool `yaml:"free"`
}

// Model is one routing rule. Rules are tried in order; the first whose
// Match pattern (path.Match syntax, for example "claude-*") fits the
// requested model wins.
type Model struct {
	Match    string `yaml:"match"`
	Provider string `yaml:"provider"`
	// Upstream replaces the model name sent to the provider.
	Upstream string `yaml:"upstream"`
	// StripPrefix is removed from the requested name before sending, so
	// "ollama/*" with strip_prefix "ollama/" sends "qwen2.5-coder:7b" for
	// "ollama/qwen2.5-coder:7b".
	StripPrefix string `yaml:"strip_prefix"`
}

// Load reads and validates a config file.
func Load(file string) (*Config, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, err
	}
	c.dir = filepath.Dir(file)
	return c, nil
}

// Parse decodes and validates YAML config.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DefaultMaxTokens == 0 {
		c.DefaultMaxTokens = 8192
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = 32 << 20
	}
	if c.PricesFile == "" {
		c.PricesFile = "prices.yaml"
	}
	if c.TaskIdleGap == 0 {
		c.TaskIdleGap = 5 * time.Minute
	}
	if c.MetricsListen == nil {
		addr := ":9464"
		c.MetricsListen = &addr
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.BaseURLEnv != "" {
			if v := os.Getenv(p.BaseURLEnv); v != "" {
				p.BaseURL = v
			}
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if p.MaxTokensParam == "" {
			p.MaxTokensParam = "max_tokens"
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	names := map[string]bool{}
	for _, p := range c.Providers {
		switch {
		case p.Name == "":
			errs = append(errs, errors.New("provider with empty name"))
		case names[p.Name]:
			errs = append(errs, fmt.Errorf("provider %q defined twice", p.Name))
		}
		names[p.Name] = true
		if p.Kind != KindOpenAI && p.Kind != KindAnthropic {
			errs = append(errs, fmt.Errorf("provider %q: kind must be %q or %q, got %q", p.Name, KindOpenAI, KindAnthropic, p.Kind))
		}
		if !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
			errs = append(errs, fmt.Errorf("provider %q: base_url must start with http:// or https://", p.Name))
		}
		if p.MaxTokensParam != "max_tokens" && p.MaxTokensParam != "max_completion_tokens" {
			errs = append(errs, fmt.Errorf("provider %q: max_tokens_param must be max_tokens or max_completion_tokens", p.Name))
		}
	}
	for name, l := range map[string]*guard.Limit{
		"repeated_exchange": c.Guard.RepeatedExchange, "repeated_tool_call": c.Guard.RepeatedToolCall,
		"context_tokens": c.Guard.ContextTokens, "task_cost_usd": c.Guard.TaskCostUSD,
	} {
		if l == nil {
			continue
		}
		if l.Warn < 0 || l.Stop < 0 || (l.Warn > 0 && l.Stop > 0 && l.Warn > l.Stop) {
			errs = append(errs, fmt.Errorf("guard.%s: limits must be 0 (off) or positive, with warn at or below stop", name))
		}
	}
	for i, m := range c.Models {
		if m.Match == "" {
			errs = append(errs, fmt.Errorf("models[%d]: match is empty", i))
		} else if _, err := path.Match(m.Match, ""); err != nil {
			errs = append(errs, fmt.Errorf("models[%d]: bad pattern %q: %w", i, m.Match, err))
		}
		if !names[m.Provider] {
			errs = append(errs, fmt.Errorf("models[%d] (%s): unknown provider %q", i, m.Match, m.Provider))
		}
	}
	return errors.Join(errs...)
}
