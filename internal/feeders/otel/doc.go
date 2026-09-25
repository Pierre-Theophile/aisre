// SPDX-License-Identifier: Apache-2.0

// Package otel is the OpenTelemetry topology feeder (FR-042, research §11).
//
// It receives OTLP trace exports — gRPC on :4317, HTTP/protobuf and HTTP/JSON on :4318 — and
// turns them into the structural half of what the spans say: which services exist, which of
// them call which, how much traffic a call path carries as a coarse class, which third parties
// they depend on, and when a service's version changed. It never stores a span.
//
// Three properties are worth stating before any code:
//
//   - A span is read and dropped. The aggregator keeps counters keyed by service and callee,
//     not spans, and the counters themselves never reach the graph either: only the weight
//     class they produced does (constitution IV, ADR-0001 D1). The attributes that may be
//     copied into a property are a closed list — see allowedResourceAttrs and allowedSpanAttrs
//     in aggregate.go — so a span attribute nobody whitelisted cannot leak into a node.
//
//   - Everything is derived from the aggregation window a span belongs to, never from the
//     order payloads arrived in. A window is keyed by the span's own timestamp, and a window
//     closes when the watermark has passed its end by a full reordering window, so permuting
//     the input inside the window a feeder declares cannot move a span between windows
//     (testkit.Shuffle, FR-021).
//
//   - Event ids are a pure function of (source, kind, subject, window start), so re-delivering
//     a recording is a no-op (FR-018, FR-020). The window part is spelled `@w<key>`, and the
//     key has two formats: `20260901T1300Z` (the default, unambiguous across days) and `1300`
//     (compat, what fixtures/baseline-topology-01 was hand-authored in). See Feeder.IDFormat.
//
// The receiver, the aggregator and the emitter are three files because they fail differently:
// the receiver is I/O and always acks, the aggregator is a pure function of spans, and the
// emitter is where the graph's vocabulary is spoken.
package otel
