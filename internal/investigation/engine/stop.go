// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"strings"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// Typed stop reasons (T064, FR-045b).
//
// Every investigation ends, and every ending has a name. The set is closed, and it is closed on
// purpose: "it finished" and "it ran out of budget" and "the model declined" are three different
// things to tell the person reading the report at 03:00, and an engine that collapsed them would
// be reporting a partial answer as a complete one.
//
// The reason appears in **both** renderings — the machine-readable investigation and the written
// report — because a stop that is only in the JSON is a stop the on-call never sees, and a stop
// that is only in the prose is a stop no evaluation can score.

// StopReason is one of the six published reasons.
type StopReason string

// The six published stop reasons.
const (
	// StopCompleted is the ordinary ending: the investigation reached a verdict it could
	// evidence, or `unknown` with what would resolve it.
	StopCompleted StopReason = "completed"
	// StopBudgetExhausted is a budget reached. The detail names which one: a stop that does not
	// say which budget bound it is a stop nobody can act on.
	StopBudgetExhausted StopReason = "budget_exhausted"
	// StopDiminishingReturns is the control law firing: the posterior stopped moving.
	StopDiminishingReturns StopReason = "diminishing_returns"
	// StopWorkerUnavailable is a worker the investigation needed that could not answer at all.
	StopWorkerUnavailable StopReason = "worker_unavailable"
	// StopRefused is an API refusal. It is handled and recorded like any other terminal
	// condition and never silently retried (FR-045b).
	StopRefused StopReason = "refused"
	// StopFailed is an engine failure. It is the only one that is a defect rather than an
	// outcome.
	StopFailed StopReason = "failed"
)

// StopReasons is the published set, in the order a rendering lists them.
var StopReasons = []StopReason{
	StopCompleted, StopBudgetExhausted, StopDiminishingReturns,
	StopWorkerUnavailable, StopRefused, StopFailed,
}

// Valid reports whether r is one of the published reasons.
func (r StopReason) Valid() bool {
	for _, each := range StopReasons {
		if each == r {
			return true
		}
	}
	return false
}

// Proto maps the reason onto the published enum.
func (r StopReason) Proto() investigationv1.StopReason {
	switch r {
	case StopCompleted:
		return investigationv1.StopReason_COMPLETED
	case StopBudgetExhausted:
		return investigationv1.StopReason_BUDGET_EXHAUSTED_STOP
	case StopDiminishingReturns:
		return investigationv1.StopReason_DIMINISHING_RETURNS
	case StopWorkerUnavailable:
		return investigationv1.StopReason_WORKER_UNAVAILABLE
	case StopRefused:
		return investigationv1.StopReason_REFUSED
	case StopFailed:
		return investigationv1.StopReason_ENGINE_FAILED
	default:
		return investigationv1.StopReason_STOP_REASON_UNSPECIFIED
	}
}

// Stop is a typed stop with the detail that makes it actionable.
type Stop struct {
	// Reason is one of the six.
	Reason StopReason
	// Detail names the budget, the worker, the refusal category or the failure. It is required
	// for every reason but `completed`, where it is the one-line summary of why the engine
	// judged itself finished.
	Detail string
}

// Validate refuses a stop that would be recorded uninterpretably.
func (s Stop) Validate() error {
	if !s.Reason.Valid() {
		return fmt.Errorf("engine: stop reason %q is not one of the published reasons %v", s.Reason, StopReasons)
	}
	if s.Reason != StopCompleted && strings.TrimSpace(s.Detail) == "" {
		return fmt.Errorf("engine: a %s stop carries a detail; %s",
			s.Reason, detailRequirement[s.Reason])
	}
	return nil
}

// detailRequirement says what each reason's detail has to carry, so the error message teaches
// rather than scolds.
var detailRequirement = map[StopReason]string{
	StopBudgetExhausted:    "it names the budget that bound (FR-045b)",
	StopDiminishingReturns: "it names the threshold and the window of calls it was measured over",
	StopWorkerUnavailable:  "it names the worker and what it was being asked",
	StopRefused:            "it carries the refusal category and explanation as the API gave them",
	StopFailed:             "it says what failed",
}

// Line renders the stop as it appears in the written report — one sentence, no jargon a person
// would have to look up.
func (s Stop) Line() string {
	switch s.Reason {
	case StopCompleted:
		if s.Detail == "" {
			return "Stopped: the investigation reached a verdict it can evidence."
		}
		return "Stopped: " + s.Detail
	case StopBudgetExhausted:
		return "Stopped early: the " + s.Detail + " budget was reached. " +
			"What follows is what was established before that, with the untested hypotheses listed."
	case StopDiminishingReturns:
		return "Stopped early: further evidence stopped moving the ranking (" + s.Detail + ")."
	case StopWorkerUnavailable:
		return "Stopped early: " + s.Detail + ". The hypotheses it would have tested are reported untested."
	case StopRefused:
		return "Stopped: the model declined to answer (" + s.Detail + "). Nothing was retried."
	case StopFailed:
		return "Stopped: the engine failed (" + s.Detail + "). This is a defect, not a finding."
	default:
		return "Stopped: " + string(s.Reason)
	}
}

// String renders "budget_exhausted: wall_time".
func (s Stop) String() string {
	if s.Detail == "" {
		return string(s.Reason)
	}
	return string(s.Reason) + ": " + s.Detail
}

// Outcome maps a stop onto the investigation outcome it implies, which is the other half of
// FR-045b: the stop reason and the outcome must agree, and deriving one from the other is how
// they cannot disagree.
func (s Stop) Outcome(ranked bool) investigationv1.InvestigationOutcome {
	switch s.Reason {
	case StopFailed:
		return investigationv1.InvestigationOutcome_INVESTIGATION_FAILED
	case StopBudgetExhausted:
		return investigationv1.InvestigationOutcome_BUDGET_EXHAUSTED
	case StopCompleted, StopDiminishingReturns, StopWorkerUnavailable, StopRefused:
		if ranked {
			return investigationv1.InvestigationOutcome_RANKED
		}
		return investigationv1.InvestigationOutcome_UNKNOWN
	default:
		return investigationv1.InvestigationOutcome_INVESTIGATION_OUTCOME_UNSPECIFIED
	}
}

// RefusalStop builds the stop for an API refusal, carrying the category and explanation verbatim.
func RefusalStop(category, explanation string) Stop {
	detail := category
	if detail == "" {
		detail = "category not given"
	}
	if explanation != "" {
		detail += ": " + explanation
	}
	return Stop{Reason: StopRefused, Detail: detail}
}
