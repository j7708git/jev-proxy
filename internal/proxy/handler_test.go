package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jev-proxy/internal/config"
	"jev-proxy/internal/jev"
	"jev-proxy/internal/scorer"
	"jev-proxy/internal/store"
)

// jevResponse is the fixed evaluate response the fake Jev endpoint returns,
// mirroring the live API schema (probabilities keyed by level index).
const jevResponse = `{"model":"typesafe/jev-test","answers":{
	"quality":{"type":"score","score":1.7,"probabilities":{"0":0.02,"1":0.08,"2":0.30,"3":0.60},"confidence":0.8},
	"safety":{"type":"noul","noul":0.01}},
	"usage":{"input_tokens":100,"output_tokens":10,"cost":0.00001}}`

// newTestStack wires a proxy over a fake upstream and a fake Jev endpoint.
// It returns the proxy's own test server plus the store and metrics handles.
// Optional tweaks mutate the config before the stack is built.
func newTestStack(t *testing.T, upstreamHandler http.HandlerFunc, tweaks ...func(*config.Config)) (*httptest.Server, *store.Store, *scorer.Metrics) {
	t.Helper()
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)
	jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(jevResponse))
	}))
	t.Cleanup(jevSrv.Close)

	logPath := filepath.Join(t.TempDir(), "scores.jsonl")
	st, err := store.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		Upstream: upstream.URL,
		Jev:      config.Jev{Endpoint: jevSrv.URL, Timeout: 2 * time.Second},
		Scoring: config.Scoring{
			SampleRate: 1, ContextTurns: 10, SystemMaxChars: 1000,
			MessageMaxChars: 2000, ReplyMaxChars: 8000,
			FlagReviewBelow: 1.0, SuspectL0Below: 0.5, SafetyFlagAbove: 0.5,
			MinConfidence: 0.3,
		},
		Queue: config.Queue{Cap: 100, Workers: 1},
		Log:   config.Log{Path: logPath},
	}
	for _, tw := range tweaks {
		tw(cfg)
	}
	sc := scorer.New(&jev.Client{URL: jevSrv.URL, HTTP: jevSrv.Client()}, st, cfg)
	ts := httptest.NewServer(New(cfg, sc, st))
	t.Cleanup(ts.Close)
	return ts, st, sc.M
}

func waitForEvent(t *testing.T, st *store.Store, id string) *store.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ev, ok := st.Get(id); ok {
			return ev
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no score event for %s within deadline", id)
	return nil
}

const sseBody = "data: {\"id\":\"chatcmpl-stream-1\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-stream-1\",\"model\":\"gpt-test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好，這是回答。\"}}]}\n\n" +
	"data: [DONE]\n\n"

func TestStreamPassthroughAndAsyncScore(t *testing.T) {
	ts, st, m := newTestStack(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseBody)
	})

	body := `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"問題"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != sseBody {
		t.Fatalf("stream body altered:\n got  %q\n want %q", got, sseBody)
	}
	rid := resp.Header.Get("X-Jev-Request-Id")
	if rid == "" {
		t.Fatal("missing X-Jev-Request-Id")
	}
	if resp.Header.Get("X-Jev-Weighted") != "" {
		t.Fatal("stream responses must not carry sync score headers")
	}

	ev := waitForEvent(t, st, rid)
	if ev.Status != "ok" || ev.Weighted == nil || math.Abs(*ev.Weighted-2.48) > 1e-9 {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Reply != "你好，這是回答。" {
		t.Fatalf("event reply = %q", ev.Reply)
	}
	if ev.UpstreamID != "chatcmpl-stream-1" || ev.UpstreamModel != "gpt-test" {
		t.Fatalf("upstream id/model = %s/%s", ev.UpstreamID, ev.UpstreamModel)
	}
	if ev.LowConfidence || ev.FlagReview || ev.SafetyFlag {
		t.Fatalf("unexpected flags: %+v", ev)
	}
	if n := m.ScoredTotal.Load(); n != 1 {
		t.Fatalf("scored_total = %d", n)
	}
}

func TestNonStreamSyncScoreHeaders(t *testing.T) {
	var capturedAuth string
	ts, st, _ := newTestStack(t, func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-9","model":"gpt-test","choices":[{"message":{"role":"assistant","content":"完整回覆內容"}}]}`)
	})

	body := `{"model":"gpt-test","messages":[{"role":"user","content":"問題"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var back map[string]any
	json.NewDecoder(resp.Body).Decode(&back)
	if back["id"] != "chatcmpl-9" {
		t.Fatalf("response body altered: %v", back)
	}
	// The proxy must not leak its own Jev credentials upstream.
	if capturedAuth != "" {
		t.Fatalf("proxy injected Authorization header upstream: %q", capturedAuth)
	}
	if w := resp.Header.Get("X-Jev-Weighted"); w != "2.480" {
		t.Fatalf("X-Jev-Weighted = %q, want 2.480", w)
	}
	if s := resp.Header.Get("X-Jev-Safety"); s != "0.010" {
		t.Fatalf("X-Jev-Safety = %q", s)
	}
	rid := resp.Header.Get("X-Jev-Request-Id")
	if ev, ok := st.Get(rid); !ok || ev.Status != "ok" {
		t.Fatalf("sync score event missing: %+v ok=%v", ev, ok)
	}
}

func TestStreamSampleRateZeroNoScore(t *testing.T) {
	// sample_rate 0 must drop scoring jobs on the stream path too — the
	// reply still passes through byte-for-byte with its request id stamped.
	ts, st, m := newTestStack(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseBody)
	}, func(c *config.Config) { c.Scoring.SampleRate = 0 })

	body := `{"model":"gpt-test","stream":true,"messages":[{"role":"user","content":"問題"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != sseBody {
		t.Fatalf("sampled-out stream body altered:\n got  %q\n want %q", got, sseBody)
	}
	rid := resp.Header.Get("X-Jev-Request-Id")
	if rid == "" {
		t.Fatal("sampled-out stream lost X-Jev-Request-Id")
	}
	time.Sleep(200 * time.Millisecond)
	if _, ok := st.Get(rid); ok {
		t.Fatal("sampled-out stream must not produce a score event")
	}
	if n := m.ScoredTotal.Load(); n != 0 {
		t.Fatalf("scored_total = %d, want 0", n)
	}
}

func TestNonStreamSampleRateZeroNoHeaders(t *testing.T) {
	ts, st, _ := newTestStack(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-z","model":"gpt-test","choices":[{"message":{"role":"assistant","content":"回覆"}}]}`)
	}, func(c *config.Config) { c.Scoring.SampleRate = 0 })

	body := `{"model":"gpt-test","messages":[{"role":"user","content":"問題"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if w := resp.Header.Get("X-Jev-Weighted"); w != "" {
		t.Fatalf("sampled-out response carries X-Jev-Weighted=%q", w)
	}
	if rid := resp.Header.Get("X-Jev-Request-Id"); rid != "" {
		if _, ok := st.Get(rid); ok {
			t.Fatal("sampled-out response must not produce a score event")
		}
	}
}

func TestToolCallOnlyReplyNotScored(t *testing.T) {
	ts, st, m := newTestStack(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-tool","model":"gpt-test","choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"t1","function":{"name":"f","arguments":"{}"}}]}}]}`)
	})
	body := `{"model":"gpt-test","messages":[{"role":"user","content":"查一下"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	rid := resp.Header.Get("X-Jev-Request-Id")
	time.Sleep(200 * time.Millisecond)
	if _, ok := st.Get(rid); ok {
		t.Fatal("tool-call-only turn must not produce a score event")
	}
	if n := m.NoContentTotal.Load(); n != 1 {
		t.Fatalf("no_content_total = %d, want 1", n)
	}
}

func TestStreamWithoutDoneSkipped(t *testing.T) {
	ts, st, _ := newTestStack(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"half\"}}]}\n\n")
		// handler returns: clean EOF, but no [DONE] sentinel
	})
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"q"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	rid := resp.Header.Get("X-Jev-Request-Id")
	ev := waitForEvent(t, st, rid)
	if ev.Status != "skipped" || ev.Reason != "stream_no_done" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestScoreLookupAndHealth(t *testing.T) {
	ts, st, _ := newTestStack(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c2","model":"m","choices":[{"message":{"content":"r"}}]}`)
	})
	body := `{"model":"m","messages":[{"role":"user","content":"q"}]}`
	resp, _ := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	rid := resp.Header.Get("X-Jev-Request-Id")
	resp.Body.Close()
	waitForEvent(t, st, rid) // ensure scoring settled before the lookups below

	r2, err := http.Get(ts.URL + "/v1/scores/" + rid)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var ev store.Event
	json.NewDecoder(r2.Body).Decode(&ev)
	if ev.ResponseID != rid || ev.Status != "ok" {
		t.Fatalf("lookup = %+v", ev)
	}

	r3, _ := http.Get(ts.URL + "/healthz")
	if r3.StatusCode != 200 {
		t.Fatalf("healthz = %d", r3.StatusCode)
	}
	r3.Body.Close()

	r4, err := http.Get(ts.URL + "/v1/scores/does-not-exist")
	if err != nil || r4.StatusCode != 404 {
		t.Fatalf("missing score lookup = %v/%v", err, r4)
	}
	if r4 != nil {
		r4.Body.Close()
	}

	r5, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer r5.Body.Close()
	mb, _ := io.ReadAll(r5.Body)
	if !bytes.Contains(mb, []byte("scored_total 1")) || !bytes.Contains(mb, []byte("score_level_l2_total 1")) {
		t.Fatalf("metrics missing counters:\n%s", mb)
	}
}

func TestUpstreamErrorPassthrough(t *testing.T) {
	ts, st, _ := newTestStack(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	})
	body := `{"model":"m","messages":[{"role":"user","content":"q"}]}`
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	if rid := resp.Header.Get("X-Jev-Request-Id"); rid != "" {
		if _, ok := st.Get(rid); ok {
			t.Fatal("upstream errors must not be scored")
		}
	}
}
