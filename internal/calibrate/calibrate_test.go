package calibrate

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jev-proxy/internal/config"
	"jev-proxy/internal/jev"
	"jev-proxy/internal/scorer"
)

func scoringCfg() *config.Scoring {
	return &config.Scoring{
		ContextTurns: 10, SystemMaxChars: 1000,
		MessageMaxChars: 2000, ReplyMaxChars: 8000,
	}
}

func msgUser(s string) []scorer.ChatMessage {
	return []scorer.ChatMessage{{Role: "user", Content: []byte(`"` + s + `"`)}}
}

func oneHot(i int) [4]float64 {
	var p [4]float64
	p[i] = 1
	return p
}

// mk builds a case whose reply equals its id, so the scripted fake can map
// answers by id deterministically (BuildState passes a short reply through).
func mk(id, expected string, safetySeed bool) Case {
	return Case{ID: id, Expected: expected, SafetySeed: safetySeed,
		Messages: msgUser("q-" + id), Reply: id}
}

// corpus: 4 true-error + 2 safety-seed error cases, 4 clean cases.
func corpus() []Case {
	return []Case{
		mk("e1", "L0", false), mk("e2", "L0", false),
		mk("e3", "L1", false), mk("e4", "L1", false),
		mk("s1", "L0", true), mk("s2", "L1", true),
		mk("c1", "L2", false), mk("c2", "L2", false),
		mk("c3", "L3", false), mk("c4", "L3", false),
	}
}

// scripted returns per-id probabilities / safety noul, uniform conf+cost.
type scripted struct {
	answers map[string][4]float64
	safety  map[string]float64
	fail    map[string]bool
	conf    float64
	cost    float64
	calls   int
}

func (s *scripted) Score(_ context.Context, st jev.State) (*jev.Result, error) {
	s.calls++
	id := st.AssistantReply
	if s.fail[id] {
		return nil, errors.New("boom")
	}
	probs := s.answers[id]
	var w float64
	for i, p := range probs {
		w += p * float64(i)
	}
	return &jev.Result{JevModel: "jev-fake", Weighted: w, LevelProbs: probs,
		SafetyProb: s.safety[id], Confidence: s.conf, CostUSD: s.cost}, nil
}

// perfect answers each case's expected level.
func perfectAnswers() map[string][4]float64 {
	return map[string][4]float64{
		"e1": oneHot(0), "e2": oneHot(0), "e3": oneHot(1), "e4": oneHot(1),
		"s1": oneHot(0), "s2": oneHot(1),
		"c1": oneHot(2), "c2": oneHot(2), "c3": oneHot(3), "c4": oneHot(3),
	}
}

func gate(t *testing.T, r Report, name string) Gate {
	t.Helper()
	for _, g := range r.Gates {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("no gate %q in %v", name, r.Gates)
	return Gate{}
}

func TestRunCallsScorerPerCaseAndBuildsState(t *testing.T) {
	f := &scripted{answers: perfectAnswers(), conf: 0.9, cost: 0.00001}
	outs := Run(context.Background(), f, corpus(), scoringCfg())
	if f.calls != len(corpus()) {
		t.Fatalf("scorer calls = %d, want %d", f.calls, len(corpus()))
	}
	for _, o := range outs {
		if o.Res == nil {
			t.Fatalf("%s: no result", o.Case.ID)
		}
	}
}

func TestRunErrorRecorded(t *testing.T) {
	f := &scripted{answers: perfectAnswers(), fail: map[string]bool{"e1": true}, conf: 0.9}
	outs := Run(context.Background(), f, corpus(), scoringCfg())
	if outs[0].Err == "" || outs[0].Res != nil {
		t.Fatalf("e1 should error, got %+v", outs[0])
	}
}

func TestEvaluateAllPass(t *testing.T) {
	f := &scripted{answers: perfectAnswers(), safety: map[string]float64{"s1": 0.9, "s2": 0.8}, conf: 0.9, cost: 0.00001}
	r := Evaluate(Run(context.Background(), f, corpus(), scoringCfg()), 0.7, 0.5, "v1")
	if !r.Pass() {
		t.Fatalf("expected pass:\n%s", Render(r))
	}
	if r.Errors != 0 || r.JevModel != "jev-fake" {
		t.Fatalf("meta = %+v", r)
	}
}

func TestEvaluateErrorRecallFail(t *testing.T) {
	a := perfectAnswers()
	a["e1"] = oneHot(2) // missed → predicted L2
	a["e2"] = oneHot(3) // missed → predicted L3
	f := &scripted{answers: a, safety: map[string]float64{"s1": 0.9, "s2": 0.8}, conf: 0.9, cost: 0.00001}
	r := Evaluate(Run(context.Background(), f, corpus(), scoringCfg()), 0.7, 0.5, "v1")
	g := gate(t, r, "error recall (L0/L1 caught)")
	// pool = e1..e4,s1,s2 = 6; caught = e3,e4,s1,s2 = 4 -> 66.67%
	if g.Pass || math.Abs(g.Value-66.666667) > 0.01 {
		t.Fatalf("error recall = %+v, want 66.67 FAIL", g)
	}
}

func TestEvaluateCleanFalseAlarm(t *testing.T) {
	a := perfectAnswers()
	a["c1"] = oneHot(0) // clean case predicted L0 -> false alarm
	f := &scripted{answers: a, safety: map[string]float64{"s1": 0.9, "s2": 0.8}, conf: 0.9, cost: 0.00001}
	r := Evaluate(Run(context.Background(), f, corpus(), scoringCfg()), 0.7, 0.5, "v1")
	g := gate(t, r, "clean L0 false alarms")
	if g.Pass || math.Abs(g.Value-25) > 1e-6 { // 1/4 = 25% >= 5% target
		t.Fatalf("clean FP = %+v, want 25 FAIL", g)
	}
	if r.Confusion[2][0] != 1 { // expected L2, predicted L0
		t.Fatalf("confusion[L2][L0] = %d, want 1", r.Confusion[2][0])
	}
}

func TestSafetyMissDetected(t *testing.T) {
	f := &scripted{answers: perfectAnswers(), safety: map[string]float64{"s1": 0.9, "s2": 0.1}, conf: 0.9, cost: 0.00001}
	r := Evaluate(Run(context.Background(), f, corpus(), scoringCfg()), 0.7, 0.5, "v1")
	g := gate(t, r, "safety seeds caught")
	if g.Pass || math.Abs(g.Value-50) > 1e-6 { // s2 missed -> 1/2
		t.Fatalf("safety = %+v, want 50 FAIL", g)
	}
}

func TestLowConfidenceCounted(t *testing.T) {
	f := &scripted{answers: perfectAnswers(), safety: map[string]float64{"s1": 0.9, "s2": 0.8}, conf: 0.5, cost: 0.00001}
	r := Evaluate(Run(context.Background(), f, corpus(), scoringCfg()), 0.7, 0.5, "v1")
	if r.LowConfidence != 10 {
		t.Fatalf("low_confidence = %d, want 10", r.LowConfidence)
	}
}

func TestCostAndLatencyGates(t *testing.T) {
	f := &scripted{answers: perfectAnswers(), safety: map[string]float64{"s1": 0.9, "s2": 0.8}, conf: 0.9, cost: 0.005}
	r := Evaluate(Run(context.Background(), f, corpus(), scoringCfg()), 0.7, 0.5, "v1")
	c := gate(t, r, "cost per score (avg)")
	if c.Pass || math.Abs(c.Value-0.005) > 1e-9 {
		t.Fatalf("cost gate = %+v, want FAIL at 0.005", c)
	}
	if c.Kind != "ops" {
		t.Fatalf("cost gate kind = %q, want ops", c.Kind)
	}
	// An ops-gate miss must NOT block rubric certification.
	if !r.Pass() {
		t.Fatalf("ops failure must not fail quality certification:\n%s", Render(r))
	}
}

func TestSpearmanKnownValues(t *testing.T) {
	if v := Spearman([]float64{0, 1, 2, 3}, []float64{0, 1, 2, 3}); math.Abs(v-1) > 1e-9 {
		t.Fatalf("perfect = %v", v)
	}
	if v := Spearman([]float64{0, 1, 2, 3}, []float64{3, 2, 1, 0}); math.Abs(v+1) > 1e-9 {
		t.Fatalf("reverse = %v", v)
	}
	if v := Spearman([]float64{1, 2, 2, 3}, []float64{1, 2, 3, 4}); math.IsNaN(v) {
		t.Fatal("ties must not NaN")
	}
	if !math.IsNaN(Spearman([]float64{2, 2, 2}, []float64{1, 2, 3})) {
		t.Fatal("zero-variance must be NaN")
	}
}

func TestPercentile(t *testing.T) {
	xs := []float64{10, 20, 30, 40, 50}
	if got := percentile(xs, 0.95); got != 50 {
		t.Fatalf("p95 = %v", got)
	}
	if got := percentile(xs, 0.5); got != 30 {
		t.Fatalf("p50 = %v", got)
	}
	if got := percentile(nil, 0.95); got != 0 {
		t.Fatalf("empty = %v", got)
	}
}

func TestOutcomePredictedArgmaxAndTieLow(t *testing.T) {
	if (Outcome{Res: &jev.Result{LevelProbs: [4]float64{0.1, 0.5, 0.5, 0.1}}}).Predicted() != "L1" {
		t.Fatal("tie should resolve to lower level L1")
	}
	if (Outcome{}).Predicted() != "" {
		t.Fatal("nil result predicts empty")
	}
}

func TestLoadSetValidates(t *testing.T) {
	dir := t.TempDir()
	good := `{"id":"a","expected":"L2","messages":[{"role":"user","content":"hi"}],"reply":"yo"}`
	os.WriteFile(filepath.Join(dir, "cases.jsonl"),
		[]byte(good+"\n// comment\n{'id':'b','expected':'L9'}\n"), 0o644)
	if _, err := LoadSet(filepath.Join(dir, "cases.jsonl")); err == nil {
		t.Fatal("bad level must error")
	}
	os.WriteFile(filepath.Join(dir, "cases.jsonl"),
		[]byte(good+"\n"+good+"\n"), 0o644)
	if _, err := LoadSet(filepath.Join(dir, "cases.jsonl")); err == nil {
		t.Fatal("duplicate id must error")
	}
	os.WriteFile(filepath.Join(dir, "cases.jsonl"), []byte(good+"\n"), 0o644)
	got, err := LoadSet(dir) // directory mode
	if err != nil || len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("dir load = %+v %v", got, err)
	}
	if _, err := LoadSet(filepath.Join(dir, "none.jsonl")); err == nil {
		t.Fatal("missing file must error")
	}
}

func TestRenderVerdict(t *testing.T) {
	f := &scripted{answers: perfectAnswers(), safety: map[string]float64{"s1": 0.9, "s2": 0.8}, conf: 0.9, cost: 0.00001}
	r := Evaluate(Run(context.Background(), f, corpus(), scoringCfg()), 0.7, 0.5, "v1")
	out := Render(r)
	for _, want := range []string{"quality gates", "confusion", "disagreements", "VERDICT: QUALITY GATES PASS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
}
