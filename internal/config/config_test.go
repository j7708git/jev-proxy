package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTemp loads from a minimal config that only sets upstream.
func loadWithUpstream(t *testing.T, provider string) *Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	yaml := "upstream: https://example.test\n"
	if provider != "" {
		yaml += "jev:\n  provider: " + provider + "\n"
	}
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENROUTER_API_KEY", "x")
	t.Setenv("TYPESAFE_API_KEY", "x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The OpenRouter Decisions route 400s unless the model carries the "~"
// author namespace; the previous default (typesafe/jev-latest) is exactly
// what broke the first end-to-end run.
func TestOpenRouterDefaultsToTildeSlug(t *testing.T) {
	cfg := loadWithUpstream(t, "openrouter")
	if cfg.Jev.Model != "~typesafe/jev-latest" {
		t.Fatalf("openrouter default model = %q, want ~typesafe/jev-latest", cfg.Jev.Model)
	}
	if cfg.Jev.Endpoint != "https://openrouter.ai/api/alpha/decisions" {
		t.Fatalf("endpoint = %q", cfg.Jev.Endpoint)
	}
}

func TestTypesafeDefaults(t *testing.T) {
	cfg := loadWithUpstream(t, "typesafe")
	// TypeSafe's own API has a route default, so model stays empty.
	if cfg.Jev.Model != "" {
		t.Fatalf("typesafe default model = %q, want empty", cfg.Jev.Model)
	}
	if cfg.Jev.Endpoint != "https://api.typesafe.ai/v1/systemone" {
		t.Fatalf("endpoint = %q", cfg.Jev.Endpoint)
	}
}

func TestTruncationDefaults(t *testing.T) {
	cfg := loadWithUpstream(t, "openrouter")
	s := cfg.Scoring
	if s.ContextTurns != 10 || s.SystemMaxChars != 1000 ||
		s.MessageMaxChars != 2000 || s.ReplyMaxChars != 8000 {
		t.Fatalf("truncation defaults wrong: %+v", s)
	}
	if s.FlagReviewBelow != 1.0 || s.SuspectL0Below != 0.5 || s.SafetyFlagAbove != 0.5 {
		t.Fatalf("threshold defaults wrong: %+v", s)
	}
	if s.MinConfidence != 0.7 {
		t.Fatalf("min_confidence default = %v, want 0.7", s.MinConfidence)
	}
	if cfg.Listen != "127.0.0.1:8080" {
		t.Fatalf("listen default = %q, want localhost-only for a key vault", cfg.Listen)
	}
}

func TestGatewayConfigModes(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "x")
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gw := `
providers:
  openrouter: { endpoint: https://openrouter.ai/api/v1, api_key_env: OPENROUTER_API_KEY }
  deepseek:   { endpoint: https://api.deepseek.com/v1, api_key_env: DEEPSEEK_API_KEY }
routes:
  - { match: "deepseek-chat", provider: deepseek }
default: openrouter
`
	cfg, err := Load(write(t, gw))
	if err != nil {
		t.Fatalf("gateway config: %v", err)
	}
	if !cfg.IsGateway() {
		t.Fatal("should be gateway")
	}
	// deepseek key absent from env → surfaced as a startup warning, not fatal
	if len(cfg.MissingKeys) != 1 || cfg.MissingKeys[0] != "deepseek:DEEPSEEK_API_KEY" {
		t.Fatalf("missing keys = %v", cfg.MissingKeys)
	}
	d, err := cfg.Resolver().Resolve("deepseek-chat")
	if err != nil || d.Provider != "deepseek" || d.How != "route" {
		t.Fatalf("resolve deepseek-chat: %+v %v", d, err)
	}
	d, _ = cfg.Resolver().Resolve("openrouter/deepseek/deepseek-chat-v3")
	if d.Provider != "openrouter" || d.Model != "deepseek/deepseek-chat-v3" {
		t.Fatalf("namespaced: %+v", d)
	}

	// rejections: both modes at once; unknown route target; unknown default
	if _, err := Load(write(t, gw+"upstream: https://x\n")); err == nil {
		t.Fatal("upstream+providers must error")
	}
	if _, err := Load(write(t, `
providers: { a: { endpoint: https://x/v1 } }
routes: [ { match: "*", provider: ghost } ]
`)); err == nil {
		t.Fatal("route to unknown provider must error")
	}
	if _, err := Load(write(t, `default: nope
`+gw)); err == nil {
		t.Fatal("unknown default must error")
	}
	// single provider without explicit default becomes the default
	solo := `
providers: { solo: { endpoint: https://x/v1 } }
`
	if _, err := Load(write(t, solo)); err != nil {
		t.Fatalf("single-provider default: %v", err)
	}
}
