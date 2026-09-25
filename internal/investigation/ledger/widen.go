// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"errors"
	"fmt"
)

// Widening a cut-short confidence (T058, FR-045, research §8).
//
// When a budget runs out mid-test, the honest report is not the last number the ledger computed.
// That number was computed on the evidence that arrived, and the evidence that did not arrive was
// expected to move it — so freezing it publishes a precision the run did not earn. FR-045 says the
// confidence of a hypothesis whose testing was cut short MUST widen rather than freeze.
//
// What widens is the *reported bucket*, not the computed confidence. The confidence stays exactly
// what the rule produced, because Invariant 1 requires recomputation from the rows to reproduce
// it, and a report that moved the number would break the audit trail it exists to keep. The bucket
// moves one published step outward, toward the mass the untested evidence could have moved, and
// the move is recorded with its reason — `widened`, `widened_reason` and the direction — so a
// reader can always see both the computed number and the wider claim being made from it.
//
// The direction is a judgment about what was *about* to be tested, so the caller supplies it:
//
//   - WidenDown when the cut-short query was expected to bear against the hypothesis, and when
//     nothing is known about it. Unearned confidence is the failure the constitution guards
//     against, so "we do not know which way it would have gone" widens downward.
//   - WidenUp when the cut-short query was the one expected to confirm it.
//
// One step, never more. Two steps from `very_high` would land in `moderate` and say something
// about the evidence that no missing query justifies.

// WidenDirection is which way a cut-short bucket moved.
type WidenDirection string

// The two widening directions.
const (
	// WidenDown reports one bucket lower: the untested evidence might have taken mass away.
	WidenDown WidenDirection = "down"
	// WidenUp reports one bucket higher: the untested evidence was the confirming one.
	WidenUp WidenDirection = "up"
)

// Widen records that a hypothesis's test was cut short and reports its bucket one step outward
// (FR-045).
//
// A reason is required: a widened bucket that does not say why is a number nobody can check.
// Widening twice is not an error and does not compound — the reported bucket is always exactly one
// step from the computed one — but the later reason replaces the earlier, because the last thing
// that cut the test short is the one worth reporting.
func (l *Ledger) Widen(hypothesisID string, direction WidenDirection, reason string) error {
	i, ok := l.index[hypothesisID]
	if !ok {
		return fmt.Errorf("ledger: widen: hypothesis %s is not in the ledger", hypothesisID)
	}
	if reason == "" {
		return errors.New("ledger: widen: a widened bucket says why it was widened (FR-045)")
	}
	switch direction {
	case WidenDown, WidenUp:
	default:
		return fmt.Errorf("ledger: widen %s: %w: direction %q", hypothesisID, ErrVocabulary, direction)
	}

	h := &l.hypotheses[i]
	h.Widened = true
	h.WidenedDirection = direction
	h.WidenedReason = reason
	// recompute rather than setting the bucket here, so that a judgment arriving after the
	// widening moves the computed confidence and the reported bucket together.
	l.recompute()
	return nil
}

// UnevidencedCeiling is the highest bucket a hypothesis nothing observed supports is reported in
// (Phase 8 K1, FR-023, constitution V).
//
// **Prior-only mass is not confidence.** A hypothesis can hold most of the posterior because the
// ranker liked it and no rival was tested — that is a statement about the prior and about what the
// run did not do, and reporting it as `high` tells an on-call at 03:00 that something is known when
// nothing is. So the *reported* bucket is capped at `moderate` until a supporting judgment resting
// on telemetry evidence or a human fact arrives; the computed confidence is untouched, exactly as
// it is under widening, and both numbers stay visible in the rendering.
const UnevidencedCeiling = "moderate"

// reportedBucket is the bucket a hypothesis is published in: the computed one, one step outward
// when the hypothesis was widened, and never above the unevidenced ceiling when nothing observed
// supports it.
func (l *Ledger) reportedBucket(h Hypothesis) Bucket {
	bucket := reportedBucket(h)
	if l.evidencedBelief(h) {
		return bucket
	}
	ceiling, _ := BucketNamed(UnevidencedCeiling)
	if bucket.Low > ceiling.Low {
		return ceiling
	}
	return bucket
}

// BucketNamed returns a published bucket by name, and whether there is one.
func BucketNamed(name string) (Bucket, bool) {
	for _, b := range buckets {
		if b.Name == name {
			return b, true
		}
	}
	return buckets[len(buckets)-1], false
}

// reportedBucket is the bucket a hypothesis is published in before the unevidenced ceiling is
// applied: the computed one, or one step outward when the hypothesis was widened.
func reportedBucket(h Hypothesis) Bucket {
	idx := bucketIndex(h.Confidence)
	if !h.Widened {
		return buckets[idx]
	}
	switch h.WidenedDirection {
	case WidenUp:
		if idx < len(buckets)-1 {
			idx++
		}
	case WidenDown:
		if idx > 0 {
			idx--
		}
	}
	return buckets[idx]
}
