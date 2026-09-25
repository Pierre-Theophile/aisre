// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// Suggestions and the precedence of a human decision (FR-037, FR-038, FR-040, SC-007).
//
// A probable rule may not merge. What it may do is leave a row saying "these two look like one
// thing, here is the rule, the score and the reason" for a person to decide, and that row is the
// whole of this file. ADR-0001 D6 is the rule it enforces and it is enforced structurally: the
// only caller that reaches p.merge is the certain branch of the resolution pass, and a match
// arriving here with Certain set is a programming error rather than a policy decision.
//
// Precedence, in the order the graph applies it (research §10, "human decision > certain rule >
// probable suggestion"):
//
//	reject   a pair a human has rejected is never merged again, and a rule that goes on
//	         matching it is recorded as a conflict rather than as a pending suggestion;
//	pin      an entity a human has confirmed or manually merged is frozen: a later rule that
//	         would merge it with a *third* entity is recorded as a conflict, not applied,
//	         because merging would silently undo what the person decided (US6 scenario 3);
//	suggest  everything else a probable rule finds is pending until someone decides.
//
// The pin is the half FR-040 is usually read as missing. "A human decision must take precedence
// over any automated rule regardless of score" cannot mean only "do not re-merge what was
// rejected": a person who says *checkout-svc is checkout* has also said *checkout-svc is not
// anything else*, and an automated rule that later merges it into something else has overridden
// them just as surely. So the disagreement is surfaced, never applied.

// Suggestion statuses, as graph.suggestions stores them (data-model.md §graph.suggestions).
const (
	statusPending   = "pending"
	statusConfirmed = "confirmed"
	statusRejected  = "rejected"
	statusConflict  = "conflict"
)

// Resolution decision kinds, as graph.resolution_decisions stores them.
const (
	decisionAutoMerge   = "auto_merge"
	decisionSuggest     = "suggest"
	decisionConfirm     = "confirm"
	decisionReject      = "reject"
	decisionSplit       = "split"
	decisionManualMerge = "manual_merge"
)

// decisionID is the deterministic id of one decision row.
//
// `auto_merge` and `reject` keep the historical spelling — the hash of the pair key and the
// event — because those rows were already being written before the human decisions landed and
// their ids are part of what a replay must reproduce byte for byte (FR-023). Every other kind
// qualifies the event with the kind, so that one event producing two decisions about one pair
// (a suggestion that a later rule in the same pass turns into a merge) cannot collide.
func decisionID(kind, pairKey, eventID string) string {
	switch kind {
	case decisionAutoMerge, decisionReject:
		return graph.DecisionID(pairKey, eventID)
	default:
		return graph.DecisionID(pairKey, kind+":"+eventID)
	}
}

// suggestionRow is graph.suggestions as the projector reads it.
type suggestionRow struct {
	pairKey   string
	ruleID    string
	score     float64
	rationale string
	status    string
}

// recordSuggestion stores or refreshes the suggestion a probable match produces (FR-033).
//
// Nothing is merged here and nothing ever will be: the function has no path to p.merge. What it
// decides is the *status* the pair is filed under, which is where the precedence rules bite.
//
// A decision row is written only when the suggestion actually changed. The suggestions table is
// a current view and the decisions table is an append-only history; re-deriving the same
// suggestion from every subsequent claim would fill the history with a hundred identical
// "suggest" rows and bury the one decision a person has to read.
func (p *Projector) recordSuggestion(ctx context.Context, tx pgx.Tx, match resolution.Match, eventID string, observedAt time.Time) error {
	if match.Certain {
		return fmt.Errorf("projector: rule %s is certain; a certain match is merged, not suggested", match.RuleID)
	}
	a, err := p.follow(ctx, tx, match.EntityA)
	if err != nil {
		return err
	}
	b, err := p.follow(ctx, tx, match.EntityB)
	if err != nil {
		return err
	}
	if a == b {
		// Already one entity: there is nothing left to suggest.
		return nil
	}
	lo, hi := orderedPair(a, b)

	status, err := p.suggestionStatus(ctx, tx, lo, hi)
	if err != nil {
		return err
	}

	existing, found, err := p.suggestion(ctx, tx, graph.PairKey(lo, hi))
	if err != nil {
		return err
	}
	if found {
		// A pair a person has already decided is never reopened by a rule. The rule's opinion
		// still matters when it *contradicts* the decision, and that is what status carries.
		if existing.status != statusPending {
			status = existing.status
		}
		if status == statusPending &&
			existing.ruleID == match.RuleID && existing.score == match.Score &&
			existing.rationale == match.Rationale {
			return nil
		}
		if status != statusPending && existing.ruleID == match.RuleID && existing.score == match.Score {
			return nil
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.suggestions
			(pair_key, entity_a, entity_b, rule_id, score, rationale, created_at, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (pair_key) DO UPDATE SET
			rule_id = EXCLUDED.rule_id, score = EXCLUDED.score,
			rationale = EXCLUDED.rationale, status = EXCLUDED.status`,
		graph.PairKey(lo, hi), lo, hi, match.RuleID, match.Score, match.Rationale,
		observedAt.UTC(), status); err != nil {
		return fmt.Errorf("projector: record suggestion %s/%s: %w", lo, hi, err)
	}

	return p.recordDecision(ctx, tx, decisionRecord{
		kind:        decisionSuggest,
		survivingID: lo,
		mergedID:    hi,
		ruleID:      match.RuleID,
		score:       match.Score,
		rationale:   match.Rationale,
		claimIDs:    match.SupportingClaimIDs,
		observedAt:  observedAt,
		eventID:     eventID,
	})
}

// suggestionStatus applies the precedence rules to a pair no human has decided on directly.
func (p *Projector) suggestionStatus(ctx context.Context, tx pgx.Tx, a, b string) (string, error) {
	rejected, err := p.pairRejected(ctx, tx, a, b)
	if err != nil {
		return "", err
	}
	if rejected {
		return statusRejected, nil
	}
	pinned, err := p.eitherPinned(ctx, tx, a, b)
	if err != nil {
		return "", err
	}
	if pinned {
		return statusConflict, nil
	}
	return statusPending, nil
}

// suggestion reads the current view of one pair.
func (p *Projector) suggestion(ctx context.Context, tx pgx.Tx, pairKey string) (suggestionRow, bool, error) {
	var row suggestionRow
	err := tx.QueryRow(ctx, `
		SELECT pair_key, rule_id, score, rationale, status
		FROM graph.suggestions WHERE pair_key = $1`, pairKey).
		Scan(&row.pairKey, &row.ruleID, &row.score, &row.rationale, &row.status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return suggestionRow{}, false, nil
	case err != nil:
		return suggestionRow{}, false, fmt.Errorf("projector: read suggestion %s: %w", pairKey, err)
	}
	return row, true, nil
}

// setSuggestionStatus moves a pair to a status a human decision put it in, creating nothing: a
// decision about a pair no rule ever suggested has no suggestion row to move, and inventing one
// would claim the graph proposed something it did not.
func (p *Projector) setSuggestionStatus(ctx context.Context, tx pgx.Tx, pairKey, status, decisionID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE graph.suggestions SET status = $2, decision_id = $3 WHERE pair_key = $1`,
		pairKey, status, nullableString(decisionID)); err != nil {
		return fmt.Errorf("projector: set suggestion %s to %s: %w", pairKey, status, err)
	}
	return nil
}

// eitherPinned reports whether a human decision has pinned either entity.
func (p *Projector) eitherPinned(ctx context.Context, tx pgx.Tx, ids ...string) (bool, error) {
	for _, id := range ids {
		pinned, err := p.entityPinned(ctx, tx, id)
		if err != nil || pinned {
			return pinned, err
		}
	}
	return false, nil
}

// entityPinned reports whether a person has confirmed or manually merged this entity, and the
// decision has not been superseded (by a split, which is the only thing that lifts a pin).
//
// It is asked of a *canonical* id: the survivor of the human's merge is the entity the pair now
// is, so a decision naming either side pins it.
func (p *Projector) entityPinned(ctx context.Context, tx pgx.Tx, entityID string) (bool, error) {
	if entityID == "" {
		return false, nil
	}
	var pinned bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM graph.resolution_decisions
			WHERE kind IN ('confirm', 'manual_merge') AND superseded_by IS NULL
			  AND (surviving_id = $1 OR merged_id = $1))`, entityID).Scan(&pinned)
	if err != nil {
		return false, fmt.Errorf("projector: check pinned entity %s: %w", entityID, err)
	}
	return pinned, nil
}

// decisionRecord is one row of graph.resolution_decisions, as the projector writes it.
type decisionRecord struct {
	kind        string
	survivingID string
	mergedID    string
	ruleID      string
	score       float64
	rationale   string
	claimIDs    []string
	principal   string
	observedAt  time.Time
	eventID     string
}

// recordDecision appends one decision to the append-only history (FR-038).
//
// Every merge, automated or human, goes through here, so the four things constitution VI
// requires — what was merged, the rule or decision, the score, the rationale and the claims it
// stood on — are recorded in one place and cannot be recorded in part.
func (p *Projector) recordDecision(ctx context.Context, tx pgx.Tx, d decisionRecord) error {
	pairKey := graph.PairKey(d.survivingID, d.mergedID)
	var score any
	if d.score != 0 {
		score = d.score
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.resolution_decisions
			(decision_id, kind, surviving_id, merged_id, rule_id, score, rationale,
			 supporting_claim_ids, principal, decided_at, event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (decision_id) DO NOTHING`,
		decisionID(d.kind, pairKey, d.eventID), d.kind, d.survivingID, nullableString(d.mergedID),
		nullableString(d.ruleID), score, d.rationale, claimIDs(d.claimIDs),
		nullableString(d.principal), d.observedAt.UTC(), d.eventID); err != nil {
		return fmt.Errorf("projector: record %s decision %s: %w", d.kind, pairKey, err)
	}
	// resolution_decisions_total{kind,by} (plan.md §Observability). Every decision the graph
	// takes passes through here, so this is the only place the counter has to be touched, and
	// `by` says which authority took it: the principal for a human decision, the rule id for an
	// automated one, matching what the row itself records (FR-038, FR-041).
	p.metrics.ResolutionDecision(ctx, d.kind, d.decidedBy())
	return nil
}

// decidedBy names the authority behind a decision, in the form the metric publishes.
func (d decisionRecord) decidedBy() string {
	switch {
	case d.principal != "":
		return d.principal
	case d.ruleID != "":
		return "rule:" + d.ruleID
	default:
		return "unattributed"
	}
}

// claimIDs normalizes a nil slice to the empty array the column defaults to, so a replayed row
// and a live one are the same bytes.
func claimIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// orderedPair returns the pair in the order graph.suggestions stores it (entity_a < entity_b).
func orderedPair(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}
