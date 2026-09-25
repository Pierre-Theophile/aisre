// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
)

// The two vocabularies (Phase 7 Track F; FR-016, FR-029, constitution II).
//
// Both tests here are regressions on shims that used to live in `internal/cli`'s fixture
// recorder, wrapping the graph service to paper over what the engine got wrong. The wrapper's own
// comment said it: *"This is a shim over a defect in the engine, not a design."* It supplied the
// `as_of` a pointers read arrived without, and it rewrote a diff's canonical target ids into
// references before the first wave read them as references. Both defects are fixed at the source
// and the wrapper is gone, so what stops them coming back is here.
//
// The second is the more interesting of the two, because the fix is not "convert one to the
// other" but *"hold both, and know which one each field means"*. A change's targets and an edge's
// ends are **canonical entity ids**; `pointers`, `subgraph` and `impact` take a **Ref**. Neither
// spelling is wrong and neither is derivable from the other — identity resolution may have merged
// several refs onto one entity — so the engine reads the pairing out of the node versions the
// graph returns and uses whichever the field it is filling means.

// TestEveryGraphReadCarriesBothInstants (constitution II, FR-015).
//
// Feature 001's graph refuses a read with no `as_of.valid_at`, and it is right to: a graph that
// answered "now" when it was asked "then" is the confusion the bitemporal model exists to
// prevent. The engine pinned both instants before it asked anything, so it is the engine that
// must put them on the request.
func TestEveryGraphReadCarriesBothInstants(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil, "")
	if _, err := h.engine.Publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var graphReads int
	for _, record := range h.engine.Trajectory().Records() {
		body, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_WorkerRequest)
		if !ok {
			continue
		}
		term := body.WorkerRequest.GetRequest().GetTerm().GetGraph()
		if term == nil {
			continue
		}
		graphReads++
		valid, observed, named := asOfOf(term)
		if !named {
			continue // extent and node_history take no instant: the proto gives them nowhere to put one
		}
		if valid == nil {
			t.Errorf("the %s read carries no as_of.valid_at; feature 001's graph refuses such a read, "+
				"and the engine pinned the instant before it asked anything", graphTermName(term))
		}
		if observed == nil {
			t.Errorf("the %s read carries no observed instant; an answer pinned to what is known now, "+
				"for a question about what was known then, is an answer to a different question",
				graphTermName(term))
		}
	}
	if graphReads == 0 {
		t.Fatal("the provisional step made no graph read at all")
	}
}

// asOfOf reads the instants off whichever graph request the term carries. The third return says
// whether the request has anywhere to put them.
func asOfOf(term *investigationv1.GraphTerm) (valid, observed *timestamppb.Timestamp, named bool) {
	switch body := term.GetTerm().(type) {
	case *investigationv1.GraphTerm_Subgraph:
		as := body.Subgraph.GetAsOf()
		return as.GetValidAt(), as.GetObservedAt(), true
	case *investigationv1.GraphTerm_Pointers:
		as := body.Pointers.GetAsOf()
		return as.GetValidAt(), as.GetObservedAt(), true
	case *investigationv1.GraphTerm_Impact:
		as := body.Impact.GetAsOf()
		return as.GetValidAt(), as.GetObservedAt(), true
	case *investigationv1.GraphTerm_Diff:
		// A diff's window is t1/t2 and `as_of.valid_at` is ignored for it, but the observed
		// instant is not: it decides which version of the past the diff is computed over.
		return body.Diff.GetSubgraph().GetAsOf().GetValidAt(), body.Diff.GetObservedAt(), true
	case *investigationv1.GraphTerm_ResolutionAudit:
		return body.ResolutionAudit.GetObservedAt(), body.ResolutionAudit.GetObservedAt(), true
	default:
		return nil, nil, false
	}
}

func graphTermName(term *investigationv1.GraphTerm) string {
	switch term.GetTerm().(type) {
	case *investigationv1.GraphTerm_Subgraph:
		return "subgraph"
	case *investigationv1.GraphTerm_Pointers:
		return "pointers"
	case *investigationv1.GraphTerm_Impact:
		return "impact"
	case *investigationv1.GraphTerm_Diff:
		return "diff"
	case *investigationv1.GraphTerm_ResolutionAudit:
		return "resolution_audit"
	default:
		return "unknown"
	}
}

// TestTheFirstWaveNamesEntitiesInTheVocabularyEachFieldMeans (FR-016, FR-029).
//
// A `pointers` read takes a reference and `error_spans` names its two ends by canonical entity
// id. Passing a reference through under a field named `entity_id` keys the term to a string no
// backend and no recording has ever used for that entity, which is how a first wave that asked
// all the right questions got `not_recorded` to every one of them.
func TestTheFirstWaveNamesEntitiesInTheVocabularyEachFieldMeans(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil, "")
	ctx := context.Background()
	for _, step := range []func() error{
		func() error { _, err := h.engine.Publish(ctx); return err },
		func() error { _, err := h.engine.EstimateOnset(ctx); return err },
		func() error { _, err := h.engine.OrderCausally(ctx); return err },
		func() error { _, err := h.engine.RunFirstWave(ctx); return err },
	} {
		if err := step(); err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	// The catalogue learned both directions from the neighbourhood the run read first.
	catalogue := h.engine.Catalogue()
	if got := catalogue.EntityIDFor(paymentsRef); got != paymentsRef {
		t.Errorf("the catalogue resolves %s to %q; the subgraph named that entity", paymentsRef, got)
	}
	if got := catalogue.RefFor(paymentsRef); got != paymentsRef {
		t.Errorf("the catalogue renders the entity as %q; the subgraph published a reference for it", got)
	}

	var pointerReads, spanQueries int
	for _, record := range h.engine.Trajectory().Records() {
		body, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_WorkerRequest)
		if !ok {
			continue
		}
		term := body.WorkerRequest.GetRequest().GetTerm()
		if pointers := term.GetGraph().GetPointers(); pointers != nil {
			pointerReads++
			// A `Ref` is "namespace=value" in two typed fields; a canonical entity id would
			// arrive as a value with no namespace.
			if pointers.GetFocus().GetNamespace() == "" {
				t.Errorf("a pointers read names its focus %q with no namespace; the target came "+
					"out of the ranker as a canonical entity id and was never translated",
					pointers.GetFocus().GetValue())
			}
		}
		if spans, ok := term.GetTerm().(*investigationv1.AlgebraTerm_ErrorSpans); ok {
			spanQueries++
			for _, id := range []string{spans.ErrorSpans.GetSrcEntityId(), spans.ErrorSpans.GetDstEntityId()} {
				if catalogue.RefFor(id) == "" {
					t.Errorf("error_spans names an end %q that the graph never described as an "+
						"entity; the field is a canonical entity id and this is something else", id)
				}
			}
			if spans.ErrorSpans.GetEdgeType() == graphv1.EdgeType_EDGE_TYPE_UNSPECIFIED {
				t.Error("error_spans names no edge type; a span query over an edge names the edge's " +
					"own published type rather than assuming one")
			}
		}
	}
	if pointerReads == 0 {
		t.Error("the first wave read no pointers at all")
	}
	if spanQueries == 0 {
		t.Error("the first wave asked no error_spans; the neighbourhood holds an edge from the " +
			"subject to the culprit's target and the wave is meant to ask about it")
	}
}

// TestTheWindowPlanIsOneDerivation (Phase 7 Track F, FR-042c).
//
// The engine and `fixture record-world` derive their windows from the same exported functions.
// This asserts the arithmetic those functions publish, so that a change to it is a change a
// reviewer sees in a diff rather than a corpus that quietly stops intersecting.
func TestTheWindowPlanIsOneDerivation(t *testing.T) {
	t.Parallel()

	const lookback = 90 * time.Minute // as the MVP fixture's question declares it

	search := engine.OnsetSearchWindow(firedAt, lookback)
	if got := search.GetEnd().AsTime(); !got.Equal(firedAt) {
		t.Errorf("the onset search ends at %s, want the alert instant %s", got, firedAt)
	}
	if got := search.GetStart().AsTime(); !got.Equal(firedAt.Add(-lookback)) {
		t.Errorf("the onset search starts at %s, want the start of the investigation window", got)
	}

	// Half the window, capped: 45 minutes would put the baseline half in a different part of the
	// day, so the published ceiling applies.
	if got := engine.CompareHalfWidth(lookback); got != engine.CompareCeiling {
		t.Errorf("the comparison half-width for a %s lookback is %s, want the published ceiling %s",
			lookback, got, engine.CompareCeiling)
	}
	// Nobody sees the future: `observed_at` is the horizon, and the pair is planned inside it
	// (Phase 8, plan.go ComparePairs). Three cases, which are the three shapes a pair can have.
	width := engine.CompareCeiling
	for _, tc := range []struct {
		name                     string
		reference, observedAt    time.Time
		baselineFrom, baselineTo time.Time
		symptomFrom, symptomTo   time.Time
	}{
		{
			// Wholly before the horizon: untouched, the reference instant divides the two halves.
			name:      "before the horizon",
			reference: firedAt.Add(-2 * width), observedAt: firedAt,
			baselineFrom: firedAt.Add(-3 * width), baselineTo: firedAt.Add(-2 * width),
			symptomFrom: firedAt.Add(-2 * width), symptomTo: firedAt.Add(-width),
		},
		{
			// Straddling it: the symptom half ends at the horizon and the baseline narrows to the
			// same width, because a comparison between halves of different widths is not one.
			name:      "straddling the horizon",
			reference: firedAt.Add(-10 * time.Minute), observedAt: firedAt,
			baselineFrom: firedAt.Add(-20 * time.Minute), baselineTo: firedAt.Add(-10 * time.Minute),
			symptomFrom: firedAt.Add(-10 * time.Minute), symptomTo: firedAt,
		},
		{
			// At the horizon — the commonest case in the corpus, where the alert instant *is*
			// `observed_at`: the pair slides back so the symptom half is the last observable
			// width rather than the first width of the future.
			name:      "at the horizon",
			reference: firedAt, observedAt: firedAt,
			baselineFrom: firedAt.Add(-2 * width), baselineTo: firedAt.Add(-width),
			symptomFrom: firedAt.Add(-width), symptomTo: firedAt,
		},
	} {
		pairs := engine.ComparePairs(tc.reference, tc.observedAt, lookback)
		if len(pairs) != 1 {
			t.Fatalf("%s: the wave plans %d comparison pairs, want one", tc.name, len(pairs))
		}
		pair := pairs[0]
		for _, got := range []struct {
			what string
			at   time.Time
			want time.Time
		}{
			{"the baseline half starts", pair.GetBaseline().GetStart().AsTime(), tc.baselineFrom},
			{"the baseline half ends", pair.GetBaseline().GetEnd().AsTime(), tc.baselineTo},
			{"the symptom half starts", pair.GetSymptom().GetStart().AsTime(), tc.symptomFrom},
			{"the symptom half ends", pair.GetSymptom().GetEnd().AsTime(), tc.symptomTo},
		} {
			if !got.at.Equal(got.want) {
				t.Errorf("%s: %s at %s, want %s", tc.name, got.what, got.at, got.want)
			}
		}
		if end := pair.GetSymptom().GetEnd().AsTime(); end.After(tc.observedAt) {
			t.Errorf("%s: the symptom half reaches past the horizon %s to %s; nobody sees the future",
				tc.name, tc.observedAt, end)
		}
		if got := time.Duration(pair.GetWidthSeconds()) * time.Second; got !=
			pair.GetSymptom().GetEnd().AsTime().Sub(pair.GetSymptom().GetStart().AsTime()) {
			t.Errorf("%s: the pair declares width %s, which is not the width of its own halves",
				tc.name, got)
		}
	}
}
