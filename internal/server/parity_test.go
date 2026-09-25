// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// gRPC / JSON parity, for every RPC (T087, FR-035, plan.md §Testing, research §16).
//
// FR-035 promises that "the same procedure answers gRPC and a plain JSON POST". ConnectRPC
// makes that true by construction, which is exactly why it needs a test: a promise that holds
// because of a library is a promise that breaks silently when a handler starts doing its own
// serialization, adds an interceptor that touches the body, or returns a type the JSON codec
// renders differently.
//
// So every RPC of QueryService and ResolutionService is called twice — once through the
// generated Connect client over gRPC, once with `curl`-shaped `POST /<service>/<method>` and
// `Content-Type: application/json` — and the two answers are compared after canonical
// serialization. Canonical rather than byte-for-byte: protojson deliberately randomizes its
// whitespace, so comparing raw bytes would test the encoder's mood rather than the contract.
//
// # Why two fixtures, in two graphs
//
// `baseline-topology-01` is a recorded cluster and is what the topology reads have something to
// say about: subgraph, diff, impact, pointers, history, extent. `ambiguous-identity-01` is the
// only fixture that leaves a pending suggestion and a resolution audit trail behind, so it is
// what Suggestions and ResolutionAudit are asked about.
//
// They get a graph each. The two recordings name some of the same services — both have a
// `checkout` and a `payments` — at instants two weeks apart, so loading them into one database
// would be one source correcting another's history rather than two independent fixtures.
//
// # Why the decision RPCs get their own databases
//
// Confirm, Reject, Split and Merge are not reads: calling one twice against one graph would
// compare a first decision with a duplicate no-op. Each is therefore run against a *pair* of
// freshly loaded graphs — gRPC into one, JSON into the other — which is a fair comparison
// because the event id of a human decision is derived from the decision itself (resolution.go
// §"Deterministic event ids"): the same person, pair and rationale produce the same event id in
// both databases, so an identical IngestResult is exactly what the contract requires.

const (
	parityBaselineDir  = "../../fixtures/baseline-topology-01"
	parityAmbiguousDir = "../../fixtures/ambiguous-identity-01"
	// parityPrincipal is the principal key the dev provider mints for `alice`, and the one the
	// ambiguous fixture's recorded decisions are credited to.
	parityPrincipal = "sre-agent-dev|alice"
)

// Instants inside each fixture's clock at which its graph is populated.
func parityBaselineAt() time.Time  { return mustParseTime("2026-09-16T09:51:22Z") }
func parityAmbiguousAt() time.Time { return mustParseTime("2026-09-01T15:05:00Z") }

func mustParseTime(s string) time.Time {
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return parsed
}

func ref(namespace, value string) *graphv1.Ref {
	return graph.Ref{Namespace: namespace, Value: value}.Proto()
}

// newGraphWithFixture is a running server with one fixture replayed into it.
func newGraphWithFixture(t *testing.T, dir string) *graphServer {
	t.Helper()
	ts := newGraphServer(t)
	report, err := fixture.Load(context.Background(), ts.projector, dir,
		fixture.LoadOptions{Principal: parityPrincipal})
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	if report.Applied == 0 {
		t.Fatalf("load %s applied no events", dir)
	}
	return ts
}

// postJSON calls one procedure the way `curl` does: an ordinary HTTP POST with a JSON body.
//
// This is the half of FR-035 that has no generated client, so it is deliberately written
// without one: no Connect client, no codec, just the URL, the content type and the bytes.
func postJSON(t *testing.T, ts *graphServer, token, procedure string, req proto.Message, out proto.Message) {
	t.Helper()

	body, err := protojson.Marshal(req)
	if err != nil {
		t.Fatalf("marshal %s request: %v", procedure, err)
	}
	url := ts.baseURL + procedure
	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build %s request: %v", procedure, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", procedure, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d: %s", url, resp.StatusCode, raw)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal %s response %s: %v", procedure, raw, err)
	}
}

// assertParity compares the two answers after canonical serialization.
func assertParity(t *testing.T, name string, viaGRPC, viaJSON proto.Message) {
	t.Helper()
	grpcJSON, err := graph.CanonicalJSON(viaGRPC)
	if err != nil {
		t.Fatalf("%s: canonicalize gRPC response: %v", name, err)
	}
	jsonJSON, err := graph.CanonicalJSON(viaJSON)
	if err != nil {
		t.Fatalf("%s: canonicalize JSON response: %v", name, err)
	}
	if !bytes.Equal(grpcJSON, jsonJSON) {
		t.Errorf("%s: gRPC and JSON answers differ\n  gRPC: %s\n  JSON: %s",
			name, indent(grpcJSON), indent(jsonJSON))
	}
	if len(grpcJSON) == 0 || string(grpcJSON) == "{}" {
		t.Errorf("%s: both answers are empty; the comparison proves nothing", name)
	}
}

// indent re-renders canonical JSON readably, for a failure message only.
func indent(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "    ", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// ---------------------------------------------------------------------------
// QueryService
// ---------------------------------------------------------------------------

func TestQueryServiceGRPCAndJSONAgree(t *testing.T) {
	ctx := context.Background()

	ts := newGraphWithFixture(t, parityBaselineDir)
	token := ts.readerToken(t)
	client := ts.queryClient(token)

	// The resolution reads are asked of the ambiguous-identity graph, which is the only one
	// that has a suggestion and an audit trail to answer with.
	rts := newGraphWithFixture(t, parityAmbiguousDir)
	rtoken := rts.readerToken(t)
	rclient := rts.queryClient(rtoken)

	baselineAsOf := &graphv1.AsOf{ValidAt: timestamppb.New(parityBaselineAt())}

	subgraphReq := &graphv1.SubgraphRequest{
		Focus: ref("otel.service.name", "checkout"), AsOf: baselineAsOf,
		Hops: 2, Direction: graphv1.Direction_BOTH,
	}
	diffReq := &graphv1.DiffRequest{
		Subgraph: &graphv1.SubgraphRequest{
			Focus: ref("otel.service.name", "checkout"), AsOf: baselineAsOf,
			Hops: 2, Direction: graphv1.Direction_BOTH,
		},
		T1: timestamppb.New(mustParseTime("2026-09-16T09:38:31Z")),
		T2: timestamppb.New(parityBaselineAt()),
	}
	impactReq := &graphv1.ImpactRequest{
		Focus: ref("otel.service.name", "payments"), AsOf: baselineAsOf,
	}
	pointersReq := &graphv1.PointersRequest{
		Focus: ref("otel.service.name", "payments"), AsOf: baselineAsOf,
	}
	auditReq := &graphv1.ResolutionAuditRequest{
		A:          ref("otel.service.name", "checkout"),
		B:          ref("k8s.deployment", "shop/checkout-svc"),
		ObservedAt: timestamppb.New(parityAmbiguousAt()),
	}
	historyReq := &graphv1.NodeHistoryRequest{Focus: ref("otel.service.name", "checkout")}
	suggestionsReq := &graphv1.SuggestionsRequest{Status: "pending"}
	extentReq := &graphv1.ExtentRequest{}

	cases := []parityCase{
		{
			name: "Subgraph", procedure: procedure("QueryService", "Subgraph"),
			req: subgraphReq, out: &graphv1.SubgraphResponse{},
			call: func() (proto.Message, error) {
				resp, err := client.Subgraph(ctx, connect.NewRequest(subgraphReq))
				return msgOrErr(resp, err)
			},
		},
		{
			name: "Diff", procedure: procedure("QueryService", "Diff"),
			req: diffReq, out: &graphv1.DiffResponse{},
			call: func() (proto.Message, error) {
				resp, err := client.Diff(ctx, connect.NewRequest(diffReq))
				return msgOrErr(resp, err)
			},
		},
		{
			name: "Impact", procedure: procedure("QueryService", "Impact"),
			req: impactReq, out: &graphv1.ImpactResponse{},
			call: func() (proto.Message, error) {
				resp, err := client.Impact(ctx, connect.NewRequest(impactReq))
				return msgOrErr(resp, err)
			},
		},
		{
			name: "Pointers", procedure: procedure("QueryService", "Pointers"),
			req: pointersReq, out: &graphv1.PointersResponse{},
			call: func() (proto.Message, error) {
				resp, err := client.Pointers(ctx, connect.NewRequest(pointersReq))
				return msgOrErr(resp, err)
			},
		},
		{
			name: "ResolutionAudit", procedure: procedure("QueryService", "ResolutionAudit"),
			req: auditReq, out: &graphv1.ResolutionAuditResponse{},
			server: rts, token: rtoken,
			call: func() (proto.Message, error) {
				resp, err := rclient.ResolutionAudit(ctx, connect.NewRequest(auditReq))
				return msgOrErr(resp, err)
			},
		},
		{
			name: "NodeHistory", procedure: procedure("QueryService", "NodeHistory"),
			req: historyReq, out: &graphv1.NodeHistoryResponse{},
			call: func() (proto.Message, error) {
				resp, err := client.NodeHistory(ctx, connect.NewRequest(historyReq))
				return msgOrErr(resp, err)
			},
		},
		{
			name: "Suggestions", procedure: procedure("QueryService", "Suggestions"),
			req: suggestionsReq, out: &graphv1.SuggestionsResponse{},
			server: rts, token: rtoken,
			call: func() (proto.Message, error) {
				resp, err := rclient.Suggestions(ctx, connect.NewRequest(suggestionsReq))
				return msgOrErr(resp, err)
			},
		},
		{
			name: "Extent", procedure: procedure("QueryService", "Extent"),
			req: extentReq, out: &graphv1.Extent{},
			call: func() (proto.Message, error) {
				resp, err := client.Extent(ctx, connect.NewRequest(extentReq))
				return msgOrErr(resp, err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			viaGRPC, err := tc.call()
			if err != nil {
				t.Fatalf("%s over gRPC: %v", tc.name, err)
			}
			server, bearer := ts, token
			if tc.server != nil {
				server, bearer = tc.server, tc.token
			}
			postJSON(t, server, bearer, tc.procedure, tc.req, tc.out)
			assertParity(t, tc.name, viaGRPC, tc.out)
		})
	}
}

// parityCase is one RPC measured both ways.
type parityCase struct {
	name      string
	procedure string
	// call runs the RPC through the generated Connect client over gRPC.
	call func() (proto.Message, error)
	// req is the same request for the JSON half; out receives its answer.
	req proto.Message
	out proto.Message
	// server and token override the graph the JSON half is posted to, for the RPCs that are
	// asked of the ambiguous-identity fixture rather than the baseline one.
	server *graphServer
	token  string
}

// msgOrErr unwraps a Connect response into the protobuf message it carries.
//
// The constraint is spelled with the pointer-receiver idiom because generated message types
// implement proto.Message on *T, and connect.Response is parameterized by T.
func msgOrErr[T any, PT interface {
	*T
	proto.Message
}](resp *connect.Response[T], err error) (proto.Message, error) {
	if err != nil {
		return nil, err
	}
	return PT(resp.Msg), nil
}

// procedure is the HTTP path of one RPC, which is what a JSON caller posts to.
func procedure(service, method string) string {
	return "/sreagent.graph.v1." + service + "/" + method
}

// ---------------------------------------------------------------------------
// ResolutionService
// ---------------------------------------------------------------------------

// TestResolutionServiceGRPCAndJSONAgree runs each decision RPC against a fresh pair of graphs.
func TestResolutionServiceGRPCAndJSONAgree(t *testing.T) {
	cases := []struct {
		name      string
		procedure string
		req       proto.Message
		// grpc calls the RPC on one server; the JSON half posts the same request to the other.
		grpc func(ctx context.Context, ts *graphServer, token string) (proto.Message, error)
	}{
		{
			name:      "Confirm",
			procedure: procedure("ResolutionService", "Confirm"),
			req: &graphv1.ConfirmMerge{
				EntityA:   "otel.service.name=notifications",
				EntityB:   "k8s.deployment=ops/notifications-svc",
				Rationale: "parity test: the deployment runs the service",
			},
			grpc: func(ctx context.Context, ts *graphServer, token string) (proto.Message, error) {
				resp, err := ts.resolutionGRPCClient(token).Confirm(ctx, connect.NewRequest(
					&graphv1.ConfirmMerge{
						EntityA:   "otel.service.name=notifications",
						EntityB:   "k8s.deployment=ops/notifications-svc",
						Rationale: "parity test: the deployment runs the service",
					}))
				return msgOrErr(resp, err)
			},
		},
		{
			name:      "Reject",
			procedure: procedure("ResolutionService", "Reject"),
			req: &graphv1.RejectMerge{
				EntityA:   "otel.service.name=notifications",
				EntityB:   "k8s.deployment=ops/notifications-svc",
				Rationale: "parity test: different teams, different systems",
			},
			grpc: func(ctx context.Context, ts *graphServer, token string) (proto.Message, error) {
				resp, err := ts.resolutionGRPCClient(token).Reject(ctx, connect.NewRequest(
					&graphv1.RejectMerge{
						EntityA:   "otel.service.name=notifications",
						EntityB:   "k8s.deployment=ops/notifications-svc",
						Rationale: "parity test: different teams, different systems",
					}))
				return msgOrErr(resp, err)
			},
		},
		{
			name:      "Merge",
			procedure: procedure("ResolutionService", "Merge"),
			req: &graphv1.ManualMerge{
				EntityA:   "otel.service.name=checkout-legacy",
				EntityB:   "otel.service.name=checkout-svc",
				Rationale: "parity test: one service under two spellings",
			},
			grpc: func(ctx context.Context, ts *graphServer, token string) (proto.Message, error) {
				resp, err := ts.resolutionGRPCClient(token).Merge(ctx, connect.NewRequest(
					&graphv1.ManualMerge{
						EntityA:   "otel.service.name=checkout-legacy",
						EntityB:   "otel.service.name=checkout-svc",
						Rationale: "parity test: one service under two spellings",
					}))
				return msgOrErr(resp, err)
			},
		},
		{
			name:      "Split",
			procedure: procedure("ResolutionService", "Split"),
			req: &graphv1.SplitEntity{
				EntityId:     "otel.service.name=payments",
				DetachClaims: []*graphv1.Ref{ref("k8s.deployment", "shop/payments")},
				Rationale:    "parity test: the deployment was reused for something else",
			},
			grpc: func(ctx context.Context, ts *graphServer, token string) (proto.Message, error) {
				resp, err := ts.resolutionGRPCClient(token).Split(ctx, connect.NewRequest(
					&graphv1.SplitEntity{
						EntityId:     "otel.service.name=payments",
						DetachClaims: []*graphv1.Ref{ref("k8s.deployment", "shop/payments")},
						Rationale:    "parity test: the deployment was reused for something else",
					}))
				return msgOrErr(resp, err)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			grpcServer := newResolutionGraph(t)
			viaGRPC, err := tc.grpc(ctx, grpcServer, grpcServer.deciderToken(t))
			if err != nil {
				t.Fatalf("%s over gRPC: %v", tc.name, err)
			}

			jsonServer := newResolutionGraph(t)
			viaJSON := &graphv1.IngestResult{}
			postJSON(t, jsonServer, jsonServer.deciderToken(t), tc.procedure, tc.req, viaJSON)

			// observed_at is the instant the *graph* accepted the event (FR-019), not part of
			// what the two protocols serialize, and the two calls happen milliseconds apart in
			// two different databases. It is asserted to be present and then cleared, so the
			// comparison is of the answer rather than of the clock.
			grpcResult, ok := viaGRPC.(*graphv1.IngestResult)
			if !ok {
				t.Fatalf("%s over gRPC returned %T, want *graphv1.IngestResult", tc.name, viaGRPC)
			}
			if grpcResult.GetObservedAt() == nil || viaJSON.GetObservedAt() == nil {
				t.Errorf("%s: an accepted decision must carry the observed time it was stamped with", tc.name)
			}
			grpcResult.ObservedAt, viaJSON.ObservedAt = nil, nil

			assertParity(t, tc.name, grpcResult, viaJSON)
			if viaJSON.GetStatus() != graphv1.IngestResult_APPLIED {
				t.Errorf("%s: status = %s (%s); the parity comparison should be of an accepted decision",
					tc.name, viaJSON.GetStatus(), viaJSON.GetReasonDetail())
			}
		})
	}
}

// newResolutionGraph is a server with the ambiguous-identity fixture loaded, which is the graph
// the decision RPCs have something to decide about.
func newResolutionGraph(t *testing.T) *graphServer {
	t.Helper()
	return newGraphWithFixture(t, parityAmbiguousDir)
}

// resolutionGRPCClient speaks the gRPC protocol, which is the half of the parity comparison
// that has to be a real gRPC call rather than Connect's own.
func (ts *graphServer) resolutionGRPCClient(token string) graphv1connect.ResolutionServiceClient {
	return graphv1connect.NewResolutionServiceClient(h2cClient(token), ts.baseURL, connect.WithGRPC())
}
