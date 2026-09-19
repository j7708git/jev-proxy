// Package route resolves a request's model name to a provider and the model
// string to forward. Resolution order is deliberate:
//
//  1. routes      — ordered glob rules on the FULL model name (escape hatch:
//     "deepseek/*" → openrouter, exact renames, per-route model)
//  2. prefix      — first path segment equals a registered provider name →
//     that provider, prefix stripped (the namespace that
//     /v1/models advertises: "nous/x", "openrouter/a/b/c")
//  3. default     — configured fallback provider (recommended: openrouter)
//
// Providers with colliding slugs (Nous and OpenRouter host many of the same
// model IDs) stay unambiguous because the namespaced name from /v1/models
// always resolves at layer 2.
package route

import (
	"fmt"
	"strings"
)

// Rule is one config route-table entry.
type Rule struct {
	Match    string // glob over the full model name; '*' matches any run, including '/'
	Provider string // registered provider name to forward to
	Model    string // optional: exact model string to forward instead (rename)
}

// Decision is the resolved upstream for one request.
type Decision struct {
	Provider string
	Model    string // model as it will be sent upstream ("" for non-chat paths)
	How      string // "route" | "prefix" | "default" — recorded for debug
	Renamed  bool   // true when the forwarded model differs from the request
}

// Resolver applies the three resolution layers.
type Resolver struct {
	rules     []Rule
	providers map[string]bool
	def       string
}

// New builds a resolver; provider names are case-sensitive.
func New(rules []Rule, providers map[string]bool, def string) *Resolver {
	return &Resolver{rules: rules, providers: providers, def: def}
}

// Resolve maps a requested model to a provider and forwarded model.
func (r *Resolver) Resolve(model string) (Decision, error) {
	if model == "" {
		return Decision{}, fmt.Errorf("route: empty model in request")
	}
	for _, ru := range r.rules {
		if !globMatch(ru.Match, model) {
			continue
		}
		if !r.providers[ru.Provider] {
			return Decision{}, fmt.Errorf("route: rule %q targets unknown provider %q", ru.Match, ru.Provider)
		}
		fwd := model
		if ru.Model != "" {
			fwd = ru.Model
		}
		return Decision{Provider: ru.Provider, Model: fwd, How: "route", Renamed: fwd != model}, nil
	}
	// Layer 2: strip a registered provider-name prefix. Only when the
	// remainder itself is non-empty ("nous/" is not a valid request).
	if i := strings.IndexByte(model, '/'); i > 0 && i < len(model)-1 {
		if head, rest := model[:i], model[i+1:]; r.providers[head] {
			return Decision{Provider: head, Model: rest, How: "prefix", Renamed: true}, nil
		}
	}
	if r.def != "" && r.providers[r.def] {
		return Decision{Provider: r.def, Model: model, How: "default"}, nil
	}
	return Decision{}, fmt.Errorf("route: no provider for model %q (no rule matched, no default set)", model)
}

// Default reports the fallback provider name ("" when none).
func (r *Resolver) Default() string { return r.def }

// Providers returns the registered provider names.
func (r *Resolver) Providers() []string {
	out := make([]string, 0, len(r.providers))
	for p := range r.providers {
		out = append(out, p)
	}
	return out
}

// Match exposes the route-table glob for reuse (e.g. model allow-lists).
func Match(pattern, s string) bool { return globMatch(pattern, s) }

// globMatch: '*' matches any sequence (including '/'); otherwise exact.
// Simple left-to-right segment scan; patterns like "a*a*a" behave greedily,
// which is fine for model-name rules.
func globMatch(pattern, s string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	if !strings.HasSuffix(s, last) {
		return false
	}
	s = s[:len(s)-len(last)]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return true
}
