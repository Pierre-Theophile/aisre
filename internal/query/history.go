// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Every version this node has ever had, and every decision taken about it (FR-032, US7).
//
// Every other read of the graph answers "what was true at T, as known at O" and therefore shows
// one version of a thing. This one answers the question underneath that: *what has the graph
// believed about this node, and when did it change its mind?* It is the query a correction is
// audited from, and the only one with no as-of instant at all — pinning observed time would hide
// exactly the rows it exists to show.
//
// # What is in the list
//
// Every row of `graph.entity_versions` the entity owns, superseded ones included. A correction
// never overwrites (constitution II): it closes the observed interval of the old version and
// opens a new one, so a node corrected once has two versions whose *valid* intervals may be
// identical and whose *observed* intervals abut. Reading the two side by side is how an operator
// sees that the graph learned something rather than that production moved.
//
// Versions of entities that were **merged away** are in the list too. A merge does not move the
// rows it absorbed; they keep their own `entity_id` and are closed in observed time (research
// §4). Dropping them would make the history of a merged entity start at the merge, which is the
// opposite of auditable.
//
// That is also how such a version is flagged. Every other query reports a version under the
// *canonical* entity id, so that a response is internally consistent with its edges; this one
// reports each version under **the id it is stored against**. A version whose `entity_id` is not
// the id the reference resolved to is an absorbed one — the `decisions` list in the same
// response says which decision absorbed it, and its bounded observed interval says when. No
// field of the published NodeVersion is repurposed to carry the flag, because the identity of
// the row already carries it honestly.
//
// # Decisions
//
// Alongside the versions, every resolution decision that names the entity or any id that
// resolves to it, in the order they were taken, loaded through the same reader the resolution
// audit uses (audit.go) so that "why does this node look like this?" and "why are these two the
// same thing?" can never give two different accounts of one decision.

// NodeHistory returns all versions of a node across observed time, and the resolution decisions
// about it (FR-032, FR-034).
func (e *Engine) NodeHistory(ctx context.Context, req *graphv1.NodeHistoryRequest) (*graphv1.NodeHistoryResponse, error) {
	focusRef := graph.RefFromProto(req.GetFocus())
	focusID, err := e.resolveFocus(ctx, focusRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}
	// Every id that resolves to the survivor: the survivor's own rows and the rows of
	// everything ever merged into it.
	ids := r.raw([]string{focusID})

	versions, err := e.loadAllVersions(ctx, ids)
	if err != nil {
		return nil, err
	}
	decisions, err := e.historyDecisions(ctx, ids)
	if err != nil {
		return nil, err
	}
	return &graphv1.NodeHistoryResponse{Versions: versions, Decisions: decisions}, nil
}

// loadAllVersions reads every stored version of the given entity ids, oldest knowledge first.
//
// The order is observed start, then valid start, then version id: a history is read down the
// page as the graph learned things, and within one moment of learning as production moved. The
// third key makes it total, so two runs produce the same bytes.
func (e *Engine) loadAllVersions(ctx context.Context, ids []string) ([]*graphv1.NodeVersion, error) {
	rows, err := e.store.Pool().Query(ctx, `
		SELECT v.entity_id, v.version_id, v.display_name, v.valid, v.observed,
		       v.valid_from_unknown, v.valid_to_unknown, v.props, v.conflicts, v.facets,
		       v.pointers, v.change, v.produced_by_event_ids
		FROM graph.entity_versions v
		WHERE v.entity_id = ANY($1)
		ORDER BY lower(v.observed), lower(v.valid), v.version_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("query: read node history: %w", err)
	}
	defer rows.Close()

	var (
		out       []*graphv1.NodeVersion
		eventIDs  []string
		byEntity  = map[string][]*graphv1.NodeVersion{}
		entityIDs []string
	)
	for rows.Next() {
		var (
			entityID, versionID string
			displayName         string
			validRange          postgres.TimeRange
			observedRange       postgres.TimeRange
			fromUnknown, toUnkn bool
			props               []byte
			conflicts           []string
			facets              []string
			pointers            []byte
			change              []byte
			producedBy          []string
		)
		if err := rows.Scan(&entityID, &versionID, &displayName, &validRange, &observedRange,
			&fromUnknown, &toUnkn, &props, &conflicts, &facets, &pointers, &change,
			&producedBy); err != nil {
			return nil, fmt.Errorf("query: scan node history: %w", err)
		}

		version := &graphv1.NodeVersion{
			EntityId:    entityID,
			VersionId:   versionID,
			DisplayName: displayName,
			Valid:       intervalOf(validRange, fromUnknown, toUnkn).Proto(),
			Observed:    intervalOf(observedRange, false, false).Proto(),
			Facets:      nodeTypesProto(facets),
		}
		decoded, err := decodeProps(props)
		if err != nil {
			return nil, fmt.Errorf("query: node %s: %w", versionID, err)
		}
		version.Props, version.Conflicts = decoded.structWithConflicts(conflicts)
		if version.Pointers, err = decodePointers(pointers); err != nil {
			return nil, fmt.Errorf("query: node %s: %w", versionID, err)
		}
		if version.Change, err = decodeChange(change); err != nil {
			return nil, fmt.Errorf("query: node %s: %w", versionID, err)
		}
		version.Provenance = &graphv1.Provenance{ProducedByEventIds: producedBy}
		eventIDs = append(eventIDs, producedBy...)

		if _, seen := byEntity[entityID]; !seen {
			entityIDs = append(entityIDs, entityID)
		}
		byEntity[entityID] = append(byEntity[entityID], version)
		out = append(out, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read node history: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}

	if err := e.attachHistoryTypes(ctx, entityIDs, byEntity); err != nil {
		return nil, err
	}
	if err := e.attachHistoryAliases(ctx, entityIDs, byEntity); err != nil {
		return nil, err
	}
	sources, err := e.eventSources(ctx, eventIDs)
	if err != nil {
		return nil, err
	}
	for _, version := range out {
		version.GetProvenance().SourceId = firstSource(version.GetProvenance().GetProducedByEventIds(), sources)
	}
	return out, nil
}

// attachHistoryTypes fills in each version's resolved type from graph.entities, per stored
// entity id: an absorbed entity keeps the type it was resolved to, which is part of what makes
// the merge readable after the fact.
func (e *Engine) attachHistoryTypes(ctx context.Context, ids []string, byEntity map[string][]*graphv1.NodeVersion) error {
	rows, err := e.store.Pool().Query(ctx,
		`SELECT entity_id, type FROM graph.entities WHERE entity_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("query: read entity types: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var entityID, typeName string
		if err := rows.Scan(&entityID, &typeName); err != nil {
			return fmt.Errorf("query: read entity types: %w", err)
		}
		for _, version := range byEntity[entityID] {
			version.Type = graph.NodeType(typeName).Proto()
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: read entity types: %w", err)
	}
	return nil
}

// attachHistoryAliases fills in the identifiers each stored entity is claimed under, sorted by
// namespace then value, exactly as a slice read does — except that they are attached per stored
// id rather than pooled onto the survivor, so a version of an absorbed entity shows the names it
// answered to.
func (e *Engine) attachHistoryAliases(ctx context.Context, ids []string, byEntity map[string][]*graphv1.NodeVersion) error {
	rows, err := e.store.Pool().Query(ctx, `
		SELECT DISTINCT entity_id, namespace, value
		FROM graph.identity_claims
		WHERE entity_id = ANY($1)
		ORDER BY namespace, value, entity_id`, ids)
	if err != nil {
		return fmt.Errorf("query: read aliases: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var entityID, namespace, value string
		if err := rows.Scan(&entityID, &namespace, &value); err != nil {
			return fmt.Errorf("query: read aliases: %w", err)
		}
		for _, version := range byEntity[entityID] {
			version.Aliases = append(version.Aliases,
				&graphv1.Ref{Namespace: namespace, Value: value})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: read aliases: %w", err)
	}
	// The database's ORDER BY is not the published order: it sorts under the *cluster's* collation,
	// and a golden that records it becomes a fact about somebody's locale. Under C, "TWIN-2026-1002"
	// precedes "inference-api@…" (T is 0x54, i is 0x69); under en_US or any ICU locale, case and
	// punctuation fold and the order reverses — so a corpus recorded on one machine fails on another.
	//
	// Sorting here makes the order a property of this code instead. Byte-wise, which is the order the
	// rest of the project already publishes for refs.
	sortAliases(byEntity)
	return nil
}

// sortAliases puts every version's aliases in byte order, so a golden does not encode the locale of
// the database it was recorded against.
func sortAliases(byEntity map[string][]*graphv1.NodeVersion) {
	for _, versions := range byEntity {
		for _, version := range versions {
			slices.SortFunc(version.GetAliases(), compareRefs)
		}
	}
}

// compareRefs orders two refs by namespace then value, byte-wise.
func compareRefs(a, b *graphv1.Ref) int {
	if c := strings.Compare(a.GetNamespace(), b.GetNamespace()); c != 0 {
		return c
	}
	return strings.Compare(a.GetValue(), b.GetValue())
}

// historyDecisions returns every resolution decision naming one of the entity's ids, oldest
// first. It reuses the audit's reader so that a decision is described identically wherever it is
// read (constitution VI).
func (e *Engine) historyDecisions(ctx context.Context, ids []string) ([]*graphv1.ResolutionDecision, error) {
	decisions, err := e.auditDecisions(ctx, auditNow())
	if err != nil {
		return nil, err
	}
	var out []*graphv1.ResolutionDecision
	for _, d := range decisions {
		if !slices.Contains(ids, d.survivingID) && !slices.Contains(ids, d.mergedID) {
			continue
		}
		out = append(out, d.proto())
	}
	return out, nil
}
