// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"context"
	"errors"
	"fmt"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// The connector contract (FR-044, FR-045, contracts/feeder-sdk.md).
//
// A feeder is a pure function from raw payloads to typed events. Everything that varies
// between running live and running from a recording is pushed into two interfaces — Source
// (where payloads come from) and Emitter (where events go) — so that the feeder itself has one
// code path and the recorded mode is the test (FR-044).
//
// Three rules follow from the constitution and are worth stating before any code is written:
//
//   - A feeder never queries the graph and never exposes data any other way. Emitting events
//     is the whole of its contract with the rest of the system (constitution I).
//   - A feeder never emits telemetry. A metric sample, a log line or a span in a property is
//     refused by every Emitter in this SDK before it leaves the process (constitution IV); a
//     feeder attaches a Pointer that says where the telemetry lives instead.
//   - A feeder uses read-only credentials and refuses to start without them (FR-046). The SDK
//     cannot check that for you — only the source system knows what a scope grants — so
//     Description.RequiredScopes documents what you asked for and your Run must verify it.

// SchemaVersion is the event schema version this SDK emits. It is the value a Description
// leaves empty defaults to, and it is what fixtures record as `schema_version`.
const SchemaVersion = "1.0.0"

// SDKVersion is the semantic version of this SDK, recorded in a fixture's `sdk_version`.
// MINOR adds helpers, MAJOR changes the interfaces in this file (contracts/feeder-sdk.md
// §Versioning).
const SDKVersion = "0.1.0"

// Ordering is the delivery guarantee a feeder declares for its own events.
type Ordering string

const (
	// OrderingPerSourceSequence means the feeder numbers its events with a monotonic
	// per-source sequence (a Kubernetes resourceVersion, a log offset). The graph still never
	// reorders; the number is what makes a gap detectable.
	OrderingPerSourceSequence Ordering = "per_source_sequence"
	// OrderingNone means the feeder makes no ordering promise beyond its reordering window.
	OrderingNone Ordering = "none"
)

// String renders the ordering as it is stored in `log.sources` and written in a fixture
// manifest.
func (o Ordering) String() string {
	if o == "" {
		return string(OrderingNone)
	}
	return string(o)
}

// Valid reports whether o is one of the two published orderings. The empty string is valid and
// means OrderingNone.
func (o Ordering) Valid() bool {
	switch o {
	case "", OrderingNone, OrderingPerSourceSequence:
		return true
	default:
		return false
	}
}

// Description is everything the graph and the test harness need to know about a feeder before
// it emits anything. It is registered with the graph once (RegisterSource) and copied into a
// fixture's manifest, so two feeders that describe themselves differently are two sources.
type Description struct {
	// SourceID is this feeder's identity, e.g. "k8s:prod-eu1". It prefixes every event id it
	// mints, and a feeder token is scoped to exactly one of them (FR-046).
	SourceID string
	// Kind is the connector family: "k8s", "otel", and later vendors. Fixtures group payload
	// directories by it.
	Kind string
	// SchemaVersion is the event schema version the feeder emits. Empty means SchemaVersion.
	SchemaVersion string
	// Ordering is the delivery guarantee. Empty means OrderingNone.
	Ordering Ordering
	// ReorderingWindow is how far out of order the feeder may deliver its own events
	// (FR-021). It is the window testkit.Shuffle permutes payloads within, so declaring a
	// window wider than the truth makes the conformance test stricter, never weaker.
	ReorderingWindow time.Duration
	// RequiredScopes documents the read-only permissions the feeder asks the source system
	// for, e.g. "get,list,watch on apps/v1 deployments". It is documentation and a startup
	// checklist, not an enforcement point (FR-046).
	RequiredScopes []string
	// Namespaces are the Ref namespaces the feeder claims to emit identities in, e.g.
	// NSK8sDeployment. testkit fails a run that emits a Ref outside this set, which is what
	// stops one connector quietly minting identifiers another connector already owns. An
	// empty list disables the check.
	Namespaces []string
}

// Version returns the event schema version the feeder emits, defaulting to SchemaVersion.
func (d Description) Version() string {
	if d.SchemaVersion == "" {
		return SchemaVersion
	}
	return d.SchemaVersion
}

// Validate refuses a description that could not produce a well-formed event. A feeder should
// call it in Run before it does anything else.
func (d Description) Validate() error {
	if d.SourceID == "" {
		return errors.New("feeder: Description.SourceID is required; it is the identity every event and every token is scoped to")
	}
	if d.Kind == "" {
		return fmt.Errorf("feeder: Description.Kind is required for source %s (the connector family: k8s, otel, ...)", d.SourceID)
	}
	if !d.Ordering.Valid() {
		return fmt.Errorf("feeder: Description.Ordering %q is neither %q nor %q", d.Ordering, OrderingPerSourceSequence, OrderingNone)
	}
	if d.ReorderingWindow < 0 {
		return fmt.Errorf("feeder: Description.ReorderingWindow is negative (%s)", d.ReorderingWindow)
	}
	return nil
}

// RegisterRequest renders the description as the registration call every Emitter makes before
// its first event (FR-018).
func (d Description) RegisterRequest() *graphv1.RegisterSourceRequest {
	return &graphv1.RegisterSourceRequest{
		SourceId:                d.SourceID,
		Kind:                    d.Kind,
		Ordering:                d.Ordering.String(),
		ReorderingWindowSeconds: int64(d.ReorderingWindow / time.Second),
		SchemaVersion:           d.Version(),
	}
}

// DeclaresNamespace reports whether ns is one the feeder said it would emit. A description
// that names no namespaces declares all of them.
func (d Description) DeclaresNamespace(ns string) bool {
	if len(d.Namespaces) == 0 {
		return true
	}
	for _, declared := range d.Namespaces {
		if declared == ns {
			return true
		}
	}
	return false
}

// Payload is one raw observation as the source system produced it: a watch event, an OTLP
// export request, a webhook body. The SDK never looks inside Bytes.
type Payload struct {
	// Kind groups payloads that are decoded the same way, e.g. "deployments", "traces". It is
	// the directory name a recording stores the payload under.
	Kind string
	// At is when the source produced the payload. It is what a recording replays in order and
	// what testkit.Shuffle permutes within the reordering window.
	At time.Time
	// Seq is the source-native sequence number, when there is one (a Kubernetes
	// resourceVersion, a Kafka offset). Zero means the source has none.
	Seq int64
	// Bytes is the payload itself, unmodified. JSON for most sources, protobuf for OTLP.
	Bytes []byte
}

// Source abstracts live versus recorded input so both modes share one code path (FR-044).
type Source interface {
	// Next returns the next raw payload, or io.EOF when the source is exhausted. A recorded
	// source ends; a live source blocks until ctx is done and then returns ctx.Err().
	Next(ctx context.Context) (Payload, error)
}

// Emitter validates and forwards events. Validation errors are returned, never swallowed.
type Emitter interface {
	// Emit sends one event. The returned IngestResult says whether the graph applied it, saw
	// it before (DUPLICATE_NOOP) or refused it (REJECTED with a published reason code). An
	// error means the event could not be delivered at all and the feeder should retry or stop;
	// a REJECTED result is an answer, not a failure.
	//
	// A batching Emitter may return a nil result because the graph has not answered yet; see
	// the implementation's documentation.
	Emit(ctx context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error)
	// Checkpoint records that the feeder has observed everything in [fact.ExtentFrom,
	// fact.ExtentTo). fact.GapBefore marks a resync after a gap, so the graph knows the silence
	// before it was ignorance rather than absence (FR-032). A feeder checkpoints on start, on
	// resync and after any gap. The implementation mints the deterministic event id; everything
	// else is the feeder's to state.
	//
	// It takes the whole CheckpointFact rather than three of its fields, and the reason is a
	// defect this signature caused. It used to be `(ctx, from, to, gapBefore)`, which cannot
	// carry `Note` — and Note is not decoration: FR-057 asks a checkpoint to say **which scope
	// was in force**, and FR-012's distinction between "nothing happened" and "nobody was
	// watching" is read off it. So the GCP feeder built a rich checkpoint with an audit scope, a
	// region scope, held shifts, suppressions and a measured lag distribution, validated it,
	// asserted its `Note()` in four test files — and then handed three fields to this method and
	// dropped the rest on the floor. The Kubernetes feeder had already hit the same wall and
	// worked around it by bypassing this method entirely, which left the hole for the next
	// three connectors to fall into.
	//
	// Passing the fact means the compiler finds every caller when a field is added, and there is
	// one door rather than one door and a way around it.
	Checkpoint(ctx context.Context, fact CheckpointFact) error
	// Flush delivers everything buffered and waits for its results. A feeder calls it before
	// returning from Run and whenever it needs the graph to be up to date.
	Flush(ctx context.Context) error
}

// Feeder turns one external system into typed graph events.
type Feeder interface {
	// Describe returns the feeder's contract. It must be a pure function: the harness calls
	// it before Run and compares what Run emits against it.
	Describe() Description
	// Run reads from src and writes to em until src is exhausted or ctx is done.
	//
	// Returning nil means "the source ended and everything was emitted"; a recorded run is
	// expected to end that way. Run must call em.Flush before it returns, and must verify its
	// credentials are read-only before it emits anything (FR-046).
	Run(ctx context.Context, src Source, em Emitter) error
}
