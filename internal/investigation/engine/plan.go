// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	backend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// The published window plan (Phase 7 Track F; FR-042b, FR-042c, SC-003).
//
// Every window the deterministic engine asks about is derived here, and nowhere else, from the
// question: the alert instant, the lookback, and — for the comparison pairs — the reference
// instant the causal step settled on.
//
// The reason this is a file of its own rather than four expressions inside `firstwave.go` is the
// **recorded world**. A world is admitted to the corpus on its miss rate, and a miss is a term
// the engine issued that the recording does not hold. Before this file existed the engine derived
// its onset search window from the investigation window while `fixture record-world` derived its
// from the manifest's grid, and the two never intersected: every term the engine issued keyed
// differently from every term the world held, the miss rate was 1.0000, and the engine fell back
// to the alert instant as its reference — which then made every comparison miss as well. Two
// places deriving "the same" window from different inputs is not a bug that gets fixed once; it
// is a bug that comes back. So there is one derivation, it is exported, and the recorder calls
// the same functions the engine calls.
//
// What is deliberately *not* here: the profile. `page`, `ticket` and `batch` bound what an
// investigation may spend, not what it may look at — a cheaper profile asks fewer questions, not
// narrower ones — so no window below takes a profile, and a reader looking for one should find
// this sentence instead of a parameter that is ignored.

// CompareCeiling caps the half-width of a before/after comparison.
//
// A wide lookback must not produce a comparison whose baseline half is a different part of the
// day: at some point "before the change" stops meaning "the same system under the same load" and
// the comparison is measuring the diurnal cycle. Thirty minutes is the published figure.
const CompareCeiling = 30 * time.Minute

// CompareFloor is the half-width used when the investigation window is degenerate. A window of no
// width measures nothing, and refusing the whole first wave over it would throw away the
// comparison rather than narrow it.
const CompareFloor = 15 * time.Minute

// OnsetSearchWindow is the window the engine searches for the symptom's onset over: the whole
// investigation window, `[fired_at − lookback, fired_at]`.
//
// The whole window, not a slice of it, and that is the load-bearing choice. Onset is the answer to
// "when did this actually start, as distinct from when the monitor noticed", and a search window
// that began after the true onset can only return the alert instant back to the caller dressed up
// as an estimate. The search is therefore allowed to reach as far back as the question does.
func OnsetSearchWindow(firedAt time.Time, lookback time.Duration) *investigationv1.Window {
	end := firedAt.UTC().Truncate(time.Second)
	if lookback <= 0 {
		lookback = time.Hour
	}
	return backend.NewWindow(end.Add(-lookback), end)
}

// CompareHalfWidth is the width each side of the reference instant a before/after comparison
// covers: half the investigation window, floored and capped by the two published constants.
func CompareHalfWidth(lookback time.Duration) time.Duration {
	half := lookback / 2
	switch {
	case half <= 0:
		return CompareFloor
	case half > CompareCeiling:
		return CompareCeiling
	default:
		return half
	}
}

// ComparePairs are the before/after pairs the deterministic first wave asks over, given the
// reference instant the causal step settled on and the investigation's horizon.
//
// One pair. The wave's job is to separate candidates quickly, and a second width would double
// every recording without asking a question the first does not already answer — the ceiling above
// is what keeps the single pair meaningful.
//
// **Nobody sees the future.** `observed_at` is the investigation's horizon: telemetry after it did
// not exist when the question was asked, and a backend answers a window wholly at or past it with
// `no_data` rather than inventing one. A comparison referenced at the alert instant would put its
// whole symptom half there — the commonest case in the corpus, where the reference *is*
// `observed_at` — so the pair is brought back inside the horizon here, in the one place every
// window is derived:
//
//   - a pair that ends before the horizon is unchanged;
//   - a pair that straddles it ends at it, and its baseline narrows to the same width, because a
//     comparison between two halves of different widths is not a comparison;
//   - a reference at or after the horizon slides the pair back so its symptom half is the last
//     `w` of observable time rather than the first `w` of the future.
//
// The engine and `fixture record-world` both call this, so the windows a world holds are the
// windows the engine asks about (the note at the top of this file).
func ComparePairs(reference, observedAt time.Time, lookback time.Duration) []*investigationv1.WindowPair {
	width := CompareHalfWidth(lookback)
	ref := reference.UTC().Truncate(time.Second)
	horizon := observedAt.UTC().Truncate(time.Second)
	if horizon.IsZero() || !ref.Add(width).After(horizon) {
		return []*investigationv1.WindowPair{backend.NewWindowPair(ref, width)}
	}

	end := horizon
	start := ref
	if !start.Before(end) {
		start = end.Add(-width)
	}
	half := end.Sub(start)
	return []*investigationv1.WindowPair{{
		ReferenceAt:  timestamppb.New(start),
		WidthSeconds: int64(half / time.Second),
		Baseline:     backend.NewWindow(start.Add(-half), start),
		Symptom:      backend.NewWindow(start, end),
	}}
}

// ReferenceInstants are the instants a recorder must cover for a deterministic run whose onset it
// cannot know in advance.
//
// The engine references its comparisons to the estimated onset where the metrics worker produced
// one and to the alert instant where it did not (FR-029b), and which of the two it will use is
// not knowable before the run. A recording that covered only one of them would hold the answers
// for exactly the branch that did not happen, so both are enumerated. The estimated onset itself
// is covered by running the engine, which is what `fixture record-world` does before it takes
// this cross product.
func ReferenceInstants(firedAt time.Time, extra ...time.Time) []time.Time {
	out := []time.Time{firedAt.UTC().Truncate(time.Second)}
	seen := map[time.Time]bool{out[0]: true}
	for _, at := range extra {
		at = at.UTC().Truncate(time.Second)
		if at.IsZero() || seen[at] {
			continue
		}
		seen[at] = true
		out = append(out, at)
	}
	return out
}
