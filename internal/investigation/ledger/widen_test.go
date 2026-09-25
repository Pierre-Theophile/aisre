// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Widening on a cut-short test (T058, FR-045).

// TestWideningMovesTheReportedBucketOneStepAndSaysWhy is the rule: the bucket moves outward, the
// computed confidence does not, and the move is recorded with its reason.
func TestWideningMovesTheReportedBucketOneStepAndSaysWhy(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	mustAddEvidence(t, l, "e1")
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Strong)

	before, _ := l.Hypothesis("h1")
	if before.Widened {
		t.Fatal("a hypothesis nobody widened reports as widened")
	}
	computed := ledger.BucketFor(before.Confidence)

	const reason = "the wall-time budget was exhausted before the second compare returned"
	if err := l.Widen("h1", ledger.WidenDown, reason); err != nil {
		t.Fatalf("widen: %v", err)
	}

	after, _ := l.Hypothesis("h1")
	switch {
	case after.Confidence != before.Confidence:
		t.Errorf("widening moved the computed confidence from %.6f to %.6f; it moves the reported "+
			"bucket, never the number the rows reproduce", before.Confidence, after.Confidence)
	case !after.Widened:
		t.Error("the hypothesis does not report as widened")
	case after.WidenedReason != reason:
		t.Errorf("widened reason = %q, want %q", after.WidenedReason, reason)
	case after.WidenedDirection != ledger.WidenDown:
		t.Errorf("widened direction = %q, want %q", after.WidenedDirection, ledger.WidenDown)
	case after.Bucket == computed:
		t.Errorf("the reported bucket is still %s; widening moves it one step outward", after.Bucket)
	}

	// Exactly one step, whichever way.
	all := ledger.Buckets()
	var computedIndex, reportedIndex int
	for i, b := range all {
		if b == computed {
			computedIndex = i
		}
		if b == after.Bucket {
			reportedIndex = i
		}
	}
	if computedIndex-reportedIndex != 1 {
		t.Errorf("the reported bucket moved from %s to %s, want exactly one step down",
			computed, after.Bucket)
	}

	// The widening is visible wherever the confidence is: the derived rationale and the export.
	if !strings.Contains(after.Rationale, "widened") || !strings.Contains(after.Rationale, reason) {
		t.Errorf("the rationale does not record the widening: %q", after.Rationale)
	}
	exported := l.Proto().GetHypotheses()[0]
	if !exported.GetWidened() || exported.GetWidenedReason() != reason {
		t.Errorf("the export lost the widening: %+v", exported)
	}
	if exported.GetBucket().GetName() != after.Bucket.Name {
		t.Errorf("the export reports bucket %s, the ledger %s",
			exported.GetBucket().GetName(), after.Bucket.Name)
	}
}

// TestWideningUpwardsAndAtTheEdges: a widening never leaves the published scale, and widening
// twice does not compound — the reported bucket is always exactly one step from the computed one.
func TestWideningUpwardsAndAtTheEdges(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	mustAddEvidence(t, l, "e1")
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Decisive)

	if err := l.Widen("h1", ledger.WidenUp, "the confirming query never ran"); err != nil {
		t.Fatalf("widen up: %v", err)
	}
	h1, _ := l.Hypothesis("h1")
	// h1 is already in the top bucket, so widening up clamps there rather than inventing a sixth.
	if h1.Bucket.Name != "very_high" {
		t.Errorf("widening up from the top bucket produced %s", h1.Bucket)
	}

	// h2 sits at the bottom; widening down clamps at very_low.
	if err := l.Widen("h2", ledger.WidenDown, "the refuting query never ran"); err != nil {
		t.Fatalf("widen down: %v", err)
	}
	h2, _ := l.Hypothesis("h2")
	if h2.Bucket.Name != "very_low" {
		t.Errorf("widening down from the bottom bucket produced %s", h2.Bucket)
	}

	// Widening twice keeps one step, and the later reason replaces the earlier.
	if err := l.Widen("h2", ledger.WidenDown, "and neither did the second one"); err != nil {
		t.Fatalf("widen twice: %v", err)
	}
	again, _ := l.Hypothesis("h2")
	if again.Bucket != h2.Bucket {
		t.Errorf("widening twice compounded: %s then %s", h2.Bucket, again.Bucket)
	}
	if again.WidenedReason != "and neither did the second one" {
		t.Errorf("the later widening did not replace the reason: %q", again.WidenedReason)
	}
}

// TestWideningRequiresAReason: a widened bucket that does not say why is a number nobody can check.
func TestWideningRequiresAReason(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	if err := l.Widen("h1", ledger.WidenDown, ""); err == nil {
		t.Error("a widening with no reason was accepted")
	}
	if err := l.Widen("h1", "sideways", "because"); err == nil {
		t.Error("a widening in a direction outside the published vocabulary was accepted")
	}
	if err := l.Widen("h-missing", ledger.WidenDown, "because"); err == nil {
		t.Error("a widening of a hypothesis that is not in the ledger was accepted")
	}
}

// TestJudgingAfterWideningKeepsTheOffsetOneStep: a hypothesis whose test resumes and whose
// confidence then moves is still reported one step outward until the widening is lifted, so the
// report never silently re-narrows.
func TestJudgingAfterWideningKeepsTheOffsetOneStep(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	mustAddEvidence(t, l, "e1")
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Moderate)
	if err := l.Widen("h1", ledger.WidenDown, "budget exhausted mid-test"); err != nil {
		t.Fatalf("widen: %v", err)
	}

	mustAddEvidence(t, l, "e2")
	mustJudge(t, l, "j2", "h1", "e2", ledger.Supports, ledger.Strong)

	h1, _ := l.Hypothesis("h1")
	computed := ledger.BucketFor(h1.Confidence)
	if h1.Bucket == computed {
		t.Errorf("after a later judgment the reported bucket snapped back to the computed %s", computed)
	}
	if !h1.Widened {
		t.Error("a later judgment cleared the widening flag")
	}
}
