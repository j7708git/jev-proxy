package proxy

import (
	"fmt"
	"net/http"
)

// metrics renders cumulative counters and the rolling Jev latency window as
// plain text — enough for a first look at score distribution and failure
// rates without a monitoring stack.
func (h *Handler) metrics(w http.ResponseWriter, _ *http.Request) {
	m := h.scorer.M
	p50, p95 := m.LatencyMS()
	lines := []string{
		fmt.Sprintf("requests_total %d", m.RequestsTotal.Load()),
		fmt.Sprintf("scored_total %d", m.ScoredTotal.Load()),
		fmt.Sprintf("skipped_total %d", m.SkippedTotal.Load()),
		fmt.Sprintf("error_total %d", m.ErrorTotal.Load()),
		fmt.Sprintf("no_content_total %d", m.NoContentTotal.Load()),
		fmt.Sprintf("flag_review_total %d", m.FlagReviewTotal.Load()),
		fmt.Sprintf("suspect_l0_total %d", m.SuspectL0Total.Load()),
		fmt.Sprintf("safety_flag_total %d", m.SafetyFlagTotal.Load()),
		fmt.Sprintf("low_confidence_total %d", m.LowConfidenceTotal.Load()),
		fmt.Sprintf("queue_depth %d", m.QueueDepth.Load()),
		fmt.Sprintf("jev_latency_ms_p50 %d", p50),
		fmt.Sprintf("jev_latency_ms_p95 %d", p95),
	}
	for lvl, name := range [4]string{"l0", "l1", "l2", "l3"} {
		lines = append(lines, fmt.Sprintf("score_level_%s_total %d", name, m.ScoreLevelTotals[lvl].Load()))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}
