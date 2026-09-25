// SPDX-License-Identifier: Apache-2.0

package otel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The feeder itself (T053, FR-042, FR-044).
//
// Run is one loop over one Source, so live and recorded are the same code path and the
// recorded mode is the test. The only thing the two modes do not share is where the clock
// comes from: replaying, it is each payload's own arrival time, so a recording replays its own
// timing whatever wall clock it runs against; live, payload arrivals are still the clock, and
// a heartbeat advances it while nothing is arriving so that a quiet window still closes.

// PayloadKindTraces is the payload kind this feeder reads, and the directory a recording
// stores exports under.
const PayloadKindTraces = "traces"

// shutdownFlush bounds the final flush when the context is already done. Without it a
// cancelled run would drop the window it had just closed.
const shutdownFlush = 5 * time.Second

// DeclaredReorderingWindow is the window this feeder promises a consumer in Describe: how far
// out of order two of *its own events* may be observed (FR-021, FR-048).
//
// It is a constant, deliberately independent of --window, and it is small. The reasoning:
//
//   - The feeder emits window by window, and `Aggregator.take` sorts every batch of closed
//     windows by window start. Emission order across windows is therefore always time order.
//     The feeder never reorders, and the only events that can genuinely be observed out of
//     order are the ones inside a single flush — one window's nodes, edges, retractions and
//     checkpoint, handed to the emitter in one batch and accepted milliseconds apart. A
//     batching emitter, a retry or a concurrent ingest may shuffle *those*.
//   - Two events from *different* flushes are separated by at least one tick of the clock
//     that drives the watermark: a payload arrival, or — while nothing is arriving — the 15 s
//     heartbeat of `aisre feed otel` (internal/cli/feed_otel.go). Thirty seconds is twice
//     that and comfortably covers a whole flush.
//
// It used to carry a third job, and no longer does. Spanning two flushes that talk about the
// same edge used to be unsafe, because a retraction delivered before a later re-assertion of
// one call path lost the interval the re-assertion opened; the previous value, the aggregation
// window itself, licensed exactly that swap and the shuffle step of `fixture verify` found it
// on `fixtures/feeder-gap-01` (a retraction and a re-assertion 296 s apart inside a declared
// 300 s window). The projector survives that ordering now — a coalesced segment remembers the
// first instant each source restated it and a retraction splits rather than truncates
// (internal/projector/segments.go) — so 30 s is a statement about this feeder's flushes, not a
// hedge against the graph. It stays 30 s because that is what the flush reasoning above
// supports; declaring more would be claiming something unmeasured.
//
// The aggregator's own lateness allowance is a different number and stays the aggregation
// window: that one is about how late a *span* may arrive from an exporter, which is an input
// property, not a promise this feeder makes downstream.
const DeclaredReorderingWindow = 30 * time.Second

// Feeder derives topology from OTLP trace exports.
//
// The zero value is not usable: SourceID is required. Everything else has a published default.
type Feeder struct {
	// SourceID is this feeder's identity, e.g. "otel:demo". Every event id it mints starts
	// with it and its token is scoped to it.
	SourceID string
	// Window is the aggregation window, aligned to the wall clock. Zero means DefaultWindow.
	Window time.Duration
	// RetractAfter is how many windows a call path may go unobserved before it is retracted.
	// Zero means DefaultRetractAfter; a negative value disables retraction.
	RetractAfter int
	// IDFormat is how a window is spelled inside an event id. Empty means IDFormatFull.
	IDFormat IDFormat
	// PointerCompat replays a corpus recorded before a pointer field existed. Zero — the
	// default, and what a live run wants — emits every field this SDK knows, join keys
	// included (ADR-0005 D6).
	PointerCompat feeder.PointerCompat
	// TraceBackend, MetricBackend and LatencyMetric name what a pointer points at. Empty
	// means DefaultTraceBackend, DefaultMetricBackend and DefaultLatencyMetric.
	TraceBackend  string
	MetricBackend string
	LatencyMetric string
	// Heartbeat is how often a live run advances its clock while no payload is arriving, so
	// that a window with no traffic in it still closes. Zero — the default, and what a replay
	// wants — means the clock only moves when a payload does.
	Heartbeat time.Duration
	// Now is the wall clock a heartbeat reads. Nil means time.Now.
	Now func() time.Time
	// Logger receives the feeder's own counters. Nil means slog.Default.
	Logger *slog.Logger
}

var _ feeder.Feeder = (*Feeder)(nil)

// Describe is this feeder's contract (see the SDK guide §Describe).
//
// Ordering is none: an OTLP exporter numbers nothing this feeder could use as a sequence, and
// a batch may arrive after one exported later. The reordering window is
// DeclaredReorderingWindow, which documents why it is a constant and why it is not the
// aggregation window.
//
// RequiredScopes is empty, and is the one place where FR-046 reads differently for this
// connector than for a polling one: a receiver has no credential on the source system and
// makes no call to it, so there is no write scope it could be holding. What it does hold is a
// feeder-role token scoped to SourceID, which the graph checks on every event.
func (f *Feeder) Describe() feeder.Description {
	return feeder.Description{
		SourceID:         f.SourceID,
		Kind:             "otel",
		Ordering:         feeder.OrderingNone,
		ReorderingWindow: DeclaredReorderingWindow,
		Namespaces: []string{
			feeder.NSOTelService,
			feeder.NSServerAddress,
			feeder.NSOTelChange,
		},
	}
}

// Run reads OTLP exports from src until it is exhausted or ctx is done, emitting one batch of
// events per closed aggregation window.
func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	desc := f.Describe()
	if err := desc.Validate(); err != nil {
		return err
	}
	if !f.IDFormat.Valid() {
		return fmt.Errorf("otel: --id-format %q: want %s or %s", f.IDFormat, IDFormatFull, IDFormatCompat)
	}

	log := f.logger()
	// The aggregator's lateness allowance is the aggregation window, not desc.ReorderingWindow:
	// how long a window is held open for straggling *spans* is a property of the exporters
	// feeding this receiver, while the declared window is a promise about this feeder's own
	// event order (DeclaredReorderingWindow). Holding a window open for longer than promised
	// loses nothing — events are merely emitted later — so the two numbers are free to differ.
	agg := NewAggregator(f.windowLength(), f.windowLength(), log)
	out := newEmitter(f)

	for {
		payload, err := f.next(ctx, src)
		switch {
		case errors.Is(err, errHeartbeat):
			agg.Advance(f.now())
		case errors.Is(err, io.EOF):
			return f.finish(ctx, em, agg, out, log)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// A live run ends by cancellation, and what it has already aggregated is still
			// true. Finish on a context the cancellation cannot reach.
			log.Info("otel: shutting down, flushing the open windows")
			stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownFlush)
			defer cancel()
			return f.finish(stop, em, agg, out, log)
		case err != nil:
			return err
		default:
			if err := f.consume(agg, payload, log); err != nil {
				return err
			}
		}
		if err := f.emitClosed(ctx, em, agg, out, log); err != nil {
			return err
		}
	}
}

// consume decodes one payload into the aggregator. A payload that is not an OTLP trace export
// is logged and skipped rather than fatal: a recording may hold several kinds, and a feeder
// that dies on the first unfamiliar one cannot be pointed at a shared fixture.
func (f *Feeder) consume(agg *Aggregator, payload feeder.Payload, log *slog.Logger) error {
	if payload.Kind != "" && payload.Kind != PayloadKindTraces {
		log.Debug("otel: payload of another kind skipped", "kind", payload.Kind)
		return nil
	}
	if len(payload.Bytes) == 0 {
		return nil
	}
	req := &coltracepb.ExportTraceServiceRequest{}
	if err := proto.Unmarshal(payload.Bytes, req); err != nil {
		return fmt.Errorf("otel: decode OTLP export of %d bytes: %w", len(payload.Bytes), err)
	}
	agg.Add(req, payload.At)
	return nil
}

// emitClosed emits every window the watermark has moved past.
func (f *Feeder) emitClosed(ctx context.Context, em feeder.Emitter, agg *Aggregator, out *emitter, log *slog.Logger) error {
	for _, w := range agg.Closed() {
		if err := f.emitWindow(ctx, em, out, w, log); err != nil {
			return err
		}
	}
	return nil
}

// finish emits the windows still open and flushes, which is how a recording's last window
// reaches the graph.
func (f *Feeder) finish(ctx context.Context, em feeder.Emitter, agg *Aggregator, out *emitter, log *slog.Logger) error {
	for _, w := range agg.Drain() {
		if err := f.emitWindow(ctx, em, out, w, log); err != nil {
			return err
		}
	}
	stats := agg.Stats()
	log.Info("otel: run finished",
		"spans", stats.Spans, "windows", stats.Windows,
		"late_spans", stats.LateSpans, "resources_without_service_name", stats.SkippedResources)
	return em.Flush(ctx)
}

func (f *Feeder) emitWindow(ctx context.Context, em feeder.Emitter, out *emitter, w *windowAgg, log *slog.Logger) error {
	if err := out.emitWindow(ctx, em, w); err != nil {
		return err
	}
	log.Debug("otel: window closed",
		"window_start", w.start.Format(time.RFC3339),
		"services", len(w.services), "callees", len(w.callees), "edges", len(w.edges))
	return nil
}

// errHeartbeat is how next reports that nothing arrived before the heartbeat elapsed. It is
// not a failure: it is the clock moving on its own.
var errHeartbeat = errors.New("otel: heartbeat")

// next takes the next payload, bounded by the heartbeat when there is one.
func (f *Feeder) next(ctx context.Context, src feeder.Source) (feeder.Payload, error) {
	if f.Heartbeat <= 0 {
		return src.Next(ctx)
	}
	beat, cancel := context.WithTimeout(ctx, f.Heartbeat)
	defer cancel()
	payload, err := src.Next(beat)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return feeder.Payload{}, errHeartbeat
	}
	return payload, err
}

func (f *Feeder) windowLength() time.Duration {
	if f.Window <= 0 {
		return DefaultWindow
	}
	return f.Window
}

func (f *Feeder) retractWindows() int {
	if f.RetractAfter == 0 {
		return DefaultRetractAfter
	}
	return f.RetractAfter
}

func (f *Feeder) idFormat() IDFormat {
	if f.IDFormat == "" {
		return IDFormatFull
	}
	return f.IDFormat
}

func (f *Feeder) logger() *slog.Logger {
	if f.Logger == nil {
		return slog.Default()
	}
	return f.Logger
}

func (f *Feeder) now() time.Time {
	if f.Now == nil {
		return time.Now().UTC()
	}
	return f.Now().UTC()
}
