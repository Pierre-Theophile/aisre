// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Projecting a human fact (ADR-0005 D3, 002 FR-057a/b, 002 data-model §"What this feature
// writes into the graph").
//
// "The canary is still on the old build" is a fact about production that no feeder observed and
// no query can answer. It arrives from a person, against a running or a concluded
// investigation, and the graph's job is to record it — with its author, its instant and the
// interval it concerns — without letting it rewrite anything.
//
// Three decisions, all of them about what this must *not* do:
//
//   - **It does not create a fact node.** The fact itself is an event, and the log is the
//     source of truth for events; minting a node per sentence would fill the graph with
//     entities nothing can be said about. What the graph gains is the *relationship* the fact
//     asserts: a CONCERNS edge from the investigation to each entity the fact names, over the
//     interval the fact concerns, with the fact's event id in `produced_by_event_ids`. "Which
//     human facts bear on payments in this investigation" is then one edge read plus a log
//     lookup, and the edge is bitemporal like everything else.
//   - **It does not rewrite the investigation's version.** A fact pushed at a concluded run is
//     the case US9 is about, and a concluded investigation is immutable (002 FR-007). Adding an
//     edge leaves the conclusion exactly as it was recorded.
//   - **It does not resolve the contradiction.** If the fact contradicts what the run
//     concluded, both stand. Stating the contradiction is the engine's job (002 FR-057b); the
//     graph's job is to hold both.
//
// The author is on the edge because a fact with no name on it is not evidence. Validation has
// already refused an anonymous one before it reached the log (missing_principal).

func (p *Projector) applySubmitHumanFact(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.SubmitHumanFact, observedAt time.Time) error {
	entityID, _, err := p.investigationEntity(ctx, tx, env, body.GetInvestigationId(), observedAt)
	if err != nil {
		return err
	}

	// The interval the fact concerns, defaulting to "from when it was submitted, open-ended".
	// A person who says when it started says so; nothing here guesses one (FR-011).
	start := body.GetSubmittedAt().AsTime().UTC()
	if ts := body.GetConcernsFrom(); ts != nil {
		start = ts.AsTime().UTC()
	}
	var end *time.Time
	if ts := body.GetConcernsTo(); ts != nil && ts.AsTime().After(start) {
		at := ts.AsTime().UTC()
		end = &at
	}

	props := propSet{}
	setProp(props, env, InvestigationFactKindProp, body.GetKind())
	setProp(props, env, InvestigationFactAuthorProp, body.GetAuthor())

	for _, ref := range refsOf(body.GetEntities()) {
		targetID, err := p.resolveRef(ctx, tx, ref, graph.NodeTypeUnspecified,
			env.GetEventId(), env.GetSourceId(), observedAt)
		if err != nil {
			return err
		}
		key := edgeKey{srcID: entityID, dstID: targetID, typ: graph.EdgeTypeConcerns}
		if err := p.linkRecord(ctx, tx, key, start, end, props, env, observedAt); err != nil {
			return err
		}
	}
	return nil
}
