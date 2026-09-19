// Package config loads jev-proxy's YAML configuration with defaults.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full proxy configuration. Every field has a workable default
// except Upstream, which must point at an OpenAI-compatible chat endpoint.
type Config struct {
	Listen   string  `yaml:"listen"`
	Upstream string  `yaml:"upstream"`
	Jev      Jev     `yaml:"jev"`
	Scoring  Scoring `yaml:"scoring"`
	Queue    Queue   `yaml:"queue"`
	Log      Log     `yaml:"log"`
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

// Load reads the YAML file at path and applies defaults for absent fields.
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
	if cfg.Upstream == "" {
		return nil, fmt.Errorf("config: upstream is required (OpenAI-compatible chat completions URL)")
	}
	if !cfg.Jev.hasAPIKey() {
		return nil, fmt.Errorf("config: set %s in the environment (jev.api_key_env)", cfg.Jev.APIKeyEnv)
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	setStr(&c.Listen, ":8080")
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
	if c.Queue.Cap == 0 {
		c.Queue.Cap = 1000
	}
	if c.Queue.Workers == 0 {
		c.Queue.Workers = 2
	}
	setStr(&c.Log.Path, "scores.jsonl")
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
