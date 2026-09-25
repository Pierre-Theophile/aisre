// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Projecting a reopen (ADR-0005 D3, 002 FR-057a/e, 002 data-model).
//
// A concluded investigation is immutable. When a human fact or a late answer contradicts it,
// what happens is not an edit: a NEW investigation is opened and linked to the old one. That is
// the whole content of this handler, and the one thing it must never do is touch the parent —
// no new version, no closed observed interval, no prop appended. The parent's conclusion stays
// exactly as it was published, including to anyone who read it before the reopen; the child is
// where the revised answer lives.
//
// The link is an INVESTIGATED edge from the child to the parent. The child investigated the
// parent's conclusion, which is literally what a reopen is, and it means "what superseded this
// investigation" is one edge read in the same direction as every other question about an
// investigation's subjects.
//
// The child normally does not exist yet — a reopen happens when the new run *starts*, and the
// run's record arrives when it concludes. The entity is created here and its version arrives
// later; `record_investigation` carries the reopen's props forward onto it, so the two events
// commute (FR-021).

func (p *Projector) applyReopenInvestigation(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.ReopenInvestigation, observedAt time.Time) error {
	parentID, _, err := p.investigationEntity(ctx, tx, env, body.GetParentInvestigationId(), observedAt)
	if err != nil {
		return err
	}
	childID, _, err := p.investigationEntity(ctx, tx, env, body.GetChildInvestigationId(), observedAt)
	if err != nil {
		return err
	}

	props := propSet{}
	setProp(props, env, InvestigationReopenedProp, body.GetParentInvestigationId())
	setProp(props, env, InvestigationReopenCause, reopenCause(body))

	// Valid from the instant the reopen was observed, open-ended: the link is not a statement
	// about a window, it is a statement that from now on this investigation supersedes that
	// one. A reopen body carries no valid time of its own, and inventing one would be a guess.
	key := edgeKey{srcID: childID, dstID: parentID, typ: graph.EdgeTypeInvestigated}
	return p.linkRecord(ctx, tx, key, observedAt.UTC(), nil, props, env, observedAt)
}

// reopenCause renders the published cause vocabulary with its reference, e.g.
// "human_fact:fact-0007". One string rather than two props keeps the pair from drifting apart
// when a version is corrected.
func reopenCause(body *graphv1.ReopenInvestigation) string {
	cause := body.GetCause()
	if cause == "" {
		return ""
	}
	if ref := body.GetCauseRef(); ref != "" {
		return cause + ":" + ref
	}
	return cause
}
