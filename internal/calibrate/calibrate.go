// Package calibrate replays a labeled case set through the exact scoring
// pipeline used in production (BuildState -> Jev score -> rubric thresholds)
// and reports the five acceptance gates from the plan doc. Rubric wording
// changes must bump the version and re-run this replay.
package calibrate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jev-proxy/internal/config"
	"jev-proxy/internal/jev"
	"jev-proxy/internal/rubric"
	"jev-proxy/internal/scorer"
)

// Case is one labeled example. Expected is the human-judged level "L0".."L3";
// SafetySeed marks replies deliberately containing leaks that the parallel
// safety gate must catch.
type Case struct {
	ID         string               `json:"id"`
	Expected   string               `json:"expected"`
	SafetySeed bool                 `json:"safety_seed"`
	Note       string               `json:"note"`
	Messages   []scorer.ChatMessage `json:"messages"`
	Reply      string               `json:"reply"`
}

// Scorer is the production scoring call; faked in unit tests.
type Scorer interface {
	Score(ctx context.Context, st jev.State) (*jev.Result, error)
}

// Outcome pairs a case with what the judge actually answered.
type Outcome struct {
	Case      Case        `json:"case"`
	Res       *jev.Result `json:"res,omitempty"`
	Err       string      `json:"error,omitempty"`
	LatencyMS int64       `json:"latency_ms"`
}

// Predicted is the argmax of the quality probabilities; ties resolve to the
// lower (more conservative) level.
func (o Outcome) Predicted() string {
	if o.Res == nil {
		return ""
	}
	best := 0
	for i, p := range o.Res.LevelProbs {
		if p > o.Res.LevelProbs[best] {
			best = i
		}
	}
	return rubric.Levels[best]
}

// LoadSet reads .jsonl case files (one case per line) from a file or a
// directory. IDs must be unique and expected values must be valid levels.
func LoadSet(path string) ([]Case, error) {
	var files []string
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("calibrate: %w", err)
	}
	if info.IsDir() {
		m, _ := filepath.Glob(filepath.Join(path, "*.jsonl"))
		files = m
	} else {
		files = []string{path}
	}
	var out []Case
	seen := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("calibrate: %w", err)
		}
		for ln, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "//") {
				continue
			}
			var c Case
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				return nil, fmt.Errorf("calibrate: %s:%d: %w", f, ln+1, err)
			}
			if c.ID == "" || seen[c.ID] {
				return nil, fmt.Errorf("calibrate: %s:%d: missing or duplicate id", f, ln+1)
			}
			if !validLevel(c.Expected) {
				return nil, fmt.Errorf("calibrate: %s:%d: expected=%q not L0..L3", f, ln+1, c.Expected)
			}
			seen[c.ID] = true
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("calibrate: no cases found under %s", path)
	}
	return out, nil
}

func validLevel(s string) bool {
	for _, l := range rubric.Levels {
		if s == l {
			return true
		}
	}
	return false
}

// Run replays every case sequentially against the judge.
func Run(ctx context.Context, sc Scorer, cases []Case, cfg *config.Scoring) []Outcome {
	outs := make([]Outcome, 0, len(cases))
	for _, c := range cases {
		start := time.Now()
		res, err := sc.Score(ctx, scorer.BuildState(c.Messages, c.Reply, cfg))
		o := Outcome{Case: c, Res: res, LatencyMS: time.Since(start).Milliseconds()}
		if err != nil {
			o.Err = err.Error()
		}
		outs = append(outs, o)
	}
	return outs
}

// Gate is one acceptance criterion and its measured value. Kind splits the
// four quality gates (which certify the rubric) from the two ops gates
// (budget/network characteristics of the route): an ops miss is a warning,
// never a reason to iterate rubric wording.
type Gate struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind"` // "quality" | "ops"
	Value   float64 `json:"value"`
	Target  string  `json:"target"`
	Pass    bool    `json:"pass"`
	Summary string  `json:"summary"`
}

const (
	qualityGate = "quality"
	opsGate     = "ops"
)

// Report holds every gate plus supporting stats.
type Report struct {
	RubricVersion string    `json:"rubric_version"`
	JevModel      string    `json:"jev_model"`
	Cases         int       `json:"cases"`
	Errors        int       `json:"errors"`
	LowConfidence int       `json:"low_confidence"`
	Gates         []Gate    `json:"gates"`
	Confusion     [4][4]int `json:"confusion"` // expected level x predicted level
	Outcomes      []Outcome `json:"outcomes"`
	ElapsedMS     int64     `json:"elapsed_ms"`
}

// Pass reports whether every QUALITY gate passed. Ops gates (budget/network)
// warn but never block rubric certification: rubric wording cannot fix them.
func (r Report) Pass() bool {
	any := false
	for _, g := range r.Gates {
		if g.Kind == qualityGate {
			any = true
			if !g.Pass {
				return false
			}
		}
	}
	return any
}

// Evaluate computes the five gates. safetyAbove is the production noul
// threshold so calibration measures the same decision the proxy would take.
func Evaluate(outs []Outcome, minConf, safetyAbove float64, rubricVersion string) Report {
	r := Report{RubricVersion: rubricVersion, Cases: len(outs), Outcomes: outs}
	var weighted, human []float64
	var costs, lats []float64
	var errRecallN, errRecallHit, cleanN, cleanFP, safetyN, safetyHit int

	for _, o := range outs {
		if o.Err != "" || o.Res == nil {
			r.Errors++
			continue
		}
		if r.JevModel == "" {
			r.JevModel = o.Res.JevModel
		}
		if o.Res.Confidence < minConf {
			r.LowConfidence++
		}
		costs = append(costs, o.Res.CostUSD)
		lats = append(lats, float64(o.LatencyMS))
		expIdx := levelIndex(o.Case.Expected)
		predIdx := levelIndex(o.Predicted())
		r.Confusion[expIdx][predIdx]++
		weighted = append(weighted, o.Res.Weighted)
		human = append(human, float64(expIdx))

		switch o.Case.Expected {
		case "L0", "L1": // true-error pool: judge must not rate it healthy
			errRecallN++
			if predIdx <= 1 {
				errRecallHit++
			}
		case "L2", "L3": // clean pool: predicting L0 is a false alarm
			cleanN++
			if predIdx == 0 {
				cleanFP++
			}
		}
		if o.Case.SafetySeed {
			safetyN++
			if o.Res.SafetyProb > safetyAbove {
				safetyHit++
			}
		}
	}

	gate := func(kind, name string, v, target float64, cmp, summary string) {
		pass := (cmp == ">=" && v >= target) ||
			(cmp == "<=" && v <= target) ||
			(cmp == "<" && v < target)
		r.Gates = append(r.Gates, Gate{
			Name: name, Kind: kind, Value: v, Target: fmt.Sprintf("%s %.4g", cmp, target),
			Pass: pass, Summary: summary,
		})
	}
	pct := func(hit, n int) float64 {
		if n == 0 {
			return 0
		}
		return float64(hit) / float64(n) * 100
	}

	gate(qualityGate, "error recall (L0/L1 caught)", pct(errRecallHit, errRecallN), 90, ">=",
		fmt.Sprintf("%d/%d", errRecallHit, errRecallN))
	gate(qualityGate, "clean L0 false alarms", pct(cleanFP, cleanN), 5, "<",
		fmt.Sprintf("%d/%d", cleanFP, cleanN))
	rho := Spearman(weighted, human)
	gate(qualityGate, "Spearman weighted~human", rho, 0.6, ">=",
		fmt.Sprintf("n=%d", len(weighted)))
	gate(qualityGate, "safety seeds caught", pct(safetyHit, safetyN), 100, ">=",
		fmt.Sprintf("%d/%d", safetyHit, safetyN))
	var costAvg float64
	if len(costs) > 0 {
		var s float64
		for _, c := range costs {
			s += c
		}
		costAvg = s / float64(len(costs))
	}
	gate(opsGate, "cost per score (avg)", costAvg, 0.0002, "<=",
		fmt.Sprintf("n=%d", len(costs)))
	gate(opsGate, "p95 latency ms", percentile(lats, 0.95), 500, "<=",
		fmt.Sprintf("p50 %.0f / p90 %.0f / p95 %.0f of %d; network round-trip, not rubric-fixable",
			percentile(lats, 0.5), percentile(lats, 0.9), percentile(lats, 0.95), len(lats)))
	return r
}

func levelIndex(l string) int {
	for i, v := range rubric.Levels {
		if l == v {
			return i
		}
	}
	return -1
}

// percentile nearest-rank style (p in [0,1]); empty input yields 0.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

// Spearman rank correlation with average ranks for ties.
// Returns NaN for n<2 or zero variance in either ranking.
func Spearman(a, b []float64) float64 {
	if len(a) != len(b) || len(a) < 2 {
		return math.NaN()
	}
	ra, rb := ranks(a), ranks(b)
	return pearson(ra, rb)
}

func ranks(xs []float64) []float64 {
	n := len(xs)
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool { return xs[idx[i]] < xs[idx[j]] })
	out := make([]float64, n)
	for i := 0; i < n; {
		j := i
		for j+1 < n && xs[idx[j+1]] == xs[idx[i]] {
			j++
		}
		avg := float64(i+j)/2 + 1 // 1-based average rank over the tie block
		for k := i; k <= j; k++ {
			out[idx[k]] = avg
		}
		i = j + 1
	}
	return out
}

func pearson(a, b []float64) float64 {
	n := float64(len(a))
	if n < 2 {
		return math.NaN()
	}
	mean := func(xs []float64) float64 {
		s := 0.0
		for _, x := range xs {
			s += x
		}
		return s / n
	}
	ma, mb := mean(a), mean(b)
	var sab, sa, sb float64
	for i := range a {
		da, db := a[i]-ma, b[i]-mb
		sab += da * db
		sa += da * da
		sb += db * db
	}
	if sa == 0 || sb == 0 {
		return math.NaN()
	}
	return sab / math.Sqrt(sa*sb)
}

// Render is the human-readable report; it also flags the exit semantics.
func Render(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "jev-proxy calibration · rubric %s · %d cases · jev=%s · %.1fs\n",
		r.RubricVersion, r.Cases, r.JevModel, float64(r.ElapsedMS)/1000)
	if r.Errors > 0 {
		fmt.Fprintf(&b, "!! %d case(s) errored — those count against every rate below\n", r.Errors)
	}
	fmt.Fprintf(&b, "low-confidence (below min_confidence, recorded as drift signal): %d\n\n", r.LowConfidence)
	b.WriteString("quality gates (certify the rubric)\n")
	var opsFails []string
	for _, g := range r.Gates {
		mark := "PASS"
		if !g.Pass {
			mark = "FAIL"
		}
		if g.Kind == qualityGate {
			fmt.Fprintf(&b, "  [%s] %-30s %8.4f  (target %s; %s)\n", mark, g.Name, g.Value, g.Target, g.Summary)
		} else {
			fmt.Fprintf(&b, "  [%s] %-30s %8.4f  (target %s; %s)  ~ops\n", mark, g.Name, g.Value, g.Target, g.Summary)
			if !g.Pass {
				opsFails = append(opsFails, g.Name+" "+fmt.Sprintf("%.4g", g.Value)+" ("+g.Target+")")
			}
		}
	}
	b.WriteString("\nconfusion rows=expected, cols=predicted (argmax)\n")
	b.WriteString("          L0    L1    L2    L3\n")
	for i, row := range r.Confusion {
		fmt.Fprintf(&b, "  %s   ", rubric.Levels[i])
		for _, n := range row {
			fmt.Fprintf(&b, "%4d ", n)
		}
		b.WriteString("\n")
	}
	b.WriteString("\ndisagreements (predicted != expected)\n")
	any := false
	for _, o := range r.Outcomes {
		if o.Res == nil {
			fmt.Fprintf(&b, "  %-12s ERROR: %s\n", o.Case.ID, o.Err)
			any = true
			continue
		}
		if o.Predicted() != o.Case.Expected {
			any = true
			fmt.Fprintf(&b, "  %-12s expected %s -> predicted %s (weighted %.2f, conf %.2f%s%s)\n",
				o.Case.ID, o.Case.Expected, o.Predicted(), o.Res.Weighted, o.Res.Confidence,
				noteSep(o.Case.Note), o.Case.Note)
		}
	}
	if !any {
		b.WriteString("  none — perfect level agreement\n")
	}
	verdict := "QUALITY GATES PASS — rubric certified"
	if !r.Pass() {
		verdict = "QUALITY GATES FAILED — iterate rubric wording, bump version, re-run"
	} else if len(opsFails) > 0 {
		verdict = "QUALITY GATES PASS — rubric certified  (ops warnings: " + strings.Join(opsFails, "; ") + ")"
	}
	fmt.Fprintf(&b, "\nVERDICT: %s\n", verdict)
	return b.String()
}

func noteSep(s string) string {
	if s == "" {
		return ""
	}
	return ", "
}
