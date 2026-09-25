// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Projecting the one-click label (ADR-0005 D3, 002 FR-058).
//
// "Was this right?" is the cheapest evaluation signal there is and the only one that comes from
// the person the answer was for, so it is a first-class event rather than a row in a feedback
// table somewhere. It lands on the investigation node as three props: the judgement, who made
// it, and when.
//
// Recording it does not edit the conclusion. It writes a new *version* of the same valid
// interval — the observed interval of the old row is closed and a new row opened, exactly as
// constitution II requires of every correction — so "what did the graph say about this run
// before anyone labelled it" is still answerable as-known-at. The conclusion's own props are
// carried over unchanged; the label is additional, never a replacement.
//
// A label for an investigation whose record has not arrived yet is still a fact. It creates the
// entity and a version over the instant it was made, and `record_investigation` carries the
// label props forward when it lands, so the two orders converge on the same graph (FR-021) —
// which is what the fixture shuffle checks and the reason the carry exists at all.

func (p *Projector) applyLabelInvestigation(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.LabelInvestigation, observedAt time.Time) error {
	entityID, facets, err := p.investigationEntity(ctx, tx, env, body.GetInvestigationId(), observedAt)
	if err != nil {
		return err
	}
	current, err := p.currentNodeRows(ctx, tx, entityID)
	if err != nil {
		return err
	}

	labelledAt := body.GetLabelledAt().AsTime().UTC()
	start, end := labelledAt, labelledAt.Add(changeInstant)
	displayName := ""
	props := propSet{}
	if row := latestRecord(current); row != nil {
		// The run's own interval and its conclusion, kept as they were recorded.
		start, end = row.valid.Start, row.valid.End
		if row.valid.EndUnbounded {
			end = start.Add(changeInstant)
		}
		displayName = row.displayName
		for key, records := range row.props {
			props[key] = records
		}
	}
	setBoolProp(props, env, InvestigationLabelProp, body.GetWasThisRight())
	setProp(props, env, InvestigationLabelledByProp, body.GetAuthor())
	setProp(props, env, InvestigationLabelledAtProp, labelledAt.Format(time.RFC3339Nano))

	content := nodeContent{
		displayName: displayName,
		props:       props,
		conflicts:   props.conflicts(),
		facets:      facets,
	}
	return p.writeRecordVersion(ctx, tx, entityID, start, end, false, content, env.GetEventId(), observedAt)
}

// latestRecord is the current version an investigation's props live on. There is at most one —
// a record node has a single bounded version — but the loop is written for the general case so
// that a label landing on a graph mid-correction takes the latest rather than the first.
func latestRecord(rows []*nodeRow) *nodeRow {
	var out *nodeRow
	for _, row := range rows {
		if out == nil || row.valid.Start.After(out.valid.Start) {
			out = row
		}
	}
	return out
}
