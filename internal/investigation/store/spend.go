// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// nullableTimestamp turns an absent instant into a SQL NULL rather than into the zero time, which
// would read as 1 January year 1 and mean something quite different from "this did not happen".
func nullableTimestamp(ts *timestamppb.Timestamp) any {
	if ts == nil {
		return nil
	}
	return ts.AsTime().UTC()
}

// The spend row (FR-044, FR-048, data-model §investigation.budget_spend).
//
// One row per investigation, holding **both** halves: what the run was allowed to spend and what
// it spent. Keeping the limits is not redundancy — a profile is edited between one incident and
// the next, and a consumption figure read against today's profile would be read against a budget
// that was not in force. The price table version travels for the same reason: the monetary figure
// is derived, and a cost printed a year ago only means something beside the table it came from.

// SaveSpend writes the run's consumption. It is an upsert: a run persisted twice — a retry, a
// resumed conclusion — records one row, because the spend is a property of the run rather than
// an event in it.
func (d *InvestigationDAO) SaveSpend(
	ctx context.Context,
	investigationID, profile string,
	spend *investigationv1.BudgetSpend,
) error {
	if d == nil || d.store == nil {
		return errors.New("investigation store: save spend: no database")
	}
	if spend == nil {
		return nil
	}
	limits, err := marshalMessage(spend.GetLimits())
	if err != nil {
		return fmt.Errorf("investigation store: spend %s: limits: %w", investigationID, err)
	}
	if limits == nil {
		// `limits` is NOT NULL with a non-empty CHECK: a consumption figure with no budget beside
		// it is a number nobody can read as over or under.
		return fmt.Errorf("investigation store: spend %s: the budget profile in force is required "+
			"(FR-047)", investigationID)
	}
	// The consumed half is the report with its limits removed, so the two columns do not each
	// carry a copy of the profile.
	consumedMsg, ok := proto.Clone(spend).(*investigationv1.BudgetSpend)
	if !ok {
		return fmt.Errorf("investigation store: spend %s: clone", investigationID)
	}
	consumedMsg.Limits = nil
	consumed, err := marshalMessage(consumedMsg)
	if err != nil {
		return fmt.Errorf("investigation store: spend %s: consumed: %w", investigationID, err)
	}
	if consumed == nil {
		consumed = []byte(`{}`)
	}
	name := profile
	if name == "" {
		name = spend.GetLimits().GetName()
	}

	if _, err := d.store.Pool().Exec(ctx, `
		INSERT INTO investigation.budget_spend (
			investigation_id, profile, limits, consumed, reserve_entered_at, price_table_version)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (investigation_id) DO UPDATE SET
			profile = EXCLUDED.profile,
			limits = EXCLUDED.limits,
			consumed = EXCLUDED.consumed,
			reserve_entered_at = EXCLUDED.reserve_entered_at,
			price_table_version = EXCLUDED.price_table_version`,
		investigationID, name, limits, consumed,
		nullableTimestamp(spend.GetReserveEnteredAt()), spend.GetPriceTableVersion(),
	); err != nil {
		return fmt.Errorf("investigation store: spend %s: %w", investigationID, err)
	}
	return nil
}

// SpendOf reads the run's consumption back, or nil when none was recorded.
func (d *InvestigationDAO) SpendOf(ctx context.Context, investigationID string) (*investigationv1.BudgetSpend, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: spend: no database")
	}
	var limits, consumed []byte
	err := d.store.Pool().QueryRow(ctx, `
		SELECT limits, consumed FROM investigation.budget_spend WHERE investigation_id = $1`,
		investigationID).Scan(&limits, &consumed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("investigation store: spend %s: %w", investigationID, err)
	}
	spend := &investigationv1.BudgetSpend{}
	if err := unmarshalMessage(consumed, spend); err != nil {
		return nil, fmt.Errorf("investigation store: spend %s: consumed: %w", investigationID, err)
	}
	profile := &investigationv1.BudgetProfile{}
	if err := unmarshalMessage(limits, profile); err != nil {
		return nil, fmt.Errorf("investigation store: spend %s: limits: %w", investigationID, err)
	}
	spend.Limits = profile
	return spend, nil
}

// LatestPrior reads π₀ from the most recent published coverage audit (ADR-0005 D9, FR-069a).
//
// A deployment that has never run an audit gets π₀ = 1 with an empty audit id, and that is the
// only honest answer rather than a defensive default: no measured ceiling means no evidence that
// any cause is observable at all, so the whole prior mass belongs to *no observed change explains
// this* and every investigation reports `unknown` until a ceiling is measured. A silent 0 here
// would publish the opposite claim — that everything is visible — which is exactly the unearned
// confidence User Story 0 exists to prevent.
func LatestPrior(ctx context.Context, store *postgres.Store) (audit.PriorRecord, error) {
	if store == nil {
		return audit.PriorRecord{Prior: 1}, errors.New("investigation store: latest prior: no database")
	}
	var (
		auditID   string
		ceiling   float64
		count     int
		runAt     time.Time
		feederSet []byte
	)
	err := store.Pool().QueryRow(ctx, `
		SELECT audit_id, ceiling, incident_count, run_at, feeder_set
		FROM investigation.coverage_audits ORDER BY run_at DESC LIMIT 1`).
		Scan(&auditID, &ceiling, &count, &runAt, &feederSet)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return audit.PriorRecord{Prior: 1}, nil
	case err != nil:
		return audit.PriorRecord{Prior: 1}, fmt.Errorf("investigation store: latest prior: %w", err)
	}
	return audit.PriorRecord{
		Prior:         audit.PriorFromCeiling(ceiling),
		AuditID:       auditID,
		Ceiling:       ceiling,
		FeederSet:     feederSetName(feederSet),
		IncidentCount: count,
		RunAt:         audit.Instant{Time: runAt.UTC()},
	}, nil
}

// feederSetName reads the name of the feeder set in force out of the audit's stored JSON. The
// column holds the whole ladder; what the prior record carries is which rung was in force, and a
// shape this reader does not recognise yields an empty name rather than a guess.
func feederSetName(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var object struct {
		InForce string `json:"in_force"`
		Name    string `json:"name"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	if object.InForce != "" {
		return object.InForce
	}
	return object.Name
}
