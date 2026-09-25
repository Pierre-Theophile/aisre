// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// End-to-end tests of the serving layer (FR-017, FR-035, FR-041a, FR-046).
//
// They run against a real listener rather than the in-process handler on purpose: h2c, the
// gRPC protocol and the JSON protocol are exactly the parts that a handler-level test would
// not exercise, and "a feeder speaks gRPC and a curl speaks JSON to the same procedure" is the
// promise being made.

// The two source ids are the ones the shipped baseline fixture carries. It is a recording of
// a live kind cluster since 2026-09-16, so they are `:kind` rather than the `:demo` of the
// hand-authored era; these tests read real events off it and must name its sources.
const (
	testSourceID    = "otel:kind"
	otherSourceID   = "k8s:kind"
	testFixtureDir  = "../../fixtures/baseline-topology-01"
	testEventsCount = 10
)

// graphServer is a running graph on a random port, with a dev identity provider.
type graphServer struct {
	baseURL   string
	projector *projector.Projector
	issuer    *DevIssuer
}

func newGraphServer(t *testing.T) *graphServer {
	t.Helper()

	store := pgtest.Open(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	auth, err := NewDevAuthenticator(DevConfig{Enabled: true, Logger: logger})
	if err != nil {
		t.Fatalf("dev authenticator: %v", err)
	}
	proj := projector.New(store)

	srv, err := New(Config{
		Listen:          "127.0.0.1:0",
		Auth:            auth,
		Projector:       proj,
		Logger:          logger,
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	})

	return &graphServer{baseURL: "http://" + srv.Addr(), projector: proj, issuer: auth.Issuer()}
}

// feederToken mints a token scoped to exactly one source (FR-046).
func (ts *graphServer) feederToken(t *testing.T, sourceID string) string {
	t.Helper()
	token, err := ts.issuer.MintFor(DevUser{
		Name: "otel-feeder", Roles: []Role{RoleFeeder}, SourceID: sourceID,
	})
	if err != nil {
		t.Fatalf("mint feeder token: %v", err)
	}
	return token
}

func (ts *graphServer) readerToken(t *testing.T) string {
	t.Helper()
	token, err := ts.issuer.MintFor(DevUser{Name: "alice", Roles: []Role{RoleReader}})
	if err != nil {
		t.Fatalf("mint reader token: %v", err)
	}
	return token
}

// bearerTransport attaches the credential to every request, streams included.
type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(req)
}

// h2cClient speaks HTTP/2 over cleartext, which is what a gRPC client needs against a server
// with no TLS.
func h2cClient(token string) *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{Protocols: protocols}
	return &http.Client{Transport: &bearerTransport{base: transport, token: token}}
}

func (ts *graphServer) ingestClient(token string) graphv1connect.IngestServiceClient {
	return graphv1connect.NewIngestServiceClient(h2cClient(token), ts.baseURL, connect.WithGRPC())
}

func (ts *graphServer) queryClient(token string) graphv1connect.QueryServiceClient {
	return graphv1connect.NewQueryServiceClient(h2cClient(token), ts.baseURL, connect.WithGRPC())
}

// fixtureEvents reads the first n events of one source out of a fixture's events.jsonl.
//
// `appendedSeq` and `observedAt` are columns the log adds, not fields of EventEnvelope, so
// they are lifted off before unmarshalling; unmarshalling itself is strict, because a fixture
// carrying a field the published schema does not have is a broken fixture.
func fixtureEvents(t *testing.T, sourceID string, n int) []*graphv1.EventEnvelope {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(testFixtureDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read fixture events: %v", err)
	}
	var events []*graphv1.EventEnvelope
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("parse fixture line: %v", err)
		}
		delete(fields, "appendedSeq")
		delete(fields, "observedAt")

		body, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("re-encode fixture line: %v", err)
		}
		env := &graphv1.EventEnvelope{}
		if err := (protojson.UnmarshalOptions{}).Unmarshal(body, env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.GetSourceId() != sourceID {
			continue
		}
		events = append(events, env)
		if len(events) == n {
			break
		}
	}
	if len(events) != n {
		t.Fatalf("fixture has %d events for source %s, want %d", len(events), sourceID, n)
	}
	return events
}

// registerSource declares the feeder, which FR-018 requires before it may append.
func registerSource(t *testing.T, client graphv1connect.IngestServiceClient, sourceID string) {
	t.Helper()
	_, err := client.RegisterSource(context.Background(), connect.NewRequest(&graphv1.RegisterSourceRequest{
		SourceId:                sourceID,
		Kind:                    "otel",
		Ordering:                "none",
		ReorderingWindowSeconds: 300,
		SchemaVersion:           "1.0.0",
	}))
	if err != nil {
		t.Fatalf("register source %s: %v", sourceID, err)
	}
}

func TestIngestStreamOverGRPCApplies(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.ingestClient(ts.feederToken(t, testSourceID))
	registerSource(t, client, testSourceID)

	events := fixtureEvents(t, testSourceID, testEventsCount)

	ctx := context.Background()
	stream := client.Ingest(ctx)
	for _, env := range events {
		if err := stream.Send(env); err != nil {
			t.Fatalf("send %s: %v", env.GetEventId(), err)
		}
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}

	for i, env := range events {
		result, err := stream.Receive()
		if err != nil {
			t.Fatalf("receive result %d: %v", i, err)
		}
		if result.GetEventId() != env.GetEventId() {
			t.Errorf("result %d is for %q, want %q", i, result.GetEventId(), env.GetEventId())
		}
		if result.GetStatus() != graphv1.IngestResult_APPLIED {
			t.Errorf("event %s: status %s (%s: %s), want APPLIED",
				env.GetEventId(), result.GetStatus(), result.GetReasonCode(), result.GetReasonDetail())
		}
		if result.GetObservedAt() == nil {
			t.Errorf("event %s: applied with no observed time", env.GetEventId())
		}
	}
	if _, err := stream.Receive(); !errors.Is(err, io.EOF) {
		t.Errorf("receive after the last result: %v, want io.EOF", err)
	}
	if err := stream.CloseResponse(); err != nil {
		t.Fatalf("close response: %v", err)
	}
}

// A rejected event is an answer, not a failure: the stream must carry on.
func TestIngestStreamSurvivesRejection(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.ingestClient(ts.feederToken(t, testSourceID))
	registerSource(t, client, testSourceID)

	events := fixtureEvents(t, testSourceID, 2)
	bad := &graphv1.EventEnvelope{
		EventId:       testSourceID + ":bogus@1",
		SourceId:      testSourceID,
		SchemaVersion: "99.0.0", // not an accepted schema version (FR-025)
		Body: &graphv1.EventEnvelope_SourceCheckpoint{
			SourceCheckpoint: &graphv1.SourceCheckpoint{},
		},
	}
	sent := []*graphv1.EventEnvelope{events[0], bad, events[1]}
	want := []graphv1.IngestResult_Status{
		graphv1.IngestResult_APPLIED,
		graphv1.IngestResult_REJECTED,
		graphv1.IngestResult_APPLIED,
	}

	stream := client.Ingest(context.Background())
	for _, env := range sent {
		if err := stream.Send(env); err != nil {
			t.Fatalf("send %s: %v", env.GetEventId(), err)
		}
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("close request: %v", err)
	}
	for i := range sent {
		result, err := stream.Receive()
		if err != nil {
			t.Fatalf("receive result %d: %v", i, err)
		}
		if result.GetStatus() != want[i] {
			t.Errorf("result %d: status %s, want %s", i, result.GetStatus(), want[i])
		}
		if want[i] == graphv1.IngestResult_REJECTED && result.GetReasonCode() == "" {
			t.Errorf("result %d: rejected with no reason code", i)
		}
	}
	if err := stream.CloseResponse(); err != nil {
		t.Fatalf("close response: %v", err)
	}
}

// The same procedure, reached with a plain JSON POST: FR-035's machine-readable half costs
// nothing because ConnectRPC serves JSON on the same route.
func TestIngestBatchOverJSONApplies(t *testing.T) {
	ts := newGraphServer(t)
	token := ts.feederToken(t, testSourceID)

	postJSON := func(t *testing.T, procedure string, body any, out any) {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			ts.baseURL+procedure, bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post %s: %v", procedure, err)
		}
		defer func() { _ = resp.Body.Close() }()

		payload, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("post %s: status %d: %s", procedure, resp.StatusCode, payload)
		}
		if out != nil {
			if err := json.Unmarshal(payload, out); err != nil {
				t.Fatalf("decode response %s: %v", payload, err)
			}
		}
	}

	postJSON(t, graphv1connect.IngestServiceRegisterSourceProcedure, map[string]any{
		"sourceId":                testSourceID,
		"kind":                    "otel",
		"ordering":                "none",
		"reorderingWindowSeconds": "300",
		"schemaVersion":           "1.0.0",
	}, nil)

	// The envelopes go back out through protojson so the request body is exactly the published
	// JSON mapping a third-party feeder would write by hand.
	events := fixtureEvents(t, testSourceID, testEventsCount)
	encoded := make([]json.RawMessage, 0, len(events))
	for _, env := range events {
		line, err := protojson.Marshal(env)
		if err != nil {
			t.Fatalf("encode envelope: %v", err)
		}
		encoded = append(encoded, line)
	}

	var response struct {
		Results []struct {
			EventID      string `json:"eventId"`
			Status       string `json:"status"`
			ReasonCode   string `json:"reasonCode"`
			ReasonDetail string `json:"reasonDetail"`
		} `json:"results"`
	}
	postJSON(t, graphv1connect.IngestServiceIngestBatchProcedure,
		map[string]any{"events": encoded}, &response)

	if len(response.Results) != len(events) {
		t.Fatalf("got %d results, want %d", len(response.Results), len(events))
	}
	for i, result := range response.Results {
		if result.EventID != events[i].GetEventId() {
			t.Errorf("result %d is for %q, want %q", i, result.EventID, events[i].GetEventId())
		}
		if result.Status != "APPLIED" {
			t.Errorf("event %s: status %q (%s: %s), want APPLIED",
				result.EventID, result.Status, result.ReasonCode, result.ReasonDetail)
		}
	}
}

// FR-046: a feeder token may only write for the source it is scoped to.
func TestIngestForAnotherSourceIsPermissionDenied(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.ingestClient(ts.feederToken(t, testSourceID))
	registerSource(t, client, testSourceID)

	foreign := fixtureEvents(t, otherSourceID, 1)
	_, err := client.IngestBatch(context.Background(),
		connect.NewRequest(&graphv1.IngestBatchRequest{Events: foreign}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("ingest for a foreign source: code %v (%v), want PermissionDenied", got, err)
	}

	// Registering somebody else's source is refused for the same reason.
	_, err = client.RegisterSource(context.Background(),
		connect.NewRequest(&graphv1.RegisterSourceRequest{SourceId: otherSourceID, Kind: "k8s"}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("register a foreign source: code %v (%v), want PermissionDenied", got, err)
	}
}

// FR-041a: an anonymous call is refused before a single byte of it is applied.
func TestIngestWithoutTokenIsUnauthenticated(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.ingestClient("")

	events := fixtureEvents(t, testSourceID, 1)
	_, err := client.IngestBatch(context.Background(),
		connect.NewRequest(&graphv1.IngestBatchRequest{Events: events}))
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("anonymous ingest: code %v (%v), want Unauthenticated", got, err)
	}

	stream := client.Ingest(context.Background())
	// The interceptor refuses before the first message is read, so the failure surfaces on
	// either the send or the receive depending on how much of the stream got out.
	_ = stream.Send(events[0])
	_ = stream.CloseRequest()
	_, err = stream.Receive()
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("anonymous stream: code %v (%v), want Unauthenticated", got, err)
	}
	_ = stream.CloseResponse()
}

// Every query in the contract now answers a reader token, and answers with the code its own
// request deserves rather than with Unimplemented (FR-035, T065, T068, T070). Impact and
// Pointers refuse a request with no as-of instant — the graph answers as of an instant and
// never defaults it — and NodeHistory, which takes no instant at all, refuses a reference the
// graph has never heard of.
func TestQueriesWithReaderTokenAreImplemented(t *testing.T) {
	ts := newGraphServer(t)
	client := ts.queryClient(ts.readerToken(t))
	ctx := context.Background()

	_, err := client.Impact(ctx, connect.NewRequest(&graphv1.ImpactRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("impact without an as-of: code %v (%v), want InvalidArgument", got, err)
	}
	_, err = client.Pointers(ctx, connect.NewRequest(&graphv1.PointersRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("pointers without an as-of: code %v (%v), want InvalidArgument", got, err)
	}
	_, err = client.NodeHistory(ctx, connect.NewRequest(&graphv1.NodeHistoryRequest{
		Focus: &graphv1.Ref{Namespace: "otel.service.name", Value: "nothing-here"},
	}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Errorf("history of an unknown node: code %v (%v), want NotFound", got, err)
	}
}

// Diff now answers a reader token, and answers with the argument error the request deserves
// rather than Unimplemented: a diff without a window is a bad request (FR-027, T040).
func TestDiffWithReaderTokenIsImplemented(t *testing.T) {
	ts := newGraphServer(t)

	_, err := ts.queryClient(ts.readerToken(t)).Diff(context.Background(),
		connect.NewRequest(&graphv1.DiffRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("diff without a window: code %v (%v), want InvalidArgument", got, err)
	}
}

// The queries that have landed answer a reader token. An empty graph has no observed span,
// which is a result rather than an error.
func TestExtentWithReaderTokenAnswers(t *testing.T) {
	ts := newGraphServer(t)

	resp, err := ts.queryClient(ts.readerToken(t)).Extent(context.Background(),
		connect.NewRequest(&graphv1.ExtentRequest{}))
	if err != nil {
		t.Fatalf("extent with a reader token: %v", err)
	}
	if resp.Msg.GetEarliestObserved() != nil {
		t.Errorf("an empty log has no earliest observation, got %v", resp.Msg.GetEarliestObserved())
	}
}

func TestQueryWithoutTokenIsUnauthenticated(t *testing.T) {
	ts := newGraphServer(t)

	_, err := ts.queryClient("").Extent(context.Background(),
		connect.NewRequest(&graphv1.ExtentRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("anonymous extent: code %v (%v), want Unauthenticated", got, err)
	}
}

// A feeder can write but may not read: RoleFeeder implies nothing (research §7).
func TestQueryWithFeederTokenIsPermissionDenied(t *testing.T) {
	ts := newGraphServer(t)

	_, err := ts.queryClient(ts.feederToken(t, testSourceID)).Extent(context.Background(),
		connect.NewRequest(&graphv1.ExtentRequest{}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("extent with a feeder token: code %v (%v), want PermissionDenied", got, err)
	}
}

func TestResolutionWithReaderTokenIsPermissionDenied(t *testing.T) {
	ts := newGraphServer(t)
	client := graphv1connect.NewResolutionServiceClient(
		h2cClient(ts.readerToken(t)), ts.baseURL, connect.WithGRPC())

	_, err := client.Confirm(context.Background(), connect.NewRequest(&graphv1.ConfirmMerge{
		EntityA: "a", EntityB: "b", Rationale: "same service",
	}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("confirm with a reader token: code %v (%v), want PermissionDenied", got, err)
	}
}

// A probe has no credential, so the health endpoints must not want one.
func TestHealthEndpointsArePublic(t *testing.T) {
	ts := newGraphServer(t)

	for _, path := range []string{HealthPath, ReadyPath} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.baseURL+path, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("get %s: status %d, body %q, want 200", path, resp.StatusCode, body)
		}
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	store := pgtest.Open(t)
	auth, err := NewDevAuthenticator(DevConfig{
		Enabled: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("dev authenticator: %v", err)
	}

	if _, err := New(Config{Listen: "127.0.0.1:0", Projector: projector.New(store)}); err == nil {
		t.Error("New accepted a config with no authenticator")
	}
	if _, err := New(Config{Listen: "127.0.0.1:0", Auth: auth}); err == nil {
		t.Error("New accepted a config with no projector")
	}
}
