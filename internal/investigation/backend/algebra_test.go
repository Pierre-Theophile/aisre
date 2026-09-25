// SPDX-License-Identifier: Apache-2.0

package backend_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The algebra (tasks.md T033, FR-042b).

var origin = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)

func pointer() *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind:       graphv1.PointerKind_METRIC,
		Selector:   "sum:payments.http.errors",
		Vocabulary: "datadog-query",
		JoinKeys:   map[string]string{"version": "service.version"},
	}
}

func TestTermNameAndFamily(t *testing.T) {
	t.Parallel()

	window := engine.NewWindow(origin.Add(-time.Hour), origin)
	pair := engine.NewWindowPair(origin, 15*time.Minute)
	handle := &engine.Handle{Value: "abc", Depth: 1}

	tests := []struct {
		term   *engine.Term
		name   string
		family sdk.Family
	}{
		{engine.Compare(pointer(), pair, investigationv1.Statistic_ERROR_RATE), "compare", sdk.FamilyTelemetry},
		{engine.Onset(pointer(), window, investigationv1.OnsetMethod_SEASONAL_CUSUM), "onset", sdk.FamilyTelemetry},
		{engine.NewLogPatterns(pointer(), window, window), "new_log_patterns", sdk.FamilyTelemetry},
		{engine.ErrorSpans("a", "b", graphv1.EdgeType_CALLS, window), "error_spans", sdk.FamilyTelemetry},
		{engine.ErrorsByVersion(pointer(), window, "service.version"), "errors_by_version", sdk.FamilyTelemetry},
		{engine.MonitorState(pointer(), window), "monitor_state", sdk.FamilyTelemetry},
		{engine.Exemplars(handle, 5), "exemplars", sdk.FamilyTelemetry},
		{engine.DrillDown(handle), "drill_down", sdk.FamilyTelemetry},
		{engine.KnowledgeSearch([]string{"e1"}, []string{"checkout"}, 5), "knowledge_search", sdk.FamilyKnowledge},
		{engine.GraphSubgraph(&graphv1.SubgraphRequest{}), "subgraph", sdk.FamilyGraph},
		{engine.GraphDiff(&graphv1.DiffRequest{}), "diff", sdk.FamilyGraph},
		{engine.GraphImpact(&graphv1.ImpactRequest{}), "impact", sdk.FamilyGraph},
		{engine.GraphPointers(&graphv1.PointersRequest{}), "pointers", sdk.FamilyGraph},
		{engine.GraphNodeHistory(&graphv1.NodeHistoryRequest{}), "node_history", sdk.FamilyGraph},
		{engine.GraphResolutionAudit(&graphv1.ResolutionAuditRequest{}), "resolution_audit", sdk.FamilyGraph},
		{engine.GraphExtent(&graphv1.ExtentRequest{}), "extent", sdk.FamilyGraph},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := engine.TermName(tc.term); got != tc.name {
				t.Fatalf("term name = %q, want %q", got, tc.name)
			}
			if got := engine.FamilyOf(tc.name); got != tc.family {
				t.Errorf("family of %s = %q, want %q", tc.name, got, tc.family)
			}
			key, err := engine.TermKey(tc.term)
			if err != nil {
				t.Fatalf("term key: %v", err)
			}
			if len(key) != 64 {
				t.Errorf("term key %q is not a 64-character sha256 hex digest", key)
			}
		})
	}

	// The eight telemetry terms and no more: adding one is a published schema change.
	if got := len(sdk.Terms(sdk.FamilyTelemetry)); got != 8 {
		t.Errorf("the telemetry family has %d terms, want 8: %v", got, sdk.Terms(sdk.FamilyTelemetry))
	}
}

// TestTermKeyIsAFunctionOfTheQuestion: the key must not depend on the spelling. A world keyed by
// the spelling would answer NOT_RECORDED to a question it in fact holds.
func TestTermKeyIsAFunctionOfTheQuestion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b *engine.Term
		same bool
	}{
		{
			name: "sub-second precision is normalised away",
			a:    engine.MonitorState(pointer(), engine.NewWindow(origin, origin.Add(time.Hour))),
			b: engine.MonitorState(pointer(), &engine.Window{
				Start: timestamppb.New(origin.Add(400 * time.Millisecond)),
				End:   timestamppb.New(origin.Add(time.Hour).Add(999 * time.Millisecond)),
			}),
			same: true,
		},
		{
			name: "an unset algebra version is filled in with the current one",
			a:    engine.MonitorState(pointer(), engine.NewWindow(origin, origin.Add(time.Hour))),
			b:    withoutVersion(engine.MonitorState(pointer(), engine.NewWindow(origin, origin.Add(time.Hour)))),
			same: true,
		},
		{
			name: "entity ids are a set, not a list",
			a:    engine.KnowledgeSearch([]string{"b", "a"}, []string{"Checkout", "errors"}, 5),
			b:    engine.KnowledgeSearch([]string{"a", "b", "a"}, []string{"errors", "checkout"}, 5),
			same: true,
		},
		{
			name: "a different window is a different question",
			a:    engine.MonitorState(pointer(), engine.NewWindow(origin, origin.Add(time.Hour))),
			b:    engine.MonitorState(pointer(), engine.NewWindow(origin, origin.Add(2*time.Hour))),
			same: false,
		},
		{
			name: "a different statistic is a different question",
			a:    engine.Compare(pointer(), engine.NewWindowPair(origin, time.Hour), investigationv1.Statistic_ERROR_RATE),
			b:    engine.Compare(pointer(), engine.NewWindowPair(origin, time.Hour), investigationv1.Statistic_P95),
			same: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := engine.TermKey(tc.a)
			if err != nil {
				t.Fatalf("term key a: %v", err)
			}
			b, err := engine.TermKey(tc.b)
			if err != nil {
				t.Fatalf("term key b: %v", err)
			}
			if (a == b) != tc.same {
				t.Fatalf("keys %s and %s: equal = %v, want %v", a, b, a == b, tc.same)
			}
		})
	}
}

// TestTermKeyIsStableAcrossRuns: the key is what a world is filed under, so it must not move
// between two runs of the same build — map iteration order in the join keys included.
func TestTermKeyIsStableAcrossRuns(t *testing.T) {
	t.Parallel()
	want, err := engine.TermKey(engine.ErrorsByVersion(pointer(), engine.NewWindow(origin, origin.Add(time.Hour)), "service.version"))
	if err != nil {
		t.Fatalf("term key: %v", err)
	}
	for range 200 {
		got, err := engine.TermKey(engine.ErrorsByVersion(pointer(), engine.NewWindow(origin, origin.Add(time.Hour)), "service.version"))
		if err != nil {
			t.Fatalf("term key: %v", err)
		}
		if got != want {
			t.Fatalf("term key moved between runs: %s then %s", want, got)
		}
	}
}

// TestOutsideAlgebraNamesWhatIsAvailable: the refusal is the one an SRE reads instead of the
// contract, so it has to say what was asked and what could have been.
func TestOutsideAlgebraNamesWhatIsAvailable(t *testing.T) {
	t.Parallel()
	err := engine.OutsideAlgebra("run_this_promql")
	if engine.ReasonOf(err) != engine.ReasonOutsideAlgebra {
		t.Fatalf("reason = %q, want %q", engine.ReasonOf(err), engine.ReasonOutsideAlgebra)
	}
	for _, want := range []string{"run_this_promql", "compare", "subgraph", "knowledge_search"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %s", want, err)
		}
	}
}

func TestValidateRefusesATermThatCannotBeAnswered(t *testing.T) {
	t.Parallel()

	window := engine.NewWindow(origin.Add(-time.Hour), origin)
	inverted := engine.NewWindow(origin, origin.Add(-time.Hour))

	tests := []struct {
		name string
		term *engine.Term
		want string
	}{
		{"compare with no statistic", engine.Compare(pointer(), engine.NewWindowPair(origin, time.Hour), investigationv1.Statistic_STATISTIC_UNSPECIFIED), "statistic"},
		{"compare with no pointer", engine.Compare(nil, engine.NewWindowPair(origin, time.Hour), investigationv1.Statistic_P95), "pointer"},
		{"onset with no method", engine.Onset(pointer(), window, investigationv1.OnsetMethod_ONSET_METHOD_UNSPECIFIED), "method"},
		{"onset with an inverted window", engine.Onset(pointer(), inverted, investigationv1.OnsetMethod_SEASONAL_CUSUM), "inverted"},
		{"error_spans with no edge", engine.ErrorSpans("", "", graphv1.EdgeType_CALLS, window), "edge"},
		{"errors_by_version with no version attribute", engine.ErrorsByVersion(pointer(), window, ""), "version"},
		{"exemplars with no handle", engine.Exemplars(&engine.Handle{}, 5), "handle"},
		{"drill_down with no handle", engine.DrillDown(nil), "handle"},
		{"knowledge_search with no entities", engine.KnowledgeSearch(nil, []string{"x"}, 5), "entities"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := engine.Validate(tc.term)
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if engine.ReasonOf(err) != engine.ReasonOutsideAlgebra {
				t.Errorf("reason = %q, want %q", engine.ReasonOf(err), engine.ReasonOutsideAlgebra)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestUnknownAlgebraVersionIsRefused: a term of a version this build does not implement is
// refused rather than guessed at, because every term key would move under it.
func TestUnknownAlgebraVersionIsRefused(t *testing.T) {
	t.Parallel()
	term := engine.MonitorState(pointer(), engine.NewWindow(origin, origin.Add(time.Hour)))
	term.AlgebraVersion = "99.0.0"

	if _, err := engine.TermKey(term); engine.ReasonOf(err) != engine.ReasonUnknownAlgebraVersion {
		t.Fatalf("reason = %q, want %q (%v)", engine.ReasonOf(err), engine.ReasonUnknownAlgebraVersion, err)
	}
}

func withoutVersion(term *engine.Term) *engine.Term {
	term.AlgebraVersion = ""
	return term
}
