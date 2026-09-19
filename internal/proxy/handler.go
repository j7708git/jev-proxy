// Package proxy implements the OpenAI-compatible front: in gateway mode it
// routes each request to the right provider (keys injected from the
// environment), in legacy mode it is a transparent passthrough. Either way
// the scoring side-channel is identical: streams are tracked and scored
// asynchronously, buffered replies synchronously, contents never modified.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"jev-proxy/internal/config"
	"jev-proxy/internal/modelcatalog"
	"jev-proxy/internal/route"
	"jev-proxy/internal/scorer"
	"jev-proxy/internal/store"
)

const maxRequestBody = 8 << 20

type ctxKey struct{}

// reqInfo is what the scoring side-channel needs from the request phase.
type reqInfo struct {
	RequestID string
	Provider  string // resolved gateway provider ("" in legacy mode)
	Model     string // model as requested by the client
	Messages  []scorer.ChatMessage
	Stream    bool
	Score     bool
}

// Handler serves the proxy and its read-only side endpoints.
type Handler struct {
	cfg     *config.Config
	res     *route.Resolver
	proxies map[string]*httputil.ReverseProxy
	catalog *modelcatalog.Catalog
	scorer  *scorer.Scorer
	store   *store.Store
	mux     *http.ServeMux
}

// New wires one reverse proxy per provider (plus the scoring hooks) onto a
// single mux with the side endpoints.
func New(cfg *config.Config, sc *scorer.Scorer, st *store.Store) *Handler {
	if err := cfg.Normalize(); err != nil {
		panic("proxy: " + err.Error())
	}
	h := &Handler{
		cfg: cfg, res: cfg.Resolver(), scorer: sc, store: st,
		proxies: map[string]*httputil.ReverseProxy{},
	}
	for name, p := range cfg.Providers {
		target, err := url.Parse(p.Endpoint)
		if err != nil {
			panic(fmt.Sprintf("proxy: invalid endpoint for provider %q: %v", name, err))
		}
		provider, legacy := p, p.Legacy()
		h.proxies[name] = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				out := pr.Out.URL
				out.Scheme = target.Scheme
				out.Host = target.Host
				out.Path = chatPath(target.Path, legacy)
				out.RawPath = ""
				pr.Out.Host = target.Host
				// The gateway is the vault: replace whatever token the
				// client sent with this provider's own key. Legacy mode
				// never touches Authorization.
				if provider.InjectsAuth() {
					if k := provider.APIKey(); k != "" {
						pr.Out.Header.Set("Authorization", "Bearer "+k)
					}
				}
			},
			ModifyResponse: h.modifyResponse,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				log.Printf("proxy: upstream error: %v", err)
				w.WriteHeader(http.StatusBadGateway)
			},
		}
	}
	h.catalog = modelcatalog.New(cfg)

	h.mux = http.NewServeMux()
	h.mux.HandleFunc("/v1/chat/completions", h.chat)
	h.mux.HandleFunc("/v1/models", h.models)
	h.mux.HandleFunc("/v1/scores/", h.scores)
	h.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	h.mux.HandleFunc("/metrics", h.metrics)
	return h
}

// chatPath maps an endpoint base path to its chat-completions path. Legacy
// upstreams keep the historical /v1/chat/completions append.
func chatPath(base string, legacy bool) string {
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	if legacy {
		return strings.TrimSuffix(base, "/") + "/v1/chat/completions"
	}
	return strings.TrimSuffix(base, "/") + "/chat/completions"
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// authOK enforces the optional client gate: a key held in the environment
// that every /v1 request must present. Empty key disables enforcement.
func (h *Handler) authOK(r *http.Request) bool {
	want := h.cfg.Auth.ClientKey()
	if want == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// chat buffers the (small) request body to extract routing and scoring
// inputs, resolves the provider (gateway), stamps a request id, then lets
// the matching reverse proxy take over.
func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	if !h.authOK(r) {
		http.Error(w, `{"error":{"message":"invalid or missing client key","type":"jev_proxy_auth_error"}}`, http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody))
	r.Body.Close()
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var creq struct {
		Model    string               `json:"model"`
		Stream   bool                 `json:"stream"`
		Messages []scorer.ChatMessage `json:"messages"`
	}
	json.Unmarshal(body, &creq) // unparseable bodies pass through in legacy mode

	info := &reqInfo{
		RequestID: newUUID(),
		Model:     creq.Model,
		Messages:  creq.Messages,
		Stream:    creq.Stream,
		Score:     h.sample(),
	}
	h.scorer.M.RequestsTotal.Add(1)

	if h.cfg.IsGateway() {
		dec, err := h.res.Resolve(creq.Model)
		if err != nil {
			h.scorer.M.RouteErrorTotal.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":{"message":%q,"type":"jev_proxy_route_error"}}`, err.Error())
			return
		}
		info.Provider = dec.Provider
		if dec.Renamed {
			if rb, err := rewriteModel(body, dec.Model); err == nil {
				body = rb
				r.ContentLength = int64(len(body)) // rewritten bytes: fix framing
			} // unparseable body + rename: forward as-is; provider will reject
		}
		h.scorer.M.AddProvider(dec.Provider)
	}
	rp := h.proxies[info.Provider]
	if rp == nil { // legacy: single synthesized provider
		rp = h.proxies["upstream"]
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.Header.Del("X-Jev-Request-Id") // never trust an inbound stamp
	h.rpServe(rp, w, r, info)
}

func (h *Handler) rpServe(rp *httputil.ReverseProxy, w http.ResponseWriter, r *http.Request, info *reqInfo) {
	rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, info)))
}

// rewriteModel replaces the "model" field of a buffered request body.
func rewriteModel(body []byte, model string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	return json.Marshal(m)
}

func (h *Handler) sample() bool {
	rate := h.cfg.Scoring.SampleRate
	return rate >= 1 || (rate > 0 && randFloat() < rate)
}

// models serves GET /v1/models: the merged, provider-namespaced catalog so
// agents can pick "nous/x" vs "openrouter/x" without ambiguity.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	if !h.authOK(r) {
		http.Error(w, `{"error":{"message":"invalid or missing client key"}}`, http.StatusUnauthorized)
		return
	}
	if !h.cfg.IsGateway() {
		http.Error(w, "models listing needs gateway mode (providers)", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	entries, partial := h.catalog.Models(ctx)
	out := struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}{Object: "list"}
	for _, e := range entries {
		out.Data = append(out.Data, map[string]any{
			"id": e.ID, "object": "model", "jev_provider": e.Provider,
		})
	}
	if len(entries) == 0 && len(partial) > 0 {
		http.Error(w, "no provider could list models: "+strings.Join(partial, ", "), http.StatusBadGateway)
		return
	}
	if len(partial) > 0 {
		w.Header().Set("X-Jev-Models-Partial", strings.Join(partial, ","))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// modifyResponse attaches the request id to every response, then either
// tracks the stream for async scoring or scores the buffered body synchronously.
func (h *Handler) modifyResponse(resp *http.Response) error {
	info, _ := resp.Request.Context().Value(ctxKey{}).(*reqInfo)
	if info == nil {
		return nil // request never went through chat(); pass through
	}
	resp.Header.Set("X-Jev-Request-Id", info.RequestID)
	if info.Provider != "" {
		resp.Header.Set("X-Jev-Provider", info.Provider)
	}
	if resp.StatusCode != http.StatusOK {
		return nil // upstream errors are not scoring material
	}
	if info.Stream || strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body = newStreamTracker(resp.Body, info, h)
		return nil
	}
	if !info.Score {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBody))
	resp.Body.Close()
	if err != nil {
		return nil // don't break the response over telemetry
	}
	cc := parseChatCompletion(body)
	if cc.Content == "" {
		h.scorer.M.NoContentTotal.Add(1)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	sctx, cancel := context.WithTimeout(context.Background(), h.cfg.Jev.Timeout)
	ev := h.scorer.ScoreNow(sctx, h.job(info, cc, body))
	cancel()
	if ev.Status == "ok" {
		resp.Header.Set("X-Jev-Weighted", fmt.Sprintf("%.3f", *ev.Weighted))
		resp.Header.Set("X-Jev-Safety", fmt.Sprintf("%.3f", *ev.SafetyProb))
		resp.Header.Set("X-Jev-Confidence", fmt.Sprintf("%.3f", *ev.Confidence))
		if ev.FlagReview {
			resp.Header.Set("X-Jev-Flag", "review")
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}

func (h *Handler) job(info *reqInfo, cc chatCompletion, _ []byte) scorer.Job {
	return scorer.Job{
		ResponseID:    info.RequestID,
		Provider:      info.Provider,
		UpstreamID:    cc.ID,
		UpstreamModel: firstNonEmpty(cc.Model, info.Model),
		Messages:      info.Messages,
		Reply:         cc.Content,
	}
}

// scores serves GET /v1/scores/{response_id}.
func (h *Handler) scores(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/scores/")
	if id == "" {
		http.Error(w, "missing response id", http.StatusBadRequest)
		return
	}
	ev, ok := h.store.Get(id)
	if !ok {
		http.Error(w, "no score event for "+id, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ev)
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
