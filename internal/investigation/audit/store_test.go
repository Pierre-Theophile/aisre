// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"context"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// TestPersistWritesTheAuditAndItsItems: the published JSON is the artifact, but a confidence
// recorded in a ledger years from now is only interpretable if the ceiling it came from is still
// readable from the same database (ADR-0005 D9).
func TestPersistWritesTheAuditAndItsItems(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	result := runFixture(t, "synthetic-01", audit.Options{})
	if err := audit.Persist(ctx, store, result); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	var (
		incidentCount int
		ceiling       float64
		inputDigest   string
		author        string
	)
	if err := store.Pool().QueryRow(ctx, `
		SELECT incident_count, ceiling, input_digest, author
		FROM investigation.coverage_audits WHERE audit_id = $1`, result.AuditID,
	).Scan(&incidentCount, &ceiling, &inputDigest, &author); err != nil {
		t.Fatalf("read back the audit: %v", err)
	}
	if incidentCount != 13 {
		t.Errorf("incident_count = %d, want 13 — the count the ceiling rests on", incidentCount)
	}
	if !closeTo(ceiling, wantCeiling) {
		t.Errorf("ceiling = %v, want %v", ceiling, wantCeiling)
	}
	if inputDigest != result.InputDigest {
		t.Errorf("input_digest = %q, want %q", inputDigest, result.InputDigest)
	}
	if author != result.Author {
		t.Errorf("author = %q, want %q", author, result.Author)
	}

	var items int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.coverage_audit_items WHERE audit_id = $1`,
		result.AuditID,
	).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 13 {
		t.Errorf("items = %d, want 13", items)
	}

	// The three classifications the column accepts must all be writable, and the stated cause
	// must be present on every row (the column is NOT NULL because the audit never infers).
	var absent int
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM investigation.coverage_audit_items
		WHERE audit_id = $1 AND classification = 'cause_absent' AND stated_cause <> ''`,
		result.AuditID,
	).Scan(&absent); err != nil {
		t.Fatalf("count absent: %v", err)
	}
	if absent != 5 {
		t.Errorf("cause_absent rows = %d, want 5 (2 symptom-only + 3 unobservable)", absent)
	}
}

// TestPersistIsIdempotent: `audit coverage --db …` can be run as often as the operator likes and
// the database always holds the current reading of that audit, never two.
func TestPersistIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	result := runFixture(t, "synthetic-01", audit.Options{})
	for range 3 {
		if err := audit.Persist(ctx, store, result); err != nil {
			t.Fatalf("Persist: %v", err)
		}
	}

	var audits, items int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.coverage_audits`).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.coverage_audit_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if audits != 1 || items != 13 {
		t.Errorf("audits/items = %d/%d after three runs, want 1/13", audits, items)
	}
}

// TestPersistRewritesWhenTheFeederSetInForceChanges: re-publishing the same audit against a
// lower rung must move the stored classification, not add a second row per incident — the schema
// is UNIQUE (audit_id, incident_ref) and says so.
func TestPersistRewritesWhenTheFeederSetInForceChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	best := runFixture(t, "synthetic-01", audit.Options{})
	if err := audit.Persist(ctx, store, best); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	weakest := runFixture(t, "synthetic-01", audit.Options{FeederSet: "feature-001"})
	if err := audit.Persist(ctx, store, weakest); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	var items, present int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.coverage_audit_items WHERE audit_id = $1`,
		best.AuditID).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 13 {
		t.Errorf("items = %d, want 13", items)
	}
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM investigation.coverage_audit_items
		WHERE audit_id = $1 AND classification = 'cause_present'`,
		best.AuditID).Scan(&present); err != nil {
		t.Fatalf("count present: %v", err)
	}
	if present != 0 {
		t.Errorf("cause_present rows = %d, want 0 with the 001 feeders alone", present)
	}

	var ceiling float64
	if err := store.Pool().QueryRow(ctx,
		`SELECT ceiling FROM investigation.coverage_audits WHERE audit_id = $1`,
		best.AuditID).Scan(&ceiling); err != nil {
		t.Fatalf("read ceiling: %v", err)
	}
	if !closeTo(ceiling, 0) {
		t.Errorf("ceiling = %v, want 0", ceiling)
	}
}

// TestPersistAnAggregateWritesTheAuditRowAlone: a published aggregate deliberately carries no
// incident-level detail, and the honest record of it is the audit row with no items.
func TestPersistAnAggregateWritesTheAuditRowAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	result := runFixture(t, "synthetic-01", audit.Options{AggregateOnly: true})
	if err := audit.Persist(ctx, store, result); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	var audits, items int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.coverage_audits`).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.coverage_audit_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if audits != 1 || items != 0 {
		t.Errorf("audits/items = %d/%d, want 1/0", audits, items)
	}
}

// TestPersistTwoRunsCoexist: FR-071a's comparison needs both runs on the record.
func TestPersistTwoRunsCoexist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	for _, name := range []string{"synthetic-01", "synthetic-02"} {
		if err := audit.Persist(ctx, store, runFixture(t, name, audit.Options{})); err != nil {
			t.Fatalf("Persist(%s): %v", name, err)
		}
	}

	rows, err := store.Pool().Query(ctx,
		`SELECT audit_id, ceiling FROM investigation.coverage_audits ORDER BY audit_id`)
	if err != nil {
		t.Fatalf("query audits: %v", err)
	}
	defer rows.Close()

	got := map[string]float64{}
	for rows.Next() {
		var id string
		var ceiling float64
		if err := rows.Scan(&id, &ceiling); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = ceiling
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("audits = %d, want 2", len(got))
	}
	if !closeTo(got["synthetic-01"], wantCeiling) || !closeTo(got["synthetic-02"], 0.692308) {
		t.Errorf("ceilings = %v, want 0.615385 and 0.692308", got)
	}
}

func TestPersistRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	result := runFixture(t, "synthetic-01", audit.Options{AggregateOnly: true})
	if err := audit.Persist(ctx, nil, result); err == nil {
		t.Error("Persist accepted a nil store")
	}

	store := pgtest.Open(t)
	if err := audit.Persist(ctx, store, nil); err == nil {
		t.Error("Persist accepted a nil result")
	}

	unsigned := *result
	unsigned.Author = ""
	if err := audit.Persist(ctx, store, &unsigned); err == nil {
		t.Error("Persist accepted an audit nobody signed")
	}

	anonymous := *result
	anonymous.AuditID = ""
	if err := audit.Persist(ctx, store, &anonymous); err == nil {
		t.Error("Persist accepted an audit with no id")
	}
}

// storeCompiles keeps the postgres import honest if the tests above are ever trimmed.
var _ = (*postgres.Store)(nil)
