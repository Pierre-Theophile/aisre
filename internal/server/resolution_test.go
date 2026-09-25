// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// ResolutionService over the wire (FR-040, FR-041, FR-041a, SC-011).
//
// The projector's own tests prove what a decision does to the graph. What can only be tested
// here is the promise the *service* makes: the decider role is demanded, the authenticated
// individual is stamped on the event without the caller being able to choose it, the decision
// becomes an ordinary log entry under `source_id = "human"`, and repeating it is a no-op.

func (ts *graphServer) resolutionClient(token string) graphv1connect.ResolutionServiceClient {
	return graphv1connect.NewResolutionServiceClient(h2cClient(token), ts.baseURL)
}

func (ts *graphServer) deciderToken(t *testing.T) string {
	t.Helper()
	token, err := ts.issuer.MintFor(DevUser{Name: "alice", Roles: []Role{RoleDecider}})
	if err != nil {
		t.Fatalf("mint decider token: %v", err)
	}
	return token
}

// seedTwoServices gives the graph two entities a person can decide about, straight through the
// projector: this file is about the decision path, not about ingestion.
func seedTwoServices(t *testing.T, ts *graphServer) {
	t.Helper()
	ctx := context.Background()
	if err := ts.projector.RegisterSource(ctx, eventlog.Source{
		SourceID: "otel:demo", Kind: "otel", Ordering: "none", SchemaVersion: "1.0.0",
	}); err != nil {
		t.Fatalf("register source: %v", err)
	}
	validAt := timestamppb.New(time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC))
	for _, name := range []string{"checkout", "checkout-svc"} {
		env := &graphv1.EventEnvelope{
			EventId:        "otel:demo:node:" + name,
			IdempotencyKey: "otel:demo:node:" + name,
			SourceId:       "otel:demo",
			SchemaVersion:  "1.0.0",
			Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
				Ref:         &graphv1.Ref{Namespace: "otel.service.name", Value: name},
				Type:        graphv1.NodeType_SERVICE,
				DisplayName: name,
				ValidAt:     validAt,
			}},
		}
		result, err := ts.projector.Apply(ctx, env, time.Time{})
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if result.GetStatus() != graphv1.IngestResult_APPLIED {
			t.Fatalf("seed %s: status %s (%s)", name, result.GetStatus(), result.GetReasonDetail())
		}
	}
}

func TestConfirmStampsThePrincipalAndIsIdempotent(t *testing.T) {
	ts := newGraphServer(t)
	seedTwoServices(t, ts)

	client := ts.resolutionClient(ts.deciderToken(t))
	request := func() *connect.Request[graphv1.ConfirmMerge] {
		return connect.NewRequest(&graphv1.ConfirmMerge{
			EntityA:   "otel.service.name=checkout",
			EntityB:   "otel.service.name=checkout-svc",
			Rationale: "same service under two names",
		})
	}

	first, err := client.Confirm(context.Background(), request())
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if first.Msg.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Fatalf("status = %s (%s: %s), want APPLIED", first.Msg.GetStatus(),
			first.Msg.GetReasonCode(), first.Msg.GetReasonDetail())
	}

	// The decision is an ordinary event under the `human` source, and it names the caller.
	var (
		kind      string
		principal string
		sourceID  string
	)
	if err := ts.projector.Store().Pool().QueryRow(context.Background(), `
		SELECT d.kind, coalesce(d.principal, ''), e.source_id
		FROM graph.resolution_decisions d
		JOIN log.events e ON e.event_id = d.event_id
		WHERE d.kind = 'confirm'`).Scan(&kind, &principal, &sourceID); err != nil {
		t.Fatalf("read the decision: %v", err)
	}
	if principal != DefaultDevIssuer+"|alice" {
		t.Errorf("principal = %q, want %q (FR-041)", principal, DefaultDevIssuer+"|alice")
	}
	if sourceID != HumanSourceID {
		t.Errorf("source_id = %q, want %q", sourceID, HumanSourceID)
	}

	// The individual is also registered, so an audit can name them later.
	var registered string
	if err := ts.projector.Store().Pool().QueryRow(context.Background(),
		`SELECT principal FROM graph.principals`).Scan(&registered); err != nil {
		t.Fatalf("read the principal registry: %v", err)
	}
	if registered != principal {
		t.Errorf("graph.principals holds %q, want %q", registered, principal)
	}

	// The same person deciding the same thing again changes nothing: the event id is derived
	// from the decision (graph.HumanEventID).
	second, err := client.Confirm(context.Background(), request())
	if err != nil {
		t.Fatalf("Confirm again: %v", err)
	}
	if second.Msg.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
		t.Errorf("second status = %s, want DUPLICATE_NOOP", second.Msg.GetStatus())
	}
	if second.Msg.GetEventId() != first.Msg.GetEventId() {
		t.Errorf("event ids differ (%s vs %s); the same decision must be the same event",
			first.Msg.GetEventId(), second.Msg.GetEventId())
	}
}

func TestDecisionWithoutATokenIsUnauthenticated(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.resolutionClient("")

	_, err := client.Reject(context.Background(), connect.NewRequest(&graphv1.RejectMerge{
		EntityA: "otel.service.name=checkout", EntityB: "otel.service.name=checkout-svc",
		Rationale: "no",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %s, want unauthenticated: FR-041 forbids an anonymous decision",
			connect.CodeOf(err))
	}
}

func TestSplitOverRPCDetachesClaims(t *testing.T) {
	ts := newGraphServer(t)
	seedTwoServices(t, ts)
	client := ts.resolutionClient(ts.deciderToken(t))
	ctx := context.Background()

	if _, err := client.Confirm(ctx, connect.NewRequest(&graphv1.ConfirmMerge{
		EntityA: "otel.service.name=checkout", EntityB: "otel.service.name=checkout-svc",
		Rationale: "same service under two names",
	})); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	resp, err := client.Split(ctx, connect.NewRequest(&graphv1.SplitEntity{
		EntityId:     "otel.service.name=checkout",
		DetachClaims: []*graphv1.Ref{{Namespace: "otel.service.name", Value: "checkout-svc"}},
		Rationale:    "they are not the same after all",
	}))
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if resp.Msg.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Fatalf("status = %s (%s: %s), want APPLIED", resp.Msg.GetStatus(),
			resp.Msg.GetReasonCode(), resp.Msg.GetReasonDetail())
	}

	// The confirmation is superseded by the split, which is what lifts its pin.
	var superseded bool
	if err := ts.projector.Store().Pool().QueryRow(ctx, `
		SELECT superseded_by IS NOT NULL FROM graph.resolution_decisions
		WHERE kind = 'confirm'`).Scan(&superseded); err != nil {
		t.Fatalf("read the confirmation: %v", err)
	}
	if !superseded {
		t.Error("the confirmation is still in force after a split superseded it")
	}

	// And the detached identifier resolves to an entity of its own again.
	var same bool
	if err := ts.projector.Store().Pool().QueryRow(ctx, `
		SELECT (SELECT coalesce(merged_into, entity_id) FROM graph.entities
		        WHERE entity_id = (SELECT entity_id FROM graph.identity_claims
		                           WHERE namespace = 'otel.service.name' AND value = 'checkout-svc'))
		     = (SELECT coalesce(merged_into, entity_id) FROM graph.entities
		        WHERE entity_id = (SELECT entity_id FROM graph.identity_claims
		                           WHERE namespace = 'otel.service.name' AND value = 'checkout'))`).
		Scan(&same); err != nil {
		t.Fatalf("compare the two identifiers: %v", err)
	}
	if same {
		t.Error("the split left both identifiers resolving to one entity")
	}
}
