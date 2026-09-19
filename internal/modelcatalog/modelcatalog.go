// Package modelcatalog fetches and merges the model lists of all gateway
// providers into one namespaced catalog: each entry is advertised as
// "<provider>/<provider's-model-id>". Because Nous and OpenRouter host many
// identical model IDs, the namespace is what makes the agent's choice
// unambiguous — and it is exactly the string the router resolves.
package modelcatalog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"jev-proxy/internal/config"
	"jev-proxy/internal/route"
)

// Entry is one namespaced model.
type Entry struct {
	ID       string // "provider/model-id-as-provider-knows-it"
	Provider string
}

// userAgent is sent on proxy-originated calls (model listing). Never rely
// on Go's default UA: it trips bot filters (Nous Portal answers 403).
const userAgent = "jev-proxy/model-catalog"

// Catalog caches merged provider listings with a TTL.
type Catalog struct {
	cfg    *config.Config
	client *http.Client
	ttl    time.Duration

	mu      sync.Mutex
	cached  []Entry
	stale   []Entry
	partial []string // providers that failed in the last refresh
	at      time.Time
	refresh sync.Mutex // single-flight: one refresh serves all waiters
}

// New builds a catalog for gateway configs; listing interval 5 minutes.
func New(cfg *config.Config) *Catalog {
	return &Catalog{
		cfg:    cfg,
		client: &http.Client{Timeout: 15 * time.Second},
		ttl:    5 * time.Minute,
	}
}

// ttlRetry is the (much shorter) cache lifetime after a refresh that had
// failures: a flaky provider is re-attempted on the next listing instead of
// being frozen out for the full TTL.
const ttlRetry = 30 * time.Second

// freshLocked reports cache validity; caller must hold c.mu.
func (c *Catalog) freshLocked() bool {
	ttl := c.ttl
	if len(c.partial) > 0 {
		ttl = ttlRetry
	}
	return c.cached != nil && time.Since(c.at) < ttl
}

// Models returns the merged catalog, refreshing when the cache is older
// than the TTL. Providers whose fetch fails are reported in `partial`; if a
// previous refresh succeeded for them, their stale entries are kept.
func (c *Catalog) Models(ctx context.Context) (entries []Entry, partial []string) {
	c.mu.Lock()
	fresh := c.freshLocked()
	entries, partial = c.cached, c.partial
	c.mu.Unlock()
	if fresh {
		return entries, partial
	}

	c.refresh.Lock() // single-flight
	defer c.refresh.Unlock()
	c.mu.Lock() // re-check after acquiring the flight
	if c.freshLocked() {
		e, p := c.cached, c.partial
		c.mu.Unlock()
		return e, p
	}
	c.mu.Unlock()

	type res struct {
		name    string
		entries []Entry
		err     string
	}
	var (
		wg        sync.WaitGroup
		ch        = make(chan res, len(c.cfg.Providers))
		providers = c.cfg.Providers
	)
	for name, p := range providers {
		if p.Legacy() || !p.InjectsAuth() {
			continue // passthrough upstream: no key to list with
		}
		wg.Add(1)
		go func(name string, p *config.Provider) {
			defer wg.Done()
			es, err := c.fetch(ctx, name, p)
			r := res{name: name, entries: es}
			if err != nil {
				r.err = err.Error()
			}
			ch <- r
		}(name, p)
	}
	go func() { wg.Wait(); close(ch) }()

	var merged []Entry
	var failed []string
	for r := range ch {
		if r.err != "" {
			failed = append(failed, r.name)
			// keep previous entries for this provider if we have them
			c.mu.Lock()
			for _, s := range c.stale {
				if s.Provider == r.name {
					merged = append(merged, s)
				}
			}
			c.mu.Unlock()
			continue
		}
		merged = append(merged, r.entries...)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].ID < merged[j].ID })
	sort.Strings(failed)

	c.mu.Lock()
	c.cached, c.partial, c.stale, c.at = merged, failed, merged, time.Now()
	c.mu.Unlock()
	return merged, failed
}

// fetches one provider's list and namespaces every id.
func (c *Catalog) fetch(ctx context.Context, name string, p *config.Provider) ([]Entry, error) {
	key := p.APIKey()
	if key == "" {
		return nil, &listError{name, "no api key in env " + p.APIKeyEnv}
	}
	u := modelsURL(p.Endpoint, p.ModelsPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &listError{name, err.Error()}
	}
	// The Go default UA ("Go-http-client/2.0") is bot-blocked by some
	// providers' edge rules — Nous answers 403. Identify ourselves.
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &listError{name, err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, &listError{name, err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		return nil, &listError{name, resp.Status}
	}
	var raw struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, &listError{name, "unparseable listing: " + err.Error()}
	}
	var out []Entry
	for _, m := range raw.Data {
		if m.ID == "" {
			continue
		}
		if len(p.IncludeModels) > 0 && !anyMatch(p.IncludeModels, m.ID) {
			continue
		}
		out = append(out, Entry{ID: name + "/" + m.ID, Provider: name})
	}
	return out, nil
}

// modelsURL turns an endpoint base into its listing URL:
// "https://x/v1" → "https://x/v1/models"; a full chat URL swaps its suffix.
func modelsURL(endpoint, path string) string {
	endpoint = strings.TrimSuffix(endpoint, "/chat/completions")
	endpoint = strings.TrimSuffix(endpoint, "/")
	if path == "" {
		path = "/models"
	}
	return endpoint + path
}

func anyMatch(patterns []string, s string) bool {
	for _, pat := range patterns {
		if route.Match(pat, s) {
			return true
		}
	}
	return false
}

type listError struct{ provider, msg string }

func (e *listError) Error() string { return e.provider + ": " + e.msg }
