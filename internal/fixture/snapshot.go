// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The structural snapshot (contracts/fixture-format.md verification step 3, research §4).
//
// Replay demands byte identity; shuffle demands only structural identity. A canonical entity id
// is the hash of the identity claim the log saw *first*, so delivering the same events in a
// different order inside a source's reordering window legitimately mints different ids for the
// same things. Comparing raw rows would therefore report a failure where the contract says
// there is none.
//
// Snapshot renders the current valid-time state keyed by *alias set* instead: every entity is
// named by the sorted set of identity claims that resolve to it, which is order-independent by
// construction. Two runs that agree on what is true — and disagree only on what things are
// called internally — produce the same text.
//
// Provenance is deliberately dropped from prop values: two orderings may credit the same value
// to two equally true events, and the fixture contract compares valid-time state, not which
// event happened to assert it first.

// snapshot renders the whole graph's current valid-time state as comparable text.
func snapshot(ctx context.Context, store *postgres.Store) (string, error) {
	names, err := entityNames(ctx, store)
	if err != nil {
		return "", err
	}

	nodes, err := snapshotNodes(ctx, store, names)
	if err != nil {
		return "", err
	}
	edges, err := snapshotEdges(ctx, store, names)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	for _, key := range sortedKeys(nodes) {
		lines := slices.Clone(nodes[key])
		slices.Sort(lines)
		out.WriteString("\nNODE " + key + "\n  " + strings.Join(lines, "\n  "))
	}
	for _, key := range sortedKeys(edges) {
		lines := slices.Clone(edges[key])
		slices.Sort(lines)
		out.WriteString("\nEDGE " + key + "\n  " + strings.Join(lines, "\n  "))
	}
	return out.String(), nil
}

// entityNames returns a function naming any entity id by the alias set of the entity it
// resolves to, following merge redirects (FR-039).
func entityNames(ctx context.Context, store *postgres.Store) (func(string) string, error) {
	redirect := map[string]string{}
	rows, err := store.Pool().Query(ctx, `SELECT entity_id, coalesce(merged_into, '') FROM graph.entities`)
	if err != nil {
		return nil, fmt.Errorf("fixture: read entities: %w", err)
	}
	for rows.Next() {
		var id, into string
		if err := rows.Scan(&id, &into); err != nil {
			rows.Close()
			return nil, fmt.Errorf("fixture: read entities: %w", err)
		}
		redirect[id] = into
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fixture: read entities: %w", err)
	}

	resolve := func(id string) string {
		// Bounded rather than cycle-detecting: a redirect cycle is a projector bug, and
		// stopping after a fixed number of hops reports it as a strange name instead of
		// hanging the verifier.
		for range 64 {
			next, ok := redirect[id]
			if !ok || next == "" {
				return id
			}
			id = next
		}
		return id
	}

	aliases := map[string][]string{}
	rows, err = store.Pool().Query(ctx, `SELECT entity_id, namespace, value FROM graph.identity_claims`)
	if err != nil {
		return nil, fmt.Errorf("fixture: read identity claims: %w", err)
	}
	for rows.Next() {
		var id, namespace, value string
		if err := rows.Scan(&id, &namespace, &value); err != nil {
			rows.Close()
			return nil, fmt.Errorf("fixture: read identity claims: %w", err)
		}
		key := resolve(id)
		aliases[key] = append(aliases[key], namespace+"="+value)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fixture: read identity claims: %w", err)
	}

	return func(id string) string {
		resolved := resolve(id)
		list := slices.Clone(aliases[resolved])
		slices.Sort(list)
		list = slices.Compact(list)
		if len(list) == 0 {
			// Nothing claimed this entity, so there is no order-free name for it. The
			// canonical id is used, which is stable for an entity no shuffle can rename.
			return "anonymous:" + resolved
		}
		return strings.Join(list, ",")
	}, nil
}

func snapshotNodes(ctx context.Context, store *postgres.Store, name func(string) string) (map[string][]string, error) {
	rows, err := store.Pool().Query(ctx, `
		SELECT entity_id, valid, valid_from_unknown, display_name, props, conflicts, facets,
		       coalesce(change::text, '')
		FROM graph.entity_versions WHERE upper_inf(observed)`)
	if err != nil {
		return nil, fmt.Errorf("fixture: read entity versions: %w", err)
	}
	defer rows.Close()

	nodes := map[string][]string{}
	for rows.Next() {
		var (
			id          string
			valid       postgres.TimeRange
			fromUnknown bool
			displayName string
			props       []byte
			conflicts   []string
			facets      []string
			change      string
		)
		if err := rows.Scan(&id, &valid, &fromUnknown, &displayName, &props, &conflicts, &facets, &change); err != nil {
			return nil, fmt.Errorf("fixture: read entity versions: %w", err)
		}
		values, err := propValues(props)
		if err != nil {
			return nil, err
		}
		key := name(id)
		nodes[key] = append(nodes[key], fmt.Sprintf(
			"valid=%s from_unknown=%v name=%q props=%s conflicts=%v facets=%v change=%s",
			valid, fromUnknown, displayName, values, conflicts, facets, change))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fixture: read entity versions: %w", err)
	}
	return nodes, nil
}

func snapshotEdges(ctx context.Context, store *postgres.Store, name func(string) string) (map[string][]string, error) {
	rows, err := store.Pool().Query(ctx, `
		SELECT src_id, dst_id, type, valid, weight_class, props,
		       coalesce(closed_as_consequence_of, '') <> ''
		FROM graph.edge_versions WHERE upper_inf(observed)`)
	if err != nil {
		return nil, fmt.Errorf("fixture: read edge versions: %w", err)
	}
	defer rows.Close()

	edges := map[string][]string{}
	for rows.Next() {
		var (
			src, dst, typ string
			valid         postgres.TimeRange
			weight        *int16
			props         []byte
			cascaded      bool
		)
		if err := rows.Scan(&src, &dst, &typ, &valid, &weight, &props, &cascaded); err != nil {
			return nil, fmt.Errorf("fixture: read edge versions: %w", err)
		}
		values, err := propValues(props)
		if err != nil {
			return nil, err
		}
		weightText := "none"
		if weight != nil {
			weightText = fmt.Sprint(*weight)
		}
		key := name(src) + " -" + typ + "-> " + name(dst)
		edges[key] = append(edges[key], fmt.Sprintf("valid=%s weight=%s props=%s cascaded=%v",
			valid, weightText, values, cascaded))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fixture: read edge versions: %w", err)
	}
	return edges, nil
}

// propValues reduces a stored props column to "who asserted what", dropping event ids.
func propValues(raw []byte) (string, error) {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", fmt.Errorf("fixture: decode props: %w", err)
	}
	type record struct {
		Value    json.RawMessage `json:"value"`
		SourceID string          `json:"source_id"`
	}
	parts := make([]string, 0, len(decoded))
	for _, key := range sortedKeys(decoded) {
		var records []record
		if err := json.Unmarshal(decoded[key], &records); err != nil {
			var single record
			if err := json.Unmarshal(decoded[key], &single); err != nil {
				return "", fmt.Errorf("fixture: decode prop %s: %w", key, err)
			}
			records = []record{single}
		}
		rendered := make([]string, 0, len(records))
		for _, r := range records {
			rendered = append(rendered, r.SourceID+":"+string(r.Value))
		}
		slices.Sort(rendered)
		parts = append(parts, key+"="+strings.Join(rendered, "|"))
	}
	return "{" + strings.Join(parts, " ") + "}", nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// countRows runs a scalar count query.
func countRows(ctx context.Context, store *postgres.Store, sql string) (int, error) {
	var count int
	if err := store.Pool().QueryRow(ctx, sql).Scan(&count); err != nil {
		return 0, fmt.Errorf("fixture: count (%s): %w", sql, err)
	}
	return count, nil
}
