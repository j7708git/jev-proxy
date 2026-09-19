// Package proxy implements the transparent passthrough handler with an
// async (streaming) or sync (non-streaming) Jev scoring side-channel.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"jev-proxy/internal/config"
	"jev-proxy/internal/scorer"
	"jev-proxy/internal/store"
)

const maxRequestBody = 8 << 20

type ctxKey struct{}

// reqInfo is what the scoring side-channel needs from the request phase.
type reqInfo struct {
	RequestID string
	Model     string
	Messages  []scorer.ChatMessage
	Stream    bool
	Score     bool
}

// Handler serves the proxy and its read-only side endpoints.
type Handler struct {
	cfg    *config.Config
	target *url.URL
	rp     *httputil.ReverseProxy
	scorer *scorer.Scorer
	store  *store.Store
	mux    *http.ServeMux
}

// New wires the reverse proxy (upstream target, scoring hooks) and the
// side endpoints onto one mux.
func New(cfg *config.Config, sc *scorer.Scorer, st *store.Store) *Handler {
	h := &Handler{cfg: cfg, scorer: sc, store: st}

	target, err := url.Parse(cfg.Upstream)
	if err != nil {
		panic(fmt.Sprintf("proxy: invalid upstream %q: %v", cfg.Upstream, err))
	}
	h.target = target
	h.rp = &httputil.ReverseProxy{
		// Route manually instead of SetURL: SetURL joins the target path
		// with the inbound path, which would double the /v1/chat/completions
		// suffix. An upstream path that already ends in /chat/completions is
		// used verbatim; otherwise /v1/chat/completions is appended.
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out.URL
			out.Scheme = target.Scheme
			out.Host = target.Host
			if strings.HasSuffix(target.Path, "/chat/completions") {
				out.Path = target.Path
			} else {
				out.Path = strings.TrimSuffix(target.Path, "/") + "/v1/chat/completions"
			}
			out.RawPath = ""
			pr.Out.Host = target.Host
		},
		ModifyResponse: h.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy: upstream error: %v", err)
			w.WriteHeader(http.StatusBadGateway)
		},
	}

	h.mux = http.NewServeMux()
	h.mux.HandleFunc("/v1/chat/completions", h.chat)
	h.mux.HandleFunc("/v1/scores/", h.scores)
	h.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	h.mux.HandleFunc("/metrics", h.metrics)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// chat buffers the (small) request body to extract scoring inputs, stamps a
// request id, then lets the reverse proxy take over.
func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
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
	json.Unmarshal(body, &creq) // unparseable bodies pass through untouched

	info := &reqInfo{
		RequestID: newUUID(),
		Model:     creq.Model,
		Messages:  creq.Messages,
		Stream:    creq.Stream,
		Score:     h.sample(),
	}
	h.scorer.M.RequestsTotal.Add(1)

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.Header.Del("X-Jev-Request-Id") // never trust an inbound stamp
	h.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, info)))
}

func (h *Handler) sample() bool {
	rate := h.cfg.Scoring.SampleRate
	return rate >= 1 || (rate > 0 && randFloat() < rate)
}

// modifyResponse attaches the request id to every response, then either
// tracks the stream for async scoring or scores the buffered body synchronously.
func (h *Handler) modifyResponse(resp *http.Response) error {
	info, _ := resp.Request.Context().Value(ctxKey{}).(*reqInfo)
	if info == nil {
		return nil // request never went through chat(); pass through
	}
	resp.Header.Set("X-Jev-Request-Id", info.RequestID)
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
	ev := h.scorer.ScoreNow(sctx, scorer.Job{
		ResponseID:    info.RequestID,
		UpstreamID:    cc.ID,
		UpstreamModel: firstNonEmpty(cc.Model, info.Model),
		Messages:      info.Messages,
		Reply:         cc.Content,
	})
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
