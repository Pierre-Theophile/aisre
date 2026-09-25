// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// The two front doors, one shape (FR-002, FR-002a, FR-008b, ADR-0005 D2).
//
// A monitor transition and a human declaration are the same event seen from two sides, and this
// package's whole job is to say so in code: both normalise into one `Symptom`, both carry the
// same 4-tuple idempotency key, both pin the same two instants, and everything downstream — the
// grouping rule, the resolver, the ledger, the renderer — reads one shape.
//
// What is deliberately *not* here:
//
//   - no guessing. A declaration with no parseable target gets no invented entity; it gets an
//     `unknown` outcome and a request for the target (FR-002b, FR-026);
//   - no clock. The observed instant is the symptom instant, never "now", unless review mode was
//     asked for explicitly and is recorded (FR-004, FR-004a, FR-005);
//   - no parse rules. The registry of naming rules that turn a declaration's title into entity
//     references is a *connector* artifact (FR-002b, U2). What an investigation records is the
//     rule id and the rule version that produced a reference, so a later reader can resolve the
//     rule from the connector that owns it. This engine never interprets a rule's text.

// NormalisationVersion is the version of the normalisation this package implements. It is
// recorded so that a fixture normalised by an older build stays interpretable.
const NormalisationVersion = "1.0.0"

// DefaultLookback is the look-back window `investigate` uses when none is given
// (contracts/cli.md: `--lookback 90m`).
const DefaultLookback = 90 * time.Minute

// Transports, exactly as `investigation.symptoms.transport` accepts them.
const (
	// TransportMonitor is a monitor or policy state transition.
	TransportMonitor = "monitor"
	// TransportHumanDeclared is an incident a person declared (FR-001a).
	TransportHumanDeclared = "human_declared"
	// TransportExplicitReference is `investigate <ns>=<value> --at T`: a person pointing at a
	// node rather than at an alert.
	TransportExplicitReference = "explicit_reference"
)

// Actor kinds, exactly as `investigation.symptoms.actor_kind` accepts them. This is the row's
// actor kind, which is not the change actor kind of ADR-0005 D3: a monitor transition has no
// change actor at all (FR-029d).
const (
	// ActorKindMonitor is a machine observing a threshold.
	ActorKindMonitor = "monitor"
	// ActorKindHuman is a person declaring an incident.
	ActorKindHuman = "human"
)

// Intake is a normalised front door: one symptom, the two pinned instants, and the window to
// reason over.
//
// It is what both NormaliseAlert and NormaliseDeclaration return, and what Group consumes.
type Intake struct {
	// Symptom is the normalised symptom, ready to be written to `investigation.symptoms`.
	Symptom *investigationv1.Symptom
	// ValidAt is the instant the investigation is about: the transition instant, or the instant
	// of declaration (FR-004, FR-004a).
	ValidAt time.Time
	// ObservedAt is what the investigation is entitled to know. It defaults to ValidAt so that
	// facts the system learned later are invisible (FR-004); review mode is the only way it may
	// run ahead, and it is recorded (FR-005).
	ObservedAt time.Time
	// ReviewMode records that ObservedAt was "now" rather than pinned. Never the default for an
	// alert-driven run (FR-005).
	ReviewMode bool
	// Window is the half-open look-back window `[ValidAt-Lookback, ValidAt)`.
	Window *investigationv1.Window
	// Lookback is the width of Window, retained so a re-normalisation reproduces it.
	Lookback time.Duration
	// UnresolvedTargets is true when the intake names no entity at all. It is not an error: the
	// investigation opens, and reaches `unknown` with a request for the target (FR-002b, FR-026).
	UnresolvedTargets bool
}

// Origin reports the intake origin kind — `monitor` or `declared` — carried on the symptom.
func (in *Intake) Origin() investigationv1.IntakeOrigin {
	if in == nil || in.Symptom == nil {
		return investigationv1.IntakeOrigin_INTAKE_ORIGIN_UNSPECIFIED
	}
	return in.Symptom.GetOrigin()
}

// IdempotencyKey is the published 4-tuple key of the transition or declaration (FR-008b).
func (in *Intake) IdempotencyKey() string {
	if in == nil || in.Symptom == nil {
		return ""
	}
	return in.Symptom.GetIdempotencyKey()
}

// Errors intake raises. Every one of them is a refusal to invent something.
var (
	// ErrNoSubject is an intake that names nothing at all and carries no origin to key on.
	ErrNoSubject = errors.New("intake names no subject and no origin")
	// ErrNoInstant is an intake with no transition or declaration instant. The engine will not
	// substitute the instant it observed the input (FR-004a).
	ErrNoInstant = errors.New("intake carries no instant")
	// ErrAnonymous is a declaration with no authenticated declaring identity (FR-066).
	ErrAnonymous = errors.New("declaration carries no authenticated identity")
	// ErrObservedBeforeValid is an observed instant earlier than the valid instant outside
	// review mode, which the schema's observed-time pin refuses.
	ErrObservedBeforeValid = errors.New("observed instant runs ahead of the valid instant outside review mode")
)

// lookbackOr returns the given look-back, or DefaultLookback when it is not positive.
func lookbackOr(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultLookback
	}
	return d
}

// windowFor builds the half-open look-back window `[at-lookback, at)`.
func windowFor(at time.Time, lookback time.Duration) *investigationv1.Window {
	return &investigationv1.Window{
		Start: timestamppb.New(at.Add(-lookback).UTC()),
		End:   timestamppb.New(at.UTC()),
	}
}

// pin resolves the two instants. `observedAt` zero means "default to the symptom instant"
// (FR-004); review mode is the recorded exception where it may be later (FR-005).
//
// The rule it enforces is the schema's: outside review mode `observed_at <= valid_at`. An
// observed instant *earlier* than the valid instant is legal and meaningful — it is what a
// replay of a historical investigation looks like — but one that is later is the engine knowing
// something it should not.
func pin(validAt, observedAt time.Time, reviewMode bool, now func() time.Time) (time.Time, time.Time, error) {
	if validAt.IsZero() {
		return time.Time{}, time.Time{}, ErrNoInstant
	}
	validAt = validAt.UTC()
	switch {
	case reviewMode && observedAt.IsZero():
		if now == nil {
			now = time.Now
		}
		observedAt = now()
	case observedAt.IsZero():
		observedAt = validAt
	}
	observedAt = observedAt.UTC()
	if !reviewMode && observedAt.After(validAt) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: observed %s, valid %s",
			ErrObservedBeforeValid, observedAt.Format(time.RFC3339Nano), validAt.Format(time.RFC3339Nano))
	}
	return validAt, observedAt, nil
}

// timestampOf converts an instant to the wire form, in UTC.
func timestampOf(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t.UTC()) }
