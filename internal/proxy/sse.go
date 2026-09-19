package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	mrand "math/rand/v2"
	"strings"

	"jev-proxy/internal/scorer"
)

// chatCompletion is the fields the scoring side-channel needs from either a
// buffered chat completion or an assembled SSE stream.
type chatCompletion struct {
	ID      string
	Model   string
	Content string
}

// randFloat backs sample-rate decisions. math/rand/v2 needs no seeding.
func randFloat() float64 { return mrand.Float64() }

// parseChatCompletion extracts id/model/content from a buffered non-stream
// response body. Unparseable or empty bodies yield zero fields.
func parseChatCompletion(body []byte) chatCompletion {
	var raw struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &raw) == nil && len(raw.Choices) > 0 {
		return chatCompletion{ID: raw.ID, Model: raw.Model, Content: raw.Choices[0].Message.Content}
	}
	return chatCompletion{}
}

// parseSSE assembles an OpenAI-compatible chat stream: content deltas are
// concatenated, id/model captured when first seen, and done reports whether
// the terminal [DONE] sentinel arrived.
func parseSSE(body []byte) (cc chatCompletion, done bool) {
	var raw struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	for _, line := range strings.Split(string(body), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			done = true
			continue
		}
		if json.Unmarshal([]byte(payload), &raw) != nil {
			continue
		}
		if cc.ID == "" {
			cc.ID = raw.ID
		}
		if cc.Model == "" {
			cc.Model = raw.Model
		}
		if len(raw.Choices) > 0 {
			cc.Content += raw.Choices[0].Delta.Content
		}
	}
	return cc, done
}

// streamTracker passes response bytes through untouched while keeping a copy
// for the judge. On close it either submits async scoring or records why the
// stream was not scoring material. Its errors never reach the client.
type streamTracker struct {
	rc     io.ReadCloser
	buf    bytes.Buffer
	sawEOF bool
	info   *reqInfo
	h      *Handler
}

func newStreamTracker(rc io.ReadCloser, info *reqInfo, h *Handler) *streamTracker {
	return &streamTracker{rc: rc, info: info, h: h}
}

func (t *streamTracker) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.buf.Write(p[:n])
	}
	if err == io.EOF {
		t.sawEOF = true
	}
	return n, err
}

func (t *streamTracker) Close() error {
	err := t.rc.Close()
	t.finish()
	return err
}

func (t *streamTracker) finish() {
	defer func() {
		if r := recover(); r != nil {
			// Scoring must never take the proxy down, even on a bug.
			t.h.scorer.M.ErrorTotal.Add(1)
		}
	}()

	base := scorer.Job{ResponseID: t.info.RequestID, Provider: t.info.Provider, UpstreamModel: t.info.Model}
	if !t.sawEOF {
		t.h.scorer.RecordSkipped(base, "stream_interrupted")
		return
	}
	cc, done := parseSSE(t.buf.Bytes())
	if !done {
		t.h.scorer.RecordSkipped(base, "stream_no_done")
		return
	}
	if cc.Content == "" {
		t.h.scorer.M.NoContentTotal.Add(1)
		return
	}
	if !t.info.Score {
		// Sampled out: the reply still gets its X-Jev-Request-Id, but no
		// scoring job and no event — same as the non-stream path.
		return
	}
	base.UpstreamID = cc.ID
	base.UpstreamModel = firstNonEmpty(cc.Model, t.info.Model)
	base.Messages = t.info.Messages
	base.Reply = cc.Content
	t.h.scorer.Submit(base)
}
