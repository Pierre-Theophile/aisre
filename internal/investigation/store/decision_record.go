// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/Pierre-Theophile/aisre/internal/investigation/record"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/projector"
)

// The decision record (T059, FR-033, FR-034, FR-035, constitution III/IV, SC-010).
//
// One investigation emits exactly one event, once, at conclusion, in one transaction, keyed by the
// investigation id. Judgments, worker calls and ledger updates are never events: they are working
// material, they are rebuildable from the recording, and putting them in the log would turn the
// log into a debugging trace (constitution III, data-model §"What this feature writes into the
// graph").
//
// What the record carries is the *conclusion and its provenance*: identifiers, statuses,
// confidences with their buckets, ranks, rationales, spend, model configuration, and the
// recording's key and digest. What it may never carry is telemetry — not a sample, not a series,
// not a log line — and the enforcement is in two places that check different things:
//
//   - `internal/log`'s allow-list: every property sits under `sre.investigation.*` or the event is
//     refused with `prop_namespace`. That is what stops a future author attaching "just the series
//     that proves it" under a plausible new key.
//   - `internal/log`'s denylist, applied inside the namespace: a list of numbers is a series
//     whatever it is called, and a property over 4 KiB is a payload whatever it is called. A
//     decision record carrying a series is rejected with `telemetry_payload`, which is what
//     SC-010 measures.
//
// The 4 KiB cap is the reason the hypothesis summary is built by a ladder rather than by
// serialising the ledger: fifty hypotheses with statements and rationales do not fit in a graph
// property and should not — the full ledger lives in the recording, addressed by `recording_key`
// and `recording_digest`, and the record links to it. The ladder drops the *optional* fields
// first, in a published order, and says in the record itself how much detail survived.

// EmitDecisionRecord appends and projects the decision record, and links the investigation row to
// it, in one transaction (FR-035, constitution III).
//
// It is emitted once. If the investigation row already names a decision event, nothing is emitted
// and the existing event id comes back as a DUPLICATE_NOOP — belt and braces with the idempotency
// key, and the half that holds when an operator re-runs a conclusion by hand.
//
// A rejected record is a result, not an error, exactly as it is for a feeder: the reason code and
// the offending field come back, the rejection is recorded in `log.rejected_events`, and the graph
// is untouched. That is the path SC-010 measures when a decision record carries a series.
func EmitDecisionRecord(ctx context.Context, proj *projector.Projector, in record.DecisionInput) (*graphv1.IngestResult, error) {
	if proj == nil {
		return nil, errors.New("investigation store: decision record: no projector")
	}
	env, err := in.Envelope()
	if err != nil {
		return nil, err
	}
	investigationID := env.GetBody().(*graphv1.EventEnvelope_RecordInvestigation).RecordInvestigation.GetInvestigationId()

	var result *graphv1.IngestResult
	err = proj.Store().WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var existing string
		err := tx.QueryRow(ctx,
			`SELECT coalesce(decision_event_id, '') FROM investigation.investigations
			 WHERE investigation_id = $1`, investigationID).Scan(&existing)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("investigation store: decision record: investigation %s is not in the store",
				investigationID)
		case err != nil:
			return fmt.Errorf("investigation store: decision record: read %s: %w", investigationID, err)
		case existing != "":
			result = &graphv1.IngestResult{
				EventId: existing,
				Status:  graphv1.IngestResult_DUPLICATE_NOOP,
			}
			return nil
		}

		applied, err := proj.ApplyInTx(ctx, tx, env, projector.ApplyOptions{
			ObservedAt: in.EndedAt.UTC(),
			Principal:  in.Requester,
		})
		if err != nil {
			return err
		}
		result = applied
		if applied.GetStatus() != graphv1.IngestResult_APPLIED {
			return nil
		}

		// The link back: the investigation row names the event that recorded it, and carries the
		// recording it points at. Same transaction, so there is no window in which the graph holds
		// a conclusion the investigation does not know it emitted.
		if _, err := tx.Exec(ctx, `
			UPDATE investigation.investigations
			SET decision_event_id = $2,
			    recording_key = coalesce(nullif($3, ''), recording_key),
			    recording_digest = coalesce(nullif($4, ''), recording_digest),
			    ended_at = coalesce(ended_at, $5)
			WHERE investigation_id = $1`,
			investigationID, applied.GetEventId(), in.RecordingKey, in.RecordingDigest,
			in.EndedAt.UTC(),
		); err != nil {
			return fmt.Errorf("investigation store: decision record: link %s: %w", investigationID, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
