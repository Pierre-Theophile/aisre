// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// The proposed dependency, which is not an edge (003 FR-029, FR-122; data-model.md §4.2).
//
// Resolution can already suggest that two REFS name one ENTITY — that is graph.suggestions and
// suggest.go. Nothing could suggest that an EDGE exists between two DIFFERENT entities, so a feeder
// holding suggestive but inconclusive evidence of a dependency had exactly two options and both
// were worse than a stated gap: assert the edge anyway, or drop the evidence.
//
// The concrete case: a Cloud Run service whose configuration, connection settings or labels name a
// Cloud SQL instance gets a real `depends-on` edge with the evidence that derived it recorded
// (FR-028). Where the dependency cannot be derived, this is the path instead.
//
// The invariant, enforced here and nowhere else: **nothing in this file writes an edge except the
// confirmation handler.** applyProposeDependency has no path to applyUpsertEdge, which is the same
// structural guarantee recordSuggestion makes about p.merge.
//
// Precedence, in the order it is applied — the edge analogue of suggest.go's reject/pin/suggest:
//
//	rejected   a triple a person has rejected is never reopened by a rule. A rule that goes on
//	           proposing it is recorded as a `conflict`, which is FR-122's "a human decision
//	           outranks any automated match regardless of score" applied to edges. Reopening it
//	           would let a rule outvote a person by simply running again.
//	confirmed  already an edge; there is nothing left to propose.
//	pending    raised once and NOT re-raised while pending (FR-029), so a feeder seeing the same
//	           weak evidence every minute does not produce a queue nobody can read. The proposal's
//	           event list still grows, so "when did we first suspect this" stays answerable.
//
// Two things are deliberately refused rather than accommodated, both in checkResolvable:
//
//   - a proposal naming an entity the graph has never observed. A proposal is a question about two
//     known things; minting a placeholder for either would put a phantom in the review queue.
//   - a decision on a triple with no proposal. Unlike a merge, a dependency decision has no meaning
//     on its own: confirming a proposal nobody made would claim the graph had suggested something
//     it never did. Asserting an edge with no proposal behind it is what upsert_edge is for.

// Proposal statuses, as graph.proposed_dependencies stores them.
const (
	proposalPending   = "pending"
	proposalConfirmed = "confirmed"
	proposalRejected  = "rejected"
	proposalConflict  = "conflict"
)

// proposalRow is graph.proposed_dependencies as the projector reads it.
type proposalRow struct {
	key       string
	ruleID    string
	score     float64
	rationale string
	status    string
	eventIDs  []string
}

// proposalKey is the row's primary key: the triple in the order asserted.
//
// Not an ordered pair, unlike graph.PairKey. A dependency is directed — "checkout depends on
// orders-db" and its reverse are different claims, only one of which is true — so ordering the
// endpoints would file both under one row that means neither.
func proposalKey(srcID, dstID string, typ graph.EdgeType) string {
	return srcID + "|" + dstID + "|" + typ.String()
}

// applyProposeDependency files a proposal, or declines to refile one (FR-029).
func (p *Projector) applyProposeDependency(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.ProposeDependency, observedAt time.Time,
) error {
	srcID, dstID, typ, err := p.resolveProposalTriple(ctx, tx, body.GetSrc(), body.GetDst(), body.GetType())
	if err != nil {
		return err
	}
	if srcID == dstID {
		// After a merge the two ends can turn out to be one entity, exactly as for an edge. A
		// self-dependency carries no information, and the table's own constraint refuses it.
		return nil
	}

	key := proposalKey(srcID, dstID, typ)
	existing, found, err := p.proposal(ctx, tx, key)
	if err != nil {
		return err
	}

	// Is the relationship already asserted? Then there is nothing to propose, whatever this row
	// says. Checked before the status rules because an edge created by any other path — a derived
	// dependency, a human upsert — settles the question too.
	asserted, err := p.edgeAsserted(ctx, tx, srcID, dstID, typ)
	if err != nil {
		return err
	}
	if asserted {
		return nil
	}

	if found {
		switch existing.status {
		case proposalRejected, proposalConflict:
			// A person said no. Record that the rule disagrees; never reopen the question.
			return p.markProposalConflict(ctx, tx, key, env.GetEventId())
		case proposalConfirmed:
			// Confirmed but the edge is gone — retracted since. Not this handler's business to
			// resurrect it: that needs a person or an upsert, not a rule re-proposing.
			return p.appendProposalEvent(ctx, tx, key, env.GetEventId())
		case proposalPending:
			// Raised once, not re-raised (FR-029). The event list still grows, so the first
			// suspicion keeps its date.
			if existing.ruleID == body.GetRuleId() && existing.score == body.GetScore() &&
				existing.rationale == body.GetRationale() {
				return p.appendProposalEvent(ctx, tx, key, env.GetEventId())
			}
			// A different rule, or the same rule with a changed score: that is new information,
			// so the row is refreshed and the event appended.
		}
	}

	evidence, err := marshalEvidence(body.GetEvidence())
	if err != nil {
		return fmt.Errorf("projector: propose dependency %s: %w", key, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.proposed_dependencies
			(proposal_key, src_entity_id, dst_entity_id, edge_type, rule_id, score, rationale,
			 evidence, created_at, status, proposed_by_event_ids)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, ARRAY[$11]::text[])
		ON CONFLICT (proposal_key) DO UPDATE SET
			rule_id = EXCLUDED.rule_id, score = EXCLUDED.score,
			rationale = EXCLUDED.rationale, evidence = EXCLUDED.evidence,
			proposed_by_event_ids =
				CASE WHEN graph.proposed_dependencies.proposed_by_event_ids @> ARRAY[$11]::text[]
				     THEN graph.proposed_dependencies.proposed_by_event_ids
				     ELSE graph.proposed_dependencies.proposed_by_event_ids || ARRAY[$11]::text[]
				END`,
		key, srcID, dstID, typ.String(), body.GetRuleId(), body.GetScore(), body.GetRationale(),
		evidence, observedAt.UTC(), proposalPending, env.GetEventId()); err != nil {
		return fmt.Errorf("projector: propose dependency %s: %w", key, err)
	}
	return nil
}

// applyConfirmDependency records the decision and CREATES the edge — the one place in this file
// that does, and the whole meaning of "not an edge until confirmed".
//
// The edge is written through applyUpsertEdge rather than by a private INSERT, so a confirmed
// dependency is indistinguishable from any other edge once it exists: the same segment planning,
// the same bitemporal rules, the same provenance column. A second write path for edges would be a
// second set of temporal bugs.
func (p *Projector) applyConfirmDependency(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.ConfirmDependency, observedAt time.Time, principal string,
) error {
	srcID, dstID, typ, err := p.resolveProposalTriple(ctx, tx, body.GetSrc(), body.GetDst(), body.GetType())
	if err != nil {
		return err
	}
	key := proposalKey(srcID, dstID, typ)

	if err := p.decideProposal(ctx, tx, key, proposalConfirmed, principal, env.GetEventId(), observedAt); err != nil {
		return err
	}

	// The provenance chain: this edge names the confirming event, and the proposal row names both
	// this event and every event that proposed it. So "why does this edge exist" walks from the
	// edge to the decision to the evidence, without the edge schema having to carry a second
	// provenance dimension it has no other use for.
	return p.applyUpsertEdge(ctx, tx, env, &graphv1.UpsertEdge{
		Src:              body.GetSrc(),
		Dst:              body.GetDst(),
		Type:             body.GetType(),
		ValidAt:          body.GetValidAt(),
		ValidFromUnknown: body.GetValidFromUnknown(),
	}, observedAt)
}

// applyRejectDependency records that a person looked and said no. It writes no edge, and the row it
// leaves is what makes a later automated proposal a conflict rather than a reopening.
func (p *Projector) applyRejectDependency(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.RejectDependency, observedAt time.Time, principal string,
) error {
	srcID, dstID, typ, err := p.resolveProposalTriple(ctx, tx, body.GetSrc(), body.GetDst(), body.GetType())
	if err != nil {
		return err
	}
	return p.decideProposal(ctx, tx,
		proposalKey(srcID, dstID, typ), proposalRejected, principal, env.GetEventId(), observedAt)
}

// decideProposal moves a proposal to a terminal status, naming who decided and when.
//
// It updates rather than inserting: a decision on a triple with no proposal is refused in
// checkResolvable, so by the time this runs the row exists. The status is set unconditionally
// because a person may overturn their own earlier decision, which is a new event and a new
// decision instant — what may not happen is a RULE overturning one, and no rule reaches here.
func (p *Projector) decideProposal(ctx context.Context, tx pgx.Tx,
	key, status, principal, eventID string, observedAt time.Time,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE graph.proposed_dependencies
		SET status = $2, decided_by = $3, decided_at = $4, decision_event_id = $5
		WHERE proposal_key = $1`,
		key, status, principal, observedAt.UTC(), eventID)
	if err != nil {
		return fmt.Errorf("projector: %s proposal %s: %w", status, key, err)
	}
	if tag.RowsAffected() == 0 {
		// Defence in depth behind checkResolvable, not a substitute for it: reaching here means
		// the pre-check and this handler disagree about what exists, which is a bug rather than
		// bad input, and silently succeeding would leave a decision event with no effect.
		return fmt.Errorf("projector: %s names proposal %s, which does not exist", status, key)
	}
	return nil
}

// markProposalConflict records that a rule goes on proposing what a person rejected (FR-122).
//
// The status moves to `conflict` and stays there: it is not a step back toward `pending`, and the
// decision fields are cleared because the row is no longer a decided one — the person's decision is
// preserved in the decision event and in the graph's own log, which is where an audit reads it. A
// conflict is a thing to show someone, not a thing to act on.
func (p *Projector) markProposalConflict(ctx context.Context, tx pgx.Tx, key, eventID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE graph.proposed_dependencies
		SET status = $2, decided_by = NULL, decided_at = NULL,
		    proposed_by_event_ids =
				CASE WHEN proposed_by_event_ids @> ARRAY[$3]::text[]
				     THEN proposed_by_event_ids
				     ELSE proposed_by_event_ids || ARRAY[$3]::text[]
				END
		WHERE proposal_key = $1`, key, proposalConflict, eventID); err != nil {
		return fmt.Errorf("projector: mark proposal %s in conflict: %w", key, err)
	}
	return nil
}

// appendProposalEvent records that a rule re-derived a proposal without changing it, so the row's
// history says how long it has been suspected while the queue shows it once.
func (p *Projector) appendProposalEvent(ctx context.Context, tx pgx.Tx, key, eventID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE graph.proposed_dependencies
		SET proposed_by_event_ids =
				CASE WHEN proposed_by_event_ids @> ARRAY[$2]::text[]
				     THEN proposed_by_event_ids
				     ELSE proposed_by_event_ids || ARRAY[$2]::text[]
				END
		WHERE proposal_key = $1`, key, eventID); err != nil {
		return fmt.Errorf("projector: append event to proposal %s: %w", key, err)
	}
	return nil
}

// proposal reads the current view of one triple.
func (p *Projector) proposal(ctx context.Context, tx pgx.Tx, key string) (proposalRow, bool, error) {
	var row proposalRow
	err := tx.QueryRow(ctx, `
		SELECT proposal_key, rule_id, score, rationale, status, proposed_by_event_ids
		FROM graph.proposed_dependencies WHERE proposal_key = $1`, key).
		Scan(&row.key, &row.ruleID, &row.score, &row.rationale, &row.status, &row.eventIDs)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return proposalRow{}, false, nil
	case err != nil:
		return proposalRow{}, false, fmt.Errorf("projector: read proposal %s: %w", key, err)
	}
	return row, true, nil
}

// resolveProposalTriple follows the merge chain on both endpoints, so a proposal is filed under
// canonical ids exactly as an edge is. Without it, a proposal made before a merge and one made
// after would be two rows about one relationship.
func (p *Projector) resolveProposalTriple(ctx context.Context, tx pgx.Tx,
	src, dst *graphv1.Ref, typ graphv1.EdgeType,
) (string, string, graph.EdgeType, error) {
	srcID, err := p.canonicalOf(ctx, tx, graph.RefFromProto(src))
	if err != nil {
		return "", "", "", err
	}
	dstID, err := p.canonicalOf(ctx, tx, graph.RefFromProto(dst))
	if err != nil {
		return "", "", "", err
	}
	return srcID, dstID, graph.EdgeTypeFromProto(typ), nil
}

// canonicalOf resolves a ref to the entity it currently belongs to, without creating one.
func (p *Projector) canonicalOf(ctx context.Context, tx pgx.Tx, ref graph.Ref) (string, error) {
	entityID, found, err := p.lookupRef(ctx, tx, ref)
	if err != nil {
		return "", err
	}
	if !found {
		// checkResolvable refuses this before the event is appended; reaching here is a bug.
		return "", fmt.Errorf("projector: ref %s does not resolve", ref)
	}
	return p.follow(ctx, tx, entityID)
}

// edgeAsserted reports whether the relationship is currently asserted, by any path.
func (p *Projector) edgeAsserted(ctx context.Context, tx pgx.Tx, srcID, dstID string, typ graph.EdgeType) (bool, error) {
	var exists bool
	// upper_inf on BOTH ranges is what "asserted right now" means in this schema: the latest
	// observation (observed open above) of a relationship that has not ended (valid open above).
	// Testing only observed would count an edge somebody has since retracted.
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM graph.edge_versions
			WHERE src_id = $1 AND dst_id = $2 AND type = $3
			  AND upper_inf(observed) AND upper_inf(valid))`,
		srcID, dstID, typ.String()).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("projector: check edge %s->%s/%s: %w", srcID, dstID, typ, err)
	}
	return exists, nil
}

// marshalEvidence renders the proposal's evidence as the jsonb the column stores, normalising an
// absent or empty struct to an empty object so a replayed row and a live one are the same bytes.
//
// protojson rather than encoding/json, because a structpb.Struct marshalled by the latter comes out
// as its wire representation ({"fields":{"k":{"Kind":{"StringValue":"v"}}}}) rather than as the
// object it represents — which would be committed, golden, and unreadable.
func marshalEvidence(evidence *structpb.Struct) ([]byte, error) {
	if len(evidence.GetFields()) == 0 {
		return []byte("{}"), nil
	}
	return protojson.Marshal(evidence)
}

// checkProposalRefs refuses a proposal or decision naming an entity the graph has never observed.
//
// A proposal is a question about two known things, and minting a placeholder for either would put a
// phantom in the review queue that nobody can judge. Called from checkResolvable, so a refused
// event never reaches log.events at all (FR-024).
func (p *Projector) checkProposalRefs(ctx context.Context, tx pgx.Tx, field string,
	src, dst *graphv1.Ref,
) (*eventlog.Rejection, error) {
	for _, end := range []struct {
		name string
		ref  graph.Ref
	}{
		{field + ".src", graph.RefFromProto(src)},
		{field + ".dst", graph.RefFromProto(dst)},
	} {
		if strings.TrimSpace(end.ref.Namespace) == "" || strings.TrimSpace(end.ref.Value) == "" {
			return &eventlog.Rejection{
				ReasonCode:   eventlog.ReasonMissingRef,
				ReasonDetail: end.name + " names no entity",
			}, nil
		}
		if _, found, err := p.lookupRef(ctx, tx, end.ref); err != nil {
			return nil, err
		} else if !found {
			return &eventlog.Rejection{
				ReasonCode: eventlog.ReasonRefUnresolvable,
				ReasonDetail: fmt.Sprintf("%s names %s, which the graph has never observed; a "+
					"proposed dependency is a question about two known things and may not create "+
					"either", end.name, end.ref),
			}, nil
		}
	}
	return nil, nil
}

// checkProposalDecision refuses a decision with no authenticated principal (FR-041) and a decision
// on a triple no rule ever proposed.
//
// The second half is what distinguishes this from a merge decision. Confirming or rejecting a merge
// means something on its own — a person may assert that two entities are one whether or not a rule
// noticed. A dependency decision does not: confirming a proposal nobody made would claim the graph
// had suggested something it never did, and asserting an edge on a person's own authority is what
// upsert_edge is for.
func (p *Projector) checkProposalDecision(ctx context.Context, tx pgx.Tx, principal, field string,
	src, dst *graphv1.Ref, typ graphv1.EdgeType,
) (*eventlog.Rejection, error) {
	if strings.TrimSpace(principal) == "" {
		return &eventlog.Rejection{
			ReasonCode: eventlog.ReasonMissingPrincipal,
			ReasonDetail: field + " carries no authenticated individual; the graph does not accept a " +
				"dependency decision from an anonymous or shared credential (FR-041)",
		}, nil
	}
	if rejection, err := p.checkProposalRefs(ctx, tx, field, src, dst); err != nil || rejection != nil {
		return rejection, err
	}

	srcID, dstID, edgeType, err := p.resolveProposalTriple(ctx, tx, src, dst, typ)
	if err != nil {
		return nil, err
	}
	key := proposalKey(srcID, dstID, edgeType)
	if _, found, err := p.proposal(ctx, tx, key); err != nil {
		return nil, err
	} else if !found {
		return &eventlog.Rejection{
			ReasonCode: eventlog.ReasonRefUnresolvable,
			ReasonDetail: fmt.Sprintf("%s names the dependency %s, which nothing has proposed; a "+
				"decision may not invent the proposal it decides. To assert this edge on your own "+
				"authority, use upsert_edge", field, key),
		}, nil
	}
	return nil, nil
}
