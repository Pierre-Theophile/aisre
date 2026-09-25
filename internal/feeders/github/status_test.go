// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
)

// What a deployment status means (004 T048–T053, FR-012, FR-022, SC-003).
//
// GitHub designates no terminal state, so every row of this table is this connector's decision. These
// tests are the review of it.

var (
	runStarted  = time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	builtAt     = time.Date(2026, 3, 1, 14, 1, 30, 0, time.UTC)
	completedAt = time.Date(2026, 3, 1, 14, 3, 12, 0, time.UTC)
	polledAt    = time.Date(2026, 3, 1, 14, 9, 0, 0, time.UTC)
)

func status(state string, at time.Time) github.DeploymentStatus {
	return github.DeploymentStatus{State: state, CreatedAt: at, Environment: "production"}
}

// The published table: each state's outcome, stated once and in one place.
func TestEveryDocumentedStateHasAPublishedOutcome(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]github.StatusOutcome{
		github.StateSuccess:    github.OutcomeRollout,
		github.StateFailure:    github.OutcomeFailedAttempt,
		github.StateError:      github.OutcomeFailedAttempt,
		github.StateInactive:   github.OutcomeProperty,
		github.StateQueued:     github.OutcomeNothing,
		github.StateInProgress: github.OutcomeNothing,
		github.StatePending:    github.OutcomeNothing,
	} {
		got := github.DispositionOf(state)
		if got.Outcome != want {
			t.Errorf("DispositionOf(%q).Outcome = %q, want %q", state, got.Outcome, want)
		}
		if got.Why == "" {
			t.Errorf("DispositionOf(%q) carries no reason; GitHub designates no terminal state, so every "+
				"row of this table is a decision somebody has to be able to review", state)
		}
	}
	// And the table has exactly the seven GitHub documents — a row nobody added and a row nobody
	// removed are both things a reader of the connector page needs to see.
	if got := github.Dispositions(); len(got) != 7 {
		t.Errorf("the published table has %d rows, want GitHub's seven documented states", len(got))
	}
}

// T048: a success is the ROLLOUT, and its valid time is the platform's completion instant.
//
// T052/SC-003: not the run's start, not the build instant, and not the instant this connector polled.
// Each of those three is a plausible wrong answer and the history below contains all three so that
// picking any of them fails.
func TestASuccessRollsOutAtThePlatformsCompletionInstant(t *testing.T) {
	t.Parallel()
	got := github.ReadStatuses([]github.DeploymentStatus{
		// Newest first, as GitHub returns them.
		status(github.StateSuccess, completedAt),
		status(github.StateInProgress, builtAt),
		status(github.StateQueued, runStarted),
	})
	if !got.RolledOut {
		t.Fatal("a history ending in success did not roll out")
	}
	if !got.CompletionStated {
		t.Fatal("the completion instant was not stated, though the success status carried one")
	}
	switch {
	case got.CompletedAt.Equal(runStarted):
		t.Error("the rollout is dated at the run's start; a queued deployment has not moved anything")
	case got.CompletedAt.Equal(builtAt):
		t.Error("the rollout is dated at the build instant; building is not deploying (SC-003)")
	case got.CompletedAt.Equal(polledAt):
		t.Error("the rollout is dated when we polled, which dates the change to when we noticed it")
	case !got.CompletedAt.Equal(completedAt):
		t.Errorf("CompletedAt = %v, want the platform's completion instant %v", got.CompletedAt, completedAt)
	}
}

// T049: queued, in_progress and pending produce nothing, because nothing in production has moved.
func TestProgressStatesProduceNothing(t *testing.T) {
	t.Parallel()
	for _, state := range []string{github.StateQueued, github.StateInProgress, github.StatePending} {
		got := github.ReadStatuses([]github.DeploymentStatus{status(state, completedAt)})
		switch {
		case got.RolledOut:
			t.Errorf("%q was read as a rollout; nothing in production has moved", state)
		case got.Attempted:
			t.Errorf("%q was read as a failed attempt; nothing has failed either", state)
		case got.Deactivated:
			t.Errorf("%q was read as a deactivation", state)
		case len(got.Unrecognised) != 0:
			t.Errorf("%q was counted as unrecognised, though the table rules on it: %v",
				state, got.Unrecognised)
		}
	}
}

// T050: failure and error are a failed attempt and never a ROLLOUT, and which of the two the platform
// said is kept rather than flattened.
func TestFailureAndErrorAreAttemptsAndNeverRollouts(t *testing.T) {
	t.Parallel()
	for _, state := range []string{github.StateFailure, github.StateError} {
		got := github.ReadStatuses([]github.DeploymentStatus{
			status(state, completedAt),
			status(github.StateInProgress, builtAt),
		})
		switch {
		case got.RolledOut:
			t.Errorf("%q was read as a rollout; nothing in production changed, and a change node for it "+
				"would put an event that changed nothing on the graph", state)
		case !got.Attempted:
			t.Errorf("%q recorded no attempt; an operator asking what changed around then wants to know "+
				"a deploy was tried and did not land", state)
		case got.AttemptState != state:
			t.Errorf("the attempt was recorded as %q, want %q; GitHub distinguishes the two by cause and "+
				"re-labelling somebody else's vocabulary loses that for no gain", got.AttemptState, state)
		case !got.AttemptedAt.Equal(completedAt):
			t.Errorf("AttemptedAt = %v, want %v", got.AttemptedAt, completedAt)
		}
	}
}

// T051 / FR-022: a later `inactive` is a property of the same rollout, never a second one.
func TestALaterInactiveIsAPropertyOfTheSameRollout(t *testing.T) {
	t.Parallel()
	supersededAt := completedAt.Add(6 * time.Hour)
	got := github.ReadStatuses([]github.DeploymentStatus{
		status(github.StateInactive, supersededAt),
		status(github.StateSuccess, completedAt),
	})
	switch {
	case !got.RolledOut:
		t.Fatal("the rollout was lost once a deactivation followed it")
	case got.Successes != 1:
		t.Errorf("Successes = %d; `inactive` must not be counted as a second rollout — the deploy would "+
			"be doubled, and the second one dated at the moment the FIRST stopped serving (FR-022)",
			got.Successes)
	case !got.CompletedAt.Equal(completedAt):
		t.Errorf("CompletedAt = %v, want the success instant %v; the deactivation is a fact about this "+
			"rollout, not its date", got.CompletedAt, completedAt)
	case !got.Deactivated:
		t.Error("the deactivation was dropped rather than recorded against the rollout")
	case !got.DeactivatedAt.Equal(supersededAt):
		t.Errorf("DeactivatedAt = %v, want %v", got.DeactivatedAt, supersededAt)
	}
}

// An `inactive` with no success has nothing to be a property of, and attaching it to a rollout that was
// never stated would invent the rollout.
func TestAnInactiveWithNoSuccessInventsNoRollout(t *testing.T) {
	t.Parallel()
	got := github.ReadStatuses([]github.DeploymentStatus{
		status(github.StateInactive, completedAt),
	})
	if got.RolledOut {
		t.Error("a deployment marked inactive with no success was read as a rollout")
	}
	if !got.DeactivatedWithoutRollingOut() {
		t.Error("the case is not reported, so a caller cannot count it and it would pass over silently")
	}
}

// T053: where the platform states a success with no instant, the valid start is unknown rather than
// guessed. Filling it with the poll instant would date the rollout to when we noticed it.
func TestASuccessWithNoInstantLeavesTheValidStartUnknown(t *testing.T) {
	t.Parallel()
	got := github.ReadStatuses([]github.DeploymentStatus{
		{State: github.StateSuccess, Environment: "production"},
	})
	switch {
	case !got.RolledOut:
		t.Fatal("a success with no instant was read as no rollout at all; the platform did state it")
	case got.CompletionStated:
		t.Error("the completion instant is reported as stated, though the payload carried none")
	case !got.ValidStartUnknown():
		t.Error("ValidStartUnknown() is false; the valid start has to be recorded as unknown rather " +
			"than filled in (T053)")
	case !got.CompletedAt.IsZero():
		t.Errorf("CompletedAt = %v for a payload that stated none", got.CompletedAt)
	}
}

// The earliest success is the rollout's instant, because that is when it first went live — and a
// history carrying more than one against one deployment id is an ambiguity to surface rather than
// average.
func TestSeveralSuccessesTakeTheEarliestAndReportTheAmbiguity(t *testing.T) {
	t.Parallel()
	second := completedAt.Add(time.Hour)
	got := github.ReadStatuses([]github.DeploymentStatus{
		status(github.StateSuccess, second),
		status(github.StateSuccess, completedAt),
	})
	if !got.CompletedAt.Equal(completedAt) {
		t.Errorf("CompletedAt = %v, want the earliest success %v — when it first went live",
			got.CompletedAt, completedAt)
	}
	if !got.Ambiguous() {
		t.Errorf("Successes = %d and the ambiguity is not reported; FR-028 keys a re-run as a NEW "+
			"change, so two successes against one deployment id is something to surface", got.Successes)
	}
}

// The platform's order is preserved, not normalised. Re-sorting would replace GitHub's answer with
// this code's for two transitions recorded in the same second (Edge case 11).
func TestTheResultDoesNotDependOnThePlatformsOrdering(t *testing.T) {
	t.Parallel()
	// Two of each state that carries an instant, so that "whichever came last in the list wins" gives a
	// different answer depending on which end the list starts at — one of each would let that bug pass.
	history := []github.DeploymentStatus{
		status(github.StateInProgress, builtAt),
		status(github.StateFailure, completedAt.Add(-5*time.Minute)),
		status(github.StateError, completedAt.Add(-2*time.Minute)),
		status(github.StateSuccess, completedAt),
		status(github.StateSuccess, completedAt.Add(time.Minute)),
		status(github.StateInactive, completedAt.Add(time.Hour)),
		status(github.StateInactive, completedAt.Add(2*time.Hour)),
	}
	oldestFirst := slices.Clone(history)
	newestFirst := slices.Clone(history)
	slices.Reverse(newestFirst)
	a, b := github.ReadStatuses(newestFirst), github.ReadStatuses(oldestFirst)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("reading the same history in the two orders GitHub might return it gave\n  %+v\nand\n  %+v\n"+
			"The instants come from comparison rather than from position, so the answer cannot depend on "+
			"which end the list starts at", a, b)
	}
}

// An unrecognised state is counted under its own spelling and never acted on. A connector that guessed
// would eventually type a new state as a production change on the strength of its name.
func TestAnUnrecognisedStateIsCountedAndNeverARollout(t *testing.T) {
	t.Parallel()
	got := github.ReadStatuses([]github.DeploymentStatus{
		status("rolled_back", completedAt),
		status("rolled_back", completedAt.Add(time.Minute)),
		status("", completedAt),
	})
	switch {
	case got.RolledOut:
		t.Error("a state the table has not ruled on was read as a rollout")
	case got.Unrecognised["rolled_back"] != 2:
		t.Errorf("the unrecognised states are %v, want two under `rolled_back` — a count by spelling, "+
			"because `4 excluded` cannot tell a configured filter from a misread vocabulary (FR-019)",
			got.Unrecognised)
	case got.Unrecognised["unnamed"] != 1:
		t.Errorf("a status with no state at all is %v; it is counted under a name rather than dropped, "+
			"because an exclusion nobody can attribute is what the counter exists to show", got.Unrecognised)
	}
	if d := github.DispositionOf("rolled_back"); d.Outcome != github.OutcomeUnrecognised || d.Terminal {
		t.Errorf("DispositionOf(`rolled_back`) = %+v, want unrecognised and not terminal", d)
	}
}

// An empty history is no rollout and no attempt — a deployment whose statuses have not arrived yet.
func TestAnEmptyHistoryIsNothing(t *testing.T) {
	t.Parallel()
	got := github.ReadStatuses(nil)
	if got.RolledOut || got.Attempted || got.Deactivated || len(got.Unrecognised) != 0 {
		t.Errorf("an empty history read as %+v, want nothing", got)
	}
}

// A failure followed by a success is a rollout that took two goes: the success wins and the attempt is
// still recorded, because both happened.
func TestAFailureFollowedByASuccessIsARolloutThatTookTwoGoes(t *testing.T) {
	t.Parallel()
	failedAt := completedAt.Add(-2 * time.Minute)
	got := github.ReadStatuses([]github.DeploymentStatus{
		status(github.StateSuccess, completedAt),
		status(github.StateFailure, failedAt),
	})
	switch {
	case !got.RolledOut:
		t.Error("an earlier failure cancelled the later success")
	case !got.CompletedAt.Equal(completedAt):
		t.Errorf("CompletedAt = %v, want the success instant %v", got.CompletedAt, completedAt)
	case !got.Attempted || !got.AttemptedAt.Equal(failedAt):
		t.Errorf("the earlier failure was dropped (%+v); both happened and an operator reading the "+
			"window wants both", got)
	}
}

// The published table and the code, compared in both directions (T048).
//
// `docs/connectors/github.md` §6 is what a reader consults to know how this connector reads GitHub's
// status vocabulary. GitHub designates no terminal state, so that page is the only statement of it —
// and a statement that can drift from the process is a statement about a document.
func TestThePublishedStatusTableMatchesTheCode(t *testing.T) {
	t.Parallel()
	const page = "../../../docs/connectors/github.md"
	rows := testkit.ReadPublishedRows(t, page, "What a deployment status means")
	if len(rows) == 0 {
		t.Fatalf("no state was parsed out of %s's table; a comparison against nothing is not a "+
			"comparison", page)
	}

	enforced := map[string]github.StatusDisposition{}
	for _, d := range github.Dispositions() {
		enforced[d.State] = d
	}

	for state, cells := range rows {
		d, ok := enforced[state]
		if !ok {
			t.Errorf("%s publishes the state %q, which the code does not rule on; a reader would take "+
				"the page's outcome for the process's", page, state)
			continue
		}
		if len(cells) < 2 {
			t.Errorf("%s's row for %q has %d columns, want outcome and why", page, state, len(cells))
			continue
		}
		if got, want := cells[0], string(d.Outcome); got != want {
			t.Errorf("%s publishes %q for %q and the code says %q", page, got, state, want)
		}
		if got, want := cells[1], d.Why; got != want {
			t.Errorf("%s's reason for %q is\n  %q\nand the code's is\n  %q", page, state, got, want)
		}
	}
	for state := range enforced {
		if _, ok := rows[state]; !ok {
			t.Errorf("the code rules on %q and %s does not publish it", state, page)
		}
	}
}
