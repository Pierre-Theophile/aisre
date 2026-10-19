// SPDX-License-Identifier: Apache-2.0

// Package attribution is the attribution evaluation (009 T027, FR-061, research R13): every
// labelled case under fixtures/attribution is replayed from empty, the attributor runs over it,
// and its decisions are scored against the labels, per layer and in aggregate.
package attribution

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// Decision is one entry a decision named, with how.
type Decision struct {
	Slug  string  `json:"slug"`
	Layer string  `json:"layer"`
	Rule  string  `json:"rule"`
	Score float64 `json:"score"`
}

// Outcome is one labelled signal at the end of its case.
type Outcome struct {
	Case   string   `json:"case"`
	Signal string   `json:"signal"`
	Expect []string `json:"expect"`
	// Automatic is what the attributor decided on its own: its last decision before any person
	// decided about the signal or a name it carries. When a person later corrected it, this is the
	// decision they overruled, so every correction is part of what the evaluation scores (FR-046).
	// Empty is `unknown`.
	Automatic []Decision `json:"automatic"`
	// Suggestions are the candidates the automatic decision ranked without attributing.
	Suggestions []Decision `json:"suggestions,omitempty"`
	// Corrected reports whether a person decided about the signal after the automatic decision.
	Corrected bool `json:"corrected"`
	// Final is the entries the signal is attributed to at the case's end, whoever decided.
	Final []string `json:"final"`
}

// Run replays every case into a database of its own and scores it.
func Run(ctx context.Context, newStore fixture.StoreFactory, cases []*Case, opts ...catalog.Option) ([]Outcome, error) {
	var out []Outcome
	for _, c := range cases {
		outcomes, err := runCase(ctx, newStore, c, opts...)
		if err != nil {
			return nil, err
		}
		out = append(out, outcomes...)
	}
	return out, nil
}

func runCase(ctx context.Context, newStore fixture.StoreFactory, c *Case, opts ...catalog.Option) ([]Outcome, error) {
	store, cleanup, err := newStore(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	p, _, err := Replay(ctx, store, c, opts...)
	if err != nil {
		return nil, err
	}
	engine := query.NewEngine(p.Store())
	end := &graphv1.AsOf{ValidAt: timestamppb.New(c.Manifest.Clock.End)}
	var out []Outcome
	for _, label := range c.Labels {
		ref, err := graph.ParseRef(label.Signal)
		if err != nil {
			return nil, err
		}
		why, err := engine.WhyAttributed(ctx, &graphv1.WhyAttributedRequest{Signal: ref.Proto(), AsOf: end})
		if err != nil {
			return nil, fmt.Errorf("attribution: %s: why %s: %w", c.ID, label.Signal, err)
		}
		o := Outcome{Case: c.ID, Signal: label.Signal, Expect: append([]string{}, label.Expect...), Final: []string{}}
		slices.Sort(o.Expect)
		for _, e := range why.GetCurrent().GetEntries() {
			o.Final = append(o.Final, e.GetEntrySlug())
		}
		slices.Sort(o.Final)
		for _, d := range why.GetHistory() {
			if byPerson(d) {
				o.Corrected = true
				break
			}
			o.Automatic, o.Suggestions = decisions(d.GetEntries()), decisions(d.GetSuggestions())
			if d.GetEvidence().GetFields()[catalog.EvidenceAliasDiscovered].GetBoolValue() {
				for i := range o.Automatic {
					if o.Automatic[i].Layer == graph.AttributionLayerAlias {
						o.Automatic[i].Layer = LayerDiscovered
					}
				}
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// byPerson reports whether a decision is a person's: a correction or its revert, or the
// attributor applying one (a name-level correction is attributed at the correction layer).
func byPerson(d *graphv1.AttributionDecision) bool {
	if d.GetKind() != "attribute" {
		return true
	}
	for _, e := range d.GetEntries() {
		if e.GetLayer() == graph.AttributionLayerCorrection {
			return true
		}
	}
	return false
}

func decisions(items []*graphv1.Attributed) []Decision {
	out := []Decision{}
	for _, a := range items {
		out = append(out, Decision{Slug: a.GetEntrySlug(), Layer: a.GetLayer(), Rule: a.GetRuleId(), Score: a.GetScore()})
	}
	return out
}

// Right reports whether an automatic decision is what the label says.
func (o Outcome) Right() bool {
	got := []string{}
	for _, d := range o.Automatic {
		got = append(got, d.Slug)
	}
	slices.Sort(got)
	return slices.Equal(got, o.Expect)
}
