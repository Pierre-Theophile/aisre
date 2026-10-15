// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The knowledge corpus read (007 FR-006–FR-008, research R1–R3, ADR-0013 item 3).
//
// The knowledge worker ranks; this read scopes. It answers one question — "which durable
// documents and which reviewed past investigations are linked to these entities, as of these two
// instants?" — and nothing else, so that the scoping the constitution requires ("the graph is the
// RAG index") lives in one query rather than in a walk a caller could get wrong.
//
// Two kinds of candidate, read two ways:
//
//   - A KNOWLEDGE_DOC is a candidate while its CONCERNS edge to a requested entity is valid at the
//     valid instant and known at the observed instant. The document's own version is read at the
//     same two instants, so its title and summary are the ones that held then.
//   - A reviewed INVESTIGATION is a candidate when its INVESTIGATED edge to a requested entity
//     *started* before the valid instant. The projector writes that edge over the run's own
//     interval, which is right for "what was being investigated then" and would make every past
//     run invisible to "what was known then"; reading the edge's start is how this read asks the
//     second question without a derived edge. Its version must carry a review label known at the
//     observed instant: 002 FR-052 asks for *reviewed* investigations, and an unreviewed verdict
//     is the engine quoting itself.
//
// Both are bounded by the observed instant, so a replay retrieves exactly what the original run
// could have retrieved (007 FR-007).

// InvestigationLabelProp is the prop a review label writes on an INVESTIGATION version
// (internal/projector.InvestigationLabelProp). It is repeated here because query does not import
// the projector; a test pins the two together.
const InvestigationLabelProp = "sre.investigation.was_this_right"

// KnowledgeCandidate is one document or reviewed investigation linked to a requested entity.
type KnowledgeCandidate struct {
	// EntityID is the candidate's canonical entity id.
	EntityID string
	// Type is graph.NodeTypeKnowledgeDoc or graph.NodeTypeInvestigation.
	Type graph.NodeType
	// Ref is the name a person can cite: the smallest identity claim on the entity, as
	// `namespace=value` (`notion.page=…`, `sre.investigation.id=…`), else the entity id.
	Ref string
	// DisplayName is the version's display name: the document's title.
	DisplayName string
	// Props are the version's props, provenance stripped: the first record's value per key.
	Props map[string]any
	// Linked are the requested references this candidate is linked to, spelled as requested.
	Linked []string
	// Elsewhere are the citable references of the other entities it is linked to (008): set by
	// KnowledgeDocuments, empty from KnowledgeCandidates.
	Elsewhere []string
	// LinkedSince is the earliest valid start among its links.
	LinkedSince time.Time
}

// KnowledgeCandidates returns the candidates linked to any of refs, as of at: KnowledgeDocuments
// filtered to those a requested reference names. A reference that names nothing in the graph
// contributes nothing; it is not an error, because "nothing is known about this" is an answer the
// worker reports as NO_DATA.
//
// at.ValidAt is required. A zero at.ObservedAt reads current knowledge.
func (e *Engine) KnowledgeCandidates(ctx context.Context, refs []string, at AsOf) ([]KnowledgeCandidate, error) {
	all, err := e.KnowledgeDocuments(ctx, refs, at)
	if err != nil {
		return nil, err
	}
	var out []KnowledgeCandidate
	for _, c := range all {
		if len(c.Linked) == 0 {
			continue
		}
		c.Elsewhere = nil
		out = append(out, c)
	}
	return out, nil
}

// KnowledgeDocuments returns every document and reviewed investigation linked to anything, as of
// at (008 research R3). Scope is no longer a filter: a postmortem about another service with the
// symptoms an investigation sees is still a candidate, ranked lower by the worker. Each candidate's
// Linked holds the references in refs it is linked to, spelled as requested, and Elsewhere the
// citable references of every other entity it is linked to. A candidate with a non-empty Linked is
// in scope.
//
// at.ValidAt is required. A zero at.ObservedAt reads current knowledge.
func (e *Engine) KnowledgeDocuments(ctx context.Context, refs []string, at AsOf) ([]KnowledgeCandidate, error) {
	if at.ValidAt.IsZero() {
		return nil, errors.New("query: a knowledge read needs a valid instant; retrieval is as of one")
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}

	// requested maps a canonical entity id to the references that named it.
	requested := map[string][]string{}
	for _, spelled := range refs {
		ref, err := graph.ParseRef(spelled)
		if err != nil {
			return nil, fmt.Errorf("query: knowledge read: %w", err)
		}
		id, err := e.resolveFocus(ctx, ref)
		if errors.Is(err, ErrFocusNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !slices.Contains(requested[id], spelled) {
			requested[id] = append(requested[id], spelled)
		}
	}

	links, err := e.knowledgeLinks(ctx, r, nil, at)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, nil
	}

	byCandidate := map[string]*KnowledgeCandidate{}
	elsewhere := map[string][]string{} // candidate -> canonical ids of unrequested linked entities
	var order []string
	for _, link := range links {
		c, ok := byCandidate[link.src]
		if !ok {
			c = &KnowledgeCandidate{EntityID: link.src, Type: link.typ, LinkedSince: link.since}
			byCandidate[link.src] = c
			order = append(order, link.src)
		}
		if spellings, ok := requested[link.dst]; ok {
			for _, spelled := range spellings {
				if !slices.Contains(c.Linked, spelled) {
					c.Linked = append(c.Linked, spelled)
				}
			}
		} else if !slices.Contains(elsewhere[link.src], link.dst) {
			elsewhere[link.src] = append(elsewhere[link.src], link.dst)
		}
		if link.since.Before(c.LinkedSince) {
			c.LinkedSince = link.since
		}
	}
	sort.Strings(order)

	versions, err := e.knowledgeVersions(ctx, r, order, at)
	if err != nil {
		return nil, err
	}
	nameable := append([]string(nil), order...)
	for _, ids := range elsewhere {
		nameable = append(nameable, ids...)
	}
	names, err := e.citableRefs(ctx, nameable)
	if err != nil {
		return nil, err
	}
	name := func(id string) string {
		if n := names[id]; n != "" {
			return n
		}
		return IDNamespace + "=" + id
	}

	out := make([]KnowledgeCandidate, 0, len(order))
	for _, id := range order {
		c := byCandidate[id]
		v, ok := versions[id]
		if !ok {
			// Linked, but no version of the node holds at these instants: an edge that outlived
			// its node's description. There is nothing to cite.
			continue
		}
		if c.Type == graph.NodeTypeInvestigation {
			if _, reviewed := v.props[InvestigationLabelProp]; !reviewed {
				continue
			}
		}
		c.DisplayName = v.displayName
		c.Props = v.props
		c.Ref = name(id)
		for _, other := range elsewhere[id] {
			if spelled := name(other); !slices.Contains(c.Elsewhere, spelled) {
				c.Elsewhere = append(c.Elsewhere, spelled)
			}
		}
		sort.Strings(c.Linked)
		sort.Strings(c.Elsewhere)
		out = append(out, *c)
	}
	return out, nil
}

type knowledgeLink struct {
	src, dst string
	typ      graph.NodeType
	since    time.Time
}

// knowledgeLinks reads the CONCERNS edges from documents valid at the instant, and the
// INVESTIGATED edges from investigations that started before it, into the target ids, or into
// anything when targets is nil.
func (e *Engine) knowledgeLinks(ctx context.Context, r *redirects, targets []string, at AsOf) ([]knowledgeLink, error) {
	args := []any{targets, at.ValidAt.UTC(),
		string(graph.EdgeTypeConcerns), string(graph.NodeTypeKnowledgeDoc),
		string(graph.EdgeTypeInvestigated), string(graph.NodeTypeInvestigation)}
	// A nil targets reads every link (008 research R3); the parameter stays bound either way so
	// the statement keeps one shape.
	destination := "ev.dst_id = ANY($1)"
	if targets == nil {
		destination = "($1::text[] IS NULL)"
	}
	observed, extra := at.observedPredicate("ev.observed", len(args)+1)
	args = append(args, extra...)

	sql := fmt.Sprintf(`
		SELECT ev.src_id, ev.dst_id, en.type, lower(ev.valid)
		FROM graph.edge_versions ev
		JOIN graph.entities en ON en.entity_id = ev.src_id
		WHERE %s AND %s
		  AND (
		        (ev.type = $3 AND en.type = $4 AND ev.valid @> $2::timestamptz)
		     OR (ev.type = $5 AND en.type = $6 AND lower(ev.valid) < $2::timestamptz)
		  )
		ORDER BY ev.src_id, ev.dst_id, lower(ev.valid)`, destination, observed)

	rows, err := e.store.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: read knowledge links: %w", err)
	}
	defer rows.Close()

	var out []knowledgeLink
	for rows.Next() {
		var (
			link  knowledgeLink
			typ   string
			since *time.Time
		)
		if err := rows.Scan(&link.src, &link.dst, &typ, &since); err != nil {
			return nil, fmt.Errorf("query: scan knowledge links: %w", err)
		}
		link.src = r.canonical(link.src)
		link.dst = r.canonical(link.dst)
		link.typ = graph.NodeType(typ)
		if since != nil {
			link.since = since.UTC()
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read knowledge links: %w", err)
	}
	return out, nil
}

type knowledgeVersion struct {
	displayName string
	props       map[string]any
}

// knowledgeVersions reads each candidate's version: for a document the one valid at the instant,
// for an investigation the latest that began before it. Both known at the observed instant.
func (e *Engine) knowledgeVersions(ctx context.Context, r *redirects, ids []string, at AsOf) (map[string]knowledgeVersion, error) {
	args := []any{r.raw(ids), at.ValidAt.UTC()}
	observed, extra := at.observedPredicate("observed", len(args)+1)
	args = append(args, extra...)

	sql := fmt.Sprintf(`
		SELECT DISTINCT ON (entity_id) entity_id, display_name, props
		FROM graph.entity_versions
		WHERE entity_id = ANY($1) AND lower(valid) <= $2::timestamptz AND %s
		ORDER BY entity_id, lower(valid) DESC, version_id`, observed)

	rows, err := e.store.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: read knowledge versions: %w", err)
	}
	defer rows.Close()

	out := map[string]knowledgeVersion{}
	for rows.Next() {
		var (
			id, name string
			raw      []byte
		)
		if err := rows.Scan(&id, &name, &raw); err != nil {
			return nil, fmt.Errorf("query: scan knowledge versions: %w", err)
		}
		decoded, err := decodeProps(raw)
		if err != nil {
			return nil, fmt.Errorf("query: knowledge version of %s: %w", id, err)
		}
		props := make(map[string]any, len(decoded))
		for key, records := range decoded {
			if len(records) > 0 {
				props[key] = records[0].Value
			}
		}
		out[r.canonical(id)] = knowledgeVersion{displayName: name, props: props}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read knowledge versions: %w", err)
	}
	return out, nil
}

// citableRefs returns, per entity, the smallest identity claim made on it.
func (e *Engine) citableRefs(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := e.store.Pool().Query(ctx, `
		SELECT DISTINCT ON (entity_id) entity_id, namespace, value
		FROM graph.identity_claims
		WHERE entity_id = ANY($1)
		ORDER BY entity_id, namespace, value`, ids)
	if err != nil {
		return nil, fmt.Errorf("query: read knowledge refs: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, namespace, value string
		if err := rows.Scan(&id, &namespace, &value); err != nil {
			return nil, fmt.Errorf("query: scan knowledge refs: %w", err)
		}
		out[id] = graph.Ref{Namespace: namespace, Value: value}.String()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read knowledge refs: %w", err)
	}
	return out, nil
}
