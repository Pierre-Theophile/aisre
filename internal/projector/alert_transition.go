// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Projecting an alert transition (ADR-0005 D2, ADR-0003 D10, ADR-0004 D4, 002 FR-008b).
//
// An alert is a node like any other and its state is a fact like any other: true from the
// instant of the transition until something contradicts it. That sentence is the whole design.
// It means an alert transition is *not* projected by hand here — it is turned into the
// `upsert_node` and `upsert_edge` assertions it already is, and handed to the machinery that
// has been segmenting facts since 001:
//
//   - the alert node gets a version from `transition_at`, carrying the state, the severity, the
//     title and the transport, and the next transition splits it exactly as a late-arriving
//     fact splits any other version (FR-021, segments.go);
//   - each `watches` ref gets a WATCHES edge from the alert, valid from the same instant, so
//     "what does this monitor watch, as of when it fired" is an ordinary as-of edge read.
//
// Doing it any other way would have meant a second, parallel implementation of versioning for
// one event type, and it would have got the hard cases wrong: two transitions arriving out of
// order, a monitor that fires again while the first version is still open, a replay that has to
// reproduce observed intervals. `nodeAssertions` and `edgeAssertions` read this body back
// through the same two functions used here, so a segment planned from an alert transition folds
// like a segment planned from an upsert.
//
// A human declaration is the same event, distinguished by `actor_kind = PERSON` and
// `transport = human_declared`, never by a separate path (002 FR-002a). The monitor ref is then
// the place the incident lives, on its stable identifier, and the "group" half of the
// idempotency key is empty (internal/log/alert.go).
//
// What is deliberately absent: flapping and no-data filtering, which belong to the feeder
// (log.SuppressAlertTransition), and any interpretation of the state vocabulary. A recovery is
// projected like any other transition, because a recovery is evidence.

// Props written on an ALERT node by a transition. Published: 002 reads them back.
const (
	AlertStateProp             = "sre.alert.state"
	AlertPreviousStateProp     = "sre.alert.previous_state"
	AlertGroupKeyProp          = "sre.alert.group_key"
	AlertSeverityProp          = "sre.alert.severity"
	AlertTransportProp         = "sre.alert.transport"
	AlertOriginRefProp         = "sre.alert.origin_ref"
	AlertActorKindProp         = "sre.alert.actor_kind"
	AlertDeclaringIdentityProp = "sre.alert.declaring_identity"
	AlertTransitionAtProp      = "sre.alert.transition_at"
	// AlertUnattachedWatchesProp lists the refs this alert watches that the graph has not
	// observed yet (003 FR-048). Its shape matches UnattachedTargetsProp so attach.go's
	// machinery is shared rather than duplicated.
	AlertUnattachedWatchesProp = "sre.alert.unattached_watches"
)

// The alert lane (005): a transition is folded in a lane of its own, beside its source's other
// assertions about the same node rather than in place of them.
//
// The fold takes, per source, the latest assertion at or before an instant (segments.go). Keyed on
// the source alone, a transition — which states only the alert's state — hid everything the same
// source had said about the monitor: from the transition onwards the ALERT node lost the service,
// environment and type its definition carries, although nothing had contradicted them. They are
// two different statements: what the monitor is, and what state it is in. So a transition is keyed
// on its source's alert lane: a later transition still replaces an earlier one (a state replaces a
// state), a re-asserted definition still replaces the previous definition, and the node carries both.
// Property records still name the real source, so provenance does not change.
const alertLaneSuffix = "\x00alert"

// alertLane is the fold key of a source's alert transitions.
func alertLane(sourceID string) string { return sourceID + alertLaneSuffix }

// laneSource is the source a fold key belongs to.
func laneSource(key string) string { return strings.TrimSuffix(key, alertLaneSuffix) }

func (p *Projector) applyAlertTransition(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope, body *graphv1.AlertTransition, observedAt time.Time) error {
	// Which watched refs the graph already knows is decided BEFORE the node is written, because the
	// answer goes into the node: the unresolved ones become its unattached list (FR-048).
	attached, unattached, err := p.splitWatches(ctx, tx, body)
	if err != nil {
		return err
	}

	if err := p.applyNodeAssertion(ctx, tx, env,
		alertNodeAssertionWithUnattached(body, unattached), observedAt, alertLane(env.GetSourceId())); err != nil {
		return err
	}

	// Only the resolvable ones get an edge now. An unresolved one would otherwise go through
	// applyUpsertEdge, which mints a placeholder endpoint — leaving an edge to an entity with no
	// type and no description, indistinguishable to an investigation from an alert watching
	// something real. It is attached by attach.go the moment the target appears, with the
	// transition event and the creating event both named on the edge.
	for _, watched := range attached {
		edge := AlertWatchAssertion(body, watched)
		if err := p.applyUpsertEdge(ctx, tx, env, edge, observedAt); err != nil {
			return err
		}
	}
	return nil
}

// splitWatches sorts a transition's watched refs into those the graph knows now and those it does not.
func (p *Projector) splitWatches(ctx context.Context, tx pgx.Tx, body *graphv1.AlertTransition) (attached, unattached []*graphv1.Ref, err error) {
	for _, watched := range body.GetWatches() {
		ref := graph.RefFromProto(watched)
		if ref.Namespace == "" || ref.Value == "" {
			continue
		}
		_, found, err := p.lookupRef(ctx, tx, ref)
		if err != nil {
			return nil, nil, err
		}
		if found {
			attached = append(attached, watched)
		} else {
			unattached = append(unattached, watched)
		}
	}
	return attached, unattached, nil
}

// storedAlertAssertion renders a transition read back out of the log, when a later assertion re-plans
// the alert's segments. It carries the unattached list the direct path would write now, so a
// re-planned segment and a freshly projected one are the same node whatever the arrival order. Without
// it, a re-planned segment lost the list while the latest one kept it, and which segments carried it
// depended on which transition arrived last: the first live Datadog campaign's shuffle step caught it
// (005 T082), on a monitor watching a service the graph never learns. A target that does appear later
// is removed from every waiting version by attach.go, so reading the graph's state now is consistent
// with that path too.
func (p *Projector) storedAlertAssertion(ctx context.Context, tx pgx.Tx, body *graphv1.AlertTransition) (*graphv1.UpsertNode, error) {
	_, unattached, err := p.splitWatches(ctx, tx, body)
	if err != nil {
		return nil, err
	}
	return alertNodeAssertionWithUnattached(body, unattached), nil
}

// alertNodeAssertionWithUnattached is AlertNodeAssertion plus the unattached-watches list.
//
// The list is written only when it is non-empty, so an alert whose targets all resolve produces
// exactly the node it did before this existed and no golden moves.
func alertNodeAssertionWithUnattached(body *graphv1.AlertTransition, unattached []*graphv1.Ref) *graphv1.UpsertNode {
	node := AlertNodeAssertion(body)
	if len(unattached) == 0 {
		return node
	}
	refs := make([]*structpb.Value, 0, len(unattached))
	for _, ref := range unattached {
		refs = append(refs, structpb.NewStringValue(graph.RefFromProto(ref).String()))
	}
	if node.Props == nil {
		node.Props = &structpb.Struct{Fields: map[string]*structpb.Value{}}
	}
	node.Props.Fields[AlertUnattachedWatchesProp] =
		structpb.NewListValue(&structpb.ListValue{Values: refs})
	return node
}

// AlertNodeAssertion renders an alert transition as the node assertion it is.
//
// It is exported and pure because it is used twice: once when the event is projected, and once
// when a later transition reads it back out of the log to plan its segments. Two independent
// renderings would be a correctness bug waiting for the second transition to arrive. Both paths add
// the unattached-watches list through alertNodeAssertionWithUnattached, for the same reason.
//
// Every prop is omitted when the source said nothing, so an alert whose transport reports only
// a state produces exactly one property and a golden that does not move when a richer connector
// starts filling the rest in.
func AlertNodeAssertion(body *graphv1.AlertTransition) *graphv1.UpsertNode {
	props := map[string]*structpb.Value{}
	put := func(key, value string) {
		if value != "" {
			props[key] = structpb.NewStringValue(value)
		}
	}
	put(AlertStateProp, body.GetToState())
	put(AlertPreviousStateProp, body.GetFromState())
	put(AlertGroupKeyProp, body.GetGroupKey())
	put(AlertSeverityProp, body.GetSeverity())
	put(AlertTransportProp, body.GetTransport())
	put(AlertOriginRefProp, body.GetOriginRef())
	put(AlertDeclaringIdentityProp, body.GetDeclaringIdentity())
	if kind := body.GetActorKind(); kind != graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED {
		// The same rule the change projection follows: a silent source leaves the field
		// unspecified and nothing at all is written (ADR-0005 D1, plan §I1).
		put(AlertActorKindProp, kind.String())
	}
	if ts := body.GetTransitionAt(); ts != nil {
		put(AlertTransitionAtProp, ts.AsTime().UTC().Format(time.RFC3339Nano))
	}

	return &graphv1.UpsertNode{
		Ref:         body.GetMonitor(),
		Type:        graphv1.NodeType_ALERT,
		DisplayName: body.GetTitle(),
		ValidAt:     body.GetTransitionAt(),
		Props:       &structpb.Struct{Fields: props},
	}
}

// AlertWatchAssertion renders one WATCHES edge of a transition. The direction is alert →
// watched entity, which is the way the edge type reads and the way an investigation walks it:
// from the thing that fired to the things it was watching.
func AlertWatchAssertion(body *graphv1.AlertTransition, watched *graphv1.Ref) *graphv1.UpsertEdge {
	return &graphv1.UpsertEdge{
		Src:     body.GetMonitor(),
		Dst:     watched,
		Type:    graphv1.EdgeType_WATCHES,
		ValidAt: body.GetTransitionAt(),
	}
}
