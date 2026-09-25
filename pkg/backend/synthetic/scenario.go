// SPDX-License-Identifier: Apache-2.0

// Package synthetic is a telemetry backend that generates deterministic series, logs and spans
// from a fixture's own graph, so that a world can be recorded — and therefore so that the
// investigation engine can be built and evaluated — before any vendor connector exists.
//
// # It is a stand-in, and it says so
//
// This backend answers the published algebra with the published digests, under the published
// redaction policy, priced with the published cost classes. What it does *not* do is observe
// production: it derives its answers from the change nodes and the topology the fixture's event
// log already contains. It is the telemetry a reader of `rollout-regression-01` would expect to
// see given what that fixture says happened, not the telemetry anybody saw.
//
// That is a deliberate, temporary arrangement with a published end date. **Feature 003's GCP
// backend records the first real world**; feature 005's Datadog backend records the second. Once
// either exists, a fixture recorded against this backend is re-recorded against that one and its
// provenance moves from `synthetic` to `recorded` (contracts/incident-format.md
// §ground_truth.provenance). Until then, every fixture whose world came from here carries
// `provenance: synthetic`, and no accuracy claim that rests on such a fixture may be published
// as though it rested on a recording — which is exactly what the provenance field is for.
//
// # Why a generator rather than hand-written digests
//
// Hand-written digests would drift from the graph the moment anybody edited a fixture's events,
// and the drift would be silent: the world would still load, still replay, and still describe a
// topology that no longer exists. Deriving the telemetry from the graph makes the two move
// together, and makes "this fixture's telemetry agrees with this fixture's events" a property of
// the recorder rather than a review item.
//
// # Determinism
//
// Every value is a pure function of (scenario seed, selector, instant). There is no clock, no
// randomness that is not seeded, and no map iteration in any ordering-sensitive path. A
// re-record of an unchanged fixture is byte-identical, which is what T039 requires.
package synthetic

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"time"
)

// Version is the generator's own version, recorded in every digest it produces. It moves when
// the arithmetic moves, which invalidates every world recorded from it.
const Version = "0.1.0"

// Vendor is the vendor name this backend declares. It is deliberately not the name of any real
// vendor: a reader of `backend list` must be able to see at a glance that these answers were
// generated rather than observed.
const Vendor = "synthetic"

// Change is one change the fixture's graph holds: what changed, when, what it targets and the
// version it rolled out. The recorder reads these from the graph's ranked changes, so the
// telemetry and the topology cannot disagree.
type Change struct {
	// EntityID is the change node's entity id, e.g. `k8s.change=shop/payments@rev7`.
	EntityID string
	// At is when the change became valid.
	At time.Time
	// TargetEntityIDs are the entities it changed.
	TargetEntityIDs []string
	// Version is the version it rolled out, as it appears in a `version` join key.
	Version string
	// Degrades says whether this change makes its targets worse. A fixture's culprit degrades;
	// its decoys do not, which is what makes a decoy a decoy rather than a second culprit.
	Degrades bool

	// ---- degradation shape (Phase 8 Track J) -------------------------------------------------
	//
	// The fields below describe *how* a degrading change makes its targets worse. Their
	// zero values are the shape this generator has always produced — a step to the package
	// factor at the change instant — so a scenario that sets none of them is byte-identical to
	// one recorded before they existed.

	// Factor overrides degradationFactor for this change: how much worse it makes its targets
	// once it is fully in effect. Zero takes the package constant.
	Factor float64
	// RampSeconds grows the degradation linearly from 1x at At to the full factor at
	// At+RampSeconds. Zero is a step, which is the shape a rollout regression has and the shape
	// the onset estimator is specified against. A ramp is the shape a saturating resource has:
	// the change lands, and the effect arrives over the following minutes or hours.
	RampSeconds int64
	// LoadCoupled makes the factor a function of demand rather than of elapsed time alone: the
	// change is invisible while the generated request rate sits at or below the ceiling, and
	// bites in proportion to the relative excess above it. It is what a connection-pool ceiling
	// actually is — a pool of eight refuses work only once concurrent demand exceeds eight.
	LoadCoupled bool
	// Capacity is that ceiling, in generated requests per second. It is used when it is
	// positive; otherwise the ceiling is CapacityFraction of the target's own baseline request
	// rate, which is how a shape preset states a ceiling without knowing which entity it will be
	// applied to.
	Capacity float64
	// CapacityFraction is the ceiling as a fraction of the target's baseline request rate, used
	// when Capacity is not positive. Zero takes 1.0 — a ceiling exactly at baseline demand.
	CapacityFraction float64
}

// factorOf is the multiplier this change applies once it is fully in effect.
func (c Change) factorOf() float64 {
	if c.Factor > 0 {
		return c.Factor
	}
	return degradationFactor
}

// rampFractionAt is how much of the full factor elapsed time has brought into effect: 0 before
// the change, 1 at or after At+RampSeconds, and linear in between. A change with no ramp is a
// step and returns 1 from the instant it lands.
func (c Change) rampFractionAt(at time.Time) float64 {
	if c.At.After(at) {
		return 0
	}
	if c.RampSeconds <= 0 {
		return 1
	}
	fraction := round6(at.Sub(c.At).Seconds() / float64(c.RampSeconds))
	if fraction >= 1 {
		return 1
	}
	if fraction < 0 {
		return 0
	}
	return fraction
}

// Entity is one node of the fixture's graph the backend can be asked about.
type Entity struct {
	// EntityID is the graph's entity id.
	EntityID string
	// Name is the service or workload name, used in generated log templates and span
	// operations.
	Name string
	// Selectors are the pointer selectors that resolve to this entity, from the graph's own
	// pointers. A request naming any of them is a request about this entity.
	Selectors []string
	// BaselineVersion is the version in force before any change in the window.
	BaselineVersion string
}

// primarySelector is the selector an entity-level quantity — its demand, say — is derived from.
// Selectors are sorted by the recorder, so the first one is a stable choice and the answer does
// not depend on which selector a caller happened to ask about.
func (e Entity) primarySelector() string {
	if len(e.Selectors) > 0 {
		return e.Selectors[0]
	}
	return e.EntityID
}

// Edge is one edge of the fixture's graph, which `error_spans` is asked about.
type Edge struct {
	// SrcEntityID and DstEntityID name the edge.
	SrcEntityID string
	// DstEntityID is the callee.
	DstEntityID string
	// WeightClass is 001's traffic weight class, 1 (heaviest) to 5. It scales the generated
	// span counts, so a heavy path produces more spans than a light one.
	WeightClass uint32
	// Type is the graph's edge type, as its enum name: `CALLS`, `RUNS_ON`, `CHANGED_BY` and the
	// rest. It is optional and additive: a recorder that does not set it gets the generator's
	// long-standing view, in which every edge it was handed is a call edge — which is already
	// what `error_spans` and `weightOf` assume about the same slice. A recorder that does set it
	// keeps degradation off the structural edges, which is what it is for.
	Type string
}

// propagates says whether a degradation on the callee travels along this edge to the caller. A
// call is a dependency on an answer, so a failing callee shows up on its caller; running on the
// same node, or having been changed by the same change, is not.
func (e Edge) propagates() bool {
	switch e.Type {
	case "", "CALLS", "DEPENDS_ON":
		return true
	default:
		return false
	}
}

// Scenario is everything the generator needs: the fixture's clock, its entities, its edges and
// its changes. The recorder builds it from the graph; nothing in it is invented here.
type Scenario struct {
	// Seed makes one fixture's telemetry differ from another's while keeping each one stable.
	// It is the fixture id.
	Seed string
	// Start and End bound the generated series.
	Start time.Time
	// End is exclusive.
	End time.Time
	// Resolution is the sample interval. Sixty seconds is the default and what every fixture
	// in this repository uses.
	Resolution time.Duration
	// Entities are the nodes, by entity id.
	Entities []Entity
	// Edges are the call edges.
	Edges []Edge
	// Changes are the changes the graph holds.
	Changes []Change
	// IngestionLag is the lag the generated coverage blocks report, so that a fixture can
	// exercise NOT_YET_INGESTED without a vendor.
	IngestionLag time.Duration

	// ---- realism (Phase 8 Track J) ------------------------------------------------------------

	// Propagation says whether a degradation travels from a changed service to the services that
	// call it. The zero value is the published default — it does — because that is what the real
	// world does and because a subject whose own series is flat while its dependency burns makes
	// the `onset` term on that subject meaningless.
	Propagation PropagationMode
	// DiurnalAmplitude shapes the generated request rate over the UTC day: the rate is multiplied
	// by `1 + DiurnalAmplitude·sin(2π·(t − peak + 6h)/24h)`, so it peaks at
	// DiurnalPeakSecondsOfDay and troughs twelve hours away. Zero — the default — is the flat
	// demand this generator has always produced, and leaves every existing series untouched.
	//
	// It exists so that a load-coupled change has something to be coupled *to*: a ceiling under
	// flat demand is either always breached or never breached, and neither is a slow burn.
	DiurnalAmplitude float64
	// DiurnalPeakSecondsOfDay is the second of the UTC day demand peaks at. Zero takes
	// diurnalDefaultPeakSeconds, 16:00 UTC.
	DiurnalPeakSecondsOfDay int64
}

// PropagationMode says how a degradation on a callee shows up on its callers.
type PropagationMode int

const (
	// PropagateAttenuated is the published default and the zero value: a degradation travels
	// along call edges to the callers of the entity it started on, attenuated once per hop and
	// delayed by one resolution step per hop.
	PropagateAttenuated PropagationMode = iota
	// NoPropagation is the generator's pre-Track-J behaviour: only the entities a change targets
	// degrade, and a caller of a burning dependency reads flat. It is kept as a switch so that a
	// scenario can ask for the old series deliberately rather than by omission.
	NoPropagation
)

// resolution returns the sample interval, defaulting to a minute.
func (s Scenario) resolution() time.Duration {
	if s.Resolution <= 0 {
		return time.Minute
	}
	return s.Resolution
}

// entityOfSelector resolves a pointer selector to the entity it names. A selector the fixture's
// graph never published resolves to nothing, and the backend answers NO_DATA naming the absent
// source — the same answer a real backend gives for a selector its vendor does not know.
func (s Scenario) entityOfSelector(selector string) (Entity, bool) {
	for _, entity := range s.Entities {
		for _, candidate := range entity.Selectors {
			if candidate == selector {
				return entity, true
			}
		}
	}
	return Entity{}, false
}

func (s Scenario) entityByID(id string) (Entity, bool) {
	for _, entity := range s.Entities {
		if entity.EntityID == id {
			return entity, true
		}
	}
	return Entity{}, false
}

// changesAffecting returns the changes targeting an entity, earliest first. Ties break on the
// change's own entity id, so the ordering is total and the generated series is stable.
func (s Scenario) changesAffecting(entityID string) []Change {
	out := make([]Change, 0, 2)
	for _, change := range s.Changes {
		for _, target := range change.TargetEntityIDs {
			if target == entityID {
				out = append(out, change)
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].EntityID < out[j].EntityID
	})
	return out
}

// versionAt is the version in force for an entity at an instant: the latest change targeting it
// at or before that instant, or the entity's baseline version.
func (s Scenario) versionAt(entity Entity, at time.Time) string {
	version := entity.BaselineVersion
	for _, change := range s.changesAffecting(entity.EntityID) {
		if !change.At.After(at) && change.Version != "" {
			version = change.Version
		}
	}
	return version
}

// degradationAt is the multiplier applied to an entity's **own** error rate at an instant by the
// degrading changes that target it: 1 before any of them, and the product of what each has
// brought into effect after.
//
// What each change has brought into effect is the product of two fractions, both 1 for the
// default step shape, so a scenario that sets no shape fields gets exactly the series this
// generator produced before they existed:
//
//   - the **ramp** fraction, which elapsed time supplies: 1 at once for a step, and linear over
//     `RampSeconds` for a ramp;
//   - the **load** fraction, which demand supplies for a load-coupled change: 0 while the
//     generated request rate sits at or below the ceiling, and the relative excess above it,
//     capped at 1, once it does not.
//
// A fraction f of a factor F is `1 + (F − 1)·f`, so a fraction of 0 is no degradation at all and
// a fraction of 1 is the whole of it.
//
// This is the entity's own degradation. What a caller of a degraded service reads is
// effectiveDegradationAt, which adds the propagation.
func (s Scenario) degradationAt(entity Entity, at time.Time) float64 {
	factor := 1.0
	for _, change := range s.changesAffecting(entity.EntityID) {
		if !change.Degrades {
			continue
		}
		fraction := change.rampFractionAt(at)
		if change.LoadCoupled {
			fraction = round6(fraction * s.loadFractionAt(entity, change, at))
		}
		if fraction <= 0 {
			continue
		}
		factor = round6(factor * round6(1+(change.factorOf()-1)*fraction))
	}
	return factor
}

// loadFractionAt is how much of a load-coupled change's factor demand has brought into effect at
// an instant: 0 while the generated request rate is at or below the ceiling, and the relative
// excess above it, capped at 1 — so demand at twice the ceiling applies the whole factor.
//
// The ceiling is `Capacity` requests per second where the change states one, and
// `CapacityFraction` of the target's own baseline request rate where it does not. The second
// form is what lets a named shape state "a pool sized ten per cent above today's demand" without
// knowing which entity it will be stamped onto.
func (s Scenario) loadFractionAt(entity Entity, change Change, at time.Time) float64 {
	capacity := change.Capacity
	if capacity <= 0 {
		fraction := change.CapacityFraction
		if fraction <= 0 {
			fraction = 1
		}
		capacity = round6(s.baselineRateOf(entity) * fraction)
	}
	if capacity <= 0 {
		return 1
	}
	rate := s.requestRateAt(entity, at)
	if rate <= capacity {
		return 0
	}
	excess := round6(round6(rate-capacity) / capacity)
	if excess > 1 {
		return 1
	}
	return excess
}

// baselineRateOf is an entity's baseline request rate: the same deterministic function of the
// seed and the entity's primary selector the RATE statistic is generated from.
func (s Scenario) baselineRateOf(entity Entity) float64 {
	return round6(baselineRateFloor + baselineRateSpan*unitOf(s.Seed, entity.primarySelector(), "rate"))
}

// requestRateAt is the generated demand on an entity at an instant: its baseline rate, the
// instant's seeded wobble, and the day's shape. With DiurnalAmplitude at its default of zero the
// last term is 1 and this is exactly the RATE statistic the backend has always generated.
func (s Scenario) requestRateAt(entity Entity, at time.Time) float64 {
	base := s.baselineRateOf(entity)
	wobble := wobbleOf(s.Seed, entity.primarySelector(), at)
	return round6(round6(base*round6(1+wobble)) * s.diurnalAt(at))
}

// diurnalAt is the day's shape: 1 everywhere when DiurnalAmplitude is zero, and a sine peaking at
// DiurnalPeakSecondsOfDay otherwise. The sine is rounded before it is used, like every other
// accumulation here, so no architecture can fuse its way to a different sixth decimal.
func (s Scenario) diurnalAt(at time.Time) float64 {
	if s.DiurnalAmplitude == 0 {
		return 1
	}
	peak := s.DiurnalPeakSecondsOfDay
	if peak <= 0 {
		peak = diurnalDefaultPeakSeconds
	}
	utc := at.UTC()
	second := int64(utc.Hour()*3600 + utc.Minute()*60 + utc.Second())
	// The quarter-day offset puts the sine's maximum at `peak` rather than a quarter of a day
	// after it.
	phase := float64((((second-peak+secondsPerDay/4)%secondsPerDay)+secondsPerDay)%secondsPerDay) / float64(secondsPerDay)
	return round6(1 + s.DiurnalAmplitude*round6(math.Sin(2*math.Pi*phase)))
}

// effectiveDegradationAt is the multiplier an entity's error rate actually carries at an instant:
// its own degradation, multiplied by what its callees' degradation does to it.
//
// The published formula (research.md §Backend and estimator realism) is
//
//	E(v, t) = D(v, t) · (1 + Σ_{v → c} α · (E(c, t − Δ) − 1))
//
// where D is the entity's own degradation, the sum runs over the callees of v along propagating
// edges, α is propagationAttenuation and Δ is one resolution step. Applying it recursively gives
// a callee at h hops an attenuation of α^h and a delay of h·Δ for free, which is what a chain of
// timeouts looks like: each hop loses some of the signal and gains some of the delay.
//
// Why it exists at all: without it a service whose dependency is burning reads perfectly flat,
// which makes `onset` on the alerting subject — the first telemetry term every investigation asks
// — an estimate over noise. That is not a modelling nicety; it is the difference between a
// corpus that exercises causal ordering and one that exercises an estimator's false-positive rate.
func (s Scenario) effectiveDegradationAt(entity Entity, at time.Time) float64 {
	if s.Propagation == NoPropagation {
		return s.degradationAt(entity, at)
	}
	return s.propagatedDegradation(entity, at, 0, make(map[string]struct{}, propagationMaxHops+1))
}

// propagatedDegradation walks the call edges callee-ward, bounded by propagationMaxHops and by a
// visited set so that a cycle in the graph terminates rather than recursing forever. Callees are
// visited in sorted order, so the accumulated sum is the same on every run.
func (s Scenario) propagatedDegradation(entity Entity, at time.Time, hop int, seen map[string]struct{}) float64 {
	own := s.degradationAt(entity, at)
	if hop >= propagationMaxHops {
		return own
	}
	if _, dup := seen[entity.EntityID]; dup {
		return own
	}
	seen[entity.EntityID] = struct{}{}
	defer delete(seen, entity.EntityID)

	inherited := 0.0
	for _, calleeID := range s.calleesOf(entity.EntityID) {
		if _, dup := seen[calleeID]; dup {
			continue
		}
		callee, ok := s.entityByID(calleeID)
		if !ok {
			continue // an edge to something the scenario holds no telemetry for carries nothing
		}
		excess := round6(s.propagatedDegradation(callee, at.Add(-s.resolution()), hop+1, seen) - 1)
		if excess <= 0 {
			continue
		}
		inherited = round6(inherited + round6(propagationAttenuation*excess))
	}
	if inherited <= 0 {
		return own
	}
	return round6(own * round6(1+inherited))
}

// calleesOf is the entities an entity calls, deduplicated and sorted so the walk is total.
func (s Scenario) calleesOf(entityID string) []string {
	out := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	for _, edge := range s.Edges {
		if edge.SrcEntityID != entityID || edge.DstEntityID == entityID || !edge.propagates() {
			continue
		}
		if _, dup := seen[edge.DstEntityID]; dup {
			continue
		}
		seen[edge.DstEntityID] = struct{}{}
		out = append(out, edge.DstEntityID)
	}
	sort.Strings(out)
	return out
}

// The generator's published constants. They are constants rather than parameters so that two
// fixtures generated from the same graph shape produce comparable telemetry.
const (
	// degradationFactor is how much worse a degrading change makes its targets.
	degradationFactor = 24.0
	// baselineErrorRateFloor and baselineErrorRateSpan bound the per-entity baseline error
	// rate, which is a deterministic function of the entity id.
	baselineErrorRateFloor = 0.004
	baselineErrorRateSpan  = 0.008
	// baselineLatencyFloorMS and baselineLatencySpanMS bound the per-entity baseline p50.
	baselineLatencyFloorMS = 18.0
	baselineLatencySpanMS  = 42.0
	// baselineRateFloor and baselineRateSpan bound requests per second.
	baselineRateFloor = 30.0
	baselineRateSpan  = 90.0
	// propagationAttenuation is how much of a callee's excess error rate its caller inherits, per
	// hop. Six tenths is enough for a three-hop chain to remain visible at the top (0.6³ = 0.216
	// of the excess) and small enough that the caller's own series is plainly milder than the
	// culprit's, which is what makes "follow it down the chain" the right move rather than a
	// formality.
	propagationAttenuation = 0.6
	// propagationMaxHops bounds the callee-ward walk. Four is one more than the longest chain any
	// fixture in this corpus holds (checkout → payments → ledger → fx-rates), so the bound is a
	// termination guarantee rather than a modelling choice.
	propagationMaxHops = 4
	// diurnalDefaultPeakSeconds is 16:00 UTC, the second of the day demand peaks at when a
	// scenario asks for a daily shape without saying when.
	diurnalDefaultPeakSeconds = 57600
	// secondsPerDay is the day the diurnal shape repeats over.
	secondsPerDay = 86400
)

// unitOf is a deterministic value in [0, 1) derived from the parts. It is the generator's only
// source of variation: seeded, reproducible, and identical on every architecture because it is
// integer arithmetic until the final division.
func unitOf(parts ...string) float64 {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	n := binary.BigEndian.Uint32(sum[:4])
	return round6(float64(n%1_000_000) / 1_000_000.0)
}

// wobbleOf is the small, repeating, deterministic variation a real series has. It is a function
// of the instant, so the same instant always produces the same wobble and a re-record is
// byte-identical.
func wobbleOf(seed, selector string, at time.Time) float64 {
	return round6(unitOf(seed, selector, "wobble", at.UTC().Format(time.RFC3339))*0.08 - 0.04)
}

// round6 is the project's published rounding, applied to every generated value so a golden
// compares byte for byte.
func round6(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}
