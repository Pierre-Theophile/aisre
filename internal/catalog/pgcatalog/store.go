// SPDX-License-Identifier: Apache-2.0

// Package pgcatalog is the Postgres adapter of the catalog's ports: catalog.Store reads the graph
// and catalog tables, and the projector is the catalog.EventLog the attributor appends to. It
// holds every SQL statement the attributor runs, and no rule.
package pgcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/event"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// NewAttributor builds an attributor over the projector's store and log.
func NewAttributor(p *projector.Projector, opts ...catalog.Option) *catalog.Attributor {
	return catalog.New(New(p.Store()), Events(p), opts...)
}

// NewNames builds a signal reader over store.
func NewNames(store *postgres.Store) *catalog.Names { return catalog.NewNames(New(store)) }

// Store implements catalog.Store over Postgres.
type Store struct {
	store *postgres.Store
}

var _ catalog.Store = (*Store)(nil)

// New reads through store.
func New(store *postgres.Store) *Store { return &Store{store: store} }

// events adapts the projector to catalog.EventLog.
type events struct{ p *projector.Projector }

// Events returns the projector as the attributor's event log.
func Events(p *projector.Projector) catalog.EventLog { return events{p: p} }

func (e events) RegisterSource(ctx context.Context, src event.Source) error {
	return e.p.RegisterSource(ctx, src)
}

func (e events) Apply(ctx context.Context, env *graphv1.EventEnvelope, observedAt time.Time) (*graphv1.IngestResult, error) {
	return e.p.Apply(ctx, env, observedAt)
}

func (e events) Page(ctx context.Context, cursor int64, limit int, fn func(int64, *graphv1.EventEnvelope) error) error {
	return e.p.Log().Page(ctx, cursor, limit, func(r *eventlog.Record) error { return fn(r.AppendedSeq, r.Envelope) })
}

// CurrentSignals implements catalog.Store. A change is an instant, never valid now, so every change
// the graph still believes in is current.
func (s *Store) CurrentSignals(ctx context.Context) ([]string, error) {
	rows, err := s.store.Pool().Query(ctx, `
		SELECT e.entity_id FROM graph.entities e
		WHERE e.merged_into IS NULL AND e.type = ANY($1)
		  AND EXISTS (SELECT 1 FROM graph.entity_versions v
		              WHERE v.entity_id = e.entity_id AND upper_inf(v.observed)
		                AND (upper_inf(v.valid) OR e.type = $2))
		ORDER BY e.entity_id`,
		[]string{string(graph.NodeTypeAlert), string(graph.NodeTypeKnowledgeDoc), string(graph.NodeTypeChange)},
		string(graph.NodeTypeChange))
	if err != nil {
		return nil, fmt.Errorf("catalog: list current signals: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("catalog: list current signals: %w", err)
	}
	return ids, nil
}

// StandingDecision implements catalog.Store.
func (s *Store) StandingDecision(ctx context.Context, signalID string) (string, []catalog.Attributed, []catalog.Attributed, error) {
	var (
		eventID              string
		entries, suggestions []byte
	)
	err := s.store.Pool().QueryRow(ctx, `
		SELECT event_id, entries, suggestions FROM graph.attribution_decisions
		WHERE signal_entity_id = $1 AND kind IN ('attribute', 'correct')
		  AND superseded_by_event_id IS NULL AND reverted_by_event_id IS NULL
		ORDER BY decided_at DESC, event_id DESC LIMIT 1`, signalID).Scan(&eventID, &entries, &suggestions)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil, nil, nil
	case err != nil:
		return "", nil, nil, fmt.Errorf("catalog: read the decision on %s: %w", signalID, err)
	}
	var out [2][]catalog.Attributed
	for i, raw := range [][]byte{entries, suggestions} {
		var items []struct {
			EntrySlug string  `json:"entry_slug"`
			Layer     string  `json:"layer"`
			RuleID    string  `json:"rule_id"`
			Score     float64 `json:"score"`
		}
		if err := json.Unmarshal(raw, &items); err != nil {
			return "", nil, nil, fmt.Errorf("catalog: decode the decision on %s: %w", signalID, err)
		}
		for _, item := range items {
			out[i] = append(out[i], catalog.Attributed{Slug: item.EntrySlug, Layer: item.Layer, RuleID: item.RuleID, Score: item.Score})
		}
	}
	return eventID, out[0], out[1], nil
}

// Correction implements catalog.Store.
func (s *Store) Correction(ctx context.Context, signalID string) ([]string, bool, error) {
	var entries []byte
	err := s.store.Pool().QueryRow(ctx, `
		SELECT entries FROM graph.attribution_decisions
		WHERE signal_entity_id = $1 AND kind = 'correct'
		  AND superseded_by_event_id IS NULL AND reverted_by_event_id IS NULL
		ORDER BY decided_at DESC, event_id DESC LIMIT 1`, signalID).Scan(&entries)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("catalog: read corrections of %s: %w", signalID, err)
	}
	var items []struct {
		EntrySlug string `json:"entry_slug"`
	}
	if err := json.Unmarshal(entries, &items); err != nil {
		return nil, false, fmt.Errorf("catalog: decode the correction of %s: %w", signalID, err)
	}
	slugs := []string{}
	for _, item := range items {
		slugs = append(slugs, item.EntrySlug)
	}
	return slugs, true, nil
}

// Aliases implements catalog.Store.
func (s *Store) Aliases(ctx context.Context, ruleEntityClaim string) ([]catalog.Alias, error) {
	rows, err := s.store.Pool().Query(ctx, `
		SELECT a.namespace, a.value, a.environment, a.entry_slug,
		       EXISTS (SELECT 1 FROM graph.attribution_decisions d
		               WHERE d.event_id = a.decided_by_event_id AND d.kind = 'correct'),
		       coalesce(a.rule_id, '') = $1
		FROM graph.catalog_alias_versions a WHERE upper_inf(a.observed)
		ORDER BY 1, 2, 3`, ruleEntityClaim)
	if err != nil {
		return nil, fmt.Errorf("catalog: read aliases: %w", err)
	}
	defer rows.Close()
	var out []catalog.Alias
	for rows.Next() {
		var alias catalog.Alias
		if err := rows.Scan(&alias.Name.Ref.Namespace, &alias.Name.Ref.Value, &alias.Name.Environment,
			&alias.Slug, &alias.ByCorrection, &alias.Discovered); err != nil {
			return nil, fmt.Errorf("catalog: scan alias: %w", err)
		}
		out = append(out, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: read aliases: %w", err)
	}
	return out, nil
}

// HiddenEntries implements catalog.Store.
func (s *Store) HiddenEntries(ctx context.Context) ([]string, error) {
	rows, err := s.store.Pool().Query(ctx, `
		SELECT c.value FROM graph.identity_claims c
		JOIN graph.entity_versions v ON v.entity_id = c.entity_id
		WHERE c.namespace = $1 AND upper_inf(v.observed) AND upper_inf(v.valid)
		  AND v.props->$2->>'value' = $3`,
		graph.CatalogNamespace, graph.CatalogStatusProp, graph.CatalogStatusHidden)
	if err != nil {
		return nil, fmt.Errorf("catalog: read hidden entries: %w", err)
	}
	hidden, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("catalog: read hidden entries: %w", err)
	}
	return hidden, nil
}

// Entries implements catalog.Store.
func (s *Store) Entries(ctx context.Context) ([]catalog.EntryRow, error) {
	rows, err := s.store.Pool().Query(ctx, `
		SELECT c.value, coalesce(v.display_name, ''), v.props
		FROM graph.identity_claims c
		JOIN graph.entities e ON e.entity_id = c.entity_id AND e.merged_into IS NULL
		JOIN LATERAL (
			SELECT display_name, props FROM graph.entity_versions
			WHERE entity_id = c.entity_id AND upper_inf(observed) AND upper_inf(valid)
			ORDER BY version_id DESC LIMIT 1
		) v ON true
		WHERE c.namespace = $1`, graph.CatalogNamespace)
	if err != nil {
		return nil, fmt.Errorf("catalog: read entries: %w", err)
	}
	defer rows.Close()
	var out []catalog.EntryRow
	for rows.Next() {
		var e catalog.EntryRow
		if err := rows.Scan(&e.Slug, &e.Display, &e.Props); err != nil {
			return nil, fmt.Errorf("catalog: scan entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EntryOwners implements catalog.Store.
func (s *Store) EntryOwners(ctx context.Context) (map[string][]string, error) {
	rows, err := s.store.Pool().Query(ctx, `
		WITH named AS (
			SELECT a.entry_slug, c.entity_id FROM graph.catalog_alias_versions a
			JOIN graph.identity_claims c ON c.namespace = a.namespace AND c.value = a.value
			WHERE upper_inf(a.observed) AND a.entry_slug <> $2
			UNION
			SELECT a.entry_slug, k.entity_id FROM graph.catalog_alias_versions a
			JOIN graph.correlation_keys k ON k.namespace = a.namespace AND k.value = a.value
			WHERE upper_inf(a.observed) AND a.entry_slug <> $2
		)
		SELECT DISTINCT named.entry_slug, coalesce(t.merged_into, o.dst_id)
		FROM named
		JOIN graph.entities m ON m.entity_id = named.entity_id
		JOIN graph.edge_versions o ON o.src_id = coalesce(m.merged_into, m.entity_id)
		LEFT JOIN graph.entities t ON t.entity_id = o.dst_id
		WHERE o.type = $1 AND upper_inf(o.observed) AND upper_inf(o.valid)
		ORDER BY 1, 2`, string(graph.EdgeTypeOwnedBy), graph.HiddenEntrySlug)
	if err != nil {
		return nil, fmt.Errorf("catalog: read entry owners: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var slug, owner string
		if err := rows.Scan(&slug, &owner); err != nil {
			return nil, fmt.Errorf("catalog: scan entry owner: %w", err)
		}
		out[slug] = append(out[slug], owner)
	}
	return out, rows.Err()
}

// Lookup implements catalog.Store.
func (s *Store) Lookup(ctx context.Context, ref graph.Ref) (string, bool, error) {
	var id string
	err := s.store.Pool().QueryRow(ctx, `
		SELECT entity_id FROM (
			SELECT entity_id, 0 AS rank, '' AS claim_id FROM graph.entities WHERE entity_id = $1
			UNION ALL
			SELECT entity_id, 1, claim_id FROM graph.identity_claims WHERE namespace = $2 AND value = $3
		) found ORDER BY rank, claim_id LIMIT 1`, ref.EntityID(), ref.Namespace, ref.Value).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("catalog: look up %s: %w", ref, err)
	}
	return id, true, nil
}

// LogEnd implements catalog.Store.
func (s *Store) LogEnd(ctx context.Context) (int64, error) {
	var seq int64
	err := s.store.Pool().QueryRow(ctx,
		`SELECT coalesce(max(appended_seq), 0) FROM log.events`).Scan(&seq)
	return seq, err
}

// AliasProposed implements catalog.Store.
func (s *Store) AliasProposed(ctx context.Context, key string) (bool, error) {
	var exists bool
	err := s.store.Pool().QueryRow(ctx, `SELECT true FROM graph.alias_proposals WHERE proposal_key = $1`, key).Scan(&exists)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("catalog: read alias proposal: %w", err)
	}
	return true, nil
}

// Services implements catalog.Store.
func (s *Store) Services(ctx context.Context) ([]catalog.EntityRow, error) {
	rows, err := s.store.Pool().Query(ctx, `
		SELECT e.entity_id, coalesce(v.display_name, '')
		FROM graph.entities e
		JOIN LATERAL (
			SELECT display_name FROM graph.entity_versions
			WHERE entity_id = e.entity_id AND upper_inf(observed) AND upper_inf(valid)
			ORDER BY version_id DESC LIMIT 1
		) v ON true
		WHERE e.merged_into IS NULL AND e.type = $1
		  AND NOT EXISTS (SELECT 1 FROM graph.identity_claims c WHERE c.entity_id = e.entity_id AND c.namespace = $2)
		ORDER BY e.entity_id`, string(graph.NodeTypeService), graph.CatalogNamespace)
	if err != nil {
		return nil, fmt.Errorf("catalog: list services: %w", err)
	}
	defer rows.Close()
	var out []catalog.EntityRow
	for rows.Next() {
		var row catalog.EntityRow
		if err := rows.Scan(&row.ID, &row.Display); err != nil {
			return nil, fmt.Errorf("catalog: scan service: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: list services: %w", err)
	}
	return out, nil
}

// EntityNames implements catalog.Store.
func (s *Store) EntityNames(ctx context.Context, entityID string) ([]graph.Ref, []catalog.Name, error) {
	rows, err := s.store.Pool().Query(ctx, `
		SELECT 'claim', namespace, value, coalesce(attributes->>$2, '') FROM graph.identity_claims
		WHERE entity_id IN (SELECT entity_id FROM graph.entities WHERE entity_id = $1 OR merged_into = $1)
		UNION
		SELECT 'key', namespace, value, coalesce(attributes->>$2, '') FROM graph.correlation_keys
		WHERE entity_id IN (SELECT entity_id FROM graph.entities WHERE entity_id = $1 OR merged_into = $1)
		ORDER BY 1, 2, 3, 4`, entityID, catalog.AttrEnvironment)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: read names of %s: %w", entityID, err)
	}
	defer rows.Close()
	var (
		claims []graph.Ref
		names  []catalog.Name
	)
	for rows.Next() {
		var kind string
		var n catalog.Name
		if err := rows.Scan(&kind, &n.Ref.Namespace, &n.Ref.Value, &n.Environment); err != nil {
			return nil, nil, fmt.Errorf("catalog: scan name: %w", err)
		}
		names = append(names, n)
		if kind == "claim" && !slices.Contains(claims, n.Ref) {
			claims = append(claims, n.Ref)
		}
	}
	return claims, names, rows.Err()
}

// DocumentProps implements catalog.Store.
func (s *Store) DocumentProps(ctx context.Context) ([][]byte, error) {
	rows, err := s.store.Pool().Query(ctx, `
		SELECT v.props FROM graph.entities e
		JOIN graph.entity_versions v ON v.entity_id = e.entity_id
		WHERE e.merged_into IS NULL AND e.type = $1 AND upper_inf(v.observed) AND upper_inf(v.valid)
		ORDER BY e.entity_id, v.version_id`, string(graph.NodeTypeKnowledgeDoc))
	if err != nil {
		return nil, fmt.Errorf("catalog: list documents: %w", err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var props []byte
		if err := rows.Scan(&props); err != nil {
			return nil, fmt.Errorf("catalog: scan document: %w", err)
		}
		out = append(out, props)
	}
	return out, rows.Err()
}

// ResolveEntity implements catalog.Store.
func (s *Store) ResolveEntity(ctx context.Context, entityID string) (string, graph.NodeType, graph.Ref, bool, error) {
	var (
		canonical, typ string
		ref            graph.Ref
	)
	err := s.store.Pool().QueryRow(ctx, `
		WITH RECURSIVE chain(id, depth) AS (
			SELECT $1::text, 0
			UNION ALL
			SELECT e.merged_into, c.depth + 1 FROM graph.entities e JOIN chain c ON e.entity_id = c.id
			WHERE e.merged_into IS NOT NULL AND c.depth < 64
		)
		SELECT e.entity_id, e.type, coalesce(c.namespace, ''), coalesce(c.value, '')
		FROM chain JOIN graph.entities e ON e.entity_id = chain.id AND e.merged_into IS NULL
		LEFT JOIN LATERAL (
			SELECT namespace, value FROM graph.identity_claims
			WHERE entity_id = e.entity_id ORDER BY observed_at, claim_id LIMIT 1
		) c ON true
		LIMIT 1`, entityID).Scan(&canonical, &typ, &ref.Namespace, &ref.Value)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", "", graph.Ref{}, false, nil
	case err != nil:
		return "", "", graph.Ref{}, false, fmt.Errorf("catalog: read entity %s: %w", entityID, err)
	}
	return canonical, graph.NodeType(typ), ref, true, nil
}

// LatestVersion implements catalog.Store.
func (s *Store) LatestVersion(ctx context.Context, entityID string) (string, []byte, error) {
	var (
		display string
		props   []byte
	)
	err := s.store.Pool().QueryRow(ctx, `
		SELECT display_name, props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)
		ORDER BY upper_inf(valid) DESC, lower(valid) DESC, version_id LIMIT 1`, entityID).Scan(&display, &props)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, fmt.Errorf("catalog: read %s: %w", entityID, err)
	}
	return display, props, nil
}

// Targets implements catalog.Store: every identity claim and correlation key of each entity the
// signal's edges reach, with its environment.
func (s *Store) Targets(ctx context.Context, signalID string, typ graph.EdgeType, inbound bool) ([]catalog.Name, error) {
	near, far := ends(inbound)
	rows, err := s.store.Pool().Query(ctx, fmt.Sprintf(`
		WITH ends AS (
			SELECT DISTINCT coalesce(t.merged_into, e.%[2]s) AS id
			FROM graph.edge_versions e
			LEFT JOIN graph.entities t ON t.entity_id = e.%[2]s
			WHERE e.%[1]s = $1 AND e.type = $2 AND upper_inf(e.observed) AND (upper_inf(e.valid) OR $4)
		)
		SELECT c.namespace, c.value, coalesce(c.attributes->>$3, '')
		FROM ends JOIN graph.entities m ON m.entity_id = ends.id OR m.merged_into = ends.id
		JOIN graph.identity_claims c ON c.entity_id = m.entity_id
		UNION
		SELECT k.namespace, k.value, coalesce(k.attributes->>$3, '')
		FROM ends JOIN graph.entities m ON m.entity_id = ends.id OR m.merged_into = ends.id
		JOIN graph.correlation_keys k ON k.entity_id = m.entity_id
		ORDER BY 1, 2, 3`, near, far), signalID, string(typ), catalog.AttrEnvironment, inbound)
	if err != nil {
		return nil, fmt.Errorf("catalog: read %s targets of %s: %w", typ, signalID, err)
	}
	defer rows.Close()
	var out []catalog.Name
	for rows.Next() {
		var name catalog.Name
		if err := rows.Scan(&name.Ref.Namespace, &name.Ref.Value, &name.Environment); err != nil {
			return nil, fmt.Errorf("catalog: scan target name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Neighbourhood implements catalog.Store: the names of the entities the targets run on, depend on
// or call, and the owners they are owned by, through edges valid and believed now.
func (s *Store) Neighbourhood(ctx context.Context, signalID string, typ graph.EdgeType, inbound bool) ([]catalog.Name, []string, error) {
	near, far := ends(inbound)
	rows, err := s.store.Pool().Query(ctx, fmt.Sprintf(`
		WITH ends AS (
			SELECT DISTINCT coalesce(t.merged_into, e.%[2]s) AS id
			FROM graph.edge_versions e
			LEFT JOIN graph.entities t ON t.entity_id = e.%[2]s
			WHERE e.%[1]s = $1 AND e.type = $2 AND upper_inf(e.observed) AND (upper_inf(e.valid) OR $5)
		), next AS (
			SELECT DISTINCT o.type, coalesce(t.merged_into, o.dst_id) AS id
			FROM ends JOIN graph.edge_versions o ON o.src_id = ends.id
			LEFT JOIN graph.entities t ON t.entity_id = o.dst_id
			WHERE o.type = ANY($3) AND upper_inf(o.observed) AND upper_inf(o.valid)
		)
		SELECT 'owner', '', next.id, '' FROM next WHERE next.type = $4
		UNION
		SELECT 'name', c.namespace, c.value, coalesce(c.attributes->>$6, '')
		FROM next JOIN graph.entities m ON m.entity_id = next.id OR m.merged_into = next.id
		JOIN graph.identity_claims c ON c.entity_id = m.entity_id
		WHERE next.type <> $4
		UNION
		SELECT 'name', k.namespace, k.value, coalesce(k.attributes->>$6, '')
		FROM next JOIN graph.entities m ON m.entity_id = next.id OR m.merged_into = next.id
		JOIN graph.correlation_keys k ON k.entity_id = m.entity_id
		WHERE next.type <> $4
		ORDER BY 1, 2, 3, 4`, near, far), signalID, string(typ),
		[]string{string(graph.EdgeTypeRunsOn), string(graph.EdgeTypeDependsOn), string(graph.EdgeTypeCalls), string(graph.EdgeTypeOwnedBy)},
		string(graph.EdgeTypeOwnedBy), inbound, catalog.AttrEnvironment)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: read the neighbourhood of %s: %w", signalID, err)
	}
	defer rows.Close()
	var (
		related []catalog.Name
		owners  []string
	)
	for rows.Next() {
		var kind string
		var name catalog.Name
		if err := rows.Scan(&kind, &name.Ref.Namespace, &name.Ref.Value, &name.Environment); err != nil {
			return nil, nil, fmt.Errorf("catalog: scan neighbour: %w", err)
		}
		if kind == "owner" {
			owners = append(owners, name.Ref.Value)
		} else {
			related = append(related, name)
		}
	}
	return related, owners, rows.Err()
}

// ends names the edge columns on the signal's side and on the far side.
func ends(inbound bool) (near, far string) {
	if inbound {
		return "dst_id", "src_id"
	}
	return "src_id", "dst_id"
}
