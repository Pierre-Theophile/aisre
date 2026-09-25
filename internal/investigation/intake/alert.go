// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"fmt"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// The monitor front door (T076, FR-001, FR-002, FR-004, FR-005).
//
// A monitor transition arrives as the published `alert.transition` event body (ADR-0005 D2), and
// normalisation takes four things out of it: the entities it watches (the subject references),
// a symptom statement, the instant it fired, and the window to reason over. The origin reference
// is retained verbatim, because "which alert was this?" is the first question a reviewer asks.
//
// Two decisions worth stating.
//
// **The observed instant is the transition instant.** Not the instant the engine got round to
// investigating, and not "now". The investigation is entitled to know what the graph knew when
// the monitor fired, and nothing more (FR-004). Review mode is the single, recorded exception
// (FR-005) and it is never the default for an alert-driven run — the flag has to be set.
//
// **A transition carries no change actor kind.** `AlertTransition.actor_kind` exists for the
// declaration door, where the actor is the person who declared. A monitor observing a threshold
// is not an actor in the ADR-0005 D3 sense (FR-029d), so the symptom row's `actor_kind` is
// `monitor` and the change actor kind stays empty.

// MonitorIntake is a monitor or policy state transition, as the published event body carries it.
type MonitorIntake struct {
	// SourceID is the feeder that observed the transition. It is the first element of the
	// published 4-tuple key (FR-008b).
	SourceID string
	// Transition is the event body. Required.
	Transition *graphv1.AlertTransition
	// Statement overrides the symptom statement. Empty means "derive one from the transition",
	// which is what a connector that supplies no title gets.
	Statement string
	// Lookback is the window width; zero means DefaultLookback.
	Lookback time.Duration
	// ReviewMode runs the investigation with the observed instant at "now" (FR-005). It is
	// recorded on the investigation and is never the default here.
	ReviewMode bool
	// ObservedAt overrides the observed instant. Zero means the transition instant outside
	// review mode, and Now() inside it.
	ObservedAt time.Time
	// Now is the clock review mode reads. Nil means time.Now; tests pass a fake one.
	Now func() time.Time
}

// NormaliseAlert turns a monitor transition into the one symptom shape (FR-002).
//
// It never invents: a transition that watches nothing yields a symptom with no named identifier
// and UnresolvedTargets set, which the engine carries to `unknown` under FR-026 rather than
// guessing a service from the monitor's name.
func NormaliseAlert(in MonitorIntake) (*Intake, error) {
	if in.Transition == nil {
		return nil, fmt.Errorf("normalise alert: %w", ErrNoSubject)
	}
	body := in.Transition
	if body.GetTransitionAt() == nil {
		return nil, fmt.Errorf("normalise alert: %w", ErrNoInstant)
	}
	firedAt := body.GetTransitionAt().AsTime()

	validAt, observedAt, err := pin(firedAt, in.ObservedAt, in.ReviewMode, in.Now)
	if err != nil {
		return nil, fmt.Errorf("normalise alert: %w", err)
	}
	lookback := lookbackOr(in.Lookback)

	named := make([]*graphv1.Ref, 0, len(body.GetWatches()))
	targets := make([]*investigationv1.TargetRef, 0, len(body.GetWatches()))
	for _, watched := range body.GetWatches() {
		if watched.GetNamespace() == "" && watched.GetValue() == "" {
			continue
		}
		named = append(named, watched)
		// A monitor's own WATCHES edges are the provenance: the graph says what this monitor
		// watches, so the reference is neither parsed from prose nor supplied by a person.
		targets = append(targets, &investigationv1.TargetRef{
			Ref:        watched,
			Provenance: investigationv1.TargetRefProvenance_FROM_MONITOR,
		})
	}

	symptom := &investigationv1.Symptom{
		SymptomId:        SymptomID(in.SourceID, body),
		OriginSystem:     in.SourceID,
		OriginRef:        originRefOf(body),
		Transport:        TransportMonitor,
		Statement:        statementOf(in.Statement, body),
		FiredAt:          body.GetTransitionAt(),
		NamedIdentifiers: named,
		Origin:           investigationv1.IntakeOrigin_INTAKE_ORIGIN_MONITOR,
		Severity:         body.GetSeverity(),
		Title:            body.GetTitle(),
		TargetRefs:       targets,
		IdempotencyKey:   eventlog.AlertTransitionKey(in.SourceID, body),
	}

	return &Intake{
		Symptom:           symptom,
		ValidAt:           validAt,
		ObservedAt:        observedAt,
		ReviewMode:        in.ReviewMode,
		Window:            windowFor(validAt, lookback),
		Lookback:          lookback,
		UnresolvedTargets: len(named) == 0,
	}, nil
}

// ExplicitReference is `investigate <ns>=<value> --at T`: a person pointing at a node rather
// than at an alert. It is the third transport, and it normalises into the same shape so that
// nothing downstream has to know which door was used.
type ExplicitReference struct {
	// Ref is the node reference the person typed. Required.
	Ref *graphv1.Ref
	// At is the instant to investigate. Required (FR-001).
	At time.Time
	// Statement is the symptom in words; empty derives one from the reference.
	Statement string
	// Requester is the authenticated identity that asked. Recorded as the declaring identity so
	// that "who asked this question?" is answerable from the symptom row alone.
	Requester string
	// Lookback, ReviewMode, ObservedAt and Now behave as on MonitorIntake.
	Lookback   time.Duration
	ReviewMode bool
	ObservedAt time.Time
	Now        func() time.Time
}

// NormaliseReference turns an explicit node reference into the one symptom shape.
func NormaliseReference(in ExplicitReference) (*Intake, error) {
	ref := graph.RefFromProto(in.Ref)
	if ref.IsZero() {
		return nil, fmt.Errorf("normalise reference: %w", ErrNoSubject)
	}
	validAt, observedAt, err := pin(in.At, in.ObservedAt, in.ReviewMode, in.Now)
	if err != nil {
		return nil, fmt.Errorf("normalise reference: %w", err)
	}
	lookback := lookbackOr(in.Lookback)

	statement := in.Statement
	if strings.TrimSpace(statement) == "" {
		statement = "explicit investigation of " + ref.String()
	}
	// The key is the same 4-tuple: the source is the tool, the stable identifier is the node,
	// the group is empty and the instant is the one asked about. Two people asking the same
	// question about the same node at the same instant ask one question.
	key := eventlog.AlertTransitionKeyParts("investigate", ref.String(), "",
		validAt.Format(time.RFC3339Nano))

	symptom := &investigationv1.Symptom{
		SymptomId:         key,
		OriginSystem:      "investigate",
		OriginRef:         ref.String(),
		Transport:         TransportExplicitReference,
		Statement:         statement,
		FiredAt:           timestampOf(validAt),
		NamedIdentifiers:  []*graphv1.Ref{in.Ref},
		Origin:            investigationv1.IntakeOrigin_INTAKE_ORIGIN_MONITOR,
		DeclaringIdentity: in.Requester,
		TargetRefs: []*investigationv1.TargetRef{{
			Ref:        in.Ref,
			Provenance: investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN,
		}},
		IdempotencyKey: key,
	}

	return &Intake{
		Symptom:    symptom,
		ValidAt:    validAt,
		ObservedAt: observedAt,
		ReviewMode: in.ReviewMode,
		Window:     windowFor(validAt, lookback),
		Lookback:   lookback,
	}, nil
}

// SymptomID is the stable identifier of a symptom row. It is the published idempotency key,
// which is already a hash of the 4-tuple: a symptom and its key are the same fact, so giving
// them separate identities would only create a second way to be wrong.
func SymptomID(sourceID string, body *graphv1.AlertTransition) string {
	return eventlog.AlertTransitionKey(sourceID, body)
}

// originRefOf retains the alert's own reference (FR-002). The event body carries an explicit
// `origin_ref` where the connector had one; otherwise the monitor reference is it.
func originRefOf(body *graphv1.AlertTransition) string {
	if ref := strings.TrimSpace(body.GetOriginRef()); ref != "" {
		return ref
	}
	return graph.RefFromProto(body.GetMonitor()).String()
}

// statementOf derives the symptom statement. The order is: what the caller supplied, the
// transition's own title, then a generated line naming the monitor and the states — never an
// empty statement, because "the symptom in words" is what the verdict line quotes.
func statementOf(override string, body *graphv1.AlertTransition) string {
	if s := strings.TrimSpace(override); s != "" {
		return s
	}
	if s := strings.TrimSpace(body.GetTitle()); s != "" {
		return s
	}
	monitor := graph.RefFromProto(body.GetMonitor()).String()
	from, to := body.GetFromState(), body.GetToState()
	switch {
	case from != "" && to != "":
		return fmt.Sprintf("%s went %s → %s", monitor, from, to)
	case to != "":
		return fmt.Sprintf("%s entered %s", monitor, to)
	default:
		return monitor + " fired"
	}
}
