// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"pgregory.net/rapid"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The formal statement of "two runs that gather the same evidence agree" (T053, FR-023,
// data-model §Invariants 1).
//
// Everything else in this feature rests on this property. A trajectory replay compares
// confidences byte for byte; a calibration table pools hypotheses from runs that visited their
// evidence in different orders; a grader diffs two investigations of the same incident. All three
// are meaningless if the posterior depends on the order judgments arrived in.
//
// The proof is one line of arithmetic — `ln posterior = ln prior + Σ ln LR`, and addition
// commutes — so what these tests actually guard is the *implementation* not quietly introducing
// an order dependence: a running normalisation, a map iterated in Go's randomised order and then
// rounded, a tie broken by insertion position. rapid searches for exactly that.
//
// Three properties, in increasing strength:
//
//	(i)   Posteriors over a permuted judgment slice returns the identical distribution;
//	(ii)  a Ledger built by applying the same judgments in a permuted order exports byte-identical
//	      hypotheses — confidences, buckets, ranks, rationales and evidence lists included;
//	(iii) recomputing from the rows alone, as the DAO reads them back, reproduces the stored
//	      confidence exactly at six decimals (Invariant 1).

// scenario is a generated ledger: some candidates with ranker scores, a π₀, and a multiset of
// judgments over them.
type scenario struct {
	pi0        float64
	scores     []ledger.PriorInput
	hypotheses []ledger.Hypothesis
	judgments  []ledger.Judgment
}

var (
	directions = []ledger.Direction{ledger.Supports, ledger.Refutes, ledger.Neutral}
	strengths  = []ledger.Strength{ledger.Weak, ledger.Moderate, ledger.Strong, ledger.Decisive}
)

// genScenario draws a ledger. Evidence ids are unique per judgment, so every judgment is a legal
// one: at most one judgment per (evidence item, hypothesis) pair is a *cap*, and a generator that
// broke it would be testing the rejection rather than the arithmetic.
func genScenario(t *rapid.T) scenario {
	pi0 := rapid.Float64Range(0, 1).Draw(t, "pi0")
	n := rapid.IntRange(1, 6).Draw(t, "hypotheses")

	s := scenario{pi0: math.Round(pi0*1e6) / 1e6}
	for i := range n {
		id := fmt.Sprintf("h%d", i)
		s.scores = append(s.scores, ledger.PriorInput{
			HypothesisID: id,
			Score:        rapid.Float64Range(0, 100).Draw(t, "score"),
		})
	}
	priors, err := ledger.Priors(s.scores, s.pi0, "open")
	if err != nil {
		t.Fatalf("priors: %v", err)
	}
	for _, score := range s.scores {
		s.hypotheses = append(s.hypotheses, ledger.Hypothesis{
			ID: score.HypothesisID, Kind: ledger.KindChange, Prior: priors[score.HypothesisID],
		})
	}
	s.hypotheses = append(s.hypotheses, ledger.Hypothesis{
		ID: "open", Kind: ledger.KindNoObservedChange, Prior: priors["open"],
	})

	m := rapid.IntRange(0, 24).Draw(t, "judgments")
	for i := range m {
		direction := rapid.SampledFrom(directions).Draw(t, "direction")
		strength := rapid.SampledFrom(strengths).Draw(t, "strength")
		if direction == ledger.Neutral {
			strength = ""
		}
		target := rapid.IntRange(0, n-1).Draw(t, "target")
		s.judgments = append(s.judgments, judgment(
			fmt.Sprintf("j%02d", i), fmt.Sprintf("h%d", target), fmt.Sprintf("e%02d", i),
			direction, strength))
	}
	return s
}

// TestPosteriorOrderIndependenceProperty is property (i): the arithmetic itself.
func TestPosteriorOrderIndependenceProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		s := genScenario(t)
		want := ledger.Posteriors(s.hypotheses, s.judgments)

		shuffled := rapid.Permutation(s.judgments).Draw(t, "permuted judgments")
		got := ledger.Posteriors(s.hypotheses, shuffled)

		for id, w := range want {
			if got[id] != w {
				t.Fatalf("permuting the judgments moved %s from %.6f to %.6f", id, w, got[id])
			}
		}

		// Invariant 10 holds for every draw, not only for the tidy ones: the millionths sum to
		// exactly one million whatever the priors and judgments were.
		units := int64(0)
		for _, v := range got {
			units += int64(math.Round(v * 1e6))
		}
		if units != 1_000_000 {
			t.Fatalf("confidences sum to %d millionths, want 1 000 000: %v", units, got)
		}
	})
}

// TestLedgerExportOrderIndependenceProperty is property (ii): the whole ledger, through the
// same path the export and the recording take.
//
// This is the version that would catch an order dependence hiding in ranking, bucketing or the
// derived rationale rather than in the posterior — a tie broken by insertion order would show up
// here and nowhere else.
func TestLedgerExportOrderIndependenceProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		s := genScenario(t)
		shuffled := rapid.Permutation(s.judgments).Draw(t, "permuted judgments")

		want := buildLedger(t, s, s.judgments)
		got := buildLedger(t, s, shuffled)
		if !bytes.Equal(want, got) {
			t.Fatalf("permuting the judgments changed the exported hypotheses:\n%s\nvs\n%s", want, got)
		}
	})
}

// buildLedger applies the scenario's judgments to a real Ledger in the order given and returns the
// canonical JSON of its hypotheses.
func buildLedger(t *rapid.T, s scenario, order []ledger.Judgment) []byte {
	t.Helper()

	l, err := ledger.New("inv-property", audit.PriorRecord{Prior: s.pi0, AuditID: "audit-property"},
		ledger.WithOpenHypothesisID("open"))
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	for _, score := range s.scores {
		err := l.AddHypothesis(ledger.Hypothesis{
			ID: score.HypothesisID, Kind: ledger.KindChange, Statement: "candidate " + score.HypothesisID,
		}, score.Score)
		if err != nil {
			t.Fatalf("add hypothesis %s: %v", score.HypothesisID, err)
		}
	}
	for _, j := range order {
		if err := l.AddEvidence(evidenceItem(j.EvidenceID)); err != nil {
			t.Fatalf("add evidence %s: %v", j.EvidenceID, err)
		}
		if _, err := l.Judge(j); err != nil {
			t.Fatalf("judge %s: %v", j.ID, err)
		}
	}

	// The hypotheses alone: the judgments list on the exported Ledger is in recording order,
	// which really is a sequence and really does differ between permutations. What must not
	// differ is anything computed from them.
	raw, err := graph.CanonicalJSON(&investigationv1.Ledger{Hypotheses: l.Proto().GetHypotheses()})
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	return raw
}

// TestRecomputationFromTheRowsProperty is property (iii), Invariant 1.
//
// The DAO stores `prior` on the hypothesis and `ln_lr` on the judgment precisely so that this is
// checkable without the ledger that produced them. A reader years from now, with the two tables
// and the published rule, must land on the number the report published — and if they do not, the
// report was never evidence of anything.
func TestRecomputationFromTheRowsProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		s := genScenario(t)
		l, err := ledger.New("inv-property", audit.PriorRecord{Prior: s.pi0},
			ledger.WithOpenHypothesisID("open"))
		if err != nil {
			t.Fatalf("new ledger: %v", err)
		}
		for _, score := range s.scores {
			if err := l.AddHypothesis(ledger.Hypothesis{
				ID: score.HypothesisID, Kind: ledger.KindChange, Statement: "candidate",
			}, score.Score); err != nil {
				t.Fatalf("add hypothesis: %v", err)
			}
		}
		for _, j := range s.judgments {
			if err := l.AddEvidence(evidenceItem(j.EvidenceID)); err != nil {
				t.Fatalf("add evidence: %v", err)
			}
			if _, err := l.Judge(j); err != nil {
				t.Fatalf("judge: %v", err)
			}
		}

		// The rows, as the DAO would read them back: priors and ln LRs, nothing else.
		rows := make([]ledger.Hypothesis, 0, len(s.hypotheses))
		for _, h := range l.Hypotheses() {
			rows = append(rows, ledger.Hypothesis{ID: h.ID, Prior: h.Prior})
		}
		recomputed := ledger.Posteriors(rows, l.Judgments())
		for _, h := range l.Hypotheses() {
			if recomputed[h.ID] != h.Confidence {
				t.Fatalf("recomputing %s from the rows gives %.6f, stored %.6f",
					h.ID, recomputed[h.ID], h.Confidence)
			}
			// The reported bucket is the computed one, except for the two published reasons it
			// is not: a cut-short test widens it, and a hypothesis nothing observed supports is
			// capped at the unevidenced ceiling (Phase 8 K1).
			want := ledger.BucketFor(h.Confidence)
			if ceiling, ok := ledger.BucketNamed(ledger.UnevidencedCeiling); ok &&
				!l.Evidenced(h.ID) && want.Low > ceiling.Low {
				want = ceiling
			}
			if !h.Widened && want != h.Bucket {
				t.Fatalf("%s is reported in %s but %.6f with evidenced=%t calls for %s",
					h.ID, h.Bucket, h.Confidence, l.Evidenced(h.ID), want)
			}
		}
	})
}
