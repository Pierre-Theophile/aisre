// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The ledger DAO against a real database (T051, T054, Invariants 1, 2, 4, 10).
//
// Every test here is about something the schema enforces and Go cannot: the deferred sum trigger,
// the one-judgment-per-pair uniqueness, the partial unique index on the open hypothesis. A DAO
// test that mocked the database would assert only that the SQL was spelled the way the test
// expected it to be spelled.

const (
	testInvestigationID = "inv-dao-0001"
	testIncidentID      = "inc-dao-0001"
	testPrincipal       = "sre@example.invalid"
)

// seedInvestigation inserts the incident and investigation rows the ledger's foreign keys need.
//
// It is raw SQL rather than a DAO call on purpose: Phase 6 owns writing investigations, and a
// helper here would be a second way to do it.
func seedInvestigation(ctx context.Context, t *testing.T, store *postgres.Store) {
	t.Helper()

	at := time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	err := store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO graph.principals (principal) VALUES ($1) ON CONFLICT DO NOTHING`,
			testPrincipal); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO investigation.incidents
				(incident_id, canonical_subject_id, opened_at, last_symptom_at, association_rule_version)
			VALUES ($1, 'svc:checkout', $2, $2, 'v1')`, testIncidentID, at); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO investigation.investigations (
				investigation_id, incident_id, valid_at, observed_at, "window", profile,
				requester, algebra_version, ledger_rule_version, schema_version, started_at)
			VALUES ($1, $2, $3, $3, tstzrange($4, $3), 'default', $5, '1.0.0', $6, '1.0.0', $3)`,
			testInvestigationID, testIncidentID, at, at.Add(-time.Hour), testPrincipal,
			ledger.LedgerRuleVersion)
		return err
	})
	if err != nil {
		t.Fatalf("seed investigation: %v", err)
	}
}

// savedLedger builds the ledger the DAO tests write: two candidates, the open hypothesis, two
// evidence items and two judgments pointing opposite ways.
func savedLedger(t *testing.T) *ledger.Ledger {
	t.Helper()

	l, err := ledger.New(testInvestigationID, audit.PriorRecord{
		Prior: 0.38, AuditID: "audit-2026-09", Ceiling: 0.62, IncidentCount: 13,
	})
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	for _, c := range []struct {
		id    string
		score float64
	}{{"h1", 0.8}, {"h2", 0.2}} {
		if err := l.AddHypothesis(ledger.Hypothesis{
			ID: c.id, Kind: ledger.KindChange, Statement: "candidate " + c.id,
			CandidateChangeEntityID: c.id + "-change", TargetEntityIDs: []string{"svc:payments"},
			ActorKind: "controller",
		}, c.score); err != nil {
			t.Fatalf("add hypothesis %s: %v", c.id, err)
		}
	}
	for _, id := range []string{"e1", "e2"} {
		if err := l.AddEvidence(daoEvidence(id)); err != nil {
			t.Fatalf("add evidence %s: %v", id, err)
		}
	}
	if _, err := l.Judge(ledger.Judgment{
		ID: "j1", HypothesisID: "h1", EvidenceID: "e1",
		Direction: ledger.Supports, Strength: ledger.Strong, Source: ledger.SourceFirstWave,
		RecordedAt: time.Date(2026, 9, 1, 14, 34, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("judge j1: %v", err)
	}
	if _, err := l.Judge(ledger.Judgment{
		ID: "j2", HypothesisID: "h2", EvidenceID: "e2",
		Direction: ledger.Refutes, Strength: ledger.Moderate, Source: ledger.SourceModel,
		RecordedAt: time.Date(2026, 9, 1, 14, 35, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("judge j2: %v", err)
	}
	return l
}

// TestSaveAndLoadRoundTripsTheLedger is Invariant 1 through the database: the rows reproduce the
// confidence the ledger computed, exactly, at six decimals.
func TestSaveAndLoadRoundTripsTheLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	dao := investigationstore.NewLedgerDAO(store)
	l := savedLedger(t)
	if err := dao.Save(ctx, l); err != nil {
		t.Fatalf("save ledger: %v", err)
	}

	rows, err := dao.Load(ctx, testInvestigationID)
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	if len(rows.Hypotheses) != 3 || len(rows.Judgments) != 2 || len(rows.Evidence) != 2 {
		t.Fatalf("loaded %d hypotheses, %d judgments, %d evidence items; want 3/2/2",
			len(rows.Hypotheses), len(rows.Judgments), len(rows.Evidence))
	}

	// Rank order survives, and so does every computed field.
	want := map[string]ledger.Hypothesis{}
	for _, h := range l.Hypotheses() {
		want[h.ID] = h
	}
	for i, got := range rows.Hypotheses {
		expected := want[got.ID]
		switch {
		case got.Rank != i+1:
			t.Errorf("hypothesis %s loaded at position %d with rank %d", got.ID, i+1, got.Rank)
		case got.Confidence != expected.Confidence:
			t.Errorf("%s confidence = %.6f, want %.6f", got.ID, got.Confidence, expected.Confidence)
		case got.Prior != expected.Prior:
			t.Errorf("%s prior = %.6f, want %.6f", got.ID, got.Prior, expected.Prior)
		case got.Bucket != expected.Bucket:
			t.Errorf("%s bucket = %s, want %s", got.ID, got.Bucket, expected.Bucket)
		case got.Kind != expected.Kind || got.Status != expected.Status:
			t.Errorf("%s loaded as %s/%s, want %s/%s", got.ID, got.Kind, got.Status,
				expected.Kind, expected.Status)
		case got.Rationale != expected.Rationale:
			t.Errorf("%s rationale = %q, want %q", got.ID, got.Rationale, expected.Rationale)
		}
	}

	// Invariant 1, stated as the property that matters: recomputing from the rows alone — the
	// priors and the stored ln LRs — reproduces the stored confidences exactly.
	recomputed := ledger.Posteriors(rows.Hypotheses, rows.Judgments)
	for _, h := range rows.Hypotheses {
		if recomputed[h.ID] != h.Confidence {
			t.Errorf("recomputing %s from the rows gives %.6f, the row holds %.6f",
				h.ID, recomputed[h.ID], h.Confidence)
		}
	}

	// The judgments carry the published ln LR, which is what makes that recomputation possible
	// years after the table is next revised.
	for _, j := range rows.Judgments {
		ln, err := ledger.LnLR(j.Direction, j.Strength)
		if err != nil {
			t.Fatalf("ln LR for %s: %v", j.ID, err)
		}
		if j.LnLR != ln {
			t.Errorf("judgment %s stored ln_lr %v, want the published %v", j.ID, j.LnLR, ln)
		}
	}

	// The evidence round-trips with its coverage block and its digest, and with no telemetry.
	for _, e := range rows.Evidence {
		if e.Coverage == nil || e.Coverage.GetDataSource() == "" {
			t.Errorf("evidence %s lost its coverage block", e.ID)
		}
		if e.ResponseDigest == "" {
			t.Errorf("evidence %s lost its response digest", e.ID)
		}
	}
}

// TestSavingAgainAfterAJudgmentRenormalisesTheWholeSet is the transaction rule (Invariant 10): a
// second save writes every hypothesis's new confidence, and the deferred trigger passes because
// the set still sums to one.
func TestSavingAgainAfterAJudgmentRenormalisesTheWholeSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	dao := investigationstore.NewLedgerDAO(store)
	l := savedLedger(t)
	if err := dao.Save(ctx, l); err != nil {
		t.Fatalf("first save: %v", err)
	}

	if err := l.AddEvidence(daoEvidence("e3")); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	if _, err := l.Judge(ledger.Judgment{
		ID: "j3", HypothesisID: "h2", EvidenceID: "e3",
		Direction: ledger.Supports, Strength: ledger.Decisive, Source: ledger.SourceFirstWave,
	}); err != nil {
		t.Fatalf("judge j3: %v", err)
	}
	if err := dao.Save(ctx, l); err != nil {
		t.Fatalf("second save: %v", err)
	}

	rows, err := dao.Load(ctx, testInvestigationID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var sum int64
	for _, h := range rows.Hypotheses {
		sum += int64(h.Confidence*1e6 + 0.5)
	}
	if sum != 1_000_000 {
		t.Errorf("stored confidences sum to %d millionths, want 1 000 000", sum)
	}
	// h2 started third, behind the open hypothesis: a moderate refute against a prior of 0.124.
	// A decisive support moves it past the open hypothesis but not past h1's strong support —
	// which is the arithmetic, not a tie-break, and the renormalised set says so.
	for _, h := range rows.Hypotheses {
		if h.ID == "h2" && h.Rank != 2 {
			t.Errorf("after a decisive support h2 is ranked %d, want 2 (rows: %v)", h.Rank, rankLine(rows.Hypotheses))
		}
	}
}

// TestOneHypothesisOnItsOwnFailsAtCommit is why Save takes the whole set: a distribution is not a
// row, and the database says so at commit rather than accepting a ledger that sums to 0.49.
func TestOneHypothesisOnItsOwnFailsAtCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	err := store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return investigationstore.SaveHypothesesInTx(ctx, tx, testInvestigationID, []ledger.Hypothesis{{
			ID: "h-alone", Kind: ledger.KindChange, Statement: "the only one",
			CausalRole: ledger.RoleCause, Prior: 0.49, Status: ledger.StatusProposed,
			Confidence: 0.49, Bucket: ledger.BucketFor(0.49), Rank: 1,
		}})
	})
	if err == nil {
		t.Fatal("a lone hypothesis with confidence 0.49 was committed; Invariant 10 is not enforced")
	}
}

// TestDuplicateJudgmentIsRefusedByTheSchemaToo: the ledger refuses a second judgment per pair in
// memory, and the schema refuses one that arrives another way — reported with the same published
// reason code, so a caller matches one vocabulary.
func TestDuplicateJudgmentIsRefusedByTheSchemaToo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	dao := investigationstore.NewLedgerDAO(store)
	l := savedLedger(t)
	if err := dao.Save(ctx, l); err != nil {
		t.Fatalf("save: %v", err)
	}

	err := store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return investigationstore.InsertJudgmentInTx(ctx, tx, testInvestigationID, ledger.Judgment{
			ID: "j-dup", HypothesisID: "h1", EvidenceID: "e1",
			Direction: ledger.Supports, Strength: ledger.Weak, LnLR: ledger.LnLRWeak,
			Source: ledger.SourceModel,
		})
	})
	var rejection *ledger.RejectionError
	if !errors.As(err, &rejection) || rejection.ReasonCode != ledger.ReasonDuplicateJudgment {
		t.Fatalf("second judgment for (h1, e1) returned %v, want %s", err, ledger.ReasonDuplicateJudgment)
	}
}

// TestASecondOpenHypothesisIsRefusedByTheIndex is Invariant 2's structural half, reached through
// the DAO rather than through the ledger's own refusal.
func TestASecondOpenHypothesisIsRefusedByTheIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	dao := investigationstore.NewLedgerDAO(store)
	l := savedLedger(t)
	if err := dao.Save(ctx, l); err != nil {
		t.Fatalf("save: %v", err)
	}

	err := store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return investigationstore.SaveHypothesesInTx(ctx, tx, testInvestigationID, []ledger.Hypothesis{{
			ID: "h-second-open", Kind: ledger.KindNoObservedChange,
			Statement: "nothing explains this either", CausalRole: ledger.RoleCause,
			Prior: 0, Status: ledger.StatusProposed, Confidence: 0,
			Bucket: ledger.BucketFor(0), Rank: 4,
		}})
	})
	if err == nil {
		t.Fatal("a second no_observed_change hypothesis was accepted (Invariant 2)")
	}
}

// TestExonerationRoundTripsItsOnsetEvidence is Invariant 9: the onset estimate an exoneration
// rests on is recoverable from the rows, through the judgment that carries it.
func TestExonerationRoundTripsItsOnsetEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	l := savedLedger(t)
	onset := daoEvidence("e-onset")
	onset.Kind = "onset_estimate"
	onset.Worker = "onset"
	if err := l.AddEvidence(onset); err != nil {
		t.Fatalf("add onset evidence: %v", err)
	}
	if _, err := l.Exonerate("h1", ledger.Exoneration{
		JudgmentID: "j-exo", OnsetEvidenceID: "e-onset",
		Reason: "the rollout starts 11 minutes after onset",
	}); err != nil {
		t.Fatalf("exonerate: %v", err)
	}

	dao := investigationstore.NewLedgerDAO(store)
	if err := dao.Save(ctx, l); err != nil {
		t.Fatalf("save: %v", err)
	}
	rows, err := dao.Load(ctx, testInvestigationID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, h := range rows.Hypotheses {
		if h.ID != "h1" {
			continue
		}
		switch {
		case h.Status != ledger.StatusExonerated:
			t.Errorf("h1 loaded as %s, want %s", h.Status, ledger.StatusExonerated)
		case h.CausalRole != ledger.RoleCandidateEffect:
			t.Errorf("h1 loaded with causal role %s, want %s", h.CausalRole, ledger.RoleCandidateEffect)
		case h.OnsetEvidenceID != "e-onset":
			t.Errorf("h1 lost the onset estimate's evidence id: %q", h.OnsetEvidenceID)
		}
	}
}

// TestUntestedRowsCarryTheirReasonAndNextQuery is FR-031 through the schema's own CHECK.
func TestUntestedRowsCarryTheirReasonAndNextQuery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	l := savedLedger(t)
	if err := l.SetStatus("h2", ledger.StatusUntested, ledger.StatusUpdate{
		Reason:            "no metric pointer of the required kind",
		NextQuery:         daoNextQuery(),
		NextQueryDeepLink: "https://example.invalid/compare",
	}); err != nil {
		t.Fatalf("set untested: %v", err)
	}

	dao := investigationstore.NewLedgerDAO(store)
	if err := dao.Save(ctx, l); err != nil {
		t.Fatalf("save: %v", err)
	}
	rows, err := dao.Load(ctx, testInvestigationID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, h := range rows.Hypotheses {
		if h.ID != "h2" {
			continue
		}
		if h.UntestedReason == "" || h.NextQuery == nil || h.NextQueryDeepLink == "" {
			t.Errorf("the untested row lost its reason or its next query: %+v", h)
		}
		if h.NextQuery.GetCompare() == nil {
			t.Errorf("the stored next query is not the compare term it was written as: %+v", h.NextQuery)
		}
	}
}

// daoEvidence is a legal evidence item pinned to the seeded investigation's instants: the
// observed-time trigger refuses anything the investigation could not have known (Invariant 7).
func daoEvidence(id string) ledger.EvidenceItem {
	at := time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	return ledger.EvidenceItem{
		ID:             id,
		Kind:           "algebra_answer",
		Worker:         "metrics",
		Capability:     "compare",
		SourceOfTruth:  "prometheus",
		Term:           daoNextQuery(),
		ValidAt:        at,
		ObservedAt:     at,
		CalledAt:       at.Add(time.Minute),
		Mode:           "recorded",
		Outcome:        "digest",
		ResponseDigest: "sha256:" + id,
		ResponseKey:    "world/" + id,
		Coverage: &investigationv1.Coverage{
			DataSource:       "prometheus",
			VolumeConsidered: 1200,
		},
		JoinKeys:      &investigationv1.JoinKeys{Version: "rev7"},
		GraphEventIDs: []string{"evt-" + id},
		DeepLink:      "https://example.invalid/q/" + id,
	}
}

// daoNextQuery is a published algebra term: the compare an untested hypothesis would be tested by.
func daoNextQuery() *investigationv1.AlgebraTerm {
	return &investigationv1.AlgebraTerm{
		Term: &investigationv1.AlgebraTerm_Compare{
			Compare: &investigationv1.CompareTerm{Statistic: investigationv1.Statistic_ERROR_RATE},
		},
	}
}

// rankLine renders the ranked ids and confidences for a failure message.
func rankLine(hypotheses []ledger.Hypothesis) string {
	var b strings.Builder
	for i, h := range hypotheses {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%.6f", h.ID, h.Confidence)
	}
	return b.String()
}

// A rollback judgment is written and read back under its own source (004 T155). This is the migration
// 0013 check: the schema's source CHECK must admit `rollback`, or the first investigation that credits a
// rollback fails at commit — after the engine did its work.
func TestARollbackJudgmentRoundTripsUnderItsOwnSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedInvestigation(ctx, t, store)

	l := savedLedger(t)
	diff := daoEvidence("e-diff")
	diff.Kind = "graph_answer"
	diff.Worker = "graph"
	if err := l.AddEvidence(diff); err != nil {
		t.Fatalf("add evidence: %v", err)
	}
	if _, err := l.Judge(ledger.Judgment{
		ID: "j-rollback", HypothesisID: "h1", EvidenceID: "e-diff",
		Direction: ledger.Supports, Strength: ledger.Strong, Source: ledger.SourceRollback,
	}); err != nil {
		t.Fatalf("judge: %v", err)
	}

	dao := investigationstore.NewLedgerDAO(store)
	if err := dao.Save(ctx, l); err != nil {
		t.Fatalf("save: a rollback judgment was refused by the schema: %v", err)
	}
	rows, err := dao.Load(ctx, testInvestigationID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, j := range rows.Judgments {
		if j.ID == "j-rollback" {
			if j.Source != ledger.SourceRollback || j.Strength != ledger.Strong {
				t.Errorf("the rollback judgment loaded as %s/%s", j.Source, j.Strength)
			}
			return
		}
	}
	t.Error("the rollback judgment was not read back")
}
