// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The published association rule (T078, FR-008a, FR-008c).
//
// One investigation covers one *incident*, not one alert. A monitor fires; forty seconds later
// somebody declares an incident about the same outage in a channel; ninety seconds after that a
// second monitor on the neighbouring service fires too. That is one incident and it gets one
// investigation. Two investigations for one incident are a defect whichever door produced them.
//
// The rule is deliberately simple and deliberately published, because an on-call has to be able
// to predict it:
//
//	group  ⟺  |t₁ − t₂| ≤ Window  ∧  graph distance(entities₁, entities₂) ≤ Hops
//
// Whichever symptom arrives first opens the investigation; the later one attaches as an
// additional symptom carrying its own instant, origin reference and actor kind. A declaration
// that the rule does not group opens its own investigation rather than being dropped — the
// failure mode of grouping too little is a duplicate to merge, and the failure mode of grouping
// too much is an investigation about two unrelated outages.
//
// The decision itself is recorded as **evidence** (FR-008a), with the two distances it rested
// on. A reader who disagrees with a grouping can see the numbers that produced it rather than
// being told that the rule grouped them.
//
// A symptom with no resolved entity has no neighbourhood, so it can only group on time. The rule
// refuses to do that: proximity in time alone would put every alert in a busy hour into one
// incident. Such a symptom opens its own investigation, which is also what FR-002b asks for.

// AssociationRuleVersion is the published version of the rule below. It is written onto every
// incident row (`investigation.incidents.association_rule_version`) so that a historical
// grouping stays interpretable after the rule is next revised.
const AssociationRuleVersion = "1.0.0"

// Default thresholds of the published rule. They are the values the rule ships with, not
// limits: an operator may configure them, and whatever was in force is recorded on the evidence.
const (
	// DefaultGroupingWindow is the proximity in time within which two symptoms may be one
	// incident. Ten minutes covers "the monitor fired, somebody looked, somebody declared"
	// without reaching the next unrelated alert in a busy hour.
	DefaultGroupingWindow = 10 * time.Minute
	// DefaultGroupingHops is the proximity in the graph neighbourhood. One hop is "the same
	// service, or something it talks to directly".
	DefaultGroupingHops = 1
)

// Reasons a grouping decision records. They are a closed vocabulary so that a reader can count
// them across a corpus.
const (
	// ReasonAttached is a symptom that joined an incident already under investigation.
	ReasonAttached = "attached"
	// ReasonOpened is a symptom that opened a new incident because no candidate matched.
	ReasonOpened = "opened"
	// ReasonTooFarInTime is a candidate rejected on the time distance alone.
	ReasonTooFarInTime = "too_far_in_time"
	// ReasonTooFarInGraph is a candidate rejected on the neighbourhood distance.
	ReasonTooFarInGraph = "too_far_in_graph"
	// ReasonNoNeighbourhood is a symptom with no resolved entity: it cannot be grouped on
	// proximity in the graph, and time alone is not the rule.
	ReasonNoNeighbourhood = "no_neighbourhood"
)

// GroupingRule is the rule in force for one decision.
type GroupingRule struct {
	// Window is the maximum distance in time between two symptoms of one incident.
	Window time.Duration
	// Hops is the maximum distance in the graph neighbourhood.
	Hops int
	// Version is the published rule version recorded with the decision.
	Version string
}

// DefaultGroupingRule is the rule as published.
func DefaultGroupingRule() GroupingRule {
	return GroupingRule{
		Window:  DefaultGroupingWindow,
		Hops:    DefaultGroupingHops,
		Version: AssociationRuleVersion,
	}
}

func (r GroupingRule) normalised() GroupingRule {
	if r.Window <= 0 {
		r.Window = DefaultGroupingWindow
	}
	if r.Hops < 0 {
		r.Hops = DefaultGroupingHops
	}
	if r.Version == "" {
		r.Version = AssociationRuleVersion
	}
	return r
}

// OpenIncident is a candidate an arriving symptom might attach to: an incident already under
// investigation, with the instant it opened, the instant of its most recent symptom, and the
// canonical entities its symptoms resolved to.
type OpenIncident struct {
	// IncidentID is the incident's identifier.
	IncidentID string
	// InvestigationID is the investigation covering it, where one is running.
	InvestigationID string
	// OpenedAt and LastSymptomAt bound it in time.
	OpenedAt      time.Time
	LastSymptomAt time.Time
	// EntityIDs are the canonical graph entity ids its symptoms resolved to.
	EntityIDs []string
}

// Neighbourhood answers the one graph question the rule asks: how far apart are two entities?
//
// It is an interface rather than a query engine so that the rule is testable on a small
// hand-built graph and so that the engine can hand it whatever it already has loaded. A pair the
// implementation cannot reach reports ok = false, which the rule reads as "further than Hops",
// never as zero.
type Neighbourhood interface {
	// Distance returns the hop distance from a to b, and whether a path within the caller's
	// horizon exists at all.
	Distance(ctx context.Context, a, b string) (int, bool, error)
}

// SameEntityNeighbourhood is the degenerate neighbourhood: two entities are at distance 0 when
// they are the same entity and unreachable otherwise. It is what an engine with no graph handy
// passes, and what makes the rule's behaviour with no graph explicit rather than accidental.
type SameEntityNeighbourhood struct{}

// Distance implements Neighbourhood.
func (SameEntityNeighbourhood) Distance(_ context.Context, a, b string) (int, bool, error) {
	if a == b && a != "" {
		return 0, true, nil
	}
	return 0, false, nil
}

// GroupingDecision is what the rule decided and the distances it rested on. It is recorded as an
// evidence item of kind `association` (FR-008a).
type GroupingDecision struct {
	// IncidentID is the incident the symptom belongs to: the one it attached to, or the new one
	// it opened.
	IncidentID string
	// Attached is true when the symptom joined an incident already under investigation.
	Attached bool
	// InvestigationID is the investigation it attached to, empty when it opened a new incident.
	InvestigationID string
	// Reason is one of the published reasons above.
	Reason string
	// TimeDistance and GraphDistance are the two distances the decision rested on. They are set
	// for the candidate that was chosen, or for the nearest candidate that was rejected.
	TimeDistance  time.Duration
	GraphDistance int
	// GraphDistanceKnown is false when no path was found within the horizon: a rejected
	// candidate at "unknown distance" is not a candidate at distance zero.
	GraphDistanceKnown bool
	// Rule is the rule in force, recorded with the decision.
	Rule GroupingRule
	// Considered is every candidate the rule looked at, with why it was rejected. It is part of
	// the evidence: "we grouped with A" is a weaker statement than "we looked at A, B and C".
	Considered []ConsideredIncident
	// SymptomID is the symptom the decision is about.
	SymptomID string
	// EntityIDs are the canonical entities the symptom resolved to.
	EntityIDs []string
	// At is the symptom's instant.
	At time.Time
}

// ConsideredIncident is one candidate the rule examined.
type ConsideredIncident struct {
	// IncidentID is the candidate.
	IncidentID string
	// TimeDistance is |symptom instant − nearest symptom instant of the candidate|.
	TimeDistance time.Duration
	// GraphDistance is the smallest hop distance between the symptom's entities and the
	// candidate's; GraphDistanceKnown is false when no path was found.
	GraphDistance      int
	GraphDistanceKnown bool
	// Reason is why it was or was not chosen.
	Reason string
}

// Group applies the published association rule (FR-008a, FR-008c).
//
// `entityIDs` are the canonical graph entities the symptom resolved to — the output of Resolve,
// not the identifiers the alert named, because two names for one service must group.
//
// `now` is the clock the incident id is minted from when a new incident opens; it is a parameter
// so a fixture's grouping is deterministic. It may be nil, in which case the symptom's own
// instant is used, which is what the engine passes.
func Group(
	ctx context.Context,
	rule GroupingRule,
	in *Intake,
	entityIDs []string,
	open []OpenIncident,
	nb Neighbourhood,
) (GroupingDecision, error) {
	if in == nil || in.Symptom == nil {
		return GroupingDecision{}, fmt.Errorf("group: %w", ErrNoSubject)
	}
	rule = rule.normalised()
	if nb == nil {
		nb = SameEntityNeighbourhood{}
	}
	at := in.ValidAt.UTC()

	decision := GroupingDecision{
		Rule:      rule,
		SymptomID: in.Symptom.GetSymptomId(),
		EntityIDs: dedupeSorted(entityIDs),
		At:        at,
	}

	// A symptom with no resolved entity has no neighbourhood. Grouping on time alone would put
	// every alert in a busy hour into one incident, so it opens its own (FR-008c: never dropped).
	if len(decision.EntityIDs) == 0 {
		decision.IncidentID = IncidentID(in, at)
		decision.Reason = ReasonNoNeighbourhood
		return decision, nil
	}

	var best *ConsideredIncident
	var bestOpen *OpenIncident
	for i := range open {
		candidate := &open[i]
		considered, err := consider(ctx, rule, at, decision.EntityIDs, candidate, nb)
		if err != nil {
			return GroupingDecision{}, err
		}
		decision.Considered = append(decision.Considered, considered)
		if considered.Reason != ReasonAttached {
			continue
		}
		// Nearest in the graph wins; time breaks a tie. A symptom equidistant from two open
		// incidents is a genuine ambiguity, and taking the closer-in-time one is the reading an
		// on-call expects.
		if best == nil ||
			considered.GraphDistance < best.GraphDistance ||
			(considered.GraphDistance == best.GraphDistance && considered.TimeDistance < best.TimeDistance) {
			best = &decision.Considered[len(decision.Considered)-1]
			bestOpen = candidate
		}
	}

	if best == nil {
		decision.IncidentID = IncidentID(in, at)
		decision.Reason = ReasonOpened
		if nearest := nearestRejected(decision.Considered); nearest != nil {
			decision.TimeDistance = nearest.TimeDistance
			decision.GraphDistance = nearest.GraphDistance
			decision.GraphDistanceKnown = nearest.GraphDistanceKnown
		}
		return decision, nil
	}

	decision.IncidentID = bestOpen.IncidentID
	decision.InvestigationID = bestOpen.InvestigationID
	decision.Attached = true
	decision.Reason = ReasonAttached
	decision.TimeDistance = best.TimeDistance
	decision.GraphDistance = best.GraphDistance
	decision.GraphDistanceKnown = best.GraphDistanceKnown

	// The symptom carries the attachment on its own row, so a reader of the symptoms alone can
	// tell the opener from the joiners (data-model §investigation.symptoms).
	in.Symptom.AttachedAsAdditional = true
	return decision, nil
}

func consider(
	ctx context.Context,
	rule GroupingRule,
	at time.Time,
	entityIDs []string,
	candidate *OpenIncident,
	nb Neighbourhood,
) (ConsideredIncident, error) {
	out := ConsideredIncident{IncidentID: candidate.IncidentID}
	out.TimeDistance = timeDistance(at, candidate)
	if out.TimeDistance > rule.Window {
		out.Reason = ReasonTooFarInTime
		return out, nil
	}

	best, known := 0, false
	for _, a := range entityIDs {
		for _, b := range candidate.EntityIDs {
			d, ok, err := nb.Distance(ctx, a, b)
			if err != nil {
				return ConsideredIncident{}, fmt.Errorf("group: neighbourhood distance %s→%s: %w", a, b, err)
			}
			if !ok {
				continue
			}
			if !known || d < best {
				best, known = d, true
			}
		}
	}
	out.GraphDistance, out.GraphDistanceKnown = best, known
	switch {
	case !known || best > rule.Hops:
		out.Reason = ReasonTooFarInGraph
	default:
		out.Reason = ReasonAttached
	}
	return out, nil
}

// timeDistance is the distance from the arriving symptom to the candidate's nearest edge in
// time: zero while it falls inside `[opened_at, last_symptom_at]`, and the gap to the nearer end
// otherwise. An incident that has been running for an hour is still close in time to a symptom
// arriving now.
func timeDistance(at time.Time, candidate *OpenIncident) time.Duration {
	opened, last := candidate.OpenedAt.UTC(), candidate.LastSymptomAt.UTC()
	if last.Before(opened) {
		opened, last = last, opened
	}
	switch {
	case at.Before(opened):
		return opened.Sub(at)
	case at.After(last):
		return at.Sub(last)
	default:
		return 0
	}
}

func nearestRejected(considered []ConsideredIncident) *ConsideredIncident {
	var best *ConsideredIncident
	for i := range considered {
		c := &considered[i]
		if best == nil || c.TimeDistance < best.TimeDistance {
			best = c
		}
	}
	return best
}

// IncidentID is the deterministic identifier of a new incident: `sha256(first symptom's
// canonical subject, first symptom instant)` (data-model §investigation.incidents).
//
// It is derived from the symptom's idempotency key, which is already the hash of the published
// 4-tuple, so re-delivering the opening symptom re-derives the same incident id rather than
// opening a second one.
func IncidentID(in *Intake, at time.Time) string {
	key := in.Symptom.GetIdempotencyKey()
	if key == "" {
		key = in.Symptom.GetSymptomId()
	}
	return "inc:" + strings.TrimPrefix(key, "alert:") + ":" +
		fmt.Sprintf("%d", at.UTC().Unix())
}

// AssociationEvidenceID is the evidence id a grouping decision is recorded under.
func AssociationEvidenceID(symptomID string) string { return "ev-association-" + symptomID }

// EvidenceKindAssociation is the published evidence kind of a grouping decision.
const EvidenceKindAssociation = "association"

// Evidence turns the decision into the evidence item FR-008a requires, with the two distances it
// rested on in the coverage block.
//
// The term is the graph subgraph query the neighbourhood distance is an answer about: the
// association is a claim about the graph, so it cites a graph term rather than pretending to be
// a telemetry answer. `valid_at` and `observed_at` are the investigation's own instants, which
// is what Invariant 7 checks.
func (d GroupingDecision) Evidence(validAt, observedAt, calledAt time.Time) ledger.EvidenceItem {
	searched := append([]string{}, d.EntityIDs...)
	for _, c := range d.Considered {
		searched = append(searched, "incident:"+c.IncidentID)
	}
	coverage := &investigationv1.Coverage{
		SearchedEntities: searched,
		DataSource:       "investigation.incidents",
		Sampling: fmt.Sprintf("association rule %s: window %s, hops %d",
			d.Rule.Version, d.Rule.Window, d.Rule.Hops),
		Truncation:        d.summary(),
		VolumeConsidered:  int64(len(d.Considered)),
		ExecutedAt:        timestampOf(calledAt),
		QuotaUndetermined: true,
	}
	term := backend.GraphSubgraph(&graphv1.SubgraphRequest{
		Focus: firstRef(d.EntityIDs),
		Hops:  uint32(max(d.Rule.Hops, 0)), //nolint:gosec // Hops is bounded by the rule
	})
	return ledger.EvidenceItem{
		ID:         AssociationEvidenceID(d.SymptomID),
		Kind:       EvidenceKindAssociation,
		Worker:     "intake",
		Capability: "associate",
		Term:       term,
		ValidAt:    validAt.UTC(),
		ObservedAt: observedAt.UTC(),
		CalledAt:   calledAt.UTC(),
		Mode:       backend.ModeRecorded,
		Outcome:    "digest",
		Coverage:   coverage,
		// The association is a decision about the tool's own rows, not a query a person can
		// re-run against a vendor. Saying so is what FR-057d asks for when no link exists.
		DeepLinkAbsentReason: "the association rule is evaluated inside the engine; " +
			"`aisre investigate get <id> --evidence` prints the distances it rested on",
		FreeText: d.summary(),
	}
}

// summary is the one-line human form of the decision, carried on the evidence so that the
// rendering can quote it without re-deriving it.
func (d GroupingDecision) summary() string {
	var b strings.Builder
	if d.Attached {
		fmt.Fprintf(&b, "attached to incident %s", d.IncidentID)
	} else {
		fmt.Fprintf(&b, "opened incident %s (%s)", d.IncidentID, d.Reason)
	}
	if d.GraphDistanceKnown {
		fmt.Fprintf(&b, "; time distance %s, graph distance %d hop(s)", d.TimeDistance, d.GraphDistance)
	} else if len(d.Considered) > 0 {
		fmt.Fprintf(&b, "; time distance %s, graph distance unknown", d.TimeDistance)
	}
	fmt.Fprintf(&b, "; rule %s (window %s, hops %d); %d candidate(s) considered",
		d.Rule.Version, d.Rule.Window, d.Rule.Hops, len(d.Considered))
	return b.String()
}

func firstRef(entityIDs []string) *graphv1.Ref {
	if len(entityIDs) == 0 {
		return nil
	}
	// Entity ids are opaque here; the graph term carries it as the focus value so that the term
	// key is stable and the query is re-runnable by someone who holds the id.
	return &graphv1.Ref{Namespace: "entity", Value: entityIDs[0]}
}

func dedupeSorted(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
