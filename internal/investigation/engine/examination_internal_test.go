// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/expectations"
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

// TestAnEnforcedTableHoldsASupportedHypothesisWithAnOmission is FR-022b's `enforce` mode (T132):
// a rollout supported while its target's metrics were reachable and never asked is held at
// inconclusive, the omission is its resolving action, and the run does not conclude `unobserved`
// over it. The same run under the published `observe` table keeps it supported.
func TestAnEnforcedTableHoldsASupportedHypothesisWithAnOmission(t *testing.T) {
	t.Parallel()

	build := func(table *expectations.Table) *Engine {
		paymentsMetric := &graphv1.Pointer{Kind: graphv1.PointerKind_METRIC, Selector: "payments-rate"}
		now := time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
		profile := BuildProfile(&graphv1.SubgraphResponse{
			Focus: &graphv1.NodeVersion{EntityId: "e-payments", Type: graphv1.NodeType_SERVICE,
				Pointers: []*graphv1.Pointer{paymentsMetric},
				Aliases:  []*graphv1.Ref{{Namespace: "otel.service.name", Value: "payments"}}},
		}, now.Add(-time.Hour), now, now)
		book, err := ledger.New("inv-x", audit.PriorRecord{Prior: 0.4, AuditID: "a", Ceiling: 0.6, IncidentCount: 13})
		if err != nil {
			t.Fatal(err)
		}
		if err := book.AddHypothesis(ledger.Hypothesis{ID: "h-1", Kind: ledger.KindChange, Statement: "rollout",
			CandidateChangeEntityID: "c-rollout", TargetEntityIDs: []string{"e-payments"},
			CausalRole: ledger.RoleCause}, 0.3); err != nil {
			t.Fatal(err)
		}
		// A person said so: support that needs no telemetry, so the metrics stay unasked.
		if err := book.AddEvidence(ledger.EvidenceItem{
			ID: "e-1", Kind: "human_fact", Worker: "human", Outcome: "ok", DeepLinkAbsentReason: "stated",
			Coverage: &investigationv1.Coverage{},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := book.Judge(ledger.Judgment{ID: "j-1", HypothesisID: "h-1", EvidenceID: "e-1",
			Direction: ledger.Supports, Strength: ledger.Strong, Source: ledger.SourceHumanFact}); err != nil {
			t.Fatal(err)
		}
		if err := book.SetStatus("h-1", ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
			t.Fatal(err)
		}
		return &Engine{ledger: book, catalogue: NewCatalogue(), profile: profile, expectations: table,
			changeKinds: map[string]graphv1.ChangeKind{"c-rollout": graphv1.ChangeKind_ROLLOUT}}
	}

	observed := build(nil)
	if err := observed.concludeVerdict(Stop{Reason: StopCompleted}); err != nil {
		t.Fatal(err)
	}
	if h, _ := observed.ledger.Hypothesis("h-1"); h.Status != ledger.StatusSupported {
		t.Errorf("under the observe table h-1 is %s, want supported: observe holds nothing", h.Status)
	}

	enforced := *expectations.Published()
	enforced.Mode = expectations.ModeEnforce
	e := build(&enforced)
	if err := e.concludeVerdict(Stop{Reason: StopCompleted}); err != nil {
		t.Fatal(err)
	}
	if h, _ := e.ledger.Hypothesis("h-1"); h.Status != ledger.StatusInconclusive {
		t.Errorf("under the enforced table h-1 is %s, want inconclusive", h.Status)
	}
	if open, _ := e.ledger.Hypothesis(e.ledger.OpenHypothesisID()); open.Status == ledger.StatusSupported {
		t.Error("the run concluded `unobserved` over a held hypothesis")
	}
	x := e.Examination()
	if len(x.Hypotheses) == 0 || !x.Hypotheses[0].Held {
		t.Fatalf("the examination does not say h-1 was held: %+v", x.Hypotheses)
	}
	if !x.Proto().GetHypotheses()[0].GetHeld() || x.Proto().GetExpectationTable() != "v1 (enforce)" {
		t.Errorf("the published report = %v", x.Proto())
	}
	if want := "to resolve, query: metrics on otel.service.name=payments"; !strings.Contains(x.Render(), want) {
		t.Errorf("the rendering does not name the resolving action %q:\n%s", want, x.Render())
	}
}
