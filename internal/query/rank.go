// SPDX-License-Identifier: Apache-2.0

package query

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// The published ranking score (FR-028, research §9).
//
// A diff that lists forty changes in arbitrary order is a worse answer than no diff at all: the
// operator still has to read all forty. Ranking them is the product thesis, so the score is
// published, fixed, and reported component by component with every item — an SRE who disagrees
// with a rank must be able to see exactly which term put it there (constitution V).
//
//	dt          = reference - change start, in whole seconds, SIGNED
//	temporal    = exp(-dt / tau)                    for dt >= 0  (the change preceded the reference)
//	            = kappa * exp(-|dt| / tau_post)     for dt <  0  (the change FOLLOWED it)
//	topological = 1 / (1 + h)             h  = hops from the focus to the nearest target
//	traffic     = (1 + w) / 6             w  = heaviest weight class on the way there
//	score       = 0.5*temporal + 0.35*topological + 0.15*traffic
//
// with published kappa = 0.5 and tau_post = tau/2 (ADR-0005 D4).
//
// Why these three and in that proportion: an incident is a coincidence in time first — nothing
// correlates with a failure like having happened a minute before it — so temporal carries half
// the weight. Topology is the check on that coincidence: a rollout of an unrelated service is
// as recent as the rollout of your dependency and must not rank with it. Traffic is a tiebreak
// with real information in it, not a third opinion: of two dependencies at the same distance,
// the one carrying a hundred times the requests is the one whose failure you would notice.
//
// The two sides of the decay are the one place the curve is not symmetric, and the asymmetry is
// deliberate. A change that happened *after* the instant you are asking about cannot have caused
// what you are asking about, but it is not irrelevant either: it may be an effect of the same
// incident, or a remediation, and an operator wants to see it. Flooring dt at 0 — which is what
// 001 shipped — gave it the *maximum* temporal weight, which is tolerable while the reference is
// the end of the window and nothing can follow it, and wrong as soon as the reference is an
// estimated symptom onset with half the window after it. So a post-reference candidate decays
// from a ceiling of kappa = 0.5 rather than 1.0, and it decays twice as fast (tau_post = tau/2),
// because "shortly after" is the only interesting case: an effect follows its cause closely, and
// something an hour later is neither cause nor effect. The four properties that follow are
// asserted as properties rather than as examples (rank_property_test.go):
//
//   - a post-reference candidate never receives the maximum temporal weight;
//   - it scores strictly below an equally distant pre-reference candidate;
//   - the score is monotone decreasing in |dt| on both sides;
//   - the caller can tell it is post-reference from the response, without recomputing anything:
//     `signed_time_distance_seconds` and `post_reference` say so. Field 7,
//     `time_distance_seconds`, keeps its floored meaning so that no 001 consumer breaks.
//
// Three further rules keep the list honest at its edges:
//
//   - A change whose target the graph cannot resolve is never dropped (FR-028, US2 scenario 3).
//     It ranks as though it were one hop beyond the furthest attached change could be —
//     `hop_cap + 1`, where hop_cap is the subgraph radius plus the change margin — and with no
//     traffic credit. It is therefore always below an attached change of the same age, and
//     always above nothing.
//   - Ties are broken by the change's entity id, ascending, and every item says so in
//     `tie_break`. Two changes at the same instant and the same distance are genuinely
//     indistinguishable to the score; what matters is that two runs agree (US2 scenario 4).
//   - The published components are rounded to six decimals, and the order is over the rounded
//     score. A golden fixture is compared byte for byte, and `math.Exp` is free to differ in the
//     last bit between architectures; a ranking that flipped between an arm64 laptop and an
//     amd64 CI runner would make the whole harness meaningless. Six decimals is far finer than
//     any distinction the formula can support and coarser than any error it can accumulate.

// Score weights, published (research §9). They sum to 1.
const (
	// WeightTemporal is how much recency counts.
	WeightTemporal = 0.5
	// WeightTopological is how much closeness in the graph counts.
	WeightTopological = 0.35
	// WeightTraffic is how much the traffic on the path counts.
	WeightTraffic = 0.15
)

// DefaultTau is the default half-life-ish constant of the temporal decay: at 30 minutes a
// change has decayed to 1/e of its weight (research §9). Callers may override it per query,
// which is what `--tau` on the command line does.
const DefaultTau = 30 * time.Minute

// The published parameters of the post-reference side of the decay (ADR-0005 D4).
const (
	// PostReferenceCeiling is kappa: the temporal weight a change that happened *at* the
	// reference instant would have if it were a hair later than it. It is the ceiling of the
	// whole post-reference side, so no change after the reference can ever outscore, on the
	// temporal term, a change of the same distance before it.
	PostReferenceCeiling = 0.5
	// PostReferenceTauDivisor makes tau_post = tau / 2: the post-reference side decays twice
	// as fast as the pre-reference side, because a plausible *effect* follows its cause
	// closely and something long after is neither cause nor effect.
	PostReferenceTauDivisor = 2
)

// MaxWeightClass is the top traffic weight class (research §8), which makes the traffic term's
// denominator 6.
const MaxWeightClass = 5

// TieBreakChangeID is the published tie-break rule, carried on every ranked item so the rule is
// part of the answer rather than of the documentation (FR-028).
const TieBreakChangeID = "change_id"

// rankPrecision is the number of decimals the published components are rounded to.
const rankPrecision = 6

// Candidate is one change the diff found, before it is scored.
//
// It is the whole input to the ranking: everything the score needs has already been measured
// against the graph, so Rank is a pure function of these values and can be tested with numbers
// worked out by hand.
type Candidate struct {
	// Change is the change node's version, as it will appear in the response.
	Change *graphv1.NodeVersion
	// TargetIDs are the canonical ids of the change's targets that lie inside the considered
	// neighbourhood, sorted. Empty for an unattached change.
	TargetIDs []string
	// Hop is the shortest hop distance from the focus to any of those targets.
	Hop uint32
	// WeightClass is the heaviest traffic class on the shortest path to the nearest target.
	WeightClass uint32
	// Unattached marks a change whose target the graph could not resolve to any node.
	Unattached bool
}

// RankParams are the knobs of one ranking, all of them published with the result.
type RankParams struct {
	// Reference is the instant proximity is measured to: T2 unless the caller names another,
	// which is how "rank against when the alert fired" is asked for.
	Reference time.Time
	// Tau is the temporal decay constant.
	Tau time.Duration
	// HopCap is the furthest an attached change's target can be — the subgraph radius plus the
	// change margin. An unattached change is ranked at HopCap + 1.
	HopCap uint32
}

// tauSeconds is the decay constant in seconds, defaulted.
func (p RankParams) tauSeconds() float64 {
	if p.Tau <= 0 {
		return DefaultTau.Seconds()
	}
	return p.Tau.Seconds()
}

// tauPostSeconds is the decay constant of the post-reference side, derived from tau and never
// configured on its own: one knob, two sides, so `--tau` cannot put the curve in a state the
// published formula does not describe.
func (p RankParams) tauPostSeconds() float64 {
	return p.tauSeconds() / PostReferenceTauDivisor
}

// TemporalWeight is the published two-sided decay, exported so that a caller — or a property
// test — can evaluate the curve without constructing a candidate.
//
// dt is the SIGNED distance in whole seconds: positive when the change preceded the reference
// instant, negative when it followed it.
func TemporalWeight(dt int64, p RankParams) float64 {
	if dt >= 0 {
		return math.Exp(-float64(dt) / p.tauSeconds())
	}
	return PostReferenceCeiling * math.Exp(float64(dt)/p.tauPostSeconds())
}

// Rank scores the candidates and returns them in the published order — score descending, then change
// entity id ascending (FR-028) — together with the candidates it **excluded** and why (003 FR-065).
//
// # The exclusion, and how it sits beside the two-sided decay
//
// These two rules look like they contradict each other and do not:
//
//	ADR-0005 D4   a change that happened *after* the reference is kept, at a decayed score. It may
//	              be an effect of the same incident, or a remediation, and an operator wants it.
//	003 FR-065    a change whose valid start is after the reference is not a candidate cause, and is
//	              excluded **for that stated reason** rather than by scoring low.
//
// The difference is whether the change has happened at all. D4's post-reference candidate is a real
// event later in the window: somebody deployed at 02:15 after the 02:10 alert, and that is worth
// seeing. FR-065's is an **announced** fact — a vendor maintenance window on 2 October, read on 17
// September — which has not happened, cannot have caused anything, and would otherwise sit in a list of
// candidate causes for an incident three weeks before it.
//
// So the exclusion is keyed on the announcement state and not on the sign of the time distance alone: a
// candidate is excluded when its change is ANNOUNCED *and* its valid interval begins after the
// reference. Everything else keeps D4's treatment, which is why asserting "post-reference candidates
// are kept, decayed" and "announced future changes are excluded" in one test suite is consistent.
//
// It is an exclusion rather than a low score because those are different answers. A ranked list with a
// vendor's October window at the bottom says "this is the least likely of the things that could have
// caused it"; an excluded change says "this could not have caused it, and here is the instant that
// settles it". An operator can act on the second. The first is how a forward-looking read gets reached
// by accident from a backward-looking one, which FR-065 exists to prevent.
func Rank(candidates []Candidate, p RankParams) ([]*graphv1.RankedChange, []*graphv1.ExcludedChange) {
	ranked := make([]*graphv1.RankedChange, 0, len(candidates))
	var excluded []*graphv1.ExcludedChange
	for _, c := range candidates {
		if reason, detail, ok := excludedFrom(c, p); ok {
			excluded = append(excluded, &graphv1.ExcludedChange{
				Change: c.Change, Reason: reason, Detail: detail,
			})
			continue
		}
		ranked = append(ranked, score(c, p))
	}
	slices.SortFunc(excluded, func(a, b *graphv1.ExcludedChange) int {
		return strings.Compare(a.GetChange().GetEntityId(), b.GetChange().GetEntityId())
	})
	slices.SortFunc(ranked, func(a, b *graphv1.RankedChange) int {
		if a.GetScore() != b.GetScore() {
			if a.GetScore() > b.GetScore() {
				return -1
			}
			return 1
		}
		return strings.Compare(a.GetChange().GetEntityId(), b.GetChange().GetEntityId())
	})
	return ranked, excluded
}

// excludedFrom decides whether a candidate is excluded rather than ranked (003 FR-065).
//
// Both conditions are required, and each is load-bearing. Without the state check, every change later
// in the window would vanish from the list, which is what ADR-0005 D4 deliberately does not do. Without
// the instant check, an announced change whose window has already begun — a maintenance that started an
// hour ago and may well be the cause — would be excluded from the one list that should contain it.
func excludedFrom(c Candidate, p RankParams) (graphv1.ExclusionReason, string, bool) {
	change := c.Change.GetChange()
	if change.GetAnnouncementState() != graphv1.AnnouncementState_ANNOUNCED {
		return graphv1.ExclusionReason_EXCLUSION_REASON_UNSPECIFIED, "", false
	}
	start := c.Change.GetValid().GetStart()
	if start == nil || !start.AsTime().After(p.Reference) {
		return graphv1.ExclusionReason_EXCLUSION_REASON_UNSPECIFIED, "", false
	}
	// The detail states both instants, so a reader can check the decision without recomputing it.
	return graphv1.ExclusionReason_EXCLUSION_ANNOUNCED_AFTER_REFERENCE,
		fmt.Sprintf("announced change valid from %s, after the reference instant %s: it has not "+
			"happened, so it is not a candidate cause (FR-065)",
			start.AsTime().UTC().Format(time.RFC3339), p.Reference.UTC().Format(time.RFC3339)),
		true
}

// score applies the published formula to one candidate.
func score(c Candidate, p RankParams) *graphv1.RankedChange {
	signed := signedTimeDistanceSeconds(c.Change, p.Reference)
	seconds := max(signed, 0)

	hop := c.Hop
	weight := c.WeightClass
	if c.Unattached {
		hop = p.HopCap + 1
		weight = 0
	}

	temporal := TemporalWeight(signed, p)
	topological := 1 / float64(1+hop)
	traffic := float64(1+weight) / float64(1+MaxWeightClass)
	total := WeightTemporal*temporal + WeightTopological*topological + WeightTraffic*traffic

	return &graphv1.RankedChange{
		Change:                    c.Change,
		Score:                     round(total),
		Temporal:                  round(temporal),
		Topological:               round(topological),
		Traffic:                   round(traffic),
		HopDistance:               hop,
		TimeDistanceSeconds:       seconds,
		Unattached:                c.Unattached,
		TargetEntityIds:           slices.Clone(c.TargetIDs),
		TieBreak:                  TieBreakChangeID,
		SignedTimeDistanceSeconds: signed,
		PostReference:             signed < 0,
		ActorKind:                 c.Change.GetChange().GetActorKind(),
	}
}

// signedTimeDistanceSeconds is how long before the reference instant the change started, in
// whole seconds, signed: negative when the change *followed* the reference (ADR-0005 D2).
//
// It is truncated towards zero rather than rounded, on both sides, so that the published
// integer is always the one the temporal term was evaluated at and an auditor recomputing the
// score by hand from the response gets the published number back.
//
// Field 7 of RankedChange, `time_distance_seconds`, remains this value floored at 0: that is
// what 001 published and what every existing consumer reads. The signed value travels beside
// it in field 11 with `post_reference` in field 12, which is what lets a caller whose reference
// instant is an estimated symptom onset — rather than the end of the window, where nothing can
// follow it — tell a candidate cause from a candidate effect.
func signedTimeDistanceSeconds(change *graphv1.NodeVersion, reference time.Time) int64 {
	start := change.GetValid().GetStart()
	if start == nil {
		return 0
	}
	return int64(reference.Sub(start.AsTime()) / time.Second)
}

// round trims a component to the published precision.
func round(v float64) float64 {
	factor := math.Pow(10, rankPrecision)
	return math.Round(v*factor) / factor
}

// RankingFormula renders the exact formula and parameters a response was ranked with, for the
// `ranking_formula` field.
//
// It is part of the answer because a ranking nobody can reproduce is a ranking nobody should
// trust: with this string, the change's valid start and the components on each item, an auditor
// can recompute every score by hand (constitution V, FR-028).
func RankingFormula(p RankParams) string {
	return fmt.Sprintf(
		"score = %.2f*temporal + %.2f*topological + %.2f*traffic; "+
			"dt = reference - change.valid.start in whole seconds, signed, published as "+
			"signed_time_distance_seconds (time_distance_seconds is dt floored at 0); "+
			"temporal = exp(-dt/tau) for dt >= 0 and kappa*exp(-|dt|/tau_post) for dt < 0, "+
			"with tau = %gs, kappa = %g and tau_post = tau/%d = %gs, so a post-reference "+
			"candidate (post_reference = true) never takes the maximum temporal weight; "+
			"topological = 1/(1+h) with h = hops from the focus to the nearest resolved target, "+
			"h = hop_cap+1 = %d for an unattached change; "+
			"traffic = (1+w)/%d with w = heaviest traffic weight class on that path, 0 for unattached "+
			"targets and for edge types that carry no class; "+
			"reference = %s; components rounded to %d decimals; "+
			"ties broken by %s ascending (research §9, ADR-0005 D4, docs/schema/ranking.md)",
		WeightTemporal, WeightTopological, WeightTraffic,
		p.tauSeconds(), PostReferenceCeiling, PostReferenceTauDivisor, p.tauPostSeconds(),
		p.HopCap+1, 1+MaxWeightClass,
		p.Reference.UTC().Format(time.RFC3339), rankPrecision, TieBreakChangeID)
}
