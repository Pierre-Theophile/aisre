// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The bridge from a fixture manifest to the query engine (FR-047, constitution VIII).
//
// A manifest query is a flat YAML mapping covering every query kind the contract names; this
// file turns one into a request and answers it. Kinds whose engine has not landed yet are
// reported as unsupported rather than guessed at, so a fixture may list the queries a later
// phase will answer and the verifier skips them instead of failing — which is what lets the
// goldens for one user story be recorded without waiting for the next.

// Runner answers manifest queries against one engine. It implements fixture.QueryRunner.
type Runner struct {
	engine *Engine
}

var _ fixture.QueryRunner = (*Runner)(nil)

// NewRunner returns the manifest query runner for engine.
func NewRunner(engine *Engine) *Runner { return &Runner{engine: engine} }

// Run answers one manifest query. The returned message is the whole response message, which is
// what the golden holds.
func (r *Runner) Run(ctx context.Context, q fixture.Query) (proto.Message, error) {
	switch q.Kind {
	case "subgraph":
		req, err := SubgraphRequestFromQuery(q)
		if err != nil {
			return nil, err
		}
		return r.engine.Subgraph(ctx, req)
	case "diff":
		req, err := DiffRequestFromQuery(q)
		if err != nil {
			return nil, err
		}
		return r.engine.Diff(ctx, req)
	case "impact":
		req, err := ImpactRequestFromQuery(q)
		if err != nil {
			return nil, err
		}
		return r.engine.Impact(ctx, req)
	case "pointers":
		req, err := PointersRequestFromQuery(q)
		if err != nil {
			return nil, err
		}
		return r.engine.Pointers(ctx, req)
	case "history":
		req, err := NodeHistoryRequestFromQuery(q)
		if err != nil {
			return nil, err
		}
		return r.engine.NodeHistory(ctx, req)
	case "audit":
		req, err := ResolutionAuditRequestFromQuery(q)
		if err != nil {
			return nil, err
		}
		return r.engine.ResolutionAudit(ctx, req)
	case "suggestions":
		req, err := SuggestionsRequestFromQuery(q)
		if err != nil {
			return nil, err
		}
		return r.engine.Suggestions(ctx, req)
	case "extent":
		return r.engine.log.Extent(ctx)
	default:
		return nil, fmt.Errorf("query: %q: %w", q.Kind, fixture.ErrUnsupportedQueryKind)
	}
}

// SubgraphRequestFromQuery builds a SubgraphRequest from a manifest query. Unset numeric fields
// stay unset so the engine applies the published defaults rather than zero.
func SubgraphRequestFromQuery(q fixture.Query) (*graphv1.SubgraphRequest, error) {
	ref, err := q.FocusRef()
	if err != nil {
		return nil, err
	}
	direction, err := ParseDirection(q.Direction)
	if err != nil {
		return nil, err
	}
	edgeTypes, err := ParseEdgeTypes(q.EdgeTypes)
	if err != nil {
		return nil, err
	}

	req := &graphv1.SubgraphRequest{
		Focus:     ref.Proto(),
		AsOf:      &graphv1.AsOf{ValidAt: timestamppb.New(q.ValidAt.UTC())},
		Hops:      uint32(max(q.Hops, 0)),
		Direction: direction,
		EdgeTypes: edgeTypes,
	}
	if !q.ObservedAt.IsZero() {
		req.AsOf.ObservedAt = timestamppb.New(q.ObservedAt.UTC())
	}
	if q.PerHopCap > 0 {
		cap := uint32(q.PerHopCap)
		req.PerHopCap = &cap
	}
	if q.TotalCap > 0 {
		cap := uint32(q.TotalCap)
		req.TotalCap = &cap
	}
	if q.MinWeightClass != nil {
		if *q.MinWeightClass < 0 {
			return nil, fmt.Errorf("query: min_weight_class %d is negative", *q.MinWeightClass)
		}
		class := uint32(*q.MinWeightClass)
		req.MinWeightClass = &class
	}
	return req, nil
}

// DiffRequestFromQuery builds a DiffRequest from a manifest query (FR-027).
//
// The window and the reference instant are named keys; the two ranking knobs — `tau_seconds`
// and `change_hop_margin` — arrive through Query.Extra, which is where the manifest loader puts
// keys it predates. A fixture that does not set them gets the published defaults, which is what
// a fixture recording the *published* ranking should do.
func DiffRequestFromQuery(q fixture.Query) (*graphv1.DiffRequest, error) {
	sub, err := SubgraphRequestFromQuery(q)
	if err != nil {
		return nil, err
	}
	// A diff's two instants are t1 and t2; the sub-request's own as-of is ignored by the
	// engine, so it is cleared rather than left holding the zero instant.
	sub.AsOf = nil

	if q.T1.IsZero() || q.T2.IsZero() {
		return nil, fmt.Errorf("query: diff %q needs both t1 and t2", q.Name)
	}
	req := &graphv1.DiffRequest{
		Subgraph: sub,
		T1:       timestamppb.New(q.T1.UTC()),
		T2:       timestamppb.New(q.T2.UTC()),
	}
	if !q.ObservedAt.IsZero() {
		req.ObservedAt = timestamppb.New(q.ObservedAt.UTC())
	}
	if !q.ReferenceAt.IsZero() {
		req.ReferenceAt = timestamppb.New(q.ReferenceAt.UTC())
	}
	tau, ok, err := extraNumber(q, "tau_seconds")
	if err != nil {
		return nil, err
	}
	if ok {
		req.TauSeconds = &tau
	}
	margin, ok, err := extraNumber(q, "change_hop_margin")
	if err != nil {
		return nil, err
	}
	if ok {
		if margin < 0 {
			return nil, fmt.Errorf("query: diff %q: change_hop_margin %v is negative", q.Name, margin)
		}
		value := uint32(margin)
		req.ChangeHopMargin = &value
	}
	return req, nil
}

// ImpactRequestFromQuery builds an ImpactRequest from a manifest query (FR-029).
//
// The radius may be written either as the manifest's own `hops` key — which is what it is
// called for a subgraph, and an impact query's radius is the same idea — or as `max_hops`, the
// name the published request uses; `max_hops` wins when both are set. `total_cap` is the
// manifest's named key. Anything left unset gets the published default, which is what a fixture
// recording the *published* behaviour should record.
func ImpactRequestFromQuery(q fixture.Query) (*graphv1.ImpactRequest, error) {
	ref, err := q.FocusRef()
	if err != nil {
		return nil, fmt.Errorf("query: impact %q: focus: %w", q.Name, err)
	}
	if q.ValidAt.IsZero() {
		return nil, fmt.Errorf("query: impact %q needs a valid_at; the graph answers as of an instant", q.Name)
	}
	req := &graphv1.ImpactRequest{
		Focus: ref.Proto(),
		AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(q.ValidAt.UTC())},
	}
	if !q.ObservedAt.IsZero() {
		req.AsOf.ObservedAt = timestamppb.New(q.ObservedAt.UTC())
	}
	if q.Hops > 0 {
		hops := uint32(q.Hops)
		req.MaxHops = &hops
	}
	maxHops, ok, err := extraNumber(q, "max_hops")
	if err != nil {
		return nil, err
	}
	if ok {
		if maxHops < 0 {
			return nil, fmt.Errorf("query: impact %q: max_hops %v is negative", q.Name, maxHops)
		}
		hops := uint32(maxHops)
		req.MaxHops = &hops
	}
	if q.TotalCap > 0 {
		cap := uint32(q.TotalCap)
		req.TotalCap = &cap
	}
	return req, nil
}

// PointersRequestFromQuery builds a PointersRequest from a manifest query (FR-030). Pointers are
// versioned like any other property, so the instant is required: a fixture that asked for "now"
// would record a golden that stops matching the moment anything else is learned.
func PointersRequestFromQuery(q fixture.Query) (*graphv1.PointersRequest, error) {
	ref, err := q.FocusRef()
	if err != nil {
		return nil, fmt.Errorf("query: pointers %q: focus: %w", q.Name, err)
	}
	if q.ValidAt.IsZero() {
		return nil, fmt.Errorf("query: pointers %q needs a valid_at; pointers are versioned in time", q.Name)
	}
	req := &graphv1.PointersRequest{
		Focus: ref.Proto(),
		AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(q.ValidAt.UTC())},
	}
	if !q.ObservedAt.IsZero() {
		req.AsOf.ObservedAt = timestamppb.New(q.ObservedAt.UTC())
	}
	return req, nil
}

// NodeHistoryRequestFromQuery builds a NodeHistoryRequest from a manifest query (FR-032).
//
// A history takes no instant, so a manifest entry that carries one is a fixture saying something
// this query cannot do. It is refused rather than ignored: a silently dropped `observed_at` would
// record a golden that does not answer the question the manifest asked.
func NodeHistoryRequestFromQuery(q fixture.Query) (*graphv1.NodeHistoryRequest, error) {
	ref, err := q.FocusRef()
	if err != nil {
		return nil, fmt.Errorf("query: history %q: focus: %w", q.Name, err)
	}
	if !q.ValidAt.IsZero() {
		return nil, fmt.Errorf("query: history %q sets valid_at; a history is the whole of observed time "+
			"and takes no instant (FR-032)", q.Name)
	}
	return &graphv1.NodeHistoryRequest{Focus: ref.Proto()}, nil
}

// extraNumber reads a numeric manifest key out of Query.Extra, reporting whether it was set.
// YAML gives back an int or a float depending on how it was written, so both are accepted.
func extraNumber(q fixture.Query, key string) (float64, bool, error) {
	raw, ok := q.Extra[key]
	if !ok {
		return 0, false, nil
	}
	switch value := raw.(type) {
	case int:
		return float64(value), true, nil
	case int64:
		return float64(value), true, nil
	case float64:
		return value, true, nil
	default:
		return 0, false, fmt.Errorf("query: %q: %s = %v: want a number", q.Name, key, raw)
	}
}

// ParseDirection accepts the spellings a manifest and the CLI use. An empty string means the
// default, BOTH.
func ParseDirection(s string) (graphv1.Direction, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "both":
		return graphv1.Direction_BOTH, nil
	case "up", "upstream":
		return graphv1.Direction_UPSTREAM, nil
	case "down", "downstream":
		return graphv1.Direction_DOWNSTREAM, nil
	default:
		return graphv1.Direction_DIRECTION_UNSPECIFIED,
			fmt.Errorf("query: direction %q: want up, down or both", s)
	}
}

// ParseEdgeTypes converts edge type names to the published enum. Both the canonical spelling
// (`depends_on`) and the hyphenated one used in prose and on the command line (`depends-on`)
// are accepted.
func ParseEdgeTypes(names []string) ([]graphv1.EdgeType, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]graphv1.EdgeType, 0, len(names))
	for _, name := range names {
		normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
		if normalized == "" {
			continue
		}
		edgeType, err := graph.ParseEdgeType(normalized)
		if err != nil {
			return nil, fmt.Errorf("query: edge type %q: want one of calls, depends_on, runs_on, deployed_by, owned_by, exposed_via, changed_by", name)
		}
		out = append(out, edgeType.Proto())
	}
	return out, nil
}

// ResolutionAuditRequestFromQuery builds a ResolutionAuditRequest from a manifest query
// (FR-031). `ref_a` and `ref_b` are the pair; `observed_at` pins the instant the answer is given
// as of, and leaving it unset means "as known now" — which a fixture should not do, because a
// golden recorded against the current clock would stop matching the moment anything else was
// decided.
func ResolutionAuditRequestFromQuery(q fixture.Query) (*graphv1.ResolutionAuditRequest, error) {
	refA, err := graph.ParseRef(q.RefA)
	if err != nil {
		return nil, fmt.Errorf("query: audit %q: ref_a: %w", q.Name, err)
	}
	refB, err := graph.ParseRef(q.RefB)
	if err != nil {
		return nil, fmt.Errorf("query: audit %q: ref_b: %w", q.Name, err)
	}
	req := &graphv1.ResolutionAuditRequest{A: refA.Proto(), B: refB.Proto()}
	if !q.ObservedAt.IsZero() {
		req.ObservedAt = timestamppb.New(q.ObservedAt.UTC())
	}
	return req, nil
}

// SuggestionsRequestFromQuery builds a SuggestionsRequest from a manifest query (FR-033). Both
// `focus` and `status` are optional: a fixture that names neither records the whole queue, which
// is exactly what "nothing was merged by a probable rule" is asserted from (SC-007).
func SuggestionsRequestFromQuery(q fixture.Query) (*graphv1.SuggestionsRequest, error) {
	req := &graphv1.SuggestionsRequest{Status: q.Status}
	if strings.TrimSpace(q.Focus) != "" {
		ref, err := q.FocusRef()
		if err != nil {
			return nil, fmt.Errorf("query: suggestions %q: focus: %w", q.Name, err)
		}
		req.Focus = ref.Proto()
	}
	limit, ok, err := extraNumber(q, "limit")
	if err != nil {
		return nil, err
	}
	if ok {
		req.Limit = uint32(max(limit, 0))
	}
	if cursor, ok := q.Extra["cursor"].(string); ok {
		req.Cursor = cursor
	}
	return req, nil
}
