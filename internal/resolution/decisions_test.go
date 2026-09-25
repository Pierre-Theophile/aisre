// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"path/filepath"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The rules a human decision has to obey, checked against a real graph (FR-040, FR-041, SC-007,
// SC-011).
//
// The fixture's goldens already pin *what* the graph answers. These tests pin the three
// properties that no golden can express, because they are statements about what the graph
// refuses and about what is true of every row rather than of one query:
//
//   - a decision with no authenticated individual is refused before it reaches the log (FR-041);
//   - no automated merge ever comes from a probable rule (SC-007);
//   - every recorded human decision names a principal (SC-011).

const ambiguousIdentityFixture = "ambiguous-identity-01"

// loadAmbiguousIdentity loads the resolution fixture into a fresh database and returns it.
func loadAmbiguousIdentity(t *testing.T) (*postgres.Store, *projector.Projector) {
	t.Helper()
	store := pgtest.Open(t)
	p := projector.New(store)
	report, err := fixture.Load(context.Background(), p,
		filepath.Join(fixturesDir, ambiguousIdentityFixture), fixture.LoadOptions{})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if len(report.RejectedMismatches) > 0 {
		t.Fatalf("fixture did not load cleanly: %v", report.RejectedMismatches)
	}
	return store, p
}

func TestNoAutomatedMergeComesFromAProbableRule(t *testing.T) {
	store, _ := loadAmbiguousIdentity(t)

	var count int
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM graph.resolution_decisions
		WHERE kind = 'auto_merge' AND rule_id LIKE 'P%'`).Scan(&count); err != nil {
		t.Fatalf("count probable auto-merges: %v", err)
	}
	if count != 0 {
		t.Errorf("%d automated merge(s) came from a probable rule; SC-007 requires zero", count)
	}

	// And the pair a probable rule did match is still two entities, listed as a suggestion.
	var status string
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT status FROM graph.suggestions WHERE rule_id = 'P1' AND status = 'pending'`).Scan(&status); err != nil {
		t.Fatalf("read the pending suggestion: %v", err)
	}
}

func TestEveryHumanDecisionNamesAnIndividual(t *testing.T) {
	store, _ := loadAmbiguousIdentity(t)

	rows, err := store.Pool().Query(context.Background(), `
		SELECT decision_id, kind, coalesce(principal, '')
		FROM graph.resolution_decisions
		WHERE kind IN ('confirm', 'manual_merge', 'reject', 'split')
		ORDER BY decided_at, decision_id`)
	if err != nil {
		t.Fatalf("read human decisions: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var id, kind, principal string
		if err := rows.Scan(&id, &kind, &principal); err != nil {
			t.Fatalf("scan decision: %v", err)
		}
		seen++
		if principal == "" {
			t.Errorf("%s decision %s carries no principal; SC-011 requires one on every decision", kind, id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read human decisions: %v", err)
	}
	if seen == 0 {
		t.Fatal("the fixture recorded no human decisions; it is supposed to record two")
	}
}

func TestAnonymousDecisionIsRefused(t *testing.T) {
	_, p := loadAmbiguousIdentity(t)
	ctx := context.Background()

	if err := p.RegisterSource(ctx, eventlog.Source{
		SourceID: "human", Kind: "human", Ordering: "none", SchemaVersion: "1.0.0",
	}); err != nil {
		t.Fatalf("register the human source: %v", err)
	}

	env := &graphv1.EventEnvelope{
		EventId:        "test:anonymous-confirm",
		IdempotencyKey: "test:anonymous-confirm",
		SourceId:       "human",
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_ConfirmMerge{ConfirmMerge: &graphv1.ConfirmMerge{
			EntityA:   "otel.service.name=notifications",
			EntityB:   "k8s.deployment=ops/notifications-svc",
			Rationale: "nobody in particular says so",
		}},
	}
	result, err := p.ApplyWithOptions(ctx, env, projector.ApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Fatalf("status = %s, want REJECTED: FR-041 forbids an anonymous resolution decision",
			result.GetStatus())
	}
	if result.GetReasonCode() != eventlog.ReasonMissingPrincipal {
		t.Errorf("reason_code = %q, want %q", result.GetReasonCode(), eventlog.ReasonMissingPrincipal)
	}
}

func TestDecisionNamingAnUnknownEntityIsRefused(t *testing.T) {
	_, p := loadAmbiguousIdentity(t)
	ctx := context.Background()

	if err := p.RegisterSource(ctx, eventlog.Source{
		SourceID: "human", Kind: "human", Ordering: "none", SchemaVersion: "1.0.0",
	}); err != nil {
		t.Fatalf("register the human source: %v", err)
	}

	env := &graphv1.EventEnvelope{
		EventId:        "test:unknown-confirm",
		IdempotencyKey: "test:unknown-confirm",
		SourceId:       "human",
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_ConfirmMerge{ConfirmMerge: &graphv1.ConfirmMerge{
			EntityA:   "otel.service.name=notifications",
			EntityB:   "otel.service.name=does-not-exist",
			Rationale: "typo",
		}},
	}
	result, err := p.ApplyWithOptions(ctx, env, projector.ApplyOptions{Principal: "sre-agent-dev|alice"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Fatalf("status = %s, want REJECTED", result.GetStatus())
	}
	if result.GetReasonCode() != eventlog.ReasonRefUnresolvable {
		t.Errorf("reason_code = %q, want %q", result.GetReasonCode(), eventlog.ReasonRefUnresolvable)
	}
}

func TestRepeatingADecisionIsANoop(t *testing.T) {
	_, p := loadAmbiguousIdentity(t)
	ctx := context.Background()

	if err := p.RegisterSource(ctx, eventlog.Source{
		SourceID: "human", Kind: "human", Ordering: "none", SchemaVersion: "1.0.0",
	}); err != nil {
		t.Fatalf("register the human source: %v", err)
	}

	const principal = "sre-agent-dev|bob"
	const rationale = "same thing, different name"
	refs := []string{"otel.service.name=notifications", "k8s.deployment=ops/notifications-svc"}
	build := func() *graphv1.EventEnvelope {
		id := graph.HumanEventID("confirm", refs, principal, rationale)
		return &graphv1.EventEnvelope{
			EventId:        id,
			IdempotencyKey: id,
			SourceId:       "human",
			SchemaVersion:  "1.0.0",
			Body: &graphv1.EventEnvelope_ConfirmMerge{ConfirmMerge: &graphv1.ConfirmMerge{
				EntityA: refs[0], EntityB: refs[1], Rationale: rationale,
			}},
		}
	}

	first, err := p.ApplyWithOptions(ctx, build(), projector.ApplyOptions{Principal: principal})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if first.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Fatalf("first status = %s (%s: %s), want APPLIED",
			first.GetStatus(), first.GetReasonCode(), first.GetReasonDetail())
	}
	second, err := p.ApplyWithOptions(ctx, build(), projector.ApplyOptions{Principal: principal})
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if second.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
		t.Errorf("second status = %s, want DUPLICATE_NOOP: the same decision is the same event",
			second.GetStatus())
	}
}
