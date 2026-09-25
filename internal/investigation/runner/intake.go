// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"fmt"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
)

// Turning a wire request into the one normalised intake shape.
//
// The RPC carries a `Symptom`, which is deliberately the same message both front doors produce:
// a connector that can write one can drive either. What this file decides is *which door it came
// through*, and it decides it from the transport the caller declared rather than by guessing from
// the fields that happen to be set — a declaration with a monitor-shaped origin is still a
// declaration, and treating it as a transition would pin the wrong instant (FR-004a).
//
// The idempotency key is the one place the caller's own value wins. A connector that observed a
// transition has already computed the published 4-tuple key for it, and re-deriving one here from
// a partially reconstructed transition body would make re-delivery through a different path open
// a second investigation — the exact failure FR-008b exists to prevent. So: where the request
// carries a key, it is the key; where it does not, intake derives one.

// intakeOf normalises an InvestigateRequest.
func (r *Runner) intakeOf(req *investigationv1.InvestigateRequest, requester string) (*intake.Intake, error) {
	symptom := req.GetSymptom()
	if symptom == nil {
		return nil, fmt.Errorf("investigate: %w: the request carries no symptom", intake.ErrNoSubject)
	}
	validAt := instantOf(req.GetValidAt(), symptom.GetFiredAt())
	if validAt.IsZero() {
		return nil, fmt.Errorf("investigate: %w: neither the request nor the symptom pins an instant "+
			"and the engine does not substitute the instant it observed the input (FR-004a)", intake.ErrNoInstant)
	}
	lookback := time.Duration(req.GetLookbackSeconds()) * time.Second
	observedAt := time.Time{}
	if ts := req.GetObservedAt(); ts != nil {
		observedAt = ts.AsTime().UTC()
	}

	var (
		in  *intake.Intake
		err error
	)
	switch {
	case declared(symptom):
		// A declaration delivered through Investigate rather than Declare. It is the same fact
		// and it takes the same door, so that the two paths cannot produce two keys.
		declaration, derr := declarationOf(symptom, requester, lookback)
		if derr != nil {
			return nil, derr
		}
		declaration.ReviewMode = req.GetReviewMode()
		declaration.ObservedAt = observedAt
		declaration.Now = r.clock
		in, err = intake.NormaliseDeclaration(declaration)
	case symptom.GetTransport() == intake.TransportExplicitReference || symptom.GetOriginSystem() == "investigate":
		ref := firstRef(symptom)
		if ref == nil {
			return nil, fmt.Errorf("investigate: %w: an explicit reference names a node", intake.ErrNoSubject)
		}
		in, err = intake.NormaliseReference(intake.ExplicitReference{
			Ref:        ref,
			At:         validAt,
			Statement:  symptom.GetStatement(),
			Requester:  requester,
			Lookback:   lookback,
			ReviewMode: req.GetReviewMode(),
			ObservedAt: observedAt,
			Now:        r.clock,
		})
	default:
		in, err = intake.NormaliseAlert(intake.MonitorIntake{
			SourceID:   originSystemOf(symptom),
			Transition: transitionOf(symptom, validAt),
			Statement:  symptom.GetStatement(),
			Lookback:   lookback,
			ReviewMode: req.GetReviewMode(),
			ObservedAt: observedAt,
			Now:        r.clock,
		})
	}
	if err != nil {
		return nil, err
	}
	adoptKey(in, symptom.GetIdempotencyKey())
	return in, nil
}

// intakeOfDeclaration normalises a DeclareRequest (FR-001a, FR-002a).
func (r *Runner) intakeOfDeclaration(req *investigationv1.DeclareRequest, requester string) (*intake.Intake, error) {
	symptom := req.GetDeclaration()
	if symptom == nil {
		return nil, fmt.Errorf("declare: %w: the request carries no declaration", intake.ErrNoSubject)
	}
	declaration, err := declarationOf(symptom, requester,
		time.Duration(req.GetLookbackSeconds())*time.Second)
	if err != nil {
		return nil, err
	}
	declaration.Now = r.clock
	in, err := intake.NormaliseDeclaration(declaration)
	if err != nil {
		return nil, err
	}
	adoptKey(in, symptom.GetIdempotencyKey())
	return in, nil
}

// declarationOf reads the declaration out of the wire symptom. Nothing is inferred: severity,
// title, the instant and the declaring identity are as declared or the declaration is refused
// (FR-002a).
func declarationOf(symptom *investigationv1.Symptom, requester string, lookback time.Duration) (intake.Declaration, error) {
	declaredAt := time.Time{}
	if ts := symptom.GetFiredAt(); ts != nil {
		declaredAt = ts.AsTime().UTC()
	}
	system, placeID := placeOf(symptom)
	if placeID == "" {
		return intake.Declaration{}, fmt.Errorf("declare: %w: the declaration names no stable "+
			"identifier for the place the incident lives; the key may not rest on a name (FR-008b)",
			intake.ErrNoSubject)
	}
	identity := symptom.GetDeclaringIdentity()
	if identity == "" {
		identity = requester
	}
	if identity == "" {
		return intake.Declaration{}, fmt.Errorf("declare: %w", intake.ErrAnonymous)
	}

	targets := make([]intake.DeclaredTarget, 0, len(symptom.GetTargetRefs()))
	for _, t := range symptom.GetTargetRefs() {
		provenance, err := provenanceName(t.GetProvenance())
		if err != nil {
			return intake.Declaration{}, err
		}
		targets = append(targets, intake.DeclaredTarget{
			Ref:        t.GetRef(),
			Provenance: provenance,
			Rule:       intake.ParseRule(t.GetRuleId()),
		})
	}
	// A declaration whose targets arrived as bare named identifiers — a connector that filled
	// `named_identifiers` and not `target_refs` — is still a declaration whose targets a person
	// supplied. It is not a parse, so it is never recorded as one (FR-002b).
	if len(targets) == 0 {
		for _, ref := range symptom.GetNamedIdentifiers() {
			targets = append(targets, intake.DeclaredTarget{
				Ref: ref, Provenance: intake.TargetProvenanceSupplied,
			})
		}
	}

	return intake.Declaration{
		SourceID:          system,
		PlaceID:           placeID,
		OriginRef:         symptom.GetOriginRef(),
		DeclaredAt:        declaredAt,
		Severity:          symptom.GetSeverity(),
		Title:             symptom.GetTitle(),
		Statement:         symptom.GetStatement(),
		DeclaringIdentity: identity,
		Targets:           targets,
		Lookback:          lookback,
	}, nil
}

// placeOf splits the declaration's origin into the observing system and the stable identifier of
// the place the incident lives. `origin_ref` is written `<system>:<stable-id>` by the CLI; a
// connector that filled `origin_system` separately gets that, with `origin_ref` as the place.
func placeOf(symptom *investigationv1.Symptom) (system, placeID string) {
	system = strings.TrimSpace(symptom.GetOriginSystem())
	ref := strings.TrimSpace(symptom.GetOriginRef())
	if prefix, rest, found := strings.Cut(ref, ":"); found && prefix != "" && rest != "" {
		if system == "" {
			system = prefix
		}
		if prefix == system {
			return system, rest
		}
	}
	return system, ref
}

// provenanceName maps the wire provenance onto intake's spelling, refusing the unspecified value
// rather than defaulting it: a reference with no provenance is a guessed reference (FR-002b).
func provenanceName(p investigationv1.TargetRefProvenance) (string, error) {
	switch p {
	case investigationv1.TargetRefProvenance_PARSED_FROM_DECLARATION:
		return intake.TargetProvenanceParsed, nil
	case investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN:
		return intake.TargetProvenanceSupplied, nil
	case investigationv1.TargetRefProvenance_FROM_MONITOR:
		return intake.TargetProvenanceMonitor, nil
	default:
		return "", fmt.Errorf("declare: %w", intake.ErrUnknownProvenance)
	}
}

// transitionOf reconstructs the published `alert.transition` body from a wire symptom, so that
// the monitor door normalises exactly as a connector-delivered transition would.
func transitionOf(symptom *investigationv1.Symptom, validAt time.Time) *graphv1.AlertTransition {
	monitor := monitorRef(symptom)
	return &graphv1.AlertTransition{
		Monitor:      monitor,
		TransitionAt: timestampOf(validAt),
		ToState:      "alerting",
		Watches:      symptom.GetNamedIdentifiers(),
		Transport:    orDefault(symptom.GetTransport(), intake.TransportMonitor),
		OriginRef:    symptom.GetOriginRef(),
		Severity:     symptom.GetSeverity(),
		Title:        symptom.GetTitle(),
	}
}

// monitorRef is the monitor the transition came from. A connector that wrote `origin_ref` as a
// reference gets that reference; one that wrote a bare identifier gets it in the `monitor`
// namespace, which is what the published key hashes over either way.
func monitorRef(symptom *investigationv1.Symptom) *graphv1.Ref {
	raw := strings.TrimSpace(symptom.GetOriginRef())
	if raw == "" {
		raw = strings.TrimSpace(symptom.GetSymptomId())
	}
	if ref, err := graph.ParseRef(raw); err == nil {
		return ref.Proto()
	}
	return &graphv1.Ref{Namespace: "monitor", Value: raw}
}

// adoptKey honours the caller's published idempotency key where it supplied one (FR-008b).
func adoptKey(in *intake.Intake, key string) {
	key = strings.TrimSpace(key)
	if key == "" || in == nil || in.Symptom == nil {
		return
	}
	in.Symptom.IdempotencyKey = key
	in.Symptom.SymptomId = key
}

// declared reports whether a wire symptom is a human declaration.
func declared(symptom *investigationv1.Symptom) bool {
	return symptom.GetTransport() == intake.TransportHumanDeclared ||
		symptom.GetOrigin() == investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED
}

func firstRef(symptom *investigationv1.Symptom) *graphv1.Ref {
	for _, ref := range symptom.GetNamedIdentifiers() {
		if ref.GetNamespace() != "" || ref.GetValue() != "" {
			return ref
		}
	}
	for _, t := range symptom.GetTargetRefs() {
		if t.GetRef().GetNamespace() != "" || t.GetRef().GetValue() != "" {
			return t.GetRef()
		}
	}
	return nil
}

func originSystemOf(symptom *investigationv1.Symptom) string {
	if s := strings.TrimSpace(symptom.GetOriginSystem()); s != "" {
		return s
	}
	return "monitor"
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// errNoRequest is the invalid-argument every entry point raises for a nil request.
var errNoRequest = errors.New("the request is empty")
