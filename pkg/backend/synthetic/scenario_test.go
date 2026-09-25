// SPDX-License-Identifier: Apache-2.0

package synthetic

import (
	"math"
	"testing"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend/onset"
)

// Tests for the generator's degradation shapes and for propagation (Phase 8 Track J).
//
// The first test is the important one: the step shape is what every world recorded before Track J
// holds, and the new fields are only additive if a scenario that sets none of them produces the
// same numbers it always did. It pins those numbers rather than comparing two code paths, so a
// future edit that changes the step cannot be made to pass by changing both sides of a comparison.

var changeAt = time.Date(2026, 9, 1, 14, 7, 0, 0, time.UTC)

// chain builds checkout -> payments -> ledger with one change on ledger, and returns the scenario
// and its three entities. Nothing here has selectors that resolve to anything: these tests are
// about the arithmetic, not about the request path.
func chain(t *testing.T, change Change) (Scenario, Entity, Entity, Entity) {
	t.Helper()
	checkout := Entity{EntityID: "otel.service.name=checkout", Name: "checkout", Selectors: []string{"sel-checkout"}}
	payments := Entity{EntityID: "otel.service.name=payments", Name: "payments", Selectors: []string{"sel-payments"}}
	ledger := Entity{EntityID: "otel.service.name=ledger", Name: "ledger", Selectors: []string{"sel-ledger"}}
	scenario := Scenario{
		Seed:       "track-j",
		Start:      changeAt.Add(-time.Hour),
		End:        changeAt.Add(time.Hour),
		Resolution: time.Minute,
		Entities:   []Entity{checkout, payments, ledger},
		Edges: []Edge{
			{SrcEntityID: checkout.EntityID, DstEntityID: payments.EntityID, WeightClass: 1, Type: "CALLS"},
			{SrcEntityID: payments.EntityID, DstEntityID: ledger.EntityID, WeightClass: 2, Type: "CALLS"},
		},
		Changes: []Change{change},
	}
	return scenario, checkout, payments, ledger
}

func ledgerChange() Change {
	return Change{
		EntityID:        "k8s.change=shop/ledger@rev5",
		At:              changeAt,
		TargetEntityIDs: []string{"otel.service.name=ledger"},
		Version:         "rev5",
		Degrades:        true,
	}
}

// TestStepDegradationIsUnchanged pins the shape every world recorded before Track J holds: 1
// before the change, the package factor from the change instant onward, and the product of the
// factors where two degrading changes overlap.
func TestStepDegradationIsUnchanged(t *testing.T) {
	t.Parallel()

	scenario, _, _, ledger := chain(t, ledgerChange())
	cases := []struct {
		at   time.Time
		want float64
	}{
		{changeAt.Add(-time.Hour), 1},
		{changeAt.Add(-time.Second), 1},
		{changeAt, 24},
		{changeAt.Add(time.Minute), 24},
		{changeAt.Add(time.Hour), 24},
	}
	for _, tc := range cases {
		if got := scenario.degradationAt(ledger, tc.at); got != tc.want {
			t.Errorf("degradationAt(%s) = %v, want %v; the step shape is what every recorded world holds and it may not move",
				tc.at.Format(time.RFC3339), got, tc.want)
		}
	}

	second := ledgerChange()
	second.EntityID = "k8s.change=shop/ledger@rev6"
	second.At = changeAt.Add(10 * time.Minute)
	scenario.Changes = append(scenario.Changes, second)
	if got := scenario.degradationAt(ledger, changeAt.Add(20*time.Minute)); got != 576 {
		t.Errorf("two degrading changes gave %v, want 576 (24 x 24)", got)
	}
}

// TestRampIsMonotoneAndReachesTheFactor: a ramp starts at no degradation, never goes backwards,
// and arrives at exactly the full factor at change + RampSeconds.
func TestRampIsMonotoneAndReachesTheFactor(t *testing.T) {
	t.Parallel()

	change := ledgerChange()
	change.RampSeconds = 600
	scenario, _, _, ledger := chain(t, change)

	if got := scenario.degradationAt(ledger, changeAt); got != 1 {
		t.Errorf("at the change instant a ramp gave %v, want 1: a ramp that starts at its full factor is a step", got)
	}
	if got := scenario.degradationAt(ledger, changeAt.Add(300*time.Second)); math.Abs(got-12.5) > 1e-6 {
		t.Errorf("halfway up the ramp gave %v, want 12.5 (1 + 23 x 0.5)", got)
	}
	if got := scenario.degradationAt(ledger, changeAt.Add(600*time.Second)); got != 24 {
		t.Errorf("at the top of the ramp gave %v, want the full factor 24", got)
	}
	if got := scenario.degradationAt(ledger, changeAt.Add(2*time.Hour)); got != 24 {
		t.Errorf("past the top of the ramp gave %v, want the full factor 24", got)
	}

	previous := 0.0
	for second := -60; second <= 900; second += 15 {
		got := scenario.degradationAt(ledger, changeAt.Add(time.Duration(second)*time.Second))
		if got < previous {
			t.Fatalf("the ramp went backwards at +%ds: %v after %v", second, got, previous)
		}
		previous = got
	}
}

// TestLoadCoupledIsZeroBelowCapacity: a ceiling above demand is a change that did nothing, and a
// ceiling demand is 25 % over applies a quarter of the factor.
func TestLoadCoupledIsZeroBelowCapacity(t *testing.T) {
	t.Parallel()

	at := changeAt.Add(20 * time.Minute)
	change := ledgerChange()
	change.LoadCoupled = true
	change.Capacity = 1e9 // no demand this generator produces can reach it
	scenario, _, _, ledger := chain(t, change)

	if got := scenario.degradationAt(ledger, at); got != 1 {
		t.Errorf("a ceiling above demand degraded by %v, want 1: a pool nothing saturates refuses nothing", got)
	}

	rate := scenario.requestRateAt(ledger, at)
	if rate <= 0 {
		t.Fatalf("the generated request rate is %v", rate)
	}

	// Demand a quarter above the ceiling applies a quarter of the factor.
	scenario.Changes[0].Capacity = round6(rate / 1.25)
	if got := scenario.degradationAt(ledger, at); math.Abs(got-6.75) > 1e-3 {
		t.Errorf("demand 25%% over the ceiling degraded by %v, want 6.75 (1 + 23 x 0.25)", got)
	}

	// Demand at twice the ceiling applies the whole factor, and no more.
	scenario.Changes[0].Capacity = round6(rate / 4)
	if got := scenario.degradationAt(ledger, at); math.Abs(got-24) > 1e-3 {
		t.Errorf("demand four times the ceiling degraded by %v, want the factor 24 and not more", got)
	}
}

// TestFactorOverride: a change may state its own factor, and the default is the package constant.
func TestFactorOverride(t *testing.T) {
	t.Parallel()

	change := ledgerChange()
	change.Factor = 6
	scenario, _, _, ledger := chain(t, change)
	if got := scenario.degradationAt(ledger, changeAt.Add(time.Minute)); got != 6 {
		t.Errorf("a change declaring Factor 6 degraded by %v", got)
	}
	scenario.Changes[0].Factor = 0
	if got := scenario.degradationAt(ledger, changeAt.Add(time.Minute)); got != degradationFactor {
		t.Errorf("a change declaring no factor degraded by %v, want the package constant %v", got, float64(degradationFactor))
	}
}

// TestPropagationTwoHop is the formula, by hand.
//
//	E(v, t) = D(v, t) · (1 + Σ α · (E(c, t − Δ) − 1)),  α = 0.6, Δ = one resolution step
//
// ledger steps to 24 at the change instant. payments, one hop up, inherits 0.6 × 23 = 13.8 one
// step later, so it reads 14.8. checkout, two hops up, inherits 0.6 × 13.8 = 8.28 a further step
// later, so it reads 9.28.
func TestPropagationTwoHop(t *testing.T) {
	t.Parallel()

	scenario, checkout, payments, ledger := chain(t, ledgerChange())
	step := scenario.resolution()

	cases := []struct {
		name   string
		entity Entity
		at     time.Time
		want   float64
	}{
		{"ledger at the change instant", ledger, changeAt, 24},
		{"payments one step later", payments, changeAt.Add(step), 14.8},
		{"payments at the change instant is still clean", payments, changeAt, 1},
		{"checkout two steps later", checkout, changeAt.Add(2 * step), 9.28},
		{"checkout one step later is still clean", checkout, changeAt.Add(step), 1},
		{"checkout well after", checkout, changeAt.Add(time.Hour), 9.28},
		{"checkout before the change", checkout, changeAt.Add(-time.Minute), 1},
	}
	for _, tc := range cases {
		if got := scenario.effectiveDegradationAt(tc.entity, tc.at); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPropagationStopsAtStructuralEdges: a degradation travels along a call, not along "runs on
// the same node as".
func TestPropagationStopsAtStructuralEdges(t *testing.T) {
	t.Parallel()

	scenario, checkout, _, _ := chain(t, ledgerChange())
	scenario.Edges[1].Type = "RUNS_ON"
	if got := scenario.effectiveDegradationAt(checkout, changeAt.Add(time.Hour)); got != 1 {
		t.Errorf("a degradation travelled along a RUNS_ON edge and gave %v, want 1", got)
	}
	// An edge whose type the recorder did not state is treated as a call, which is what every
	// other part of this generator already assumes about the same slice.
	scenario.Edges[1].Type = ""
	if got := scenario.effectiveDegradationAt(checkout, changeAt.Add(time.Hour)); math.Abs(got-9.28) > 1e-9 {
		t.Errorf("an untyped edge gave %v, want 9.28", got)
	}
}

// TestPropagationTerminatesOnACycle: two services that call each other do not recurse forever.
func TestPropagationTerminatesOnACycle(t *testing.T) {
	t.Parallel()

	scenario, checkout, payments, _ := chain(t, ledgerChange())
	scenario.Edges = append(scenario.Edges, Edge{SrcEntityID: payments.EntityID, DstEntityID: checkout.EntityID, Type: "CALLS"})
	if got := scenario.effectiveDegradationAt(checkout, changeAt.Add(time.Hour)); got <= 0 {
		t.Fatalf("a cycle produced %v", got)
	}
}

// TestNoPropagationReproducesTheOldSeries is the switch: a scenario that asks for the pre-Track-J
// behaviour gets exactly the series it got before, on the caller and on the culprit alike.
func TestNoPropagationReproducesTheOldSeries(t *testing.T) {
	t.Parallel()

	scenario, checkout, _, _ := chain(t, ledgerChange())
	scenario.Propagation = NoPropagation

	if got := scenario.effectiveDegradationAt(checkout, changeAt.Add(time.Hour)); got != 1 {
		t.Errorf("with propagation off the caller degraded by %v, want 1", got)
	}

	backend, err := New(scenario)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	window := engine.NewWindow(scenario.Start, scenario.End)
	points := backend.series(checkout, "sel-checkout", window, investigationv1.Statistic_ERROR_RATE)
	if len(points) == 0 {
		t.Fatalf("no points")
	}
	base := round6(baselineErrorRateFloor + baselineErrorRateSpan*unitOf(scenario.Seed, "sel-checkout", "error"))
	for _, pt := range points {
		want := round6(base * round6(1+wobbleOf(scenario.Seed, "sel-checkout", pt.At)))
		if pt.Value != want {
			t.Fatalf("at %s the caller's series is %v, want the undegraded %v", pt.At.Format(time.RFC3339), pt.Value, want)
		}
	}

	// And with propagation on — the default — the same series is plainly not flat.
	scenario.Propagation = PropagateAttenuated
	propagating, err := New(scenario)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	moved := propagating.series(checkout, "sel-checkout", window, investigationv1.Statistic_ERROR_RATE)
	var rose int
	for i, pt := range moved {
		if pt.Value > points[i].Value*2 {
			rose++
		}
	}
	if rose == 0 {
		t.Fatalf("with propagation on the alerting service's series never moved; onset on the subject is then an estimate over noise")
	}
}

// TestDiurnalShape: off by default, and a sine peaking where it says it does when it is on.
func TestDiurnalShape(t *testing.T) {
	t.Parallel()

	scenario, _, _, _ := chain(t, ledgerChange())
	for hour := range 24 {
		at := time.Date(2026, 9, 1, hour, 0, 0, 0, time.UTC)
		if got := scenario.diurnalAt(at); got != 1 {
			t.Fatalf("with no amplitude the day's shape at %02d:00 is %v, want 1 — every series recorded before Track J depends on it", hour, got)
		}
	}

	scenario.DiurnalAmplitude = 0.6
	if got := scenario.diurnalAt(time.Date(2026, 9, 1, 16, 0, 0, 0, time.UTC)); math.Abs(got-1.6) > 1e-6 {
		t.Errorf("the default peak is 16:00 UTC; got %v there, want 1.6", got)
	}
	if got := scenario.diurnalAt(time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC)); math.Abs(got-0.4) > 1e-6 {
		t.Errorf("the trough is twelve hours from the peak; got %v at 04:00, want 0.4", got)
	}
	// The two instants slow-burn-01 turns on, stated so that the fixture's prose can be checked
	// against a number rather than against a story.
	if got := scenario.diurnalAt(time.Date(2026, 9, 1, 11, 32, 0, 0, time.UTC)); math.Abs(got-1.234439) > 1e-6 {
		t.Errorf("demand at 11:32 is %v, want 1.234439 x baseline", got)
	}
	if got := scenario.diurnalAt(time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)); math.Abs(got-1.556310) > 1e-6 {
		t.Errorf("demand at 14:32 is %v, want 1.556310 x baseline", got)
	}
}

// TestSlowBurnShape is the shape `slow-burn-01` describes in prose, as numbers: a ceiling that
// bites mildly when it is lowered at 11:32 and four times harder when the page arrives at 14:32.
func TestSlowBurnShape(t *testing.T) {
	t.Parallel()

	shape, ok := ShapeByName(ShapeSlowBurn)
	if !ok {
		t.Fatalf("ShapeByName(%q) found nothing; the published names are %v", ShapeSlowBurn, ShapeNames())
	}

	change := ledgerChange()
	change.At = time.Date(2026, 9, 1, 11, 32, 0, 0, time.UTC)
	scenario, _, _, ledger := chain(t, change)
	scenario.Start = time.Date(2026, 9, 1, 10, 30, 0, 0, time.UTC)
	scenario.End = time.Date(2026, 9, 1, 14, 40, 0, 0, time.UTC)
	shape.ApplyTo(&scenario)

	if !scenario.Changes[0].LoadCoupled || scenario.Changes[0].CapacityFraction != 1.10 {
		t.Fatalf("the shape did not reach the degrading change: %+v", scenario.Changes[0])
	}
	if scenario.DiurnalAmplitude != 0.6 {
		t.Fatalf("the shape did not reach the scenario: amplitude %v", scenario.DiurnalAmplitude)
	}

	before := scenario.degradationAt(ledger, change.At.Add(-time.Minute))
	atChange := scenario.degradationAt(ledger, change.At)
	atAlert := scenario.degradationAt(ledger, time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC))

	if before != 1 {
		t.Errorf("before the ConfigMap edit the pool refused %v, want 1", before)
	}
	if atChange < 3 || atChange > 5 {
		t.Errorf("at 11:32 the degradation is %v, want about 3.8: the pool bites at once, but mildly", atChange)
	}
	if atAlert < 9 || atAlert > 12 {
		t.Errorf("at 14:32 the degradation is %v, want about 10.5", atAlert)
	}
	if atAlert <= atChange*2 {
		t.Errorf("the burn did not grow: %v at 11:32 and %v at 14:32", atChange, atAlert)
	}
}

// TestShapeRegistry: the four published names exist, an unknown name is refused rather than
// silently defaulted, and a shape leaves a non-degrading change alone.
func TestShapeRegistry(t *testing.T) {
	t.Parallel()

	want := []string{ShapeAdjacentDecoy, ShapeDistantCulprit, ShapeSlowBurn, ShapeStep}
	got := ShapeNames()
	if len(got) != len(want) {
		t.Fatalf("ShapeNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ShapeNames() = %v, want %v", got, want)
		}
	}
	if _, ok := ShapeByName("no-such-shape"); ok {
		t.Errorf("an unknown shape name was accepted; a manifest asking for a shape this generator cannot produce must fail the recording")
	}
	for _, name := range got {
		shape, ok := ShapeByName(name)
		if !ok || shape.Name != name || shape.Summary == "" {
			t.Errorf("shape %q is incomplete: %+v", name, shape)
		}
	}

	decoy := ledgerChange()
	decoy.Degrades = false
	scenario, _, _, _ := chain(t, decoy)
	slowBurn, _ := ShapeByName(ShapeSlowBurn)
	slowBurn.ApplyTo(&scenario)
	if scenario.Changes[0].LoadCoupled {
		t.Errorf("a shape reached a change that does not degrade; a decoy with a ceiling on it is a number in the recording that never does anything")
	}
}

// TestDegradationIsDeterministic: the same question asked many times gives the same answer, which
// is what a byte-identical re-record rests on.
func TestDegradationIsDeterministic(t *testing.T) {
	t.Parallel()

	change := ledgerChange()
	change.LoadCoupled = true
	change.CapacityFraction = 1.10
	scenario, checkout, _, _ := chain(t, change)
	scenario.DiurnalAmplitude = 0.6

	at := changeAt.Add(37 * time.Minute)
	want := scenario.effectiveDegradationAt(checkout, at)
	for range 256 {
		if got := scenario.effectiveDegradationAt(checkout, at); got != want {
			t.Fatalf("the same instant gave %v then %v", want, got)
		}
	}
}

// TestOnsetOnTheSubjectFindsThePropagatedSymptom is J1 and J2 and J3 meeting: with propagation on,
// the alerting service's own series moves when its dependency two hops down burns, and the
// estimator — now that it refuses crossings that are not level shifts — finds that movement where
// it actually is rather than in the noise forty minutes earlier.
//
// This is the whole point of Track J stated as one assertion. `onset` is the first telemetry term
// every investigation asks, it is asked on the SUBJECT's pointer, and before this the subject's
// series was flat by construction.
func TestOnsetOnTheSubjectFindsThePropagatedSymptom(t *testing.T) {
	t.Parallel()

	scenario, checkout, _, _ := chain(t, ledgerChange())
	backend, err := New(scenario)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	window := engine.NewWindow(scenario.Start, scenario.End)
	series := backend.series(checkout, "sel-checkout", window, investigationv1.Statistic_ERROR_RATE)

	got := onset.EstimateOnset(series, onset.Window{Start: scenario.Start, End: scenario.End}, onset.Params{})
	if got.Unavailable {
		t.Fatalf("onset on the alerting service is %q; with propagation on its series is no longer flat and the estimate must land", got.Reason)
	}

	// The symptom reaches checkout two resolution steps after it starts on ledger, so that — not
	// the change instant — is what an honest estimate finds.
	want := changeAt.Add(2 * scenario.resolution())
	if delta := got.At.Sub(want); delta < -5*time.Minute || delta > 5*time.Minute {
		t.Errorf("onset on the subject = %s, want within five minutes of %s (off by %s)",
			got.At.Format(time.RFC3339), want.Format(time.RFC3339), delta)
	}
	if got.Confidence <= 0 {
		t.Errorf("confidence = %v on a real 9.28x propagated shift", got.Confidence)
	}
}
