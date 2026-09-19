package jev

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestScorePayloadAndParsing(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"model":"typesafe/jev-test","answers":{
			"quality":{"type":"score","score":1.7,
				"probabilities":{"0":0.02,"1":0.08,"2":0.30,"3":0.60},
				"confidence":0.8},
			"safety":{"type":"noul","noul":0.01}},
			"usage":{"input_tokens":100,"output_tokens":10,"cost":0.00001}}`))
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	res, err := c.Score(context.Background(), State{
		AssistantReply: "回覆", Conversation: "user: 問題",
	})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}

	// Payload carries both questions with the right types and rubric criteria.
	q, _ := gotBody["questions"].(map[string]any)
	if q == nil {
		t.Fatal("request has no questions")
	}
	quality, _ := q["quality"].(map[string]any)
	safety, _ := q["safety"].(map[string]any)
	if quality["type"] != "score" || safety["type"] != "noul" {
		t.Fatalf("question types = %v / %v", quality["type"], safety["type"])
	}
	if criteria, _ := quality["criteria"].([]any); len(criteria) != 4 {
		t.Fatalf("quality criteria = %d items, want 4", len(criteria))
	}
	state, _ := gotBody["state"].(map[string]any)
	if state["assistant_reply"] != "回覆" {
		t.Fatalf("state reply = %v", state["assistant_reply"])
	}

	// Weighted is computed here from level probabilities, not taken from the API.
	if math.Abs(res.Weighted-2.48) > 1e-9 {
		t.Fatalf("weighted = %v, want 2.48", res.Weighted)
	}
	if res.APIScore != 1.7 || res.SafetyProb != 0.01 || res.Confidence != 0.8 {
		t.Fatalf("api_score/safety/confidence = %v/%v/%v", res.APIScore, res.SafetyProb, res.Confidence)
	}
	if res.LevelProbs[3] != 0.60 || res.LevelProbs[0] != 0.02 {
		t.Fatalf("level probs = %v", res.LevelProbs)
	}
	if res.JevModel != "typesafe/jev-test" || res.InputTokens != 100 {
		t.Fatalf("model/tokens = %s/%d", res.JevModel, res.InputTokens)
	}
}

func TestScoreServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	if _, err := c.Score(context.Background(), State{AssistantReply: "x"}); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestScoreMissingLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"model":"m","answers":{"quality":{"score":1,"probabilities":{"0":1}},"safety":{"noul":0}}}`))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	if _, err := c.Score(context.Background(), State{AssistantReply: "x"}); err == nil {
		t.Fatal("expected error when a level probability is missing")
	}
}
