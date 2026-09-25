// SPDX-License-Identifier: Apache-2.0

package intake_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// Intake normalisation (T076–T080, FR-001a, FR-002, FR-002a, FR-002b, FR-004, FR-004a, FR-008b,
// FR-008c).
//
// The table below is the contract: two front doors produce one shape, and every field that must
// not be invented is asserted absent rather than asserted plausible.

var (
	firedAt    = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	declaredAt = time.Date(2026, 9, 1, 14, 35, 0, 0, time.UTC)
	fakeNow    = func() time.Time { return time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC) }
)

func monitorBody() *graphv1.AlertTransition {
	return &graphv1.AlertTransition{
		Monitor:      &graphv1.Ref{Namespace: "monitor", Value: "checkout-error-rate"},
		GroupKey:     "prod",
		TransitionAt: timestamppb.New(firedAt),
		FromState:    eventlog.AlertStateOK,
		ToState:      eventlog.AlertStateAlert,
		Watches: []*graphv1.Ref{
			{Namespace: "k8s.service", Value: "shop/checkout"},
		},
		Transport: "webhook",
		OriginRef: "https://monitor.invalid/alerts/9931",
		Severity:  "critical",
		Title:     "checkout error rate above 5%",
	}
}

func TestNormaliseAlertPinsBothInstants(t *testing.T) {
	t.Parallel()

	in, err := intake.NormaliseAlert(intake.MonitorIntake{
		SourceID:   "datadog:prod",
		Transition: monitorBody(),
		Now:        fakeNow,
	})
	if err != nil {
		t.Fatalf("normalise alert: %v", err)
	}

	if !in.ValidAt.Equal(firedAt) {
		t.Errorf("valid instant = %s, want the transition instant %s", in.ValidAt, firedAt)
	}
	// FR-004: the observed instant defaults to the symptom instant, so facts learned later are
	// invisible. A run that defaulted it to "now" would let the investigation see a merge that
	// was decided after the alert.
	if !in.ObservedAt.Equal(firedAt) {
		t.Errorf("observed instant = %s, want the symptom instant %s", in.ObservedAt, firedAt)
	}
	if in.ReviewMode {
		t.Error("review mode is on by default for an alert-driven run; FR-005 forbids that")
	}
	if got, want := in.Symptom.GetTransport(), intake.TransportMonitor; got != want {
		t.Errorf("transport = %q, want %q", got, want)
	}
	if got, want := in.Symptom.GetOrigin(), investigationv1.IntakeOrigin_INTAKE_ORIGIN_MONITOR; got != want {
		t.Errorf("origin = %v, want %v", got, want)
	}
	if got, want := in.Symptom.GetOriginRef(), "https://monitor.invalid/alerts/9931"; got != want {
		t.Errorf("origin ref = %q, want the alert's own reference %q (FR-002)", got, want)
	}
	if got, want := in.Window.GetEnd().AsTime(), firedAt; !got.Equal(want) {
		t.Errorf("window ends at %s, want the symptom instant %s", got, want)
	}
	if got, want := in.Window.GetStart().AsTime(), firedAt.Add(-intake.DefaultLookback); !got.Equal(want) {
		t.Errorf("window starts at %s, want %s", got, want)
	}
	if len(in.Symptom.GetTargetRefs()) != 1 ||
		in.Symptom.GetTargetRefs()[0].GetProvenance() != investigationv1.TargetRefProvenance_FROM_MONITOR {
		t.Errorf("a monitor's watched entity must carry provenance FROM_MONITOR, got %v",
			in.Symptom.GetTargetRefs())
	}
	if in.UnresolvedTargets {
		t.Error("a transition that watches an entity has a target")
	}
}

func TestNormaliseAlertReviewModeIsRecordedAndNeverDefault(t *testing.T) {
	t.Parallel()

	in, err := intake.NormaliseAlert(intake.MonitorIntake{
		SourceID:   "datadog:prod",
		Transition: monitorBody(),
		ReviewMode: true,
		Now:        fakeNow,
	})
	if err != nil {
		t.Fatalf("normalise alert: %v", err)
	}
	if !in.ReviewMode {
		t.Fatal("review mode was asked for and not recorded (FR-005)")
	}
	if !in.ObservedAt.Equal(fakeNow()) {
		t.Errorf("review mode observed instant = %s, want now (%s)", in.ObservedAt, fakeNow())
	}
	if !in.ValidAt.Equal(firedAt) {
		t.Errorf("review mode moved the valid instant to %s; it is still the symptom instant %s",
			in.ValidAt, firedAt)
	}
}

func TestNormaliseAlertRefusesAnObservedInstantAfterTheValidOne(t *testing.T) {
	t.Parallel()

	_, err := intake.NormaliseAlert(intake.MonitorIntake{
		SourceID:   "datadog:prod",
		Transition: monitorBody(),
		ObservedAt: firedAt.Add(time.Hour),
	})
	if !errors.Is(err, intake.ErrObservedBeforeValid) {
		t.Fatalf("error = %v, want ErrObservedBeforeValid: outside review mode the engine may "+
			"not know anything learned after the instant it is about", err)
	}
}

func TestNormaliseAlertWithNoWatchedEntityDoesNotGuessOne(t *testing.T) {
	t.Parallel()

	body := monitorBody()
	body.Watches = nil
	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: body})
	if err != nil {
		t.Fatalf("normalise alert: %v", err)
	}
	if len(in.Symptom.GetNamedIdentifiers()) != 0 {
		t.Errorf("named identifiers = %v, want none: the monitor's title is not an entity",
			in.Symptom.GetNamedIdentifiers())
	}
	if !in.UnresolvedTargets {
		t.Error("a transition that watches nothing must be flagged unresolved (FR-002b, FR-026)")
	}
}

func declaration() intake.Declaration {
	return intake.Declaration{
		SourceID:          "slack:acme",
		PlaceID:           "C0123STABLE",
		OriginRef:         "#incident-checkout",
		DeclaredAt:        declaredAt,
		Severity:          "sev2",
		Title:             "checkout is throwing 500s",
		DeclaringIdentity: "oidc|alice",
		Targets: []intake.DeclaredTarget{{
			Ref:        &graphv1.Ref{Namespace: "k8s.service", Value: "shop/checkout"},
			Provenance: intake.TargetProvenanceParsed,
			Rule:       intake.RuleRef{ID: "slack-title-service", Version: "3"},
		}},
	}
}

func TestNormaliseDeclarationIsFirstClassAndPinsTheDeclarationInstant(t *testing.T) {
	t.Parallel()

	in, err := intake.NormaliseDeclaration(declaration())
	if err != nil {
		t.Fatalf("normalise declaration: %v", err)
	}

	// FR-004a: the pin is the instant of declaration, never the instant the engine observed it.
	if !in.ValidAt.Equal(declaredAt) || !in.ObservedAt.Equal(declaredAt) {
		t.Errorf("instants = (%s, %s), want both at the declaration instant %s",
			in.ValidAt, in.ObservedAt, declaredAt)
	}
	if got, want := in.Symptom.GetOrigin(), investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED; got != want {
		t.Errorf("origin = %v, want %v", got, want)
	}
	if got, want := in.Symptom.GetTransport(), intake.TransportHumanDeclared; got != want {
		t.Errorf("transport = %q, want %q", got, want)
	}
	if got, want := in.Symptom.GetSeverity(), "sev2"; got != want {
		t.Errorf("severity = %q, want the declared %q", got, want)
	}
	if got, want := in.Symptom.GetDeclaringIdentity(), "oidc|alice"; got != want {
		t.Errorf("declaring identity = %q, want %q", got, want)
	}
	// FR-002b and U2: what is recorded about a parse is the rule id and the rule version, and
	// nothing else — no rule text, no pattern, no confidence.
	target := in.Symptom.GetTargetRefs()[0]
	if got, want := target.GetProvenance(), investigationv1.TargetRefProvenance_PARSED_FROM_DECLARATION; got != want {
		t.Errorf("provenance = %v, want %v", got, want)
	}
	rule := intake.ParseRule(target.GetRuleId())
	if rule.ID != "slack-title-service" || rule.Version != "3" {
		t.Errorf("recorded rule = %+v, want id slack-title-service version 3", rule)
	}
}

func TestDeclarationIdempotencyKeyIsThePublished4TupleWithAnEmptyGroup(t *testing.T) {
	t.Parallel()

	in, err := intake.NormaliseDeclaration(declaration())
	if err != nil {
		t.Fatalf("normalise declaration: %v", err)
	}
	// ADR-0005 D2 / FR-008b: (source, stable place id, "", declared instant). Spelled out here
	// rather than taken from the helper, so that a change to either spelling fails.
	want := eventlog.AlertTransitionKeyParts("slack:acme", "incident=C0123STABLE", "",
		declaredAt.Format(time.RFC3339Nano))
	if got := in.IdempotencyKey(); got != want {
		t.Errorf("idempotency key = %q, want %q", got, want)
	}

	// Re-delivery of the identical declaration is the same key, so it is a no-op upstream.
	again, err := intake.NormaliseDeclaration(declaration())
	if err != nil {
		t.Fatalf("re-normalise: %v", err)
	}
	if again.IdempotencyKey() != in.IdempotencyKey() {
		t.Error("re-delivery produced a second key; FR-008b makes it a no-op")
	}

	// A declaration in the same place at a different instant is a different incident.
	other := declaration()
	other.DeclaredAt = declaredAt.Add(time.Minute)
	third, err := intake.NormaliseDeclaration(other)
	if err != nil {
		t.Fatalf("normalise third: %v", err)
	}
	if third.IdempotencyKey() == in.IdempotencyKey() {
		t.Error("two declarations a minute apart share a key")
	}

	// The key rests on the STABLE id, not the name: renaming the channel changes nothing.
	renamed := declaration()
	renamed.OriginRef = "#incident-checkout-renamed"
	fourth, err := intake.NormaliseDeclaration(renamed)
	if err != nil {
		t.Fatalf("normalise renamed: %v", err)
	}
	if fourth.IdempotencyKey() != in.IdempotencyKey() {
		t.Error("renaming the place changed the key; FR-008b keys on the stable identifier")
	}
}

func TestDeclarationRefusalsAreRefusalsToInvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func(*intake.Declaration)
		want error
	}{
		{"no stable place id", func(d *intake.Declaration) { d.PlaceID = "" }, intake.ErrNoSubject},
		{"no declared instant", func(d *intake.Declaration) { d.DeclaredAt = time.Time{} }, intake.ErrNoInstant},
		{"anonymous", func(d *intake.Declaration) { d.DeclaringIdentity = "" }, intake.ErrAnonymous},
		{"target with no provenance", func(d *intake.Declaration) {
			d.Targets[0].Provenance = ""
		}, intake.ErrUnknownProvenance},
		{"parsed target with no rule", func(d *intake.Declaration) {
			d.Targets[0].Rule = intake.RuleRef{}
		}, intake.ErrParsedWithoutRule},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := declaration()
			tt.edit(&d)
			_, err := intake.NormaliseDeclaration(d)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDeclarationWithNoParseableTargetAsksForOne(t *testing.T) {
	t.Parallel()

	d := declaration()
	d.Targets = nil
	in, err := intake.NormaliseDeclaration(d)
	if err != nil {
		t.Fatalf("normalise declaration: %v", err)
	}
	if !in.UnresolvedTargets {
		t.Fatal("a declaration naming no target must be flagged, not guessed (FR-002b)")
	}
	if len(in.Symptom.GetTargetRefs()) != 0 {
		t.Errorf("target refs = %v, want none", in.Symptom.GetTargetRefs())
	}
	res := intake.MissingTargetRequest(in)
	if res.GetKind() != intake.ResolutionKindConfirmIdentity {
		t.Errorf("resolution kind = %q, want %q", res.GetKind(), intake.ResolutionKindConfirmIdentity)
	}
	if !strings.Contains(res.GetStatement(), intake.MissingTargetResolution) {
		t.Errorf("resolution statement = %q, want it to ask for the affected services", res.GetStatement())
	}
}

func TestDeclarationBodyIsThePublishedAlertTransitionShape(t *testing.T) {
	t.Parallel()

	body := intake.DeclarationBody(declaration())
	if got, want := body.GetActorKind(), graphv1.ActorKind_PERSON; got != want {
		t.Errorf("actor kind = %v, want %v (FR-002a)", got, want)
	}
	if body.GetGroupKey() != "" {
		t.Errorf("group key = %q, want empty for a declaration (ADR-0005 D2)", body.GetGroupKey())
	}
	if got, want := body.GetToState(), intake.DeclaredSourceKind; got != want {
		t.Errorf("to state = %q, want %q", got, want)
	}
	if got, want := body.GetMonitor().GetValue(), "C0123STABLE"; got != want {
		t.Errorf("monitor value = %q, want the stable place id %q", got, want)
	}
	// The body's derived key and the symptom's key are the same fact.
	in, err := intake.NormaliseDeclaration(declaration())
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	if got, want := eventlog.AlertTransitionKey("slack:acme", body), in.IdempotencyKey(); got != want {
		t.Errorf("body key %q and symptom key %q disagree", got, want)
	}
}

// --- grouping (T078, FR-008a, FR-008c) ---------------------------------------------------

// smallGraph is the neighbourhood the grouping tests reason over: checkout talks to payments,
// and search is off on its own.
type smallGraph struct{ edges map[string][]string }

func newSmallGraph() smallGraph {
	return smallGraph{edges: map[string][]string{
		"e:checkout": {"e:payments"},
		"e:payments": {"e:checkout"},
		"e:search":   {},
	}}
}

func (g smallGraph) Distance(_ context.Context, a, b string) (int, bool, error) {
	if a == b {
		return 0, true, nil
	}
	for _, n := range g.edges[a] {
		if n == b {
			return 1, true, nil
		}
	}
	return 0, false, nil
}

func TestGroupAttachesALaterMonitorToADeclaredIncident(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The declaration opened the incident three minutes before the monitor fires on the
	// neighbouring service.
	open := []intake.OpenIncident{{
		IncidentID:      "inc-1",
		InvestigationID: "inv-1",
		OpenedAt:        declaredAt,
		LastSymptomAt:   declaredAt,
		EntityIDs:       []string{"e:checkout"},
	}}
	body := monitorBody()
	body.TransitionAt = timestamppb.New(declaredAt.Add(3 * time.Minute))
	later, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: body})
	if err != nil {
		t.Fatalf("normalise alert: %v", err)
	}

	decision, err := intake.Group(ctx, intake.DefaultGroupingRule(), later,
		[]string{"e:payments"}, open, newSmallGraph())
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	if !decision.Attached {
		t.Fatalf("decision = %+v, want it attached to inc-1 (FR-008a: one investigation per incident)", decision)
	}
	if decision.IncidentID != "inc-1" || decision.InvestigationID != "inv-1" {
		t.Errorf("attached to (%s, %s), want (inc-1, inv-1)", decision.IncidentID, decision.InvestigationID)
	}
	if decision.TimeDistance != 3*time.Minute {
		t.Errorf("time distance = %s, want 3m", decision.TimeDistance)
	}
	if !decision.GraphDistanceKnown || decision.GraphDistance != 1 {
		t.Errorf("graph distance = (%d, known=%v), want (1, true)",
			decision.GraphDistance, decision.GraphDistanceKnown)
	}
	if !later.Symptom.GetAttachedAsAdditional() {
		t.Error("the later symptom must be marked as attached (FR-008c)")
	}

	// FR-008a: the decision itself is evidence, carrying the distances it rested on.
	ev := decision.Evidence(later.ValidAt, later.ObservedAt, later.ValidAt)
	if ev.Kind != intake.EvidenceKindAssociation {
		t.Errorf("evidence kind = %q, want %q", ev.Kind, intake.EvidenceKindAssociation)
	}
	if ev.Coverage == nil {
		t.Fatal("evidence has no coverage block; Invariant 4 rejects the row")
	}
	if !strings.Contains(ev.Coverage.GetSampling(), intake.AssociationRuleVersion) {
		t.Errorf("coverage sampling = %q, want the rule version in it", ev.Coverage.GetSampling())
	}
	if !strings.Contains(ev.FreeText, "3m0s") || !strings.Contains(ev.FreeText, "1 hop") {
		t.Errorf("evidence detail = %q, want the two distances stated", ev.FreeText)
	}
	if ev.DeepLink == "" && ev.DeepLinkAbsentReason == "" {
		t.Error("an evidence item needs a deep link or a reason there is none (FR-057d)")
	}
}

func TestGroupOpensItsOwnIncidentWhenTheRuleDoesNotMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	open := []intake.OpenIncident{{
		IncidentID:    "inc-1",
		OpenedAt:      declaredAt,
		LastSymptomAt: declaredAt,
		EntityIDs:     []string{"e:checkout"},
	}}

	tests := []struct {
		name       string
		at         time.Time
		entityIDs  []string
		wantReason string
	}{
		{"too far in time", declaredAt.Add(2 * time.Hour), []string{"e:payments"}, intake.ReasonTooFarInTime},
		{"too far in the graph", declaredAt.Add(time.Minute), []string{"e:search"}, intake.ReasonTooFarInGraph},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body := monitorBody()
			body.TransitionAt = timestamppb.New(tt.at)
			in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: body})
			if err != nil {
				t.Fatalf("normalise: %v", err)
			}
			decision, err := intake.Group(ctx, intake.DefaultGroupingRule(), in, tt.entityIDs, open, newSmallGraph())
			if err != nil {
				t.Fatalf("group: %v", err)
			}
			if decision.Attached {
				t.Fatalf("decision attached; want a new incident (%s)", tt.wantReason)
			}
			if decision.Reason != intake.ReasonOpened {
				t.Errorf("reason = %q, want %q", decision.Reason, intake.ReasonOpened)
			}
			if len(decision.Considered) != 1 || decision.Considered[0].Reason != tt.wantReason {
				t.Errorf("considered = %+v, want one candidate rejected for %q",
					decision.Considered, tt.wantReason)
			}
			if in.Symptom.GetAttachedAsAdditional() {
				t.Error("a symptom that opened its own incident is not an attachment")
			}
		})
	}
}

func TestGroupNeverDropsADeclarationWithNoNeighbourhood(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	d := declaration()
	d.Targets = nil
	in, err := intake.NormaliseDeclaration(d)
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	open := []intake.OpenIncident{{
		IncidentID:    "inc-1",
		OpenedAt:      declaredAt,
		LastSymptomAt: declaredAt,
		EntityIDs:     []string{"e:checkout"},
	}}
	decision, err := intake.Group(ctx, intake.DefaultGroupingRule(), in, nil, open, newSmallGraph())
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	if decision.Attached {
		t.Fatal("a symptom with no resolved entity grouped on time alone; the rule needs both distances")
	}
	if decision.Reason != intake.ReasonNoNeighbourhood {
		t.Errorf("reason = %q, want %q", decision.Reason, intake.ReasonNoNeighbourhood)
	}
	if decision.IncidentID == "" {
		t.Error("the declaration was dropped; FR-008c says it opens its own investigation")
	}
}

func TestIncidentIDIsStableAcrossRedelivery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	first, err := intake.NormaliseDeclaration(declaration())
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	second, err := intake.NormaliseDeclaration(declaration())
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	a, err := intake.Group(ctx, intake.DefaultGroupingRule(), first, []string{"e:checkout"}, nil, newSmallGraph())
	if err != nil {
		t.Fatalf("group first: %v", err)
	}
	b, err := intake.Group(ctx, intake.DefaultGroupingRule(), second, []string{"e:checkout"}, nil, newSmallGraph())
	if err != nil {
		t.Fatalf("group second: %v", err)
	}
	if a.IncidentID != b.IncidentID {
		t.Errorf("re-delivery minted a second incident id: %s vs %s", a.IncidentID, b.IncidentID)
	}
}

// --- resolution (T079, FR-003) -----------------------------------------------------------

type stubResolver struct {
	canonical string
	decisions []*graphv1.ResolutionDecision
	err       error
}

func (s stubResolver) ResolutionAudit(context.Context, *graphv1.ResolutionAuditRequest) (*graphv1.ResolutionAuditResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &graphv1.ResolutionAuditResponse{
		SameEntity: true, CanonicalId: s.canonical, Decisions: s.decisions,
	}, nil
}

func TestResolveReportsBothTheGivenAndTheResolvedIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: monitorBody()})
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	set, err := intake.Resolve(ctx, stubResolver{canonical: "e:checkout-survivor"}, in)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(set.Resolutions) != 1 {
		t.Fatalf("resolutions = %d, want 1", len(set.Resolutions))
	}
	r := set.Resolutions[0]
	if got, want := r.Given.String(), "k8s.service=shop/checkout"; got != want {
		t.Errorf("given = %q, want %q (FR-003 asks for both)", got, want)
	}
	if r.EntityID != "e:checkout-survivor" {
		t.Errorf("resolved = %q, want the survivor", r.EntityID)
	}
	if set.AnyUnresolved() {
		t.Error("nothing was unresolved")
	}
	if len(set.Evidence) != 1 || set.Evidence[0].Kind != intake.EvidenceKindResolutionAudit {
		t.Fatalf("evidence = %+v, want one resolution_audit item (FR-003)", set.Evidence)
	}
	if set.Evidence[0].Coverage == nil {
		t.Error("resolution evidence has no coverage block")
	}
	if in.Symptom.GetResolutionEvidenceId() != set.Evidence[0].ID {
		t.Error("the symptom row does not cite the audit that justifies its resolution")
	}
}

func TestResolveSaysWhenAMergeIsInvisibleUnderTheObservedPin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: monitorBody()})
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	set, err := intake.Resolve(ctx, stubResolver{
		canonical: "e:checkout",
		decisions: []*graphv1.ResolutionDecision{{
			DecisionId: "human:confirm:abc", Kind: "confirm",
			DecidedAt: timestamppb.New(firedAt.Add(time.Hour)),
		}},
	}, in)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	notes := set.Notes()
	if len(notes) != 1 || !strings.Contains(notes[0], "NOT in force") {
		t.Fatalf("notes = %v, want one saying the later merge is not in force", notes)
	}
	if !strings.Contains(notes[0], "human:confirm:abc") {
		t.Errorf("note = %q, want it to name the invisible decision", notes[0])
	}
}

func TestResolveDoesNotGuessAnUnresolvableSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: monitorBody()})
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	set, err := intake.Resolve(ctx, stubResolver{canonical: ""}, in)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !set.AnyUnresolved() {
		t.Fatal("an empty canonical id must leave the subject unresolved, never guessed (FR-003)")
	}
	if len(set.EntityIDs) != 0 {
		t.Errorf("entity ids = %v, want none", set.EntityIDs)
	}
	req := intake.UnresolvedSubjectRequest(set.Unresolved[0])
	if req.GetKind() != intake.ResolutionKindConfirmIdentity {
		t.Errorf("resolution kind = %q, want %q", req.GetKind(), intake.ResolutionKindConfirmIdentity)
	}
}

func TestResolveFailsLoudlyWhenTheResolutionPathIsUnreachable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: monitorBody()})
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	_, err = intake.Resolve(ctx, stubResolver{err: errors.New("graph unavailable")}, in)
	if err == nil {
		t.Fatal("a transport failure was reported as an unresolvable subject; those are different answers")
	}
}

// --- extent (T080, FR-032) ---------------------------------------------------------------

type stubExtent struct{ extent *graphv1.Extent }

func (s stubExtent) Extent(context.Context, *graphv1.ExtentRequest) (*graphv1.Extent, error) {
	return s.extent, nil
}

func TestConsultExtentFlagsAGapOverlappingTheWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: monitorBody()})
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	src := stubExtent{extent: &graphv1.Extent{
		EarliestObserved: timestamppb.New(firedAt.Add(-24 * time.Hour)),
		LatestObserved:   timestamppb.New(firedAt),
		Sources: []*graphv1.SourceExtent{
			{SourceId: "k8s:prod", Gaps: []*graphv1.Interval{{
				Start: timestamppb.New(firedAt.Add(-20 * time.Minute)),
				End:   timestamppb.New(firedAt.Add(-9 * time.Minute)),
			}}},
			{SourceId: "otel:prod", Gaps: []*graphv1.Interval{{
				// Entirely before the window: not this investigation's problem.
				Start: timestamppb.New(firedAt.Add(-10 * time.Hour)),
				End:   timestamppb.New(firedAt.Add(-9 * time.Hour)),
			}}},
		},
	}}
	report, err := intake.ConsultExtent(ctx, src, in)
	if err != nil {
		t.Fatalf("consult extent: %v", err)
	}
	if !report.GapAffected() {
		t.Fatal("an 11-minute k8s gap inside the window was not flagged (FR-032)")
	}
	if got := report.SourceIDs(); len(got) != 1 || got[0] != "k8s:prod" {
		t.Errorf("gap sources = %v, want only k8s:prod", got)
	}
	if got := report.Gaps[0].OverlapSeconds; got != 11*60 {
		t.Errorf("overlap = %.0fs, want 660s", got)
	}
	if report.Evidence.Outcome != "partial" {
		t.Errorf("evidence outcome = %q, want partial: the window is covered in part, which is "+
			"not `no_data` (FR-027)", report.Evidence.Outcome)
	}
	if !strings.Contains(report.Summary(), "k8s:prod") {
		t.Errorf("summary = %q, want it to name the source", report.Summary())
	}
}

func TestConsultExtentWithNoGapIsNoData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: monitorBody()})
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	report, err := intake.ConsultExtent(ctx, stubExtent{extent: &graphv1.Extent{
		EarliestObserved: timestamppb.New(firedAt.Add(-24 * time.Hour)),
	}}, in)
	if err != nil {
		t.Fatalf("consult extent: %v", err)
	}
	if report.GapAffected() {
		t.Errorf("gaps = %v, want none", report.Gaps)
	}
	if report.Evidence.Outcome != "no_data" {
		t.Errorf("outcome = %q, want no_data", report.Evidence.Outcome)
	}
}

func TestConsultExtentFlagsAWindowBeforeTheGraphsHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: monitorBody()})
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	report, err := intake.ConsultExtent(ctx, stubExtent{extent: &graphv1.Extent{
		EarliestObserved: timestamppb.New(firedAt.Add(-10 * time.Minute)),
	}}, in)
	if err != nil {
		t.Fatalf("consult extent: %v", err)
	}
	if !report.BeforeHistory {
		t.Fatal("a window starting before the graph's earliest observation is not fully covered")
	}
	if !strings.Contains(report.Summary(), "earliest") {
		t.Errorf("summary = %q, want it to say the window predates the history", report.Summary())
	}
}

func TestNormaliseReferenceIsTheThirdDoorIntoOneShape(t *testing.T) {
	t.Parallel()

	in, err := intake.NormaliseReference(intake.ExplicitReference{
		Ref:       &graphv1.Ref{Namespace: "k8s.service", Value: "shop/checkout"},
		At:        firedAt,
		Requester: "oidc|alice",
	})
	if err != nil {
		t.Fatalf("normalise reference: %v", err)
	}
	if got, want := in.Symptom.GetTransport(), intake.TransportExplicitReference; got != want {
		t.Errorf("transport = %q, want %q", got, want)
	}
	if !in.ObservedAt.Equal(firedAt) {
		t.Errorf("observed instant = %s, want the asked-about instant", in.ObservedAt)
	}
	if _, err := graph.ParseRef(in.Symptom.GetOriginRef()); err != nil {
		t.Errorf("origin ref %q is not a node reference: %v", in.Symptom.GetOriginRef(), err)
	}
}
