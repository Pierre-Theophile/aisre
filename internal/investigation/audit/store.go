// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Persisting an audit (T012, data-model §investigation.coverage_audits).
//
// The published JSON is the artifact; the two tables are the record. They exist because ADR-0005
// D9 records π₀ against an audit id in every investigation's ledger, and a confidence stored in
// 2026 is only interpretable in 2028 if the ceiling it was computed from is still readable from
// the same database. `scripts/check-migrations.sh` refuses any later migration that drops or
// deletes from `investigation.coverage_audits` for exactly that reason.
//
// Two decisions are worth stating.
//
// The item rows record the classification under the **feeder set in force**, not one row per
// feeder set per incident: `coverage_audit_items` is UNIQUE (audit_id, incident_ref), which is
// the schema saying that an audit has one verdict per incident. The per-rung detail lives in the
// published JSON, which is where `compare` reads it from.
//
// Writing is idempotent. Re-running the audit over the same list rewrites the same rows rather
// than accumulating a second copy under a new id, so `audit coverage --db …` can be run as often
// as the operator likes and the database always holds the current reading of that audit.

// Persist writes an audit run to `investigation.coverage_audits` and
// `investigation.coverage_audit_items` in one transaction.
//
// A result with no items — a published aggregate — writes the audit row alone, which is the
// honest record of an audit whose incident-level detail was deliberately not carried.
func Persist(ctx context.Context, store *postgres.Store, result *Result) error {
	if store == nil {
		return fmt.Errorf("audit: persist: no database")
	}
	if result == nil {
		return fmt.Errorf("audit: persist: no audit result")
	}
	if result.AuditID == "" {
		return fmt.Errorf("audit: persist: audit_id is required")
	}
	if result.Author == "" {
		return fmt.Errorf("audit: persist: author is required: a ceiling nobody signed is not a measurement")
	}

	feederSet, err := feederSetJSON(result)
	if err != nil {
		return err
	}

	return store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The author is a FK into graph.principals, which is what makes the ceiling
		// attributable (FR-069a). An audit is often the first thing a new principal does,
		// so the row is upserted rather than assumed.
		if _, err := tx.Exec(ctx, `
			INSERT INTO graph.principals (principal)
			VALUES ($1)
			ON CONFLICT (principal) DO UPDATE SET last_seen = now()`,
			result.Author,
		); err != nil {
			return fmt.Errorf("audit: persist: record author %s: %w", result.Author, err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO investigation.coverage_audits
				(audit_id, run_at, incident_count, ceiling, feeder_set, input_digest, author)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (audit_id) DO UPDATE SET
				run_at = EXCLUDED.run_at,
				incident_count = EXCLUDED.incident_count,
				ceiling = EXCLUDED.ceiling,
				feeder_set = EXCLUDED.feeder_set,
				input_digest = EXCLUDED.input_digest,
				author = EXCLUDED.author`,
			result.AuditID, result.RunAt.Time, result.ClassifiableCount, result.Ceiling,
			feederSet, result.InputDigest, result.Author,
		); err != nil {
			return fmt.Errorf("audit: persist: audit %s: %w", result.AuditID, err)
		}

		for _, item := range result.Items {
			if err := persistItem(ctx, tx, result.AuditID, item); err != nil {
				return err
			}
		}
		return nil
	})
}

func persistItem(ctx context.Context, tx pgx.Tx, auditID string, item Item) error {
	var category any
	if item.Category != "" {
		category = string(item.Category)
	}
	var reason any
	if item.Reason != "" {
		reason = string(item.Reason)
	}
	var matched any
	if item.MatchedEntityID != "" {
		matched = item.MatchedEntityID
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation.coverage_audit_items
			(item_id, audit_id, incident_ref, alert_at, stated_cause, classification,
			 category, reason, matched_entity_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (audit_id, incident_ref) DO UPDATE SET
			item_id = EXCLUDED.item_id,
			alert_at = EXCLUDED.alert_at,
			stated_cause = EXCLUDED.stated_cause,
			classification = EXCLUDED.classification,
			category = EXCLUDED.category,
			reason = EXCLUDED.reason,
			matched_entity_id = EXCLUDED.matched_entity_id`,
		item.ItemID, auditID, item.IncidentRef, item.AlertAt.Time, item.StatedCause,
		string(item.Classification), category, reason, matched,
	); err != nil {
		return fmt.Errorf("audit: persist: item %s: %w", item.IncidentRef, err)
	}
	return nil
}

// feederSetJSON renders `coverage_audits.feeder_set`: the configuration the ceiling was measured
// against, not merely its name, so the row stays readable when the ladder is rewritten.
func feederSetJSON(result *Result) ([]byte, error) {
	set, ok := result.FeederSet(result.FeederSetInForce)
	if !ok {
		return nil, fmt.Errorf("audit: persist: feeder set in force %q is not in the result",
			result.FeederSetInForce)
	}
	payload := map[string]any{
		"name":           set.Name,
		"order":          set.Order,
		"feeders":        set.Feeders,
		"observed":       set.Observed,
		"symptom_only":   set.SymptomOnly,
		"not_observable": set.NotObservable,
		"undecidable":    set.Undecidable,
		"classifiable":   set.Classifiable,
		"prior":          set.Prior,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("audit: persist: encode feeder set: %w", err)
	}
	return encoded, nil
}
