// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The service catalog, read back (feature 009, ADR-0015 item 6; FR-005, FR-039).
//
// Every read takes 001's AsOf, so "what was the catalog, and what was this alert attributed to,
// when the investigation ran" has the answer the investigation saw. Entries are graph nodes and
// attributions graph edges, so they are read the way every slice is; aliases and decisions live in
// tables of their own (migration 0015) and are read under the same observed-time predicate.

// CatalogStatuses are the statuses ListEntries may filter by (ADR-0015 item 2).
var CatalogStatuses = []string{graph.CatalogStatusProposed, graph.CatalogStatusConfirmed, graph.CatalogStatusHidden}

// DefaultAttributedSignalsLimit and MaxAttributedSignalsLimit bound one AttributedSignals page.
const (
	DefaultAttributedSignalsLimit = 50
	MaxAttributedSignalsLimit     = 500
)

// ListEntries lists the catalog as of an instant, with each entry's aliases and the hidden names.
func (e *Engine) ListEntries(ctx context.Context, req *graphv1.ListEntriesRequest) (*graphv1.ListEntriesResponse, error) {
	asOf, err := catalogAsOf(req.GetAsOf())
	if err != nil {
		return nil, err
	}
	status := strings.ToLower(strings.TrimSpace(req.GetStatus()))
	if status != "" && !slices.Contains(CatalogStatuses, status) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"query: status %q: want one of %s", req.GetStatus(), strings.Join(CatalogStatuses, ", ")))
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}
	slugs, err := e.catalogSlugs(ctx, r)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(slugs))
	for id := range slugs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	entries, err := e.catalogEntries(ctx, asOf, r, ids, slugs)
	if err != nil {
		return nil, err
	}
	aliases, err := e.catalogAliases(ctx, asOf)
	if err != nil {
		return nil, err
	}

	resp := &graphv1.ListEntriesResponse{Hidden: aliases[graph.HiddenEntrySlug]}
	for _, entry := range entries {
		if status != "" && entry.GetStatus() != status {
			continue
		}
		for _, slug := range slugs[entry.GetNode().GetEntityId()] {
			entry.Aliases = append(entry.Aliases, aliases[slug]...)
		}
		sortCatalogAliases(entry.Aliases)
		resp.Entries = append(resp.Entries, entry)
	}
	return resp, nil
}

// GetEntry reads one entry as of an instant, its aliases, and every version it has had.
func (e *Engine) GetEntry(ctx context.Context, req *graphv1.GetEntryRequest) (*graphv1.GetEntryResponse, error) {
	asOf, err := catalogAsOf(req.GetAsOf())
	if err != nil {
		return nil, err
	}
	entryID, err := e.resolveFocus(ctx, graph.CatalogRef(req.GetSlug()))
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("query: no catalog entry %q", req.GetSlug()))
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}
	slugs, err := e.catalogSlugs(ctx, r)
	if err != nil {
		return nil, err
	}
	entries, err := e.catalogEntries(ctx, asOf, r, []string{entryID}, slugs)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf(
			"query: the catalog entry %q did not exist at that instant", req.GetSlug()))
	}
	aliases, err := e.catalogAliases(ctx, asOf)
	if err != nil {
		return nil, err
	}
	entry := entries[0]
	for _, slug := range slugs[entryID] {
		entry.Aliases = append(entry.Aliases, aliases[slug]...)
	}
	sortCatalogAliases(entry.Aliases)
	history, err := e.loadAllVersions(ctx, r.raw([]string{entryID}))
	if err != nil {
		return nil, err
	}
	return &graphv1.GetEntryResponse{Entry: entry, History: history}, nil
}

// WhyAttributed answers "what is this signal attributed to, by which rule, and why" — and "why is
// it unknown" — as of an instant (FR-039).
func (e *Engine) WhyAttributed(ctx context.Context, req *graphv1.WhyAttributedRequest) (*graphv1.WhyAttributedResponse, error) {
	asOf, err := catalogAsOf(req.GetAsOf())
	if err != nil {
		return nil, err
	}
	signalID, err := e.resolveFocus(ctx, graph.RefFromProto(req.GetSignal()))
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}
	versions, err := e.loadNodeVersions(ctx, asOf, r, []string{signalID})
	if err != nil {
		return nil, err
	}
	history, err := e.attributionHistory(ctx, asOf, r.raw([]string{signalID}))
	if err != nil {
		return nil, err
	}
	return &graphv1.WhyAttributedResponse{
		Signal:  versions[signalID],
		Current: currentAttribution(history),
		History: history,
	}, nil
}

// AttributedSignals lists the signals attributed to an entry as of an instant, and how (FR-039).
func (e *Engine) AttributedSignals(ctx context.Context, req *graphv1.AttributedSignalsRequest) (*graphv1.AttributedSignalsResponse, error) {
	asOf, err := catalogAsOf(req.GetAsOf())
	if err != nil {
		return nil, err
	}
	entryID, err := e.resolveFocus(ctx, graph.CatalogRef(req.GetSlug()))
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("query: no catalog entry %q", req.GetSlug()))
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = DefaultAttributedSignalsLimit
	}
	limit = min(limit, MaxAttributedSignalsLimit)
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}

	args := []any{r.raw([]string{entryID}), string(graph.EdgeTypeAttributedTo), asOf.ValidAt}
	observed, extra := asOf.observedPredicate("observed", len(args)+1)
	args = append(args, extra...)
	rows, err := e.store.Pool().Query(ctx, fmt.Sprintf(`
		SELECT src_id, props FROM graph.edge_versions
		WHERE dst_id = ANY($1) AND type = $2 AND valid @> $3::timestamptz AND %s
		ORDER BY src_id, version_id`, observed), args...)
	if err != nil {
		return nil, fmt.Errorf("query: read attributions of %s: %w", req.GetSlug(), err)
	}
	how := map[string]*graphv1.AttributedSignal{}
	var order []string
	for rows.Next() {
		var (
			srcID string
			props []byte
		)
		if err := rows.Scan(&srcID, &props); err != nil {
			rows.Close()
			return nil, fmt.Errorf("query: scan attribution: %w", err)
		}
		canonical := r.canonical(srcID)
		if _, seen := how[canonical]; seen || canonical <= req.GetCursor() {
			continue
		}
		decoded, err := decodeProps(props)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("query: attribution of %s: %w", srcID, err)
		}
		how[canonical] = &graphv1.AttributedSignal{
			How: &graphv1.Attributed{
				EntrySlug: req.GetSlug(),
				Layer:     decoded.text(graph.AttributionLayerProp),
				RuleId:    decoded.text(graph.AttributionRuleProp),
				Score:     decoded.number(graph.AttributionScoreProp),
			},
			DecisionEventId: decoded.text(graph.AttributionDecisionProp),
		}
		order = append(order, canonical)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read attributions: %w", err)
	}
	slices.Sort(order)
	resp := &graphv1.AttributedSignalsResponse{}
	if len(order) > limit {
		order = order[:limit]
		resp.NextCursor = order[len(order)-1]
	}
	versions, err := e.loadNodeVersions(ctx, asOf, r, order)
	if err != nil {
		return nil, err
	}
	for _, id := range order {
		signal := how[id]
		signal.Signal = versions[id]
		resp.Signals = append(resp.Signals, signal)
	}
	return resp, nil
}

// catalogAsOf validates the as-of every catalog read takes.
func catalogAsOf(p *graphv1.AsOf) (AsOf, error) {
	asOf := AsOfFromProto(p)
	if err := asOf.Validate(); err != nil {
		return asOf, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return asOf, nil
}

// catalogSlugs maps every canonical entry id to the slugs that name it: one, or several once two
// entries have been merged.
func (e *Engine) catalogSlugs(ctx context.Context, r *redirects) (map[string][]string, error) {
	rows, err := e.store.Pool().Query(ctx, `
		SELECT DISTINCT entity_id, value FROM graph.identity_claims WHERE namespace = $1
		ORDER BY value`, graph.CatalogNamespace)
	if err != nil {
		return nil, fmt.Errorf("query: read catalog entries: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			return nil, fmt.Errorf("query: scan catalog entry: %w", err)
		}
		canonical := r.canonical(id)
		if !slices.Contains(out[canonical], slug) {
			out[canonical] = append(out[canonical], slug)
		}
	}
	return out, rows.Err()
}

// catalogEntries reads the entries among ids that existed at the as-of instant, in slug order.
func (e *Engine) catalogEntries(ctx context.Context, asOf AsOf, r *redirects, ids []string,
	slugs map[string][]string,
) ([]*graphv1.CatalogEntry, error) {
	versions, err := e.loadNodeVersions(ctx, asOf, r, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*graphv1.CatalogEntry, 0, len(versions))
	for _, id := range ids {
		version, ok := versions[id]
		if !ok {
			continue
		}
		fields := version.GetProps().GetFields()
		entry := &graphv1.CatalogEntry{
			Slug:        entrySlug(id, slugs[id]),
			Node:        version,
			Kind:        fields[graph.CatalogKindProp].GetStringValue(),
			Status:      fields[graph.CatalogStatusProp].GetStringValue(),
			Owner:       fields[graph.CatalogOwnerProp].GetStringValue(),
			Description: fields[graph.CatalogDescriptionProp].GetStringValue(),
		}
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b *graphv1.CatalogEntry) int { return strings.Compare(a.GetSlug(), b.GetSlug()) })
	return out, nil
}

// entrySlug is an entry's own slug: the one its id was minted from, or the first otherwise.
func entrySlug(id string, slugs []string) string {
	for _, slug := range slugs {
		if graph.CatalogRef(slug).EntityID() == id {
			return slug
		}
	}
	if len(slugs) > 0 {
		return slugs[0]
	}
	return ""
}

// catalogAliases reads every alias current at the as-of observed instant, by entry slug.
func (e *Engine) catalogAliases(ctx context.Context, asOf AsOf) (map[string][]*graphv1.CatalogAlias, error) {
	observed, args := asOf.observedPredicate("observed", 1)
	rows, err := e.store.Pool().Query(ctx, fmt.Sprintf(`
		SELECT namespace, value, environment, entry_slug, decided_by_event_id,
		       coalesce(principal, ''), coalesce(rule_id, ''), observed
		FROM graph.catalog_alias_versions WHERE %s
		ORDER BY namespace, value, environment`, observed), args...)
	if err != nil {
		return nil, fmt.Errorf("query: read aliases: %w", err)
	}
	defer rows.Close()
	out := map[string][]*graphv1.CatalogAlias{}
	for rows.Next() {
		var (
			ns, value, env, slug, eventID, principal, rule string
			observedRange                                  postgres.TimeRange
		)
		if err := rows.Scan(&ns, &value, &env, &slug, &eventID, &principal, &rule, &observedRange); err != nil {
			return nil, fmt.Errorf("query: scan alias: %w", err)
		}
		out[slug] = append(out[slug], &graphv1.CatalogAlias{
			Name:             &graphv1.Ref{Namespace: ns, Value: value},
			Environment:      env,
			DecidedByEventId: eventID,
			Principal:        principal,
			RuleId:           rule,
			ObservedFrom:     timestamppb.New(observedRange.Start),
		})
	}
	return out, rows.Err()
}

func sortCatalogAliases(aliases []*graphv1.CatalogAlias) {
	slices.SortFunc(aliases, func(a, b *graphv1.CatalogAlias) int {
		if c := strings.Compare(graph.RefFromProto(a.GetName()).String(), graph.RefFromProto(b.GetName()).String()); c != 0 {
			return c
		}
		return strings.Compare(a.GetEnvironment(), b.GetEnvironment())
	})
}

// attributionHistory reads the decisions about a signal known at the as-of observed instant, oldest
// first. A supersession or revert decided after that instant is not shown: it was not yet known.
func (e *Engine) attributionHistory(ctx context.Context, asOf AsOf, signalIDs []string) ([]*graphv1.AttributionDecision, error) {
	cutoff := asOf.ObservedAt
	if asOf.ObservedNow() {
		cutoff = time.Now().UTC()
	}
	rows, err := e.store.Pool().Query(ctx, `
		SELECT event_id, kind, entries, suggestions, ruleset, evidence, coalesce(principal, ''),
		       decided_at, coalesce(superseded_by_event_id, ''), coalesce(reverted_by_event_id, '')
		FROM graph.attribution_decisions
		WHERE signal_entity_id = ANY($1) AND decided_at <= $2
		ORDER BY decided_at, event_id`, signalIDs, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query: read attribution decisions: %w", err)
	}
	defer rows.Close()

	var out []*graphv1.AttributionDecision
	known := map[string]bool{}
	reverted := map[string]string{}
	for rows.Next() {
		var (
			d                              graphv1.AttributionDecision
			entries, suggestions, evidence []byte
			decidedAt                      time.Time
			supersededBy, revertedBy       string
		)
		if err := rows.Scan(&d.EventId, &d.Kind, &entries, &suggestions, &d.Ruleset, &evidence,
			&d.Principal, &decidedAt, &supersededBy, &revertedBy); err != nil {
			return nil, fmt.Errorf("query: scan attribution decision: %w", err)
		}
		d.DecidedAt = timestamppb.New(decidedAt)
		if d.Entries, err = decodeAttributed(entries); err != nil {
			return nil, err
		}
		if d.Suggestions, err = decodeAttributed(suggestions); err != nil {
			return nil, err
		}
		if len(evidence) > 0 && string(evidence) != "{}" {
			d.Evidence = &structpb.Struct{}
			if err := protojson.Unmarshal(evidence, d.Evidence); err != nil {
				return nil, fmt.Errorf("query: decode evidence of %s: %w", d.EventId, err)
			}
		}
		if supersededBy != "" {
			d.SupersededByEventId = supersededBy
		}
		if revertedBy != "" {
			reverted[d.EventId] = revertedBy
		}
		known[d.EventId] = true
		out = append(out, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read attribution decisions: %w", err)
	}
	// Hide what was decided after the cutoff: a supersession the reader could not yet know of.
	for _, d := range out {
		if d.SupersededByEventId != "" && !known[d.SupersededByEventId] {
			d.SupersededByEventId = ""
		}
		if by, ok := reverted[d.EventId]; ok && known[by] && d.SupersededByEventId == "" {
			d.SupersededByEventId = by
		}
	}
	return out, nil
}

// currentAttribution replays the decisions in order the way the projector applied them: a
// correction stands until reverted, and an automated decision does not move it.
func currentAttribution(history []*graphv1.AttributionDecision) *graphv1.AttributionDecision {
	var current, standing *graphv1.AttributionDecision
	for _, d := range history {
		switch d.GetKind() {
		case "attribute":
			if standing == nil {
				current = d
			}
		case "correct":
			current, standing = d, d
		case "revert":
			current, standing = nil, nil
		}
	}
	return current
}

func decodeAttributed(raw []byte) ([]*graphv1.Attributed, error) {
	var items []struct {
		EntrySlug string  `json:"entry_slug"`
		Layer     string  `json:"layer"`
		RuleID    string  `json:"rule_id"`
		Score     float64 `json:"score"`
		Rationale string  `json:"rationale"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("query: decode attributed entries: %w", err)
	}
	out := make([]*graphv1.Attributed, 0, len(items))
	for _, item := range items {
		out = append(out, &graphv1.Attributed{
			EntrySlug: item.EntrySlug, Layer: item.Layer, RuleId: item.RuleID,
			Score: item.Score, Rationale: item.Rationale,
		})
	}
	return out, nil
}

// text and number read a prop's first value, provenance stripped.
func (p propJSON) text(key string) string {
	if records := p[key]; len(records) > 0 {
		if s, ok := records[0].Value.(string); ok {
			return s
		}
	}
	return ""
}

func (p propJSON) number(key string) float64 {
	if records := p[key]; len(records) > 0 {
		if f, ok := records[0].Value.(float64); ok {
			return f
		}
	}
	return 0
}
