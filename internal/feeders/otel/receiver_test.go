// SPDX-License-Identifier: Apache-2.0

package otel_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	otelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/otel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// The receiver (T050). Every test here asserts the same two things in a different transport:
// the export reaches the feeder as a payload holding the request's protobuf bytes, and the
// exporter is told everything was accepted.

// startReceiver brings up both endpoints on free ports.
func startReceiver(t *testing.T, buffer int, pushTimeout time.Duration) (*otelfeeder.Receiver, *source.ChanSource) {
	t.Helper()
	sink := source.NewChanSource(buffer)
	receiver := &otelfeeder.Receiver{
		Sink:        sink,
		GRPCAddr:    "127.0.0.1:0",
		HTTPAddr:    "127.0.0.1:0",
		PushTimeout: pushTimeout,
		Now:         func() time.Time { return fixtureStart },
	}
	if err := receiver.Start(t.Context()); err != nil {
		t.Fatalf("start receiver: %v", err)
	}
	t.Cleanup(func() {
		if err := receiver.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return receiver, sink
}

// oneExport is a single-span export of storefront calling checkout.
func oneExport() *coltracepb.ExportTraceServiceRequest {
	return buildExport(export(fixtureStart,
		shopCaller("storefront", "3.1.0"),
		map[string]any{"peer.service": "checkout"}, 1), 0)
}

// takePayload reads the payload the receiver pushed and asserts it is the export it was given.
func takePayload(t *testing.T, sink *source.ChanSource, want *coltracepb.ExportTraceServiceRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	payload, err := sink.Next(ctx)
	if err != nil {
		t.Fatalf("no payload reached the feeder: %v", err)
	}
	if payload.Kind != otelfeeder.PayloadKindTraces {
		t.Errorf("payload kind = %q, want %q", payload.Kind, otelfeeder.PayloadKindTraces)
	}
	if !payload.At.Equal(fixtureStart) {
		t.Errorf("payload arrival time = %s, want %s", payload.At, fixtureStart)
	}
	got := &coltracepb.ExportTraceServiceRequest{}
	if err := proto.Unmarshal(payload.Bytes, got); err != nil {
		t.Fatalf("the payload is not an OTLP export: %v", err)
	}
	if !proto.Equal(got, want) {
		t.Error("the payload is not the export that was sent")
	}
}

func TestReceiverGRPC(t *testing.T) {
	t.Parallel()
	receiver, sink := startReceiver(t, 4, 0)

	conn, err := grpc.NewClient(receiver.GRPCAddress(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	request := oneExport()
	response, err := coltracepb.NewTraceServiceClient(conn).Export(t.Context(), request)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if rejected := response.GetPartialSuccess().GetRejectedSpans(); rejected != 0 {
		t.Errorf("the receiver rejected %d spans; a feeder never back-pressures the pipeline it watches", rejected)
	}
	takePayload(t, sink, request)
}

func TestReceiverHTTP(t *testing.T) {
	t.Parallel()
	request := oneExport()
	asProtobuf, err := proto.Marshal(request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	asJSON, err := protojson.Marshal(request)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}

	cases := []struct {
		name        string
		contentType string
		body        []byte
		gzipped     bool
	}{
		{name: "protobuf", contentType: "application/x-protobuf", body: asProtobuf},
		{name: "protobuf with charset parameters", contentType: "application/x-protobuf; charset=utf-8", body: asProtobuf},
		{name: "json", contentType: "application/json", body: asJSON},
		{name: "gzipped protobuf", contentType: "application/x-protobuf", body: asProtobuf, gzipped: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			receiver, sink := startReceiver(t, 4, 0)
			body := tc.body
			if tc.gzipped {
				var buf bytes.Buffer
				zip := gzip.NewWriter(&buf)
				if _, err := zip.Write(body); err != nil {
					t.Fatalf("gzip: %v", err)
				}
				if err := zip.Close(); err != nil {
					t.Fatalf("gzip close: %v", err)
				}
				body = buf.Bytes()
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				"http://"+receiver.HTTPAddress()+otelfeeder.TracesPath, bytes.NewReader(body))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.Header.Set("Content-Type", tc.contentType)
			if tc.gzipped {
				req.Header.Set("Content-Encoding", "gzip")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d (%s), want 200", resp.StatusCode, raw)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			acknowledged := &coltracepb.ExportTraceServiceResponse{}
			if tc.contentType == "application/json" {
				err = protojson.Unmarshal(raw, acknowledged)
			} else {
				err = proto.Unmarshal(raw, acknowledged)
			}
			if err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if rejected := acknowledged.GetPartialSuccess().GetRejectedSpans(); rejected != 0 {
				t.Errorf("the receiver rejected %d spans", rejected)
			}
			takePayload(t, sink, request)
		})
	}
}

func TestReceiverHTTPRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	receiver, _ := startReceiver(t, 4, 0)
	url := "http://" + receiver.HTTPAddress() + otelfeeder.TracesPath

	cases := []struct {
		name        string
		method      string
		contentType string
		body        string
		want        int
	}{
		{name: "a GET", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "a media type nobody speaks", method: http.MethodPost, contentType: "text/yaml", body: "spans: []", want: http.StatusUnsupportedMediaType},
		{name: "protobuf that is not an export", method: http.MethodPost, contentType: "application/x-protobuf", body: "\xff\xff\xff\xff\xff", want: http.StatusBadRequest},
		{name: "json that is not an export", method: http.MethodPost, contentType: "application/json", body: `{"resourceSpans": 7}`, want: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(t.Context(), tc.method, url, bytes.NewReader([]byte(tc.body)))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestReceiverDropsRatherThanBlocking is the contract at the top of receiver.go: a feeder that
// cannot keep up loses a window's precision, and the exporter it is watching notices nothing.
func TestReceiverDropsRatherThanBlocking(t *testing.T) {
	t.Parallel()
	// A sink nobody reads, and a push timeout short enough that the test is not slow.
	receiver, _ := startReceiver(t, 0, 20*time.Millisecond)

	request := oneExport()
	body, err := proto.Marshal(request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+receiver.HTTPAddress()+otelfeeder.TracesPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: a dropped export is still an accepted one", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the exporter waited %s; the receiver must not block on the feeder", elapsed)
	}
	if dropped := receiver.Stats().Dropped; dropped != 1 {
		t.Errorf("dropped = %d, want 1: the drop must be counted, not hidden", dropped)
	}
}

// TestShutdownEndsTheSource: stopping the receiver is how a live feeder is told the input has
// ended, so that it flushes what it has rather than abandoning it.
func TestShutdownEndsTheSource(t *testing.T) {
	t.Parallel()
	sink := source.NewChanSource(4)
	receiver := &otelfeeder.Receiver{Sink: sink, HTTPAddr: "127.0.0.1:0"}
	if err := receiver.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := receiver.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := sink.Next(t.Context()); !errors.Is(err, io.EOF) {
		t.Errorf("after shutdown the source returned %v, want io.EOF", err)
	}
}

func TestReceiverNeedsSomewhereToListen(t *testing.T) {
	t.Parallel()
	receiver := &otelfeeder.Receiver{Sink: source.NewChanSource(1)}
	if err := receiver.Start(t.Context()); err == nil {
		t.Error("a receiver with no listen address started")
	}
	if err := (&otelfeeder.Receiver{HTTPAddr: "127.0.0.1:0"}).Start(t.Context()); err == nil {
		t.Error("a receiver with no sink started")
	}
}

// TestLiveRunEmitsWhenTheClockMovesOn is the live path end to end — receiver, channel source,
// feeder, emitter — and the one thing a replay can never exercise: a window that closes because
// time passed rather than because a payload arrived (Feeder.Heartbeat).
func TestLiveRunEmitsWhenTheClockMovesOn(t *testing.T) {
	t.Parallel()
	receiver, sink := startReceiver(t, 4, 0)

	body, err := proto.Marshal(oneExport())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+receiver.HTTPAddress()+otelfeeder.TracesPath, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close response: %v", err)
	}

	f := &otelfeeder.Feeder{
		SourceID:  "otel:live",
		Heartbeat: 10 * time.Millisecond,
		// An hour later, so the first heartbeat is past the window's end plus the reordering
		// window and the window closes with nothing more arriving.
		Now: func() time.Time { return fixtureStart.Add(time.Hour) },
	}
	em := emit.NewMemoryEmitter(f.Describe(), emit.WithStrict(false))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, sink, em) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(em.Events()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	events := em.Events()
	if len(events) == 0 {
		t.Fatal("the window never closed; a live feeder with no further traffic said nothing")
	}
	if got := events[0].GetEventId(); got != "otel:live:svc:storefront@w20260901T1300Z" {
		t.Errorf("first event = %s, want the storefront service of the 13:00 window", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if em.Flushes() == 0 {
		t.Error("Run returned without flushing")
	}
}
