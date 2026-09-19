// Package config loads jev-proxy's YAML configuration with defaults.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"jev-proxy/internal/route"
)

// Config covers both operating modes:
//
//   - legacy passthrough: set `upstream` only. The client's Authorization
//     rides through untouched; nothing else changes.
//   - gateway: set `providers` (+ `routes`/`default`). jev-proxy becomes the
//     key vault: it routes by model name and injects each provider's key
//     from the environment (.env supported).
type Config struct {
	Listen   string  `yaml:"listen"`
	Upstream string  `yaml:"upstream"`
	Jev      Jev     `yaml:"jev"`
	Scoring  Scoring `yaml:"scoring"`
	Queue    Queue   `yaml:"queue"`
	Log      Log     `yaml:"log"`

	Providers map[string]*Provider `yaml:"providers"`
	Routes    []Route              `yaml:"routes"`
	Default   string               `yaml:"default"`
	Auth      Auth                 `yaml:"auth"`

	// MissingKeys lists provider api_key_env names absent from the
	// environment at startup. Requests to those providers fail at call
	// time; surfacing them early is a startup warning, not fatal.
	MissingKeys []string `yaml:"-"`
}

// Provider is one upstream LLM API.
type Provider struct {
	// Endpoint is the API base, e.g. https://api.deepseek.com/v1.
	// A URL already ending in /chat/completions is used verbatim.
	Endpoint  string `yaml:"endpoint"`
	APIKeyEnv string `yaml:"api_key_env"` // set => proxy injects "Bearer <key>"
	// ModelsPath overrides the models listing path (default "/models").
	ModelsPath string `yaml:"models_path"`
	// IncludeModels optionally filters the /v1/models view (glob list).
	IncludeModels []string `yaml:"include_models"`

	// legacy marks the synthesized single-upstream provider: it never
	// injects auth and keeps the historical /v1/chat/completions append.
	legacy bool
}

// Route is one ordered name-based rule (see package route).
type Route struct {
	Match    string `yaml:"match"`
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"` // optional forward-time rename
}

// Auth optionally locks the gateway: when ClientKeyEnv resolves to a
// non-empty value, every /v1 request must present it as its Bearer token.
type Auth struct {
	ClientKeyEnv string `yaml:"client_key_env"`
}

type Jev struct {
	// Provider selects the endpoint family: "typesafe" or "openrouter".
	Provider string `yaml:"provider"`
	// Endpoint overrides the provider's default URL (full URL, not a base).
	Endpoint  string        `yaml:"endpoint"`
	APIKeyEnv string        `yaml:"api_key_env"`
	Model     string        `yaml:"model"`
	Timeout   time.Duration `yaml:"timeout"`
}

type Scoring struct {
	SampleRate      float64 `yaml:"sample_rate"`
	ContextTurns    int     `yaml:"context_turns"`
	SystemMaxChars  int     `yaml:"system_max_chars"`
	MessageMaxChars int     `yaml:"message_max_chars"`
	ReplyMaxChars   int     `yaml:"reply_max_chars"`
	FlagReviewBelow float64 `yaml:"flag_review_below"`
	SuspectL0Below  float64 `yaml:"suspect_l0_below"`
	SafetyFlagAbove float64 `yaml:"safety_flag_above"`
	// MinConfidence: answers below it are still recorded but marked
	// low_confidence and never raise flags — drift signal, not decision input.
	MinConfidence float64 `yaml:"min_confidence"`
}

type Queue struct {
	Cap     int `yaml:"cap"`
	Workers int `yaml:"workers"`
}

type Log struct {
	Path string `yaml:"path"`
}

// Load reads the YAML file at path, applies defaults, and validates.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	cfg.applyDefaults()
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if !cfg.Jev.hasAPIKey() {
		return nil, fmt.Errorf("config: set %s in the environment (jev.api_key_env)", cfg.Jev.APIKeyEnv)
	}
	cfg.MissingKeys = cfg.checkProviderKeys()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	setStr(&c.Listen, "127.0.0.1:8080")
	switch c.Jev.Provider {
	case "", "openrouter":
		c.Jev.Provider = "openrouter"
		setStr(&c.Jev.Endpoint, "https://openrouter.ai/api/alpha/decisions")
		setStr(&c.Jev.APIKeyEnv, "OPENROUTER_API_KEY")
		// OpenRouter's Decisions route has no default model; an omitted
		// model is rejected with 400 (unlike TypeSafe's own API). Author
		// namespaces on OpenRouter are prefixed with "~" (see typesafe-mcp
		// cmd/evaluate/main.go); the unprefixed slug 400s with
		// "Model ... does not exist".
		setStr(&c.Jev.Model, "~typesafe/jev-latest")
	case "typesafe":
		setStr(&c.Jev.Endpoint, "https://api.typesafe.ai/v1/systemone")
		setStr(&c.Jev.APIKeyEnv, "TYPESAFE_API_KEY")
	default:
		// Custom provider: Endpoint must be set explicitly; key env defaults.
		setStr(&c.Jev.APIKeyEnv, "JEV_API_KEY")
	}
	c.Jev.Timeout = durOr(c.Jev.Timeout, 5*time.Second)
	s := &c.Scoring
	if s.SampleRate == 0 {
		s.SampleRate = 1.0
	}
	if s.ContextTurns == 0 {
		s.ContextTurns = 10
	}
	if s.SystemMaxChars == 0 {
		s.SystemMaxChars = 1000
	}
	if s.MessageMaxChars == 0 {
		s.MessageMaxChars = 2000
	}
	if s.ReplyMaxChars == 0 {
		s.ReplyMaxChars = 8000
	}
	if s.FlagReviewBelow == 0 {
		s.FlagReviewBelow = 1.0
	}
	if s.SuspectL0Below == 0 {
		s.SuspectL0Below = 0.5
	}
	if s.SafetyFlagAbove == 0 {
		s.SafetyFlagAbove = 0.5
	}
	if s.MinConfidence == 0 {
		s.MinConfidence = 0.7
	}
	if c.Queue.Cap == 0 {
		c.Queue.Cap = 1000
	}
	if c.Queue.Workers == 0 {
		c.Queue.Workers = 2
	}
	setStr(&c.Log.Path, "scores.jsonl")
}

// normalize wires the two modes apart: no providers → a single synthesized
// legacy passthrough provider; providers set → validate names/defaults.
func (c *Config) normalize() error {
	if len(c.Providers) == 0 {
		if c.Upstream == "" {
			return fmt.Errorf("config: set either `upstream` (passthrough) or `providers` (gateway)")
		}
		if len(c.Routes) > 0 || c.Default != "" {
			return fmt.Errorf("config: routes/default require providers")
		}
		c.Providers = map[string]*Provider{"upstream": {Endpoint: c.Upstream, legacy: true}}
		c.Default = "upstream"
		return nil
	}
	if c.Upstream != "" {
		return fmt.Errorf("config: set `upstream` OR `providers`, not both")
	}
	for name, p := range c.Providers {
		if name == "" || p == nil || p.Endpoint == "" {
			return fmt.Errorf("config: provider %q needs an endpoint", name)
		}
		if p.ModelsPath == "" {
			p.ModelsPath = "/models"
		}
	}
	for _, r := range c.Routes {
		if _, ok := c.Providers[r.Provider]; !ok {
			return fmt.Errorf("config: route %q targets unknown provider %q", r.Match, r.Provider)
		}
	}
	if c.Default == "" {
		if len(c.Providers) == 1 {
			for name := range c.Providers {
				c.Default = name
			}
		} else {
			return fmt.Errorf("config: `default` provider is required with multiple providers")
		}
	} else if _, ok := c.Providers[c.Default]; !ok {
		return fmt.Errorf("config: default provider %q not registered", c.Default)
	}
	return nil
}

// IsGateway reports provider-routing mode (vs legacy passthrough).
func (c *Config) IsGateway() bool {
	return !(len(c.Providers) == 1 && c.Providers["upstream"] != nil && c.Providers["upstream"].legacy)
}

// Resolver builds the model-name router from config.
func (c *Config) Resolver() *route.Resolver {
	names := make(map[string]bool, len(c.Providers))
	for n := range c.Providers {
		names[n] = true
	}
	rules := make([]route.Rule, 0, len(c.Routes))
	for _, r := range c.Routes {
		rules = append(rules, route.Rule{Match: r.Match, Provider: r.Provider, Model: r.Model})
	}
	return route.New(rules, names, c.Default)
}

// APIKey resolves a provider's key from the environment at call time, so a
// rotated key is picked up without restarting. Empty means not configured.
func (p *Provider) APIKey() string {
	if p.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(p.APIKeyEnv)
}

func (p *Provider) InjectsAuth() bool { return !p.legacy && p.APIKeyEnv != "" }

// Legacy reports the synthesized single-upstream provider (no auth injection,
// historical /v1 path append).
func (p *Provider) Legacy() bool { return p.legacy }

// Normalize makes a bare Config usable: without a Load/parse pass (e.g. test
// structs) an `upstream`-only config still gets its legacy provider. Safe to
// call repeatedly.
func (c *Config) Normalize() error { return c.normalize() }

// ClientKey resolves the agent-side gate token; "" disables auth entirely.
func (a Auth) ClientKey() string {
	if a.ClientKeyEnv == "" {
		return ""
	}
	return os.Getenv(a.ClientKeyEnv)
}

func (c *Config) checkProviderKeys() (missing []string) {
	for name, p := range c.Providers {
		if p.InjectsAuth() && p.APIKey() == "" {
			missing = append(missing, name+":"+p.APIKeyEnv)
		}
	}
	return missing
}

func setStr(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

func durOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (j Jev) hasAPIKey() bool {
	_, ok := os.LookupEnv(j.APIKeyEnv)
	return ok
}

// APIKey reads the key from the environment at call time, so a key rotated
// after startup is picked up on the next scoring call.
func (j Jev) APIKey() string {
	return os.Getenv(j.APIKeyEnv)
}
