// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Late attachment of dangling changes and alerts (edge case "dangling change", FR-028;
// 003 FR-048).
//
// A change often arrives before the thing it changed. The Kubernetes feeder sees a rollout of
// `shop/payments` in its change stream seconds before the informer has listed the Deployment;
// a deploy pipeline reports a release for a service the topology feeder has not seen traffic
// for yet. Dropping the change would lose exactly the event an incident is about, so the change
// node is kept with its targets recorded as unattached, and the edge is created the moment the
// target appears.
//
// The attachment is recorded as evidence from *both* sides: the changed_by edge's
// produced_by_event_ids names the change event and the event that finally created the node
// (FR-034), so an operator asking why that edge exists sees both halves of the story. The
// change node is corrected at the same time — the target is removed from its unattached list —
// which is a normal correction: observed interval closed, new version opened.
//
// ---------------------------------------------------------------------------------------------
// Alerts take the same path, and feature 003 put them on it (FR-048: "an alert whose targets
// cannot be resolved MUST be kept and marked unattached, and MUST attach automatically if a
// target later appears").
//
// They did not before, and what they did instead looked harmless: the WATCHES edge went through
// applyUpsertEdge, which mints a PLACEHOLDER entity for an endpoint nothing has described. So the
// edge existed immediately, pointing at an entity with no type and no description.
//
// That is worse than no edge, for one reason. An investigation walking WATCHES out of a firing
// alert could not distinguish "this alert watches a service we know about" from "this alert
// watches a name we have never seen" — both are an edge to an entity, and the second is a
// placeholder only if you go and look. The unattached list makes the second a STATED fact about
// the alert, which is the same reason the change path works this way: a graph that cannot say
// what it does not know will be read as knowing.
//
// The three pieces below are therefore shared rather than duplicated per node kind: the waiting
// query is parameterised by the property, the correction is parameterised by the property, and
// only the edge each one creates differs.

// attachWaiting is called whenever an entity is created. It links everything that named this ref
// before anything described it: changes waiting for a target, and alerts waiting for something to
// watch.
//
// One entry point rather than two call sites, so a future node kind that can dangle is added here
// and cannot be forgotten at one of the places entities are created (refs.go and split_entity.go
// are both of them today).
func (p *Projector) attachWaiting(ctx context.Context, tx pgx.Tx, entityID string, ref graph.Ref, eventID string, observedAt time.Time) error {
	if err := p.attachChanges(ctx, tx, entityID, ref, eventID, observedAt); err != nil {
		return err
	}
	return p.attachAlerts(ctx, tx, entityID, ref, eventID, observedAt)
}

// attachChanges finds the change nodes waiting for this ref and links them.
//
// A change that attaches here gains a target it did not have, so a rule whose answer depends on the
// targets a change shares has to be asked again — see retrigger.go. C8 is the only such rule, and
// without this it would fire only when the target happened to exist before the deploy claim arrived,
// which is the arrival-order dependence FR-021 forbids and a fixture's shuffle would find.
func (p *Projector) attachChanges(ctx context.Context, tx pgx.Tx, entityID string, ref graph.Ref, eventID string, observedAt time.Time) error {
	waiting, err := p.nodesWaitingFor(ctx, tx, ref, UnattachedTargetsProp, "change IS NOT NULL", observedAt)
	if err != nil {
		return err
	}
	attached := make([]string, 0, len(waiting))
	for _, change := range waiting {
		if err := p.linkChange(ctx, tx, entityID, change.entityID, change.valid.Start,
			changeEnd(change.valid), append(slices.Clone(change.producedBy), eventID),
			eventID, observedAt); err != nil {
			return err
		}
		if err := p.dropUnattachedTarget(ctx, tx, change, ref, UnattachedTargetsProp,
			eventID, observedAt); err != nil {
			return err
		}
		attached = append(attached, change.entityID)
	}
	slices.Sort(attached)
	return p.queueDeployKeys(ctx, tx, slices.Compact(attached),
		"a change attached to a target that did not exist when its claim arrived", eventID, observedAt)
}

// attachAlerts finds the alerts waiting to watch this ref and creates their WATCHES edges
// (003 FR-048).
//
// The edge's valid time is the naming transition's, not the moment the target showed up: dating it
// from the discovery would say the alert started watching when the graph happened to learn about the
// target — an arrival-order artefact of exactly the kind FR-021 forbids.
func (p *Projector) attachAlerts(ctx context.Context, tx pgx.Tx, entityID string, ref graph.Ref, eventID string, observedAt time.Time) error {
	waiting, err := p.nodesWaitingFor(ctx, tx, ref, AlertUnattachedWatchesProp, "true", observedAt)
	if err != nil {
		return err
	}
	// One edge per ALERT, dated from the alert's first transition — the instant the edge would have had
	// if the target had been known when that transition arrived, which is what applyAlertTransition
	// gives it on the direct path.
	//
	// An alert's state is segmented in valid time: one that has fired and recovered has two current
	// versions. WHAT IT WATCHES is not a property of a state segment, so one edge is created, not one
	// per version, and it starts at the earliest version a transition wrote (the one carrying
	// sre.alert.state).
	//
	// It used to start at the alert's earliest current version of ANY kind. For a monitor whose
	// definition a feeder asserts, that is the definition's instant, weeks before the transition, and it
	// made the edge's start depend on arrival order: with the target already known, the direct path
	// dates it from the transition, so a corpus in which the target arrived first and one in which it
	// arrived second gave two graphs. datadog-monitor-transitions-01's shuffle step caught it (005 T059),
	// the reordering window covering a transition and the discovery tick that asserted its target; an
	// arrival-order artefact is exactly what FR-021 forbids.
	byAlert := map[string][]string{}
	for _, alert := range waiting {
		byAlert[alert.entityID] = append(byAlert[alert.entityID], alert.producedBy...)
	}
	for alertID, producedBy := range byAlert {
		start, err := p.earliestTransition(ctx, tx, alertID)
		if err != nil {
			return err
		}
		if err := p.linkWatches(ctx, tx, alertID, entityID, start,
			append(slices.Clone(producedBy), eventID), eventID, observedAt); err != nil {
			return err
		}
	}
	// Every waiting version is corrected, not just the earliest: each one's list must stop naming a
	// target that is now attached, or a later reader sees the gap the graph has already closed.
	for _, alert := range waiting {
		if err := p.dropUnattachedTarget(ctx, tx, alert, ref, AlertUnattachedWatchesProp,
			eventID, observedAt); err != nil {
			return err
		}
	}
	return nil
}

// linkWatches creates one WATCHES edge with two-sided provenance: the transition that named the
// target and the event that finally created it (FR-048, mirroring FR-034 for changes).
//
// Direction is alert → watched, as AlertWatchAssertion documents: the way the edge type reads and
// the way an investigation walks it, from the thing that fired to the things it was watching.
//
// The valid interval is open-ended, unlike a change's. A change happened at an instant; an alert
// goes on watching until something says otherwise, so an end would be an invention.
func (p *Projector) linkWatches(ctx context.Context, tx pgx.Tx, alertID, watchedID string, validAt time.Time, producedBy []string, eventID string, observedAt time.Time) error {
	key := edgeKey{srcID: alertID, dstID: watchedID, typ: graph.EdgeTypeWatches}
	if key.srcID == key.dstID {
		return nil
	}
	existing, err := p.currentEdgeRows(ctx, tx, key)
	if err != nil {
		return err
	}
	for _, row := range existing {
		if row.valid.Start.Equal(validAt) && row.valid.EndUnbounded {
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
		postgres.OpenTimeRange(validAt), postgres.OpenTimeRange(observedAt.UTC()),
		evidence); err != nil {
		return fmt.Errorf("projector: link alert %s to %s: %w", alertID, watchedID, err)
	}
	return nil
}

func changeEnd(valid postgres.TimeRange) time.Time {
	if valid.EndUnbounded {
		return valid.Start.Add(changeInstant)
	}
	return valid.End
}

// waitingChange is a current change version that still lists a target it could not resolve.
type waitingChange struct {
	entityID   string
	versionID  string
	valid      postgres.TimeRange
	producedBy []string
}

// nodesWaitingFor finds the current node versions whose unattached list, under propKey, names ref.
// The containment operator matches inside the stored property record, so the lookup is one indexed
// jsonb query rather than a scan of every node.
//
// `extra` narrows it to the node kind the caller means — `change IS NOT NULL` for a change. It is a
// literal rather than a parameter because it is a schema predicate written by this file's two
// callers, never by input.
func (p *Projector) nodesWaitingFor(ctx context.Context, tx pgx.Tx, ref graph.Ref, propKey, extra string, observedAt time.Time) ([]waitingChange, error) {
	filter, err := json.Marshal(map[string]any{
		propKey: map[string]any{"value": []string{refString(ref)}},
	})
	if err != nil {
		return nil, fmt.Errorf("projector: build unattached-target filter: %w", err)
	}
	// `lower(observed) <= $2` is what keeps a correction from running backwards.
	//
	// Attaching rewrites the waiting node — observed interval closed, new version opened — and
	// closing an interval at an instant before it opened is refused by the database, correctly. That
	// can happen whenever a node-creating event is observed EARLIER than the version waiting for it,
	// which is not exotic: a late-arriving fact carries the observed time it was true at, so an
	// investigation event observed at 14:36 can be applied after an alert observed at 14:38 and
	// create the entity that alert was waiting for.
	//
	// A version observed after this event is also, logically, one this event cannot inform: at the
	// instant that version was observed the entity already existed, so it should not have listed the
	// ref as unattached at all. Whatever wrote it was working from a later view, and correcting it
	// from an earlier one would be backdating a discovery.
	//
	// This guard was added for the alert path and applies to changes too, where the same hazard was
	// latent — reachable by any corpus that interleaves observed times, and simply not exercised.
	rows, err := tx.Query(ctx, `
		SELECT entity_id, version_id, valid, produced_by_event_ids
		FROM graph.entity_versions
		WHERE upper_inf(observed) AND `+extra+` AND props @> $1::jsonb
		  AND lower(observed) <= $2
		ORDER BY version_id`, filter, observedAt.UTC())
	if err != nil {
		return nil, fmt.Errorf("projector: find nodes waiting for %s: %w", ref, err)
	}
	defer rows.Close()

	var out []waitingChange
	for rows.Next() {
		var change waitingChange
		if err := rows.Scan(&change.entityID, &change.versionID, &change.valid, &change.producedBy); err != nil {
			return nil, fmt.Errorf("projector: scan waiting change: %w", err)
		}
		out = append(out, change)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: find nodes waiting for %s: %w", ref, err)
	}
	return out, nil
}

// dropUnattachedTarget rewrites a node version without the target that has just been attached, so
// the unattached list always says what is STILL missing rather than what once was.
func (p *Projector) dropUnattachedTarget(ctx context.Context, tx pgx.Tx, change waitingChange, ref graph.Ref, propKey, eventID string, observedAt time.Time) error {
	rows, err := p.currentNodeRows(ctx, tx, change.entityID)
	if err != nil {
		return err
	}
	var row *nodeRow
	for _, candidate := range rows {
		if candidate.versionID == change.versionID {
			row = candidate
			break
		}
	}
	if row == nil {
		return nil
	}

	records := row.props[propKey]
	if len(records) == 0 {
		return nil
	}
	remaining := removeTarget(records[0].Value, refString(ref))

	content := nodeContent{
		displayName: row.displayName,
		props:       clonePropsWithout(row.props, propKey),
		facets:      row.facets,
		change:      row.change,
	}
	if len(remaining) > 0 {
		content.props[propKey] = []propRecord{{
			Value:    remaining,
			SourceID: records[0].SourceID,
			EventID:  eventID,
		}}
	}
	content.conflicts = content.props.conflicts()
	content.pointers = row.pointers

	if err := closeObserved(ctx, tx, "entity_versions", row.versionID, observedAt, eventID); err != nil {
		return err
	}
	seg := segment{
		start:       row.valid.Start,
		end:         endOf(row.valid),
		fromUnknown: row.fromUnknown,
		toUnknown:   row.toUnknown,
		boundary:    append(slices.Clone(row.producedBy), eventID),
	}
	return p.insertNodeVersion(ctx, tx, change.entityID, seg, content, eventID, observedAt)
}

func removeTarget(value any, target string) []any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && text == target {
			continue
		}
		out = append(out, item)
	}
	return out
}

func clonePropsWithout(props propSet, key string) propSet {
	out := make(propSet, len(props))
	for name, records := range props {
		if name == key {
			continue
		}
		out[name] = slices.Clone(records)
	}
	return out
}

// earliestTransition is the earliest valid lower bound among an alert's current versions that a
// transition wrote: when the graph first knew the alert to be in a state.
func (p *Projector) earliestTransition(ctx context.Context, tx pgx.Tx, entityID string) (time.Time, error) {
	var start time.Time
	err := tx.QueryRow(ctx, `
		SELECT min(lower(valid)) FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed) AND props ? $2`, entityID, AlertStateProp).Scan(&start)
	if err != nil {
		return time.Time{}, fmt.Errorf("projector: earliest transition of %s: %w", entityID, err)
	}
	return start.UTC(), nil
}
