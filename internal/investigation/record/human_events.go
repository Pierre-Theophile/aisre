// SPDX-License-Identifier: Apache-2.0

package record

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/event"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// HumanActionSchemaVersion is the event schema version the human channel is emitted under. It
// tracks the decision record's, because the three bodies were published in the same release.
const HumanActionSchemaVersion = DecisionRecordSchemaVersion

// HumanActionSourceID is the source every human action against an investigation is logged under.
const HumanActionSourceID = event.HumanSourceID

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
	return HumanKey("human_fact:", investigationID, author,
		submittedAt.UTC().Format(time.RFC3339Nano), statement)
}

// LabelKey is the published idempotency key of a label: `sha256(investigation_id, author,
// labelled_at)`. A second label from the same person at a *different* instant is a second row and
// a second event, which is what "additive like any other human decision" means (FR-057e).
func LabelKey(investigationID, author string, labelledAt time.Time) string {
	return HumanKey("label:", investigationID, author, labelledAt.UTC().Format(time.RFC3339Nano))
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

func HumanKey(prefix string, parts ...string) string {
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
		Entities:        RefsOf(fact.GetEntityIds()),
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

// RefsOf parses the `namespace=value` entity references a fact names, dropping anything that is
// not in that spelling rather than guessing a namespace for it.
func RefsOf(ids []string) []*graphv1.Ref {
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
