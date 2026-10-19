// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// Aliases (feature 009, ADR-0015 item 3, research R2, R9).
//
// An alias says that a name — a ref namespace and value, optionally in one environment — denotes a
// catalog entry. It is a row of graph.catalog_alias_versions and never a merge: the entities that
// carry the name stay what they are, so production and staging keep their own histories while both
// are aliased to one entry.
//
// At one observed instant a name denotes at most one entry. Declaring it for another entry closes
// the open row and opens a new one (a reassignment); retracting it closes the row. History is never
// rewritten: closing an open interval through graph.close_observed is the one mutation.
//
// Who may open a row (FR-050): a person (declare_alias, a name-level correction), or the rule that
// carried it with a proposed entry (D1, from 001's certain rules). A probable rule never does; it
// files a proposal (propose_alias) that waits for a person.

// Alias proposal statuses, as graph.alias_proposals stores them. The same four as
// graph.proposed_dependencies, for the same reasons (propose_dependency.go).
const (
	aliasProposalPending  = "pending"
	aliasProposalAccepted = "accepted"
	aliasProposalRejected = "rejected"
	aliasProposalConflict = "conflict"
)

// aliasRow is one alias decision about to be projected.
type aliasRow struct {
	name        graph.Ref
	environment string
	entrySlug   string
	principal   string
	ruleID      string
	eventID     string
}

// openAliasRow is the current version of a name, as stored.
type openAliasRow struct {
	versionID string
	entrySlug string
	eventID   string
	// byPerson is whether a person decided it, rather than a rule.
	byPerson bool
}

// openAlias makes row the name's current alias. When the name already denotes another entry, a
// person's decision (reassign) closes that row first; a rule-carried alias never overrides anything
// and is dropped instead.
func (p *Projector) openAlias(ctx context.Context, tx pgx.Tx, row aliasRow, observedAt time.Time, reassign bool) error {
	current, found, err := p.currentAlias(ctx, tx, row.name, row.environment)
	if err != nil {
		return err
	}
	if found {
		// Already so. Only a person confirming what a rule carried replaces the row, so the alias
		// then names who decided it (FR-050).
		if current.entrySlug == row.entrySlug && (current.byPerson || row.principal == "") {
			return nil
		}
		if !reassign {
			return nil
		}
		if err := closeObserved(ctx, tx, "catalog_alias_versions", current.versionID, observedAt, row.eventID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.catalog_alias_versions
			(version_id, namespace, value, environment, entry_slug, observed, decided_by_event_id,
			 principal, rule_id)
		VALUES ($1, $2, $3, $4, $5, tstzrange($6, NULL, '[)'), $7, $8, $9)`,
		graph.AliasVersionID(row.name.Namespace, row.name.Value, row.environment, row.eventID),
		row.name.Namespace, row.name.Value, row.environment, row.entrySlug, observedAt.UTC(),
		row.eventID, nullableString(row.principal), nullableString(row.ruleID)); err != nil {
		return fmt.Errorf("projector: open alias %s: %w", row.name, err)
	}
	return nil
}

// currentAlias reads the open alias version of a name in one environment ("" for all).
func (p *Projector) currentAlias(ctx context.Context, tx pgx.Tx, name graph.Ref, environment string) (openAliasRow, bool, error) {
	var row openAliasRow
	err := tx.QueryRow(ctx, `
		SELECT version_id, entry_slug, decided_by_event_id, principal IS NOT NULL
		FROM graph.catalog_alias_versions
		WHERE namespace = $1 AND value = $2 AND environment = $3 AND upper_inf(observed)`,
		name.Namespace, name.Value, environment).Scan(&row.versionID, &row.entrySlug, &row.eventID, &row.byPerson)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return row, false, nil
	case err != nil:
		return row, false, fmt.Errorf("projector: read alias %s: %w", name, err)
	}
	return row, true, nil
}

func (p *Projector) applyDeclareAlias(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.DeclareAlias, observedAt time.Time, principal string,
) error {
	name := graph.RefFromProto(body.GetName())
	if err := p.openAlias(ctx, tx, aliasRow{
		name:        name,
		environment: body.GetEnvironment(),
		entrySlug:   body.GetEntrySlug(),
		principal:   principal,
		eventID:     env.GetEventId(),
	}, observedAt, true); err != nil {
		return err
	}
	// Accepting a proposal is declaring what it proposed.
	return p.decideAliasProposal(ctx, tx, graph.AliasKey(name, body.GetEnvironment(), body.GetEntrySlug()),
		aliasProposalAccepted, principal, env.GetEventId(), observedAt, false)
}

func (p *Projector) applyRetractAlias(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.RetractAlias, observedAt time.Time,
) error {
	current, found, err := p.currentAlias(ctx, tx, graph.RefFromProto(body.GetName()), body.GetEnvironment())
	if err != nil {
		return err
	}
	if !found || current.entrySlug != body.GetEntrySlug() {
		// checkResolvable refuses this before the append; on replay the log already agreed.
		return nil
	}
	return closeObserved(ctx, tx, "catalog_alias_versions", current.versionID, observedAt, env.GetEventId())
}

// applyRejectAlias records that a person said a name is not this entry. It touches no alias row:
// rejecting a suggestion is not retracting a decision. A rejection with no proposal behind it is
// kept too, so the attributor never proposes that pair later (FR-011).
func (p *Projector) applyRejectAlias(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.RejectAlias, observedAt time.Time, principal string,
) error {
	name := graph.RefFromProto(body.GetName())
	return p.decideAliasProposal(ctx, tx, graph.AliasKey(name, body.GetEnvironment(), body.GetEntrySlug()),
		aliasProposalRejected, principal, env.GetEventId(), observedAt, true, aliasProposalSeed{
			name: name, environment: body.GetEnvironment(), entrySlug: body.GetEntrySlug(),
			rationale: body.GetRationale(),
		})
}

// aliasProposalSeed is what a rejection with no proposal behind it files.
type aliasProposalSeed struct {
	name        graph.Ref
	environment string
	entrySlug   string
	rationale   string
}

// decideAliasProposal moves a proposal to a terminal status. With create, a decision on a pair
// nobody proposed files the row, so the decision outlives the absence of a proposal.
func (p *Projector) decideAliasProposal(ctx context.Context, tx pgx.Tx, key, status, principal, eventID string,
	observedAt time.Time, create bool, seed ...aliasProposalSeed,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE graph.alias_proposals
		SET status = $2, decided_by = $3, decided_at = $4, decision_event_id = $5
		WHERE proposal_key = $1`, key, status, principal, observedAt.UTC(), eventID)
	if err != nil {
		return fmt.Errorf("projector: %s alias proposal %s: %w", status, key, err)
	}
	if tag.RowsAffected() > 0 || !create || len(seed) == 0 {
		return nil
	}
	s := seed[0]
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.alias_proposals
			(proposal_key, namespace, value, environment, entry_slug, rule_id, score, rationale,
			 created_at, status, decided_by, decided_at, decision_event_id)
		VALUES ($1, $2, $3, $4, $5, 'H', 1, $6, $7, $8, $9, $7, $10)`,
		key, s.name.Namespace, s.name.Value, s.environment, s.entrySlug, s.rationale,
		observedAt.UTC(), status, principal, eventID); err != nil {
		return fmt.Errorf("projector: file %s alias decision %s: %w", status, key, err)
	}
	return nil
}

// applyProposeAlias files a probable rule's suggestion, or declines to refile one: the precedence of
// applyProposeDependency, for names (research R9).
func (p *Projector) applyProposeAlias(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.ProposeAlias, observedAt time.Time,
) error {
	name := graph.RefFromProto(body.GetName())
	if current, found, err := p.currentAlias(ctx, tx, name, body.GetEnvironment()); err != nil {
		return err
	} else if found && current.entrySlug == body.GetEntrySlug() {
		return nil
	}

	key := graph.AliasKey(name, body.GetEnvironment(), body.GetEntrySlug())
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM graph.alias_proposals WHERE proposal_key = $1`, key).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("projector: read alias proposal %s: %w", key, err)
	case status == aliasProposalRejected || status == aliasProposalConflict:
		// A person said no; a rule saying it again is recorded, never reopened (FR-011).
		return p.appendAliasProposalEvent(ctx, tx, key, aliasProposalConflict, env.GetEventId())
	default:
		return p.appendAliasProposalEvent(ctx, tx, key, status, env.GetEventId())
	}

	evidence, err := marshalEvidence(body.GetEvidence())
	if err != nil {
		return fmt.Errorf("projector: propose alias %s: %w", key, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.alias_proposals
			(proposal_key, namespace, value, environment, entry_slug, rule_id, score, rationale,
			 evidence, created_at, status, proposed_by_event_ids)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, ARRAY[$12]::text[])`,
		key, name.Namespace, name.Value, body.GetEnvironment(), body.GetEntrySlug(), body.GetRuleId(),
		body.GetScore(), body.GetRationale(), evidence, observedAt.UTC(), aliasProposalPending,
		env.GetEventId()); err != nil {
		return fmt.Errorf("projector: propose alias %s: %w", key, err)
	}
	return nil
}

func (p *Projector) appendAliasProposalEvent(ctx context.Context, tx pgx.Tx, key, status, eventID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE graph.alias_proposals
		SET status = $2,
		    proposed_by_event_ids =
				CASE WHEN proposed_by_event_ids @> ARRAY[$3]::text[]
				     THEN proposed_by_event_ids
				     ELSE proposed_by_event_ids || ARRAY[$3]::text[]
				END
		WHERE proposal_key = $1`, key, status, eventID); err != nil {
		return fmt.Errorf("projector: record alias proposal %s: %w", key, err)
	}
	return nil
}

// checkEntries refuses a catalog event naming an entry the graph has never seen. "-" (a hidden
// name) has no node and is always known.
func (p *Projector) checkEntries(ctx context.Context, tx pgx.Tx, field string, slugs ...string) (*eventlog.Rejection, error) {
	for _, slug := range slugs {
		if slug == graph.HiddenEntrySlug {
			continue
		}
		if _, found, err := p.lookupRef(ctx, tx, graph.CatalogRef(slug)); err != nil {
			return nil, err
		} else if !found {
			return &eventlog.Rejection{
				ReasonCode: eventlog.ReasonRefUnresolvable,
				ReasonDetail: fmt.Sprintf("%s names the catalog entry %q, which does not exist; declare it "+
					"first", field, slug),
			}, nil
		}
	}
	return nil, nil
}

// checkRetractAlias refuses a retraction of an alias that is not current.
func (p *Projector) checkRetractAlias(ctx context.Context, tx pgx.Tx, body *graphv1.RetractAlias) (*eventlog.Rejection, error) {
	name := graph.RefFromProto(body.GetName())
	current, found, err := p.currentAlias(ctx, tx, name, body.GetEnvironment())
	if err != nil {
		return nil, err
	}
	if !found || current.entrySlug != body.GetEntrySlug() {
		return &eventlog.Rejection{
			ReasonCode: eventlog.ReasonRefUnresolvable,
			ReasonDetail: fmt.Sprintf("retract_alias: %s (environment %q) is not currently an alias of %q",
				name, body.GetEnvironment(), body.GetEntrySlug()),
		}, nil
	}
	return nil, nil
}
