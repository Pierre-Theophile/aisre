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
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Rebuilding the human channel from the event log (docs/schema/investigation.md §"The
// `investigation` schema is derived").
//
// The claim schema `investigation` is admitted under is that every row in it is rebuildable from
// the event log plus a regenerable world. For the runner's rows that is true because re-running
// the question against the same graph and the same world produces them again. For the human
// channel there is nothing to re-run — nobody is going to push the fact a second time — so the
// rows have to come back from the three events the channel emits, and this is the function that
// brings them back.
//
// It is not a projector: it writes into schema `investigation`, not into the graph, and it reads
// the log rather than being driven by it. It is the inverse of `human_events.go`, and the two are
// tested against each other in the runner's rebuild test.
//
// Three passes, because the rows have an order the log does not guarantee:
//
//  1. reopens, which may have to *create* the child investigation row before anything can be
//     written against it. The child inherits the parent's question exactly as `ReopenWithFact`
//     builds it — same incident, same instants, same window, same profile — because that is what
//     a reopen is, and a rebuild that invented a different question would be a different run.
//  2. facts, each of which writes its evidence item and its row. The identifiers are derived from
//     the investigation id and the submitted-at instant, so a rebuilt fact carries the id it had.
//  3. labels, likewise.
//
// Nothing here emits: the events it is reading are already in the log, and re-emitting them would
// be a DUPLICATE_NOOP at best and a second event at worst.

// HumanRebuildReport is what a rebuild restored, for a caller that wants to say so.
type HumanRebuildReport struct {
	// Facts, Reopens and Labels are the rows written. A row that was already there is not
	// counted twice: every write is ON CONFLICT DO NOTHING.
	Facts, Reopens, Labels int
	// MissingInvestigations names the investigations an event referred to that the schema does
	// not hold and the log could not reconstruct — a reopen whose parent is gone, say. They are
	// reported rather than swallowed: a rebuild that silently dropped a fact would make the
	// derivability claim unfalsifiable.
	MissingInvestigations []string
}

// RebuildHumanChannel rewrites the human-channel rows of schema `investigation` from the event
// log.
func RebuildHumanChannel(ctx context.Context, store *postgres.Store) (HumanRebuildReport, error) {
	var report HumanRebuildReport
	if store == nil {
		return report, errors.New("investigation store: rebuild the human channel: no database")
	}
	events, err := humanEvents(ctx, store)
	if err != nil {
		return report, err
	}

	err = store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, body := range events.reopens {
			restored, err := rebuildReopenInTx(ctx, tx, body)
			if err != nil {
				return err
			}
			if !restored {
				report.MissingInvestigations = append(report.MissingInvestigations,
					body.GetParentInvestigationId())
				continue
			}
			report.Reopens++
		}
		for _, body := range events.facts {
			exists, err := investigationExists(ctx, tx, body.GetInvestigationId())
			if err != nil {
				return err
			}
			if !exists {
				report.MissingInvestigations = append(report.MissingInvestigations,
					body.GetInvestigationId())
				continue
			}
			if _, err := SubmitFactInTx(ctx, tx, body.GetInvestigationId(), factFromEvent(body)); err != nil {
				return err
			}
			report.Facts++
		}
		for _, body := range events.labels {
			exists, err := investigationExists(ctx, tx, body.GetInvestigationId())
			if err != nil {
				return err
			}
			if !exists {
				report.MissingInvestigations = append(report.MissingInvestigations,
					body.GetInvestigationId())
				continue
			}
			if err := rebuildLabelInTx(ctx, tx, body); err != nil {
				return err
			}
			report.Labels++
		}
		return nil
	})
	if err != nil {
		return HumanRebuildReport{}, err
	}
	return report, nil
}

// humanChannelEvents is the log's three human bodies, in append order within each kind.
type humanChannelEvents struct {
	facts   []*graphv1.SubmitHumanFact
	reopens []*graphv1.ReopenInvestigation
	labels  []*graphv1.LabelInvestigation
}

func humanEvents(ctx context.Context, store *postgres.Store) (humanChannelEvents, error) {
	var out humanChannelEvents
	err := eventlog.New(store).Iterate(ctx, 0, func(record *eventlog.Record) error {
		switch body := record.Envelope.GetBody().(type) {
		case *graphv1.EventEnvelope_SubmitHumanFact:
			out.facts = append(out.facts, body.SubmitHumanFact)
		case *graphv1.EventEnvelope_ReopenInvestigation:
			out.reopens = append(out.reopens, body.ReopenInvestigation)
		case *graphv1.EventEnvelope_LabelInvestigation:
			out.labels = append(out.labels, body.LabelInvestigation)
		}
		return nil
	})
	if err != nil {
		return humanChannelEvents{}, fmt.Errorf("investigation store: read the human channel from the log: %w", err)
	}
	return out, nil
}

// factFromEvent turns a `submit_human_fact` body back into the fact the store writes. The fact id
// and the evidence id are left empty on purpose: SubmitFactInTx derives both from the
// investigation id and the submitted-at instant, so the rebuilt row carries the identifiers the
// original had.
func factFromEvent(body *graphv1.SubmitHumanFact) *investigationv1.HumanFact {
	fact := &investigationv1.HumanFact{
		Kind:        body.GetKind(),
		Statement:   body.GetStatement(),
		Author:      body.GetAuthor(),
		SubmittedAt: body.GetSubmittedAt(),
		WeightClass: WeightClassStrong,
	}
	for _, ref := range body.GetEntities() {
		fact.EntityIds = append(fact.EntityIds, ref.GetNamespace()+"="+ref.GetValue())
	}
	if from, to := body.GetConcernsFrom(), body.GetConcernsTo(); from != nil || to != nil {
		fact.Concerns = &graphv1.Interval{Start: from, End: to}
	}
	return fact
}

// rebuildReopenInTx restores the parent's `reopened` status and the child row that links to it,
// creating the child from the parent's own question when the schema no longer holds it.
func rebuildReopenInTx(ctx context.Context, tx pgx.Tx, body *graphv1.ReopenInvestigation) (bool, error) {
	parentID, childID := body.GetParentInvestigationId(), body.GetChildInvestigationId()
	parent, found, err := investigationSeed(ctx, tx, parentID)
	if err != nil || !found {
		return false, err
	}
	child, childFound, err := investigationSeed(ctx, tx, childID)
	if err != nil {
		return false, err
	}
	if !childFound {
		child = parent
		child.InvestigationID = childID
		child.ReopensInvestigationID = parentID
		if err := EnsurePrincipalInTx(ctx, tx, child.Requester); err != nil {
			return false, err
		}
		if err := insertInvestigationInTx(ctx, tx, child); err != nil {
			return false, err
		}
	} else if _, err := tx.Exec(ctx, `
		UPDATE investigation.investigations SET reopens_investigation_id = $2
		WHERE investigation_id = $1 AND reopens_investigation_id IS NULL`, childID, parentID); err != nil {
		return false, fmt.Errorf("investigation store: rebuild the reopen link of %s: %w", childID, err)
	}
	if err := MarkReopenedInTx(ctx, tx, parentID); err != nil {
		return false, err
	}
	return true, nil
}

// rebuildLabelInTx writes one label row, with the identifier RecordLabel would have derived.
func rebuildLabelInTx(ctx context.Context, tx pgx.Tx, body *graphv1.LabelInvestigation) error {
	labelledAt := body.GetLabelledAt().AsTime().UTC()
	label := &investigationv1.Label{
		LabelId: "label-" + body.GetInvestigationId() + "-" +
			labelledAt.Format("20060102T150405.000000000Z"),
		WasThisRight: body.GetWasThisRight(),
		Author:       body.GetAuthor(),
		LabelledAt:   timestamppb.New(labelledAt),
	}
	if err := EnsurePrincipalInTx(ctx, tx, label.GetAuthor()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO investigation.labels
			(label_id, investigation_id, was_this_right, author, labelled_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (label_id) DO NOTHING`,
		label.GetLabelId(), body.GetInvestigationId(), label.GetWasThisRight(),
		label.GetAuthor(), labelledAt); err != nil {
		return fmt.Errorf("investigation store: rebuild label %s: %w", label.GetLabelId(), err)
	}
	return nil
}

func investigationExists(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT true FROM investigation.investigations WHERE investigation_id = $1`, id).Scan(&exists)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("investigation store: look up investigation %s: %w", id, err)
	}
	return true, nil
}

// investigationSeed reads the fields a reopened child inherits from its parent.
func investigationSeed(ctx context.Context, tx pgx.Tx, id string) (NewInvestigation, bool, error) {
	var (
		inv                    NewInvestigation
		windowStart, windowEnd *time.Time
		modelConfig            []byte
	)
	err := tx.QueryRow(ctx, `
		SELECT investigation_id, incident_id, valid_at, observed_at, review_mode,
		       lower("window"), upper("window"), profile, requester, model_config,
		       algebra_version, ledger_rule_version, schema_version, started_at
		FROM investigation.investigations WHERE investigation_id = $1`, id).Scan(
		&inv.InvestigationID, &inv.IncidentID, &inv.ValidAt, &inv.ObservedAt, &inv.ReviewMode,
		&windowStart, &windowEnd, &inv.Profile, &inv.Requester, &modelConfig,
		&inv.AlgebraVersion, &inv.LedgerRuleVersion, &inv.SchemaVersion, &inv.StartedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return NewInvestigation{}, false, nil
	case err != nil:
		return NewInvestigation{}, false, fmt.Errorf("investigation store: read investigation %s: %w", id, err)
	}
	if windowStart != nil {
		inv.WindowStart = windowStart.UTC()
	}
	if windowEnd != nil {
		inv.WindowEnd = windowEnd.UTC()
	}
	if len(modelConfig) > 0 {
		cfg := &structpb.Struct{}
		if err := cfg.UnmarshalJSON(modelConfig); err == nil {
			inv.ModelConfig = cfg
		}
	}
	return inv, true, nil
}
