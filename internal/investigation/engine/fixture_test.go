// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	backend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The recorded world of `rollout-regression-01-incident` (tasks.md T066; FR-046a, SC-013).
//
// Two things are asserted against the real corpus here, and neither needs a database or a network.
//
// The first is T066's record-time property: a fixture's recorded world holds every published
// first-wave query for the culprit's target. A world that cannot answer the first wave is a world
// that cannot answer the investigation it was recorded for, and finding that out during an
// evaluation run rather than during recording wastes the recording.
//
// The second is that the engine's published judgment rules, applied to the digests that world
// actually holds, reach the fixture's ground truth: the rollout's own version carries the errors,
// the comparison across the onset moves up, and the coincident decoy's does not. That is the
// deterministic half of the engine measured against real recorded data rather than against a
// scenario written to suit it.

const incidentFixture = "../../../fixtures/incidents/rollout-regression-01-incident"

// The selectors the fixture's graph published, as they appear in its recorded world.
const (
	paymentsMetric = `service.name="payments-v2" AND service.namespace="shop" ` +
		`AND metric.name="http.server.request.duration"`
	paymentsLog      = `k8s.namespace.name="shop" AND k8s.deployment.name="payments"`
	storefrontMetric = `service.name="storefront" AND service.namespace="shop" AND metric.name="http.server.request.duration"`
	fixtureOnset     = "2026-09-01T14:20:00Z"
)

func loadWorld(t *testing.T) *backend.Recorded {
	t.Helper()
	if _, err := os.Stat(incidentFixture + "/world/index.json"); err != nil {
		t.Skipf("the incident fixture is not present: %v", err)
	}
	recorded, err := backend.NewRecorded(incidentFixture + "/world")
	if err != nil {
		t.Fatalf("load the recorded world: %v", err)
	}
	return recorded
}

// TestTheRecordedWorldHoldsTheFirstWave (T066).
func TestTheRecordedWorldHoldsTheFirstWave(t *testing.T) {
	t.Parallel()

	recorded := loadWorld(t)
	world := recorded.World()

	// The error-spans edge is keyed by canonical entity id in the recording, so the pair is read
	// out of the world itself rather than guessed: what matters is that *an* error-spans call on a
	// CALLS edge into the culprit's target was recorded.
	src, dst := callsEdge(t, world)

	missing := engine.AuditWorldForFirstWave(world, paymentsMetric, paymentsLog, src, dst)
	if len(missing) > 0 {
		t.Errorf("the recorded world does not hold the first-wave queries %s for the culprit's target; "+
			"a world that cannot answer the first wave cannot answer the investigation it was recorded for",
			strings.Join(missing, ", "))
	}
}

// TestThePublishedRulesReachTheGroundTruthOnTheRecordedWorld.
//
// The terms are taken out of the recording rather than rebuilt, because a term rebuilt by hand is
// a term whose pointer fields must match the graph's byte for byte to hash to the same key — and
// what is under test here is the judgment rules, not the test author's memory of a pointer.
func TestThePublishedRulesReachTheGroundTruthOnTheRecordedWorld(t *testing.T) {
	t.Parallel()

	recorded := loadWorld(t)
	world := recorded.World()
	ctx := context.Background()

	// The rollout's own version carries the errors: a strong support, deterministically.
	versionSplit := ask(t, ctx, recorded, findTerm(t, world, func(term *investigationv1.AlgebraTerm) bool {
		body, ok := term.GetTerm().(*investigationv1.AlgebraTerm_ErrorsByVersion)
		return ok && body.ErrorsByVersion.GetPointer().GetSelector() == paymentsMetric
	}))
	// The symptom window opens at the rollout, so rev7 is the only version in the breakdown: the
	// answer establishes that the version serving the window fails 10.7% of the time, which is a
	// moderate support. The separation it cannot provide comes from the comparison below, which is
	// exactly how the fixture's own ground truth reads the pair.
	direction, strength, ok := engine.FirstWaveVerdict("errors_by_version", versionSplit)
	if !ok || direction != ledger.Supports || strength != ledger.Moderate {
		t.Errorf("errors_by_version on the culprit's target gave %s/%s (ok=%t); the fixture's own "+
			"decisive evidence is that rev7 carries the errors", direction, strength, ok)
	}

	// The comparison across the onset moves up on the culprit's target.
	comparison := ask(t, ctx, recorded, findTerm(t, world, func(term *investigationv1.AlgebraTerm) bool {
		body, ok := term.GetTerm().(*investigationv1.AlgebraTerm_Compare)
		return ok && body.Compare.GetPointer().GetSelector() == paymentsMetric &&
			body.Compare.GetStatistic() == investigationv1.Statistic_ERROR_RATE &&
			body.Compare.GetWindows().GetReferenceAt().AsTime().UTC().Equal(instant(t, fixtureOnset))
	}))
	direction, strength, ok = engine.FirstWaveVerdict("compare", comparison)
	if !ok || direction != ledger.Supports {
		t.Errorf("compare(error_rate) on the culprit's target gave %s/%s (ok=%t); the fixture records "+
			"0.44%% before the onset and 10.7%% after it", direction, strength, ok)
	}

	// The coincident decoy moves too, and that is the point. storefront is two hops UPSTREAM of
	// the culprit's target along `storefront -> checkout -> payments`, and the generator
	// propagates a callee's excess to its callers, so a comparison on storefront supports it just
	// as one on payments-v2 does. A comparison alone therefore does not separate them, and the
	// fixture's ground truth says so: what exonerates the scaling is the SIZE of the movement —
	// the attenuated image is smaller than the fault it is an image of.
	decoy := ask(t, ctx, recorded, findTerm(t, world, func(term *investigationv1.AlgebraTerm) bool {
		body, ok := term.GetTerm().(*investigationv1.AlgebraTerm_Compare)
		return ok && body.Compare.GetPointer().GetSelector() == storefrontMetric &&
			body.Compare.GetStatistic() == investigationv1.Statistic_ERROR_RATE &&
			body.Compare.GetWindows().GetReferenceAt().AsTime().UTC().Equal(instant(t, fixtureOnset))
	}))
	decoyExcess := relativeDeltaOf(t, decoy)
	culpritExcess := relativeDeltaOf(t, comparison)
	if decoyExcess >= culpritExcess {
		t.Errorf("the coincident decoy's relative excess is %g and the culprit's target's is %g; the "+
			"fixture's ground truth is that storefront carries the attenuated image of payments-v2's "+
			"fault and so must read strictly smaller", decoyExcess, culpritExcess)
	}

	// And by the ORDER of the two onsets: the image begins one resolution step after the fault,
	// and a cause cannot postdate its effect.
	decoyOnset := ask(t, ctx, recorded, findTerm(t, world, func(term *investigationv1.AlgebraTerm) bool {
		body, ok := term.GetTerm().(*investigationv1.AlgebraTerm_Onset)
		return ok && body.Onset.GetPointer().GetSelector() == storefrontMetric
	})).GetDigest().GetOnset()
	if decoyOnset.GetUnavailable() || decoyOnset.GetEstimatedOnset() == nil {
		t.Errorf("no onset was estimated on the coincident decoy's target: %v; under propagation the "+
			"image has an onset of its own and the fixture grades the order of the two",
			decoyOnset.GetUnavailableReason())
	} else if got := decoyOnset.GetEstimatedOnset().AsTime().UTC(); !got.After(instant(t, fixtureOnset)) {
		t.Errorf("the decoy's onset is %s, which is not after the culprit's target's %s",
			got.Format(time.RFC3339), fixtureOnset)
	}

	// The onset the metrics worker estimates on the culprit's target is the rollout instant, which
	// is the temporal half of attribution (FR-029).
	onsetAnswer := ask(t, ctx, recorded, findTerm(t, world, func(term *investigationv1.AlgebraTerm) bool {
		body, ok := term.GetTerm().(*investigationv1.AlgebraTerm_Onset)
		return ok && body.Onset.GetPointer().GetSelector() == paymentsMetric
	}))
	estimated := onsetAnswer.GetDigest().GetOnset()
	if estimated.GetUnavailable() || estimated.GetEstimatedOnset() == nil {
		t.Fatalf("no onset was estimated on the culprit's target: %v", estimated.GetUnavailableReason())
	}
	if got := estimated.GetEstimatedOnset().AsTime().UTC(); !got.Equal(instant(t, fixtureOnset)) {
		t.Errorf("estimated onset = %s, want the rollout instant %s", got.Format(time.RFC3339), fixtureOnset)
	}
}

// relativeDeltaOf reads the one ERROR_RATE comparison's relative delta out of an answer, as a
// magnitude. It is a helper rather than an inline expression because the test compares two of
// them and the comparison is the assertion.
func relativeDeltaOf(t *testing.T, answer *investigationv1.AlgebraResponse) float64 {
	t.Helper()
	for _, comparison := range answer.GetDigest().GetMetric().GetComparisons() {
		if comparison.GetStatistic() != investigationv1.Statistic_ERROR_RATE {
			continue
		}
		delta := comparison.GetRelativeDelta()
		if delta < 0 {
			delta = -delta
		}
		return delta
	}
	t.Fatalf("the answer holds no ERROR_RATE comparison: %v", answer.GetDigest().GetMetric())
	return 0
}

// findTerm returns the one recorded term matching a predicate, failing when the world holds none.
func findTerm(t *testing.T, world worldReader, match func(*investigationv1.AlgebraTerm) bool) *investigationv1.AlgebraTerm {
	t.Helper()
	for _, key := range world.Keys() {
		term, ok := world.Term(key)
		if ok && match(term) {
			return term
		}
	}
	t.Fatal("the recorded world holds no term matching the predicate")
	return nil
}

// worldReader is the little of a recorded world these assertions read.
type worldReader interface {
	Keys() []string
	Term(string) (*investigationv1.AlgebraTerm, bool)
}

// TestANotRecordedAnswerMovesNothing: a replay asked a question its world does not hold gets
// `not_recorded`, and `not_recorded` is not a negative result (FR-027, FR-040).
func TestANotRecordedAnswerMovesNothing(t *testing.T) {
	t.Parallel()

	recorded := loadWorld(t)
	resp := ask(t, context.Background(), recorded, backend.Compare(
		&graphv1.Pointer{
			Kind:        graphv1.PointerKind_METRIC,
			BackendKind: "otel",
			Vocabulary:  "otel-semconv/1.30",
			Selector:    `service.name="nobody" AND metric.name="invented"`,
		},
		backend.NewWindowPair(instant(t, fixtureOnset), 15*time.Minute),
		investigationv1.Statistic_ERROR_RATE))

	if resp.GetOutcome() != investigationv1.TermOutcome_NOT_RECORDED {
		t.Fatalf("outcome = %s, want not_recorded", resp.GetOutcome())
	}
	if _, _, ok := engine.FirstWaveVerdict("compare", resp); ok {
		t.Error("a not_recorded answer produced a judgment; it is not a negative result")
	}

	rendered, err := engine.ToolResult(resp, "")
	if err != nil {
		t.Fatalf("tool result: %v", err)
	}
	if !strings.Contains(rendered, "not a negative result") {
		t.Errorf("the tool result does not tell the model what not_recorded means:\n%s", rendered)
	}
}

// ask runs one term against the recorded backend through the worker boundary the engine uses, so
// the coverage check and the mode stamp are exercised too.
func ask(t *testing.T, ctx context.Context, recorded *backend.Recorded, term *investigationv1.AlgebraTerm) *investigationv1.AlgebraResponse {
	t.Helper()
	term.AlgebraVersion = engine.AlgebraVersion
	resp, err := recorded.Execute(ctx, &investigationv1.AlgebraRequest{
		Term:                   term,
		ValidAt:                timestamppb.New(instant(t, "2026-09-01T14:32:00Z")),
		ObservedAt:             timestamppb.New(instant(t, "2026-09-01T14:32:00Z")),
		DiscriminatingQuestion: "a fixture assertion",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return resp
}

func instant(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return at.UTC()
}

// callsEdge finds a recorded error-spans call on a CALLS edge, which is how the fixture's own
// canonical entity ids are read back without a database.
func callsEdge(t *testing.T, world worldReader) (string, string) {
	t.Helper()
	for _, key := range world.Keys() {
		term, ok := world.Term(key)
		if !ok {
			continue
		}
		spans, ok := term.GetTerm().(*investigationv1.AlgebraTerm_ErrorSpans)
		if !ok || spans.ErrorSpans.GetEdgeType() != graphv1.EdgeType_CALLS {
			continue
		}
		return spans.ErrorSpans.GetSrcEntityId(), spans.ErrorSpans.GetDstEntityId()
	}
	t.Fatal("the recorded world holds no error_spans call on a CALLS edge")
	return "", ""
}

// silence the unused-import checker for worker, which the harness in engine_test.go uses.
var _ = worker.ModeRecorded
