// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// "Why are these two identifiers the same entity?" (FR-031, constitution VI).
//
// This is the query the resolution layer exists to be able to answer. A merge that cannot be
// explained will be undone by the next skeptical operator, and a merge that was wrong poisons
// every diff and blast radius until somebody notices. So the answer is not a yes or a no: it is
// the claims the graph had, the rules it applied, the scores it gave them, the people who
// overruled them, and the order it all happened in.
//
// # The observed-time approximation
//
// The request carries an observed instant, and answering it exactly would need a bitemporal
// `merged_into`. There is none: a redirect is a single column with one value, so the projection
// only knows where an identifier points *now*.
//
// What the graph does keep bitemporally is the decision history, and that is what this query
// reconstructs from. A merge decision is *in force at O* when it was decided at or before O and
// either nothing has superseded it or the decision that superseded it was taken after O. The two
// identifiers are the same entity at O when the merge decisions in force at O connect them.
//
// The approximation is therefore precise about everything a decision records and blind to
// anything it does not:
//
//   - a merge and a later split are exact, because a split supersedes the merge decisions it
//     undoes and the supersession carries its own `decided_at` (split_entity.go);
//   - a decision's `decided_at` is the observed time of the event that carried it, so a decision
//     and the claims that triggered it are on one timeline;
//   - what is *not* modelled is a redirect changed by anything other than a recorded decision,
//     which by construction cannot happen: every merge and every split writes one.
//
// The canonical id is reported only when the two do resolve to one entity at O. Naming a
// canonical id for two things the graph says are different would be an answer to a question
// nobody asked.

// DefaultAuditObservedNow is the instant an audit with no observed time answers as of: the zero
// time means "as known now" and is replaced by the query's own clock read, so that the two
// halves of one answer — the decision reconstruction and the claim filter — agree.
var auditNow = func() time.Time { return time.Now().UTC() }

// mergeKinds are the decision kinds that make two entities one.
var mergeKinds = []string{"auto_merge", "confirm", "manual_merge"}

// auditDecision is one graph.resolution_decisions row as the audit reads it.
type auditDecision struct {
	decisionID   string
	kind         string
	survivingID  string
	mergedID     string
	ruleID       string
	score        *float64
	rationale    string
	claimIDs     []string
	principal    string
	decidedAt    time.Time
	eventID      string
	supersededAt *time.Time
}

// inForceAt reports whether the decision still stands at observedAt: it had been taken, and
// nothing that superseded it had been taken yet.
func (d auditDecision) inForceAt(observedAt time.Time) bool {
	if d.decidedAt.After(observedAt) {
		return false
	}
	return d.supersededAt == nil || d.supersededAt.After(observedAt)
}

func (d auditDecision) proto() *graphv1.ResolutionDecision {
	out := &graphv1.ResolutionDecision{
		DecisionId:         d.decisionID,
		Kind:               d.kind,
		RuleId:             d.ruleID,
		Rationale:          d.rationale,
		SupportingClaimIds: d.claimIDs,
		Principal:          d.principal,
		DecidedAt:          timestamppb.New(d.decidedAt.UTC()),
		EventId:            d.eventID,
	}
	if d.score != nil {
		out.Score = *d.score
	}
	return out
}

// ResolutionAudit answers whether two identifiers resolve to one entity as of an observed
// instant, and shows the evidence (FR-031).
func (e *Engine) ResolutionAudit(ctx context.Context, req *graphv1.ResolutionAuditRequest) (*graphv1.ResolutionAuditResponse, error) {
	refA := graph.RefFromProto(req.GetA())
	refB := graph.RefFromProto(req.GetB())
	if refA.Namespace == "" || refA.Value == "" || refB.Namespace == "" || refB.Value == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("query: a resolution audit needs two references"))
	}
	observedAt := auditNow()
	if ts := req.GetObservedAt(); ts != nil {
		observedAt = ts.AsTime().UTC()
	}

	seedA, err := e.auditSeeds(ctx, refA)
	if err != nil {
		return nil, err
	}
	seedB, err := e.auditSeeds(ctx, refB)
	if err != nil {
		return nil, err
	}
	if len(seedA) == 0 || len(seedB) == 0 {
		missing := refA
		if len(seedA) > 0 {
			missing = refB
		}
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%w: %s", ErrFocusNotFound, missing))
	}

	decisions, err := e.auditDecisions(ctx, observedAt)
	if err != nil {
		return nil, err
	}
	links := mergeLinks(decisions, observedAt)

	rootsA := rootsOf(seedA, links)
	rootsB := rootsOf(seedB, links)
	same := slices.ContainsFunc(rootsA, func(id string) bool { return slices.Contains(rootsB, id) })

	resp := &graphv1.ResolutionAuditResponse{SameEntity: same}
	if same {
		shared := slices.Clone(rootsA)
		slices.Sort(shared)
		for _, id := range shared {
			if slices.Contains(rootsB, id) {
				resp.CanonicalId = id
				break
			}
		}
	}

	involved := componentOf(append(slices.Clone(seedA), seedB...), links)

	var supporting []string
	for _, d := range decisions {
		if !slices.Contains(involved, d.survivingID) && !slices.Contains(involved, d.mergedID) {
			continue
		}
		resp.Decisions = append(resp.Decisions, d.proto())
		supporting = append(supporting, d.claimIDs...)
	}
	slices.Sort(supporting)
	supporting = slices.Compact(supporting)

	claims, err := e.auditClaims(ctx, []graph.Ref{refA, refB}, involved, supporting, observedAt)
	if err != nil {
		return nil, err
	}
	resp.Claims = claims

	// The correlation keys, on the same terms (004 T148). C8's `supporting_claim_ids` are correlation
	// ids, so an audit reading only graph.identity_claims would answer a merge C8 made with an empty
	// evidence list — which is exactly the unfalsifiable conclusion FR-031 exists to prevent.
	correlations, err := e.auditCorrelations(ctx, involved, supporting, observedAt)
	if err != nil {
		return nil, err
	}
	resp.Correlations = correlations
	return resp, nil
}

// auditSeeds returns the entity ids a reference denotes, before any redirect is followed.
//
// Following redirects here would defeat the reconstruction: the point of the audit is to decide
// where the identifier pointed *then*, and `merged_into` only knows where it points now. What is
// stable is the id an identifier mints (`EntityID(namespace, value)`, research §4), so that is
// the seed whenever such an entity exists; the current claim rows are the fallback for an
// identifier that never minted one.
func (e *Engine) auditSeeds(ctx context.Context, ref graph.Ref) ([]string, error) {
	if ref.Namespace == IDNamespace {
		known, err := e.knownEntities(ctx, []string{ref.Value})
		return known, err
	}
	minted := graph.EntityID(ref.Namespace, ref.Value)
	known, err := e.knownEntities(ctx, []string{minted})
	if err != nil {
		return nil, err
	}
	if len(known) > 0 {
		// The minted id is the only stable seed. The claim rows are *not* a second opinion to
		// be unioned with it: a merge re-points them at the survivor, so reading them here
		// would seed the audit with today's answer and make every instant before the merge
		// report the pair as already merged.
		return known, nil
	}
	claimed, err := e.claimedEntities(ctx, ref)
	if err != nil {
		return nil, err
	}
	slices.Sort(claimed)
	return slices.Compact(claimed), nil
}

// auditDecisions reads every decision taken at or before observedAt, in observed order, with the
// instant of whatever superseded it so that inForceAt can be evaluated without a second query.
func (e *Engine) auditDecisions(ctx context.Context, observedAt time.Time) ([]auditDecision, error) {
	rows, err := e.store.Pool().Query(ctx, `
		SELECT d.decision_id, d.kind, coalesce(d.surviving_id, ''), coalesce(d.merged_id, ''),
		       coalesce(d.rule_id, ''), d.score, d.rationale, d.supporting_claim_ids,
		       coalesce(d.principal, ''), d.decided_at, d.event_id, s.decided_at
		FROM graph.resolution_decisions d
		LEFT JOIN graph.resolution_decisions s ON s.decision_id = d.superseded_by
		WHERE d.decided_at <= $1::timestamptz
		ORDER BY d.decided_at, d.decision_id`, observedAt)
	if err != nil {
		return nil, fmt.Errorf("query: read resolution decisions: %w", err)
	}
	defer rows.Close()

	var out []auditDecision
	for rows.Next() {
		var d auditDecision
		if err := rows.Scan(&d.decisionID, &d.kind, &d.survivingID, &d.mergedID, &d.ruleID,
			&d.score, &d.rationale, &d.claimIDs, &d.principal, &d.decidedAt, &d.eventID,
			&d.supersededAt); err != nil {
			return nil, fmt.Errorf("query: scan resolution decision: %w", err)
		}
		d.decidedAt = d.decidedAt.UTC()
		if d.supersededAt != nil {
			at := d.supersededAt.UTC()
			d.supersededAt = &at
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read resolution decisions: %w", err)
	}
	return out, nil
}

// mergeLinks turns the decisions in force at observedAt into the redirect map they imply.
func mergeLinks(decisions []auditDecision, observedAt time.Time) map[string]string {
	links := map[string]string{}
	for _, d := range decisions {
		if !slices.Contains(mergeKinds, d.kind) || !d.inForceAt(observedAt) {
			continue
		}
		if d.mergedID == "" || d.survivingID == "" || d.mergedID == d.survivingID {
			continue
		}
		links[d.mergedID] = d.survivingID
	}
	return links
}

// rootsOf follows the reconstructed links to the surviving ids, deduplicated and sorted.
func rootsOf(ids []string, links map[string]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, followLinks(id, links))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func followLinks(id string, links map[string]string) string {
	for range maxRedirectDepth {
		next, ok := links[id]
		if !ok {
			return id
		}
		id = next
	}
	return id
}

// componentOf returns every id connected to the seeds by the reconstructed links, in either
// direction: the ids whose claims and decisions are part of the answer.
func componentOf(seeds []string, links map[string]string) []string {
	roots := map[string]bool{}
	for _, id := range seeds {
		roots[followLinks(id, links)] = true
	}
	out := slices.Clone(seeds)
	for id := range roots {
		out = append(out, id)
	}
	for from := range links {
		if roots[followLinks(from, links)] {
			out = append(out, from)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// auditClaims returns the claims behind the answer: everything asserted about the entities
// involved, plus every claim on the two identifiers themselves, as known at observedAt.
//
// Three things reach the list: every claim currently pointing at an entity in the answer, every
// claim on the two identifiers themselves, and every claim a decision in the answer recorded as
// its evidence (FR-038). The third is not redundant. A claim moves with its entity — a merge
// re-points it, a split moves it again — so a claim that supported a merge and has since been
// split away would otherwise vanish from the explanation of that very merge.
//
// Claims are filtered by their own observed time, which is exact — unlike the redirect, a claim
// row records when the graph learned it and never moves (FR-036). A claim the graph had not yet
// seen at O is absent, which is what makes the audit reproduce what could have been known then.
func (e *Engine) auditClaims(ctx context.Context, refs []graph.Ref, entityIDs, claimIDs []string, observedAt time.Time) ([]*graphv1.IdentityClaimRecord, error) {
	namespaces := make([]string, 0, len(refs))
	values := make([]string, 0, len(refs))
	for _, ref := range refs {
		namespaces = append(namespaces, ref.Namespace)
		values = append(values, ref.Value)
	}
	rows, err := e.store.Pool().Query(ctx, `
		SELECT claim_id, entity_id, namespace, value, attributes, source_id, observed_at, event_id
		FROM graph.identity_claims
		WHERE observed_at <= $1::timestamptz
		  AND (entity_id = ANY($2) OR claim_id = ANY($5) OR (namespace = ANY($3) AND value = ANY($4)))
		ORDER BY observed_at, claim_id`, observedAt, entityIDs, namespaces, values, claimIDs)
	if err != nil {
		return nil, fmt.Errorf("query: read identity claims: %w", err)
	}
	defer rows.Close()

	var out []*graphv1.IdentityClaimRecord
	for rows.Next() {
		var (
			record     graphv1.IdentityClaimRecord
			namespace  string
			value      string
			attributes []byte
			observed   time.Time
		)
		if err := rows.Scan(&record.ClaimId, &record.EntityId, &namespace, &value,
			&attributes, &record.SourceId, &observed, &record.EventId); err != nil {
			return nil, fmt.Errorf("query: scan identity claim: %w", err)
		}
		record.Claim = &graphv1.Ref{Namespace: namespace, Value: value}
		record.ObservedAt = timestamppb.New(observed.UTC())
		attrs, err := claimAttributes(attributes)
		if err != nil {
			return nil, err
		}
		record.Attributes = attrs
		out = append(out, &record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read identity claims: %w", err)
	}
	return out, nil
}

// auditCorrelations returns the correlation keys behind the answer (004 T148).
//
// Two things reach the list: every key currently on an entity in the answer, and every key a decision
// in the answer recorded as its evidence. The second is not redundant, for the reason the claim side
// gives — a key moves with its entity — and for one of its own: a merge deletes a key whose twin the
// survivor already carries, so a key that supported a merge can be gone from the live table while the
// decision citing it stands.
//
// It takes no refs, unlike auditClaims. A correlation value is not an identifier: nobody addresses an
// entity by `deploy.commit_sha=8f5b…`, so the two refs the audit was asked about are never correlation
// values, and looking them up as though they were would return every change that shipped a commit
// somebody happened to name.
func (e *Engine) auditCorrelations(ctx context.Context, entityIDs, correlationIDs []string, observedAt time.Time) ([]*graphv1.CorrelationKeyRecord, error) {
	rows, err := e.store.Pool().Query(ctx, `
		SELECT correlation_id, entity_id, namespace, value, attributes, source_id, observed_at, event_id
		FROM graph.correlation_keys
		WHERE observed_at <= $1::timestamptz
		  AND (entity_id = ANY($2) OR correlation_id = ANY($3))
		ORDER BY observed_at, correlation_id`, observedAt, entityIDs, correlationIDs)
	if err != nil {
		return nil, fmt.Errorf("query: read correlation keys: %w", err)
	}
	defer rows.Close()

	var out []*graphv1.CorrelationKeyRecord
	for rows.Next() {
		var (
			record     graphv1.CorrelationKeyRecord
			namespace  string
			value      string
			attributes []byte
			observed   time.Time
		)
		if err := rows.Scan(&record.CorrelationId, &record.EntityId, &namespace, &value,
			&attributes, &record.SourceId, &observed, &record.EventId); err != nil {
			return nil, fmt.Errorf("query: scan correlation key: %w", err)
		}
		record.Key = &graphv1.Ref{Namespace: namespace, Value: value}
		record.ObservedAt = timestamppb.New(observed.UTC())
		attrs, err := claimAttributes(attributes)
		if err != nil {
			return nil, err
		}
		record.Attributes = attrs
		out = append(out, &record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read correlation keys: %w", err)
	}
	return out, nil
}

// claimAttributes decodes a claim's supporting attributes.
//
// They are a plain JSON object, not the per-source property records a version row holds: a claim
// is one source's statement, so there is nobody to disagree with it (data-model.md
// §graph.identity_claims). An empty object yields a nil Struct, which protojson omits, so a
// claim with no attributes and a claim with an empty attribute map serialize identically.
func claimAttributes(raw []byte) (*structpb.Struct, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("query: decode claim attributes: %w", err)
	}
	if len(decoded) == 0 {
		return nil, nil
	}
	attrs, err := structpb.NewStruct(decoded)
	if err != nil {
		return nil, fmt.Errorf("query: encode claim attributes: %w", err)
	}
	return attrs, nil
}
