// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/internal/projector"
)

// A re-evaluation is dated from the LAST event that changed its answer (004 T139).
//
// ---------------------------------------------------------------------------------------------
// What this test is about
//
// A queued re-evaluation is dated `queued_at + 1µs`, and `close_observed` refuses a `closed_at` that is
// not after the lower bound of the version it closes. So a row whose `queued_at` is older than the
// versions the merge must close cannot be applied — and because the drain takes the queue head, deletes
// it and re-evaluates in one transaction, a failure rolls the delete back and the SAME row is taken
// again next time. The queue wedges permanently and the merge is lost.
//
// `retrigger.go` recorded this as a limit reachable only if a process died between the provoking commit
// and the drain. It is reachable in ordinary batched replay, which is what this test demonstrates: the
// drain runs BETWEEN batches, so any event later in the same batch can open the versions that make the
// queued row's instant too old.
//
// # Why the answer was not an audit-trail decision
//
// T139 framed it as one — record the overtaking as a fact, or re-derive the work — and both of those
// make the outcome depend on where the batch boundaries fell, because both decide something at DRAIN
// time and the drain's timing is the caller's choice. FR-021 forbids exactly that, and
// TestReplayIsIndependentOfBatchSize is the test that would have caught it.
//
// The defect was upstream of the drain. Three call sites queue this work, and the last one to run is the
// one that actually completed the rule's precondition — here the merge of the two targets. The insert
// was `ON CONFLICT (correlation_id) DO NOTHING`, so that final re-queue was DROPPED and the row kept the
// instant of the deploy key that arrived long before the answer could have been yes. Keeping the NEWEST
// instant instead makes the row say what it is: this rule's answer last changed at this event. The
// instant is then a function of the events and not of the drain's timing, so it is the same at any batch
// size, and it is late enough to be legal for the same reason it is correct.
//
// A no-op is not enough to test this. The ordering below puts the deploy keys before the claims that
// merge the targets, so the stale instant is genuinely stale and the row genuinely cannot be applied
// with it.
func TestAQueuedReEvaluationIsDatedFromTheLastEventThatChangedIt(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())
	events := c8Events()
	ctx := context.Background()

	// One batch, so no drain runs inside it. The two deploy keys (4, 5) queue C8's re-evaluation while
	// the targets are still two entities; the claims that merge them (2, 3) come afterwards, so the
	// merge's own re-queue is what has to carry the instant.
	base := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	order := []int{0, 1, 4, 5, 2, 3}
	if err := store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for step, i := range order {
			if _, err := p.ApplyInTx(ctx, tx, events[i], projector.ApplyOptions{
				ObservedAt: base.Add(time.Duration(step) * time.Minute),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}

	// The drain a batching caller owes, and it must not fail.
	if err := p.DrainPendingResolution(ctx); err != nil {
		t.Fatalf("the drain was refused, so the queue head is wedged and every later drain fails on "+
			"the same row: %v", err)
	}

	var queued int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM graph.pending_resolution`).Scan(&queued); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	if queued != 0 {
		t.Errorf("%d row(s) left queued after a drain that reported success", queued)
	}

	// And the merge it was queued for actually happened. A drain that emptied the queue without merging
	// would pass every assertion above and lose the thing the queue exists to not lose.
	var merged int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM graph.resolution_decisions
		 WHERE rule_id = 'C8' AND superseded_by IS NULL`).Scan(&merged); err != nil {
		t.Fatalf("count C8 decisions: %v", err)
	}
	if merged == 0 {
		t.Error("the queue drained and C8 never fired: the re-evaluation was dropped rather than made")
	}

	// The decision is dated after the versions it closed, which is the property that made it legal.
	var decidedAfter bool
	if err := store.Pool().QueryRow(ctx, `
		SELECT bool_and(d.decided_at > lower(v.observed))
		FROM graph.resolution_decisions d
		JOIN graph.entity_versions v ON v.entity_id IN (d.surviving_id, d.merged_id)
		WHERE d.rule_id = 'C8' AND d.superseded_by IS NULL AND NOT upper_inf(v.observed)`).
		Scan(&decidedAfter); err != nil {
		t.Fatalf("read the decision's instant: %v", err)
	}
	if !decidedAfter {
		t.Error("a C8 decision is dated at or before a version it closed; the close was legal only by " +
			"accident")
	}
}
