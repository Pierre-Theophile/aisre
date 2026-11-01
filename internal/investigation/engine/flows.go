// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Absence in the first wave (FR-046b, T126, ADR-0019 D1).
//
// Error rate and latency cannot see a service that stopped receiving calls: a silent callee has no
// errors and no slow requests, it has no requests. So the wave also compares, before and after the
// onset, the request rate of each top candidate's callers and callees, and of the entities at the
// end of every edge the intake diff reports as retracted, or whose weight class fell, inside the
// window. They are ordinary `compare` terms with the RATE statistic the algebra already publishes.
//
// They are not judged by FirstWaveVerdict. A rise in a neighbour's rate says nothing for or against
// the candidate the call served, and a fall is not evidence about that candidate either: it is a
// candidate of its own. A fall of at least StoppedFlowFall opens a `condition` hypothesis, category
// `stopped_flow`, with a prior scaled by the fall's magnitude and no judgment: the investigator
// tests it like any other (option 0 of the T126 decision, 2026-11-01). A deterministic rule that
// supported it would rank consequences above causes, because a failing service's callees lose
// their traffic too.

// CategoryStoppedFlow is the cause category of a flow the first wave surfaced by its absence.
const CategoryStoppedFlow = "stopped_flow"

// StoppedFlowFall is the relative fall in request rate at which the wave opens a stopped-flow
// candidate: half the traffic gone. A smaller fall is recorded as evidence and opens nothing,
// because a diurnal trough or a retry storm elsewhere moves a rate by a third without anything
// having stopped.
const StoppedFlowFall = 0.5

// MaxFlowComparisons caps the wave's rate comparisons, in plan order: the callers and callees of
// the first candidate before those of the second, retracted edges last. Twelve keeps the wave's
// added cost below its own five queries per candidate on the corpus's largest neighbourhood.
const MaxFlowComparisons = 12

// fallenEdge is an edge the intake diff reports as retracted, or whose weight class fell.
type fallenEdge struct {
	srcID, dstID string
	why          string
}

// flowPlan is one entity whose request rate the wave compares.
type flowPlan struct {
	hypothesisID string
	entityRef    string
	// reason is why the entity is compared: `callee of <ref>`, `caller of <ref>`, or the edge.
	reason string
}

// noteFallenEdges files the edges of the intake diff that lost their flow inside the window.
func (e *Engine) noteFallenEdges(diff *graphv1.DiffResponse) {
	var out []fallenEdge
	for _, edge := range diff.GetEdgesRemoved() {
		if edge.GetType() == graphv1.EdgeType_CALLS {
			out = append(out, fallenEdge{edge.GetSrcId(), edge.GetDstId(), "retracted in the window"})
		}
	}
	for _, delta := range diff.GetEdgesChanged() {
		before, after := delta.GetBefore(), delta.GetAfter()
		if after.GetType() != graphv1.EdgeType_CALLS || before.WeightClass == nil || after.WeightClass == nil {
			continue
		}
		if after.GetWeightClass() < before.GetWeightClass() {
			out = append(out, fallenEdge{after.GetSrcId(), after.GetDstId(), fmt.Sprintf(
				"weight class fell from %d to %d in the window", before.GetWeightClass(), after.GetWeightClass())})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].srcID != out[j].srcID {
			return out[i].srcID < out[j].srcID
		}
		return out[i].dstID < out[j].dstID
	})
	e.mu.Lock()
	e.fallenEdges = out
	e.mu.Unlock()
}

// planFlows picks the entities whose request rate the wave compares, in a fixed order, and records
// a profile blind spot for each candidate whose target has no caller or callee on the graph: no
// comparison is invented for it (FR-046b).
func (e *Engine) planFlows(plans []candidatePlan) []flowPlan {
	profile := e.Profile()
	edges := profile.AllEdges()
	seen := map[string]bool{}
	var out []flowPlan
	add := func(f flowPlan) {
		if f.entityRef == "" || seen[f.entityRef] {
			return
		}
		seen[f.entityRef] = true
		out = append(out, f)
	}
	var blind []string
	for _, plan := range plans {
		var callers, callees []string
		for _, edge := range edges {
			if edge.Type != strings.ToLower(graphv1.EdgeType_CALLS.String()) || edge.Src == edge.Dst {
				continue
			}
			switch plan.targetRef {
			case edge.Src:
				callees = append(callees, edge.Dst)
			case edge.Dst:
				callers = append(callers, edge.Src)
			}
		}
		if len(callers) == 0 && len(callees) == 0 {
			blind = append(blind, fmt.Sprintf(
				"%s, the target of %s, has no caller or callee on the graph: a flow that stopped there "+
					"cannot be compared", plan.targetRef, plan.hypothesisID))
			continue
		}
		for _, ref := range callees {
			add(flowPlan{plan.hypothesisID, ref, "callee of " + plan.targetRef})
		}
		for _, ref := range callers {
			add(flowPlan{plan.hypothesisID, ref, "caller of " + plan.targetRef})
		}
	}
	e.mu.Lock()
	fallen := append([]fallenEdge(nil), e.fallenEdges...)
	e.mu.Unlock()
	for _, edge := range fallen {
		src, dst := e.catalogue.RefFor(edge.srcID), e.catalogue.RefFor(edge.dstID)
		if dst == "" {
			continue
		}
		if src == "" {
			src = edge.srcID
		}
		add(flowPlan{"", dst, fmt.Sprintf("callee of the edge %s -> %s, %s", src, dst, edge.why)})
	}
	if len(out) > MaxFlowComparisons {
		blind = append(blind, fmt.Sprintf(
			"the first wave compared the request rate of %d of %d callers and callees (cap %d); the rest "+
				"are unexamined for a stopped flow", MaxFlowComparisons, len(out), MaxFlowComparisons))
		out = out[:MaxFlowComparisons]
	}
	if len(blind) > 0 && profile != nil {
		e.mu.Lock()
		profile.BlindSpots = append(profile.BlindSpots, blind...)
		e.mu.Unlock()
	}
	return out
}

// planFlowQueries builds the rate comparisons, one per flow whose entity has a metric pointer. An
// entity with none is already a blind spot of the profile.
func (e *Engine) planFlowQueries(flows []flowPlan, reference time.Time) ([]*investigationv1.AlgebraRequest, error) {
	pair := ComparePairs(reference, e.at.ObservedAt, e.Lookback())[0]
	var out []*investigationv1.AlgebraRequest
	for _, flow := range flows {
		metric := e.pointerOfKind(flow.entityRef, graphv1.PointerKind_METRIC)
		if metric == "" {
			continue
		}
		fields := map[string]any{
			"pointer_id":              metric,
			"reference_at":            pair.GetReferenceAt().AsTime().UTC().Format(time.RFC3339),
			"width_seconds":           pair.GetWidthSeconds(),
			"statistic":               "rate",
			"discriminating_question": "did the request rate of " + flow.entityRef + " (" + flow.reason + ") fall across the onset?",
		}
		if flow.hypothesisID != "" {
			fields["serves_hypothesis_id"] = flow.hypothesisID
		}
		req, err := DecodeCall("compare", jsonObject(fields), e.catalogue, e.at)
		if err != nil {
			return nil, fmt.Errorf("engine: first wave: flow on %s: %w", flow.entityRef, err)
		}
		out = append(out, req)
	}
	return out, nil
}

// openStoppedFlows opens a stopped-flow candidate for each rate comparison that fell by at least
// StoppedFlowFall, separably. It judges none of them.
func (e *Engine) openStoppedFlows(flows []*Answer) ([]string, error) {
	var opened []string
	for _, answer := range flows {
		fall, ok := stoppedFall(answer)
		if !ok {
			continue
		}
		pointer := answer.Request.GetTerm().GetCompare().GetPointer()
		entityID, inScope := e.Profile().EntityOfPointer(pointer)
		if !inScope || e.flowHypothesisFor(entityID) != "" {
			continue
		}
		ref := e.catalogue.RefFor(entityID)
		if ref == "" {
			ref = entityID
		}
		id := e.nextHypothesisID()
		h := ledger.Hypothesis{
			ID:   id,
			Kind: ledger.KindCondition,
			Statement: fmt.Sprintf("the flow into %s stopped: its request rate fell %.0f%% across the onset (%s)",
				ref, 100*fall, answer.EvidenceID),
			TargetEntityIDs: []string{entityID},
			CausalRole:      ledger.RoleCause,
		}
		// The prior is the condition prior scaled by the fall: a flow that stopped outright enters
		// like any named condition, half a flow at half its weight. The sign never lowers it
		// (FR-046b).
		if err := e.ledger.AddHypothesis(h, ConditionPrior*math.Min(1, fall)); err != nil {
			return nil, fmt.Errorf("engine: first wave: stopped flow on %s: %w", ref, err)
		}
		e.mu.Lock()
		e.flowHypotheses[id] = answer.EvidenceID
		e.mu.Unlock()
		opened = append(opened, id)
	}
	return opened, nil
}

// flowHypothesisFor is the stopped-flow hypothesis already open on an entity, if any.
func (e *Engine) flowHypothesisFor(entityID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range e.flowHypotheses {
		if h, ok := e.ledger.Hypothesis(id); ok && len(h.TargetEntityIDs) == 1 && h.TargetEntityIDs[0] == entityID {
			return id
		}
	}
	return ""
}

// stoppedFall is the relative fall a rate comparison reports, when it separates and is at least
// StoppedFlowFall.
func stoppedFall(answer *Answer) (float64, bool) {
	if answer == nil || answer.EvidenceID == "" || answer.Refused != nil ||
		answer.Response.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		return 0, false
	}
	comparisons := answer.Response.GetDigest().GetMetric().GetComparisons()
	if len(comparisons) == 0 {
		return 0, false
	}
	c := comparisons[0]
	fall := -c.GetRelativeDelta()
	if !c.GetSeparable() || c.GetDirection() != "down" || fall < StoppedFlowFall {
		return 0, false
	}
	return fall, true
}
