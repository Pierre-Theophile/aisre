// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// Local validation (FR-024, constitution IV).
//
// Validate is the same function the ingest server runs, called before the event leaves the
// process. Running it twice is not duplication: a feeder that learns at build time that its
// props carry a telemetry payload fixes it in an afternoon, while one that learns from a
// REJECTED result in production has already shipped. The rules — and in particular the
// telemetry denylist — live in exactly one place, internal/log/validate.go, and are reached
// from here rather than copied.
//
// The reason codes below are aliases of that package's constants, re-exported because the
// vocabulary is part of the published schema (constitution IX): a feeder matches on these
// strings and they may only change with a version bump.

// Reason codes a local validation can produce. They are the same strings the graph returns in
// IngestResult.reason_code.
const (
	// ReasonUntyped is an envelope with no body: there is no event.
	ReasonUntyped = eventlog.ReasonUntyped
	// ReasonUnknownSchemaVersion is an envelope whose schema_version this build does not
	// accept. The detail names the versions that are accepted.
	ReasonUnknownSchemaVersion = eventlog.ReasonUnknownSchemaVersion
	// ReasonMissingValidTime is an assertion with no source-asserted valid time. Set
	// ValidFromUnknown to assert an unknown start; never guess one (FR-011).
	ReasonMissingValidTime = eventlog.ReasonMissingValidTime
	// ReasonTelemetryPayload is a property carrying telemetry rather than a pointer to it:
	// a denylisted key at any depth, a series of two or more numbers, or a value above the
	// property size limit (constitution IV, FR-009).
	ReasonTelemetryPayload = eventlog.ReasonTelemetryPayload
	// ReasonSecretValue is a config node of kind secret carrying the secret's material
	// instead of only its version identifier.
	ReasonSecretValue = eventlog.ReasonSecretValue
	// ReasonUnknownType is a node or edge type left unspecified, or a change with no kind.
	ReasonUnknownType = eventlog.ReasonUnknownType
	// ReasonMissingRef is a body omitting an identity it needs: the subject of an upsert, an
	// endpoint of an edge, the claim of an identity assertion.
	ReasonMissingRef = eventlog.ReasonMissingRef
	// ReasonMissingSource is an envelope with no source_id.
	ReasonMissingSource = eventlog.ReasonMissingSource
	// ReasonMissingEventID is an envelope with no event_id; without one nothing can be
	// idempotent (FR-018).
	ReasonMissingEventID = eventlog.ReasonMissingEventID
	// ReasonUnknownSource is an envelope from a source that never registered. Only the graph
	// can decide it, so Validate never returns it; it is published here because a feeder that
	// forgot to register sees it on the wire.
	ReasonUnknownSource = eventlog.ReasonUnknownSource
	// ReasonRefUnresolvable is a retraction naming an entity the graph has never seen. Only
	// the graph can decide it, for the same reason as ReasonUnknownSource.
	ReasonRefUnresolvable = eventlog.ReasonRefUnresolvable
)

// Rejection is a machine-readable refusal: the published reason code plus the field that
// caused it, in proto JSON path form, so a feeder author can fix the event without reading the
// graph's source (FR-024).
type Rejection struct {
	// ReasonCode is one of the Reason* constants.
	ReasonCode string
	// ReasonDetail names the offending field, e.g. "upsert_node.props.latency_samples", and
	// says what to do about it.
	ReasonDetail string
}

// Error makes Rejection an error.
func (r *Rejection) Error() string {
	if r == nil {
		return ""
	}
	if r.ReasonDetail == "" {
		return r.ReasonCode
	}
	return r.ReasonCode + ": " + r.ReasonDetail
}

// AcceptedSchemaVersions returns the event schema versions this build of the graph accepts. A
// feeder whose Description.SchemaVersion is not among them will have every event refused with
// ReasonUnknownSchemaVersion.
func AcceptedSchemaVersions() []string {
	return append([]string(nil), eventlog.DefaultSchemaVersions...)
}

// Validate checks an envelope against the published contract and returns nil when the graph
// will accept it, as far as anything decidable without the database goes.
//
// What it cannot decide is deliberate: whether the source is registered, whether the
// idempotency key has been seen, and whether a retraction's ref resolves are all facts about a
// graph, not about an event. Those come back from the Emitter as REJECTED results carrying
// ReasonUnknownSource or ReasonRefUnresolvable.
func Validate(ev *graphv1.EventEnvelope) *Rejection {
	return convertRejection(eventlog.Validate(ev, eventlog.DefaultSchemaVersions))
}

// convertRejection lifts the internal refusal into the published one, so that the SDK's public
// surface never names an internal type.
func convertRejection(r *eventlog.Rejection) *Rejection {
	if r == nil {
		return nil
	}
	return &Rejection{ReasonCode: r.ReasonCode, ReasonDetail: r.ReasonDetail}
}
