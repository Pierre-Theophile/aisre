// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1/investigationv1connect"
)

// What the `investigator` role can reach (T116, FR-008, FR-066, plan F10, constitution VII).
//
// `TestRoleMapping` says which investigation RPC each role may call. The question this file
// answers is the other one, the one a security review asks: having granted `investigator`, what
// *else* has been granted? The role is new, it implies `reader`, and it is the credential an
// on-call carries all day — so the blast radius of holding one is worth a test rather than a
// paragraph.
//
// The answer, asserted below: read on the graph, write into schema `investigation` through the
// investigation RPCs, and nothing else. Not ingestion — that is a feeder's, scoped to one source.
// Not entity resolution — that is a decider's. And not the report sink: no served RPC delivers a
// report at all, which is why the delivery credential lives at the CLI and never on the server.

// TestInvestigatorRoleReachesNothingBeyondItsGrant walks every other service on the wire with an
// investigator token.
func TestInvestigatorRoleReachesNothingBeyondItsGrant(t *testing.T) {
	ts := newInvestigationServer(t)
	ctx := context.Background()
	token := ts.token(t, "ivy", RoleInvestigator)

	// Read on the graph: granted, because an investigation is an elaborate read of it.
	if _, err := ts.queryClient(token).Extent(ctx, connect.NewRequest(&graphv1.ExtentRequest{})); isAuthFailure(err) {
		t.Errorf("an investigator was refused a graph read: %v", err)
	}

	// Ingestion: refused. A feeder token is scoped to one source (FR-046); an investigator has
	// no source and no business appending observations.
	ingest := ts.ingestClient(token)
	events := fixtureEvents(t, testSourceID, 1)
	if _, err := ingest.IngestBatch(ctx, connect.NewRequest(&graphv1.IngestBatchRequest{Events: events})); !isAuthFailure(err) {
		t.Errorf("an investigator token appended events (err = %v); ingestion is a feeder's", err)
	}
	if _, err := ingest.RegisterSource(ctx, connect.NewRequest(&graphv1.RegisterSourceRequest{
		SourceId: "otel:investigator", Kind: "otel",
	})); !isAuthFailure(err) {
		t.Errorf("an investigator token registered a source (err = %v)", err)
	}
	stream := ingest.Ingest(ctx)
	// The interceptor refuses before the first message is read, so the failure surfaces on
	// either the send or the receive depending on how much of the stream got out.
	sendErr := stream.Send(events[0])
	_ = stream.CloseRequest()
	_, recvErr := stream.Receive()
	_ = stream.CloseResponse()
	if !isAuthFailure(recvErr) && !isAuthFailure(sendErr) {
		t.Errorf("an investigator token opened an ingestion stream (send = %v, recv = %v)", sendErr, recvErr)
	}

	// Entity resolution: refused. Deciding what two entities are is a different authority, and
	// auth.go says so in as many words: RoleInvestigator is deliberately not RoleDecider.
	resolution := graphv1connect.NewResolutionServiceClient(h2cClient(token), ts.baseURL, connect.WithGRPC())
	for name, call := range map[string]func() error{
		"Confirm": func() error {
			_, err := resolution.Confirm(ctx, connect.NewRequest(&graphv1.ConfirmMerge{
				EntityA: "a", EntityB: "b", Rationale: "same service"}))
			return err
		},
		"Reject": func() error {
			_, err := resolution.Reject(ctx, connect.NewRequest(&graphv1.RejectMerge{
				EntityA: "a", EntityB: "b", Rationale: "different services"}))
			return err
		},
		"Split": func() error {
			_, err := resolution.Split(ctx, connect.NewRequest(&graphv1.SplitEntity{
				EntityId: "a", Rationale: "two things"}))
			return err
		},
		"Merge": func() error {
			_, err := resolution.Merge(ctx, connect.NewRequest(&graphv1.ManualMerge{
				EntityA: "a", EntityB: "b", Rationale: "same service"}))
			return err
		},
	} {
		if err := call(); !isAuthFailure(err) {
			t.Errorf("an investigator token recorded a resolution decision through %s (err = %v); "+
				"that is a decider's authority", name, err)
		}
	}
}

// TestNoServedRPCDeliversAReport is the served surface of the write path: there isn't one.
//
// Report delivery is a CLI path (`investigate report --deliver`) holding a credential scoped to
// one report. If an RPC ever delivers, the server process would hold that credential and the
// separation the constitution requires would have to be re-argued — so the method set of the
// served service is pinned here, and adding to it is a deliberate act with a test to update.
func TestNoServedRPCDeliversAReport(t *testing.T) {
	t.Parallel()

	published := []string{
		"Declare", "Export", "Get", "Investigate", "Label", "List", "Reopen", "Replay",
		"Review", "SubmitHumanFact",
	}
	handler := reflect.TypeOf((*investigationv1connect.InvestigationServiceHandler)(nil)).Elem()
	var got []string
	for i := range handler.NumMethod() {
		got = append(got, handler.Method(i).Name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, published) {
		t.Errorf("the served investigation surface is %v, published %v. A new RPC needs a role in "+
			"the mapping and a line in docs/security/report-delivery-review-2026-09-18.md", got, published)
	}
	for _, name := range got {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "deliver") || strings.Contains(lower, "post") ||
			strings.Contains(lower, "publish") {
			t.Errorf("RPC %s looks like a write outside the tool's own stores; report delivery is "+
				"confined to the CLI path with its own scoped credential (constitution VII)", name)
		}
	}
	// And the wire types carry no credential field, so a caller cannot hand the server one.
	req := reflect.TypeOf(investigationv1.ExportRequest{})
	for i := range req.NumField() {
		if n := strings.ToLower(req.Field(i).Name); strings.Contains(n, "token") ||
			strings.Contains(n, "secret") || strings.Contains(n, "credential") {
			t.Errorf("ExportRequest carries a credential field %q", req.Field(i).Name)
		}
	}
}
