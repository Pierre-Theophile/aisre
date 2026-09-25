// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
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

// DecisionRecordSourceID is the source the engine registers to emit decision records. A source
// must be registered before it may append (FR-024 in 001), and an investigation's conclusion is a
// fact from the engine, not from a feeder.
const DecisionRecordSourceID = "investigation:engine"

// DecisionRecordSchemaVersion is the event schema version the record is emitted under.
const DecisionRecordSchemaVersion = "1.0.0"

// DecisionRecordPropCap mirrors `internal/log`'s per-property limit. It is repeated here rather
// than imported because the log's constant is unexported and because this side needs it for a
// different job: the log uses it to refuse, the ladder below uses it to *fit*.
const DecisionRecordPropCap = 4 << 10

// The published field caps of the hypothesis summary. A decision record is a pointer to the
// ledger, not a copy of it, so a statement and a rationale are carried at the length that makes
// the record readable on its own and no further.
const (
	decisionStatementCap = 140
	decisionRationaleCap = 200
)

// The published detail levels of the hypothesis summary, in the order the ladder descends.
const (
	// DetailFull carries statement and rationale for every hypothesis.
	DetailFull = "full"
	// DetailNoRationale drops the rationales; they are in the ledger and in the recording.
	DetailNoRationale = "no_rationale"
	// DetailIdentifiers carries only identifiers, statuses, confidences, buckets and ranks —
	// the fields FR-035 names, and never fewer.
	DetailIdentifiers = "identifiers"
)

// DecisionInput is a concluded investigation, as the decision record needs it.
//
// The ledger supplies every hypothesis, confidence and bucket; everything else is what only the
// engine knows. Nothing here is telemetry and nothing here is a digest's content.
type DecisionInput struct {
	// Ledger is the belief state at conclusion. Required.
	Ledger *ledger.Ledger
	// Subjects are the entities the incident was about; TargetEntities the ones the
	// investigation touched. The projector draws an INVESTIGATED edge to each.
	Subjects       []*graphv1.Ref
	TargetEntities []*graphv1.Ref
	// StartedAt and EndedAt bound the run; they become the node's valid interval.
	StartedAt time.Time
	EndedAt   time.Time
	// Outcome is `ranked`, `unknown`, `budget_exhausted` or `failed`; StopReason is the typed
	// stop (FR-045b); VerdictLine is the one line an on-call acts on.
	Outcome     string
	StopReason  string
	VerdictLine string
	// Requester is the authenticated principal the investigation ran for (FR-066).
	Requester string
	// Spend and ModelConfig are the machine-readable consumption and configuration (FR-044).
	// They are written under `sre.investigation.spend` and `sre.investigation.model_config`.
	Spend       map[string]any
	ModelConfig map[string]any
	// RecordingKey and RecordingDigest link to the bulk beside the graph (FR-035).
	RecordingKey    string
	RecordingDigest string
	// SourceID and EventID default to DecisionRecordSourceID and `inv:<id>:decision`.
	SourceID string
	EventID  string
}

// Body builds the `record_investigation` event body from the ledger (FR-035).
//
// Digests only: the hypotheses property carries ids, kinds, statuses, confidences, buckets with
// their ranges, ranks and — as far as the published cap allows — statements and rationales. No
// evidence content, no digest content, no telemetry of any kind reaches it.
func (in DecisionInput) Body() (*graphv1.RecordInvestigation, error) {
	switch {
	case in.Ledger == nil:
		return nil, errors.New("investigation store: decision record: no ledger")
	case in.Ledger.InvestigationID() == "":
		return nil, errors.New("investigation store: decision record: the ledger has no investigation id")
	case in.StartedAt.IsZero() || in.EndedAt.IsZero():
		return nil, errors.New("investigation store: decision record: started_at and ended_at are required")
	case len(in.Subjects) == 0:
		return nil, errors.New("investigation store: decision record: an investigation is about at least one subject")
	case in.Requester == "":
		return nil, fmt.Errorf("investigation store: decision record: no requester; an investigation with no "+
			"authenticated identity is refused (%s, FR-066)", eventlog.ReasonMissingPrincipal)
	}

	hypotheses, err := hypothesesProp(in.Ledger)
	if err != nil {
		return nil, err
	}
	spend, err := namespacedProp("sre.investigation.spend", in.Spend)
	if err != nil {
		return nil, err
	}
	modelConfig, err := namespacedProp("sre.investigation.model_config", in.ModelConfig)
	if err != nil {
		return nil, err
	}

	return &graphv1.RecordInvestigation{
		InvestigationId: in.Ledger.InvestigationID(),
		Subjects:        in.Subjects,
		TargetEntities:  in.TargetEntities,
		StartedAt:       timestamppb.New(in.StartedAt.UTC()),
		EndedAt:         timestamppb.New(in.EndedAt.UTC()),
		Outcome:         in.Outcome,
		StopReason:      in.StopReason,
		VerdictLine:     in.VerdictLine,
		Hypotheses:      hypotheses,
		Spend:           spend,
		ModelConfig:     modelConfig,
		RecordingKey:    in.RecordingKey,
		RecordingDigest: in.RecordingDigest,
		Requester:       in.Requester,
	}, nil
}

// Envelope wraps the body in the event envelope, with the investigation id as the idempotency key
// (data-model §"What this feature writes into the graph").
//
// The idempotency key is what makes "exactly one event per investigation" true even if the engine
// is restarted mid-conclusion, a retry duplicates the call, or a replay re-emits it: the second
// delivery is a DUPLICATE_NOOP and the graph is untouched.
func (in DecisionInput) Envelope() (*graphv1.EventEnvelope, error) {
	body, err := in.Body()
	if err != nil {
		return nil, err
	}
	sourceID := in.SourceID
	if sourceID == "" {
		sourceID = DecisionRecordSourceID
	}
	eventID := in.EventID
	if eventID == "" {
		eventID = "inv:" + body.GetInvestigationId() + ":decision"
	}
	return &graphv1.EventEnvelope{
		EventId:        eventID,
		IdempotencyKey: body.GetInvestigationId(),
		SourceId:       sourceID,
		SchemaVersion:  DecisionRecordSchemaVersion,
		Body:           &graphv1.EventEnvelope_RecordInvestigation{RecordInvestigation: body},
	}, nil
}

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
func EmitDecisionRecord(ctx context.Context, proj *projector.Projector, in DecisionInput) (*graphv1.IngestResult, error) {
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

// hypothesesProp renders the ranked hypotheses into the allow-listed property, descending the
// published detail ladder until the result fits inside DecisionRecordPropCap.
//
// The identifiers level is the floor: ids, kinds, statuses, confidences, buckets and ranks are
// what FR-035 requires, and if those alone do not fit the record is truncated by rank, with the
// truncation stated in the property rather than left for a reader to notice.
func hypothesesProp(l *ledger.Ledger) (*structpb.Struct, error) {
	ranked := l.Hypotheses()
	for _, detail := range []string{DetailFull, DetailNoRationale, DetailIdentifiers} {
		prop, fits, err := hypothesesAttempt(ranked, detail, len(ranked))
		if err != nil {
			return nil, err
		}
		if fits {
			return prop, nil
		}
	}

	// Still too large: keep the highest-ranked hypotheses that fit and say how many were left out.
	for n := len(ranked) - 1; n > 0; n-- {
		prop, fits, err := hypothesesAttempt(ranked, DetailIdentifiers, n)
		if err != nil {
			return nil, err
		}
		if fits {
			return prop, nil
		}
	}
	return nil, fmt.Errorf("investigation store: decision record: even one hypothesis does not fit in %d bytes",
		DecisionRecordPropCap)
}

// hypothesesPropKey is the single allow-listed key the summary lives under.
const hypothesesPropKey = "sre.investigation.hypotheses"

// hypothesesAttempt builds one rung of the ladder and measures it exactly the way the validator
// will: the protobuf JSON encoding of the property *value*, not of the Go map it was built from.
// Measuring the map instead would be a guess, and a guess that is 4% low is an event rejected in
// production and accepted in the test that was supposed to prove it would not be.
func hypothesesAttempt(ranked []ledger.Hypothesis, detail string, n int) (*structpb.Struct, bool, error) {
	prop, err := structpb.NewStruct(map[string]any{
		hypothesesPropKey: hypothesesPayload(ranked, detail, n),
	})
	if err != nil {
		return nil, false, fmt.Errorf("investigation store: decision record: encode hypotheses: %w", err)
	}
	encoded, err := prop.GetFields()[hypothesesPropKey].MarshalJSON()
	if err != nil {
		return nil, false, fmt.Errorf("investigation store: decision record: measure hypotheses: %w", err)
	}
	return prop, len(encoded) <= DecisionRecordPropCap, nil
}

// hypothesesPayload builds the summary at one detail level, for the top n hypotheses.
func hypothesesPayload(ranked []ledger.Hypothesis, detail string, n int) map[string]any {
	entries := make([]any, 0, n)
	for i, h := range ranked {
		if i >= n {
			break
		}
		entry := map[string]any{
			"id":         h.ID,
			"kind":       string(h.Kind),
			"status":     string(h.Status),
			"confidence": h.Confidence,
			"bucket":     h.Bucket.Name,
			"bucket_low": h.Bucket.Low,
			// Deliberately not "range_high"/"value"-shaped: each bucket carries its range
			// (FR-023), as two scalars rather than as a two-element numeric list, which the
			// telemetry denylist would read — correctly — as a series.
			"bucket_high": h.Bucket.High,
			"rank":        h.Rank,
		}
		if h.Widened {
			entry["widened"] = true
			entry["widened_reason"] = truncateRunes(h.WidenedReason, decisionRationaleCap)
		}
		if h.Status == ledger.StatusUntested && h.UntestedReason != "" {
			entry["untested_reason"] = truncateRunes(h.UntestedReason, decisionRationaleCap)
		}
		if detail != DetailIdentifiers {
			entry["statement"] = truncateRunes(h.Statement, decisionStatementCap)
		}
		if detail == DetailFull {
			entry["rationale"] = truncateRunes(h.Rationale, decisionRationaleCap)
		}
		entries = append(entries, entry)
	}

	payload := map[string]any{
		"detail_level": detail,
		"count":        len(entries),
		"total":        len(ranked),
		"ranked":       entries,
	}
	if len(entries) < len(ranked) {
		payload["truncated"] = true
	}
	return payload
}

// namespacedProp wraps a caller's map under one allow-listed key, or returns nil for an empty one
// so the record carries no empty properties.
func namespacedProp(key string, value map[string]any) (*structpb.Struct, error) {
	if len(value) == 0 {
		return nil, nil
	}
	out, err := structpb.NewStruct(map[string]any{key: value})
	if err != nil {
		return nil, fmt.Errorf("investigation store: decision record: encode %s: %w", key, err)
	}
	return out, nil
}

// truncateRunes bounds a text field on a rune boundary, marking where it was cut.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
