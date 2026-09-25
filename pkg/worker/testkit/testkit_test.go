// SPDX-License-Identifier: Apache-2.0

package testkit_test

import (
	"context"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/traces"
	"github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend/synthetic"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
	"github.com/Pierre-Theophile/aisre/pkg/worker/testkit"
)

// The worker testkit, and with it the conformance suite of the three telemetry workers
// (tasks.md T043, T044, T047, T048, T050).
//
// Each worker is built twice over the same data: once over the synthetic backend (live) and once
// over the recorded backend reading a world recorded from it. ModesAgree then asserts the two
// produce identical digests — coverage block and join keys included — which is the property that
// makes a recorded corpus a test of the live path rather than a test of itself (SC-003, FR-015).

var (
	origin  = time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	rollout = origin.Add(80 * time.Minute)
)

func scenario() synthetic.Scenario {
	return synthetic.Scenario{
		Seed:       "worker-testkit",
		Start:      origin,
		End:        origin.Add(2 * time.Hour),
		Resolution: time.Minute,
		Entities: []synthetic.Entity{{
			EntityID:        "entity-payments",
			Name:            "payments",
			Selectors:       []string{"sum:payments.http.errors"},
			BaselineVersion: "rev6",
		}, {
			EntityID:        "entity-checkout",
			Name:            "checkout",
			Selectors:       []string{"sum:checkout.http.errors"},
			BaselineVersion: "rev3",
		}},
		Edges: []synthetic.Edge{{SrcEntityID: "entity-checkout", DstEntityID: "entity-payments", WeightClass: 2}},
		Changes: []synthetic.Change{{
			EntityID:        "k8s.change=shop/payments@rev7",
			At:              rollout,
			TargetEntityIDs: []string{"entity-payments"},
			Version:         "rev7",
			Degrades:        true,
		}},
	}
}

func pointer(selector string) *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind:       graphv1.PointerKind_METRIC,
		Selector:   selector,
		JoinKeys:   map[string]string{"version": "service.version"},
		Vocabulary: "synthetic/1",
	}
}

// world records every term the three workers declare, plus the depth-1 handles they mint.
func world(t *testing.T, b *synthetic.Backend) string {
	t.Helper()
	dir := t.TempDir()
	pair := engine.NewWindowPair(rollout, 30*time.Minute)

	recorder, err := backend.NewRecorderWithOptions(dir, backend.RecordOptions{
		HopRadius:              1,
		DrillDownDepth:         1,
		WindowGrid:             []*investigationv1.WindowPair{pair},
		RedactionPolicyVersion: engine.DefaultRedactionPolicyVersion,
	})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}

	terms := make([]*engine.Term, 0, 16)
	for _, selector := range []string{"sum:payments.http.errors", "sum:checkout.http.errors"} {
		p := pointer(selector)
		terms = append(terms,
			engine.Compare(p, pair, investigationv1.Statistic_ERROR_RATE),
			engine.Onset(p, engine.NewWindow(pair.GetBaseline().GetStart().AsTime(), pair.GetSymptom().GetEnd().AsTime()),
				investigationv1.OnsetMethod_SEASONAL_CUSUM),
			engine.ErrorsByVersion(p, pair.GetSymptom(), "service.version"),
			engine.MonitorState(p, pair.GetSymptom()),
			engine.NewLogPatterns(p, pair.GetSymptom(), pair.GetBaseline()),
		)
	}
	terms = append(terms, engine.ErrorSpans("entity-checkout", "entity-payments",
		graphv1.EdgeType_CALLS, pair.GetSymptom()))

	handles := make([]*investigationv1.Handle, 0, 16)
	for _, term := range terms {
		handles = append(handles, record(t, b, recorder, term)...)
	}
	for _, handle := range handles {
		record(t, b, recorder, engine.DrillDown(handle))
		record(t, b, recorder, engine.Exemplars(handle, engine.MaxExemplars))
	}
	if _, err := recorder.Close(context.Background()); err != nil {
		t.Fatalf("close recorder: %v", err)
	}
	return dir
}

func record(t *testing.T, b *synthetic.Backend, recorder backend.Recorder, term *engine.Term) []*investigationv1.Handle {
	t.Helper()
	req := &engine.Request{Term: term, DiscriminatingQuestion: "testkit:record", WantExemplars: true}
	resp, err := b.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute %s: %v", engine.TermName(term), err)
	}
	if err := recorder.Record(context.Background(), req, resp); err != nil {
		t.Fatalf("record %s: %v", engine.TermName(term), err)
	}

	out := make([]*investigationv1.Handle, 0, 4)
	switch body := resp.GetDigest().GetBody().(type) {
	case *investigationv1.Digest_Metric:
		for _, series := range body.Metric.GetSeries() {
			if h := series.GetDrillDown().GetHandle(); h.GetValue() != "" {
				out = append(out, h)
			}
		}
	case *investigationv1.Digest_Log:
		for _, pattern := range body.Log.GetPatterns() {
			if h := pattern.GetDrillDown().GetHandle(); h.GetValue() != "" {
				out = append(out, h)
			}
		}
	case *investigationv1.Digest_Trace:
		for _, group := range body.Trace.GetGroups() {
			if h := group.GetDrillDown().GetHandle(); h.GetValue() != "" {
				out = append(out, h)
			}
		}
	case *investigationv1.Digest_ErrorsByVersion:
		for _, version := range body.ErrorsByVersion.GetVersions() {
			if h := version.GetDrillDown().GetHandle(); h.GetValue() != "" {
				out = append(out, h)
			}
		}
	}
	return out
}

// TestWorkerConformance is the suite every telemetry worker of this feature must pass.
func TestWorkerConformance(t *testing.T) {
	t.Parallel()

	live, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("new synthetic backend: %v", err)
	}
	dir := world(t, live)
	recorded, err := engine.NewRecorded(dir)
	if err != nil {
		t.Fatalf("new recorded backend: %v", err)
	}

	for _, tc := range []struct {
		name     string
		live     worker.Worker
		recorded worker.Worker
	}{
		{"metrics", metrics.New(live), metrics.New(recorded)},
		{"traces", traces.New(live), traces.New(recorded)},
		{"logs", logs.New(live), logs.New(recorded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testkit.Conformance(t, tc.recorded,
				testkit.WithFixture(dir),
				testkit.WithRecordedCounterpart(tc.recorded))
			t.Run("live_matches_recorded", func(t *testing.T) {
				testkit.ModesAgree(t, tc.live,
					testkit.WithFixture(dir),
					testkit.WithRecordedCounterpart(tc.recorded))
			})
		})
	}
}

// TestDeclarationsAreWhatTheContractSays pins the parts of each declaration a reader of
// `worker list` relies on: exactly one source of truth, every capability read-only, both modes,
// and — the one that must never drift — no model in the graph, metrics or traces workers.
func TestDeclarationsAreWhatTheContractSays(t *testing.T) {
	t.Parallel()
	live, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("new synthetic backend: %v", err)
	}

	for _, tc := range []struct {
		name          string
		worker        worker.Worker
		containsModel bool
		terms         []string
	}{
		{"metrics", metrics.New(live), false,
			[]string{engine.TermCompare, engine.TermOnset, engine.TermErrorsByVersion, engine.TermMonitorState}},
		{"traces", traces.New(live), false,
			[]string{engine.TermErrorSpans, engine.TermCompare}},
		{"logs", logs.New(live), false,
			[]string{engine.TermNewLogPatterns, engine.TermExemplars, engine.TermDrillDown}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := tc.worker.Describe()
			testkit.Declaration(t, tc.worker)

			if d.Name != tc.name {
				t.Errorf("name = %q, want %q", d.Name, tc.name)
			}
			if d.ContainsModel != tc.containsModel {
				t.Errorf("contains model = %v, want %v; a model is declared, not discovered (FR-009a)",
					d.ContainsModel, tc.containsModel)
			}
			if len(d.Capabilities) != len(tc.terms) {
				t.Errorf("declares %d capabilities, want %d", len(d.Capabilities), len(tc.terms))
			}
			for _, term := range tc.terms {
				if !d.Declares(term) {
					t.Errorf("does not declare %s", term)
				}
			}
			if d.Redaction.PolicyVersion == "" {
				t.Error("declares no redaction policy version")
			}
		})
	}
}

// TestLogsWorkerDeclaresItsModel: turning the optional labelling pass on flips ContainsModel and
// names the model. A worker whose answer a model touched says so, and an undeclared model is the
// failure FR-009a exists to make impossible.
func TestLogsWorkerDeclaresItsModel(t *testing.T) {
	t.Parallel()
	live, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("new synthetic backend: %v", err)
	}

	plain := logs.New(live).Describe()
	if plain.ContainsModel || plain.ModelID != "" {
		t.Fatalf("the default logs worker declares a model (%v, %q); the labelling pass is off unless configured",
			plain.ContainsModel, plain.ModelID)
	}

	labelled := logs.NewWithLabeller(live, stubLabeller{}).Describe()
	if !labelled.ContainsModel || labelled.ModelID != logs.LabellingModelID {
		t.Fatalf("with a labeller: contains model = %v, model = %q; want true and %q",
			labelled.ContainsModel, labelled.ModelID, logs.LabellingModelID)
	}
	if err := labelled.Validate(); err != nil {
		t.Errorf("a worker declaring its labeller is not registrable: %v", err)
	}
}

type stubLabeller struct{}

func (stubLabeller) ModelID() string { return logs.LabellingModelID }

func (stubLabeller) Label(_ context.Context, templates []logs.LabelInput) (map[string]string, string, error) {
	labels := make(map[string]string, len(templates))
	for _, template := range templates {
		if template.NewInWindow {
			labels[template.Template] = "new failure mode"
		}
	}
	return labels, "labelled the templates that are new in the window", nil
}
