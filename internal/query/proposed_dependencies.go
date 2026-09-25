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
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The proposed-dependency review queue, read-only (003 FR-029, FR-122).
//
// The edge analogue of Suggestions, and it exists for the same reason: a proposal nobody can find
// is a proposal nobody will decide, and the alternative to a queue is reading the event log. It is
// a READ, so it asks for `reader` and nothing more — an analyst with a read-only credential must be
// able to see what the graph has not dared decide, and requiring the decider role merely to look
// would push deployments into handing out write-capable tokens for review work.
//
// Deliberately a separate listing from Suggestions rather than a filter on it. A suggestion is
// about a symmetric pair of refs that may be one entity; a proposal is about a DIRECTED edge
// between two entities that are definitely not one. Sharing the response would give a reviewer an
// `A` and a `B` with a direction nothing names — and which end depends on which is the only thing
// they need to know.

// DefaultProposedDependenciesLimit is how many proposals a request naming no limit returns.
const DefaultProposedDependenciesLimit = 50

// MaxProposedDependenciesLimit caps a request's limit, so one call cannot ask for the whole table.
const MaxProposedDependenciesLimit = 500

// ProposalStatuses are the statuses a proposal may be filtered by (migration 0010).
//
// The same four words as SuggestionStatuses and deliberately a separate list: they are two
// vocabularies that happen to agree today, and collapsing them would make a change to one silently
// a change to the other.
var ProposalStatuses = []string{"pending", "confirmed", "rejected", "conflict"}

// ProposedDependencies lists proposals, strongest first, optionally filtered to the proposals one
// node is part of at either end, and to one status.
func (e *Engine) ProposedDependencies(ctx context.Context, req *graphv1.ProposedDependenciesRequest) (
	*graphv1.ProposedDependenciesResponse, error,
) {
	status := strings.ToLower(strings.TrimSpace(req.GetStatus()))
	if status != "" && !slices.Contains(ProposalStatuses, status) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"query: status %q: want one of %s", req.GetStatus(), strings.Join(ProposalStatuses, ", ")))
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = DefaultProposedDependenciesLimit
	}
	limit = min(limit, MaxProposedDependenciesLimit)

	cursor, err := parseProposalCursor(req.GetCursor())
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
		// A proposal is filed under the canonical ids current when it was made, which a later
		// merge may have turned into aliases, so the focus matches every id resolving to it.
		focusIDs = redirects.raw([]string{canonical})
	}

	rows, err := e.proposalRows(ctx, status, focusIDs, cursor, limit+1)
	if err != nil {
		return nil, err
	}

	resp := &graphv1.ProposedDependenciesResponse{}
	if len(rows) > limit {
		last := rows[limit-1]
		resp.NextCursor = encodeProposalCursor(last.GetScore(), last.GetProposalKey())
		rows = rows[:limit]
	}
	resp.Proposals = rows

	ids := make([]string, 0, 2*len(rows))
	for _, p := range rows {
		ids = append(ids, p.GetSrc().GetEntityId(), p.GetDst().GetEntityId())
	}
	identities, err := e.nodeIdentities(ctx, redirects, ids)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		if node, ok := identities[p.GetSrc().GetEntityId()]; ok {
			p.Src = node
		}
		if node, ok := identities[p.GetDst().GetEntityId()]; ok {
			p.Dst = node
		}
	}
	return resp, nil
}

// proposalRows reads the page, with each end carrying only its entity id; the identities are
// filled in afterwards, in one query rather than two per row.
func (e *Engine) proposalRows(ctx context.Context, status string, focusIDs []string,
	cursor *proposalCursor, limit int,
) ([]*graphv1.ProposedDependency, error) {
	args := []any{limit}
	where := []string{"true"}
	if status != "" {
		args = append(args, status)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	if focusIDs != nil {
		args = append(args, focusIDs)
		where = append(where, fmt.Sprintf("(src_entity_id = ANY($%d) OR dst_entity_id = ANY($%d))",
			len(args), len(args)))
	}
	if cursor != nil {
		args = append(args, cursor.score, cursor.proposalKey)
		where = append(where, fmt.Sprintf("(score, proposal_key) < ($%d::numeric, $%d)",
			len(args)-1, len(args)))
	}

	rows, err := e.store.Pool().Query(ctx, `
		SELECT proposal_key, src_entity_id, dst_entity_id, edge_type, rule_id, score, rationale,
		       evidence, created_at, status, proposed_by_event_ids, decided_by, decided_at
		FROM graph.proposed_dependencies
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY score DESC, proposal_key
		LIMIT $1`, args...)
	if err != nil {
		return nil, fmt.Errorf("query: read proposed dependencies: %w", err)
	}
	defer rows.Close()

	var out []*graphv1.ProposedDependency
	for rows.Next() {
		var (
			p         graphv1.ProposedDependency
			src, dst  string
			edgeType  string
			evidence  []byte
			createdAt time.Time
			decidedBy *string
			decidedAt *time.Time
		)
		if err := rows.Scan(&p.ProposalKey, &src, &dst, &edgeType, &p.RuleId, &p.Score,
			&p.Rationale, &evidence, &createdAt, &p.Status, &p.ProposedByEventIds,
			&decidedBy, &decidedAt); err != nil {
			return nil, fmt.Errorf("query: scan proposed dependency: %w", err)
		}
		p.Type = graph.EdgeType(edgeType).Proto()
		p.CreatedAt = timestamppb.New(createdAt.UTC())
		p.Src = &graphv1.NodeVersion{EntityId: src}
		p.Dst = &graphv1.NodeVersion{EntityId: dst}
		if decidedBy != nil {
			p.DecidedBy = *decidedBy
		}
		if decidedAt != nil {
			p.DecidedAt = timestamppb.New(decidedAt.UTC())
		}
		if structured, err := decodeEvidence(evidence); err != nil {
			return nil, fmt.Errorf("query: proposal %s evidence: %w", p.GetProposalKey(), err)
		} else {
			p.Evidence = structured
		}
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read proposed dependencies: %w", err)
	}
	return out, nil
}

// decodeEvidence turns the stored jsonb back into the struct the response carries. An empty object
// becomes nil rather than an empty struct, so canonical serialisation omits it exactly as it omits
// every other empty message.
func decodeEvidence(raw []byte) (*structpb.Struct, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return structpb.NewStruct(fields)
}

// proposalCursor is the position a paged listing resumes from: the (score, proposal_key) of the
// last row returned, which is the sort key and therefore a total order.
type proposalCursor struct {
	score       float64
	proposalKey string
}

func encodeProposalCursor(score float64, proposalKey string) string {
	return encodeSuggestionCursor(score, proposalKey)
}

func parseProposalCursor(raw string) (*proposalCursor, error) {
	parsed, err := parseSuggestionCursor(raw)
	if err != nil || parsed == nil {
		return nil, err
	}
	return &proposalCursor{score: parsed.score, proposalKey: parsed.pairKey}, nil
}
