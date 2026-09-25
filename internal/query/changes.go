// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"fmt"
	"slices"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/projector"
)

// Finding the changes of a window (FR-027, FR-028, edge case "dangling change").
//
// A change is a node with `changed_by` edges pointing at it from what it changed — src is the
// target, dst is the change (research §4) — and it is stored over the shortest non-empty valid
// interval there is, [t, t+1µs), so that a past change does not intersect every future window.
// Finding the candidates of a diff is therefore two ordinary graph reads and one judgement:
//
//  1. every `changed_by` edge out of a node in the neighbourhood whose valid interval meets
//     `(t1, t2]` — those are the changes that touched something we are looking at;
//  2. every change in the window that is attached to *nothing at all* — the dangling ones,
//     which no neighbourhood can rule out and which FR-028 forbids dropping;
//  3. plus, of the changes that name an unresolved target, those whose unresolved name is an
//     identifier of a node in the neighbourhood.
//
// The window is written `(t1, t2]`: a change at exactly T2 is in, because "what changed by
// 14:32" has to include what happened at 14:32, and a change whose interval ended at or before
// T1 is out, because it is already in the state the diff starts from. A change stamped exactly
// at T1 is *in*: its interval [t1, t1+1µs) runs past t1 and PostgreSQL ranges are continuous.
// That is the forgiving side of the boundary to be on — the change appears twice, as the state
// it produced and as the event that produced it, rather than disappearing at the edge of a
// window an operator picked by hand.
//
// The third rule is what makes an unattached change local rather than global where the graph
// has enough information to say so. The second is the honest fallback for when it does not: a
// change nobody can place could have hit anything, so it is offered everywhere, at the reduced
// rank rank.go gives it, rather than silently dropped. That asymmetry is deliberate — a change
// attached to a node *outside* the neighbourhood is excluded, because the graph does know where
// that one landed, and it did not land here.

// changeCandidates returns the change nodes of the window that bear on the neighbourhood.
func (e *Engine) changeCandidates(ctx context.Context, r *redirects, p diffParams, hood *neighbourhood) ([]Candidate, error) {
	targets := make([]string, 0, len(hood.reach))
	for id := range hood.reach {
		targets = append(targets, id)
	}
	slices.Sort(targets)

	attached, err := e.changesTargeting(ctx, r, p, targets)
	if err != nil {
		return nil, err
	}
	dangling, err := e.danglingChanges(ctx, r, p)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(attached)+len(dangling))
	for id := range attached {
		ids = append(ids, id)
	}
	for _, id := range dangling {
		if _, ok := attached[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		return nil, nil
	}

	// The window read is what confirms a candidate: the edge said a change touched the
	// neighbourhood, the node version says when the change was and what it was.
	versions, err := e.loadNodeVersionsWhen(ctx, p.asOf(p.t2), r, ids, validInWindow(p.t1, p.t2))
	if err != nil {
		return nil, err
	}

	var names map[string]bool // identifiers of the neighbourhood, loaded only if needed
	candidates := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		version, ok := versions[id]
		if !ok || version.GetChange() == nil {
			continue
		}
		if inSet := attached[id]; len(inSet) > 0 {
			candidates = append(candidates, attachedCandidate(version, inSet, hood))
			continue
		}
		unresolved := unattachedTargets(version)
		if len(unresolved) == 0 {
			continue
		}
		if slices.Contains(dangling, id) {
			candidates = append(candidates, Candidate{Change: version, Unattached: true})
			continue
		}
		if names == nil {
			if names, err = e.identifiersOf(ctx, r, targets); err != nil {
				return nil, err
			}
		}
		if slices.ContainsFunc(unresolved, func(name string) bool { return names[name] }) {
			candidates = append(candidates, Candidate{Change: version, Unattached: true})
		}
	}
	return candidates, nil
}

// attachedCandidate measures a change against the neighbourhood: the hop distance of its
// nearest target, and the heaviest path to it.
//
// "Nearest" is the right reading of a change with several targets — an IaC apply that touched
// six resources is as close to the focus as the closest thing it touched — and among the
// targets that are equally near, the heaviest path wins, because that is the one whose breakage
// would be felt (research §9).
func attachedCandidate(version *graphv1.NodeVersion, targets []string, hood *neighbourhood) Candidate {
	c := Candidate{Change: version, TargetIDs: targets}
	first := true
	for _, target := range targets {
		info, ok := hood.reach[target]
		if !ok {
			continue
		}
		switch {
		case first || info.hop < int(c.Hop):
			c.Hop, c.WeightClass, first = uint32(info.hop), info.weight, false
		case info.hop == int(c.Hop) && info.weight > c.WeightClass:
			c.WeightClass = info.weight
		}
	}
	return c
}

// changesTargeting returns, per change, the neighbourhood nodes it changed inside the window.
//
// The window predicate is applied to the `changed_by` edge, which the projector creates over
// exactly the change's own valid interval, so this is the same question as "the change's valid
// interval intersects the window" asked of an indexed column (research §2).
func (e *Engine) changesTargeting(ctx context.Context, r *redirects, p diffParams, targets []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(targets) == 0 {
		return out, nil
	}

	args := []any{r.raw(targets), string(graph.EdgeTypeChangedBy), p.t1.UTC(), p.t2.UTC()}
	observed, extra := p.asOf(p.t2).observedPredicate("observed", len(args)+1)
	args = append(args, extra...)

	sql := fmt.Sprintf(`
		SELECT DISTINCT src_id, dst_id
		FROM graph.edge_versions
		WHERE src_id = ANY($1) AND type = $2
		  AND valid && tstzrange($3::timestamptz, $4::timestamptz, '(]') AND %s
		ORDER BY dst_id, src_id`, observed)

	rows, err := e.store.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: read change edges: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			return nil, fmt.Errorf("query: scan change edges: %w", err)
		}
		change, target := r.canonical(dst), r.canonical(src)
		if !slices.Contains(out[change], target) {
			out[change] = append(out[change], target)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read change edges: %w", err)
	}
	for change := range out {
		slices.Sort(out[change])
	}
	return out, nil
}

// danglingChanges returns the changes of the window that are attached to nothing: they name at
// least one target the graph could not resolve, and they have no `changed_by` edge to any node
// at all (edge case "dangling change").
//
// These are the ones no neighbourhood can exclude, because the graph does not know where they
// landed. They are returned to every diff of the window at reduced rank rather than dropped.
func (e *Engine) danglingChanges(ctx context.Context, r *redirects, p diffParams) ([]string, error) {
	args := []any{projector.UnattachedTargetsProp, p.t1.UTC(), p.t2.UTC()}
	observed, extra := p.asOf(p.t2).observedPredicate("v.observed", len(args)+1)
	args = append(args, extra...)

	sql := fmt.Sprintf(`
		SELECT DISTINCT v.entity_id
		FROM graph.entity_versions v
		WHERE v.change IS NOT NULL AND v.props ? $1
		  AND v.valid && tstzrange($2::timestamptz, $3::timestamptz, '(]') AND %s
		ORDER BY v.entity_id`, observed)

	rows, err := e.store.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: read unresolved changes: %w", err)
	}
	unresolved, err := scanStrings(rows, "query: read unresolved changes")
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(unresolved) == 0 {
		return nil, nil
	}

	linked, err := e.changesWithTargets(ctx, r, p, unresolved)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(unresolved))
	for _, id := range unresolved {
		canonical := r.canonical(id)
		if linked[canonical] {
			continue
		}
		if !slices.Contains(out, canonical) {
			out = append(out, canonical)
		}
	}
	slices.Sort(out)
	return out, nil
}

// changesWithTargets reports which of the given changes are attached to any node at all,
// whatever the valid time: a change that was attached before the window still landed somewhere
// known, so it is not dangling.
func (e *Engine) changesWithTargets(ctx context.Context, r *redirects, p diffParams, changeIDs []string) (map[string]bool, error) {
	args := []any{r.raw(changeIDs), string(graph.EdgeTypeChangedBy)}
	observed, extra := p.asOf(p.t2).observedPredicate("observed", len(args)+1)
	args = append(args, extra...)

	sql := fmt.Sprintf(`
		SELECT DISTINCT dst_id FROM graph.edge_versions
		WHERE dst_id = ANY($1) AND type = $2 AND %s`, observed)

	rows, err := e.store.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: read change attachment: %w", err)
	}
	ids, err := scanStrings(rows, "query: read change attachment")
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[r.canonical(id)] = true
	}
	return out, nil
}

// unattachedTargets reads the targets a change could not be attached to off its node version.
// The property is written by the projector as a list of `namespace=value` strings
// (projector.UnattachedTargetsProp).
func unattachedTargets(version *graphv1.NodeVersion) []string {
	values := version.GetProps().GetFields()[projector.UnattachedTargetsProp].GetListValue()
	if values == nil {
		return nil
	}
	out := make([]string, 0, len(values.GetValues()))
	for _, value := range values.GetValues() {
		if name := value.GetStringValue(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// identifiersOf returns every identifier any source has claimed for the given entities, as
// `namespace=value` strings — the same spelling an unresolved target is recorded under, so the
// two can be compared without inventing a matching rule (constitution VI: no fuzzy identity).
func (e *Engine) identifiersOf(ctx context.Context, r *redirects, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := e.store.Pool().Query(ctx, `
		SELECT DISTINCT namespace, value FROM graph.identity_claims
		WHERE entity_id = ANY($1) ORDER BY namespace, value`, r.raw(ids))
	if err != nil {
		return nil, fmt.Errorf("query: read neighbourhood identifiers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var namespace, value string
		if err := rows.Scan(&namespace, &value); err != nil {
			return nil, fmt.Errorf("query: read neighbourhood identifiers: %w", err)
		}
		out[graph.Ref{Namespace: namespace, Value: value}.String()] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read neighbourhood identifiers: %w", err)
	}
	return out, nil
}
