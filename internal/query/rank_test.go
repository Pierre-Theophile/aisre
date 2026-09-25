// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// Unit tests for the published ranking score (T039, FR-028, research §9).
//
// Every expectation here is worked out by hand in the comment above it. That is the point of
// the file: the formula is published, so a test that recomputed it in Go would only prove that
// the code agrees with itself. If one of these numbers has to change, the documentation and the
// research decision have to change with it.

// reference is the instant these tests measure proximity to, and tau the published default.
var (
	rankReference = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	rankParams    = query.RankParams{Reference: rankReference, Tau: query.DefaultTau, HopCap: 3}
)

// changeAt builds the minimum of a change node the ranking reads: its id and when it happened.
func changeAt(id string, at time.Time) *graphv1.NodeVersion {
	return &graphv1.NodeVersion{
		EntityId: id,
		Type:     graphv1.NodeType_CHANGE,
		Valid:    &graphv1.Interval{Start: timestamppb.New(at)},
		Change:   &graphv1.Change{Kind: graphv1.ChangeKind_ROLLOUT, Summary: id},
	}
}

func minutesBefore(m int) time.Time { return rankReference.Add(-time.Duration(m) * time.Minute) }

// TestScoreComponentsAreThePublishedFormula: one rollout, one hop away, twelve minutes before
// the reference, on a path whose heaviest class is 3.
//
//	dt          = 14:32 - 14:20 = 720 s
//	temporal    = exp(-720/1800) = exp(-0.4)      = 0.670320
//	topological = 1/(1+1)                         = 0.5
//	traffic     = (1+3)/6                         = 0.666667
//	score       = 0.5*0.670320046 + 0.35*0.5 + 0.15*0.666666667
//	            = 0.335160023 + 0.175 + 0.1       = 0.610160
func TestScoreComponentsAreThePublishedFormula(t *testing.T) {
	ranked, _ := query.Rank([]query.Candidate{{
		Change:      changeAt("payments-rollout", minutesBefore(12)),
		TargetIDs:   []string{"payments"},
		Hop:         1,
		WeightClass: 3,
	}}, rankParams)

	if len(ranked) != 1 {
		t.Fatalf("ranked %d candidates, want 1", len(ranked))
	}
	got := ranked[0]
	closeTo(t, "temporal", got.GetTemporal(), 0.670320)
	closeTo(t, "topological", got.GetTopological(), 0.5)
	closeTo(t, "traffic", got.GetTraffic(), 0.666667)
	closeTo(t, "score", got.GetScore(), 0.610160)
	if got.GetHopDistance() != 1 {
		t.Errorf("hop_distance = %d, want 1", got.GetHopDistance())
	}
	if got.GetTimeDistanceSeconds() != 720 {
		t.Errorf("time_distance_seconds = %d, want 720", got.GetTimeDistanceSeconds())
	}
	if got.GetTieBreak() != query.TieBreakChangeID {
		t.Errorf("tie_break = %q, want %q", got.GetTieBreak(), query.TieBreakChangeID)
	}
	if got.GetUnattached() {
		t.Error("a change with a resolved target is not unattached")
	}
}

// TestUnattachedRanksBelowAnAttachedChangeOfTheSameAge is US2 scenario 3 and the edge case
// T042 names: the two changes happened at the same instant, and the one the graph cannot place
// must come second — included, never dropped.
//
//	both: dt = 720 s, temporal = 0.670320
//	attached   h = 1          -> topological = 0.5, traffic = (1+3)/6 = 0.666667
//	           score = 0.335160023 + 0.175   + 0.1   = 0.610160
//	unattached h = hop_cap+1 = 4 -> topological = 0.2, traffic = (1+0)/6 = 0.166667
//	           score = 0.335160023 + 0.07    + 0.025 = 0.430160
func TestUnattachedRanksBelowAnAttachedChangeOfTheSameAge(t *testing.T) {
	// The unattached change sorts first by id, so a result in the right order cannot be the
	// tie-break accidentally agreeing with the score.
	ranked, _ := query.Rank([]query.Candidate{
		{Change: changeAt("aaa-unattached", minutesBefore(12)), Unattached: true},
		{Change: changeAt("zzz-attached", minutesBefore(12)), TargetIDs: []string{"payments"}, Hop: 1, WeightClass: 3},
	}, rankParams)

	if len(ranked) != 2 {
		t.Fatalf("ranked %d candidates, want 2", len(ranked))
	}
	if ranked[0].GetChange().GetEntityId() != "zzz-attached" {
		t.Errorf("first = %q, want the attached change", ranked[0].GetChange().GetEntityId())
	}
	unattached := ranked[1]
	if !unattached.GetUnattached() {
		t.Error("the second item should be flagged unattached")
	}
	closeTo(t, "unattached score", unattached.GetScore(), 0.430160)
	closeTo(t, "unattached topological", unattached.GetTopological(), 0.2)
	closeTo(t, "unattached traffic", unattached.GetTraffic(), 0.166667)
	if unattached.GetHopDistance() != rankParams.HopCap+1 {
		t.Errorf("unattached hop_distance = %d, want hop_cap+1 = %d",
			unattached.GetHopDistance(), rankParams.HopCap+1)
	}
}

// TestEqualScoresBreakOnChangeIDDeterministically is US2 scenario 4: two changes the score
// cannot separate are ordered by change id, the same way every time, and the item says which
// rule decided it.
func TestEqualScoresBreakOnChangeIDDeterministically(t *testing.T) {
	twin := func(id string) query.Candidate {
		return query.Candidate{
			Change:      changeAt(id, minutesBefore(12)),
			TargetIDs:   []string{"payments"},
			Hop:         1,
			WeightClass: 3,
		}
	}
	forward, _ := query.Rank([]query.Candidate{twin("b-change"), twin("a-change")}, rankParams)
	backward, _ := query.Rank([]query.Candidate{twin("a-change"), twin("b-change")}, rankParams)

	for _, ranked := range [][]*graphv1.RankedChange{forward, backward} {
		if ranked[0].GetScore() != ranked[1].GetScore() {
			t.Fatalf("the twins should score identically, got %v and %v",
				ranked[0].GetScore(), ranked[1].GetScore())
		}
		if ranked[0].GetChange().GetEntityId() != "a-change" {
			t.Errorf("order = %q then %q, want the lexicographically smaller id first",
				ranked[0].GetChange().GetEntityId(), ranked[1].GetChange().GetEntityId())
		}
		if ranked[0].GetTieBreak() != query.TieBreakChangeID {
			t.Errorf("tie_break = %q, want %q", ranked[0].GetTieBreak(), query.TieBreakChangeID)
		}
	}
}

// TestChangeAfterTheReferenceDecaysOnThePostReferenceCurve is D1 and D2 of ADR-0005 in one
// case: a change five minutes *after* the reference instant.
//
// Field 7 keeps the floored meaning 001 published — `time_distance_seconds` is still 0, so no
// existing consumer sees a negative number where it never could before — and the signed
// distance and the flag travel beside it.
//
//	dt        = 14:32 - 14:37 = -300 s
//	temporal  = kappa * exp(-300/tau_post) = 0.5 * exp(-300/900)
//	          = 0.5 * exp(-0.333333) = 0.5 * 0.716531 = 0.358266
//	score     = 0.5*0.358266 + 0.35*0.5 + 0.15*(1+0)/6
//	          = 0.179133 + 0.175 + 0.025             = 0.379133
func TestChangeAfterTheReferenceDecaysOnThePostReferenceCurve(t *testing.T) {
	ranked, _ := query.Rank([]query.Candidate{{
		Change:    changeAt("later", rankReference.Add(5*time.Minute)),
		TargetIDs: []string{"payments"},
		Hop:       1,
	}}, rankParams)

	if got := ranked[0].GetTimeDistanceSeconds(); got != 0 {
		t.Errorf("time_distance_seconds = %d, want 0: field 7 stays floored", got)
	}
	if got := ranked[0].GetSignedTimeDistanceSeconds(); got != -300 {
		t.Errorf("signed_time_distance_seconds = %d, want -300", got)
	}
	if !ranked[0].GetPostReference() {
		t.Error("post_reference = false for a change after the reference instant")
	}
	closeTo(t, "temporal", ranked[0].GetTemporal(), 0.358266)
	closeTo(t, "score", ranked[0].GetScore(), 0.379133)
}

// TestPostReferenceAutoscalerDoesNotRankFirst is the worked example of docs/schema/ranking.md
// §"A reference instant with changes on both sides of it", and the reason the change package
// exists at all: with the reference instant moved to the estimated symptom onset, the change
// the incident *caused* must not outrank the change that caused the incident.
//
// Onset is 14:21. The payments rollout is a minute before it; the autoscaler reacted two
// minutes after it, on a heavier edge.
//
//	rollout    dt = +60   temporal = exp(-60/1800)        = 0.967216
//	           score = 0.5*0.967216 + 0.35*0.5 + 0.15*(1+3)/6
//	                 = 0.483608 + 0.175 + 0.100000       = 0.758608
//	autoscale  dt = -120  temporal = 0.5*exp(-120/900)    = 0.437587
//	           score = 0.5*0.437587 + 0.35*0.5 + 0.15*(1+4)/6
//	                 = 0.218793 + 0.175 + 0.125000       = 0.518793
//
// Under the rule 001 shipped, where dt was floored at 0, the autoscaler's temporal term would
// have been 1.0 — the maximum — for a score of 0.800000, and it would have ranked first.
func TestPostReferenceAutoscalerDoesNotRankFirst(t *testing.T) {
	onset := time.Date(2026, 9, 1, 14, 21, 0, 0, time.UTC)
	params := query.RankParams{Reference: onset, Tau: query.DefaultTau, HopCap: 3}

	ranked, _ := query.Rank([]query.Candidate{
		{
			Change:      changeAt("a-autoscale", onset.Add(2*time.Minute)),
			TargetIDs:   []string{"storefront"},
			Hop:         1,
			WeightClass: 4,
		},
		{
			Change:      changeAt("z-rollout", onset.Add(-1*time.Minute)),
			TargetIDs:   []string{"payments"},
			Hop:         1,
			WeightClass: 3,
		},
	}, params)

	// The autoscaler sorts first by id, so an order that comes out right cannot be the
	// tie-break quietly agreeing with the score.
	if got := ranked[0].GetChange().GetEntityId(); got != "z-rollout" {
		t.Errorf("first = %q, want the pre-reference rollout", got)
	}
	closeTo(t, "rollout temporal", ranked[0].GetTemporal(), 0.967216)
	closeTo(t, "rollout score", ranked[0].GetScore(), 0.758608)
	if ranked[0].GetPostReference() {
		t.Error("the rollout preceded the reference and is not post_reference")
	}
	if got := ranked[0].GetSignedTimeDistanceSeconds(); got != 60 {
		t.Errorf("rollout signed_time_distance_seconds = %d, want 60", got)
	}

	closeTo(t, "autoscale temporal", ranked[1].GetTemporal(), 0.437587)
	closeTo(t, "autoscale score", ranked[1].GetScore(), 0.518793)
	if !ranked[1].GetPostReference() {
		t.Error("the autoscaler followed the reference and must be flagged post_reference")
	}
	if got := ranked[1].GetSignedTimeDistanceSeconds(); got != -120 {
		t.Errorf("autoscale signed_time_distance_seconds = %d, want -120", got)
	}
}

// TestActorKindIsCarriedThroughToTheRankedItem is D3's passthrough: the ranker does not derive
// an actor kind, weight by it or guess one. It carries what the change node says, and a change
// whose source said nothing keeps the zero value, which canonical serialisation omits.
func TestActorKindIsCarriedThroughToTheRankedItem(t *testing.T) {
	typed := changeAt("typed", minutesBefore(12))
	typed.GetChange().ActorKind = graphv1.ActorKind_CONTROLLER

	ranked, _ := query.Rank([]query.Candidate{
		{Change: typed, TargetIDs: []string{"storefront"}, Hop: 1},
		{Change: changeAt("untyped", minutesBefore(12)), TargetIDs: []string{"payments"}, Hop: 1},
	}, rankParams)

	byID := map[string]*graphv1.RankedChange{}
	for _, item := range ranked {
		byID[item.GetChange().GetEntityId()] = item
	}
	if got := byID["typed"].GetActorKind(); got != graphv1.ActorKind_CONTROLLER {
		t.Errorf("actor_kind = %v, want CONTROLLER", got)
	}
	if got := byID["untyped"].GetActorKind(); got != graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED {
		t.Errorf("actor_kind = %v, want ACTOR_KIND_UNSPECIFIED for a silent source", got)
	}
}

// TestTauWidensTheTemporalWindow: the decay constant is a caller's knob, and doubling it must
// make an hour-old change score as a half-hour-old one did.
//
//	dt = 3600 s, tau = 3600 s -> temporal = exp(-1) = 0.367879
func TestTauWidensTheTemporalWindow(t *testing.T) {
	params := rankParams
	params.Tau = time.Hour
	ranked, _ := query.Rank([]query.Candidate{{
		Change:    changeAt("hour-old", minutesBefore(60)),
		TargetIDs: []string{"payments"},
		Hop:       1,
	}}, params)

	closeTo(t, "temporal", ranked[0].GetTemporal(), 0.367879)
}

// TestRankingFormulaPublishesItsParameters: the formula string is evidence, so it has to name
// the numbers this particular answer was ranked with (constitution V).
func TestRankingFormulaPublishesItsParameters(t *testing.T) {
	formula := query.RankingFormula(rankParams)
	for _, want := range []string{
		"0.50*temporal", "0.35*topological", "0.15*traffic",
		"tau = 1800s", "kappa = 0.5", "tau_post = tau/2 = 900s",
		"signed_time_distance_seconds", "post_reference",
		"hop_cap+1 = 4", "2026-09-01T14:32:00Z", query.TieBreakChangeID,
	} {
		if !strings.Contains(formula, want) {
			t.Errorf("ranking_formula does not mention %q:\n%s", want, formula)
		}
	}
}
