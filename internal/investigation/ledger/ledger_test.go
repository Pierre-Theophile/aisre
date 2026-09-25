// SPDX-License-Identifier: Apache-2.0

package ledger_test

import (
	"errors"
	"math"
	"testing"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The ledger as a whole (T051, T054, FR-019a, FR-020, FR-020a, FR-023).

// evidenceItem is a minimal, legal evidence item: a digest with a coverage block and a deep link.
// Nothing here is telemetry — a digest reference, a coverage block and join keys, which is all an
// evidence item is ever allowed to be (constitution IV).
func evidenceItem(id string) ledger.EvidenceItem {
	return ledger.EvidenceItem{
		ID:             id,
		Kind:           "algebra_answer",
		Worker:         "metrics",
		Capability:     "compare",
		SourceOfTruth:  "prometheus",
		Mode:           "recorded",
		Outcome:        "digest",
		ResponseDigest: "sha256:" + id,
		ResponseKey:    "world/" + id,
		Coverage: &investigationv1.Coverage{
			DataSource:       "prometheus",
			VolumeConsidered: 1200,
		},
		DeepLink:   "https://example.invalid/q/" + id,
		ValidAt:    time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC),
		ObservedAt: time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC),
		CalledAt:   time.Date(2026, 9, 1, 14, 33, 0, 0, time.UTC),
	}
}

// openLedger is a ledger with π₀ = 0.38 — a round stand-in for the published first audit's
// 0.384615 (ceiling 8/13, ADR-0004, research §8) — and two candidates at 0.8 and 0.2.
func openLedger(t *testing.T) *ledger.Ledger {
	t.Helper()

	l, err := ledger.New("inv-0007", audit.PriorRecord{
		Prior: 0.38, AuditID: "audit-2026-09", Ceiling: 0.62,
		FeederSet: "deploy+vendor", IncidentCount: 13,
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	add := func(id, statement string, score float64) {
		t.Helper()
		if err := l.AddHypothesis(ledger.Hypothesis{
			ID: id, Kind: ledger.KindChange, Statement: statement,
			CandidateChangeEntityID: id + "-change", TargetEntityIDs: []string{"payments"},
		}, score); err != nil {
			t.Fatalf("add hypothesis %s: %v", id, err)
		}
	}
	add("h1", "payments@rev7 caused the checkout error rate", 0.8)
	add("h2", "the checkout config change caused it", 0.2)
	return l
}

// TestTheOpenHypothesisIsAlwaysPresent is T054 and Invariant 2: exactly one `no_observed_change`
// hypothesis per investigation, always there, ranked and scored like any other.
func TestTheOpenHypothesisIsAlwaysPresent(t *testing.T) {
	t.Parallel()

	l := openLedger(t)

	var open int
	for _, h := range l.Hypotheses() {
		if h.Kind == ledger.KindNoObservedChange {
			open++
			if h.ID != l.OpenHypothesisID() {
				t.Errorf("the open hypothesis is %s, want %s", h.ID, l.OpenHypothesisID())
			}
			if h.Prior != 0.38 {
				t.Errorf("the open hypothesis has prior %.6f, want π₀ = 0.380000", h.Prior)
			}
			// "ranked, scored and rendered like any other" (FR-019a): it has a rank, a
			// confidence and a bucket, and it is not pinned to the bottom of the list.
			if h.Rank == 0 || h.Bucket.Name == "" {
				t.Errorf("the open hypothesis is not ranked and bucketed like the others: %+v", h)
			}
			if h.Statement == "" {
				t.Error("the open hypothesis has no statement to render")
			}
		}
	}
	if open != 1 {
		t.Fatalf("the ledger holds %d no_observed_change hypotheses, want exactly 1", open)
	}

	// A second one is refused, which is the code's half of the partial unique index.
	err := l.AddHypothesis(ledger.Hypothesis{
		ID: "h9", Kind: ledger.KindNoObservedChange, Statement: "nothing explains this either",
	}, 1)
	assertRejected(t, err, ledger.ReasonUnsupportedStatus)
}

// TestPriorOfTheOpenHypothesisIsPi0FromTheAudit is ADR-0005 D9: π₀ comes from the coverage audit,
// and an engine that has never measured a ceiling gives the open hypothesis all of the mass.
func TestPriorOfTheOpenHypothesisIsPi0FromTheAudit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		result  *audit.Result
		wantPi0 float64
		// wantOpen is the open hypothesis's confidence once two candidates are in the ledger.
		wantOpen float64
	}{
		{
			name:     "no audit: π₀ = 1 and every candidate sits at zero",
			result:   nil,
			wantPi0:  1,
			wantOpen: 1,
		},
		{
			name:     "a measured ceiling of 62% gives π₀ = 0.38",
			result:   &audit.Result{AuditID: "audit-2026-09", Ceiling: 0.62},
			wantPi0:  0.38,
			wantOpen: 0.38,
		},
		{
			name:     "a deployment whose feeders see everything still keeps an open hypothesis",
			result:   &audit.Result{AuditID: "audit-perfect", Ceiling: 1},
			wantPi0:  0,
			wantOpen: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			record := audit.PriorRecordFromAudit(tc.result)
			if record.Prior != tc.wantPi0 {
				t.Fatalf("π₀ = %v, want %v", record.Prior, tc.wantPi0)
			}

			l, err := ledger.New("inv-0007", record)
			if err != nil {
				t.Fatalf("new ledger: %v", err)
			}
			for _, id := range []string{"h1", "h2"} {
				if err := l.AddHypothesis(ledger.Hypothesis{
					ID: id, Kind: ledger.KindChange, Statement: "candidate " + id,
				}, 1); err != nil {
					t.Fatalf("add hypothesis: %v", err)
				}
			}

			open, ok := l.Hypothesis(l.OpenHypothesisID())
			if !ok {
				t.Fatal("the open hypothesis is missing")
			}
			if open.Confidence != tc.wantOpen {
				t.Errorf("the open hypothesis is at %.6f, want %.6f", open.Confidence, tc.wantOpen)
			}
			if l.Prior().AuditID != record.AuditID {
				t.Errorf("the ledger records audit %q, want %q", l.Prior().AuditID, record.AuditID)
			}
			assertLedgerSumsToOne(t, l)
		})
	}
}

// TestJudgmentIsTheOnlyWayConfidenceMoves is FR-020a and FR-023 together: a typed judgment moves
// the number, and nothing else can.
func TestJudgmentIsTheOnlyWayConfidenceMoves(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	before, _ := l.Hypothesis("h1")

	if err := l.AddEvidence(evidenceItem("e1")); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	recorded, err := l.Judge(ledger.Judgment{
		ID: "j1", HypothesisID: "h1", EvidenceID: "e1",
		Direction: ledger.Supports, Strength: ledger.Strong, Source: ledger.SourceFirstWave,
	})
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	// The ln LR is filled in from the published table, not by the caller.
	if recorded.LnLR != ledger.LnLRStrong {
		t.Errorf("recorded ln LR = %v, want the published %v", recorded.LnLR, ledger.LnLRStrong)
	}

	after, _ := l.Hypothesis("h1")
	if after.Confidence <= before.Confidence {
		t.Errorf("a strong support left h1 at %.6f (was %.6f)", after.Confidence, before.Confidence)
	}
	open, _ := l.Hypothesis(l.OpenHypothesisID())
	if open.Confidence >= 0.38 {
		t.Errorf("supporting a candidate did not take mass from the open hypothesis: %.6f", open.Confidence)
	}
	assertLedgerSumsToOne(t, l)

	// Every rendering the report rests on carries the same number, and the rationale explains it
	// in the ledger's own terms rather than a model's.
	if after.Rationale == "" {
		t.Error("the hypothesis has no derived rationale")
	}
}

// TestWritesTheLedgerRefuses is the write-time vocabulary (data-model §Validation rules).
func TestWritesTheLedgerRefuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		do   func(*testing.T, *ledger.Ledger) error
		want string
	}{
		{
			name: "a hypothesis arriving with a confidence",
			do: func(_ *testing.T, l *ledger.Ledger) error {
				return l.AddHypothesis(ledger.Hypothesis{
					ID: "h3", Kind: ledger.KindChange, Statement: "s", Confidence: 0.9,
				}, 1)
			},
			want: ledger.ReasonModelConfidence,
		},
		{
			name: "a hypothesis arriving with a prior of its own",
			do: func(_ *testing.T, l *ledger.Ledger) error {
				return l.AddHypothesis(ledger.Hypothesis{
					ID: "h3", Kind: ledger.KindChange, Statement: "s", Prior: 0.5,
				}, 1)
			},
			want: ledger.ReasonModelConfidence,
		},
		{
			name: "a judgment carrying a likelihood ratio nobody published",
			do: func(t *testing.T, l *ledger.Ledger) error {
				t.Helper()
				mustAddEvidence(t, l, "e1")
				_, err := l.Judge(ledger.Judgment{
					ID: "j1", HypothesisID: "h1", EvidenceID: "e1",
					Direction: ledger.Supports, Strength: ledger.Weak,
					LnLR: 4.2, Source: ledger.SourceModel,
				})
				return err
			},
			want: ledger.ReasonModelConfidence,
		},
		{
			name: "the same evidence moving the same hypothesis twice",
			do: func(t *testing.T, l *ledger.Ledger) error {
				t.Helper()
				mustAddEvidence(t, l, "e1")
				mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Moderate)
				_, err := l.Judge(ledger.Judgment{
					ID: "j2", HypothesisID: "h1", EvidenceID: "e1",
					Direction: ledger.Supports, Strength: ledger.Strong, Source: ledger.SourceModel,
				})
				return err
			},
			want: ledger.ReasonDuplicateJudgment,
		},
		{
			name: "an evidence item with no coverage block",
			do: func(_ *testing.T, l *ledger.Ledger) error {
				e := evidenceItem("e9")
				e.Coverage = nil
				return l.AddEvidence(e)
			},
			want: ledger.ReasonMissingCoverage,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertRejected(t, tc.do(t, openLedger(t)), tc.want)
		})
	}
}

// TestTheSameEvidenceMayMoveDifferentHypotheses is the other side of the duplicate rule: the cap
// is per pair, so one digest bearing on two candidates is two judgments, which is correct.
func TestTheSameEvidenceMayMoveDifferentHypotheses(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	mustAddEvidence(t, l, "e1")
	mustJudge(t, l, "j1", "h1", "e1", ledger.Supports, ledger.Moderate)
	mustJudge(t, l, "j2", "h2", "e1", ledger.Refutes, ledger.Moderate)

	if len(l.Judgments()) != 2 {
		t.Fatalf("the ledger holds %d judgments, want 2", len(l.Judgments()))
	}
	assertLedgerSumsToOne(t, l)
}

// TestAHumanFactEntersAtStrongNotDecisive is FR-057a and research §8: a fact a person pushes is
// strong evidence, not truth. One assertion may not end an investigation.
func TestAHumanFactEntersAtStrongNotDecisive(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	e := evidenceItem("e-human")
	e.Kind = "human_fact"
	e.SourceOfTruth = ""
	if err := l.AddEvidence(e); err != nil {
		t.Fatalf("add evidence: %v", err)
	}

	if _, err := l.Judge(ledger.Judgment{
		ID: "j1", HypothesisID: "h1", EvidenceID: "e-human",
		Direction: ledger.Supports, Strength: ledger.Decisive, Source: ledger.SourceHumanFact,
	}); err == nil {
		t.Fatal("a human fact was accepted at decisive strength")
	}

	if _, err := l.Judge(ledger.Judgment{
		ID: "j1", HypothesisID: "h1", EvidenceID: "e-human",
		Direction: ledger.Supports, Strength: ledger.Strong, Source: ledger.SourceHumanFact,
	}); err != nil {
		t.Fatalf("a human fact at strong was refused: %v", err)
	}
}

// TestJudgmentsRestOnRecordedEvidence is FR-022: a judgment names an evidence item the ledger
// holds, so every number is traceable to something recorded.
func TestJudgmentsRestOnRecordedEvidence(t *testing.T) {
	t.Parallel()

	l := openLedger(t)
	if _, err := l.Judge(ledger.Judgment{
		ID: "j1", HypothesisID: "h1", EvidenceID: "e-missing",
		Direction: ledger.Supports, Strength: ledger.Strong, Source: ledger.SourceModel,
	}); err == nil {
		t.Fatal("a judgment resting on evidence the ledger does not hold was accepted")
	}
}

func mustAddEvidence(t *testing.T, l *ledger.Ledger, id string) {
	t.Helper()
	if err := l.AddEvidence(evidenceItem(id)); err != nil {
		t.Fatalf("add evidence %s: %v", id, err)
	}
}

func mustJudge(t *testing.T, l *ledger.Ledger, id, hypothesisID, evidenceID string,
	d ledger.Direction, s ledger.Strength,
) ledger.Judgment {
	t.Helper()
	j, err := l.Judge(ledger.Judgment{
		ID: id, HypothesisID: hypothesisID, EvidenceID: evidenceID,
		Direction: d, Strength: s, Source: ledger.SourceFirstWave,
	})
	if err != nil {
		t.Fatalf("judge %s: %v", id, err)
	}
	return j
}

func assertRejected(t *testing.T, err error, wantCode string) {
	t.Helper()
	var rejection *ledger.RejectionError
	if !errors.As(err, &rejection) {
		t.Fatalf("error = %v, want a rejection with reason code %s", err, wantCode)
	}
	if rejection.ReasonCode != wantCode {
		t.Errorf("reason code = %s, want %s (detail: %s)", rejection.ReasonCode, wantCode, rejection.Detail)
	}
}

func assertLedgerSumsToOne(t *testing.T, l *ledger.Ledger) {
	t.Helper()
	units := int64(0)
	for _, h := range l.Hypotheses() {
		units += int64(math.Round(h.Confidence * 1e6))
	}
	if units != 1_000_000 {
		t.Errorf("the ledger's confidences sum to %d millionths, want 1 000 000 (Invariant 10)", units)
	}
}
