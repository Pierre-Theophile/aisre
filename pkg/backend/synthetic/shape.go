// SPDX-License-Identifier: Apache-2.0

package synthetic

import "sort"

// Named degradation shapes (Phase 8 Track J).
//
// A scenario is built from the fixture's own graph — `buildScenario` in internal/cli reads the
// entities, the edges and the changes from 001's published RPCs, and nothing about the *shape* of
// a degradation is inventable from a graph. Which shape a fixture wants is a property of the
// story the fixture tells, and the three adversarial fixtures tell three different ones.
//
// So the shapes are named here, as Go values, and a recorder stamps one onto the scenario it
// built. That keeps the manifest contract unchanged and additive: a manifest that names no shape
// gets `step`, which is byte-identical to what this generator produced before Track J except for
// the propagation every scenario now gets. A manifest that names one gets the shape without a
// single number moving out of this package into YAML, where it would be tuned until a fixture
// passed.

// Shape is a named degradation shape: the per-change parameters it stamps onto every degrading
// change, and the scenario-level ones it stamps onto the scenario.
type Shape struct {
	// Name is the published name, as a manifest would spell it.
	Name string
	// Summary is one sentence a reviewer can check the fixture's prose against.
	Summary string

	// Factor overrides the package degradation factor. Zero keeps it.
	Factor float64
	// RampSeconds is the per-change ramp. Zero is a step.
	RampSeconds int64
	// LoadCoupled couples the factor to demand.
	LoadCoupled bool
	// CapacityFraction is the ceiling as a fraction of the target's baseline request rate.
	CapacityFraction float64

	// Propagation is the scenario's propagation mode.
	Propagation PropagationMode
	// DiurnalAmplitude is the scenario's daily demand shape.
	DiurnalAmplitude float64
}

// The published shape names.
const (
	// ShapeStep is the default: a step to the package factor at the change instant, propagated to
	// callers. It is what a rollout regression is, and it is what every fixture recorded before
	// Track J holds.
	ShapeStep = "step"
	// ShapeSlowBurn is `slow-burn-01`'s: a ceiling lowered onto a service whose demand then
	// climbs into it over the afternoon.
	ShapeSlowBurn = "slow-burn"
	// ShapeDistantCulprit is `distant-culprit-01`'s.
	ShapeDistantCulprit = "distant-culprit"
	// ShapeAdjacentDecoy is `adjacent-decoy-01`'s.
	ShapeAdjacentDecoy = "adjacent-decoy"
)

// shapes is the registry, keyed by published name.
//
// The three adversarial fixtures map onto it like this, and the mapping is the honest one rather
// than the flattering one:
//
//   - `slow-burn-01` — "a ConfigMap edit lowers payments' connection-pool ceiling from 64 to 8 …
//     eight connections are enough for the late-morning request rate, and the pool only starts
//     refusing work as the afternoon traffic climbs against it". That is a **load-coupled**
//     degradation under a **rising demand curve**, and it needed both halves: a ceiling, and
//     something for demand to do. With the published numbers below, payments' error rate is
//     3.81× baseline when the ConfigMap lands at 11:32 and 10.54× when the page arrives at 14:32
//     — a burn that starts small and is four times worse three hours later, which is what an
//     operator not noticing for three hours actually looks like.
//   - `distant-culprit-01` — "fx-rates … is rolled out to revision 4 at 14:12 and starts failing
//     … the errors simply travel up the chain". That is a plain step, and what the fixture was
//     missing was never a shape: it was the travelling. It is `step` with propagation, which is
//     now the default, and it is listed separately only so that a manifest can name what it is
//     relying on.
//   - `adjacent-decoy-01` — the ledger rollout at 14:07 is a step too. What that fixture needed
//     was for checkout's own series to move, so that `onset` on the subject finds 14:07 instead
//     of a crossing in flat noise at 13:27. Again: propagation, not a shape.
//
// Two of the three therefore share their parameters with `step`. Saying so is more useful than
// inventing a difference to justify three entries.
var shapes = map[string]Shape{
	ShapeStep: {
		Name:        ShapeStep,
		Summary:     "a step to the package factor at the change instant, propagated to callers",
		Propagation: PropagateAttenuated,
	},
	ShapeSlowBurn: {
		Name: ShapeSlowBurn,
		Summary: "a ceiling lowered onto the target, which demand then climbs into over the " +
			"afternoon: invisible at or below the ceiling, and worse in proportion to the excess above it",
		LoadCoupled: true,
		// A pool sized ten per cent above the target's baseline demand. Under the amplitude
		// below, demand is already 23.4 % above baseline at 11:32 and 55.6 % above it at 14:32,
		// so the ceiling bites from the instant it is lowered — mildly — and goes on biting
		// harder for three hours.
		CapacityFraction: 1.10,
		Propagation:      PropagateAttenuated,
		// Six tenths is a working day's shape: a trough around 04:00 UTC and a peak at 16:00.
		DiurnalAmplitude: 0.6,
	},
	ShapeDistantCulprit: {
		Name: ShapeDistantCulprit,
		Summary: "a step at the far end of a call chain, which reaches the alerting service " +
			"attenuated once per hop and one resolution step later per hop",
		Propagation: PropagateAttenuated,
	},
	ShapeAdjacentDecoy: {
		Name: ShapeAdjacentDecoy,
		Summary: "a step two hops out, propagated, so that the alerting service's own series " +
			"moves and `onset` on the subject has something real to find",
		Propagation: PropagateAttenuated,
	},
}

// ShapeByName returns the named shape. A name the registry does not hold is reported rather than
// silently defaulted: a manifest that asks for a shape this generator cannot produce should fail
// the recording, not record a different fixture under the same name.
func ShapeByName(name string) (Shape, bool) {
	shape, ok := shapes[name]
	return shape, ok
}

// ShapeNames returns the published names, sorted.
func ShapeNames() []string {
	out := make([]string, 0, len(shapes))
	for name := range shapes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ApplyTo stamps the shape onto a scenario: the scenario-level parameters onto the scenario, and
// the per-change ones onto every **degrading** change. A change that does not degrade is left
// alone, because a decoy with a ramp on it is still a decoy and the ramp would be a number in the
// recording that never does anything.
//
// Fields a change already sets for itself win: the shape is a default, not an override, so a
// scenario can name a shape and still say one thing differently.
func (sh Shape) ApplyTo(scenario *Scenario) {
	if scenario == nil {
		return
	}
	scenario.Propagation = sh.Propagation
	if scenario.DiurnalAmplitude == 0 {
		scenario.DiurnalAmplitude = sh.DiurnalAmplitude
	}
	for i := range scenario.Changes {
		change := &scenario.Changes[i]
		if !change.Degrades {
			continue
		}
		if change.Factor == 0 {
			change.Factor = sh.Factor
		}
		if change.RampSeconds == 0 {
			change.RampSeconds = sh.RampSeconds
		}
		if !change.LoadCoupled {
			change.LoadCoupled = sh.LoadCoupled
		}
		if change.CapacityFraction == 0 {
			change.CapacityFraction = sh.CapacityFraction
		}
	}
}
