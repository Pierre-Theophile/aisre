// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The queue of things the graph is not sure about (FR-033, ADR-0001 D6).
//
// A conservative resolver is only useful if what it refused to decide is visible. This is that
// list: every pair a probable rule matched, with the rule, the score and the rationale, so that
// confirming or rejecting one is a command rather than an investigation.
//
// Two decisions about the shape of the answer:
//
//   - the result is ordered by score, highest first, because the list is a work queue and the
//     most likely match is the one worth a person's attention first. Ties break on the pair key,
//     which is a total order, so paging is stable and a golden is reproducible.
//   - each side is rendered as an *identity* — id, resolved type, facets, display name, every
//     alias — not as a version as of an instant. A suggestion is not a temporal fact: it is a
//     question about which the graph has no opinion yet, and dressing it up with a valid interval
//     would invite a reader to think it had one. The request carries no as-of for the same reason.

// DefaultSuggestionsLimit is how many suggestions a request that names no limit returns.
const DefaultSuggestionsLimit = 50

// MaxSuggestionsLimit caps a request's limit, so one call cannot ask for the whole table.
const MaxSuggestionsLimit = 500

// SuggestionStatuses are the statuses a suggestion may be filtered by (data-model.md
// §graph.suggestions). They are also the states docs/schema/resolution.md publishes.
var SuggestionStatuses = []string{"pending", "confirmed", "rejected", "conflict"}

// Suggestions lists resolution suggestions, newest-scoring first, optionally filtered to the
// pairs one node is part of and to one status (FR-033).
func (e *Engine) Suggestions(ctx context.Context, req *graphv1.SuggestionsRequest) (*graphv1.SuggestionsResponse, error) {
	status := strings.ToLower(strings.TrimSpace(req.GetStatus()))
	if status != "" && !slices.Contains(SuggestionStatuses, status) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("query: status %q: want one of %s", req.GetStatus(), strings.Join(SuggestionStatuses, ", ")))
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = DefaultSuggestionsLimit
	}
	limit = min(limit, MaxSuggestionsLimit)

	cursor, err := parseSuggestionCursor(req.GetCursor())
	if err != nil {
		return nil, err
	}

	redirects, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}

	var focusIDs []string
	if ref := graph.RefFromProto(req.GetFocus()); ref.Namespace != "" && ref.Value != "" {
		canonical, err := e.resolveFocus(ctx, ref)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		// A suggestion names the ids as they were when it was filed, which a later merge may
		// have turned into aliases; the focus therefore matches the canonical id and every id
		// that resolves to it.
		focusIDs = redirects.raw([]string{canonical})
	}

	rows, err := e.suggestionRows(ctx, status, focusIDs, cursor, limit+1)
	if err != nil {
		return nil, err
	}

	resp := &graphv1.SuggestionsResponse{}
	if len(rows) > limit {
		last := rows[limit-1]
		resp.NextCursor = encodeSuggestionCursor(last.GetScore(), last.GetPairKey())
		rows = rows[:limit]
	}
	resp.Suggestions = rows

	ids := make([]string, 0, 2*len(rows))
	for _, s := range rows {
		ids = append(ids, s.GetA().GetEntityId(), s.GetB().GetEntityId())
	}
	identities, err := e.nodeIdentities(ctx, redirects, ids)
	if err != nil {
		return nil, err
	}
	for _, s := range rows {
		if node, ok := identities[s.GetA().GetEntityId()]; ok {
			s.A = node
		}
		if node, ok := identities[s.GetB().GetEntityId()]; ok {
			s.B = node
		}
	}
	return resp, nil
}

// suggestionRows reads the page, with each side carrying only its entity id; the identities are
// filled in afterwards, in one query rather than two per row.
func (e *Engine) suggestionRows(ctx context.Context, status string, focusIDs []string, cursor *suggestionCursor, limit int) ([]*graphv1.Suggestion, error) {
	args := []any{limit}
	where := []string{"true"}
	if status != "" {
		args = append(args, status)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	if focusIDs != nil {
		args = append(args, focusIDs)
		where = append(where, fmt.Sprintf("(entity_a = ANY($%d) OR entity_b = ANY($%d))", len(args), len(args)))
	}
	if cursor != nil {
		args = append(args, cursor.score, cursor.pairKey)
		where = append(where, fmt.Sprintf("(score, pair_key) < ($%d::numeric, $%d)", len(args)-1, len(args)))
	}

	rows, err := e.store.Pool().Query(ctx, `
		SELECT pair_key, entity_a, entity_b, rule_id, score, rationale, status, created_at
		FROM graph.suggestions
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY score DESC, pair_key
		LIMIT $1`, args...)
	if err != nil {
		return nil, fmt.Errorf("query: read suggestions: %w", err)
	}
	defer rows.Close()

	var out []*graphv1.Suggestion
	for rows.Next() {
		var (
			s         graphv1.Suggestion
			a, b      string
			createdAt time.Time
		)
		if err := rows.Scan(&s.PairKey, &a, &b, &s.RuleId, &s.Score, &s.Rationale, &s.Status, &createdAt); err != nil {
			return nil, fmt.Errorf("query: scan suggestion: %w", err)
		}
		s.CreatedAt = timestamppb.New(createdAt.UTC())
		s.A = &graphv1.NodeVersion{EntityId: a}
		s.B = &graphv1.NodeVersion{EntityId: b}
		out = append(out, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read suggestions: %w", err)
	}
	return out, nil
}

// nodeIdentities returns the identity of each entity — resolved type, facets, aliases and the
// display name of its most recent current version — without pinning it to an instant.
func (e *Engine) nodeIdentities(ctx context.Context, r *redirects, ids []string) (map[string]*graphv1.NodeVersion, error) {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return map[string]*graphv1.NodeVersion{}, nil
	}

	out := make(map[string]*graphv1.NodeVersion, len(ids))
	for _, id := range ids {
		out[id] = &graphv1.NodeVersion{EntityId: id}
	}
	if err := e.attachTypes(ctx, ids, out); err != nil {
		return nil, err
	}
	if err := e.attachIdentityNames(ctx, ids, out); err != nil {
		return nil, err
	}
	// Aliases are keyed by canonical id, and a suggestion names ids that may have been merged
	// since; each id is therefore looked up as its own canonical so that both sides of a pair
	// keep the names they were suggested under.
	for _, id := range ids {
		single := map[string]*graphv1.NodeVersion{id: out[id]}
		if err := e.attachAliases(ctx, singletonRedirects(r, id), []string{id}, single); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// singletonRedirects narrows the merge map so that attachAliases treats one id as its own
// canonical: a suggestion is about the entity as it was named, not about what it was later
// merged into.
func singletonRedirects(r *redirects, id string) *redirects {
	return &redirects{
		target:  map[string]string{},
		aliases: map[string][]string{id: r.aliases[id]},
	}
}

// attachIdentityNames fills in the display name and the resolved facets from each entity's most
// recent current version. "Most recent" is the greatest valid lower bound, tie-broken by version
// id, so the answer is a function of the graph rather than of scan order.
func (e *Engine) attachIdentityNames(ctx context.Context, ids []string, versions map[string]*graphv1.NodeVersion) error {
	rows, err := e.store.Pool().Query(ctx, `
		SELECT DISTINCT ON (entity_id) entity_id, display_name, facets
		FROM graph.entity_versions
		WHERE entity_id = ANY($1) AND upper_inf(observed)
		ORDER BY entity_id, lower(valid) DESC, version_id`, ids)
	if err != nil {
		return fmt.Errorf("query: read entity identities: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			entityID    string
			displayName string
			facets      []string
		)
		if err := rows.Scan(&entityID, &displayName, &facets); err != nil {
			return fmt.Errorf("query: read entity identities: %w", err)
		}
		if version, ok := versions[entityID]; ok {
			version.DisplayName = displayName
			version.Facets = nodeTypesProto(facets)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: read entity identities: %w", err)
	}
	return nil
}

// suggestionCursor is the position a paged listing resumes from: the (score, pair_key) of the
// last row returned, which is the sort key and therefore a total order.
type suggestionCursor struct {
	score   float64
	pairKey string
}

func encodeSuggestionCursor(score float64, pairKey string) string {
	raw := strconv.FormatFloat(score, 'f', -1, 64) + "|" + pairKey
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func parseSuggestionCursor(raw string) (*suggestionCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("query: cursor %q is not one this graph issued", raw))
	}
	score, pairKey, found := strings.Cut(string(decoded), "|")
	if !found {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("query: cursor %q is not one this graph issued", raw))
	}
	value, err := strconv.ParseFloat(score, 64)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("query: cursor %q is not one this graph issued", raw))
	}
	return &suggestionCursor{score: value, pairKey: pairKey}, nil
}
