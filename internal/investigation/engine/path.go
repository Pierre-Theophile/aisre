// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"sort"
	"strings"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Propagation paths (FR-019b, T128, ADR-0019 D1).
//
// A supported hypothesis says what caused the symptom; its path says how the fault got from there
// to the alerting entity. The path is drawn on the graph, not written: the shortest chain of
// `calls` and `depends_on` edges in the search-space profile from the hypothesis's target to the
// subject, preceded by the `changed_by` link from a change to its target. It is drawn at the stop
// for every supported hypothesis and for the top-ranked one, which is the one the evaluation
// scores when nothing is supported (FR-059a).
//
// Each edge cites the evidence that shows the fault crossed it, or is marked unverified. The rule
// is published and deterministic, like the first wave's: an edge into entity Y is crossed when an
// answer about Y moved — a comparison that separates, in either direction, a new log template,
// errors concentrated in one version — or when an `error_spans` answer on the edge itself holds
// errors. A supported hypothesis with an unverified edge is reported one bucket lower (FR-023),
// and an edge the fresh-context verifier removes becomes unverified too (FR-022c).
//
// A path that cannot be drawn says so, with the missing link as a resolving action in the form
// FR-026 uses for `unknown`. On an estate whose graph holds no service-to-service edge near the
// subject that is most paths, and edge F1 is published with their share (FR-059a).

// PathEdge is one edge of a path, in the direction the fault travelled.
type PathEdge struct {
	// Src and Dst are references.
	Src, Dst string
	// Type is the graph's edge type, lower-case: changed_by, calls, depends_on.
	Type string
	// AgainstEdge says the fault travelled against the edge: a callee's fault reaching its caller.
	AgainstEdge bool
	// EvidenceIDs show the fault crossed it; empty when it is unverified.
	EvidenceIDs []string
	// Unverified is true when nothing shows the fault crossed it.
	Unverified bool
}

// Path is a hypothesis's propagation path.
type Path struct {
	HypothesisID string
	Edges        []PathEdge
	// Drawable is false when no chain of edges on the graph links the cause to the subject.
	Drawable bool
	// Missing names the absent link as a resolving action, when the path is not drawable.
	Missing *investigationv1.Resolution
}

// pathEdgeTypes are the edges a fault travels along: a call is a dependency on an answer, and so
// is a declared dependency. Running on the same node is not.
var pathEdgeTypes = map[string]bool{"calls": true, "depends_on": true}

// MaxPathEvidence caps the evidence ids an edge cites: the first ones, in evidence order.
const MaxPathEvidence = 5

// Paths returns the paths drawn at the stop, in ledger rank order.
func (e *Engine) Paths() []Path {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Path(nil), e.paths...)
}

// drawPaths draws the paths of the supported hypotheses and of the top-ranked one.
func (e *Engine) drawPaths() []Path {
	var out []Path
	top := true
	for _, h := range e.ledger.Hypotheses() {
		if h.Kind == ledger.KindNoObservedChange {
			continue
		}
		if h.Status == ledger.StatusSupported || top {
			out = append(out, e.pathOf(h))
		}
		top = false
	}
	return out
}

// pathOf draws one hypothesis's path.
func (e *Engine) pathOf(h ledger.Hypothesis) Path {
	profile := e.Profile()
	idOf := map[string]string{}
	refOf := map[string]string{}
	for _, ent := range profile.InScope() {
		idOf[ent.Ref] = ent.ID
		refOf[ent.ID] = ent.Ref
	}
	resolve := func(s string) string {
		if _, ok := refOf[s]; ok {
			return s
		}
		if id, ok := idOf[s]; ok {
			return id
		}
		if id := e.catalogue.EntityIDFor(s); id != "" {
			return id
		}
		return s
	}
	name := func(id string) string {
		if ref, ok := refOf[id]; ok {
			return ref
		}
		if ref := e.catalogue.RefFor(id); ref != "" {
			return ref
		}
		return id
	}
	subject := resolve(e.subject.EntityRef)

	type link struct {
		to      string
		typ     string
		against bool
	}
	adjacent := map[string][]link{}
	for _, edge := range profile.AllEdges() {
		if !pathEdgeTypes[edge.Type] {
			continue
		}
		src, dst := idOf[edge.Src], idOf[edge.Dst]
		if src == "" || dst == "" || src == dst {
			continue
		}
		adjacent[src] = append(adjacent[src], link{dst, edge.Type, false})
		adjacent[dst] = append(adjacent[dst], link{src, edge.Type, true})
	}
	for id := range adjacent {
		links := adjacent[id]
		sort.SliceStable(links, func(i, j int) bool {
			if links[i].to != links[j].to {
				return links[i].to < links[j].to
			}
			return links[i].typ < links[j].typ
		})
	}

	targets := make([]string, 0, len(h.TargetEntityIDs))
	for _, t := range h.TargetEntityIDs {
		targets = append(targets, resolve(t))
	}
	sort.Strings(targets)
	if len(targets) == 0 {
		targets = []string{subject}
	}

	path := Path{HypothesisID: h.ID}
	var route []string
	var hops []link
	for _, target := range targets {
		nodes, links, ok := shortestRoute(target, subject, func(id string) []link { return adjacent[id] },
			func(l link) string { return l.to })
		if ok {
			route = nodes
			hops = make([]link, len(links))
			copy(hops, links)
			break
		}
	}
	if route == nil {
		target := targets[0]
		path.Missing = &investigationv1.Resolution{
			Statement: fmt.Sprintf("no calls or depends_on edge on the graph links %s to the subject %s within "+
				"the neighbourhood read: the propagation path cannot be drawn until the graph holds that link",
				name(target), name(subject)),
			Kind:         "connect_source",
			NextQuery:    e.subgraphTerm(name(target)),
			HypothesisId: h.ID,
		}
		return path
	}
	path.Drawable = true

	if h.Kind == ledger.KindChange && h.CandidateChangeEntityID != "" {
		evidence := e.crossingEvidence("", route[0])
		path.Edges = append(path.Edges, PathEdge{
			Src: name(h.CandidateChangeEntityID), Dst: name(route[0]), Type: "changed_by",
			EvidenceIDs: evidence, Unverified: len(evidence) == 0,
		})
	}
	for i, hop := range hops {
		from, to := route[i], route[i+1]
		evidence := e.crossingEvidence(from, to)
		path.Edges = append(path.Edges, PathEdge{
			Src: name(from), Dst: name(to), Type: hop.typ, AgainstEdge: hop.against,
			EvidenceIDs: evidence, Unverified: len(evidence) == 0,
		})
	}
	return path
}

// shortestRoute is a breadth-first search from one node to another, visiting neighbours in the
// order next returns them, so the route is the same on every run.
func shortestRoute[L any](from, to string, next func(string) []L, target func(L) string) ([]string, []L, bool) {
	if from == to {
		return []string{from}, nil, true
	}
	type visit struct {
		prev string
		via  L
	}
	seen := map[string]visit{from: {}}
	queue := []string{from}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for _, l := range next(at) {
			n := target(l)
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = visit{at, l}
			if n == to {
				nodes := []string{n}
				var links []L
				for cur := n; cur != from; cur = seen[cur].prev {
					links = append([]L{seen[cur].via}, links...)
					nodes = append([]string{seen[cur].prev}, nodes...)
				}
				return nodes, links, true
			}
			queue = append(queue, n)
		}
	}
	return nil, nil, false
}

// crossingEvidence is the published rule: the evidence that shows a fault reached `to`, coming
// from `from` (empty for the changed_by link, where only `to` is known).
func (e *Engine) crossingEvidence(from, to string) []string {
	profile := e.Profile()
	e.mu.Lock()
	digests := make(map[string]*investigationv1.Digest, len(e.digests))
	for id, d := range e.digests {
		digests[id] = d
	}
	e.mu.Unlock()
	var out []string
	for _, item := range e.ledger.EvidenceItems() {
		if len(out) >= MaxPathEvidence {
			break
		}
		if item.Term == nil {
			continue
		}
		digest := digests[item.ID]
		if digest == nil {
			continue
		}
		if spans := item.Term.GetErrorSpans(); spans != nil {
			ends := map[string]bool{spans.GetSrcEntityId(): true, spans.GetDstEntityId(): true}
			if ends[to] && (from == "" || ends[from]) && errorSpanCount(digest.GetTrace()) > 0 {
				out = append(out, item.ID)
			}
			continue
		}
		touched := false
		for _, id := range e.entitiesOfTerm(item.Term, profile) {
			if id == to {
				touched = true
				break
			}
		}
		if touched && moved(digest) {
			out = append(out, item.ID)
		}
	}
	return out
}

// moved reports whether a digest shows something changed on the entity it describes: a comparison
// that separates either way, a log template new in the window, or errors concentrated in one
// version.
func moved(digest *investigationv1.Digest) bool {
	if comparisons := digest.GetMetric().GetComparisons(); len(comparisons) > 0 {
		return comparisons[0].GetSeparable()
	}
	if countNew(digest.GetLog()) > 0 {
		return true
	}
	return concentrated(digest.GetErrorsByVersion())
}

// subgraphTerm is the query that would look one hop further for the missing link.
func (e *Engine) subgraphTerm(entityRef string) *investigationv1.AlgebraTerm {
	req, err := DecodeCall("subgraph", jsonObject(map[string]any{
		"entity_ref":              entityRef,
		"hops":                    NeighbourhoodHops + 1,
		"direction":               "both",
		"discriminating_question": "which edge links " + entityRef + " to the subject?",
	}), e.catalogue, e.at)
	if err != nil {
		return nil
	}
	return req.GetTerm()
}

// widenUnverifiedPaths reports every supported hypothesis whose path has an unverified edge, or
// cannot be drawn, one bucket lower (FR-019b, FR-023). A hypothesis already widened keeps its
// first reason: one step outward is the published widening, not one per reason.
func (e *Engine) widenUnverifiedPaths(paths []Path) error {
	for _, p := range paths {
		h, ok := e.ledger.Hypothesis(p.HypothesisID)
		if !ok || h.Status != ledger.StatusSupported || h.Widened {
			continue
		}
		var reason string
		if !p.Drawable {
			reason = "its propagation path cannot be drawn on the graph (FR-019b)"
		} else if unverified := p.unverified(); len(unverified) > 0 {
			reason = "its propagation path has unverified edge(s): " + strings.Join(unverified, "; ") + " (FR-019b)"
		}
		if reason == "" {
			continue
		}
		if err := e.ledger.Widen(p.HypothesisID, ledger.WidenDown, reason); err != nil {
			return err
		}
	}
	return nil
}

func (p Path) unverified() []string {
	var out []string
	for _, edge := range p.Edges {
		if edge.Unverified {
			out = append(out, edge.Src+" -"+edge.Type+"-> "+edge.Dst)
		}
	}
	return out
}

// PathClaimRef names a path edge as a claim, so a verifier finding can be traced back to it.
func PathClaimRef(hypothesisID string, edge int) string {
	return fmt.Sprintf("path.%s.%d", hypothesisID, edge)
}

// Render is a path as a reader reads it, on one line.
func (p Path) Render() string {
	if !p.Drawable {
		if p.Missing != nil {
			return "path: cannot be drawn — " + p.Missing.GetStatement()
		}
		return "path: cannot be drawn"
	}
	if len(p.Edges) == 0 {
		return "path: the cause is on the subject itself"
	}
	parts := make([]string, 0, len(p.Edges))
	for _, edge := range p.Edges {
		cite := "unverified"
		if !edge.Unverified {
			cite = strings.Join(edge.EvidenceIDs, ", ")
		}
		parts = append(parts, fmt.Sprintf("%s -%s-> %s (%s)", edge.Src, edge.Type, edge.Dst, cite))
	}
	return "path: " + strings.Join(parts, "; ")
}

// PathsProto is the paths in their published shape.
func PathsProto(paths []Path) *investigationv1.PropagationPaths {
	if paths == nil {
		return nil
	}
	out := &investigationv1.PropagationPaths{}
	for _, p := range paths {
		pp := &investigationv1.PropagationPath{HypothesisId: p.HypothesisID, Drawable: p.Drawable, MissingLink: p.Missing}
		for _, edge := range p.Edges {
			pp.Edges = append(pp.Edges, &investigationv1.PathEdge{
				Src: edge.Src, Dst: edge.Dst, EdgeType: edge.Type, AgainstEdge: edge.AgainstEdge,
				EvidenceIds: edge.EvidenceIDs, Unverified: edge.Unverified,
			})
		}
		out.Paths = append(out.Paths, pp)
	}
	return out
}
