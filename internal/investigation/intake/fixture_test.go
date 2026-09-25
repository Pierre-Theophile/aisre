// SPDX-License-Identifier: Apache-2.0

package intake_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// Intake against the corpus fixtures (T076–T078, fixtures `declared-incident-01` and
// `human-fact-reopen-01`).
//
// These read the fixtures' own `events.jsonl` rather than a hand-built message, which is the
// point: the fixture is what the evaluation harness grades against, so an intake that normalises
// a hand-built declaration correctly and the fixture's declaration incorrectly has passed the
// wrong test.

const incidentsRoot = "../../../fixtures/incidents"

// fixtureAlerts reads every `alert_transition` out of a fixture's event log, in order, with the
// source that delivered it.
func fixtureAlerts(t *testing.T, fixture string) []fixtureAlert {
	t.Helper()

	path := filepath.Join(incidentsRoot, fixture, "events.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []fixtureAlert
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The log adds columns EventEnvelope does not have, so the envelope is rebuilt from the
		// fields that are part of the published schema.
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		body, ok := row["alertTransition"]
		if !ok {
			continue
		}
		var transition graphv1.AlertTransition
		if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(body, &transition); err != nil {
			t.Fatalf("parse alert transition in %s: %v", path, err)
		}
		var sourceID, eventID string
		_ = json.Unmarshal(row["sourceId"], &sourceID)
		_ = json.Unmarshal(row["eventId"], &eventID)
		out = append(out, fixtureAlert{SourceID: sourceID, EventID: eventID, Body: &transition})
	}
	if len(out) == 0 {
		t.Fatalf("%s carries no alert transition", path)
	}
	return out
}

type fixtureAlert struct {
	SourceID string
	EventID  string
	Body     *graphv1.AlertTransition
}

// TestDeclaredIncidentFixtureNormalises is FR-001a, FR-002a and FR-004a against the fixture that
// exists to pin them: a declaration with no monitor is a first-class intake, pinned at the
// instant of declaration.
func TestDeclaredIncidentFixtureNormalises(t *testing.T) {
	t.Parallel()

	alerts := fixtureAlerts(t, "declared-incident-01")

	var declarations, monitors int
	for _, alert := range alerts {
		in, err := intake.NormaliseAlert(intake.MonitorIntake{
			SourceID: alert.SourceID, Transition: alert.Body,
		})
		if err != nil {
			t.Fatalf("normalise %s: %v", alert.EventID, err)
		}
		firedAt := alert.Body.GetTransitionAt().AsTime()

		// FR-004: both instants are pinned, and the observed one defaults to the symptom
		// instant, for either front door.
		if !in.ValidAt.Equal(firedAt) || !in.ObservedAt.Equal(firedAt) {
			t.Errorf("%s: instants (%s, %s), want both at %s",
				alert.EventID, in.ValidAt, in.ObservedAt, firedAt)
		}

		if alert.Body.GetTransport() == intake.TransportHumanDeclared {
			declarations++
			if alert.Body.GetActorKind() != graphv1.ActorKind_PERSON {
				t.Errorf("%s: the fixture's declaration is not a PERSON act", alert.EventID)
			}
			if in.Symptom.GetSeverity() != alert.Body.GetSeverity() {
				t.Errorf("%s: severity %q, want the declared %q",
					alert.EventID, in.Symptom.GetSeverity(), alert.Body.GetSeverity())
			}
			if in.Symptom.GetDeclaringIdentity() == "" && alert.Body.GetDeclaringIdentity() != "" {
				// NormaliseAlert reads the monitor door; a declaration delivered as an event
				// carries its declaring identity on the body, and the declaration door keeps it.
				declaration := declarationFrom(alert)
				declared, err := intake.NormaliseDeclaration(declaration)
				if err != nil {
					t.Fatalf("%s: normalise declaration: %v", alert.EventID, err)
				}
				if declared.Symptom.GetDeclaringIdentity() != alert.Body.GetDeclaringIdentity() {
					t.Errorf("%s: declaring identity lost", alert.EventID)
				}
				if declared.Symptom.GetOrigin() != investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED {
					t.Errorf("%s: origin = %v, want DECLARED (FR-002a)",
						alert.EventID, declared.Symptom.GetOrigin())
				}
			}
		} else {
			monitors++
		}
	}
	if declarations == 0 {
		t.Error("the fixture has no human declaration; it exists to pin that front door")
	}
	if monitors == 0 {
		t.Error("the fixture has no monitor transition; FR-008c groups the two doors")
	}
}

// declarationFrom rebuilds the declaration intake from a fixture event, the way the connector
// that observed it would.
func declarationFrom(alert fixtureAlert) intake.Declaration {
	targets := make([]intake.DeclaredTarget, 0, len(alert.Body.GetWatches()))
	for _, watched := range alert.Body.GetWatches() {
		targets = append(targets, intake.DeclaredTarget{
			Ref: watched,
			// The fixture's manifest says the target was parsed out of `origin_ref` by the
			// connector's slug rule. What the investigation records about that is the rule id
			// and the rule version, and nothing else (FR-002b, U2).
			Provenance: intake.TargetProvenanceParsed,
			Rule:       intake.RuleRef{ID: "slack-origin-slug", Version: "1"},
		})
	}
	return intake.Declaration{
		SourceID:          alert.SourceID,
		PlaceID:           graph.RefFromProto(alert.Body.GetMonitor()).Value,
		OriginRef:         alert.Body.GetOriginRef(),
		DeclaredAt:        alert.Body.GetTransitionAt().AsTime(),
		Severity:          alert.Body.GetSeverity(),
		Title:             alert.Body.GetTitle(),
		DeclaringIdentity: alert.Body.GetDeclaringIdentity(),
		Targets:           targets,
	}
}

// TestDeclaredIncidentFixtureRedeliveryIsANoOp is FR-008b against the fixture's own second
// delivery: the chat integration retried 41 seconds later under a different event id, and it is
// the same fact.
func TestDeclaredIncidentFixtureRedeliveryIsANoOp(t *testing.T) {
	t.Parallel()

	keys := map[string][]string{}
	for _, alert := range fixtureAlerts(t, "declared-incident-01") {
		in, err := intake.NormaliseAlert(intake.MonitorIntake{
			SourceID: alert.SourceID, Transition: alert.Body,
		})
		if err != nil {
			t.Fatalf("normalise %s: %v", alert.EventID, err)
		}
		keys[in.IdempotencyKey()] = append(keys[in.IdempotencyKey()], alert.EventID)
	}

	var shared int
	for key, eventIDs := range keys {
		if len(eventIDs) > 1 {
			shared++
			t.Logf("key %s is shared by %v, which is the re-delivery the fixture pins", key, eventIDs)
		}
	}
	if shared == 0 {
		t.Error("no two events in declared-incident-01 share a key; the fixture's re-delivery " +
			"(same facts, same instant, different event id) must be one fact under FR-008b")
	}
}

// TestDeclaredIncidentFixtureUnparseableTargetAsksForOne is FR-002b: the declaration whose target
// text matches no rule carries no entity at all, and the correct outcome is `unknown` with a
// `confirm_identity` request rather than a guess.
func TestDeclaredIncidentFixtureUnparseableTargetAsksForOne(t *testing.T) {
	t.Parallel()

	var found bool
	for _, alert := range fixtureAlerts(t, "declared-incident-01") {
		if alert.Body.GetTransport() != intake.TransportHumanDeclared {
			continue
		}
		if len(alert.Body.GetWatches()) != 0 {
			continue
		}
		found = true
		in, err := intake.NormaliseDeclaration(declarationFrom(alert))
		if err != nil {
			t.Fatalf("normalise %s: %v", alert.EventID, err)
		}
		if !in.UnresolvedTargets {
			t.Errorf("%s: a declaration nobody could parse must be flagged, not guessed", alert.EventID)
		}
		if len(in.Symptom.GetTargetRefs()) != 0 {
			t.Errorf("%s: target refs = %v, want none", alert.EventID, in.Symptom.GetTargetRefs())
		}
		res := intake.MissingTargetRequest(in)
		if res.GetKind() != intake.ResolutionKindConfirmIdentity {
			t.Errorf("%s: resolution kind = %q, want %q",
				alert.EventID, res.GetKind(), intake.ResolutionKindConfirmIdentity)
		}
	}
	if !found {
		t.Error("declared-incident-01 has no unparseable declaration; the fixture's manifest " +
			"says it does (FR-002b)")
	}
}

// TestDeclaredIncidentFixtureGroupsTheMonitorIntoTheDeclaredIncident is FR-008c against the
// fixture: the declaration opens the incident at 14:32 and the monitor that fires five minutes
// later on the neighbouring service attaches rather than opening a second investigation.
func TestDeclaredIncidentFixtureGroupsTheMonitorIntoTheDeclaredIncident(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	alerts := fixtureAlerts(t, "declared-incident-01")

	var declaration, monitor *fixtureAlert
	for i := range alerts {
		alert := &alerts[i]
		switch {
		case alert.Body.GetTransport() == intake.TransportHumanDeclared &&
			len(alert.Body.GetWatches()) > 0 && declaration == nil:
			declaration = alert
		case alert.Body.GetTransport() != intake.TransportHumanDeclared && monitor == nil:
			monitor = alert
		}
	}
	if declaration == nil || monitor == nil {
		t.Fatal("the fixture must carry both a parseable declaration and a monitor transition")
	}

	declaredIn, err := intake.NormaliseDeclaration(declarationFrom(*declaration))
	if err != nil {
		t.Fatalf("normalise declaration: %v", err)
	}
	monitorIn, err := intake.NormaliseAlert(intake.MonitorIntake{
		SourceID: monitor.SourceID, Transition: monitor.Body,
	})
	if err != nil {
		t.Fatalf("normalise monitor: %v", err)
	}

	// The declaration arrives first and opens the incident.
	opened, err := intake.Group(ctx, intake.DefaultGroupingRule(), declaredIn,
		[]string{"e:checkout"}, nil, fixtureNeighbourhood{})
	if err != nil {
		t.Fatalf("group declaration: %v", err)
	}
	if opened.Attached {
		t.Fatal("the first arrival attached to something; it opens the incident (FR-008c)")
	}

	// The monitor fires minutes later on payments, one hop from checkout, and attaches.
	decision, err := intake.Group(ctx, intake.DefaultGroupingRule(), monitorIn,
		[]string{"e:payments"},
		[]intake.OpenIncident{{
			IncidentID:      opened.IncidentID,
			InvestigationID: "inv-fixture-01",
			OpenedAt:        declaredIn.ValidAt,
			LastSymptomAt:   declaredIn.ValidAt,
			EntityIDs:       []string{"e:checkout"},
		}},
		fixtureNeighbourhood{})
	if err != nil {
		t.Fatalf("group monitor: %v", err)
	}
	if !decision.Attached {
		t.Fatalf("the monitor opened a second investigation (%+v); two investigations for one "+
			"incident are a defect (FR-008a)", decision)
	}
	if decision.IncidentID != opened.IncidentID {
		t.Errorf("attached to %s, want %s", decision.IncidentID, opened.IncidentID)
	}
	gap := monitorIn.ValidAt.Sub(declaredIn.ValidAt)
	if decision.TimeDistance != gap {
		t.Errorf("time distance = %s, want the fixture's %s", decision.TimeDistance, gap)
	}
	// FR-008a: the decision is evidence, and it carries the distances it rested on.
	ev := decision.Evidence(monitorIn.ValidAt, monitorIn.ObservedAt, monitorIn.ValidAt)
	if ev.Coverage == nil || ev.FreeText == "" {
		t.Fatalf("the grouping decision was not recorded as evidence: %+v", ev)
	}
	if !strings.Contains(ev.FreeText, decision.IncidentID) {
		t.Errorf("the evidence does not name the incident it grouped into: %q", ev.FreeText)
	}
}

// fixtureNeighbourhood is the fixture's own topology: storefront calls checkout calls payments,
// so checkout and payments are one hop apart.
type fixtureNeighbourhood struct{}

func (fixtureNeighbourhood) Distance(_ context.Context, a, b string) (int, bool, error) {
	if a == b {
		return 0, true, nil
	}
	pairs := map[string]string{
		"e:checkout": "e:payments",
		"e:payments": "e:checkout",
	}
	if pairs[a] == b {
		return 1, true, nil
	}
	return 0, false, nil
}

// TestHumanFactReopenFixtureCarriesTheFactAndTheLink is the intake-side half of US9 against
// `human-fact-reopen-01`: the fact the fixture records is typed, attributable, time-bounded and
// aimed at a concluded investigation, and the reopen links a NEW investigation to the parent.
func TestHumanFactReopenFixtureCarriesTheFactAndTheLink(t *testing.T) {
	t.Parallel()

	path := filepath.Join(incidentsRoot, "human-fact-reopen-01", "events.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var fact *graphv1.SubmitHumanFact
	var reopen *graphv1.ReopenInvestigation
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if body, ok := row["submitHumanFact"]; ok {
			var parsed graphv1.SubmitHumanFact
			if err := protojson.Unmarshal(body, &parsed); err != nil {
				t.Fatalf("parse fact: %v", err)
			}
			fact = &parsed
		}
		if body, ok := row["reopenInvestigation"]; ok {
			var parsed graphv1.ReopenInvestigation
			if err := protojson.Unmarshal(body, &parsed); err != nil {
				t.Fatalf("parse reopen: %v", err)
			}
			reopen = &parsed
		}
	}
	if fact == nil || reopen == nil {
		t.Fatal("human-fact-reopen-01 must carry a submit_human_fact and a reopen_investigation")
	}

	// FR-057a: typed, authenticated, time-bounded, and about named entities.
	if fact.GetAuthor() == "" {
		t.Error("the fact carries no authenticated author (FR-066)")
	}
	if fact.GetSubmittedAt() == nil {
		t.Error("the fact carries no submission instant")
	}
	if fact.GetConcernsFrom() == nil {
		t.Error("the fact carries no interval; a fact about 14:25 is not a fact about now")
	}
	if len(fact.GetEntities()) == 0 {
		t.Error("the fact names no entity")
	}

	// The fact concerns an instant well before it was submitted, which is exactly the case
	// Invariant 7 exists for: the investigation's observed pin does not move because someone
	// told it something later.
	submitted := fact.GetSubmittedAt().AsTime()
	concerns := fact.GetConcernsFrom().AsTime()
	if !concerns.Before(submitted) {
		t.Errorf("the fact concerns %s and was submitted at %s; the fixture's point is that a "+
			"fact arrives after what it is about", concerns, submitted)
	}

	// FR-057b: a NEW investigation linked to the parent, and the parent is not rewritten.
	if reopen.GetParentInvestigationId() == "" || reopen.GetChildInvestigationId() == "" {
		t.Fatalf("the reopen does not link two investigations: %+v", reopen)
	}
	if reopen.GetParentInvestigationId() == reopen.GetChildInvestigationId() {
		t.Error("the reopen links an investigation to itself; a reopen is a new record (FR-057b)")
	}
	if reopen.GetParentInvestigationId() != fact.GetInvestigationId() {
		t.Errorf("the reopen's parent %q is not the investigation the fact was pushed at %q",
			reopen.GetParentInvestigationId(), fact.GetInvestigationId())
	}
	if reopen.GetCause() != "human_fact" {
		t.Errorf("reopen cause = %q, want human_fact", reopen.GetCause())
	}
}

// TestFixtureKeysAreThePublished4Tuple checks the two fixtures' alert keys against the published
// helper, so a fixture and the engine cannot drift apart on the one thing that decides whether a
// re-delivery opens a second investigation.
func TestFixtureKeysAreThePublished4Tuple(t *testing.T) {
	t.Parallel()

	for _, alert := range fixtureAlerts(t, "declared-incident-01") {
		in, err := intake.NormaliseAlert(intake.MonitorIntake{
			SourceID: alert.SourceID, Transition: alert.Body,
		})
		if err != nil {
			t.Fatalf("normalise %s: %v", alert.EventID, err)
		}
		want := eventlog.AlertTransitionKeyParts(
			alert.SourceID,
			graph.RefFromProto(alert.Body.GetMonitor()).String(),
			alert.Body.GetGroupKey(),
			alert.Body.GetTransitionAt().AsTime().UTC().Format(time.RFC3339Nano))
		if in.IdempotencyKey() != want {
			t.Errorf("%s: key = %q, want the published 4-tuple %q", alert.EventID, in.IdempotencyKey(), want)
		}
		if alert.Body.GetTransport() == intake.TransportHumanDeclared &&
			alert.Body.GetGroupKey() != "" {
			t.Errorf("%s: a declaration carries group %q; ADR-0005 D2 says it is empty",
				alert.EventID, alert.Body.GetGroupKey())
		}
	}
}
