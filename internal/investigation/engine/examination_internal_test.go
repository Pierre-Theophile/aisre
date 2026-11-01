// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// TestAnUnqueriedDecisiveFamilyIsAnOmissionAndAnUnreachableOneIsAGap is FR-022b on a hand-built
// run: a rollout on payments whose metrics were never asked (an omission, since payments has a
// metric pointer), a config change on a target with no pointer (a coverage gap, never an
// omission), and the subject, touched by a compare on its own metric.
func TestAnUnqueriedDecisiveFamilyIsAnOmissionAndAnUnreachableOneIsAGap(t *testing.T) {
	t.Parallel()

	checkoutMetric := &graphv1.Pointer{Kind: graphv1.PointerKind_METRIC, Selector: "checkout-rate"}
	paymentsMetric := &graphv1.Pointer{Kind: graphv1.PointerKind_METRIC, Selector: "payments-rate"}
	node := func(id, ref string, typ graphv1.NodeType, ptrs ...*graphv1.Pointer) *graphv1.NodeVersion {
		return &graphv1.NodeVersion{EntityId: id, Type: typ, Pointers: ptrs,
			Aliases: []*graphv1.Ref{{Namespace: "otel.service.name", Value: ref}}}
	}
	now := time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	profile := BuildProfile(&graphv1.SubgraphResponse{
		Focus: node("e-checkout", "checkout", graphv1.NodeType_SERVICE, checkoutMetric),
		Nodes: []*graphv1.NodeVersion{
			node("e-payments", "payments", graphv1.NodeType_SERVICE, paymentsMetric),
			node("e-config", "config", graphv1.NodeType_CONFIG),
		},
	}, now.Add(-time.Hour), now, now)

	book, err := ledger.New("inv-x", audit.PriorRecord{Prior: 0.4, AuditID: "a", Ceiling: 0.6, IncidentCount: 13})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []ledger.Hypothesis{
		{ID: "h-1", Kind: ledger.KindChange, Statement: "rollout", CandidateChangeEntityID: "c-rollout",
			TargetEntityIDs: []string{"e-payments"}, CausalRole: ledger.RoleCause},
		{ID: "h-2", Kind: ledger.KindChange, Statement: "config", CandidateChangeEntityID: "c-config",
			TargetEntityIDs: []string{"e-config"}, CausalRole: ledger.RoleCause},
	} {
		if err := book.AddHypothesis(h, 0.3); err != nil {
			t.Fatal(err)
		}
	}
	if err := book.AddEvidence(ledger.EvidenceItem{
		ID: "e-1", Kind: "algebra_answer", Worker: "metrics", Capability: "compare", Outcome: "ok",
		Coverage: &investigationv1.Coverage{}, DeepLinkAbsentReason: "recorded",
		Term: &investigationv1.AlgebraTerm{Term: &investigationv1.AlgebraTerm_Compare{
			Compare: &investigationv1.CompareTerm{Pointer: checkoutMetric}}},
	}); err != nil {
		t.Fatal(err)
	}

	e := &Engine{ledger: book, catalogue: NewCatalogue(), profile: profile,
		changeKinds: map[string]graphv1.ChangeKind{
			"c-rollout": graphv1.ChangeKind_ROLLOUT, "c-config": graphv1.ChangeKind_CONFIG_CHANGE}}
	x := e.Examination()

	byID := map[string]HypothesisExamination{}
	for _, h := range x.Hypotheses {
		byID[h.HypothesisID] = h
	}
	if h := byID["h-1"]; h.Category != "rollout" || len(h.Omissions) != 1 || h.Omissions[0] != "metrics on otel.service.name=payments" {
		t.Errorf("h-1 = %+v, want the payments metrics named as an omission", h)
	}
	if h := byID["h-2"]; h.Category != "config_change" || len(h.Omissions) != 0 || len(h.Gaps) != 2 {
		t.Errorf("h-2 = %+v, want two coverage gaps and no omission", h)
	}
	if x.Omissions != 1 {
		t.Errorf("omissions = %d, want 1", x.Omissions)
	}
	if len(x.Untouched) != 2 || x.Untouched[0] != "otel.service.name=config" {
		t.Errorf("untouched = %v, want config and payments, not the subject a compare touched", x.Untouched)
	}
	if x.Proto().GetExpectationTable() != "v1 (observe)" {
		t.Errorf("table = %q", x.Proto().GetExpectationTable())
	}
}
