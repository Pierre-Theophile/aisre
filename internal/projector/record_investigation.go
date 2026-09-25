// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Projecting the decision record (ADR-0005 D2/D3, 002 FR-033 to FR-035, 002 data-model
// §"What this feature writes into the graph").
//
// An investigation is a *record*, and the graph holds it the way it holds a change: a node with
// one bounded valid interval — the run, `[started_at, ended_at)` — carrying what was concluded,
// with edges to the entities it was about. That is not a storage convenience. The constitution
// says the graph is the source of truth for facts about how the production system is operated,
// and "at 14:32 on the first of September we concluded that the payments rollout caused this,
// with this confidence, having spent this much" is exactly such a fact. It is bitemporal for
// the same reason everything else is: the conclusion belongs to the window it was about, and
// what we believed about that window is allowed to change later without the first belief
// disappearing.
//
// What does NOT come in here is the run's working material — judgments, worker calls, ledger
// updates, model calls. Those are rebuildable from the recording and they are not facts about
// production; putting them in the log would make the log a debugging trace (constitution III).
// Nor does any telemetry: the props carry identifiers, parameters, statuses, confidences and
// digests, under the allow-listed `sre.investigation.*` namespace, and internal/log/validate.go
// refuses the rest before this file ever sees it (002 SC-010).
//
// This file also holds the small amount of machinery the other four investigation handlers
// share, because they are all the same two moves: name an investigation, and attach it to
// something.

// InvestigationNamespace is the published ref namespace of an INVESTIGATION node. An
// investigation id is globally unique on its own, so the namespace is a constant rather than
// anything derived: `sre.investigation.id=inv-2026-09-01-0007`.
const InvestigationNamespace = "sre.investigation.id"

// Props written on an INVESTIGATION node, all inside the allow-listed namespace. They are
// published: 002 reads them back and the fixtures assert them.
const (
	InvestigationOutcomeProp    = "sre.investigation.outcome"
	InvestigationStopReasonProp = "sre.investigation.stop_reason"
	InvestigationVerdictProp    = "sre.investigation.verdict_line"
	InvestigationRequesterProp  = "sre.investigation.requester"
	InvestigationRecordingProp  = "sre.investigation.recording_key"
	InvestigationDigestProp     = "sre.investigation.recording_digest"
	InvestigationHypothesesProp = "sre.investigation.hypotheses"
	InvestigationSpendProp      = "sre.investigation.spend"
	InvestigationModelProp      = "sre.investigation.model_config"
	InvestigationReopenedProp   = "sre.investigation.reopened_from"
	InvestigationReopenCause    = "sre.investigation.reopen_cause"
	InvestigationLabelProp      = "sre.investigation.was_this_right"
	InvestigationLabelledByProp = "sre.investigation.labelled_by"
	InvestigationLabelledAtProp = "sre.investigation.labelled_at"
	InvestigationFactKindProp   = "sre.investigation.fact_kind"
	InvestigationFactAuthorProp = "sre.investigation.fact_author"
)

// labelProps are the props a later label adds to an investigation version. record_investigation
// carries them forward, so that a label that arrived before the conclusion is not lost when the
// conclusion overwrites the version it landed on — which is what makes the pair order
// independent, as the fixture shuffle requires (FR-021).
var labelProps = []string{
	InvestigationLabelProp, InvestigationLabelledByProp, InvestigationLabelledAtProp,
}

// InvestigationRef is the ref an investigation id denotes.
func InvestigationRef(id string) graph.Ref {
	return graph.Ref{Namespace: InvestigationNamespace, Value: id}
}

func (p *Projector) applyRecordInvestigation(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.RecordInvestigation, observedAt time.Time) error {
	entityID, facets, err := p.investigationEntity(ctx, tx, env, body.GetInvestigationId(), observedAt)
	if err != nil {
		return err
	}

	start := body.GetStartedAt().AsTime().UTC()
	end := body.GetEndedAt().AsTime().UTC()
	if !end.After(start) {
		// A run that started and ended inside the same microsecond is still a run. The
		// interval has to be non-empty, so it takes the same shortest one a change does.
		end = start.Add(changeInstant)
	}

	props := propSet{}
	setProp(props, env, InvestigationOutcomeProp, body.GetOutcome())
	setProp(props, env, InvestigationStopReasonProp, body.GetStopReason())
	setProp(props, env, InvestigationVerdictProp, body.GetVerdictLine())
	setProp(props, env, InvestigationRequesterProp, body.GetRequester())
	setProp(props, env, InvestigationRecordingProp, body.GetRecordingKey())
	setProp(props, env, InvestigationDigestProp, body.GetRecordingDigest())
	setStructProp(props, env, InvestigationHypothesesProp, body.GetHypotheses())
	setStructProp(props, env, InvestigationSpendProp, body.GetSpend())
	setStructProp(props, env, InvestigationModelProp, body.GetModelConfig())

	// Carry forward anything a reopen or a label already recorded about this investigation.
	// The conclusion is the authority on what was concluded; it is not the authority on
	// whether a human later said it was right.
	current, err := p.currentNodeRows(ctx, tx, entityID)
	if err != nil {
		return err
	}
	carryProps(props, current, append(slices.Clone(labelProps),
		InvestigationReopenedProp, InvestigationReopenCause))

	content := nodeContent{
		displayName: body.GetVerdictLine(),
		props:       props,
		conflicts:   props.conflicts(),
		facets:      facets,
	}
	if err := p.writeRecordVersion(ctx, tx, entityID, start, end, false, content, env.GetEventId(), observedAt); err != nil {
		return err
	}

	// INVESTIGATED edges to every subject and every target, over the run's interval. Subjects
	// and targets are deliberately not distinguished on the edge: both are "this investigation
	// was about that entity", and which of them the alert named is a property of the
	// investigation, not of the relationship.
	for _, ref := range append(refsOf(body.GetSubjects()), refsOf(body.GetTargetEntities())...) {
		targetID, err := p.resolveRef(ctx, tx, ref, graph.NodeTypeUnspecified,
			env.GetEventId(), env.GetSourceId(), observedAt)
		if err != nil {
			return err
		}
		key := edgeKey{srcID: entityID, dstID: targetID, typ: graph.EdgeTypeInvestigated}
		if err := p.linkRecord(ctx, tx, key, start, &end, propSet{}, env, observedAt); err != nil {
			return err
		}
	}
	return nil
}

// investigationEntity resolves an investigation id to its entity, creating it on first sight
// and recording the `investigation` facet.
//
// First sight is routine rather than exceptional: a reopen names the child before the child has
// concluded, and a human fact may arrive against a run this graph has only heard of. The entity
// exists from the first mention and its version arrives when the record does.
func (p *Projector) investigationEntity(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, id string, observedAt time.Time) (string, []graph.NodeType, error) {
	if strings.TrimSpace(id) == "" {
		return "", nil, fmt.Errorf("projector: event %s names no investigation", env.GetEventId())
	}
	ref := InvestigationRef(id)
	entityID, err := p.resolveRef(ctx, tx, ref, graph.NodeTypeInvestigation,
		env.GetEventId(), env.GetSourceId(), observedAt)
	if err != nil {
		return "", nil, err
	}
	if err := p.storeClaim(ctx, tx, entityID, ref, nil, env.GetSourceId(), env.GetEventId(), observedAt); err != nil {
		return "", nil, err
	}
	if _, err := p.addFacet(ctx, tx, entityID, graph.NodeTypeInvestigation); err != nil {
		return "", nil, err
	}
	entity, found, err := p.entity(ctx, tx, entityID)
	if err != nil {
		return "", nil, err
	}
	if !found {
		return "", nil, fmt.Errorf("projector: investigation %s vanished after creation", id)
	}
	return entityID, entity.facets, nil
}

// linkRecord writes one record edge — INVESTIGATED, CONCERNS or WATCHES — over [start, end),
// merging rather than colliding with what is already there.
//
// Why merge. `graph.edge_versions` refuses two current rows of the same (src, dst, type) whose
// valid intervals overlap, and a record edge is asserted by events that each name their own
// interval: three human facts about the same entity, each with its own `concerns_from`, are
// three assertions of one relationship, not three relationships. So an overlapping assertion
// closes the rows it overlaps and opens one row over the union of their intervals, carrying the
// union of their evidence and their props. The union is commutative and idempotent, which is
// what makes the result independent of the order the facts arrived in (FR-021, FR-023) — the
// property the fixture shuffle exists to check.
//
// A nil end means the edge has no stated upper bound: "this investigation concerns this entity
// from here on".
func (p *Projector) linkRecord(ctx context.Context, tx pgx.Tx, key edgeKey, start time.Time, end *time.Time, props propSet, env *graphv1.EventEnvelope, observedAt time.Time) error {
	if key.srcID == key.dstID {
		return nil
	}
	existing, err := p.currentEdgeRows(ctx, tx, key)
	if err != nil {
		return err
	}

	merged := postgres.FromBounds(start.UTC(), end)
	evidence := []string{env.GetEventId()}
	out := propSet{}
	for k, v := range props {
		out[k] = v
	}

	var overlapping []*edgeRow
	for _, row := range existing {
		if !rangesMeet(row.valid, merged) {
			continue
		}
		overlapping = append(overlapping, row)
		merged = unionRange(merged, row.valid)
		evidence = append(evidence, row.producedBy...)
		for k, v := range row.props {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	if len(overlapping) == 1 && sameRange(overlapping[0].valid, merged) &&
		out.valuesEqual(overlapping[0].props) {
		// Nothing changed: the same assertion, already stored.
		return nil
	}
	for _, row := range overlapping {
		if err := closeObserved(ctx, tx, "edge_versions", row.versionID, observedAt, env.GetEventId()); err != nil {
			return err
		}
	}

	slices.Sort(evidence)
	evidence = slices.Compact(evidence)
	propsJSON, err := out.MarshalJSON()
	if err != nil {
		return fmt.Errorf("projector: encode props of %s edge: %w", key.typ, err)
	}
	versionID := graph.EdgeSegmentVersionID(key.srcID, key.dstID, string(key.typ),
		env.GetEventId(), merged.Start)
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.edge_versions (
			version_id, src_id, dst_id, type, valid, observed, props, produced_by_event_ids)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (version_id) DO NOTHING`,
		versionID, key.srcID, key.dstID, string(key.typ),
		merged, postgres.OpenTimeRange(observedAt.UTC()), propsJSON, evidence); err != nil {
		return fmt.Errorf("projector: link %s %s -> %s: %w", key.typ, key.srcID, key.dstID, err)
	}
	return nil
}

// rangesMeet reports whether two half-open intervals overlap or abut. Abutting counts: two
// facts about [13:00, 14:00) and [14:00, 15:00) describe one continuous relationship, and
// storing them as two rows would only make the reader reassemble them.
func rangesMeet(a, b postgres.TimeRange) bool {
	if a.EndUnbounded && b.EndUnbounded {
		return true
	}
	if a.EndUnbounded {
		return !b.End.Before(a.Start)
	}
	if b.EndUnbounded {
		return !a.End.Before(b.Start)
	}
	return !a.End.Before(b.Start) && !b.End.Before(a.Start)
}

func unionRange(a, b postgres.TimeRange) postgres.TimeRange {
	start := a.Start
	if b.Start.Before(start) {
		start = b.Start
	}
	if a.EndUnbounded || b.EndUnbounded {
		return postgres.OpenTimeRange(start)
	}
	end := a.End
	if b.End.After(end) {
		end = b.End
	}
	return postgres.NewTimeRange(start, end)
}

func sameRange(a, b postgres.TimeRange) bool {
	if a.EndUnbounded != b.EndUnbounded {
		return false
	}
	if !a.Start.Equal(b.Start) {
		return false
	}
	return a.EndUnbounded || a.End.Equal(b.End)
}

// setProp records one string property, skipping the empty ones so that a record which says
// nothing about a field serialises to nothing at all — the same rule the actor kind follows,
// and what keeps a golden from moving when a field is added (plan §"the 001 change package").
func setProp(props propSet, env *graphv1.EventEnvelope, key, value string) {
	if value == "" {
		return
	}
	props[key] = []propRecord{{Value: value, SourceID: env.GetSourceId(), EventID: env.GetEventId()}}
}

func setBoolProp(props propSet, env *graphv1.EventEnvelope, key string, value bool) {
	props[key] = []propRecord{{Value: value, SourceID: env.GetSourceId(), EventID: env.GetEventId()}}
}

// setStructProp records a structured property. Validation has already refused anything in it
// that is not an identifier, a parameter, a status, a confidence or a digest
// (internal/log/validate.go, InvestigationPropNamespace).
func setStructProp(props propSet, env *graphv1.EventEnvelope, key string, value *structpb.Struct) {
	if value == nil || len(value.GetFields()) == 0 {
		return
	}
	props[key] = []propRecord{{
		Value:    value.AsMap(),
		SourceID: env.GetSourceId(),
		EventID:  env.GetEventId(),
	}}
}

// carryProps copies the named props from whichever current version already holds them onto a
// version about to replace it.
func carryProps(dst propSet, rows []*nodeRow, keys []string) {
	for _, row := range rows {
		for _, key := range keys {
			if records, ok := row.props[key]; ok {
				if _, taken := dst[key]; !taken {
					dst[key] = records
				}
			}
		}
	}
}

func refsOf(refs []*graphv1.Ref) []graph.Ref {
	out := make([]graph.Ref, 0, len(refs))
	for _, ref := range refs {
		out = append(out, graph.RefFromProto(ref))
	}
	return out
}
