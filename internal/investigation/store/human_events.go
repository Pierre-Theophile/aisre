// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
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

// HumanActionSchemaVersion is the event schema version the human channel is emitted under. It
// tracks the decision record's, because the three bodies were published in the same release.
const HumanActionSchemaVersion = DecisionRecordSchemaVersion

// HumanActionSourceID is the source every human action against an investigation is logged under.
const HumanActionSourceID = eventlog.HumanSourceID

// HumanFactKey is the published idempotency key of a human fact:
// `sha256(investigation_id, author, submitted_at, statement)`.
//
// The parts are joined with a NUL byte, which none of them may contain, and the instant is
// rendered RFC 3339 with nanosecond precision in UTC — the same spelling AlertTransitionKey uses,
// so two callers that agree on the instant agree on the key whatever zone they read it in. The
// digest is prefixed `human_fact:` for the same reason an alert key is prefixed `alert:`: a key
// read in a log line should say what kind of thing it identifies.
//
// The statement is in the key deliberately (docs/schema/investigation.md): re-submitting the same
// sentence is a no-op, and a *different* sentence from the same author at the same instant is a
// second fact rather than a silently dropped one.
func HumanFactKey(investigationID, author string, submittedAt time.Time, statement string) string {
	return humanKey("human_fact:", investigationID, author,
		submittedAt.UTC().Format(time.RFC3339Nano), statement)
}

// LabelKey is the published idempotency key of a label: `sha256(investigation_id, author,
// labelled_at)`. A second label from the same person at a *different* instant is a second row and
// a second event, which is what "additive like any other human decision" means (FR-057e).
func LabelKey(investigationID, author string, labelledAt time.Time) string {
	return humanKey("label:", investigationID, author, labelledAt.UTC().Format(time.RFC3339Nano))
}

// ReopenKey is the published idempotency key of a reopen: the **child** investigation id, under
// the `reopen:` prefix. A concluded investigation is immutable, so a reopen is identified by the
// new record it produced rather than by the old one it links to (FR-007, FR-057b).
//
// The prefix is not decoration. `log.events.idempotency_key` is UNIQUE across the whole log, and
// `record_investigation`'s published key is the bare investigation id — so a reopen keyed on the
// child's bare id and the child's own decision record are the *same key*, and whichever arrived
// second would come back DUPLICATE_NOOP with nothing written. The reopen arrives first (it is
// written when the child is created; the record arrives when the child concludes), so the
// unprefixed spelling silently costs every reopened investigation its decision record. The
// prefix keeps the key derived from the child's id — re-delivering a reopen is still a no-op —
// and keeps the two events distinct. Published in docs/schema/investigation.md.
func ReopenKey(childInvestigationID string) string { return "reopen:" + childInvestigationID }

func humanKey(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(sum[:])
}

// The published reopen causes (graph.proto, `ReopenInvestigation.cause`).
const (
	// ReopenCauseHumanFact is a fact a person pushed at a concluded investigation (FR-057b).
	ReopenCauseHumanFact = "human_fact"
	// ReopenCauseAnswer is a late answer to a question the investigation asked.
	ReopenCauseAnswer = "answer"
)

// HumanFactEnvelope is the `submit_human_fact` event a stored fact becomes.
//
// `entity_ids` on the row are the refs the caller named, in the `namespace=value` spelling the
// CLI and the manifests use, so they travel as refs. An id that is not in that spelling — a
// canonical entity id, say — is carried in no ref at all rather than in an invented namespace
// that would mint a second entity for the same thing.
func HumanFactEnvelope(investigationID string, fact *investigationv1.HumanFact) (*graphv1.EventEnvelope, error) {
	switch {
	case fact == nil:
		return nil, errors.New("investigation store: human fact event: no fact")
	case investigationID == "":
		return nil, errors.New("investigation store: human fact event: no investigation id")
	case fact.GetAuthor() == "":
		return nil, fmt.Errorf("investigation store: human fact event: %w", ErrAnonymousPrincipal)
	case fact.GetSubmittedAt() == nil:
		return nil, errors.New("investigation store: human fact event: no submitted-at instant")
	}
	submittedAt := fact.GetSubmittedAt().AsTime().UTC()
	body := &graphv1.SubmitHumanFact{
		InvestigationId: investigationID,
		Kind:            fact.GetKind(),
		Statement:       fact.GetStatement(),
		Entities:        refsOf(fact.GetEntityIds()),
		Author:          fact.GetAuthor(),
		SubmittedAt:     timestamppb.New(submittedAt),
	}
	if c := fact.GetConcerns(); c != nil {
		body.ConcernsFrom = c.GetStart()
		body.ConcernsTo = c.GetEnd()
	}
	eventID := "inv:" + investigationID + ":fact:" + fact.GetFactId()
	return &graphv1.EventEnvelope{
		EventId: eventID,
		IdempotencyKey: HumanFactKey(investigationID, fact.GetAuthor(), submittedAt,
			fact.GetStatement()),
		SourceId:      HumanActionSourceID,
		SchemaVersion: HumanActionSchemaVersion,
		Body:          &graphv1.EventEnvelope_SubmitHumanFact{SubmitHumanFact: body},
	}, nil
}

// ReopenEnvelope is the `reopen_investigation` event linking a child to the parent it supersedes.
func ReopenEnvelope(parentID, childID, cause, causeRef string) (*graphv1.EventEnvelope, error) {
	switch {
	case parentID == "":
		return nil, errors.New("investigation store: reopen event: no parent investigation id")
	case childID == "":
		return nil, errors.New("investigation store: reopen event: no child investigation id")
	case childID == parentID:
		return nil, errors.New("investigation store: reopen event: the child is the parent; a reopen " +
			"is a NEW linked record, never an edit of the concluded one (FR-007, FR-057b)")
	}
	return &graphv1.EventEnvelope{
		EventId:        "inv:" + childID + ":reopen",
		IdempotencyKey: ReopenKey(childID),
		SourceId:       HumanActionSourceID,
		SchemaVersion:  HumanActionSchemaVersion,
		Body: &graphv1.EventEnvelope_ReopenInvestigation{
			ReopenInvestigation: &graphv1.ReopenInvestigation{
				ParentInvestigationId: parentID,
				ChildInvestigationId:  childID,
				Cause:                 cause,
				CauseRef:              causeRef,
			},
		},
	}, nil
}

// LabelEnvelope is the `label_investigation` event a stored label becomes.
func LabelEnvelope(investigationID string, label *investigationv1.Label) (*graphv1.EventEnvelope, error) {
	switch {
	case label == nil:
		return nil, errors.New("investigation store: label event: no label")
	case investigationID == "":
		return nil, errors.New("investigation store: label event: no investigation id")
	case label.GetAuthor() == "":
		return nil, fmt.Errorf("investigation store: label event: %w", ErrAnonymousPrincipal)
	case label.GetLabelledAt() == nil:
		return nil, errors.New("investigation store: label event: no labelled-at instant")
	}
	labelledAt := label.GetLabelledAt().AsTime().UTC()
	return &graphv1.EventEnvelope{
		EventId:        "inv:" + investigationID + ":label:" + label.GetLabelId(),
		IdempotencyKey: LabelKey(investigationID, label.GetAuthor(), labelledAt),
		SourceId:       HumanActionSourceID,
		SchemaVersion:  HumanActionSchemaVersion,
		Body: &graphv1.EventEnvelope_LabelInvestigation{
			LabelInvestigation: &graphv1.LabelInvestigation{
				InvestigationId: investigationID,
				WasThisRight:    label.GetWasThisRight(),
				Author:          label.GetAuthor(),
				LabelledAt:      timestamppb.New(labelledAt),
			},
		},
	}, nil
}

// refsOf parses the `namespace=value` entity references a fact names, dropping anything that is
// not in that spelling rather than guessing a namespace for it.
func refsOf(ids []string) []*graphv1.Ref {
	out := make([]*graphv1.Ref, 0, len(ids))
	for _, id := range ids {
		ref, err := graph.ParseRef(id)
		if err != nil {
			continue
		}
		out = append(out, &graphv1.Ref{Namespace: ref.Namespace, Value: ref.Value})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

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
	if err := d.proj.Log().RegisterSource(ctx, eventlog.HumanSource(), tx); err != nil {
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
