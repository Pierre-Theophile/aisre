// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// Log record keys for trace correlation. They match the OpenTelemetry log data model, so a
// collector can lift them into the log record's trace context without a transform.
const (
	// LogKeyTraceID is the key under which the active trace id is injected.
	LogKeyTraceID = "trace_id"
	// LogKeySpanID is the key under which the active span id is injected.
	LogKeySpanID = "span_id"
)

// LogFormat selects the slog handler.
type LogFormat string

const (
	// LogFormatJSON is the default: one JSON object per line, which is what a collector and
	// a `jq` pipeline both want.
	LogFormatJSON LogFormat = "json"
	// LogFormatText is the human-readable handler, for a laptop.
	LogFormatText LogFormat = "text"
)

// LogConfig configures NewLogger.
type LogConfig struct {
	// Level defaults to slog.LevelInfo.
	Level slog.Level
	// Format defaults to LogFormatJSON.
	Format LogFormat
	// Output defaults to os.Stderr.
	Output io.Writer
	// AddSource includes the calling file and line.
	AddSource bool
	// Attrs are added to every record, e.g. service and version.
	Attrs []slog.Attr
}

// ParseLogLevel maps a level name (debug, info, warn, error) onto an slog.Level.
func ParseLogLevel(s string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return 0, err
	}
	return level, nil
}

// NewLogger builds the structured logger the tool uses everywhere. Every record emitted
// inside an active span carries `trace_id` and `span_id`, so a log line found in a collector
// links straight back to the trace that produced it (research §15).
func NewLogger(cfg LogConfig) *slog.Logger {
	output := cfg.Output
	if output == nil {
		output = os.Stderr
	}
	opts := &slog.HandlerOptions{Level: cfg.Level, AddSource: cfg.AddSource}

	var handler slog.Handler
	switch cfg.Format {
	case LogFormatText:
		handler = slog.NewTextHandler(output, opts)
	case LogFormatJSON, "":
		handler = slog.NewJSONHandler(output, opts)
	default:
		handler = slog.NewJSONHandler(output, opts)
	}
	if len(cfg.Attrs) > 0 {
		handler = handler.WithAttrs(cfg.Attrs)
	}
	return slog.New(NewTraceHandler(handler))
}

// TraceHandler wraps an slog.Handler and injects the active span's identifiers into every
// record. Wrap any handler with NewTraceHandler to get the same behaviour from a custom sink.
type TraceHandler struct {
	inner slog.Handler
}

var _ slog.Handler = (*TraceHandler)(nil)

// NewTraceHandler wraps inner so that records logged inside a span carry trace_id and span_id.
func NewTraceHandler(inner slog.Handler) *TraceHandler {
	return &TraceHandler{inner: inner}
}

// Enabled implements slog.Handler.
func (h *TraceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler, adding trace correlation when the context carries a valid,
// sampled-or-not span context. An unsampled span still gets its ids logged: the log line is
// often the only surviving evidence of a dropped trace.
func (h *TraceHandler) Handle(ctx context.Context, record slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.HasTraceID() {
		record.AddAttrs(slog.String(LogKeyTraceID, sc.TraceID().String()))
	}
	if sc.HasSpanID() {
		record.AddAttrs(slog.String(LogKeySpanID, sc.SpanID().String()))
	}
	return h.inner.Handle(ctx, record)
}

// WithAttrs implements slog.Handler.
func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TraceHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h *TraceHandler) WithGroup(name string) slog.Handler {
	return &TraceHandler{inner: h.inner.WithGroup(name)}
}
