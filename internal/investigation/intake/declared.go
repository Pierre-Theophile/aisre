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

// The declaration front door (T077, FR-001a, FR-002a, FR-002b, FR-004a, FR-008b).
//
// A person says "we have an incident" in the place incidents are handled, and that is a
// first-class intake: no monitor need have fired, and the engine must not wait for one
// (FR-001a). It normalises into the same `alert.transition` shape as a monitor transition,
// distinguished by the intake origin `declared` and by the symptom row's actor kind `human`.
//
// Four rules this file exists to hold.
//
// **The pin is the instant of declaration** (FR-004a) — not the instant a connector delivered
// it, not the instant the engine woke up. A later edit to severity, title or targets is a
// separate, later event with its own instant and author (FR-007); it never moves the pin. That
// is why Declaration has no "edited at" field: an edit is a second Declaration, or a human fact.
//
// **The key is the published 4-tuple with an empty group** (FR-008b, ADR-0005 D2): `(source,
// stable id of the place the incident lives, "", declared instant)`. Keyed on the *stable*
// identifier — a channel id, an incident record id — never the name, because names are edited
// and a renamed channel would open a second investigation for one incident.
//
// **Targets are never guessed** (FR-002b). A reference comes either from the declaration, parsed
// by a rule the *connector* owns, or from a person who supplied it. Where neither yields one,
// the investigation opens anyway and reaches `unknown` with "name the affected service(s)" as
// the resolving action.
//
// **The parse-rule registry is a connector artifact** (FR-002b, U2). This engine neither defines
// the rules nor reads their text. What a parsed reference records is the rule id and the rule
// version that produced it, and nothing else, so a later reader resolves the rule from the
// connector that owns it. RuleRef below is the spelling of that pair.

// DeclaredSourceKind is the transition state a declaration carries (ADR-0005 D2's published
// vocabulary). A declaration is a transition into `declared`, from nothing.
const DeclaredSourceKind = eventlog.AlertStateDeclared

// TargetProvenanceParsed and TargetProvenanceSupplied are the two provenances a declared
// target may carry, spelled as strings for the row and mapped to the wire enum on the symptom.
const (
	// TargetProvenanceParsed is a reference the connector's published naming rule produced.
	TargetProvenanceParsed = "parsed_from_declaration"
	// TargetProvenanceSupplied is a reference a person gave, at declaration time or later.
	TargetProvenanceSupplied = "supplied_by_human"
	// TargetProvenanceMonitor is a reference the monitor's own WATCHES edges produced.
	TargetProvenanceMonitor = "from_monitor"
)

// RuleRef is the identity of a connector-owned parse rule: its id and its version, and nothing
// more (FR-002b, U2).
//
// It is carried on the wire in `TargetRef.rule_id` as `<id>@<version>`, because the published
// message has one field for it and widening the message would be a breaking change to a
// contract three features implement. The pair is still recorded and still separable:
// `RuleRef.String` writes it, `ParseRule` reads it back, and a rule id that already contains an
// `@` is rejected rather than silently split.
type RuleRef struct {
	// ID is the rule's identifier in the connector's registry.
	ID string
	// Version is the version of that rule which produced the reference.
	Version string
}

// IsZero reports whether the reference names no rule.
func (r RuleRef) IsZero() bool { return r.ID == "" && r.Version == "" }

// String renders the pair as `<id>@<version>`, which is what the symptom row stores.
func (r RuleRef) String() string {
	if r.IsZero() {
		return ""
	}
	if r.Version == "" {
		return r.ID
	}
	return r.ID + "@" + r.Version
}

// ParseRule reads a `<id>@<version>` rule reference back. A value with no `@` is a rule id
// whose version was not recorded, which is reported as such rather than invented.
func ParseRule(s string) RuleRef {
	id, version, found := strings.Cut(s, "@")
	if !found {
		return RuleRef{ID: s}
	}
	return RuleRef{ID: id, Version: version}
}

// DeclaredTarget is one target reference on a declaration, with the provenance that produced it.
type DeclaredTarget struct {
	// Ref is the entity reference. Required.
	Ref *graphv1.Ref
	// Provenance is TargetProvenanceParsed or TargetProvenanceSupplied. Required: a reference
	// whose provenance is unknown is a guessed reference, and FR-002b forbids one.
	Provenance string
	// Rule names the connector rule that parsed the reference. Required when Provenance is
	// TargetProvenanceParsed, and meaningless otherwise.
	Rule RuleRef
}

// Declaration is a human-declared incident, exactly as the declaring connector observed it.
// Nothing here is inferred by the engine (FR-002a).
type Declaration struct {
	// SourceID is the connector that observed the declaration. First element of the key.
	SourceID string
	// PlaceID is the STABLE identifier of the place the incident lives — a channel id, an
	// incident record id — never its name (FR-008b). Second element of the key.
	PlaceID string
	// OriginRef is the human-facing reference of that place, retained for the reader (FR-002).
	// It may be a name or a URL; it is never the key.
	OriginRef string
	// DeclaredAt is the instant of declaration. Required; the pin (FR-004a).
	DeclaredAt time.Time
	// Severity and Title are as declared. Never inferred (FR-002a).
	Severity string
	Title    string
	// Statement is the symptom in words; empty falls back to the title.
	Statement string
	// DeclaringIdentity is the authenticated individual who declared. Required (FR-002a, FR-066).
	DeclaringIdentity string
	// Targets are zero or more entity references, each with its provenance (FR-002b).
	Targets []DeclaredTarget
	// Lookback is the window width; zero means DefaultLookback.
	Lookback time.Duration
	// ReviewMode, ObservedAt and Now behave as on MonitorIntake. Note that a declaration's
	// observed instant is pinned to DeclaredAt (FR-004a), so ObservedAt is only consulted in
	// review mode.
	ReviewMode bool
	ObservedAt time.Time
	Now        func() time.Time
}

// ErrUnknownProvenance is a target reference whose provenance is not one of the published two.
// It is a refusal, not a default: a reference with no provenance is a guessed reference.
var ErrUnknownProvenance = fmt.Errorf("target reference provenance must be %q or %q",
	TargetProvenanceParsed, TargetProvenanceSupplied)

// ErrParsedWithoutRule is a parsed target reference that names no rule. The rule id and version
// are the only thing the engine records about a parse (FR-002b, U2); a parse that cannot name
// them is not auditable and is refused.
var ErrParsedWithoutRule = fmt.Errorf("a %s reference must name the rule id that produced it",
	TargetProvenanceParsed)

// NormaliseDeclaration turns a declared incident into the one symptom shape (FR-002a).
//
// The returned Intake has UnresolvedTargets set when the declaration named nothing. That is a
// legitimate intake, not an error: the investigation opens, and FR-002b routes it to `unknown`
// with a request for the affected services.
func NormaliseDeclaration(d Declaration) (*Intake, error) {
	if strings.TrimSpace(d.PlaceID) == "" {
		return nil, fmt.Errorf("normalise declaration: %w: no stable identifier for the place the "+
			"incident lives; the key may not rest on a name (FR-008b)", ErrNoSubject)
	}
	if d.DeclaredAt.IsZero() {
		return nil, fmt.Errorf("normalise declaration: %w: the instant of declaration is the pin "+
			"and is never substituted by the instant the engine observed it (FR-004a)", ErrNoInstant)
	}
	if strings.TrimSpace(d.DeclaringIdentity) == "" {
		return nil, fmt.Errorf("normalise declaration: %w", ErrAnonymous)
	}

	// FR-004a: the observed instant is the instant of declaration. Review mode is the recorded
	// exception and the only way it may be anything else.
	observedIn := d.ObservedAt
	if !d.ReviewMode {
		observedIn = time.Time{}
	}
	validAt, observedAt, err := pin(d.DeclaredAt, observedIn, d.ReviewMode, d.Now)
	if err != nil {
		return nil, fmt.Errorf("normalise declaration: %w", err)
	}
	lookback := lookbackOr(d.Lookback)

	named := make([]*graphv1.Ref, 0, len(d.Targets))
	targets := make([]*investigationv1.TargetRef, 0, len(d.Targets))
	for i, t := range d.Targets {
		if t.Ref.GetNamespace() == "" && t.Ref.GetValue() == "" {
			continue
		}
		provenance, err := targetProvenance(t)
		if err != nil {
			return nil, fmt.Errorf("normalise declaration: target %d: %w", i, err)
		}
		named = append(named, t.Ref)
		targets = append(targets, &investigationv1.TargetRef{
			Ref:        t.Ref,
			Provenance: provenance,
			RuleId:     t.Rule.String(),
		})
	}

	key := DeclarationKey(d.SourceID, d.PlaceID, validAt)
	statement := strings.TrimSpace(d.Statement)
	if statement == "" {
		statement = strings.TrimSpace(d.Title)
	}
	if statement == "" {
		// Not a guess about the system: a statement of what is known, which is that a person
		// declared an incident here at this instant.
		statement = "incident declared by " + d.DeclaringIdentity + " in " + d.PlaceID
	}

	symptom := &investigationv1.Symptom{
		SymptomId:         key,
		OriginSystem:      d.SourceID,
		OriginRef:         originRefOrPlace(d),
		Transport:         TransportHumanDeclared,
		Statement:         statement,
		FiredAt:           timestampOf(validAt),
		NamedIdentifiers:  named,
		Origin:            investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED,
		Severity:          d.Severity,
		Title:             d.Title,
		DeclaringIdentity: d.DeclaringIdentity,
		TargetRefs:        targets,
		IdempotencyKey:    key,
	}

	return &Intake{
		Symptom:           symptom,
		ValidAt:           validAt,
		ObservedAt:        observedAt,
		ReviewMode:        d.ReviewMode,
		Window:            windowFor(validAt, lookback),
		Lookback:          lookback,
		UnresolvedTargets: len(named) == 0,
	}, nil
}

// PlaceNamespace is the reference namespace of the place an incident lives. A declaration's
// "monitor" is that place, on its stable identifier, which is what makes the two front doors one
// key: `monitor=checkout-error-rate` and `incident=C0123STABLE` are both stable alert
// identifiers in the sense of FR-008b.
const PlaceNamespace = "incident"

// PlaceRef is the reference of the place an incident lives, on its stable identifier.
func PlaceRef(placeID string) graph.Ref {
	return graph.Ref{Namespace: PlaceNamespace, Value: placeID}
}

// DeclarationKey is the published 4-tuple idempotency key of a declaration: the source, the
// stable identifier of the place, an EMPTY group, and the declared instant (FR-008b, ADR-0005
// D2). Re-delivery under this key is a no-op that returns the existing investigation.
//
// It agrees byte for byte with what eventlog.AlertTransitionKey derives from the event body
// DeclarationBody builds, which is the property that makes a declaration observed by a connector
// and a declaration made with `investigate declare` one fact rather than two.
func DeclarationKey(sourceID, placeID string, declaredAt time.Time) string {
	return eventlog.AlertTransitionKeyParts(sourceID, PlaceRef(placeID).String(), "",
		declaredAt.UTC().Format(time.RFC3339Nano))
}

// DeclarationBody renders a declaration as the published `alert.transition` event body, which is
// what a connector emits into the graph and what makes the two doors one event (ADR-0005 D2).
//
// The engine builds it rather than the connector only in one place — `investigate declare`,
// where the CLI is the connector — and it is here so that the shape is written once.
func DeclarationBody(d Declaration) *graphv1.AlertTransition {
	watches := make([]*graphv1.Ref, 0, len(d.Targets))
	for _, t := range d.Targets {
		if t.Ref.GetNamespace() == "" && t.Ref.GetValue() == "" {
			continue
		}
		watches = append(watches, t.Ref)
	}
	return &graphv1.AlertTransition{
		// The monitor reference of a declaration is the place the incident lives, on its STABLE
		// identifier — which is exactly what the 4-tuple keys on.
		Monitor:           PlaceRef(d.PlaceID).Proto(),
		GroupKey:          "",
		TransitionAt:      timestampOf(d.DeclaredAt),
		FromState:         "",
		ToState:           DeclaredSourceKind,
		Watches:           watches,
		Transport:         TransportHumanDeclared,
		OriginRef:         originRefOrPlace(d),
		ActorKind:         graphv1.ActorKind_PERSON,
		Severity:          d.Severity,
		Title:             d.Title,
		DeclaringIdentity: d.DeclaringIdentity,
	}
}

func originRefOrPlace(d Declaration) string {
	if ref := strings.TrimSpace(d.OriginRef); ref != "" {
		return ref
	}
	return d.PlaceID
}

func targetProvenance(t DeclaredTarget) (investigationv1.TargetRefProvenance, error) {
	switch t.Provenance {
	case TargetProvenanceParsed:
		if t.Rule.ID == "" {
			return 0, ErrParsedWithoutRule
		}
		return investigationv1.TargetRefProvenance_PARSED_FROM_DECLARATION, nil
	case TargetProvenanceSupplied:
		return investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN, nil
	case TargetProvenanceMonitor:
		return investigationv1.TargetRefProvenance_FROM_MONITOR, nil
	default:
		return 0, fmt.Errorf("%w (got %q)", ErrUnknownProvenance, t.Provenance)
	}
}

// MissingTargetResolution is the resolving action an intake with no target carries into the
// `unknown` outcome (FR-002b, FR-026). It is named here, beside the rule that produces it, so
// the renderer and the engine quote one string.
const MissingTargetResolution = "name the affected service(s)"

// ResolutionKindConfirmIdentity is the published `Resolution.kind` for that request.
const ResolutionKindConfirmIdentity = "confirm_identity"

// MissingTargetRequest is the resolution an intake with no target produces: the concrete thing
// a person can do that would let the investigation proceed.
func MissingTargetRequest(in *Intake) *investigationv1.Resolution {
	place := ""
	if in != nil && in.Symptom != nil {
		place = in.Symptom.GetOriginRef()
	}
	statement := MissingTargetResolution
	if place != "" {
		statement += " for the incident declared in " + place
	}
	return &investigationv1.Resolution{
		Statement: statement,
		Kind:      ResolutionKindConfirmIdentity,
	}
}
