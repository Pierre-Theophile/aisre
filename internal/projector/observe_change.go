// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Projecting observe_change (FR-003, FR-004, edge case "dangling change").
//
// A change is a node, not an annotation (constitution, Definitions). That is the decision the
// whole ranking story rests on: because a rollout is an entity with `changed_by` edges to what
// it changed, "what changed near checkout between 13:00 and 14:32" is a graph query over the
// same bitemporal machinery as everything else, not a separate event feed that has to be
// correlated after the fact.
//
// Two details are specific to changes:
//
//   - A change happens at an instant, but a range cannot be empty. A change with no stated end
//     is therefore stored over the shortest non-empty half-open interval PostgreSQL can
//     represent, [t, t+1µs). Giving it an open end instead would make every past change
//     intersect every future window, which would wreck the diff query (FR-027).
//   - A target the graph cannot resolve does not make the change disappear. It is recorded on
//     the change node as an unattached target and the edge is created later, when the target
//     finally shows up (attach.go). Silently dropping a change because its target has not been
//     ingested yet would hide exactly the events an incident is about.

// UnattachedTargetsProp lists the change's targets that did not resolve, as "namespace=value"
// strings. It is how a diff flags a change as unattached (FR-028) and how attach.go finds the
// changes waiting for a node.
const UnattachedTargetsProp = "sre.change.unattached_targets"

// changeInstant is the width given to a change with no stated end: one microsecond, the
// resolution of a PostgreSQL timestamptz, so the interval is the shortest non-empty one that
// can be stored.
const changeInstant = time.Microsecond

func (p *Projector) applyObserveChange(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.ObserveChange, observedAt time.Time) error {
	ref := graph.RefFromProto(body.GetRef())
	entityID, err := p.resolveRef(ctx, tx, ref, graph.NodeTypeChange, env.GetEventId(), env.GetSourceId(), observedAt)
	if err != nil {
		return err
	}
	if err := p.storeClaim(ctx, tx, entityID, ref, nil, env.GetSourceId(), env.GetEventId(), observedAt); err != nil {
		return err
	}
	if _, err := p.addFacet(ctx, tx, entityID, graph.NodeTypeChange); err != nil {
		return err
	}
	entity, found, err := p.entity(ctx, tx, entityID)
	if err != nil || !found {
		return err
	}

	// A change whose start nobody stated begins at the observation and is marked unknown (003 FR-069).
	// Reading a missing valid_at as the zero timestamp would date it in 1970 — which is not merely
	// wrong but plausibly wrong, because an ancient change ranks as maximally distant and is never
	// excluded as a future announcement, so it looks like a fact.
	//
	// # Which observation, and why it is not the ingest instant (004)
	//
	// "The observation" is the instant the SOURCE learned the fact, where the feeder stated one, and only
	// the ingest instant otherwise. The difference is replay determinism.
	//
	// The ingest instant is assigned per event as events are applied, so two events delivered in one
	// arrival window get different ones depending on the order they happen to arrive in. Dating a change
	// from it therefore makes its VALID time — a statement about the world — depend on the order the
	// graph was told things, which FR-021 forbids and a fixture's shuffle step catches.
	//
	// It went uncaught until `vercel-promotion-01`: the corpus's other unknown-start changes each sit
	// alone in their arrival window, so permuting them moves nothing. The Vercel connector emits a change
	// and its correlation key together on every promotion, which is the ordinary case rather than a
	// contrived one, and the shuffle failed on two seeds out of six with the interval shifted by a
	// microsecond.
	//
	// `source_observed_at` is a property of the payload rather than of application order, so it is stable
	// under permutation. It is also the better answer on its own terms: "when the source learned it" is
	// closer to when the change happened than "when we got round to ingesting it", and the field was
	// already on the envelope, carried as provenance and read by nothing.
	var sourceObservedAt *time.Time
	if src := env.GetSourceObservedAt(); src != nil {
		at := src.AsTime().UTC()
		sourceObservedAt = &at
	}
	observation := newChangeObservation(env.GetEventId(), env.GetSourceId(), body, sourceObservedAt, observedAt)
	validAt, validEnd := observation.validAt, observation.validEnd

	// Resolve the targets first: the ones that do not resolve yet are recorded on the change node,
	// by the fold, as waiting.
	var attached []string
	for _, target := range body.GetTargets() {
		targetID, found, err := p.lookupRef(ctx, tx, graph.RefFromProto(target))
		if err != nil {
			return err
		}
		if found {
			attached = append(attached, targetID)
		}
	}

	// The record is the fold of everything said about this change, not this statement alone: after a
	// C8 merge the entity carries another source's observation too, and replacing it with this one
	// would make the stored change depend on which source was applied last (change_fold.go).
	current, err := p.currentNodeRows(ctx, tx, entityID)
	if err != nil {
		return err
	}
	observations, err := p.changeObservations(ctx, tx, eventIDsOf(current))
	if err != nil {
		return err
	}
	observations = append(observations, observation)
	if err := p.writeChangeRecord(ctx, tx, entityID, current, observations, entity.facets,
		env.GetEventId(), observedAt); err != nil {
		return err
	}

	for _, targetID := range attached {
		if err := p.linkChange(ctx, tx, targetID, entityID, validAt, validEnd,
			[]string{env.GetEventId()}, env.GetEventId(), observedAt); err != nil {
			return err
		}
	}
	// This change now has targets it did not have a moment ago, so a rule whose answer depends on the
	// targets a change shares has to be asked again — the third way C8's answer can change after its
	// evidence is stored, and the one retrigger.go's enumeration was missing.
	//
	// It is reached when a correlation key arrives BEFORE the observation it describes: the key's
	// subject is minted as a bare change entity, C8 evaluates it against a change with no targets and
	// finds no shared target, and then this event supplies them. attachWaiting covers the mirror case
	// (the TARGET arrives last) and refs.go covers a merge, but neither covers this one, because the
	// change is not waiting for anything here — it is the thing that arrived.
	//
	// A fixture's shuffle found it. Both orders are a real recording: a platform reports a revision
	// and a pipeline reports having deployed it, from two separate polls, in whichever order the two
	// polls return.
	return p.queueDeployKeys(ctx, tx, []string{entityID},
		"a change gained its targets after a deploy key had already been stored on it",
		env.GetEventId(), observedAt)
}

// changeProps is what a change node carries: the feeder's own properties, plus the unattached-target
// list the projector computes.
//
// The feeder's half was missing until 004, and two features had been written as though it were there —
// feature 003's contract states a revision-creation change is "marked as not having moved production
// traffic", and its feeder built that property and then discarded it because `Change` had nowhere to
// put one. So this function used to return the unattached list and nothing else, and every property a
// feeder computed about a change was dropped on the floor.
//
// The unattached list wins a collision, deliberately. It is the projector's own statement about what
// it could not resolve, and a feeder that claimed the same key would otherwise be overwriting the
// graph's account of its own resolution with an assertion the graph has no way to check.
func changeProps(unattached []string, declared *structpb.Struct, sourceID, eventID string) propSet {
	out := propSet{}
	for key, value := range declared.GetFields() {
		if key == UnattachedTargetsProp {
			// See above. Counted nowhere and refused silently would be worse: a feeder writing this key
			// is a feeder that thinks it resolves targets, which is a misunderstanding worth failing on
			// — but a projector that rejected the whole event would lose a change that really happened.
			continue
		}
		out[key] = []propRecord{{
			Value:    value.AsInterface(),
			SourceID: sourceID,
			EventID:  eventID,
		}}
	}
	if len(unattached) == 0 {
		return out
	}
	values := make([]any, 0, len(unattached))
	for _, target := range unattached {
		values = append(values, target)
	}
	out[UnattachedTargetsProp] = []propRecord{{
		Value:    values,
		SourceID: sourceID,
		EventID:  eventID,
	}}
	return out
}

// writeRecordVersion stores a *record* node's single version, correcting it if the same record
// is observed again with different details.
//
// A record does not go through the assertion segmentation the other node types use: it has one
// bounded valid interval given by the event — a change happened at an instant, an investigation
// ran from one instant to another — rather than a fact that starts at an instant and runs until
// contradicted, so there is nothing to split. Correcting one closes the observed interval of
// the version that said something else and opens a new one, exactly as constitution II
// requires; the old version stays readable as-known-at.
//
// `investigation` (record_investigation, label_investigation) uses it. `change` used to, and now
// goes through writeChangeRecord instead, because a change can be merged with another source's
// observation of the same rollout and its record is then a fold rather than one statement
// (change_fold.go); an investigation has one author and no such merge.
func (p *Projector) writeRecordVersion(ctx context.Context, tx pgx.Tx, entityID string, validAt, validEnd time.Time, fromUnknown bool, content nodeContent, eventID string, observedAt time.Time) error {
	existing, err := p.currentNodeRows(ctx, tx, entityID)
	if err != nil {
		return err
	}
	for _, row := range existing {
		// The unknown marker is part of the identity of the interval: a version that said "from
		// here, and we do not know since when" and one that said "from here" are two different
		// claims, so replacing one with the other is a correction rather than a no-op.
		if row.valid.Start.Equal(validAt) && !row.valid.EndUnbounded && row.valid.End.Equal(validEnd) &&
			row.fromUnknown == fromUnknown && content.equals(row) {
			return nil
		}
	}
	for _, row := range existing {
		if err := closeObserved(ctx, tx, "entity_versions", row.versionID, observedAt, eventID); err != nil {
			return err
		}
	}
	seg := segment{
		start:       validAt,
		end:         validEnd,
		fromUnknown: fromUnknown,
		boundary:    []string{eventID},
	}
	return p.insertNodeVersion(ctx, tx, entityID, seg, content, eventID, observedAt)
}

// linkChange creates the `changed_by` edge from what changed to the change that changed it.
//
// The direction is "target changed_by change", which reads the way the edge type is named and
// puts the change one hop from the node an alert names — the property ADR-0001 D7 is about.
func (p *Projector) linkChange(ctx context.Context, tx pgx.Tx, targetID, changeID string, validAt, validEnd time.Time, producedBy []string, eventID string, observedAt time.Time) error {
	key := edgeKey{srcID: targetID, dstID: changeID, typ: graph.EdgeTypeChangedBy}
	if key.srcID == key.dstID {
		return nil
	}
	existing, err := p.currentEdgeRows(ctx, tx, key)
	if err != nil {
		return err
	}
	for _, row := range existing {
		if row.valid.Start.Equal(validAt) && !row.valid.EndUnbounded && row.valid.End.Equal(validEnd) {
			return nil
		}
	}
	evidence := slices.Clone(producedBy)
	slices.Sort(evidence)
	evidence = slices.Compact(evidence)

	versionID := graph.EdgeSegmentVersionID(key.srcID, key.dstID, string(key.typ), eventID, validAt)
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.edge_versions (
			version_id, src_id, dst_id, type, valid, observed, props, produced_by_event_ids)
		VALUES ($1, $2, $3, $4, $5, $6, '{}'::jsonb, $7)
		ON CONFLICT (version_id) DO NOTHING`,
		versionID, key.srcID, key.dstID, string(key.typ),
		postgres.NewTimeRange(validAt, validEnd), postgres.OpenTimeRange(observedAt.UTC()),
		evidence); err != nil {
		return fmt.Errorf("projector: link change %s to %s: %w", changeID, targetID, err)
	}
	return nil
}

// structJSON renders a protobuf Struct as canonical JSON for storage.
func structJSON(s *structpb.Struct) (json.RawMessage, error) {
	if s == nil || len(s.GetFields()) == 0 {
		return nil, nil
	}
	canonical, err := graph.CanonicalJSON(s)
	if err != nil {
		return nil, fmt.Errorf("projector: encode attributes: %w", err)
	}
	return canonical, nil
}
