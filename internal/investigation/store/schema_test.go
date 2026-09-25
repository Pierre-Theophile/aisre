// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// investigationMigrationVersion is the version of 0006_investigation.sql. It is 0006 and not
// 0005 on purpose: the feature 001 change package lands 0005_actor_kind.sql (002 tasks.md
// T020) and migrations are numbered in the order they are applied.
const investigationMigrationVersion int64 = 6

// relations is every table data-model.md "Schema investigation" names. The list is spelled out
// rather than read back from the database, so a table quietly dropped from the migration fails
// this test instead of shrinking the expectation with it.
var relations = []string{
	"investigation.incidents",
	"investigation.investigations",
	"investigation.symptoms",
	"investigation.report_deliveries",
	"investigation.hypotheses",
	"investigation.judgments",
	"investigation.evidence_items",
	"investigation.worker_calls",
	"investigation.model_calls",
	"investigation.recordings",
	"investigation.human_facts",
	"investigation.human_reviews",
	"investigation.labels",
	"investigation.budget_spend",
	"investigation.verifier_findings",
	"investigation.coverage_audits",
	"investigation.coverage_audit_items",
}

// TestSchemaAppliesFromEmpty is the migration up test: a database that has never seen
// sre-agent ends up with schema `investigation` and every table of data-model.md.
func TestSchemaAppliesFromEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	for _, relation := range relations {
		var exists bool
		if err := store.Pool().QueryRow(ctx,
			`SELECT to_regclass($1) IS NOT NULL`, relation,
		).Scan(&exists); err != nil {
			t.Fatalf("look up %s: %v", relation, err)
		}
		if !exists {
			t.Errorf("relation %s does not exist after Migrate", relation)
		}
	}
}

// TestInvariantsAreEnforcedByTheSchema checks that the five invariants 0006 is responsible for
// are enforced by the database and not only by the code that writes to it. A constraint that
// exists only in a DAO is a convention, and this feature's whole argument is that the ledger's
// arithmetic is not a convention.
func TestInvariantsAreEnforcedByTheSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	// Invariants 1 and 2 (the ledger), 4 (coverage), and the judgment uniqueness that makes
	// the posterior a function of the rows rather than of the order they arrived in.
	constraints := []struct {
		relation  string
		name      string
		invariant string
	}{
		{"investigation.hypotheses", "hypotheses_prior_range_check", "1: a prior is a probability"},
		{"investigation.hypotheses", "hypotheses_confidence_range_check", "1: a confidence is a probability"},
		{"investigation.evidence_items", "evidence_items_coverage_present_check", "4: no evidence item without a coverage block"},
		{"investigation.judgments", "judgments_one_per_pair", "one judgment per (hypothesis, evidence) pair"},
	}
	for _, c := range constraints {
		var exists bool
		if err := store.Pool().QueryRow(ctx,
			`SELECT EXISTS (
			     SELECT 1 FROM pg_constraint
			     WHERE conrelid = $1::regclass AND conname = $2
			 )`, c.relation, c.name,
		).Scan(&exists); err != nil {
			t.Fatalf("look up constraint %s: %v", c.name, err)
		}
		if !exists {
			t.Errorf("constraint %s on %s is missing (invariant %s)", c.name, c.relation, c.invariant)
		}
	}

	// Invariant 2's structural half: at most one no_observed_change hypothesis per
	// investigation, as a partial unique index.
	var uniqueIndex bool
	if err := store.Pool().QueryRow(ctx,
		`SELECT indisunique FROM pg_index
		 WHERE indexrelid = 'investigation.hypotheses_one_no_observed_change_per_investigation'::regclass`,
	).Scan(&uniqueIndex); err != nil {
		t.Fatalf("look up the no_observed_change index: %v", err)
	}
	if !uniqueIndex {
		t.Error("hypotheses_one_no_observed_change_per_investigation is not a unique index (invariant 2)")
	}

	// Invariants 7 and 10 cannot be CHECKs -- one reads another table, the other is a property
	// of a set -- so they are triggers, and a trigger that is not installed enforces nothing.
	triggers := []struct {
		relation  string
		name      string
		deferred  bool
		invariant string
	}{
		{"investigation.evidence_items", "evidence_items_observed_time_pin", false, "7: the observed-time pin"},
		{"investigation.hypotheses", "hypotheses_posteriors_sum_to_one", true, "10: posteriors sum to 1.0 +/- 1e-6"},
		// 0008: FR-007's immutability, which was a guarded UPDATE in one DAO and is now the
		// database's rule.
		{"investigation.investigations", "investigations_concluded_is_immutable", false, "FR-007: a concluded investigation is never edited"},
	}
	for _, tr := range triggers {
		var initiallyDeferred bool
		err := store.Pool().QueryRow(ctx,
			`SELECT tgdeferrable AND tginitdeferred FROM pg_trigger
			 WHERE tgrelid = $1::regclass AND tgname = $2 AND NOT tgisinternal`,
			tr.relation, tr.name,
		).Scan(&initiallyDeferred)
		if err != nil {
			t.Errorf("trigger %s on %s is missing (invariant %s): %v", tr.name, tr.relation, tr.invariant, err)
			continue
		}
		if initiallyDeferred != tr.deferred {
			t.Errorf("trigger %s deferred = %t, want %t (invariant %s)",
				tr.name, initiallyDeferred, tr.deferred, tr.invariant)
		}
	}
}

// TestMigrationNumberingLeavesFiveReserved is the numbering decision, executed.
//
// 0005 is reserved for the feature 001 change package's 0005_actor_kind.sql and is not in this
// repository yet, so the embedded set has a gap. Migrate applies whatever is embedded in
// ascending version order and records each one; nothing anywhere requires the numbers to be
// contiguous, and this test is what says so out loud rather than leaving it to be rediscovered
// when 0005 lands and has to be slotted in ahead of an already-applied 0006.
func TestMigrationNumberingActorKindThenInvestigation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	embedded, err := postgres.Migrations()
	if err != nil {
		t.Fatalf("embedded migrations: %v", err)
	}

	// Phase 1 of feature 002 shipped 0006 (the investigation schema) while 0005 was reserved
	// for feature 001's change package; Phase 3 then filled it with 0005_actor_kind.sql. Both
	// must be embedded, in order, and Migrate must apply them whichever landed first.
	var haveFive, haveSix bool
	previous := int64(0)
	for _, m := range embedded {
		if m.Version <= previous {
			t.Fatalf("embedded migrations are not in ascending order: %d after %d", m.Version, previous)
		}
		previous = m.Version
		switch m.Version {
		case 5:
			haveFive = true
			if m.Name != "0005_actor_kind.sql" {
				t.Errorf("version 5 is %s, want 0005_actor_kind.sql (feature 001 change package)", m.Name)
			}
		case investigationMigrationVersion:
			haveSix = true
			if m.Name != "0006_investigation.sql" {
				t.Errorf("version 6 is %s, want 0006_investigation.sql", m.Name)
			}
		}
	}
	if !haveFive || !haveSix {
		t.Fatalf("embedded migrations must include 0005_actor_kind.sql and 0006_investigation.sql (have5=%v have6=%v)", haveFive, haveSix)
	}

	store := pgtest.Open(t)
	applied, err := store.AppliedVersions(ctx)
	if err != nil {
		t.Fatalf("applied versions: %v", err)
	}
	var appliedFive, appliedSix bool
	for _, v := range applied {
		if v == 5 {
			appliedFive = true
		}
		if v == investigationMigrationVersion {
			appliedSix = true
		}
	}
	if !appliedFive || !appliedSix {
		t.Fatalf("applied versions = %v, want both 5 and 6 among them", applied)
	}

	// Migrate is idempotent: a second call is a no-op, not a re-apply.
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

// TestMigrationIsAtomic is the down half of the up/down test.
//
// There is no down migration in this project -- history is append-only and a mistake is
// corrected forward -- so "rolls back" means what it means in PostgreSQL: the whole file is one
// transaction, and a transaction that does not commit leaves nothing behind. This drops the
// schema and re-applies the migration inside one transaction, checks it is there, rolls back,
// and checks the original schema is untouched.
func TestMigrationIsAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	embedded, err := postgres.Migrations()
	if err != nil {
		t.Fatalf("embedded migrations: %v", err)
	}
	var body string
	for _, m := range embedded {
		if m.Version == investigationMigrationVersion {
			body = stripOuterTransaction(m.SQL)
		}
	}
	if body == "" {
		t.Fatal("0006_investigation.sql is not embedded")
	}

	tx, err := store.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "DROP SCHEMA investigation CASCADE"); err != nil {
		t.Fatalf("drop schema inside the transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, body); err != nil {
		t.Fatalf("re-apply 0006 inside the transaction: %v", err)
	}
	assertRelations(ctx, t, tx, true, "inside the transaction, after re-applying")

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	// The rollback undid the drop as well as the re-apply, so the database is exactly where
	// Migrate left it.
	assertRelations(ctx, t, store.Pool(), true, "after rollback")
}

// querier is the subset of pgx both a pool and a transaction satisfy.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func assertRelations(ctx context.Context, t *testing.T, q querier, want bool, when string) {
	t.Helper()
	for _, relation := range relations {
		var exists bool
		if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, relation).Scan(&exists); err != nil {
			t.Fatalf("look up %s %s: %v", relation, when, err)
		}
		if exists != want {
			t.Errorf("%s exists = %t %s, want %t", relation, exists, when, want)
		}
	}
}

// stripOuterTransaction removes the file's own BEGIN;/COMMIT; so the body can run inside a
// transaction this test controls. It mirrors what Migrate does when it applies the file
// alongside its bookkeeping row.
func stripOuterTransaction(sql string) string {
	body := strings.TrimSpace(sql)
	if idx := strings.Index(body, "\nBEGIN;"); idx >= 0 {
		body = body[idx+len("\nBEGIN;"):]
	}
	if idx := strings.LastIndex(body, "COMMIT;"); idx >= 0 {
		body = body[:idx]
	}
	return strings.TrimSpace(body)
}

// TestEvidenceItemsRecordTheSourceOfTruth is 0008's first half (FR-022).
//
// The declaration the answering worker made is a column, not something a reader re-derives from a
// worker registry that may since have been renamed or retired. "May this item carry a hypothesis
// to `supported`?" has to be answerable from the rows years later.
func TestEvidenceItemsRecordTheSourceOfTruth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	var notNull bool
	if err := store.Pool().QueryRow(ctx, `
		SELECT attnotnull FROM pg_attribute
		WHERE attrelid = 'investigation.evidence_items'::regclass AND attname = 'source_of_truth'`,
	).Scan(&notNull); err != nil {
		t.Fatalf("investigation.evidence_items has no source_of_truth column (0008, FR-022): %v", err)
	}
	if !notNull {
		t.Error("source_of_truth is nullable; an empty declaration is a real value — a knowledge " +
			"item speaks for no source of truth — and NULL would be a second way to say it")
	}
}
