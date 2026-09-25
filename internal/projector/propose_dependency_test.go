// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The proposed dependency, which is not an edge (003 FR-029, FR-122).
//
// The single property everything here exists to protect: a proposal does NOT create an edge, and a
// person's decision about one outranks any number of later automated proposals. The first half is
// checked by counting edges after a proposal; the second by re-proposing after a rejection and
// asserting the rejection holds.

const proposalSource = "gcp:test"

func proposalProjector(t *testing.T) (*projector.Projector, *postgres.Store) {
	t.Helper()
	store := openStoreT(t)
	p := projector.New(store)
	if err := p.RegisterSource(context.Background(), eventlog.Source{
		SourceID: proposalSource, Kind: "gcp", Ordering: "none", SchemaVersion: "1.0.0",
	}); err != nil {
		t.Fatalf("register source: %v", err)
	}
	return p, store
}

func serviceRef(name string) *graphv1.Ref {
	return &graphv1.Ref{Namespace: "gcp.service", Value: name}
}

func sqlRef(name string) *graphv1.Ref {
	return &graphv1.Ref{Namespace: "gcp.sql", Value: name}
}

// proposalNodeEvent describes an entity, so a proposal about it resolves.
func proposalNodeEvent(id string, ref *graphv1.Ref) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId: id, IdempotencyKey: id, SourceId: proposalSource, SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref: ref, Type: graphv1.NodeType_SERVICE, DisplayName: ref.GetValue(),
			ValidAt: timestamppb.New(mustTime("2026-09-21T09:00:00Z")),
		}},
	}
}

func proposeEvent(id string, score float64, rule string) *graphv1.EventEnvelope {
	evidence, _ := structpb.NewStruct(map[string]any{
		"matched_env_var": "DATABASE_URL",
		"matched_on":      "host name appears in the service's environment",
	})
	return &graphv1.EventEnvelope{
		EventId: id, IdempotencyKey: id, SourceId: proposalSource, SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_ProposeDependency{ProposeDependency: &graphv1.ProposeDependency{
			Src: serviceRef("checkout"), Dst: sqlRef("orders-db"),
			Type: graphv1.EdgeType_DEPENDS_ON, Score: score, RuleId: rule,
			Rationale: "the service's configuration names the instance, but not unambiguously",
			Evidence:  evidence,
		}},
	}
}

func applyOne(t *testing.T, p *projector.Projector, env *graphv1.EventEnvelope, principal string) *graphv1.IngestResult {
	t.Helper()
	return applyAt(t, p, env, principal, "2026-09-21T10:00:00Z")
}

func applyAt(t *testing.T, p *projector.Projector, env *graphv1.EventEnvelope,
	principal, observedAt string,
) *graphv1.IngestResult {
	t.Helper()
	result, err := p.ApplyWithOptions(context.Background(), env, projector.ApplyOptions{
		ObservedAt: mustTime(observedAt), Principal: principal,
	})
	if err != nil {
		t.Fatalf("apply %s: %v", env.GetEventId(), err)
	}
	return result
}

// proposalState reads the row the review queue would show.
func proposalState(t *testing.T, store *postgres.Store) (status string, events []string, found bool) {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(),
		`SELECT status, proposed_by_event_ids FROM graph.proposed_dependencies`)
	if err != nil {
		t.Fatalf("read proposals: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&status, &events); err != nil {
			t.Fatalf("scan proposal: %v", err)
		}
		found = true
	}
	return status, events, found
}

func edgeCount(t *testing.T, store *postgres.Store) int {
	t.Helper()
	var n int
	// The type is spelled from the same mapping the projector writes with, rather than as a
	// literal: an earlier version of this test hard-coded "depends-on" while the column holds
	// "depends_on", so it reported zero edges against a schema that had one.
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM graph.edge_versions
		 WHERE type = $1 AND upper_inf(observed) AND upper_inf(valid)`,
		graph.EdgeTypeFromProto(graphv1.EdgeType_DEPENDS_ON).String()).
		Scan(&n); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	return n
}

func seedEndpoints(t *testing.T, p *projector.Projector) {
	t.Helper()
	applyOne(t, p, proposalNodeEvent("n1", serviceRef("checkout")), "")
	applyOne(t, p, proposalNodeEvent("n2", sqlRef("orders-db")), "")
}

func TestAProposalIsNotAnEdge(t *testing.T) {
	t.Parallel()
	p, store := proposalProjector(t)
	seedEndpoints(t, p)

	applyOne(t, p, proposeEvent("p1", 0.6, "gcp.env-var-names-instance"), "")

	status, events, found := proposalState(t, store)
	if !found {
		t.Fatal("the proposal was not filed at all")
	}
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if len(events) != 1 || events[0] != "p1" {
		t.Errorf("proposed_by_event_ids = %v, want [p1]", events)
	}
	// The property this whole file exists for.
	if n := edgeCount(t, store); n != 0 {
		t.Errorf("a proposal created %d edge(s); a proposal is never an edge until confirmed", n)
	}
}

func TestAProposalIsNotReRaisedWhilePending(t *testing.T) {
	t.Parallel()
	p, store := proposalProjector(t)
	seedEndpoints(t, p)

	// The same rule, same score, three cycles — which is what a feeder polling every minute
	// actually does. FR-029: raised once, not re-raised while pending.
	//
	// Each cycle is observed at a LATER instant, which is what makes the property checkable at
	// all. An earlier version of this test applied all three at one instant and asserted only the
	// row count and the event list — both of which hold whether or not the re-raise is suppressed,
	// because the primary key collapses the rows and the event list is appended either way. It
	// passed against a deliberately broken implementation, which is the definition of a test that
	// is not testing anything.
	applyAt(t, p, proposeEvent("p1", 0.6, "gcp.env-var-names-instance"), "", "2026-09-21T10:00:00Z")
	applyAt(t, p, proposeEvent("p2", 0.6, "gcp.env-var-names-instance"), "", "2026-09-21T10:01:00Z")
	applyAt(t, p, proposeEvent("p3", 0.6, "gcp.env-var-names-instance"), "", "2026-09-21T10:02:00Z")

	var n int
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM graph.proposed_dependencies`).Scan(&n); err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	if n != 1 {
		t.Errorf("%d proposal rows for one relationship; the queue must show it once", n)
	}

	// The observable property. created_at must stay at the FIRST suspicion: if a re-derivation
	// moved it, "pending since" would reset on every poll and a proposal nobody has looked at for
	// a month would present as new — which is precisely the review queue this exists to make
	// readable becoming unreadable.
	var createdAt time.Time
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT created_at FROM graph.proposed_dependencies`).Scan(&createdAt); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if want := mustTime("2026-09-21T10:00:00Z"); !createdAt.Equal(want) {
		t.Errorf("created_at = %s, want the first proposal's instant %s; a re-derivation moved it, "+
			"so \"pending since\" resets every cycle", createdAt, want)
	}

	// And the history grows, so "how long have we suspected this" stays answerable.
	_, events, _ := proposalState(t, store)
	if len(events) != 3 {
		t.Errorf("proposed_by_event_ids = %v, want all three events so the first suspicion keeps "+
			"its date", events)
	}

	// Now the path the early return does NOT cover: a rule raising its score is new information,
	// so the row IS refreshed — and created_at must survive that refresh too. This is the case
	// that actually exercises the ON CONFLICT clause, and without it the two guards mask each
	// other: a test that only re-derived identical content passed even with created_at added to
	// the update list, because the early return meant the update never ran.
	applyAt(t, p, proposeEvent("p4", 0.9, "gcp.env-var-names-instance"), "", "2026-09-21T10:03:00Z")

	var afterRefresh time.Time
	var score float64
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT created_at, score FROM graph.proposed_dependencies`).Scan(&afterRefresh, &score); err != nil {
		t.Fatalf("read after refresh: %v", err)
	}
	if score != 0.9 {
		t.Errorf("score = %v, want 0.9; a changed score is new information and must be recorded", score)
	}
	if want := mustTime("2026-09-21T10:00:00Z"); !afterRefresh.Equal(want) {
		t.Errorf("created_at after a refresh = %s, want the first suspicion %s; refreshing a "+
			"proposal must not reset how long it has been pending", afterRefresh, want)
	}
}

func TestConfirmingAProposalCreatesTheEdge(t *testing.T) {
	t.Parallel()
	p, store := proposalProjector(t)
	seedEndpoints(t, p)
	applyOne(t, p, proposeEvent("p1", 0.6, "gcp.env-var-names-instance"), "")

	applyOne(t, p, &graphv1.EventEnvelope{
		EventId: "c1", IdempotencyKey: "c1", SourceId: proposalSource, SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_ConfirmDependency{ConfirmDependency: &graphv1.ConfirmDependency{
			Src: serviceRef("checkout"), Dst: sqlRef("orders-db"),
			Type: graphv1.EdgeType_DEPENDS_ON, Rationale: "checked the connection string",
		}},
	}, "alice@example.com")

	if status, _, _ := proposalState(t, store); status != "confirmed" {
		t.Errorf("status = %q, want confirmed", status)
	}
	if n := edgeCount(t, store); n != 1 {
		t.Errorf("confirming produced %d edge(s), want 1 — confirmation is the one path that "+
			"creates one", n)
	}

	// And the decision names who took it (FR-041), which the schema also refuses to omit.
	var decidedBy string
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT decided_by FROM graph.proposed_dependencies`).Scan(&decidedBy); err != nil {
		t.Fatalf("read decided_by: %v", err)
	}
	if decidedBy != "alice@example.com" {
		t.Errorf("decided_by = %q, want the authenticated principal", decidedBy)
	}
}

func TestARejectedProposalIsNeverReopenedByARule(t *testing.T) {
	t.Parallel()
	p, store := proposalProjector(t)
	seedEndpoints(t, p)
	applyOne(t, p, proposeEvent("p1", 0.6, "gcp.env-var-names-instance"), "")

	applyOne(t, p, &graphv1.EventEnvelope{
		EventId: "r1", IdempotencyKey: "r1", SourceId: proposalSource, SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_RejectDependency{RejectDependency: &graphv1.RejectDependency{
			Src: serviceRef("checkout"), Dst: sqlRef("orders-db"),
			Type: graphv1.EdgeType_DEPENDS_ON, Rationale: "that variable is a read replica, not this instance",
		}},
	}, "alice@example.com")

	if status, _, _ := proposalState(t, store); status != "rejected" {
		t.Errorf("status after rejection = %q, want rejected", status)
	}

	// FR-122. A rule that goes on proposing what a person rejected records a disagreement — it
	// does NOT reopen the question, and a higher score buys it nothing. Reopening would let a rule
	// outvote a person by simply running again.
	applyOne(t, p, proposeEvent("p2", 0.99, "gcp.env-var-names-instance"), "")

	status, _, _ := proposalState(t, store)
	if status == "pending" {
		t.Error("a later automated proposal reopened a rejected dependency; FR-122 says a human " +
			"decision outranks any automated match regardless of score")
	}
	if status != "conflict" {
		t.Errorf("status = %q, want conflict — the rule's disagreement is recorded, not applied", status)
	}
	if n := edgeCount(t, store); n != 0 {
		t.Errorf("a rejected dependency produced %d edge(s)", n)
	}
}

func TestADependencyDecisionNeedsAPrincipalAndAProposal(t *testing.T) {
	t.Parallel()
	p, _ := proposalProjector(t)
	seedEndpoints(t, p)

	confirm := func(id string) *graphv1.EventEnvelope {
		return &graphv1.EventEnvelope{
			EventId: id, IdempotencyKey: id, SourceId: proposalSource, SchemaVersion: "1.0.0",
			Body: &graphv1.EventEnvelope_ConfirmDependency{ConfirmDependency: &graphv1.ConfirmDependency{
				Src: serviceRef("checkout"), Dst: sqlRef("orders-db"), Type: graphv1.EdgeType_DEPENDS_ON,
			}},
		}
	}

	// No proposal yet: a decision may not invent the proposal it decides.
	result := applyOne(t, p, confirm("c0"), "alice@example.com")
	if result.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Errorf("confirming an unproposed dependency was %v, want REJECTED", result.GetStatus())
	}

	applyOne(t, p, proposeEvent("p1", 0.6, "gcp.env-var-names-instance"), "")

	// No principal: FR-041.
	result = applyOne(t, p, confirm("c1"), "")
	if result.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Errorf("confirming with no principal was %v, want REJECTED", result.GetStatus())
	}
	if result.GetReasonCode() != eventlog.ReasonMissingPrincipal {
		t.Errorf("reason = %q, want %q", result.GetReasonCode(), eventlog.ReasonMissingPrincipal)
	}
}

func TestAProposalMayNotCreateItsEndpoints(t *testing.T) {
	t.Parallel()
	p, store := proposalProjector(t)
	// Deliberately no seedEndpoints: nothing has described either entity.

	result := applyOne(t, p, proposeEvent("p1", 0.6, "gcp.env-var-names-instance"), "")
	if result.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Errorf("a proposal about unknown entities was %v, want REJECTED — minting a placeholder "+
			"would put a phantom in the review queue", result.GetStatus())
	}
	if result.GetReasonCode() != eventlog.ReasonRefUnresolvable {
		t.Errorf("reason = %q, want %q", result.GetReasonCode(), eventlog.ReasonRefUnresolvable)
	}
	if _, _, found := proposalState(t, store); found {
		t.Error("a refused proposal was filed anyway")
	}
}

// TestTheEvidenceIsStoredReadably guards the shape of the one column a reviewer actually reads.
//
// A structpb.Struct marshalled with encoding/json comes out as its wire representation —
// {"fields":{"k":{"Kind":{"StringValue":"v"}}}} — which would be committed, golden, and unreadable
// by the person the proposal exists for.
func TestTheEvidenceIsStoredReadably(t *testing.T) {
	t.Parallel()
	p, store := proposalProjector(t)
	seedEndpoints(t, p)
	applyOne(t, p, proposeEvent("p1", 0.6, "gcp.env-var-names-instance"), "")

	var evidence map[string]any
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT evidence FROM graph.proposed_dependencies`).Scan(&evidence); err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	if got := evidence["matched_env_var"]; got != "DATABASE_URL" {
		t.Errorf("evidence[matched_env_var] = %#v, want %q; the struct was stored in its wire "+
			"form rather than as the object it represents", got, "DATABASE_URL")
	}
}
