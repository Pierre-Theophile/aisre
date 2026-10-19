// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Catalog entries (feature 009, ADR-0015 item 2, research R1).
//
// An entry is a SERVICE node whose ref is `sre.catalog=<slug>`, so edges can point at it and every
// as-of read of the graph answers "what was the catalog at T" without a second store. It is written
// only by two events, and both are rendered here as the node assertion they amount to:
//
//	propose_catalog_entry  the attributor found a service no alias names (R9). Status `proposed`.
//	                       A proposal for an entry that already exists changes nothing: the entry's
//	                       state is a person's to decide once it exists.
//	declare_catalog_entry  a person states the entry's whole state: create, confirm, rename, hide,
//	                       restore. Principal required (checkResolvable).
//
// Both fold under ONE lane, catalogLane, and not under their sources. The node fold keeps the latest
// assertion per lane, so with one lane the latest decision about an entry IS its current version,
// which is the state machine data-model §Catalog entry describes. Under per-source lanes a proposal
// (source `sre.attributor`) and a confirmation (source `human`) would both be current and the status
// would read as a conflict between them.

// catalogLane is the fold key every entry event shares, and the source its props are recorded under.
const catalogLane = graph.CatalogNamespace

// aliasRuleEntityClaim is the rule id recorded on an alias carried by a proposed entry from its own
// entity's claims (research R9): 001's certain rules already made those claims one entity's names,
// so carrying them over is not a new inference.
const aliasRuleEntityClaim = "D1"

func (p *Projector) applyProposeCatalogEntry(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.ProposeCatalogEntry, observedAt time.Time,
) error {
	if _, found, err := p.lookupRef(ctx, tx, graph.CatalogRef(body.GetSlug())); err != nil {
		return err
	} else if found {
		return nil
	}
	if err := p.applyNodeAssertion(ctx, tx, env, proposedEntryAssertion(body), observedAt, catalogLane); err != nil {
		return err
	}
	for _, name := range body.GetAliases() {
		if err := p.openAlias(ctx, tx, aliasRow{
			name:      graph.RefFromProto(name),
			entrySlug: body.GetSlug(),
			ruleID:    aliasRuleEntityClaim,
			eventID:   env.GetEventId(),
		}, observedAt, false); err != nil {
			return err
		}
	}
	return nil
}

func (p *Projector) applyDeclareCatalogEntry(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.DeclareCatalogEntry, observedAt time.Time,
) error {
	return p.applyNodeAssertion(ctx, tx, env, declaredEntryAssertion(body), observedAt, catalogLane)
}

// proposedEntryAssertion is the node a proposal asserts.
func proposedEntryAssertion(body *graphv1.ProposeCatalogEntry) *graphv1.UpsertNode {
	return entryAssertion(body.GetSlug(), body.GetDisplayName(), map[string]string{
		graph.CatalogKindProp:   graph.CatalogKindService,
		graph.CatalogStatusProp: graph.CatalogStatusProposed,
	})
}

// declaredEntryAssertion is the node a declaration asserts: the entry's whole state, so an owner or
// a description left empty is cleared rather than kept from an earlier version.
func declaredEntryAssertion(body *graphv1.DeclareCatalogEntry) *graphv1.UpsertNode {
	props := map[string]string{
		graph.CatalogKindProp:   body.GetKind(),
		graph.CatalogStatusProp: body.GetStatus(),
	}
	if body.GetOwner() != "" {
		props[graph.CatalogOwnerProp] = body.GetOwner()
	}
	if body.GetDescription() != "" {
		props[graph.CatalogDescriptionProp] = body.GetDescription()
	}
	return entryAssertion(body.GetSlug(), body.GetDisplayName(), props)
}

func entryAssertion(slug, displayName string, props map[string]string) *graphv1.UpsertNode {
	fields := make(map[string]*structpb.Value, len(props))
	for key, value := range props {
		fields[key] = structpb.NewStringValue(value)
	}
	return &graphv1.UpsertNode{
		Ref:         graph.CatalogRef(slug).Proto(),
		Type:        graphv1.NodeType_SERVICE,
		DisplayName: displayName,
		Props:       &structpb.Struct{Fields: fields},
	}
}

// catalogEntryAssertion decodes an entry event read back from the log as its node assertion, for
// nodeAssertions.
func catalogEntryAssertion(eventType string, payload []byte) (*graphv1.UpsertNode, error) {
	switch eventType {
	case "propose_catalog_entry":
		body := &graphv1.ProposeCatalogEntry{}
		if err := protojson.Unmarshal(payload, body); err != nil {
			return nil, fmt.Errorf("projector: decode catalog entry proposal: %w", err)
		}
		return proposedEntryAssertion(body), nil
	case "declare_catalog_entry":
		body := &graphv1.DeclareCatalogEntry{}
		if err := protojson.Unmarshal(payload, body); err != nil {
			return nil, fmt.Errorf("projector: decode catalog entry declaration: %w", err)
		}
		return declaredEntryAssertion(body), nil
	}
	return nil, fmt.Errorf("projector: %s is not a catalog entry event", eventType)
}
