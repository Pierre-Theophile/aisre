// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The human channel (T083, T084, FR-054, FR-057a, FR-057b, FR-057e, SC-024).
//
// Three kinds of human input, one discipline: **typed, attributable, additive, non-blocking, and
// never truth**.
//
//   - A **fact** is something a person knows that the telemetry does not: "I restarted that pod
//     by hand at 14:20". It enters the evidence log as an evidence item, weighted `strong` and
//     never `decisive` (research §8, and the schema's CHECK). Where telemetry contradicts it,
//     both are kept and the contradiction is stated in the affected hypotheses — the engine does
//     not adjudicate between a person and a measurement.
//   - A **review** is a person's verdict on a completed run: the validated root cause, any
//     amendments, a rationale. Additive, never an edit, and it takes precedence over the
//     engine's own scores wherever the two are compared (FR-054).
//   - A **label** is the one-click "was this right?" (FR-057e).
//
// None of the three is a state the engine waits in. A fact pushed at a running investigation is
// evidence it may use if it gets there in time and may equally miss; a fact pushed at a concluded
// one reopens it, which means a *new linked row*, never an edit of the concluded one (FR-007,
// FR-057b). That asymmetry is the whole design: the engine never blocks, and the record never
// changes under a reader.

// WeightClassStrong is the only weight class a human fact may carry. A person who is certain is
// still a person, and the ledger has to be able to report a conflict with what was measured.
const WeightClassStrong = "strong"

// The published human-fact kinds (data-model §investigation.human_facts).
const (
	FactKindManualAction = "manual_action"
	FactKindVendorNotice = "vendor_notice"
	FactKindKnownState   = "known_state"
	FactKindCorrection   = "correction"
	FactKindOther        = "other"
)

// EvidenceKindHumanFact is the evidence kind a human fact becomes.
const EvidenceKindHumanFact = "human_fact"

// FactKinds lists the published kinds, for a CLI that has to say what it accepts.
func FactKinds() []string {
	return []string{FactKindManualAction, FactKindVendorNotice, FactKindKnownState,
		FactKindCorrection, FactKindOther}
}

// factKindAliases map the spellings connectors use onto the published set.
//
// The published vocabulary is `investigation.human_facts.kind`'s CHECK and is deliberately
// short. A connector that observes a fact in a chat thread or a ticket will spell it in its own
// words, and `operator_action` — which the `human-fact-reopen-01` fixture uses in its graph
// event — is the same fact as `manual_action`. The alias table is explicit and small: a spelling
// that is not here is an error rather than a silent `other`, because a fact filed under the
// wrong kind is worse than one that was refused.
var factKindAliases = map[string]string{
	"operator_action": FactKindManualAction,
	"manual":          FactKindManualAction,
	"vendor":          FactKindVendorNotice,
	"state":           FactKindKnownState,
}

// NormaliseFactKind maps a connector's spelling onto the published set, or reports that it does
// not know it.
func NormaliseFactKind(kind string) (string, error) {
	if validFactKind(kind) {
		return kind, nil
	}
	if mapped, ok := factKindAliases[kind]; ok {
		return mapped, nil
	}
	return "", fmt.Errorf("%w (got %q)", ErrUnknownFactKind, kind)
}

// ErrUnknownFactKind is a fact whose kind is outside the published set.
var ErrUnknownFactKind = fmt.Errorf("human fact kind must be one of %v", FactKinds())

// ErrDecisiveHumanFact is an attempt to weight a human fact as decisive. It is refused here as
// well as by the schema, because the rule is the point rather than the column.
var ErrDecisiveHumanFact = errors.New(
	"a human fact is strong evidence, never decisive: where telemetry contradicts it both are kept (FR-057a)")

// SubmitFact writes a human fact against an investigation, running or concluded, and returns it
// with its evidence id filled in.
//
// It never blocks and never waits: the write is one transaction and the engine is not consulted.
// Where the investigation has concluded, ReopenWithFact is the call that also links a child.
func (d *InvestigationDAO) SubmitFact(ctx context.Context, investigationID string, fact *investigationv1.HumanFact) (*investigationv1.HumanFact, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: submit fact: no database")
	}
	var out *investigationv1.HumanFact
	err := d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = SubmitFactInTx(ctx, tx, investigationID, fact)
		if err != nil {
			return err
		}
		return d.emitFact(ctx, tx, investigationID, out)
	})
	return out, err
}

// emitFact writes the `submit_human_fact` event for a stored fact, in the transaction that stored
// it (human_events.go). The apply principal is the fact's author: the person who knows the thing
// is the person the graph records as having said it.
func (d *InvestigationDAO) emitFact(
	ctx context.Context, tx pgx.Tx, investigationID string, fact *investigationv1.HumanFact,
) error {
	if d == nil || d.proj == nil {
		return nil
	}
	env, err := HumanFactEnvelope(investigationID, fact)
	if err != nil {
		return err
	}
	return d.emitHumanEvent(ctx, tx, env, fact.GetAuthor())
}

// SubmitFactInTx is SubmitFact inside a caller's transaction.
//
// It writes two rows: the evidence item the fact becomes (FR-057a: "MUST enter the evidence log
// as an evidence item") and the fact itself, which points at it. The evidence carries the fact's
// own instants, so a fact about 14:20 submitted at 17:00 does not smuggle 17:00's knowledge into
// an investigation pinned at 14:32 — the evidence's observed instant is the investigation's.
func SubmitFactInTx(ctx context.Context, tx pgx.Tx, investigationID string, fact *investigationv1.HumanFact) (*investigationv1.HumanFact, error) {
	if fact == nil {
		return nil, errors.New("investigation store: submit fact: no fact")
	}
	kind, err := NormaliseFactKind(fact.GetKind())
	if err != nil {
		return nil, fmt.Errorf("investigation store: submit fact: %w", err)
	}
	if w := fact.GetWeightClass(); w != "" && w != WeightClassStrong {
		return nil, fmt.Errorf("investigation store: submit fact: %w (got %q)", ErrDecisiveHumanFact, w)
	}
	if fact.GetAuthor() == "" {
		return nil, fmt.Errorf("investigation store: submit fact: %w", ErrAnonymousPrincipal)
	}
	if err := EnsurePrincipalInTx(ctx, tx, fact.GetAuthor()); err != nil {
		return nil, err
	}

	out := cloneFact(fact)
	out.Kind = kind
	out.WeightClass = WeightClassStrong
	if out.GetSubmittedAt() == nil {
		out.SubmittedAt = timestamppb.New(time.Now().UTC())
	}
	if out.GetFactId() == "" {
		out.FactId = "fact-" + investigationID + "-" +
			out.GetSubmittedAt().AsTime().UTC().Format("20060102T150405.000000000Z")
	}
	if out.GetEvidenceId() == "" {
		out.EvidenceId = "ev-" + out.GetFactId()
	}

	// The evidence item's instants are the investigation's, not the fact's: Invariant 7 forbids
	// an item observed later than the run it belongs to, and a fact submitted afterwards is
	// exactly that case. What the fact is *about* travels in `concerns`.
	var validAt, observedAt time.Time
	if err := tx.QueryRow(ctx,
		`SELECT valid_at, observed_at FROM investigation.investigations WHERE investigation_id = $1`,
		investigationID).Scan(&validAt, &observedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("investigation store: submit fact to %s: %w", investigationID, ErrNotFound)
		}
		return nil, fmt.Errorf("investigation store: submit fact to %s: %w", investigationID, err)
	}

	item := FactEvidence(out, validAt, observedAt)
	if err := InsertEvidenceInTx(ctx, tx, investigationID, item); err != nil {
		return nil, err
	}

	entities := out.GetEntityIds()
	if entities == nil {
		entities = []string{}
	}
	contradicted := out.GetContradictedByEvidenceIds()
	if contradicted == nil {
		contradicted = []string{}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation.human_facts (
			fact_id, investigation_id, kind, statement, entity_ids, concerns, author,
			submitted_at, weight_class, evidence_id, event_id, contradicted_by_evidence_ids)
		VALUES ($1, $2, $3, $4, $5, tstzrange($6, $7), $8, $9, $10, $11, $12, $13)
		ON CONFLICT (fact_id) DO NOTHING`,
		out.GetFactId(), investigationID, out.GetKind(), out.GetStatement(), entities,
		intervalStart(out.GetConcerns()), intervalEnd(out.GetConcerns()),
		out.GetAuthor(), out.GetSubmittedAt().AsTime(), WeightClassStrong,
		out.GetEvidenceId(), nullable(out.GetEventId()), contradicted,
	); err != nil {
		return nil, fmt.Errorf("investigation store: submit fact %s: %w", out.GetFactId(), err)
	}
	return out, nil
}

// FactEvidence is the evidence item a human fact becomes (FR-057a).
//
// It has no algebra term — nobody queried anything — and says so in its deep-link absence reason
// rather than inventing a link. Its coverage block names the entities the fact concerns, which is
// the honest answer to "what did this look at?": one person's knowledge of those entities.
func FactEvidence(fact *investigationv1.HumanFact, validAt, observedAt time.Time) ledger.EvidenceItem {
	searched := fact.GetEntityIds()
	if searched == nil {
		searched = []string{}
	}
	return ledger.EvidenceItem{
		ID:         fact.GetEvidenceId(),
		Kind:       EvidenceKindHumanFact,
		Worker:     "human",
		Capability: fact.GetKind(),
		ValidAt:    validAt.UTC(),
		ObservedAt: observedAt.UTC(),
		CalledAt:   fact.GetSubmittedAt().AsTime().UTC(),
		Mode:       "live",
		Outcome:    "digest",
		Coverage: &investigationv1.Coverage{
			SearchedEntities:         searched,
			DataSource:               "human",
			ExecutedAt:               fact.GetSubmittedAt(),
			VolumeConsidered:         1,
			IngestionLagUndetermined: true,
			QuotaUndetermined:        true,
		},
		DeepLinkAbsentReason: "a human fact is not a query; `aisre investigate get <id> --evidence` " +
			"shows the statement, its author and the instant it was submitted",
		FreeText: fact.GetStatement(),
	}
}

// FactJudgment is the judgment a human fact makes against one hypothesis: strong, never decisive
// (FR-057a, research §8). The direction is the caller's — a fact can support or refute — and the
// ledger computes the number.
func FactJudgment(id, hypothesisID string, fact *investigationv1.HumanFact, direction ledger.Direction) ledger.Judgment {
	return ledger.Judgment{
		ID:           id,
		HypothesisID: hypothesisID,
		EvidenceID:   fact.GetEvidenceId(),
		Direction:    direction,
		Strength:     ledger.Strong,
		Source:       ledger.SourceHumanFact,
		RecordedAt:   fact.GetSubmittedAt().AsTime().UTC(),
	}
}

// RecordContradiction states, on the fact, that a measurement disagrees with it (FR-057a).
//
// Both are kept and neither is dropped: the fact's row grows a reference to the contradicting
// evidence, and the affected hypotheses carry both judgments. Nothing here resolves the
// disagreement, because nothing here can.
func (d *InvestigationDAO) RecordContradiction(ctx context.Context, factID string, evidenceIDs ...string) error {
	if d == nil || d.store == nil {
		return errors.New("investigation store: record contradiction: no database")
	}
	if len(evidenceIDs) == 0 {
		return nil
	}
	tag, err := d.store.Pool().Exec(ctx, `
		UPDATE investigation.human_facts
		SET contradicted_by_evidence_ids = (
			SELECT array_agg(DISTINCT e) FROM unnest(contradicted_by_evidence_ids || $2::text[]) AS e)
		WHERE fact_id = $1`, factID, evidenceIDs)
	if err != nil {
		return fmt.Errorf("investigation store: record contradiction on %s: %w", factID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("investigation store: record contradiction on %s: %w", factID, ErrNotFound)
	}
	return nil
}

// ReopenWithFact is FR-057b: a fact bearing on a *concluded* investigation moves it to `reopened`
// and produces a new linked record.
//
// The parent is not edited — its verdict, ledger and evidence stay exactly as produced — and the
// child is a full investigation row of its own, linked by `reopens_investigation_id`, which
// itself runs and concludes under the same rules. The fact lands on the child, because that is
// the run that gets to use it.
//
// It returns the child's id. A fact pushed at a *running* investigation never reaches here:
// SubmitFact is the whole of that path, and it does not block the run.
func (d *InvestigationDAO) ReopenWithFact(
	ctx context.Context,
	parentID string,
	child NewInvestigation,
	fact *investigationv1.HumanFact,
) (*investigationv1.HumanFact, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: reopen: no database")
	}
	if child.ReopensInvestigationID == "" {
		child.ReopensInvestigationID = parentID
	}
	if child.Requester == "" {
		child.Requester = fact.GetAuthor()
	}
	var out *investigationv1.HumanFact
	err := d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := MarkReopenedInTx(ctx, tx, parentID); err != nil {
			return err
		}
		if err := EnsurePrincipalInTx(ctx, tx, child.Requester); err != nil {
			return err
		}
		if err := insertInvestigationInTx(ctx, tx, child); err != nil {
			return err
		}
		var err error
		out, err = SubmitFactInTx(ctx, tx, child.InvestigationID, fact)
		if err != nil {
			return err
		}
		if err := d.emitFact(ctx, tx, child.InvestigationID, out); err != nil {
			return err
		}
		// The reopen link, keyed by the child's id: the parent is not edited, so the event that
		// records the supersession is identified by the record it produced (FR-007, FR-057b).
		env, err := ReopenEnvelope(parentID, child.InvestigationID,
			ReopenCauseHumanFact, out.GetFactId())
		if err != nil {
			return err
		}
		return d.emitHumanEvent(ctx, tx, env, out.GetAuthor())
	})
	return out, err
}

// FactsOf reads an investigation's human facts, oldest first.
func (d *InvestigationDAO) FactsOf(ctx context.Context, investigationID string) ([]*investigationv1.HumanFact, error) {
	rows, err := d.store.Pool().Query(ctx, `
		SELECT fact_id, kind, statement, entity_ids, lower(concerns), upper(concerns), author,
		       submitted_at, weight_class, coalesce(evidence_id, ''), coalesce(event_id, ''),
		       contradicted_by_evidence_ids
		FROM investigation.human_facts WHERE investigation_id = $1
		ORDER BY submitted_at, fact_id`, investigationID)
	if err != nil {
		return nil, fmt.Errorf("investigation store: facts of %s: %w", investigationID, err)
	}
	defer rows.Close()

	var out []*investigationv1.HumanFact
	for rows.Next() {
		var (
			f           investigationv1.HumanFact
			start, end  *time.Time
			submittedAt time.Time
		)
		if err := rows.Scan(&f.FactId, &f.Kind, &f.Statement, &f.EntityIds, &start, &end,
			&f.Author, &submittedAt, &f.WeightClass, &f.EvidenceId, &f.EventId,
			&f.ContradictedByEvidenceIds); err != nil {
			return nil, fmt.Errorf("investigation store: scan fact: %w", err)
		}
		f.SubmittedAt = timestamppb.New(submittedAt.UTC())
		if start != nil || end != nil {
			f.Concerns = &graphv1.Interval{}
			if start != nil {
				f.Concerns.Start = timestamppb.New(start.UTC())
			}
			if end != nil {
				f.Concerns.End = timestamppb.New(end.UTC())
			}
		}
		out = append(out, &f)
	}
	return out, rows.Err()
}

// --- review, label and the corpus hand-off (T084, FR-054, FR-055, FR-057e) ---------------

// RecordReview writes a human review: additive, attributable, and never an edit of the run it
// reviews (FR-054, FR-007).
//
// It takes precedence over the engine's scores wherever the two are compared — which is a
// property of the *reader*, not of this write: the row is stored beside the ledger and the
// renderer and the evaluation harness prefer it. Overwriting the ledger here would destroy the
// thing the comparison needs.
func (d *InvestigationDAO) RecordReview(ctx context.Context, investigationID string, review *investigationv1.HumanReview) (*investigationv1.HumanReview, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: review: no database")
	}
	if review == nil {
		return nil, errors.New("investigation store: review: no review")
	}
	if review.GetAuthor() == "" {
		return nil, fmt.Errorf("investigation store: review: %w", ErrAnonymousPrincipal)
	}
	if review.GetValidatedRootCause() == "" {
		return nil, errors.New("investigation store: review: a validated root cause is required; " +
			"`unobserved` and `not_change_induced:<category>` are valid answers (FR-054)")
	}

	out, ok := proto3Clone(review).(*investigationv1.HumanReview)
	if !ok {
		return nil, errors.New("investigation store: review: clone")
	}
	if out.GetDecidedAt() == nil {
		out.DecidedAt = timestamppb.New(time.Now().UTC())
	}
	if out.GetReviewId() == "" {
		out.ReviewId = "review-" + investigationID + "-" +
			out.GetDecidedAt().AsTime().UTC().Format("20060102T150405.000000000Z")
	}
	amendments, err := amendmentsJSON(out.GetAmendments())
	if err != nil {
		return nil, err
	}

	err = d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := EnsurePrincipalInTx(ctx, tx, out.GetAuthor()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO investigation.human_reviews (
				review_id, investigation_id, validated_root_cause, hypothesis_amendments,
				rationale, author, decided_at, event_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (review_id) DO NOTHING`,
			out.GetReviewId(), investigationID, out.GetValidatedRootCause(), amendments,
			out.GetRationale(), out.GetAuthor(), out.GetDecidedAt().AsTime(),
			nullable(out.GetEventId()))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("investigation store: review %s: %w", out.GetReviewId(), err)
	}
	return out, nil
}

// ReviewsOf reads an investigation's reviews, oldest first.
func (d *InvestigationDAO) ReviewsOf(ctx context.Context, investigationID string) ([]*investigationv1.HumanReview, error) {
	rows, err := d.store.Pool().Query(ctx, `
		SELECT review_id, validated_root_cause, hypothesis_amendments, rationale, author,
		       decided_at, coalesce(event_id, '')
		FROM investigation.human_reviews WHERE investigation_id = $1
		ORDER BY decided_at, review_id`, investigationID)
	if err != nil {
		return nil, fmt.Errorf("investigation store: reviews of %s: %w", investigationID, err)
	}
	defer rows.Close()

	var out []*investigationv1.HumanReview
	for rows.Next() {
		var (
			r          investigationv1.HumanReview
			amendments []byte
			decidedAt  time.Time
		)
		if err := rows.Scan(&r.ReviewId, &r.ValidatedRootCause, &amendments, &r.Rationale,
			&r.Author, &decidedAt, &r.EventId); err != nil {
			return nil, fmt.Errorf("investigation store: scan review: %w", err)
		}
		r.DecidedAt = timestamppb.New(decidedAt.UTC())
		r.Amendments = parseAmendments(amendments)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// RecordLabel writes the one-click "was this right?" (FR-057e). Additive like every other human
// decision: a second label from the same person is a second row, and the corpus sees both.
func (d *InvestigationDAO) RecordLabel(ctx context.Context, investigationID string, label *investigationv1.Label) (*investigationv1.Label, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: label: no database")
	}
	if label == nil {
		return nil, errors.New("investigation store: label: no label")
	}
	if label.GetAuthor() == "" {
		return nil, fmt.Errorf("investigation store: label: %w", ErrAnonymousPrincipal)
	}
	out, ok := proto3Clone(label).(*investigationv1.Label)
	if !ok {
		return nil, errors.New("investigation store: label: clone")
	}
	if out.GetLabelledAt() == nil {
		out.LabelledAt = timestamppb.New(time.Now().UTC())
	}
	if out.GetLabelId() == "" {
		out.LabelId = "label-" + investigationID + "-" +
			out.GetLabelledAt().AsTime().UTC().Format("20060102T150405.000000000Z")
	}
	err := d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := EnsurePrincipalInTx(ctx, tx, out.GetAuthor()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO investigation.labels
				(label_id, investigation_id, was_this_right, author, labelled_at, event_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (label_id) DO NOTHING`,
			out.GetLabelId(), investigationID, out.GetWasThisRight(), out.GetAuthor(),
			out.GetLabelledAt().AsTime(), nullable(out.GetEventId())); err != nil {
			return err
		}
		if d.proj == nil {
			return nil
		}
		env, err := LabelEnvelope(investigationID, out)
		if err != nil {
			return err
		}
		return d.emitHumanEvent(ctx, tx, env, out.GetAuthor())
	})
	if err != nil {
		return nil, fmt.Errorf("investigation store: label %s: %w", out.GetLabelId(), err)
	}
	return out, nil
}

// LabelsOf reads an investigation's labels, oldest first.
func (d *InvestigationDAO) LabelsOf(ctx context.Context, investigationID string) ([]*investigationv1.Label, error) {
	rows, err := d.store.Pool().Query(ctx, `
		SELECT label_id, was_this_right, author, labelled_at, coalesce(event_id, '')
		FROM investigation.labels WHERE investigation_id = $1
		ORDER BY labelled_at, label_id`, investigationID)
	if err != nil {
		return nil, fmt.Errorf("investigation store: labels of %s: %w", investigationID, err)
	}
	defer rows.Close()

	var out []*investigationv1.Label
	for rows.Next() {
		var (
			l          investigationv1.Label
			labelledAt time.Time
		)
		if err := rows.Scan(&l.LabelId, &l.WasThisRight, &l.Author, &labelledAt, &l.EventId); err != nil {
			return nil, fmt.Errorf("investigation store: scan label: %w", err)
		}
		l.LabelledAt = timestamppb.New(labelledAt.UTC())
		out = append(out, &l)
	}
	return out, rows.Err()
}

// AgreementWithHumans is the metric FR-056 asks for on every evaluation run: of the reviewed
// investigations, the fraction whose top-ranked hypothesis matched the validated root cause,
// and, separately, the fraction of labels that said "right".
//
// It is computed from the rows rather than materialised, because a review or a label added
// tomorrow must move it without a backfill.
type AgreementWithHumans struct {
	// Reviewed is how many investigations carry at least one review.
	Reviewed int
	// Agreed is how many of those had the validated root cause ranked first.
	Agreed int
	// Labelled and LabelledRight are the same counts over the one-click label.
	Labelled      int
	LabelledRight int
}

// Rate is Agreed/Reviewed, or 0 when nothing has been reviewed. A rate over an empty corpus is
// not 100%.
func (a AgreementWithHumans) Rate() float64 {
	if a.Reviewed == 0 {
		return 0
	}
	return float64(a.Agreed) / float64(a.Reviewed)
}

// Agreement computes AgreementWithHumans over every reviewed investigation (FR-056).
func (d *InvestigationDAO) Agreement(ctx context.Context) (AgreementWithHumans, error) {
	if d == nil || d.store == nil {
		return AgreementWithHumans{}, errors.New("investigation store: agreement: no database")
	}
	var a AgreementWithHumans
	// The engine's answer is the rank-1 hypothesis; the human's is the validated root cause.
	// `unobserved` and `not_change_induced:*` match the open hypothesis, which is the one whose
	// kind is `no_observed_change`.
	err := d.store.Pool().QueryRow(ctx, `
		WITH reviewed AS (
			SELECT DISTINCT ON (r.investigation_id) r.investigation_id, r.validated_root_cause
			FROM investigation.human_reviews r ORDER BY r.investigation_id, r.decided_at DESC
		), top AS (
			SELECT DISTINCT ON (h.investigation_id) h.investigation_id, h.kind,
			       coalesce(h.candidate_change_entity_id, '') AS culprit
			FROM investigation.hypotheses h ORDER BY h.investigation_id, h.rank
		)
		SELECT count(*),
		       count(*) FILTER (WHERE
		           (reviewed.validated_root_cause = 'unobserved'
		            OR reviewed.validated_root_cause LIKE 'not_change_induced:%')
		           AND top.kind = 'no_observed_change'
		        OR reviewed.validated_root_cause = top.culprit)
		FROM reviewed LEFT JOIN top USING (investigation_id)`).Scan(&a.Reviewed, &a.Agreed)
	if err != nil {
		return AgreementWithHumans{}, fmt.Errorf("investigation store: agreement: %w", err)
	}
	if err := d.store.Pool().QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE was_this_right)
		FROM investigation.labels`).Scan(&a.Labelled, &a.LabelledRight); err != nil {
		return AgreementWithHumans{}, fmt.Errorf("investigation store: agreement labels: %w", err)
	}
	return a, nil
}

func validFactKind(kind string) bool {
	for _, k := range FactKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func cloneFact(fact *investigationv1.HumanFact) *investigationv1.HumanFact {
	out, ok := proto3Clone(fact).(*investigationv1.HumanFact)
	if !ok {
		return &investigationv1.HumanFact{}
	}
	return out
}

func intervalStart(i *graphv1.Interval) any {
	if i == nil || i.GetStart() == nil {
		return nil
	}
	return i.GetStart().AsTime()
}

func intervalEnd(i *graphv1.Interval) any {
	if i == nil || i.GetEnd() == nil {
		return nil
	}
	return i.GetEnd().AsTime()
}

// amendmentsJSON serialises a review's hypothesis amendments. Only the fields a human may amend
// are stored — status, rank, statement and rationale — because a review is not a second ledger
// and storing a confidence here would create one (FR-023).
func amendmentsJSON(amendments []*investigationv1.Hypothesis) ([]byte, error) {
	rows := make([]map[string]any, 0, len(amendments))
	for _, h := range amendments {
		rows = append(rows, map[string]any{
			"hypothesis_id": h.GetHypothesisId(),
			"status":        h.GetStatus().String(),
			"rank":          h.GetRank(),
			"statement":     h.GetStatement(),
			"rationale":     h.GetRationale(),
		})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return nil, fmt.Errorf("investigation store: review amendments: %w", err)
	}
	return raw, nil
}

func parseAmendments(raw []byte) []*investigationv1.Hypothesis {
	var rows []map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	out := make([]*investigationv1.Hypothesis, 0, len(rows))
	for _, row := range rows {
		h := &investigationv1.Hypothesis{
			HypothesisId: stringField(row, "hypothesis_id"),
			Statement:    stringField(row, "statement"),
			Rationale:    stringField(row, "rationale"),
		}
		if v, ok := investigationv1.HypothesisStatus_value[stringField(row, "status")]; ok {
			h.Status = investigationv1.HypothesisStatus(v)
		}
		if v, ok := row["rank"].(float64); ok {
			h.Rank = uint32(v) //nolint:gosec // a rank is a small positive integer
		}
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].GetRank() < out[j].GetRank() })
	return out
}

func stringField(row map[string]any, key string) string {
	s, _ := row[key].(string)
	return s
}
