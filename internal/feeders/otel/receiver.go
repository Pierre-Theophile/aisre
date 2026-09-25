// SPDX-License-Identifier: Apache-2.0

package otel

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// The OTLP receiver (T050, research §11).
//
// Two endpoints, one behaviour: an export request becomes a feeder.Payload holding the
// request's protobuf bytes and the moment it arrived, and the exporter is told everything was
// accepted. That last part is not laziness — it is the contract. A feeder is an observer of
// somebody else's telemetry pipeline, and an observer that answers slowly, or with a partial
// success, applies back-pressure to an application that has nothing to do with it. So the
// receiver never blocks on the feeder for longer than PushTimeout, and a payload it could not
// hand over is counted, logged and dropped rather than pushed back upstream. Dropping spans
// costs a weight class its precision for one window; blocking an exporter costs somebody an
// incident.
//
// The OpenTelemetry Collector's receiver framework would have given all this for free and
// brought a few hundred packages with it (research §11, constitution X): the two endpoints
// below are a hundred lines against the generated OTLP types the project already depends on.

// OTLP default endpoints (OTEL_EXPORTER_OTLP_ENDPOINT conventions).
const (
	// DefaultGRPCAddr is the OTLP/gRPC endpoint.
	DefaultGRPCAddr = ":4317"
	// DefaultHTTPAddr is the OTLP/HTTP endpoint.
	DefaultHTTPAddr = ":4318"
	// TracesPath is the OTLP/HTTP path for trace exports.
	TracesPath = "/v1/traces"
)

// Transport limits.
const (
	// maxExportBytes bounds one export request. OTLP exporters batch, and a batch of tens of
	// megabytes is a misconfiguration rather than a workload.
	maxExportBytes = 16 << 20
	// defaultPushTimeout is how long the receiver waits for the feeder to take a payload
	// before dropping it.
	defaultPushTimeout = 250 * time.Millisecond
	// shutdownGrace bounds the graceful stop of both servers.
	shutdownGrace = 5 * time.Second
)

// Content types OTLP/HTTP speaks.
const (
	contentTypeProtobuf = "application/x-protobuf"
	contentTypeJSON     = "application/json"
)

// ReceiverStats is what the receiver has seen. It is logged, never emitted: a counter about
// the feeder is not a fact about the production system (constitution I).
type ReceiverStats struct {
	// Exports and Spans are what was accepted.
	Exports, Spans int64
	// Dropped is how many exports could not be handed to the feeder in time.
	Dropped int64
	// Malformed is how many requests could not be decoded.
	Malformed int64
}

// Receiver accepts OTLP trace exports over gRPC and HTTP and pushes them into a ChanSource.
type Receiver struct {
	// Sink is where payloads go. Required.
	Sink *source.ChanSource
	// GRPCAddr and HTTPAddr are the listen addresses. An empty one disables that endpoint;
	// both empty is a configuration error.
	GRPCAddr string
	HTTPAddr string
	// PushTimeout bounds how long one export waits for the feeder. Zero means
	// defaultPushTimeout.
	PushTimeout time.Duration
	// Now is the clock a payload's arrival time is read from. Nil means time.Now.
	Now func() time.Time
	// Logger receives the receiver's counters. Nil means slog.Default.
	Logger *slog.Logger

	mu         sync.Mutex
	grpcServer *grpc.Server
	httpServer *http.Server
	grpcAddr   string
	httpAddr   string
	serveErr   chan error

	exports, spans, dropped, malformed atomic.Int64
}

// Start binds and serves both configured endpoints. It returns as soon as they are listening,
// so a port already in use is an error the caller sees rather than a log line it misses.
func (r *Receiver) Start(ctx context.Context) error {
	if r.Sink == nil {
		return errors.New("otel: receiver needs a sink to push payloads into")
	}
	if r.GRPCAddr == "" && r.HTTPAddr == "" {
		return errors.New("otel: receiver needs at least one of --listen-grpc and --listen-http")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.serveErr = make(chan error, 2)

	if r.GRPCAddr != "" {
		listener, err := net.Listen("tcp", r.GRPCAddr)
		if err != nil {
			return fmt.Errorf("otel: listen for OTLP/gRPC on %s: %w", r.GRPCAddr, err)
		}
		server := grpc.NewServer(grpc.MaxRecvMsgSize(maxExportBytes))
		coltracepb.RegisterTraceServiceServer(server, &traceService{recv: r, ctx: ctx})
		r.grpcServer, r.grpcAddr = server, listener.Addr().String()
		r.log().Info("otel: OTLP/gRPC receiver listening", "addr", r.grpcAddr)
		go func() { r.serveErr <- server.Serve(listener) }()
	}

	if r.HTTPAddr != "" {
		listener, err := net.Listen("tcp", r.HTTPAddr)
		if err != nil {
			r.stopLocked(ctx)
			return fmt.Errorf("otel: listen for OTLP/HTTP on %s: %w", r.HTTPAddr, err)
		}
		mux := http.NewServeMux()
		mux.Handle(TracesPath, &tracesHandler{recv: r, ctx: ctx})
		server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		r.httpServer, r.httpAddr = server, listener.Addr().String()
		r.log().Info("otel: OTLP/HTTP receiver listening", "addr", r.httpAddr, "path", TracesPath)
		go func() {
			err := server.Serve(listener)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			r.serveErr <- err
		}()
	}
	return nil
}

// GRPCAddress and HTTPAddress are the addresses actually bound, which is how a caller that
// asked for port 0 finds out what it got.
func (r *Receiver) GRPCAddress() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.grpcAddr
}

func (r *Receiver) HTTPAddress() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.httpAddr
}

// Shutdown stops both endpoints, letting in-flight exports finish, and closes the sink so the
// feeder drains what it was already given and then sees end of input.
func (r *Receiver) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked(ctx)
	r.Sink.Close()
	stats := r.Stats()
	r.log().Info("otel: receiver stopped",
		"exports", stats.Exports, "spans", stats.Spans,
		"dropped_exports", stats.Dropped, "malformed_requests", stats.Malformed)

	var errs []error
	for range cap(r.serveErr) {
		select {
		case err := <-r.serveErr:
			errs = append(errs, err)
		default:
		}
	}
	return errors.Join(errs...)
}

func (r *Receiver) stopLocked(ctx context.Context) {
	if r.httpServer != nil {
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		_ = r.httpServer.Shutdown(stop)
		cancel()
		r.httpServer = nil
	}
	if r.grpcServer != nil {
		r.grpcServer.GracefulStop()
		r.grpcServer = nil
	}
}

// Stats is what the receiver has seen so far.
func (r *Receiver) Stats() ReceiverStats {
	return ReceiverStats{
		Exports:   r.exports.Load(),
		Spans:     r.spans.Load(),
		Dropped:   r.dropped.Load(),
		Malformed: r.malformed.Load(),
	}
}

// accept hands one decoded export to the feeder. The raw bytes are what is pushed and what a
// recording stores, so replaying a recording gives the feeder byte-identical input to the live
// run (FR-044).
//
// It never reports failure to the caller: every path here ends in an acknowledged export.
func (r *Receiver) accept(ctx context.Context, req *coltracepb.ExportTraceServiceRequest, raw []byte) {
	r.exports.Add(1)
	r.spans.Add(countSpans(req))

	timeout := r.PushTimeout
	if timeout <= 0 {
		timeout = defaultPushTimeout
	}
	push, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := r.Sink.Push(push, feeder.Payload{
		Kind:  PayloadKindTraces,
		At:    r.now(),
		Bytes: raw,
	}); err != nil {
		r.dropped.Add(1)
		r.log().Warn("otel: export dropped, the feeder did not take it in time",
			"spans", countSpans(req), "reason", err.Error(),
			"dropped_exports", r.dropped.Load())
		return
	}
	r.log().Debug("otel: export accepted", "spans", countSpans(req), "bytes", len(raw))
}

func (r *Receiver) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now().UTC()
}

func (r *Receiver) log() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}

// traceService is the OTLP/gRPC endpoint.
//
// The context it pushes with is the receiver's, not the RPC's: an exporter that gives up on a
// slow response must not also cancel the handover of spans this process has already accepted.
type traceService struct {
	coltracepb.UnimplementedTraceServiceServer
	recv *Receiver
	ctx  context.Context //nolint:containedctx // the receiver's lifetime, not a request's
}

func (s *traceService) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	raw, err := proto.Marshal(req)
	if err != nil {
		s.recv.malformed.Add(1)
		s.recv.log().Warn("otel: re-encoding an export failed", "error", err.Error())
		return &coltracepb.ExportTraceServiceResponse{}, nil
	}
	s.recv.accept(s.ctx, req, raw)
	// Zero rejected spans, always: see the note at the top of this file.
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// tracesHandler is the OTLP/HTTP endpoint, protobuf and JSON.
type tracesHandler struct {
	recv *Receiver
	ctx  context.Context //nolint:containedctx // the receiver's lifetime, not a request's
}

func (h *tracesHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "OTLP/HTTP accepts POST", http.StatusMethodNotAllowed)
		return
	}
	contentType := mediaType(req.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = contentTypeProtobuf
	}
	if contentType != contentTypeProtobuf && contentType != contentTypeJSON {
		http.Error(w, "OTLP/HTTP accepts "+contentTypeProtobuf+" or "+contentTypeJSON,
			http.StatusUnsupportedMediaType)
		return
	}

	body, err := readBody(w, req)
	if err != nil {
		h.recv.malformed.Add(1)
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	export := &coltracepb.ExportTraceServiceRequest{}
	raw := body
	if contentType == contentTypeJSON {
		if err := protojson.Unmarshal(body, export); err != nil {
			h.recv.malformed.Add(1)
			http.Error(w, "decode OTLP JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		// A recording holds protobuf whatever the wire format was, so that one decoder reads
		// every payload the feeder will ever see.
		if raw, err = proto.Marshal(export); err != nil {
			h.recv.malformed.Add(1)
			http.Error(w, "re-encode OTLP JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else if err := proto.Unmarshal(body, export); err != nil {
		h.recv.malformed.Add(1)
		http.Error(w, "decode OTLP protobuf: "+err.Error(), http.StatusBadRequest)
		return
	}

	h.recv.accept(h.ctx, export, raw)
	h.respond(w, contentType)
}

// respond acknowledges the export in the format it arrived in, with nothing rejected.
func (h *tracesHandler) respond(w http.ResponseWriter, contentType string) {
	response := &coltracepb.ExportTraceServiceResponse{}
	var (
		encoded []byte
		err     error
	)
	if contentType == contentTypeJSON {
		encoded, err = protojson.Marshal(response)
	} else {
		encoded, err = proto.Marshal(response)
	}
	if err != nil {
		http.Error(w, "encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(encoded); err != nil {
		h.recv.log().Debug("otel: writing the export acknowledgement failed", "error", err.Error())
	}
}

// readBody reads a bounded, possibly gzipped request body.
func readBody(w http.ResponseWriter, req *http.Request) ([]byte, error) {
	var reader io.Reader = http.MaxBytesReader(w, req.Body, maxExportBytes)
	if strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
		unzipped, err := gzip.NewReader(reader)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer func() { _ = unzipped.Close() }()
		reader = unzipped
	}
	return io.ReadAll(reader)
}

// mediaType strips the parameters of a Content-Type header.
func mediaType(header string) string {
	value, _, _ := strings.Cut(header, ";")
	return strings.ToLower(strings.TrimSpace(value))
}

// countSpans is how many spans an export carries. It is a counter for a log line; no span is
// kept.
func countSpans(req *coltracepb.ExportTraceServiceRequest) int64 {
	var total int64
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			total += int64(len(ss.GetSpans()))
		}
	}
	return total
}
