// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The as-of slice: everything a read of the graph is built out of (FR-026, research §2).
//
// A query never walks the version tables row by row. It asks for one *slice* of the graph —
// the nodes and edges that were true at a valid instant, as known at an observed instant —
// and traverses that in memory. Two predicates define a slice and they are the whole of the
// bitemporal model as SQL sees it:
//
//	valid @> $t      the fact was true then
//	observed @> $o   we knew it then   (unset $o = "as known now")
//
// Both columns are GiST-indexed together (0002_graph.sql), so a slice is an index scan rather
// than a filter over history. Traversal depth is two or three hops, so in-process BFS over
// slices beats a recursive CTE with temporal predicates — that is the benchmark-gated
// decision in research §2 and this file is its load path.
//
// Three things that look like details and are not:
//
//   - "As known now" is not `upper_inf(observed)`. A row whose observed interval was closed a
//     microsecond ago is not current, but a row closed *in the future* cannot exist, so the two
//     agree; the predicate is written as the disjunction anyway because it is the honest
//     spelling of the question and it keeps a clock skew between the writer and the reader from
//     silently dropping a fact.
//   - Endpoint ids are passed through `merged_into` at read time. A merge re-points the edges
//     that already exist (research §4, "Implementation notes recorded after Phase 2"), so what is
//     current always names canonical ids — but the rows it closed keep the id that was merged
//     away, for ever, and a read rewound to an observed instant inside their interval is supposed
//     to find them. Resolving at read time is what makes "query by either alias returns the
//     survivor" true at every observed instant without rewriting history.
//   - Placeholder entities — an edge endpoint nothing has described yet, carrying only
//     `sre.placeholder=true` — are returned as they are. "Something called this exists and
//     nothing has described it" is an answer; hiding it would make an edge point at nothing.

// ErrFocusNotFound is returned when no entity matches a query's focus reference. Callers that
// map errors onto status codes should treat it as "not found", never as "empty result": an
// unknown name and a name with nothing valid at the as-of instant are different answers.
var ErrFocusNotFound = errors.New("query: no entity matches the focus reference")

// IDNamespace is the pseudo-namespace that addresses an entity by its canonical id rather than
// by a source's name for it, written `id=<entity_id>` (contracts/cli.md).
const IDNamespace = "id"

// maxRedirectDepth bounds the merge chain resolveCanonical will follow. Merges are recorded as
// a redirect per entity, so a chain is possible (A merged into B, B later into C); a cycle is
// impossible by construction, and this bound makes that structural rather than trusted.
const maxRedirectDepth = 32

// Engine answers reads of the graph over one store.
//
// It holds no per-query state and is safe for concurrent use: every method takes the as-of
// instants it needs, so two callers asking about two different moments never share anything.
type Engine struct {
	store *postgres.Store
	log   *eventlog.Log
}

// NewEngine returns an Engine reading through store.
func NewEngine(store *postgres.Store) *Engine {
	return &Engine{store: store, log: eventlog.New(store)}
}

// Store is the store the engine reads through.
func (e *Engine) Store() *postgres.Store { return e.store }

// AsOf is the pair of instants every read of the graph carries (constitution II).
//
// ValidAt is when the facts were true in the production system. ObservedAt is when the graph
// knew them; the zero value means "as known now", which is what FR-026 requires of an omitted
// observed instant.
type AsOf struct {
	ValidAt    time.Time
	ObservedAt time.Time
}

// AsOfFromProto converts the published AsOf message. A nil message, or one with no valid
// instant, yields the zero AsOf, which Validate rejects.
func AsOfFromProto(p *graphv1.AsOf) AsOf {
	var out AsOf
	if ts := p.GetValidAt(); ts != nil {
		out.ValidAt = ts.AsTime().UTC()
	}
	if ts := p.GetObservedAt(); ts != nil {
		out.ObservedAt = ts.AsTime().UTC()
	}
	return out
}

// ObservedNow reports whether the observed instant is "as known now".
func (a AsOf) ObservedNow() bool { return a.ObservedAt.IsZero() }

// Validate rejects an as-of with no valid instant. Defaulting it to now would answer a
// different question from the one that was asked, silently.
func (a AsOf) Validate() error {
	if a.ValidAt.IsZero() {
		return errors.New("query: as_of.valid_at is required; the graph answers as of an instant")
	}
	return nil
}

// observedPredicate renders the observed-time half of a slice predicate against the named
// column, together with the arguments it needs after the ones the caller already bound.
func (a AsOf) observedPredicate(column string, next int) (string, []any) {
	if a.ObservedNow() {
		return fmt.Sprintf("(upper_inf(%s) OR %s @> now())", column, column), nil
	}
	return fmt.Sprintf("%s @> $%d::timestamptz", column, next), []any{a.ObservedAt.UTC()}
}

// validWindow is the valid-time half of a version read: one instant a version must contain, or
// a half-open window `(from, to]` it only has to intersect.
//
// The instant is what a subgraph asks ("what was true at 14:32?"). The window is what a diff
// asks of change nodes, which are stored over the shortest non-empty interval there is —
// [t, t+1µs) — and so are contained in no instant a caller would ever name (research §4). The
// window's lower bound is exclusive because a change that happened exactly at T1 is part of the
// state the diff starts from, not of what changed since (FR-027).
type validWindow struct {
	// at is the instant a version must contain; zero when the read is a window.
	at time.Time
	// from and to bound the window, exclusive then inclusive.
	from, to time.Time
}

// validAtInstant reads the versions current at one valid instant.
func validAtInstant(t time.Time) validWindow { return validWindow{at: t} }

// validInWindow reads every version whose valid interval intersects `(from, to]`.
func validInWindow(from, to time.Time) validWindow { return validWindow{from: from, to: to} }

// predicate renders the valid-time predicate against the named column, plus the arguments it
// binds after the ones the caller already has.
func (w validWindow) predicate(column string, next int) (string, []any) {
	if w.at.IsZero() {
		return fmt.Sprintf("%s && tstzrange($%d::timestamptz, $%d::timestamptz, '(]')", column, next, next+1),
			[]any{w.from.UTC(), w.to.UTC()}
	}
	return fmt.Sprintf("%s @> $%d::timestamptz", column, next), []any{w.at.UTC()}
}

// ---------- identity ----------

// redirects is the merge map: every entity that was merged away, and what it was merged into.
//
// It is loaded once per query from the partial index on `merged_into`, so it costs one small
// indexed scan and is usually empty. Resolving in Go rather than in every SQL statement keeps
// the slice queries readable and handles a chain of merges, which a single join cannot.
type redirects struct {
	// target maps an entity id that was merged away to the id it was merged into (one step).
	target map[string]string
	// aliases maps a canonical id to every id that resolves to it, itself excluded, sorted.
	aliases map[string][]string
}

func (e *Engine) loadRedirects(ctx context.Context) (*redirects, error) {
	rows, err := e.store.Pool().Query(ctx,
		`SELECT entity_id, merged_into FROM graph.entities WHERE merged_into IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("query: read merge redirects: %w", err)
	}
	defer rows.Close()

	r := &redirects{target: map[string]string{}, aliases: map[string][]string{}}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, fmt.Errorf("query: read merge redirects: %w", err)
		}
		r.target[from] = to
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read merge redirects: %w", err)
	}
	for from := range r.target {
		canonical := r.canonical(from)
		r.aliases[canonical] = append(r.aliases[canonical], from)
	}
	for canonical := range r.aliases {
		slices.Sort(r.aliases[canonical])
	}
	return r, nil
}

// canonical follows the redirect chain to the surviving entity id.
func (r *redirects) canonical(id string) string {
	for range maxRedirectDepth {
		next, ok := r.target[id]
		if !ok {
			return id
		}
		id = next
	}
	return id
}

// raw expands canonical ids into every stored id that resolves to them, which is what the
// version tables are actually keyed by.
func (r *redirects) raw(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
		out = append(out, r.aliases[id]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// resolveFocus returns the canonical entity id a focus reference denotes (FR-039).
//
// Three ways a name reaches an entity, in order: the pseudo-namespace `id`, the canonical id
// the ref itself hashes to, and any identity claim a source made on that identifier. The last
// is what makes a Kubernetes deployment findable under its OpenTelemetry service name, and all
// three end by following `merged_into`, so a name that was merged away resolves to the
// survivor.
//
// When several claims on one identifier resolve to different survivors — two entities a human
// has not reconciled yet — the lexicographically smallest canonical id wins, so the answer is
// deterministic rather than dependent on scan order.
func (e *Engine) resolveFocus(ctx context.Context, ref graph.Ref) (string, error) {
	if ref.Namespace == "" || ref.Value == "" {
		return "", fmt.Errorf("%w: %q", ErrFocusNotFound, ref.String())
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return "", err
	}

	candidates := []string{}
	if ref.Namespace == IDNamespace {
		candidates = append(candidates, ref.Value)
	} else {
		candidates = append(candidates, graph.EntityID(ref.Namespace, ref.Value))
	}

	known, err := e.knownEntities(ctx, candidates)
	if err != nil {
		return "", err
	}
	if len(known) > 0 {
		return r.canonical(known[0]), nil
	}
	if ref.Namespace == IDNamespace {
		return "", fmt.Errorf("%w: %s", ErrFocusNotFound, ref.String())
	}

	claimed, err := e.claimedEntities(ctx, ref)
	if err != nil {
		return "", err
	}
	if len(claimed) == 0 {
		return "", fmt.Errorf("%w: %s", ErrFocusNotFound, ref.String())
	}
	resolved := make([]string, 0, len(claimed))
	for _, id := range claimed {
		resolved = append(resolved, r.canonical(id))
	}
	slices.Sort(resolved)
	return resolved[0], nil
}

func (e *Engine) knownEntities(ctx context.Context, ids []string) ([]string, error) {
	rows, err := e.store.Pool().Query(ctx,
		`SELECT entity_id FROM graph.entities WHERE entity_id = ANY($1) ORDER BY entity_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("query: look up entity: %w", err)
	}
	defer rows.Close()
	return scanStrings(rows, "query: look up entity")
}

func (e *Engine) claimedEntities(ctx context.Context, ref graph.Ref) ([]string, error) {
	rows, err := e.store.Pool().Query(ctx, `
		SELECT DISTINCT entity_id FROM graph.identity_claims
		WHERE namespace = $1 AND value = $2
		ORDER BY entity_id`, ref.Namespace, ref.Value)
	if err != nil {
		return nil, fmt.Errorf("query: look up identity claim: %w", err)
	}
	defer rows.Close()
	return scanStrings(rows, "query: look up identity claim")
}

// ---------- edges ----------

// edgeRow is one graph.edge_versions row of a slice, with both endpoints already resolved
// through `merged_into`.
type edgeRow struct {
	versionID string
	// srcID and dstID are canonical: the ids the merge redirects resolve the stored endpoints
	// to. rawSrcID and rawDstID are what the row actually stores, kept so a caller can tell a
	// pre-merge edge from a post-merge one.
	srcID, dstID        string
	rawSrcID, rawDstID  string
	typ                 graph.EdgeType
	valid               graph.Interval
	observed            graph.Interval
	weightClass         *uint32
	props               propJSON
	producedBy          []string
	closedAsConsequence string
}

// otherThan returns the endpoint of the edge that is not id, which is the node an expansion
// steps to.
func (r edgeRow) otherThan(id string) string {
	if r.srcID == id {
		return r.dstID
	}
	return r.srcID
}

// compareEdges is the deterministic order the per-hop cap truncates in: heaviest traffic
// first, then the neighbour's id, then the edge type, then the version id. Weight leads
// because a cap that has to drop edges should drop the quiet ones (FR-026, "hub explosion");
// the rest of the key exists so that two runs over the same data cut in the same place.
func compareEdges(anchor string) func(a, b edgeRow) int {
	return func(a, b edgeRow) int {
		if c := compareWeight(a.weightClass, b.weightClass); c != 0 {
			return c
		}
		if c := strings.Compare(a.otherThan(anchor), b.otherThan(anchor)); c != 0 {
			return c
		}
		if c := strings.Compare(string(a.typ), string(b.typ)); c != 0 {
			return c
		}
		return strings.Compare(a.versionID, b.versionID)
	}
}

// compareWeight orders weight classes descending with "no class" last, matching the SQL
// `weight_class DESC NULLS LAST`. An edge type that carries no traffic weight (everything but
// `calls`) sorts after every weighted edge rather than as class 0, because "not measured" and
// "measured as negligible" are different statements.
func compareWeight(a, b *uint32) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case *a > *b:
		return -1
	case *a < *b:
		return 1
	default:
		return 0
	}
}

// edgeFilter is the non-temporal part of an edge slice: which edge types may be followed and
// how much traffic an edge must carry.
type edgeFilter struct {
	types []graph.EdgeType
	// minWeightClass, when set, drops edges below it. An edge with no weight class counts as
	// class 0, so `--min-weight 1` keeps `calls` edges only — which is what asking for traffic
	// means.
	minWeightClass *uint32
}

// loadFrontierEdges returns the edges of the slice incident to the frontier, capped per
// frontier node, plus the frontier nodes whose fan-out the cap cut short.
//
// The cap is per frontier node rather than per hop in total: a hop that crosses one hub and
// four quiet services should lose the hub's tail, not four fifths of everything. Each cut node
// is named in the returned list so the response can say what was truncated and where
// (FR-026, edge case "hub explosion").
func (e *Engine) loadFrontierEdges(
	ctx context.Context,
	asOf AsOf,
	r *redirects,
	frontier []string,
	direction graphv1.Direction,
	filter edgeFilter,
	perHopCap int,
) ([]edgeRow, []string, error) {
	if len(frontier) == 0 {
		return nil, nil, nil
	}
	rows, err := e.queryFrontierEdges(ctx, asOf, r, frontier, direction, filter)
	if err != nil {
		return nil, nil, err
	}

	inFrontier := make(map[string]bool, len(frontier))
	for _, id := range frontier {
		inFrontier[id] = true
	}
	followOut := direction != graphv1.Direction_UPSTREAM
	followIn := direction != graphv1.Direction_DOWNSTREAM

	incident := map[string][]edgeRow{}
	for _, row := range rows {
		if followOut && inFrontier[row.srcID] {
			incident[row.srcID] = append(incident[row.srcID], row)
		}
		if followIn && inFrontier[row.dstID] && row.dstID != row.srcID {
			incident[row.dstID] = append(incident[row.dstID], row)
		}
	}

	var (
		kept        []edgeRow
		truncatedAt []string
		seen        = map[string]bool{}
	)
	for _, anchor := range frontier {
		edges := incident[anchor]
		slices.SortFunc(edges, compareEdges(anchor))
		if perHopCap > 0 && len(edges) > perHopCap {
			truncatedAt = append(truncatedAt, anchor)
			edges = edges[:perHopCap]
		}
		for _, row := range edges {
			if seen[row.versionID] {
				continue
			}
			seen[row.versionID] = true
			kept = append(kept, row)
		}
	}
	return kept, truncatedAt, nil
}

// queryFrontierEdges is the indexed read behind loadFrontierEdges: one statement per hop
// (research §2, "two hops = two round trips").
func (e *Engine) queryFrontierEdges(
	ctx context.Context,
	asOf AsOf,
	r *redirects,
	frontier []string,
	direction graphv1.Direction,
	filter edgeFilter,
) ([]edgeRow, error) {
	args := []any{asOf.ValidAt.UTC(), r.raw(frontier)}
	observed, extra := asOf.observedPredicate("observed", len(args)+1)
	args = append(args, extra...)

	endpoint := "(src_id = ANY($2) OR dst_id = ANY($2))"
	switch direction {
	case graphv1.Direction_DOWNSTREAM:
		endpoint = "src_id = ANY($2)"
	case graphv1.Direction_UPSTREAM:
		endpoint = "dst_id = ANY($2)"
	case graphv1.Direction_DIRECTION_UNSPECIFIED, graphv1.Direction_BOTH:
	}

	typeFilter := "true"
	if len(filter.types) > 0 {
		names := make([]string, 0, len(filter.types))
		for _, t := range filter.types {
			names = append(names, string(t))
		}
		args = append(args, names)
		typeFilter = fmt.Sprintf("type = ANY($%d)", len(args))
	}
	weightFilter := "true"
	if filter.minWeightClass != nil {
		args = append(args, int16(*filter.minWeightClass))
		weightFilter = fmt.Sprintf("coalesce(weight_class, 0) >= $%d", len(args))
	}

	sql := fmt.Sprintf(`
		SELECT version_id, src_id, dst_id, type, valid, observed,
		       valid_from_unknown, valid_to_unknown, weight_class, props,
		       produced_by_event_ids, coalesce(closed_as_consequence_of, '')
		FROM graph.edge_versions
		WHERE valid @> $1::timestamptz AND %s AND %s AND %s AND %s
		ORDER BY weight_class DESC NULLS LAST, dst_id, src_id, version_id`,
		observed, endpoint, typeFilter, weightFilter)

	rows, err := e.store.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: read edge slice: %w", err)
	}
	defer rows.Close()

	var out []edgeRow
	for rows.Next() {
		var (
			row                 edgeRow
			validRange          postgres.TimeRange
			observedRange       postgres.TimeRange
			fromUnknown, toUnkn bool
			weight              *int16
			props               []byte
			edgeType            string
		)
		if err := rows.Scan(&row.versionID, &row.rawSrcID, &row.rawDstID, &edgeType,
			&validRange, &observedRange, &fromUnknown, &toUnkn, &weight, &props,
			&row.producedBy, &row.closedAsConsequence); err != nil {
			return nil, fmt.Errorf("query: scan edge slice: %w", err)
		}
		row.typ = graph.EdgeType(edgeType)
		row.srcID = r.canonical(row.rawSrcID)
		row.dstID = r.canonical(row.rawDstID)
		row.valid = intervalOf(validRange, fromUnknown, toUnkn)
		row.observed = intervalOf(observedRange, false, false)
		if weight != nil {
			class := uint32(*weight)
			row.weightClass = &class
		}
		decoded, err := decodeProps(props)
		if err != nil {
			return nil, fmt.Errorf("query: edge %s: %w", row.versionID, err)
		}
		row.props = decoded
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read edge slice: %w", err)
	}
	return out, nil
}

// edgeVersion renders a slice row as the published message (FR-034: provenance inline).
func (e *edgeRow) edgeVersion(sources map[string]string) *graphv1.EdgeVersion {
	out := &graphv1.EdgeVersion{
		VersionId:             e.versionID,
		SrcId:                 e.srcID,
		DstId:                 e.dstID,
		Type:                  e.typ.Proto(),
		Valid:                 e.valid.Proto(),
		Observed:              e.observed.Proto(),
		WeightClass:           e.weightClass,
		Provenance:            provenanceOf(e.producedBy, sources),
		ClosedAsConsequenceOf: e.closedAsConsequence,
	}
	if props, _ := e.props.struct_(); props != nil {
		out.Props = props
	}
	return out
}

// ---------- nodes ----------

// loadNodeVersions hydrates the published NodeVersion of every entity in ids, as of the slice.
//
// An entity with no version valid at the instant is simply absent from the result: "this thing
// existed but not then" is a real answer and the caller decides what to do with it.
//
// Versions are read under every id that resolves to the canonical one, because a merge does
// not move the rows it absorbed; they keep their own entity_id and are closed in observed time
// (research §4). Reading an observed instant from before a merge can therefore surface the
// absorbed row, which is correct — that is what the graph knew then — and it is reported under
// the canonical id so that the response is internally consistent with its edges. Should both
// the survivor's and an absorbed row be current at the same instant, the survivor's wins and
// the tie after that is the lowest version id, so the answer never depends on scan order.
func (e *Engine) loadNodeVersions(ctx context.Context, asOf AsOf, r *redirects, ids []string) (map[string]*graphv1.NodeVersion, error) {
	return e.loadNodeVersionsWhen(ctx, asOf, r, ids, validAtInstant(asOf.ValidAt))
}

// loadNodeVersionsWhen is loadNodeVersions with the valid-time half of the read chosen by the
// caller: one instant for a slice, a window for the change nodes of a diff.
//
// When a window admits several versions of one entity — a change corrected inside the window,
// say — the same tie-break decides as for an instant: the row stored under the canonical id
// wins, then the lowest version id. Observed time has already reduced the candidates to one
// line of knowledge, so this only ever arbitrates between versions the caller asked to see at
// once.
func (e *Engine) loadNodeVersionsWhen(ctx context.Context, asOf AsOf, r *redirects, ids []string, when validWindow) (map[string]*graphv1.NodeVersion, error) {
	if len(ids) == 0 {
		return map[string]*graphv1.NodeVersion{}, nil
	}
	raw := r.raw(ids)

	args := []any{raw}
	valid, validArgs := when.predicate("v.valid", len(args)+1)
	args = append(args, validArgs...)
	observed, extra := asOf.observedPredicate("v.observed", len(args)+1)
	args = append(args, extra...)

	sql := fmt.Sprintf(`
		SELECT v.entity_id, v.version_id, v.display_name, v.valid, v.observed,
		       v.valid_from_unknown, v.valid_to_unknown, v.props, v.conflicts, v.facets,
		       v.pointers, v.change, v.produced_by_event_ids
		FROM graph.entity_versions v
		WHERE v.entity_id = ANY($1) AND %s AND %s
		ORDER BY v.entity_id, v.version_id`, valid, observed)

	rows, err := e.store.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: read node slice: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		version *graphv1.NodeVersion
		// exact reports that the row is stored under the canonical id rather than under an id
		// that was merged into it.
		exact bool
	}
	chosen := map[string]candidate{}
	var eventIDs []string

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
			return nil, fmt.Errorf("query: scan node slice: %w", err)
		}
		canonical := r.canonical(entityID)

		version := &graphv1.NodeVersion{
			EntityId:    canonical,
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

		next := candidate{version: version, exact: entityID == canonical}
		if current, ok := chosen[canonical]; ok && !betterCandidate(next, current) {
			continue
		}
		chosen[canonical] = next
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read node slice: %w", err)
	}

	out := make(map[string]*graphv1.NodeVersion, len(chosen))
	canonicalIDs := make([]string, 0, len(chosen))
	for id, c := range chosen {
		out[id] = c.version
		canonicalIDs = append(canonicalIDs, id)
	}
	slices.Sort(canonicalIDs)

	if err := e.attachTypes(ctx, canonicalIDs, out); err != nil {
		return nil, err
	}
	if err := e.attachAliases(ctx, r, canonicalIDs, out); err != nil {
		return nil, err
	}
	return out, e.attachSources(ctx, eventIDs, out)
}

// betterCandidate implements the tie-break documented on loadNodeVersions.
func betterCandidate(next, current struct {
	version *graphv1.NodeVersion
	exact   bool
},
) bool {
	if next.exact != current.exact {
		return next.exact
	}
	return next.version.GetVersionId() < current.version.GetVersionId()
}

// attachTypes fills in the resolved node type from graph.entities, which is where the merge
// precedence lives (research §10). The version row's `facets` says what the sources asserted;
// `type` says which of them won.
func (e *Engine) attachTypes(ctx context.Context, ids []string, versions map[string]*graphv1.NodeVersion) error {
	if len(ids) == 0 {
		return nil
	}
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
		if version, ok := versions[entityID]; ok {
			version.Type = graph.NodeType(typeName).Proto()
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: read entity types: %w", err)
	}
	return nil
}

// attachAliases fills in every identifier a source has claimed for the entity, sorted by
// namespace then value (FR-036). Claims made on an id that was merged away are claims on the
// survivor, so they are read under the raw ids too — that is what lets a caller see which name
// the graph knew the thing by before the merge.
func (e *Engine) attachAliases(ctx context.Context, r *redirects, ids []string, versions map[string]*graphv1.NodeVersion) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := e.store.Pool().Query(ctx, `
		SELECT DISTINCT entity_id, namespace, value
		FROM graph.identity_claims
		WHERE entity_id = ANY($1)
		ORDER BY namespace, value, entity_id`, r.raw(ids))
	if err != nil {
		return fmt.Errorf("query: read aliases: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	for rows.Next() {
		var entityID, namespace, value string
		if err := rows.Scan(&entityID, &namespace, &value); err != nil {
			return fmt.Errorf("query: read aliases: %w", err)
		}
		canonical := r.canonical(entityID)
		version, ok := versions[canonical]
		if !ok {
			continue
		}
		key := canonical + "\x00" + namespace + "\x00" + value
		if seen[key] {
			continue
		}
		seen[key] = true
		version.Aliases = append(version.Aliases, &graphv1.Ref{Namespace: namespace, Value: value})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: read aliases: %w", err)
	}
	// Byte order, set here rather than taken from the database's ORDER BY: see sortAliases in
	// history.go for why a locale-dependent order in a golden is a corpus that fails elsewhere.
	for _, version := range versions {
		slices.SortFunc(version.GetAliases(), compareRefs)
	}
	return nil
}

// attachSources fills in Provenance.source_id, the source of the first event that produced the
// version. `produced_by_event_ids` is stored sorted (projector segments.go), so "first" is a
// property of the set rather than of the order events happened to arrive in.
func (e *Engine) attachSources(ctx context.Context, eventIDs []string, versions map[string]*graphv1.NodeVersion) error {
	sources, err := e.eventSources(ctx, eventIDs)
	if err != nil {
		return err
	}
	for _, version := range versions {
		version.GetProvenance().SourceId = firstSource(version.GetProvenance().GetProducedByEventIds(), sources)
	}
	return nil
}

// eventSources maps event ids to the source that emitted them.
func (e *Engine) eventSources(ctx context.Context, eventIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	slices.Sort(eventIDs)
	eventIDs = slices.Compact(eventIDs)

	rows, err := e.store.Pool().Query(ctx,
		`SELECT event_id, source_id FROM log.events WHERE event_id = ANY($1)`, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("query: read event sources: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var eventID, sourceID string
		if err := rows.Scan(&eventID, &sourceID); err != nil {
			return nil, fmt.Errorf("query: read event sources: %w", err)
		}
		out[eventID] = sourceID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read event sources: %w", err)
	}
	return out, nil
}

func provenanceOf(eventIDs []string, sources map[string]string) *graphv1.Provenance {
	return &graphv1.Provenance{
		ProducedByEventIds: eventIDs,
		SourceId:           firstSource(eventIDs, sources),
	}
}

func firstSource(eventIDs []string, sources map[string]string) string {
	for _, id := range eventIDs {
		if source, ok := sources[id]; ok {
			return source
		}
	}
	return ""
}

// ---------- decoding ----------

// propRecord is one asserted value of one property key: what was asserted, by whom, and in
// which event (data-model.md, graph.entity_versions.props).
type propRecord struct {
	Value    any    `json:"value"`
	SourceID string `json:"source_id"`
	EventID  string `json:"event_id"`
}

// propJSON is a decoded props column: one entry per key, one record per source that asserted
// it. The stored shape is a bare record when a single source asserted the key and a list when
// several did; both are accepted here, because both are what the projector writes.
type propJSON map[string][]propRecord

func decodeProps(raw []byte) (propJSON, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode props: %w", err)
	}
	out := make(propJSON, len(fields))
	for key, value := range fields {
		var records []propRecord
		if err := json.Unmarshal(value, &records); err == nil {
			out[key] = records
			continue
		}
		var single propRecord
		if err := json.Unmarshal(value, &single); err != nil {
			return nil, fmt.Errorf("decode prop %q: %w", key, err)
		}
		out[key] = []propRecord{single}
	}
	return out, nil
}

// struct_ flattens the property set into the published google.protobuf.Struct: the value a
// consumer reads, with provenance stripped. Where several sources asserted a key, the first
// record's value is the one in the Struct and the disagreement is reported separately, never
// resolved here (see structWithConflicts).
func (p propJSON) struct_() (*structpb.Struct, error) {
	if len(p) == 0 {
		return nil, nil
	}
	fields := make(map[string]*structpb.Value, len(p))
	for key, records := range p {
		if len(records) == 0 {
			continue
		}
		value, err := structpb.NewValue(records[0].Value)
		if err != nil {
			return nil, fmt.Errorf("query: prop %q: %w", key, err)
		}
		fields[key] = value
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return &structpb.Struct{Fields: fields}, nil
}

// structWithConflicts flattens the property set and renders the keys the projector flagged as
// contested.
//
// A conflict is never silently resolved (data-model.md, edge case "conflicting sources"): the
// Struct carries the first asserted value so that a consumer reading props sees a value at all,
// and every asserted value — with the source and event that asserted it — is listed beside it,
// so a consumer that cares can see that the graph does not know which is right.
func (p propJSON) structWithConflicts(keys []string) (*structpb.Struct, []*graphv1.PropConflict) {
	props, err := p.struct_()
	if err != nil {
		// A value the store holds that structpb cannot represent cannot happen: the value came
		// out of a structpb.Value in the first place. Dropping props here rather than failing
		// the whole read keeps a corrupted row from hiding the rest of the graph.
		props = nil
	}
	if len(keys) == 0 {
		return props, nil
	}
	conflicts := make([]*graphv1.PropConflict, 0, len(keys))
	for _, key := range keys {
		records := p[key]
		if len(records) == 0 {
			continue
		}
		values := make([]*graphv1.AssertedValue, 0, len(records))
		for _, record := range records {
			value, err := structpb.NewValue(record.Value)
			if err != nil {
				continue
			}
			values = append(values, &graphv1.AssertedValue{
				Value:    value,
				SourceId: record.SourceID,
				EventId:  record.EventID,
			})
		}
		conflicts = append(conflicts, &graphv1.PropConflict{Key: key, Values: values})
	}
	if len(conflicts) == 0 {
		return props, nil
	}
	return props, conflicts
}

func decodePointers(raw []byte) ([]*graphv1.Pointer, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("decode pointers: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]*graphv1.Pointer, 0, len(items))
	for _, item := range items {
		pointer := &graphv1.Pointer{}
		if err := protojson.Unmarshal(item, pointer); err != nil {
			return nil, fmt.Errorf("decode pointer: %w", err)
		}
		out = append(out, pointer)
	}
	return out, nil
}

func decodeChange(raw []byte) (*graphv1.Change, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	change := &graphv1.Change{}
	if err := protojson.Unmarshal(raw, change); err != nil {
		return nil, fmt.Errorf("decode change: %w", err)
	}
	return change, nil
}

func intervalOf(r postgres.TimeRange, startUnknown, endUnknown bool) graph.Interval {
	iv := graph.Interval{Start: r.Start, StartUnknown: startUnknown}
	if !r.EndUnbounded {
		iv.End = r.End
	} else {
		iv.EndUnknown = endUnknown
	}
	return iv.Normalize()
}

func nodeTypesProto(names []string) []graphv1.NodeType {
	if len(names) == 0 {
		return nil
	}
	out := make([]graphv1.NodeType, 0, len(names))
	for _, name := range names {
		out = append(out, graph.NodeType(name).Proto())
	}
	return out
}

// rowScanner is the subset of pgx.Rows scanStrings needs, so the helper does not drag the
// driver's whole interface into its signature.
type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanStrings(rows rowScanner, what string) ([]string, error) {
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}
