package modelcatalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"jev-proxy/internal/config"
)

func listSrv(t *testing.T, ids ...string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/models" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		var data []map[string]any
		for _, id := range ids {
			data = append(data, map[string]any{"id": id})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func catConfig(a, b *httptest.Server) *config.Config {
	return &config.Config{
		Providers: map[string]*config.Provider{
			"alpha": {Endpoint: a.URL, APIKeyEnv: "CAT_TEST_KEY"},
			"beta":  {Endpoint: b.URL, APIKeyEnv: "CAT_TEST_KEY"},
		},
		Default: "alpha",
	}
}

func TestModelsMergeAndNamespace(t *testing.T) {
	t.Setenv("CAT_TEST_KEY", "k")
	a, _ := listSrv(t, "gpt-4o", "deepseek/deepseek-chat-v3")
	b, _ := listSrv(t, "beta-one")
	c := New(catConfig(a, b))
	entries, partial := c.Models(context.Background())
	if len(partial) != 0 {
		t.Fatalf("partial = %v", partial)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.ID] = true
	}
	for _, want := range []string{"alpha/gpt-4o", "alpha/deepseek/deepseek-chat-v3", "beta/beta-one"} {
		if !got[want] {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
	// The collision case: same tail on two providers stays distinguishable.
	d, _ := listSrv(t, "deepseek/deepseek-chat-v3")
	c.cfg.Providers["delta"] = &config.Provider{Endpoint: d.URL, APIKeyEnv: "CAT_TEST_KEY"}
	c.at = time.Time{} // expire cache
	entries, _ = c.Models(context.Background())
	var seen [2]bool
	for _, e := range entries {
		switch e.ID {
		case "alpha/deepseek/deepseek-chat-v3":
			seen[0] = true
		case "delta/deepseek/deepseek-chat-v3":
			seen[1] = true
		}
	}
	if !seen[0] || !seen[1] {
		t.Fatalf("namespaced collision entries = %v", seen)
	}
}

func TestCacheTTL(t *testing.T) {
	t.Setenv("CAT_TEST_KEY", "k")
	a, ah := listSrv(t, "x")
	b, _ := listSrv(t, "y")
	c := New(catConfig(a, b))
	c.Models(context.Background())
	first := ah.Load()
	c.Models(context.Background()) // must come from cache
	if got := ah.Load(); got != first {
		t.Fatalf("refetched within TTL: %d -> %d", first, got)
	}
	c.at = time.Time{} // expire
	c.Models(context.Background())
	if got := ah.Load(); got <= first {
		t.Fatalf("no refetch after expiry: %d", got)
	}
}

func TestPartialFailureAndNoKeySkip(t *testing.T) {
	t.Setenv("CAT_TEST_KEY", "k")
	a, _ := listSrv(t, "x")
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(dead.Close)
	cfg := catConfig(a, dead)
	cfg.Providers["nokey"] = &config.Provider{Endpoint: a.URL} // no api_key_env → skipped
	c := New(cfg)
	entries, partial := c.Models(context.Background())
	if len(partial) != 1 || partial[0] != "beta" {
		t.Fatalf("partial = %v, want [beta]", partial)
	}
	for _, e := range entries {
		if e.Provider == "nokey" {
			t.Fatal("keyless provider must be omitted from the merged list")
		}
	}
	if len(entries) == 0 || entries[0].ID != "alpha/x" {
		t.Fatalf("entries = %v", entries)
	}
}

func TestIncludeFilter(t *testing.T) {
	t.Setenv("CAT_TEST_KEY", "k")
	a, _ := listSrv(t, "gpt-4o", "gpt-4o-mini", "whisper-1")
	b, _ := listSrv(t, "unused")
	cfg := catConfig(a, b)
	cfg.Providers["alpha"].IncludeModels = []string{"gpt-4o*"}
	c := New(cfg)
	entries, _ := c.Models(context.Background())
	n := 0
	for _, e := range entries {
		if e.Provider == "alpha" {
			n++
			if e.ID != "alpha/gpt-4o" && e.ID != "alpha/gpt-4o-mini" {
				t.Fatalf("filter leaked %q", e.ID)
			}
		}
	}
	if n != 2 {
		t.Fatalf("filtered alpha entries = %d, want 2", n)
	}
}

func TestPartialFailureShortRetryWindow(t *testing.T) {
	t.Setenv("CAT_TEST_KEY", "k")
	var alphaDown atomic.Bool
	var alphaHits atomic.Int64
	alpha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alphaHits.Add(1)
		if alphaDown.Load() {
			w.WriteHeader(502)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "x"}}})
	}))
	t.Cleanup(alpha.Close)
	b, _ := listSrv(t, "y")
	c := New(catConfig(alpha, b))

	alphaDown.Store(true)
	_, partial := c.Models(context.Background())
	if len(partial) != 1 || partial[0] != "alpha" {
		t.Fatalf("first pass partial = %v, want [alpha]", partial)
	}
	downHits := alphaHits.Load()
	c.Models(context.Background()) // inside the short retry window: no hammering
	if got := alphaHits.Load(); got != downHits {
		t.Fatalf("retried within debounce window: %d -> %d", downHits, got)
	}
	// The failure window is the SHORT one, not the 5-minute full TTL.
	if c.ttl <= ttlRetry {
		t.Fatalf("test needs ttl > ttlRetry, got %v <= %v", c.ttl, ttlRetry)
	}
	c.at = time.Now().Add(-ttlRetry - time.Second)
	alphaDown.Store(false)
	entries, partial := c.Models(context.Background())
	if len(partial) != 0 {
		t.Fatalf("retry partial = %v, want none", partial)
	}
	var hasX, hasY bool
	for _, e := range entries {
		switch e.ID {
		case "alpha/x":
			hasX = true
		case "beta/y":
			hasY = true
		}
	}
	if !hasX || !hasY {
		t.Fatalf("after recovery entries = %v", entries)
	}
}

func TestModelsURL(t *testing.T) {
	if got := modelsURL("https://x.example/v1", ""); got != "https://x.example/v1/models" {
		t.Fatal(got)
	}
	if got := modelsURL("https://x.example/v1/chat/completions", ""); got != "https://x.example/v1/models" {
		t.Fatal(got)
	}
	if got := modelsURL("https://x.example/v1", "/engines"); got != "https://x.example/v1/engines" {
		t.Fatal(got)
	}
}
