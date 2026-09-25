// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
)

// Typed stop reasons (tasks.md T064; FR-045b).
//
// Every ending has a name, the set is closed, and the reason appears in both renderings. A stop
// that is only in the JSON is a stop the on-call never sees; one that is only in the prose is a
// stop no evaluation can score.

// TestTheStopReasonSetIsTheSix.
func TestTheStopReasonSetIsTheSix(t *testing.T) {
	t.Parallel()

	want := []engine.StopReason{
		engine.StopCompleted, engine.StopBudgetExhausted, engine.StopDiminishingReturns,
		engine.StopWorkerUnavailable, engine.StopRefused, engine.StopFailed,
	}
	if len(engine.StopReasons) != len(want) {
		t.Fatalf("stop reasons = %v, want exactly %v", engine.StopReasons, want)
	}
	for i, reason := range want {
		if engine.StopReasons[i] != reason {
			t.Errorf("stop reason %d = %s, want %s", i, engine.StopReasons[i], reason)
		}
		if !reason.Valid() {
			t.Errorf("%s does not validate", reason)
		}
		if reason.Proto() == investigationv1.StopReason_STOP_REASON_UNSPECIFIED {
			t.Errorf("%s maps to the unspecified enum value", reason)
		}
	}
	if engine.StopReason("gave_up").Valid() {
		t.Error("an unpublished stop reason validated")
	}
}

// TestEveryStopButCompletedNamesWhatBoundIt (FR-045b).
func TestEveryStopButCompletedNamesWhatBoundIt(t *testing.T) {
	t.Parallel()

	if err := (engine.Stop{Reason: engine.StopCompleted}).Validate(); err != nil {
		t.Errorf("a completed stop with no detail was refused: %v", err)
	}
	for _, reason := range []engine.StopReason{
		engine.StopBudgetExhausted, engine.StopDiminishingReturns,
		engine.StopWorkerUnavailable, engine.StopRefused, engine.StopFailed,
	} {
		if err := (engine.Stop{Reason: reason}).Validate(); err == nil {
			t.Errorf("a %s stop with no detail was accepted", reason)
		}
		stop := engine.Stop{Reason: reason, Detail: "wall_time"}
		if err := stop.Validate(); err != nil {
			t.Errorf("%s with a detail was refused: %v", reason, err)
		}
		if !strings.Contains(stop.Line(), "wall_time") {
			t.Errorf("%s: the written line does not carry the detail: %q", reason, stop.Line())
		}
		if !strings.Contains(stop.String(), string(reason)) {
			t.Errorf("%s: the machine form does not carry the reason: %q", reason, stop.String())
		}
	}
}

// TestARefusalStopCarriesTheCategoryAndExplanation (FR-045b).
func TestARefusalStopCarriesTheCategoryAndExplanation(t *testing.T) {
	t.Parallel()

	stop := engine.RefusalStop("cyber", "this reads as an intrusion attempt")
	if err := stop.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !strings.Contains(stop.Detail, "cyber") || !strings.Contains(stop.Detail, "intrusion") {
		t.Errorf("detail = %q", stop.Detail)
	}
	if !strings.Contains(stop.Line(), "Nothing was retried") {
		t.Errorf("the written line does not say a refusal is never retried: %q", stop.Line())
	}

	bare := engine.RefusalStop("", "")
	if err := bare.Validate(); err != nil {
		t.Errorf("a refusal with no category was not recordable: %v", err)
	}
}

// TestTheOutcomeIsDerivedFromTheStop: the stop reason and the investigation outcome cannot
// disagree, because one is computed from the other (FR-045b).
func TestTheOutcomeIsDerivedFromTheStop(t *testing.T) {
	t.Parallel()

	cases := []struct {
		stop   engine.Stop
		ranked bool
		want   investigationv1.InvestigationOutcome
	}{
		{engine.Stop{Reason: engine.StopCompleted}, true, investigationv1.InvestigationOutcome_RANKED},
		{engine.Stop{Reason: engine.StopCompleted}, false, investigationv1.InvestigationOutcome_UNKNOWN},
		{engine.Stop{Reason: engine.StopBudgetExhausted, Detail: "wall_time"}, true,
			investigationv1.InvestigationOutcome_BUDGET_EXHAUSTED},
		{engine.Stop{Reason: engine.StopFailed, Detail: "a defect"}, true,
			investigationv1.InvestigationOutcome_INVESTIGATION_FAILED},
		{engine.Stop{Reason: engine.StopRefused, Detail: "cyber"}, false,
			investigationv1.InvestigationOutcome_UNKNOWN},
	}
	for _, each := range cases {
		if got := each.stop.Outcome(each.ranked); got != each.want {
			t.Errorf("%s (ranked=%t) → %s, want %s", each.stop.Reason, each.ranked, got, each.want)
		}
	}
}
