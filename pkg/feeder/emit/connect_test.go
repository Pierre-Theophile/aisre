// SPDX-License-Identifier: Apache-2.0

package emit_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/server"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// mustNumericSeries is the property shape the graph refuses: a list of numbers is a sample
// series whatever it is called (constitution IV).
func mustNumericSeries(t *testing.T) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(map[string]any{"latency_samples": []any{1.0, 2.0, 3.0}})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return s
}

// graphServer is a real graph on a random port, with a dev identity provider, so that the
// emitter is exercised over the wire it will actually use.
type graphServer struct {
	baseURL string
	token   string
}

func newGraphServer(t *testing.T, sourceID string) *graphServer {
	t.Helper()

	store := pgtest.Open(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	auth, err := server.NewDevAuthenticator(server.DevConfig{Enabled: true, Logger: logger})
	if err != nil {
		t.Fatalf("dev authenticator: %v", err)
	}
	srv, err := server.New(server.Config{
		Listen:          "127.0.0.1:0",
		Auth:            auth,
		Projector:       projector.New(store),
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

	token, err := auth.Issuer().MintFor(server.DevUser{
		Name: "feeder", Roles: []server.Role{server.RoleFeeder}, SourceID: sourceID,
	})
	if err != nil {
		t.Fatalf("mint feeder token: %v", err)
	}
	return &graphServer{baseURL: "http://" + srv.Addr(), token: token}
}

// newRawClient builds a plain Connect client carrying the feeder token, which the test
// decorators wrap. It duplicates what NewConnectEmitter does internally on purpose: a test that
// wants to count RPCs has to own the client the emitter is given.
func newRawClient(t *testing.T, gs *graphServer) graphv1connect.IngestServiceClient {
	t.Helper()
	return graphv1connect.NewIngestServiceClient(&http.Client{
		Transport: &tokenTransport{token: gs.token},
		Timeout:   30 * time.Second,
	}, gs.baseURL)
}

// tokenTransport attaches the feeder credential to every request.
type tokenTransport struct{ token string }

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(req)
}

// countingClient wraps the generated client so a test can see how many RPCs were made. It is
// how "the source is registered once" is asserted without reaching into the emitter.
type countingClient struct {
	graphv1connect.IngestServiceClient
	registers atomic.Int64
	batches   atomic.Int64
}

func (c *countingClient) RegisterSource(ctx context.Context, req *connect.Request[graphv1.RegisterSourceRequest]) (*connect.Response[graphv1.RegisterSourceResponse], error) {
	c.registers.Add(1)
	return c.IngestServiceClient.RegisterSource(ctx, req)
}

func (c *countingClient) IngestBatch(ctx context.Context, req *connect.Request[graphv1.IngestBatchRequest]) (*connect.Response[graphv1.IngestBatchResponse], error) {
	c.batches.Add(1)
	return c.IngestServiceClient.IngestBatch(ctx, req)
}

func newCountingEmitter(t *testing.T, d feeder.Description, opts ...emit.ConnectOption) (*emit.ConnectEmitter, *countingClient) {
	t.Helper()
	gs := newGraphServer(t, d.SourceID)

	// The emitter's own transport is reused, so the credential path is exercised too; only
	// the counting decorator is added on top.
	plain, err := emit.NewConnectEmitter(gs.baseURL, gs.token, d)
	if err != nil {
		t.Fatalf("new emitter: %v", err)
	}
	t.Cleanup(func() { _ = plain.Close(context.Background()) })

	counting := &countingClient{IngestServiceClient: newRawClient(t, gs)}
	opts = append([]emit.ConnectOption{
		emit.WithIngestClient(counting),
		emit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}, opts...)
	em, err := emit.NewConnectEmitter(gs.baseURL, gs.token, d, opts...)
	if err != nil {
		t.Fatalf("new counting emitter: %v", err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })
	return em, counting
}

func TestConnectEmitterBatchesAndRegistersOnce(t *testing.T) {
	d := testDescription()
	var (
		mu      sync.Mutex
		results []*graphv1.IngestResult
	)
	em, counting := newCountingEmitter(t, d,
		emit.WithBatchSize(3),
		emit.WithFlushInterval(0), // no background flusher: this test drives every flush
		emit.WithResultFunc(func(r *graphv1.IngestResult) {
			mu.Lock()
			defer mu.Unlock()
			results = append(results, r)
		}),
	)

	ctx := t.Context()
	const total = 7
	for i := range total {
		ev := upsert(t, d, feeder.NewID(d.SourceID, "deploy", "shop/svc", string(rune('a'+i))), "shop/svc-"+string(rune('a'+i)))
		result, err := em.Emit(ctx, ev)
		if err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
		if result != nil {
			t.Errorf("Emit %d returned a result while batching; the graph has not answered yet", i)
		}
	}
	if got := em.Buffered(); got != total%3 {
		t.Errorf("buffered %d events after %d emits with a batch of 3, want %d", got, total, total%3)
	}
	if err := em.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if got := counting.registers.Load(); got != 1 {
		t.Errorf("registered the source %d times, want exactly once", got)
	}
	if got, want := counting.batches.Load(), int64(3); got != want {
		t.Errorf("made %d IngestBatch calls for %d events with a batch of 3, want %d", got, total, want)
	}
	stats := em.Stats()
	if stats.Applied != total {
		t.Errorf("Stats.Applied = %d, want %d", stats.Applied, total)
	}
	if stats.Rejected != 0 || stats.Dropped != 0 {
		t.Errorf("Stats = %+v, want nothing rejected or dropped", stats)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(results) != total {
		t.Errorf("the result callback saw %d results, want one per event (%d)", len(results), total)
	}
}

func TestConnectEmitterSynchronousWithBatchSizeOne(t *testing.T) {
	d := testDescription()
	em, _ := newCountingEmitter(t, d, emit.WithBatchSize(1))

	result, err := em.Emit(t.Context(), upsert(t, d, "k8s:demo:deploy:shop/checkout@rv1", "shop/checkout"))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Fatalf("status %s, want APPLIED", result.GetStatus())
	}
	if result.GetObservedAt() == nil {
		t.Error("the graph's observed time did not come back")
	}
}

func TestConnectEmitterSurfacesRejections(t *testing.T) {
	d := testDescription()
	var seen []*graphv1.IngestResult
	em, _ := newCountingEmitter(t, d,
		emit.WithBatchSize(1),
		emit.WithResultFunc(func(r *graphv1.IngestResult) { seen = append(seen, r) }),
	)

	// A retraction of something the graph has never seen validates locally and is refused by
	// the projector, which is the only way to exercise a server-side rejection end to end.
	ev := feeder.RetractNode(d, "k8s:demo:retract:ghost", feeder.NodeRetraction{
		Ref:      feeder.Ref(feeder.NSK8sDeployment, "shop/ghost"),
		ValidEnd: validAt(t),
	})
	result, err := em.Emit(t.Context(), ev)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_REJECTED {
		t.Fatalf("status %s, want REJECTED", result.GetStatus())
	}
	if result.GetReasonCode() != feeder.ReasonRefUnresolvable {
		t.Errorf("reason %q, want %q", result.GetReasonCode(), feeder.ReasonRefUnresolvable)
	}
	if got := em.Stats().Rejected; got != 1 {
		t.Errorf("Stats.Rejected = %d, want 1; a rejection must be counted, not swallowed", got)
	}
	if len(seen) != 1 || seen[0].GetStatus() != graphv1.IngestResult_REJECTED {
		t.Errorf("the result callback did not see the rejection: %v", seen)
	}
}

func TestConnectEmitterRefusesTelemetryBeforeSending(t *testing.T) {
	d := testDescription()
	em, counting := newCountingEmitter(t, d, emit.WithBatchSize(1))

	bad := upsert(t, d, "k8s:demo:bad-props-1", "shop/checkout")
	bad.GetUpsertNode().Props = mustNumericSeries(t)

	result, err := em.Emit(t.Context(), bad)
	if err == nil {
		t.Fatal("a telemetry payload was accepted")
	}
	if result.GetReasonCode() != feeder.ReasonTelemetryPayload {
		t.Errorf("reason %q, want %q", result.GetReasonCode(), feeder.ReasonTelemetryPayload)
	}
	if got := counting.batches.Load(); got != 0 {
		t.Errorf("the refused event was still sent (%d batches); local validation exists to avoid the round trip", got)
	}
}

func TestConnectEmitterCheckpointAndDoubleDelivery(t *testing.T) {
	d := testDescription()
	em, _ := newCountingEmitter(t, d, emit.WithBatchSize(1))
	ctx := t.Context()

	from := validAt(t)
	to := from.Add(80 * time.Second)
	if err := em.Checkpoint(ctx, feeder.CheckpointFact{ExtentFrom: from, ExtentTo: to}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// Re-delivering the same checkpoint must be a no-op, which is only true because the event
	// id is a pure function of the extent (FR-020).
	if err := em.Checkpoint(ctx, feeder.CheckpointFact{ExtentFrom: from, ExtentTo: to}); err != nil {
		t.Fatalf("second Checkpoint: %v", err)
	}
	stats := em.Stats()
	if stats.Applied != 1 || stats.DuplicateNoop != 1 {
		t.Errorf("Stats = %+v, want one applied and one duplicate", stats)
	}
}

func TestConnectEmitterRetriesTransportFailures(t *testing.T) {
	d := testDescription()
	gs := newGraphServer(t, d.SourceID)

	flaky := &flakyClient{IngestServiceClient: newRawClient(t, gs), failures: 2}
	em, err := emit.NewConnectEmitter(gs.baseURL, gs.token, d,
		emit.WithIngestClient(flaky),
		emit.WithBatchSize(1),
		emit.WithRetryBaseDelay(time.Millisecond),
		emit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("new emitter: %v", err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })

	result, err := em.Emit(t.Context(), upsert(t, d, "k8s:demo:deploy:shop/checkout@rv1", "shop/checkout"))
	if err != nil {
		t.Fatalf("Emit through two transient failures: %v", err)
	}
	if result.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Errorf("status %s, want APPLIED", result.GetStatus())
	}
	if got := em.Stats().Retries; got < 2 {
		t.Errorf("Stats.Retries = %d, want at least 2", got)
	}
}

func TestConnectEmitterDoesNotRetryARefusal(t *testing.T) {
	d := testDescription()
	gs := newGraphServer(t, d.SourceID)

	// A token scoped to another source: the server answers PermissionDenied, which is an
	// answer and not a hiccup, so it must not be retried.
	other := testDescription()
	other.SourceID = "otel:demo"
	em, err := emit.NewConnectEmitter(gs.baseURL, gs.token, other,
		emit.WithBatchSize(1),
		emit.WithRetryBaseDelay(time.Millisecond),
		emit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("new emitter: %v", err)
	}
	t.Cleanup(func() { _ = em.Close(context.Background()) })

	if _, err := em.Emit(t.Context(), upsert(t, other, "otel:demo:svc:checkout", "shop/checkout")); err == nil {
		t.Fatal("an out-of-scope source id was accepted (FR-046)")
	}
	if got := em.Stats().Retries; got != 0 {
		t.Errorf("Stats.Retries = %d, want 0: a refusal is not retried", got)
	}
}

func TestConnectEmitterBackgroundFlush(t *testing.T) {
	d := testDescription()
	em, _ := newCountingEmitter(t, d,
		emit.WithBatchSize(100),
		emit.WithFlushInterval(20*time.Millisecond),
	)
	if _, err := em.Emit(t.Context(), upsert(t, d, "k8s:demo:deploy:shop/checkout@rv1", "shop/checkout")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for em.Stats().Applied == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the background flusher never sent a batch that was below the batch size")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNewConnectEmitterRejectsABadDescription(t *testing.T) {
	t.Parallel()
	if _, err := emit.NewConnectEmitter("localhost:8080", "t", feeder.Description{Kind: "k8s"}); err == nil {
		t.Error("an emitter was built for a description with no source id")
	}
	if _, err := emit.NewConnectEmitter("", "t", testDescription()); err == nil {
		t.Error("an emitter was built with no server address")
	}
}

// flakyClient fails the first `failures` IngestBatch calls with a transient code.
type flakyClient struct {
	graphv1connect.IngestServiceClient
	mu       sync.Mutex
	failures int
}

func (c *flakyClient) IngestBatch(ctx context.Context, req *connect.Request[graphv1.IngestBatchRequest]) (*connect.Response[graphv1.IngestBatchResponse], error) {
	c.mu.Lock()
	if c.failures > 0 {
		c.failures--
		c.mu.Unlock()
		return nil, connect.NewError(connect.CodeUnavailable, io.ErrUnexpectedEOF)
	}
	c.mu.Unlock()
	return c.IngestServiceClient.IngestBatch(ctx, req)
}
