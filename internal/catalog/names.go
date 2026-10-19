// SPDX-License-Identifier: Apache-2.0

// Package catalog is the service catalog's logic (feature 009): what a signal's names are, the
// published decision model that attributes it to catalog entries, the attributor that runs it in
// `serve`, and the catalog file import. It is neither a connector nor resolution: it reads what both
// produce and decides something different (plan §Structure Decision).
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Signal kinds (research R3).
const (
	KindAlert    = "alert"
	KindDeclared = "declared"
	KindDocument = "document"
	KindChange   = "change"
)

// AttrEnvironment is the claim attribute a name's environment is read from (005, rule C9).
const AttrEnvironment = "deployment.environment.name"

// Name is one name a signal goes by: a ref, and the environment it was stated in, if any.
type Name struct {
	Ref         graph.Ref
	Environment string
}

func (n Name) String() string {
	if n.Environment == "" {
		return n.Ref.String()
	}
	return n.Ref.String() + "@" + n.Environment
}

// Signal is what the decision model reads about one alert, declaration, document or change.
type Signal struct {
	// EntityID is the signal's canonical entity id.
	EntityID string
	// Ref is a ref that resolves to it, for the event the attributor appends.
	Ref graph.Ref
	// Kind is one of the four kinds.
	Kind string
	// Names are what the signal goes by, sorted and deduplicated.
	Names []Name
	// Text is the signal's title, for the optional classifier (PR 3) and nothing else.
	Text string
	// Related are the names of the entities a watched or target entity runs on, depends on or
	// calls (rule P3), sorted and deduplicated.
	Related []Name
	// Owners are the entity ids a watched or target entity is owned by (rule P4), sorted.
	Owners []string
}

// signalKinds are the node types a signal is.
var signalKinds = map[graph.NodeType]string{
	graph.NodeTypeAlert:        KindAlert,
	graph.NodeTypeKnowledgeDoc: KindDocument,
	graph.NodeTypeChange:       KindChange,
}

// IsSignalType reports whether a node type is one the attributor attributes.
func IsSignalType(t graph.NodeType) bool {
	_, ok := signalKinds[t]
	return ok
}

// Names reads signals out of the graph as it is now (research R3). In `serve` "now" is the
// signal's observed instant, which is what the decision is about; the log records the decision, so
// nothing here runs during a replay.
type Names struct {
	store *postgres.Store
}

// NewNames reads through store.
func NewNames(store *postgres.Store) *Names { return &Names{store: store} }

// ErrNotSignal is returned for an entity that is not an alert, a document or a change.
var ErrNotSignal = errors.New("catalog: not a signal")

// Signal reads one signal by entity id, following merges.
func (n *Names) Signal(ctx context.Context, entityID string) (Signal, error) {
	var (
		canonical, typ string
		refNS, refVal  string
	)
	err := n.store.Pool().QueryRow(ctx, `
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
		LIMIT 1`, entityID).Scan(&canonical, &typ, &refNS, &refVal)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Signal{}, fmt.Errorf("catalog: entity %s: %w", entityID, ErrNotSignal)
	case err != nil:
		return Signal{}, fmt.Errorf("catalog: read entity %s: %w", entityID, err)
	}
	kind, ok := signalKinds[graph.NodeType(typ)]
	if !ok {
		return Signal{}, fmt.Errorf("catalog: entity %s is a %s: %w", entityID, typ, ErrNotSignal)
	}
	signal := Signal{EntityID: canonical, Ref: graph.Ref{Namespace: refNS, Value: refVal}, Kind: kind}

	var (
		display string
		props   []byte
	)
	err = n.store.Pool().QueryRow(ctx, `
		SELECT display_name, props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)
		ORDER BY upper_inf(valid) DESC, lower(valid) DESC, version_id LIMIT 1`, canonical).Scan(&display, &props)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Signal{}, fmt.Errorf("catalog: read %s: %w", canonical, err)
	}
	signal.Text = display
	values := propValues(props)

	var names []Name
	switch kind {
	case KindAlert:
		watched, err := n.targets(ctx, canonical, graph.EdgeTypeWatches, false)
		if err != nil {
			return Signal{}, err
		}
		names = append(names, watched...)
		if signal.Related, signal.Owners, err = n.neighbourhood(ctx, canonical, graph.EdgeTypeWatches, false); err != nil {
			return Signal{}, err
		}
		for _, spelled := range stringList(values[projector.AlertUnattachedWatchesProp]) {
			if ref, err := graph.ParseRef(spelled); err == nil {
				names = append(names, Name{Ref: ref})
			}
		}
		if s, _ := values[projector.AlertTransportProp].(string); s == "human_declared" {
			signal.Kind = KindDeclared
			for _, token := range SlugTokens(display) {
				names = append(names, Name{Ref: graph.Ref{Namespace: graph.SlackSlugNamespace, Value: token}})
			}
		}
	case KindDocument:
		database, _ := values[graph.NotionDatabaseProp].(string)
		for _, value := range stringList(values[graph.NotionValuesProp]) {
			if database != "" && value != "" {
				names = append(names, Name{Ref: NotionValueRef(database, value)})
			}
		}
	case KindChange:
		targets, err := n.targets(ctx, canonical, graph.EdgeTypeChangedBy, true)
		if err != nil {
			return Signal{}, err
		}
		names = append(names, targets...)
		if signal.Related, signal.Owners, err = n.neighbourhood(ctx, canonical, graph.EdgeTypeChangedBy, true); err != nil {
			return Signal{}, err
		}
	}
	signal.Names = sortNames(names)
	return signal, nil
}

// NotionValueRef names a Notion page by one value of its entity property (ADR-0015 item 7).
func NotionValueRef(database, value string) graph.Ref {
	return graph.Ref{Namespace: graph.NotionValueNamespace, Value: database + "/" + value}
}

// declaredPrefix is the severity-and-date head of a declared incident's channel name, which names
// no service (006 `slack/sev-channel@1`).
var declaredPrefix = regexp.MustCompile(`^sev[0-9]-[0-9]{4}-[0-9]{2}-[0-9]{2}-?`)

// SlugTokens are the tokens of a declared incident's channel slug, as 006 reads them.
func SlugTokens(channel string) []string {
	slug := declaredPrefix.ReplaceAllString(strings.ToLower(strings.TrimSpace(channel)), "")
	tokens := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	slices.Sort(tokens)
	return slices.Compact(tokens)
}

// targets reads the entities a signal's edges reach, and every identity claim and correlation
// key of each with its environment: a Datadog service is named `<env>/<service>` and correlated by
// the bare `datadog.log_service` name, which is the one an environment-free alias matches. inbound
// reads edges INTO the signal (a change is the destination of `changed_by`), at any instant since a
// change is one; otherwise current edges out of it.
func (n *Names) targets(ctx context.Context, signalID string, typ graph.EdgeType, inbound bool) ([]Name, error) {
	near, far := "src_id", "dst_id"
	if inbound {
		near, far = "dst_id", "src_id"
	}
	rows, err := n.store.Pool().Query(ctx, fmt.Sprintf(`
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
		ORDER BY 1, 2, 3`, near, far), signalID, string(typ), AttrEnvironment, inbound)
	if err != nil {
		return nil, fmt.Errorf("catalog: read %s targets of %s: %w", typ, signalID, err)
	}
	defer rows.Close()
	var out []Name
	for rows.Next() {
		var name Name
		if err := rows.Scan(&name.Ref.Namespace, &name.Ref.Value, &name.Environment); err != nil {
			return nil, fmt.Errorf("catalog: scan target name: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// neighbourhood reads what the probable rules P3 and P4 look at around a signal's watched or target
// entities: the names of the entities they run on, depend on or call, and the owners they are owned
// by, both through edges valid and believed now.
func (n *Names) neighbourhood(ctx context.Context, signalID string, typ graph.EdgeType, inbound bool) ([]Name, []string, error) {
	near, far := "src_id", "dst_id"
	if inbound {
		near, far = "dst_id", "src_id"
	}
	rows, err := n.store.Pool().Query(ctx, fmt.Sprintf(`
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
		string(graph.EdgeTypeOwnedBy), inbound, AttrEnvironment)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: read the neighbourhood of %s: %w", signalID, err)
	}
	defer rows.Close()
	var (
		related []Name
		owners  []string
	)
	for rows.Next() {
		var kind string
		var name Name
		if err := rows.Scan(&kind, &name.Ref.Namespace, &name.Ref.Value, &name.Environment); err != nil {
			return nil, nil, fmt.Errorf("catalog: scan neighbour: %w", err)
		}
		if kind == "owner" {
			owners = append(owners, name.Ref.Value)
		} else {
			related = append(related, name)
		}
	}
	return sortNames(related), sortedUnique(owners), rows.Err()
}

func sortNames(names []Name) []Name {
	slices.SortFunc(names, func(a, b Name) int { return strings.Compare(a.String(), b.String()) })
	return slices.CompactFunc(names, func(a, b Name) bool { return a == b })
}

// propValues decodes a props column into its first value per key, provenance stripped.
func propValues(raw []byte) map[string]any {
	out := map[string]any{}
	if len(raw) == 0 {
		return out
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return out
	}
	type record struct {
		Value any `json:"value"`
	}
	for key, value := range fields {
		var many []record
		if err := json.Unmarshal(value, &many); err == nil && len(many) > 0 {
			out[key] = many[0].Value
			continue
		}
		var one record
		if err := json.Unmarshal(value, &one); err == nil {
			out[key] = one.Value
		}
	}
	return out
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
