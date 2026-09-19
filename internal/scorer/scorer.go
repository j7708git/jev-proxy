// Package scorer owns the async scoring pipeline: a bounded queue feeding a
// small worker pool that assembles judge state, calls Jev, applies threshold
// flags, and appends events to the store. It never blocks the proxy's main
// path — a full queue means the score is dropped, not the reply.
package scorer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"jev-proxy/internal/config"
	"jev-proxy/internal/jev"
	"jev-proxy/internal/rubric"
	"jev-proxy/internal/store"
)

// ChatMessage is one entry of the upstream request's messages array. Content
// stays raw: OpenAI allows a plain string or an array of typed parts.
type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Text flattens the content to plain text (string form, or the text fields
// of a part array). Tool calls carry no content and yield "".
func (m ChatMessage) Text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		out := ""
		for _, p := range parts {
			out += p.Text
		}
		return out
	}
	return ""
}

// Job is one unit of scoring work.
type Job struct {
	ResponseID    string // proxy-generated, the X-Jev-Request-Id key
	Provider      string // gateway provider routed to ("" in legacy mode)
	UpstreamID    string
	UpstreamModel string
	Messages      []ChatMessage
	Reply         string
}

// Metrics are the cumulative counters exposed on /metrics. All fields are
// concurrency-safe: workers and the proxy goroutines increment concurrently.
type Metrics struct {
	RequestsTotal      atomic.Int64
	RouteErrorTotal    atomic.Int64 // gateway: no provider matched the model
	ScoredTotal        atomic.Int64
	SkippedTotal       atomic.Int64
	ErrorTotal         atomic.Int64
	NoContentTotal     atomic.Int64 // tool-call-only turns: not scoring material
	FlagReviewTotal    atomic.Int64
	SuspectL0Total     atomic.Int64
	SafetyFlagTotal    atomic.Int64
	LowConfidenceTotal atomic.Int64
	QueueDepth         atomic.Int64
	// ScoreLevelTotals buckets ok events by round(weighted): 0..3.
	ScoreLevelTotals [4]atomic.Int64

	mu         sync.Mutex
	latencies  []int64          // jev call latency, ms; rolling window
	byProvider map[string]int64 // gateway: requests routed per provider
}

// AddProvider counts one routed request; legacy mode passes "".
func (m *Metrics) AddProvider(name string) {
	if name == "" {
		return
	}
	m.mu.Lock()
	if m.byProvider == nil {
		m.byProvider = map[string]int64{}
	}
	m.byProvider[name]++
	m.mu.Unlock()
}

// ProvidersSnapshot copies the per-provider request counts.
func (m *Metrics) ProvidersSnapshot() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int64, len(m.byProvider))
	for k, v := range m.byProvider {
		out[k] = v
	}
	return out
}

func (m *Metrics) addLatency(ms int64) {
	m.mu.Lock()
	m.latencies = append(m.latencies, ms)
	if len(m.latencies) > 1000 {
		m.latencies = m.latencies[len(m.latencies)-1000:]
	}
	m.mu.Unlock()
}

// LatencyMS returns p50/p95 of the rolling latency window.
func (m *Metrics) LatencyMS() (p50, p95 int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.latencies) == 0 {
		return 0, 0
	}
	s := append([]int64(nil), m.latencies...)
	sortSlice(s)
	pick := func(q float64) int64 {
		i := int(q * float64(len(s)-1))
		return s[i]
	}
	return pick(0.50), pick(0.95)
}

func sortSlice(s []int64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

type Scorer struct {
	client *jev.Client
	store  *store.Store
	cfg    *config.Config
	M      *Metrics
	queue  chan Job
	wg     sync.WaitGroup
}

// New starts the worker pool. Close stops it.
func New(client *jev.Client, st *store.Store, cfg *config.Config) *Scorer {
	s := &Scorer{
		client: client,
		store:  st,
		cfg:    cfg,
		M:      &Metrics{},
		queue:  make(chan Job, cfg.Queue.Cap),
	}
	for i := 0; i < cfg.Queue.Workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

// Submit enqueues without blocking. A full queue drops the score on the
// floor and records a skipped event — telemetry, not transactions.
func (s *Scorer) Submit(j Job) {
	select {
	case s.queue <- j:
		s.M.QueueDepth.Add(1)
	default:
		s.RecordSkipped(Job{ResponseID: j.ResponseID, Provider: j.Provider, UpstreamID: j.UpstreamID, UpstreamModel: j.UpstreamModel}, "queue_full")
	}
}

// RecordSkipped logs a scoring miss for a request that reached the pipeline
// but produced no judgeable reply.
func (s *Scorer) RecordSkipped(j Job, reason string) {
	s.M.SkippedTotal.Add(1)
	s.record(&store.Event{
		TS: time.Now(), ResponseID: j.ResponseID, Provider: j.Provider, UpstreamID: j.UpstreamID,
		UpstreamModel: j.UpstreamModel, RubricVersion: rubric.Version,
		Status: "skipped", Reason: reason,
	})
}

// ScoreNow runs the pipeline synchronously (non-streaming responses attach
// the result headers before the body is released).
func (s *Scorer) ScoreNow(ctx context.Context, j Job) *store.Event {
	return s.run(ctx, j)
}

func (s *Scorer) worker() {
	defer s.wg.Done()
	for j := range s.queue {
		s.M.QueueDepth.Add(-1)
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Jev.Timeout)
		s.run(ctx, j)
		cancel()
	}
}

func (s *Scorer) run(ctx context.Context, j Job) *store.Event {
	start := time.Now()
	st := BuildState(j.Messages, j.Reply, &s.cfg.Scoring)
	res, err := s.client.Score(ctx, st)
	lat := time.Since(start).Milliseconds()
	s.M.addLatency(lat)

	ev := &store.Event{
		TS: time.Now(), ResponseID: j.ResponseID, Provider: j.Provider, UpstreamID: j.UpstreamID,
		UpstreamModel: j.UpstreamModel, RubricVersion: rubric.Version,
		LatencyMS: lat, ReplySHA256: sha256Hex(j.Reply), Reply: j.Reply,
	}
	if err != nil {
		ev.Status = "error"
		ev.Error = err.Error()
		s.M.ErrorTotal.Add(1)
		s.record(ev)
		return ev
	}

	ev.Status = "ok"
	ev.JevModel = res.JevModel
	ev.Weighted = &res.Weighted
	ev.APIScore = &res.APIScore
	lp := res.LevelProbs
	ev.LevelProbs = &lp
	ev.SafetyProb = &res.SafetyProb
	ev.Confidence = &res.Confidence
	lowConf := res.Confidence < s.cfg.Scoring.MinConfidence
	ev.LowConfidence = lowConf
	// A below-threshold-confidence judgment is recorded but never raises
	// flags: it is a drift signal, not decision input.
	ev.FlagReview = res.Weighted < s.cfg.Scoring.FlagReviewBelow && !lowConf
	ev.SuspectL0 = res.Weighted < s.cfg.Scoring.SuspectL0Below && !lowConf
	// Safety is a gate, not a degree: it fires regardless of confidence.
	ev.SafetyFlag = res.SafetyProb > s.cfg.Scoring.SafetyFlagAbove

	s.M.ScoredTotal.Add(1)
	lvl := int(math.Round(res.Weighted))
	if lvl < 0 {
		lvl = 0
	} else if lvl > 3 {
		lvl = 3
	}
	s.M.ScoreLevelTotals[lvl].Add(1)
	if ev.FlagReview {
		s.M.FlagReviewTotal.Add(1)
	}
	if ev.SuspectL0 {
		s.M.SuspectL0Total.Add(1)
	}
	if ev.SafetyFlag {
		s.M.SafetyFlagTotal.Add(1)
	}
	if lowConf {
		s.M.LowConfidenceTotal.Add(1)
	}
	s.record(ev)
	return ev
}

func (s *Scorer) record(ev *store.Event) {
	if err := s.store.Append(ev); err != nil {
		// Losing a telemetry line must never take the proxy down.
		s.M.ErrorTotal.Add(1)
	}
}

// Close drains nothing by design (queue contents are droppable telemetry)
// but waits for in-flight workers to finish their current job.
func (s *Scorer) Close() {
	close(s.queue)
	s.wg.Wait()
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
