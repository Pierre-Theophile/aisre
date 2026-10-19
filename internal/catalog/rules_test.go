// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"math"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Layer 2 by hand (009 T025, research R5): every score below is computed on paper first.

func name(ns, value, env string) Name {
	return Name{Ref: graph.Ref{Namespace: ns, Value: value}, Environment: env}
}

func entries(pairs ...string) map[string]Entry {
	out := map[string]Entry{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[pairs[i]] = Entry{Display: pairs[i+1], Status: graph.CatalogStatusConfirmed}
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestProbableRulesByHand(t *testing.T) {
	r := DefaultRules()
	for _, c := range []struct {
		name        string
		signal      Signal
		facts       Facts
		attributed  map[string]float64
		suggestions map[string]float64
	}{{
		// P1 alone: 0.85 ≥ τ, and nothing else scores, so the lead is 0.85 ≥ δ.
		name:       "P1 alone decides",
		signal:     Signal{Names: []Name{name("datadog.service", "production/checkout-api", "")}},
		facts:      Facts{Entries: entries("checkout", "Checkout", "infra", "Infra")},
		attributed: map[string]float64{"checkout": 0.85},
	}, {
		// P2: tokens {candidate} against {candidate, view}, J = 1/2, 0.45 + 0.4 × 0.5 = 0.65 < τ.
		name:        "P2 alone suggests",
		signal:      Signal{Names: []Name{name("datadog.service", "production/candidate-api", "")}},
		facts:       Facts{Entries: entries("candidate-view", "Candidate View", "infra", "Infra")},
		suggestions: map[string]float64{"candidate-view": 0.65},
	}, {
		// P2 0.65 and P3 0.55 by noisy-OR: 1 − 0.35 × 0.45 = 0.8425 ≥ τ, leading nothing by 0.8425.
		name: "two weak signs outrank one",
		signal: Signal{
			Names:   []Name{name("datadog.service", "production/candidate-api", "")},
			Related: []Name{name("gcp.cloudsql.instance", "p/candidate-db", "")},
		},
		facts: Facts{
			Entries: entries("candidate-view", "Candidate View"),
			Aliases: []Alias{{Name: name("gcp.cloudsql.instance", "p/candidate-db", ""), Slug: "candidate-view"}},
		},
		attributed: map[string]float64{"candidate-view": 0.8425},
	}, {
		// P1 for both: 0.85 and 0.85, the lead is 0 < δ, unknown with both suggested.
		name:        "a tie within δ is unknown",
		signal:      Signal{Names: []Name{name("datadog.monitor_name", "search", "")}},
		facts:       Facts{Entries: entries("search", "Search", "search-web", "Search web")},
		suggestions: map[string]float64{"search": 0.85, "search-web": 0.85},
	}, {
		// P4 0.5: below τ, suggested.
		name:   "an owner alone suggests",
		signal: Signal{Names: []Name{name("uptime.check", "10.0.0.7", "")}, Owners: []string{"team-a"}},
		facts: Facts{
			Entries:     entries("billing", "Billing"),
			EntryOwners: map[string][]string{"billing": {"team-a"}},
		},
		suggestions: map[string]float64{"billing": 0.5},
	}, {
		// A hidden entry is never scored.
		name:   "a hidden entry is skipped",
		signal: Signal{Names: []Name{name("datadog.service", "production/checkout-api", "")}},
		facts: Facts{
			Entries: entries("checkout", "Checkout"),
			Hidden:  map[string]bool{"checkout": true},
		},
	}} {
		t.Run(c.name, func(t *testing.T) {
			if c.facts.Hidden == nil {
				c.facts.Hidden = map[string]bool{}
			}
			d := r.Decide(c.signal, c.facts)
			check := func(what string, got []Attributed, want map[string]float64) {
				if len(got) != len(want) {
					t.Fatalf("%s: %+v, want %v", what, got, want)
				}
				for _, a := range got {
					if w, ok := want[a.Slug]; !ok || !near(a.Score, w) || a.Layer != graph.AttributionLayerProbable {
						t.Errorf("%s: %s %s %.4f, want %.4f", what, a.Slug, a.Layer, a.Score, w)
					}
				}
			}
			check("entries", d.Entries, c.attributed)
			check("suggestions", d.Suggestions, c.suggestions)
		})
	}
}

func TestAnAliasStillOutranksTheProbableLayer(t *testing.T) {
	d := DefaultRules().Decide(
		Signal{Names: []Name{name("datadog.service", "production/checkout-api", "")}},
		Facts{
			Entries: entries("checkout", "Checkout", "payments", "Payments"),
			Aliases: []Alias{{Name: name("datadog.service", "production/checkout-api", ""), Slug: "payments"}},
			Hidden:  map[string]bool{},
		})
	if len(d.Entries) != 1 || d.Entries[0].Slug != "payments" || d.Entries[0].RuleID != RuleAlias {
		t.Fatalf("decision: %+v", d.Entries)
	}
	if _, ok := d.Evidence[graph.AttributionLayerProbable]; !ok {
		t.Errorf("the probable layer's scores are not kept as evidence: %v", d.Evidence)
	}
}

func TestRulesFromConfig(t *testing.T) {
	lower := 0.7
	if _, err := RulesFromConfig(graph.AttributionRuleset, &lower, nil); err == nil {
		t.Error("a lowered threshold under the published ruleset id was accepted")
	}
	r, err := RulesFromConfig("attribution/rules@1-local", &lower, nil)
	if err != nil || r.Threshold != 0.7 || r.Margin != DefaultMargin || r.Ruleset != "attribution/rules@1-local" {
		t.Errorf("a renamed ruleset: %+v %v", r, err)
	}
	if r, err := RulesFromConfig("", nil, nil); err != nil || r != DefaultRules() {
		t.Errorf("no block: %+v %v", r, err)
	}
}
