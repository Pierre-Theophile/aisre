// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	"pgregory.net/rapid"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// The four properties feature 002 depends on (ADR-0005 D4, 002 FR-029c).
//
// The worked examples in rank_test.go pin the curve at a handful of instants. These are the
// statements 002 actually builds on, and they have to hold at *every* instant, because 002
// moves the reference to an estimated symptom onset and then reads the answer as causal
// evidence:
//
//	(i)   a post-reference candidate never receives the maximum temporal weight;
//	(ii)  it scores strictly below an equally distant pre-reference candidate;
//	(iii) the score is monotone decreasing in |dt| on both sides;
//	(iv)  the caller can identify it as post-reference from the response.
//
// Each is checked twice: as a table of the boundary cases that are easy to get wrong, and with
// rapid over the whole parameter space. Two concessions to arithmetic, both stated rather than
// hidden:
//
//   - Rounding to the published six decimals turns a strict inequality into a non-strict one
//     for two candidates far enough out that both temporal terms round to zero. The strict form
//     is therefore asserted on the unrounded curve (TemporalWeight) and the weak form on the
//     response — the honest reading of "published to six decimals", not a weakened property.
//   - `exp` underflows. Past a few hundred decay constants two distinct instants a second apart
//     are the same float64, and past ~745 both are exactly zero. The strict form is asserted
//     out to `maxDecayConstants` decay constants, which is 17 orders of magnitude below the
//     published precision and some four days at the default tau: far beyond any window an
//     incident is asked about, and far short of where float64 runs out.

// rankedAt scores one candidate whose change happened `dt` seconds BEFORE the reference
// instant (negative dt = after it), at the given hop distance and weight class.
func rankedAt(dt int64, hop, weight uint32, p query.RankParams) *graphv1.RankedChange {
	at := p.Reference.Add(-time.Duration(dt) * time.Second)
	ranked, _ := query.Rank([]query.Candidate{{
		Change:      changeAt(fmt.Sprintf("change-%d", dt), at),
		TargetIDs:   []string{"target"},
		Hop:         hop,
		WeightClass: weight,
	}}, p)
	return ranked[0]
}

// TestPostReferenceNeverTakesTheMaximumTemporalWeight is property (i).
//
// "The maximum" is 1.0, taken only at dt = 0 on the pre-reference side. The post-reference
// side is capped at kappa = 0.5 by construction, which is the whole point: a change that
// followed the instant under investigation cannot be the most temporally suspicious thing in
// the answer, however close behind it followed.
func TestPostReferenceNeverTakesTheMaximumTemporalWeight(t *testing.T) {
	for _, dt := range []int64{-1, -2, -59, -60, -300, -1800, -3600, -86400} {
		got := query.TemporalWeight(dt, rankParams)
		if got > query.PostReferenceCeiling {
			t.Errorf("temporal(%d s) = %v, above the published ceiling kappa = %v",
				dt, got, query.PostReferenceCeiling)
		}
		if got >= 1 {
			t.Errorf("temporal(%d s) = %v, which is the maximum temporal weight", dt, got)
		}
	}

	rapid.Check(t, func(rt *rapid.T) {
		p := drawParams(rt)
		dt := -drawDistance(rt, p, 1, "dt")
		item := rankedAt(dt, uint32(rapid.IntRange(0, 4).Draw(rt, "hop")), 0, p)
		if item.GetTemporal() > query.PostReferenceCeiling {
			rt.Fatalf("temporal = %v for dt = %d s, above kappa = %v",
				item.GetTemporal(), dt, query.PostReferenceCeiling)
		}
	})
}

// TestPostReferenceScoresBelowAnEquallyDistantPreReferenceChange is property (ii), the one
// 002's causal ordering actually leans on: of two candidates the same number of seconds from
// the onset, on the same edge at the same distance, the one *before* it wins.
func TestPostReferenceScoresBelowAnEquallyDistantPreReferenceChange(t *testing.T) {
	for _, d := range []int64{1, 30, 60, 120, 300, 900, 1800, 3600} {
		before := rankedAt(d, 1, 3, rankParams)
		after := rankedAt(-d, 1, 3, rankParams)
		if after.GetScore() >= before.GetScore() {
			t.Errorf("|dt| = %d s: post-reference score %v is not below pre-reference %v",
				d, after.GetScore(), before.GetScore())
		}
		if !after.GetPostReference() || before.GetPostReference() {
			t.Errorf("|dt| = %d s: post_reference flags are %v (after) and %v (before)",
				d, after.GetPostReference(), before.GetPostReference())
		}
	}

	rapid.Check(t, func(rt *rapid.T) {
		p := drawParams(rt)
		d := drawDistance(rt, p, 1, "distance")
		hop := uint32(rapid.IntRange(0, 4).Draw(rt, "hop"))
		weight := uint32(rapid.IntRange(0, query.MaxWeightClass).Draw(rt, "weight"))

		// Strictly, on the unrounded curve: this is the property.
		if query.TemporalWeight(-d, p) >= query.TemporalWeight(d, p) {
			rt.Fatalf("|dt| = %d s: temporal after %v >= temporal before %v",
				d, query.TemporalWeight(-d, p), query.TemporalWeight(d, p))
		}
		// And no weaker than non-increasing once published to six decimals, which is all the
		// response can say when both terms have decayed below 5e-7.
		after, before := rankedAt(-d, hop, weight, p), rankedAt(d, hop, weight, p)
		if after.GetScore() > before.GetScore() {
			rt.Fatalf("|dt| = %d s: published post-reference score %v above pre-reference %v",
				d, after.GetScore(), before.GetScore())
		}
	})
}

// TestTemporalIsMonotoneInAbsoluteDistanceOnBothSides is property (iii): further from the
// reference instant is never more suspicious, on either side. A curve that was not monotone
// would make "the top three" depend on an arbitrary instant rather than on the evidence.
func TestTemporalIsMonotoneInAbsoluteDistanceOnBothSides(t *testing.T) {
	steps := []int64{0, 1, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200}
	for i := 1; i < len(steps); i++ {
		near, far := steps[i-1], steps[i]
		if query.TemporalWeight(far, rankParams) >= query.TemporalWeight(near, rankParams) {
			t.Errorf("pre-reference: temporal(%d) >= temporal(%d)", far, near)
		}
		if query.TemporalWeight(-far, rankParams) >= query.TemporalWeight(-near, rankParams) {
			t.Errorf("post-reference: temporal(-%d) >= temporal(-%d)", far, near)
		}
	}

	rapid.Check(t, func(rt *rapid.T) {
		p := drawParams(rt)
		near := drawDistance(rt, p, 0, "near")
		far := near + drawDistance(rt, p, 1, "gap")
		for _, sign := range []int64{1, -1} {
			if sign < 0 && near == 0 {
				continue // dt = 0 belongs to the pre-reference side by definition
			}
			nearW := query.TemporalWeight(sign*near, p)
			farW := query.TemporalWeight(sign*far, p)
			if farW >= nearW {
				rt.Fatalf("sign %d: temporal(%d) = %v is not below temporal(%d) = %v",
					sign, sign*far, farW, sign*near, nearW)
			}
			if !math.IsInf(nearW, 0) && round6(farW) > round6(nearW) {
				rt.Fatalf("sign %d: published temporal is not monotone: %v after %v",
					sign, round6(farW), round6(nearW))
			}
		}
	})
}

// TestTheResponseIdentifiesAPostReferenceCandidate is property (iv): the caller never has to
// recompute anything to know which side of the reference a candidate is on. `post_reference`
// says so, `signed_time_distance_seconds` says by how much, and field 7 keeps the floored
// value 001 published so that an existing consumer sees exactly what it saw before.
func TestTheResponseIdentifiesAPostReferenceCandidate(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		p := drawParams(rt)
		dt := drawDistance(rt, p, 0, "dt")
		if rapid.Bool().Draw(rt, "after") {
			dt = -dt
		}
		item := rankedAt(dt, 1, 2, p)

		if got, want := item.GetSignedTimeDistanceSeconds(), dt; got != want {
			rt.Fatalf("signed_time_distance_seconds = %d, want %d", got, want)
		}
		if got, want := item.GetPostReference(), dt < 0; got != want {
			rt.Fatalf("post_reference = %v for dt = %d, want %v", got, dt, want)
		}
		if got, want := item.GetTimeDistanceSeconds(), max(dt, int64(0)); got != want {
			rt.Fatalf("time_distance_seconds = %d, want the floored %d", got, want)
		}
		if got := query.TemporalWeight(item.GetSignedTimeDistanceSeconds(), p); round6(got) != item.GetTemporal() {
			rt.Fatalf("temporal %v is not recomputable from the published distance (%v)",
				item.GetTemporal(), round6(got))
		}
	})
}

// drawParams draws a ranking's published knobs. Tau is drawn too, because tau_post is derived
// from it: a property that held only at the default would not be a property.
// maxDecayConstants bounds the distances the strict properties are drawn over, in multiples of
// tau_post (the faster of the two sides, so the bound holds on both).
const maxDecayConstants = 40

func drawParams(rt *rapid.T) query.RankParams {
	return query.RankParams{
		Reference: rankReference,
		Tau:       time.Duration(rapid.IntRange(60, 4*3600).Draw(rt, "tau")) * time.Second,
		HopCap:    uint32(rapid.IntRange(1, 6).Draw(rt, "hopCap")),
	}
}

// drawDistance draws a distance in seconds inside the range the strict properties are stated
// over: up to maxDecayConstants times the steeper of the two decay constants.
func drawDistance(rt *rapid.T, p query.RankParams, min int, label string) int64 {
	tauPost := p.Tau.Seconds() / query.PostReferenceTauDivisor
	return int64(rapid.IntRange(min, int(maxDecayConstants*tauPost)).Draw(rt, label))
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
