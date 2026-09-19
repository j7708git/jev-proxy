package jev

import (
	"context"
	"encoding/json"
	"fmt"

	"jev-proxy/internal/rubric"
)

// State is the per-reply judging input, assembled by the scorer from the chat
// request and the buffered response text.
type State struct {
	SystemPromptExcerpt string `json:"system_prompt_excerpt,omitempty"`
	Conversation        string `json:"conversation,omitempty"`
	UserLastMessage     string `json:"user_last_message,omitempty"`
	AssistantReply      string `json:"assistant_reply"`
}

// Result is the parsed, proxy-normalized outcome of one evaluate call.
type Result struct {
	JevModel string
	// Weighted is computed here from the four level probabilities so the
	// threshold semantics stay bound to the rubric version, not the API's
	// own aggregation. APIScore is kept alongside for cross-checking.
	Weighted    float64
	APIScore    float64
	LevelProbs  [4]float64
	SafetyProb  float64
	Confidence  float64
	InputTokens int
	CostUSD     float64
}

type question struct {
	Type         string   `json:"type"`
	Instructions string   `json:"instructions"`
	Criteria     []string `json:"criteria,omitempty"`
}

type evaluateRequest struct {
	State     State               `json:"state"`
	Questions map[string]question `json:"questions"`
	Model     string              `json:"model,omitempty"`
}

type evaluateResponse struct {
	Model   string `json:"model"`
	Answers struct {
		Quality struct {
			Score         float64            `json:"score"`
			Probabilities map[string]float64 `json:"probabilities"`
			Confidence    float64            `json:"confidence"`
		} `json:"quality"`
		Safety struct {
			Noul       float64 `json:"noul"`
			Confidence float64 `json:"confidence"`
		} `json:"safety"`
	} `json:"answers"`
	Usage struct {
		InputTokens  int     `json:"input_tokens"`
		OutputTokens int     `json:"output_tokens"`
		Cost         float64 `json:"cost"`
	} `json:"usage"`
}

// Score sends the rubric's quality (score) and safety (noul) questions over
// the given state in one batched call.
func (c *Client) Score(ctx context.Context, st State) (*Result, error) {
	req := evaluateRequest{
		State: st,
		Questions: map[string]question{
			"quality": {Type: "score", Instructions: rubric.Instructions, Criteria: rubric.Criteria},
			"safety":  {Type: "noul", Instructions: rubric.SafetyInstructions},
		},
		Model: c.Model,
	}
	b, err := c.Evaluate(ctx, req)
	if err != nil {
		return nil, err
	}
	var er evaluateResponse
	if err := json.Unmarshal(b, &er); err != nil {
		return nil, fmt.Errorf("jev: decode response: %w", err)
	}
	res := &Result{
		JevModel:    er.Model,
		APIScore:    er.Answers.Quality.Score,
		SafetyProb:  er.Answers.Safety.Noul,
		Confidence:  er.Answers.Quality.Confidence,
		InputTokens: er.Usage.InputTokens,
		CostUSD:     er.Usage.Cost,
	}
	// Level probabilities come back keyed by criteria index ("0".."3").
	for i := 0; i < 4; i++ {
		p, ok := er.Answers.Quality.Probabilities[fmt.Sprintf("%d", i)]
		if !ok {
			return nil, fmt.Errorf("jev: missing probability for level %d", i)
		}
		res.LevelProbs[i] = p
		res.Weighted += p * float64(i)
	}
	return res, nil
}
