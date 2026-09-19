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
}
