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

// Re-evaluating a rule whose answer depends on the graph rather than on its evidence
// (004 T137, T148; specs/004-deploy-feeders/contracts/deploy-claims.md §2.1).
//
// ---------------------------------------------------------------------------------------------
// Why this file exists
//
// Rules run in exactly one place: when their evidence is stored — applyIdentityClaim for a claim,
// applyCorrelateEntity for a correlation key. For every rule but C8 that is enough, because every
// other rule's answer is a function of the claims it compares: store the claim, compare, done.
//
// C8 is not. It merges two change observations that state the same deploy identifier **for a target
// they share**, and "the same target" is a fact about the graph: the two sources spell a target
// differently and what makes them one is a merge resolution already made. So C8's answer can change
// after its evidence is stored, in three ways — one per party to the comparison:
//
//   - the TARGET arrives last: a change attaches to a target that did not exist when the key arrived
//     (attach.go, via attachWaiting — a change often arrives before the thing it changed);
//   - the CHANGE arrives last: a correlation key is stored on a change nobody has described yet, so
//     the key's subject is minted as a bare entity with no targets at all, and the observation that
//     supplies them comes afterwards (observe_change.go);
//   - the TARGETS MERGE, by C1, C4 or C5, so an intersection that was empty is not (refs.go).
//
// In all three cases nothing re-evaluates and the merge is never made.
//
// The second was missing from this list, and from the code, until `deploy-cross-source-merge-01`'s
// shuffle step found it: C8 fired in arrival order and not under permutation, which is precisely the
// order dependence FR-021 forbids. It is worth naming why it was missed — the first and third are both
// "something else moved", which is what a re-trigger obviously means, while the second is the rule's
// own subject arriving late, which does not feel like a graph change at all. It is one. That is the order dependence C4
// shipped with and `c4FromService` was added to fix — with the difference that C4 could fix it inside
// the rule, because its data was all claims. C8 cannot: no arrangement of arms makes a rule notice a
// graph change nobody told it about.
//
// # What it does not do
//
// It does not re-evaluate every rule. Only the deploy correlation keys, and only on the changes whose
// target set actually moved. A blanket re-evaluation would be a different and much larger promise —
// that the projection is a fixed point of the whole rule set — which nothing in the contracts asks for
// and which would make the cost of one event depend on the size of the graph.
//
// The principal is empty, which is correct rather than convenient: `decisionOptions.principal` is
// documented as "the authenticated individual behind a human decision, empty for a rule", and a
// re-trigger is a rule firing. A recorded rejection still blocks the merge, because applyMatch looks
// it up by PAIR.

// maxDrainRounds bounds the drain: a re-evaluation can merge, a merge can move another change's
// target set, and that queues more work.
//
// The chain does terminate on its own — each round either merges something, which strictly reduces the
// number of distinct entities, or queues nothing — so the bound is against a defect in that reasoning
// producing an unbounded loop rather than against the reasoning itself. Exceeding it leaves the
// remaining rows queued for the next drain rather than erroring: a late merge is a late merge, and a
// failed transaction would lose the event that provoked it.
const maxDrainRounds = 64

// drainObservedOffset is how far after the provoking event a drained re-evaluation is observed.
//
// The smallest step the store's timestamps distinguish, because the claim being made is only that the
// graph learned this after the event and not that any measurable time passed. A larger offset would be
// a claim about duration that nothing supports.
const drainObservedOffset = time.Microsecond

// queueDeployKeys records that the deploy correlation keys on these change entities need
// re-evaluating.
//
// It queues rather than re-evaluating in place, and that is forced rather than chosen: a merge closes
// the observed interval of the versions it absorbs, and close_observed refuses to close an interval at
// the instant it opened, so a second merge in one transaction is refused. See the migration
// 0011_pending_resolution.sql for the whole argument, and 0012 for why the queue holds correlation ids
// rather than claim ids.
//
// It is a no-op for entities carrying no deploy key, which is the overwhelmingly common case: a
// Kubernetes rollout or an alert transition has none, so the cost on an ordinary event is one indexed
// query returning nothing.
func (p *Projector) queueDeployKeys(ctx context.Context, tx pgx.Tx, entityIDs []string, reason, eventID string, observedAt time.Time) error {
	if len(entityIDs) == 0 {
		return nil
	}
	correlationIDs, err := p.deployKeysOf(ctx, tx, entityIDs)
	if err != nil {
		return err
	}
	for _, correlationID := range correlationIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO graph.pending_resolution (correlation_id, reason, event_id, queued_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (correlation_id) DO UPDATE
			SET reason = EXCLUDED.reason, event_id = EXCLUDED.event_id, queued_at = EXCLUDED.queued_at
			WHERE EXCLUDED.queued_at > graph.pending_resolution.queued_at`,
			correlationID, reason, eventID, observedAt); err != nil {
			return fmt.Errorf("projector: queue correlation %s for re-evaluation: %w", correlationID, err)
		}
	}
	return nil
}

// DrainPendingResolution re-evaluates every queued correlation key, one transaction each.
//
// One key per transaction for the same reason the queue exists: two merges in one transaction are
// refused. It is exported because a caller that owns its own transaction — a batch replay through
// ApplyInTx — cannot be drained from inside, so it drains between batches. ApplyWithOptions drains
// itself, immediately after the transaction that queued the work commits.
//
// A row is deleted from the queue in the same transaction that re-evaluates it, so a failure leaves
// the work queued rather than silently dropped, and a success cannot re-run it.
//
// # A row is dated from the LAST event that changed its answer (T139)
//
// A queued row carries an instant, and the re-evaluation is dated from it. Which instant is not a
// detail: `close_observed` refuses a `closed_at` that is not after the lower bound of the version it
// closes, so a row dated before the versions the merge must close CANNOT be applied — and since the
// drain takes the head, deletes it and re-evaluates in one transaction, the failure rolls the delete
// back and the same row is taken again. The queue wedges permanently and the merge is lost.
//
// This was recorded here as a limit reachable only if a process died between the provoking commit and
// the drain, with the suggested answer being a decision about the audit trail — record the overtaking,
// or re-derive the work. Both of those were wrong, and so was the premise.
//
// It is reachable in ordinary batched replay: the drain runs BETWEEN batches, so any later event in the
// same batch can open the versions that make an earlier row's instant too old. And neither suggested
// answer is available, because both decide something at DRAIN time, and the drain's timing is the
// caller's choice — the same events under a different batch size would give a different graph, which is
// what FR-021 forbids and what TestReplayIsIndependentOfBatchSize exists to catch.
//
// The defect was upstream of the drain. Three call sites queue this work, and the last one to run is the
// one that completed the rule's precondition — typically the merge that made two targets one. The insert
// was `ON CONFLICT DO NOTHING`, so that final re-queue was dropped and the row kept the instant of a
// deploy key stored long before the answer could have been yes. Keeping the NEWEST instant makes the row
// say what it means: this rule's answer last changed here. That instant is a function of the events
// rather than of when the drain ran, so it is identical at any batch size — and it is late enough to be
// legal for the same reason it is correct, which is the shape the offset below already had.
func (p *Projector) DrainPendingResolution(ctx context.Context) error {
	for round := 0; round < maxDrainRounds; round++ {
		var drained bool
		err := p.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var correlationID, eventID string
			var queuedAt time.Time
			err := tx.QueryRow(ctx, `
				DELETE FROM graph.pending_resolution
				WHERE correlation_id = (
					SELECT correlation_id FROM graph.pending_resolution
					ORDER BY queued_at, correlation_id LIMIT 1)
				RETURNING correlation_id, event_id, queued_at`).Scan(&correlationID, &eventID, &queuedAt)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return nil
			case err != nil:
				return fmt.Errorf("projector: take queued correlation: %w", err)
			}
			drained = true
			// The observed instant is the provoking event's, advanced by the smallest representable
			// step. Both halves of that matter.
			//
			// It is the EVENT'S instant and not now(): observed time is when the graph learned a thing,
			// a replay has to learn it at the same instant as the original run, and a wall clock would
			// make the projection depend on when the replay happened.
			//
			// And it is STRICTLY AFTER it, because the graph genuinely learned this later — in this
			// transaction rather than the one that queued the work. Dating it at the same instant would
			// also be refused: the provoking transaction opened versions at that instant, and
			// close_observed will not close an interval at the instant it opened. So the offset is not a
			// workaround for the guard; it is the guard being right, and the honest instant happening to
			// satisfy it.
			//
			// Valid time is untouched. What a change asserts about the world is what its source said;
			// only the moment the graph worked out that two of them are one moves.
			return p.runCorrelationRulesByID(ctx, tx, correlationID, eventID, queuedAt.Add(drainObservedOffset), "")
		})
		if err != nil {
			return err
		}
		if !drained {
			return nil
		}
	}
	return nil
}

// deployKeysOf returns the deploy correlation keys recorded against any of these entities, in
// correlation-id order so a replay re-evaluates in the same order and records the same decisions
// (FR-023).
func (p *Projector) deployKeysOf(ctx context.Context, tx pgx.Tx, entityIDs []string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT correlation_id FROM graph.correlation_keys
		WHERE entity_id = ANY($1) AND namespace = ANY($2)
		ORDER BY correlation_id`, entityIDs, resolution.DeployIdentifierNamespaces())
	if err != nil {
		return nil, fmt.Errorf("projector: read deploy keys: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var correlationID string
		if err := rows.Scan(&correlationID); err != nil {
			return nil, fmt.Errorf("projector: scan deploy key: %w", err)
		}
		out = append(out, correlationID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read deploy keys: %w", err)
	}
	return out, nil
}

// changesTargeting returns the change entities that were applied to any of these entities.
//
// It is the question a merge raises: when two entities become one, every change attached to either of
// them may now share a target with a change it did not share one with before.
func (p *Projector) changesTargeting(ctx context.Context, tx pgx.Tx, entityIDs []string) ([]string, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT coalesce(t.merged_into, e.dst_id)
		FROM graph.edge_versions e
		LEFT JOIN graph.entities t ON t.entity_id = e.dst_id
		WHERE e.type = $2 AND upper_inf(e.observed) AND e.src_id = ANY($1)
		ORDER BY 1`, entityIDs, string(graph.EdgeTypeChangedBy))
	if err != nil {
		return nil, fmt.Errorf("projector: read changes targeting %v: %w", entityIDs, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var changeID string
		if err := rows.Scan(&changeID); err != nil {
			return nil, fmt.Errorf("projector: scan change: %w", err)
		}
		out = append(out, changeID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read changes targeting %v: %w", entityIDs, err)
	}
	return out, nil
}
