// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/Pierre-Theophile/aisre/internal/event"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/projector"
)

// Emitting the human channel (FR-054, FR-055, FR-057a, FR-057b, FR-057e; ADR-0005 D3;
// docs/schema/investigation.md §"The five events this feature emits").
//
// Three of the five events this feature publishes are human actions: `submit_human_fact`,
// `reopen_investigation` and `label_investigation`. They had a schema, a validator and a
// projector before they had an emitter, which meant schema `investigation` held three kinds of
// row the event log did not know about — and the derivability claim the second schema is admitted
// under ("every row is rebuildable from the event log plus a regenerable world") was true of the
// runner's rows and merely *asserted* of the human channel's. This file closes that.
//
// Three decisions, each of them the same one the decision record made:
//
//   - **The event is written in the transaction that writes the row.** Not after it, not by a
//     worker reading a queue. A row that exists without its event is exactly the substrate leak
//     the derivability claim forbids, and a transaction is the only thing that makes "both or
//     neither" true under a crash.
//   - **The source is `human`, not `investigation:engine`.** The engine did not decide any of
//     these; a person did, and FR-054 requires every human decision to carry "the authenticated
//     individual who made it". That individual travels in the body (`author`) and in the apply
//     principal; the source id says only that the event came from a person rather than a feeder,
//     which is the same source `internal/server` registers for a merge decision.
//   - **Identifiers and the published body, nothing more.** A fact carries its statement because
//     `SubmitHumanFact.statement` is a published field and a fact with no statement is not one;
//     it carries no evidence content, no telemetry, and no ledger. The `sre.investigation.*`
//     allow-list and the 4 KiB per-property cap apply to these events exactly as they do to the
//     decision record — none of the three writes a property at all, so both hold trivially.
//
// Re-delivery is a no-op on the published idempotency key, which is derived from the content in
// every case, so an operator who re-records a fact by hand, a retried RPC and a replay all
// converge on one event.

// emitHumanEvent appends and projects one human-channel event inside the caller's transaction.
//
// A DAO with no projector emits nothing and says so by doing nothing: the in-memory and
// unit-test paths that build a DAO over a bare store keep working, and every wiring that reaches
// production (`internal/cli/serve.go`, the runner) passes one. A rejection is returned as an
// error here rather than swallowed, because unlike a delivery a human action has no "recorded and
// carried on" path: the row and its event are one transaction, so a refused event must take the
// row with it.
func (d *InvestigationDAO) emitHumanEvent(
	ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, principal string,
) error {
	if d == nil || d.proj == nil {
		return nil
	}
	if err := d.proj.Log().RegisterSource(ctx, event.HumanSource(), tx); err != nil {
		return err
	}
	// No observed instant is passed, so the log stamps the append (FR-019). The action's own
	// instant — submitted-at, labelled-at — is *valid* time and travels in the body: a person
	// stating at 17:00 what was true at 14:20 has not made the graph know it at 14:20, and
	// back-dating the observed bound would put a version behind one the graph already holds.
	result, err := d.proj.ApplyInTx(ctx, tx, env, projector.ApplyOptions{Principal: principal})
	if err != nil {
		return err
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return fmt.Errorf("investigation store: event %s was refused (%s: %s)",
			env.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return nil
}
