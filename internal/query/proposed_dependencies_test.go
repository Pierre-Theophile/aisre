// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// The proposed-dependency review queue (003 FR-029, FR-122).
//
// What is worth testing here, as opposed to in the projector: the projector's tests prove a
// proposal is filed and never becomes an edge on its own. These prove a person can FIND it — which
// is the other half of the requirement, because a proposal nobody can reach is a proposal nobody
// will decide, and before this listing existed the only way to see one was to read the event log.

func propose(b *builder, src, dst string, score float64, rule string) {
	b.t.Helper()
	evidence, err := structpb.NewStruct(map[string]any{"matched_env_var": "DATABASE_URL"})
	if err != nil {
		b.t.Fatalf("evidence: %v", err)
	}
	b.apply(&graphv1.ProposeDependency{
		Src:       &graphv1.Ref{Namespace: testNS, Value: src},
		Dst:       &graphv1.Ref{Namespace: testNS, Value: dst},
		Type:      graphv1.EdgeType_DEPENDS_ON,
		Score:     score,
		RuleId:    rule,
		Rationale: "the service's configuration names the instance, but not unambiguously",
		Evidence:  evidence,
	}, time.Time{})
}

func TestProposedDependenciesAreListedStrongestFirstWithTheirDirection(t *testing.T) {
	t.Parallel()
	b := newBuilder(t)
	for _, n := range []string{"checkout", "orders-db", "search", "cache"} {
		b.node(n)
	}
	propose(b, "checkout", "orders-db", 0.4, "gcp.env-var-names-instance")
	propose(b, "search", "cache", 0.8, "gcp.env-var-names-instance")

	resp, err := b.engine().ProposedDependencies(context.Background(),
		&graphv1.ProposedDependenciesRequest{})
	if err != nil {
		t.Fatalf("list proposals: %v", err)
	}
	if len(resp.GetProposals()) != 2 {
		t.Fatalf("got %d proposals, want 2", len(resp.GetProposals()))
	}

	// Strongest first, because that is the order a person should spend attention in.
	if got := resp.GetProposals()[0].GetScore(); got != 0.8 {
		t.Errorf("first proposal score = %v, want the strongest (0.8)", got)
	}

	first := resp.GetProposals()[0]
	// The direction is the claim. A listing that lost it would ask a reviewer "are these two
	// related?", which is not the question — which end depends on which is the whole of it.
	if first.GetSrc().GetEntityId() == first.GetDst().GetEntityId() {
		t.Fatal("both ends resolved to one entity")
	}
	if name := suggestionSideName(first.GetSrc()); name != "search" {
		t.Errorf("src = %q, want search (the dependent)", name)
	}
	if name := suggestionSideName(first.GetDst()); name != "cache" {
		t.Errorf("dst = %q, want cache (the dependency)", name)
	}
	if first.GetType() != graphv1.EdgeType_DEPENDS_ON {
		t.Errorf("type = %v, want DEPENDS_ON", first.GetType())
	}
	if first.GetStatus() != "pending" {
		t.Errorf("status = %q, want pending", first.GetStatus())
	}

	// The evidence has to reach the reviewer: they are being asked about a relationship visible
	// from neither end, so the score alone is not something a person can act on.
	if got := first.GetEvidence().AsMap()["matched_env_var"]; got != "DATABASE_URL" {
		t.Errorf("evidence did not survive the round trip: %#v", first.GetEvidence().AsMap())
	}
	if len(first.GetProposedByEventIds()) == 0 {
		t.Error("the proposal names no event; its provenance is how a reader gets back to the rule")
	}
}

func TestProposalsFilterByStatusAndFocus(t *testing.T) {
	t.Parallel()
	b := newBuilder(t)
	for _, n := range []string{"checkout", "orders-db", "search", "cache"} {
		b.node(n)
	}
	propose(b, "checkout", "orders-db", 0.4, "r1")
	propose(b, "search", "cache", 0.8, "r1")

	engine := b.engine()

	// Focus matches at EITHER end, because "what has been proposed about this thing" is the
	// question a reviewer looking at one service actually has.
	for _, focus := range []string{"checkout", "orders-db"} {
		resp, err := engine.ProposedDependencies(context.Background(),
			&graphv1.ProposedDependenciesRequest{
				Focus: &graphv1.Ref{Namespace: testNS, Value: focus},
			})
		if err != nil {
			t.Fatalf("list for %s: %v", focus, err)
		}
		if len(resp.GetProposals()) != 1 {
			t.Errorf("--for %s returned %d proposals, want 1", focus, len(resp.GetProposals()))
		}
	}

	// Nothing has been decided, so a confirmed filter is empty rather than a listing of everything.
	resp, err := engine.ProposedDependencies(context.Background(),
		&graphv1.ProposedDependenciesRequest{Status: "confirmed"})
	if err != nil {
		t.Fatalf("list confirmed: %v", err)
	}
	if len(resp.GetProposals()) != 0 {
		t.Errorf("--status confirmed returned %d proposals, want none", len(resp.GetProposals()))
	}

	// And a status outside the published set is refused rather than silently matching nothing,
	// which would be indistinguishable from an empty queue.
	if _, err := engine.ProposedDependencies(context.Background(),
		&graphv1.ProposedDependenciesRequest{Status: "probably"}); err == nil {
		t.Error("an unpublished status was accepted; an empty result would read as an empty queue")
	}
}

// suggestionSideName names an end the way the CLI does, so the test asserts what a reviewer sees.
func suggestionSideName(node *graphv1.NodeVersion) string {
	if aliases := node.GetAliases(); len(aliases) > 0 {
		return aliases[0].GetValue()
	}
	return node.GetDisplayName()
}
