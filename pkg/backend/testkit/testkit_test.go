// SPDX-License-Identifier: Apache-2.0

package testkit_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend/synthetic"
	"github.com/Pierre-Theophile/aisre/pkg/backend/testkit"
)

// The backend testkit, checked against a world it records itself (tasks.md T050).
//
// The test records a small world from the synthetic backend, then replays that world back
// through the same backend and through the recorded one. If the three ever disagree — the live
// answer, the recorded answer and the bytes on disk — one of them is wrong, and a replayed
// investigation stops being evidence about a live one (SC-003).

var (
	origin  = time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	rollout = origin.Add(80 * time.Minute)
)

func scenario() synthetic.Scenario {
	return synthetic.Scenario{
		Seed:       "testkit-fixture",
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
		Edges: []synthetic.Edge{{
			SrcEntityID: "entity-checkout",
			DstEntityID: "entity-payments",
			WeightClass: 2,
		}},
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

// recordWorld records a small but complete world: every telemetry term over two selectors and
// one edge, plus the depth-1 drill-downs and exemplars the answers mint.
func recordWorld(t *testing.T, b *synthetic.Backend) string {
	t.Helper()
	dir := t.TempDir()

	pair := engine.NewWindowPair(rollout, 30*time.Minute)
	recorder, err := backend.NewRecorderWithOptions(dir, backend.RecordOptions{
		HopRadius:              1,
		DrillDownDepth:         1,
		WindowGrid:             []*investigationv1.WindowPair{pair},
		RedactionPolicyVersion: engine.DefaultRedactionPolicyVersion,
		Focus:                  "otel.service.name=checkout",
	})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}

	terms := make([]*engine.Term, 0, 32)
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

	handles := make([]*investigationv1.Handle, 0, 32)
	for _, term := range terms {
		resp := execute(t, b, term)
		recordOne(t, recorder, term, resp)
		handles = append(handles, handlesOf(resp)...)
	}
	for _, handle := range handles {
		for _, term := range []*engine.Term{
			engine.DrillDown(handle),
			engine.Exemplars(handle, engine.MaxExemplars),
		} {
			recordOne(t, recorder, term, execute(t, b, term))
		}
	}
	if _, err := recorder.Close(context.Background()); err != nil {
		t.Fatalf("close recorder: %v", err)
	}
	return dir
}

func execute(t *testing.T, b *synthetic.Backend, term *engine.Term) *engine.Response {
	t.Helper()
	resp, err := b.Execute(context.Background(), &engine.Request{
		Term:                   term,
		DiscriminatingQuestion: "testkit:record",
	})
	if err != nil {
		t.Fatalf("execute %s: %v", engine.TermName(term), err)
	}
	return resp
}

func recordOne(t *testing.T, recorder backend.Recorder, term *engine.Term, resp *engine.Response) {
	t.Helper()
	if err := recorder.Record(context.Background(), &engine.Request{Term: term}, resp); err != nil {
		t.Fatalf("record %s: %v", engine.TermName(term), err)
	}
}

func handlesOf(resp *engine.Response) []*investigationv1.Handle {
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

func TestConformance(t *testing.T) {
	t.Parallel()
	b, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("new synthetic backend: %v", err)
	}
	testkit.Conformance(t, b, testkit.WithFixture(recordWorld(t, b)))
}

// TestRecordedBackendPassesItsOwnWorld: the recorded backend is a backend like any other and
// must satisfy the same conformance suite.
func TestRecordedBackendPassesItsOwnWorld(t *testing.T) {
	t.Parallel()
	b, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("new synthetic backend: %v", err)
	}
	dir := recordWorld(t, b)

	recorded, err := engine.NewRecorded(dir)
	if err != nil {
		t.Fatalf("new recorded backend: %v", err)
	}
	testkit.Conformance(t, recorded, testkit.WithFixture(dir))
}

// TestRecordedBackendMissesAreTypedAndCounted: an in-algebra term the world does not hold is
// NOT_RECORDED with the term echoed — never NO_DATA, and never a live call.
func TestRecordedBackendMissesAreTypedAndCounted(t *testing.T) {
	t.Parallel()
	b, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("new synthetic backend: %v", err)
	}
	recorded, err := engine.NewRecorded(recordWorld(t, b))
	if err != nil {
		t.Fatalf("new recorded backend: %v", err)
	}

	// A window nobody recorded: in the algebra, not in this world.
	missing := engine.Compare(pointer("sum:payments.http.errors"),
		engine.NewWindowPair(rollout, 7*time.Minute), investigationv1.Statistic_P95)
	resp, err := recorded.Execute(context.Background(), &engine.Request{
		Term:                   missing,
		DiscriminatingQuestion: "testkit:miss",
	})
	if err != nil {
		t.Fatalf("execute a miss: %v", err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_NOT_RECORDED {
		t.Fatalf("outcome = %s, want NOT_RECORDED; only NO_DATA is evidence that nothing happened (FR-027)", resp.GetOutcome())
	}
	if resp.GetDigest().GetCoverage() == nil {
		t.Errorf("a NOT_RECORDED answer carries no coverage block; it must say what the world does hold")
	}

	outcome, err := engine.OutcomeOf(resp)
	if err != nil {
		t.Fatalf("outcome of: %v", err)
	}
	if outcome.MeansNothingHappened() {
		t.Errorf("NOT_RECORDED reported as evidence that nothing happened; that is the defect FR-027 names")
	}
	if !strings.Contains(outcome.Render(), "not a fact about production") {
		t.Errorf("NOT_RECORDED renders as %q, which does not say it is a gap in the recording", outcome.Render())
	}

	report := recorded.Misses()
	if report.NotRecorded() != 1 {
		t.Errorf("miss count = %d, want 1", report.NotRecorded())
	}
	if report.MissRate() <= 0 {
		t.Errorf("miss rate = %f, want a positive rate after a miss", report.MissRate())
	}
}

// TestLoadWorldRefusesAnEditedRecording: a world whose files were changed by hand no longer
// matches its index digest and is refused. A recording that can drift is a recording that
// proves nothing.
func TestLoadWorldRefusesAnEditedRecording(t *testing.T) {
	t.Parallel()
	b, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("new synthetic backend: %v", err)
	}
	dir := recordWorld(t, b)

	world, err := backend.LoadWorld(dir)
	if err != nil {
		t.Fatalf("load world: %v", err)
	}
	var edited bool
	for _, key := range world.Keys() {
		victim := filepath.Join(dir, world.Index.TermKeyToFile[key])
		body, err := os.ReadFile(victim) //nolint:gosec // the path comes from the world this test wrote
		if err != nil {
			t.Fatalf("read %s: %v", victim, err)
		}
		changed := strings.Replace(string(body), `"volumeConsidered":"`, `"volumeConsidered":"9`, 1)
		if changed == string(body) {
			continue
		}
		if err := os.WriteFile(victim, []byte(changed), 0o600); err != nil {
			t.Fatalf("write %s: %v", victim, err)
		}
		edited = true
		break
	}
	if !edited {
		t.Fatalf("no answer in the world at %s holds a volume to change", dir)
	}

	if _, err := backend.LoadWorld(dir); err == nil {
		t.Fatalf("an edited world loaded without complaint; the index digest must be checked on load")
	}
}
