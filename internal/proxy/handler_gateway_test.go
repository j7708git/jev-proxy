package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"jev-proxy/internal/config"
	"jev-proxy/internal/jev"
	"jev-proxy/internal/scorer"
	"jev-proxy/internal/store"
)

// providerCapture records what the fake upstream actually received.
type providerCapture struct {
	mu    sync.Mutex
	auth  string
	model string
	hits  int
}

func (c *providerCapture) note(auth, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auth, c.model, c.hits = auth, model, c.hits+1
}

func (c *providerCapture) get() (string, string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auth, c.model, c.hits
}

// fakeProvider serves POST /chat/completions (echoing the received model)
// and GET /models (the given ids) for one gateway provider.
func fakeProvider(t *testing.T, name string, models []string) (*httptest.Server, *providerCapture) {
	t.Helper()
	rec := &providerCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // /models
			var data []map[string]any
			for _, m := range models {
				data = append(data, map[string]any{"id": m})
			}
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		rec.note(r.Header.Get("Authorization"), body.Model)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "cmpl-" + name, "model": body.Model,
			"choices": []any{map[string]any{"message": map[string]any{"content": "reply from " + name}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

type gatewayStack struct {
	ts     *httptest.Server
	st     *store.Store
	alpha  *providerCapture
	beta   *providerCapture
	alphaS *httptest.Server
	betaS  *httptest.Server
}

func newGatewayStack(t *testing.T, withClientAuth bool) *gatewayStack {
	t.Helper()
	alphaS, alpha := fakeProvider(t, "alpha", []string{"gpt-4o", "gpt-4o-mini", "deepseek/deepseek-chat-v3"})
	betaS, beta := fakeProvider(t, "beta", []string{"beta-one", "beta-deep"})
	jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(jevResponse))
	}))
	t.Cleanup(jevSrv.Close)

	t.Setenv("JEV_TEST_ALPHA_KEY", "k-alpha")
	t.Setenv("JEV_TEST_BETA_KEY", "k-beta")
	cfgAuth := config.Auth{}
	if withClientAuth {
		t.Setenv("JEV_TEST_CLIENT_KEY", "sk-client")
		cfgAuth.ClientKeyEnv = "JEV_TEST_CLIENT_KEY"
	}
	logPath := filepath.Join(t.TempDir(), "scores.jsonl")
	st, _ := store.Open(logPath)
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Providers: map[string]*config.Provider{
			"alpha": {Endpoint: alphaS.URL, APIKeyEnv: "JEV_TEST_ALPHA_KEY"},
			"beta":  {Endpoint: betaS.URL, APIKeyEnv: "JEV_TEST_BETA_KEY"},
		},
		Default: "alpha",
		Routes:  []config.Route{{Match: "beta-*", Provider: "beta"}},
		Auth:    cfgAuth,
		Jev:     config.Jev{Endpoint: jevSrv.URL, Timeout: 2 * time.Second},
		Scoring: config.Scoring{
			SampleRate: 1, ContextTurns: 10, SystemMaxChars: 1000,
			MessageMaxChars: 2000, ReplyMaxChars: 8000,
			FlagReviewBelow: 1.0, SuspectL0Below: 0.5, SafetyFlagAbove: 0.5,
			MinConfidence: 0.3,
		},
		Queue: config.Queue{Cap: 100, Workers: 1},
		Log:   config.Log{Path: logPath},
	}
	sc := scorer.New(&jev.Client{URL: jevSrv.URL, HTTP: jevSrv.Client()}, st, cfg)
	ts := httptest.NewServer(New(cfg, sc, st))
	t.Cleanup(ts.Close)
	return &gatewayStack{ts: ts, st: st, alpha: alpha, beta: beta, alphaS: alphaS, betaS: betaS}
}

func postChat(t *testing.T, url, model, clientAuth string) *http.Response {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if clientAuth != "" {
		req.Header.Set("Authorization", "Bearer "+clientAuth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestGatewayPrefixRoutingStripsAndInjectsKey(t *testing.T) {
	g := newGatewayStack(t, false)
	resp := postChat(t, g.ts.URL, "alpha/gpt-4o", "Bearer nonsense-client-token")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if p := resp.Header.Get("X-Jev-Provider"); p != "alpha" {
		t.Fatalf("X-Jev-Provider = %q", p)
	}
	auth, model, _ := g.alpha.get()
	if auth != "Bearer k-alpha" {
		t.Fatalf("provider received auth %q, gateway must inject its own", auth)
	}
	if model != "gpt-4o" {
		t.Fatalf("forwarded model = %q, want prefix stripped to gpt-4o", model)
	}
	// beta untouched; scored event carries the provider
	if _, _, n := g.beta.get(); n != 0 {
		t.Fatal("beta must not receive this request")
	}
	rid := resp.Header.Get("X-Jev-Request-Id")
	ev := waitForEvent(t, g.st, rid)
	if ev.Provider != "alpha" || ev.Status != "ok" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestGatewayRouteTableBeatsPrefix(t *testing.T) {
	g := newGatewayStack(t, false)
	postChat(t, g.ts.URL, "beta-direct", "")
	if _, model, _ := g.beta.get(); model != "beta-direct" {
		t.Fatalf("route rule forwarded %q", model)
	}
	if _, _, n := g.alpha.get(); n != 0 {
		t.Fatal("alpha must not see a beta-* route hit")
	}
}

func TestGatewayDefaultFallbackKeepsModel(t *testing.T) {
	g := newGatewayStack(t, false)
	postChat(t, g.ts.URL, "anthropic/claude-sonnet-4", "")
	if _, model, _ := g.alpha.get(); model != "anthropic/claude-sonnet-4" {
		t.Fatalf("default forward must keep the full slug, got %q", model)
	}
}

// The Nous/OpenRouter collision answer: same tail, different providers.
func TestGatewayNamespaceDisambiguatesProviders(t *testing.T) {
	g := newGatewayStack(t, false)
	// "alpha/deepseek/deepseek-chat-v3" → alpha provider with its own slug.
	postChat(t, g.ts.URL, "alpha/deepseek/deepseek-chat-v3", "")
	if _, model, _ := g.alpha.get(); model != "deepseek/deepseek-chat-v3" {
		t.Fatalf("namespaced forward = %q", model)
	}
}

func TestGatewayModelsEndpointMergesNamespaced(t *testing.T) {
	g := newGatewayStack(t, false)
	resp, err := http.Get(g.ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID       string `json:"id"`
			Provider string `json:"jev_provider"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	got := map[string]bool{}
	for _, d := range out.Data {
		got[d.ID] = true
	}
	for _, want := range []string{"alpha/gpt-4o", "alpha/deepseek/deepseek-chat-v3", "beta/beta-one"} {
		if !got[want] {
			t.Fatalf("models missing %q, got %v", want, got)
		}
	}
}

func TestGatewayClientAuth(t *testing.T) {
	g := newGatewayStack(t, true)
	if resp := postChat(t, g.ts.URL, "alpha/x", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing client key: %d, want 401", resp.StatusCode)
	}
	if resp := postChat(t, g.ts.URL, "alpha/x", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong client key: %d, want 401", resp.StatusCode)
	}
	if resp := postChat(t, g.ts.URL, "alpha/x", "sk-client"); resp.StatusCode != 200 {
		t.Fatalf("right client key: %d, want 200", resp.StatusCode)
	}
	// providers still get their OWN keys, not the client gate token
	if auth, _, _ := g.alpha.get(); auth != "Bearer k-alpha" {
		t.Fatalf("provider auth = %q", auth)
	}
}
