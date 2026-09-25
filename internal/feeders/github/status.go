// SPDX-License-Identifier: Apache-2.0

package github

import (
	"slices"
	"strings"
	"time"
)

// What a deployment status means (004 T048–T053, FR-012, FR-022, SC-003; research §2).
//
// ---------------------------------------------------------------------------------------------
// Why this table is published rather than inferred
//
// GitHub's deployment API designates **no terminal state**. It documents seven values a status may
// take and says nothing about which of them end a deployment, so any connector reading them is
// deciding — and a decision made implicitly, scattered across call sites, is one nobody can review.
// Research §2 records that finding; this file is the decision, in one table, with the reason beside
// each row.
//
// The seven states and what each one is taken to mean:
//
//	success      → a ROLLOUT. Something is running in production that was not running before.
//	failure      → a failed attempt. Nothing rolled out, and something tried.
//	error        → a failed attempt. GitHub distinguishes it from `failure` by cause, not by outcome.
//	inactive     → a PROPERTY of the rollout that already happened, never a rollout of its own.
//	queued       → nothing. Nothing in production has moved.
//	in_progress  → nothing. Same.
//	pending      → nothing. Same.
//
// # Why `failure` and `error` are not one thing, and not a rollout either
//
// FR-022 and T050: a failed attempt is a fact worth recording — an operator asking "what changed
// around 14:03" wants to know that a deploy was attempted and did not land. But it is not a ROLLOUT,
// because nothing in production changed, and a taxonomy that made it one would put a change node on
// the graph for an event that changed nothing. The two states are kept apart because GitHub
// distinguishes them and re-labelling somebody else's vocabulary loses information for no gain.
//
// # Why `inactive` is a property and not a second change
//
// GitHub marks a deployment `inactive` when a newer one supersedes it. Read as a rollout it would
// double every deploy: one change when it went live, another when the next one replaced it — and the
// second would be dated at the moment the FIRST stopped serving, which is a fact about the first. So
// it is recorded against the same change (FR-022), which is also why it cannot be handled by a
// state-to-kind map alone: it needs the history it belongs to.
//
// # Why an unrecognised state is never a rollout
//
// The list above is GitHub's as documented, and a value outside it is a value nobody has ruled on. A
// connector that guessed would eventually type a new state as a production change on the strength of
// its spelling. So it is counted, named, and produces nothing — the same fail-closed default the
// read-only permission check uses.

// GitHub's seven documented deployment status states. Spelled `State…` rather than `Status…` so that
// nothing here is mistaken for StatusError, which is an HTTP refusal in transport.go.
const (
	StateSuccess    = "success"
	StateFailure    = "failure"
	StateError      = "error"
	StateInactive   = "inactive"
	StateQueued     = "queued"
	StateInProgress = "in_progress"
	StatePending    = "pending"
)

// StatusOutcome is what one status is taken to mean.
type StatusOutcome string

const (
	// OutcomeRollout is the one outcome that becomes a ROLLOUT change.
	OutcomeRollout StatusOutcome = "rollout"
	// OutcomeFailedAttempt is a deploy that was attempted and did not land.
	OutcomeFailedAttempt StatusOutcome = "failed_attempt"
	// OutcomeNothing is a status that reports progress rather than an outcome.
	OutcomeNothing StatusOutcome = "nothing"
	// OutcomeProperty is a status that says something about a rollout that already happened.
	OutcomeProperty StatusOutcome = "property"
	// OutcomeUnrecognised is a state this table has not ruled on. Never a rollout.
	OutcomeUnrecognised StatusOutcome = "unrecognised"
)

// StatusDisposition is one row of the published table.
type StatusDisposition struct {
	State   string
	Outcome StatusOutcome
	// Terminal records whether this state ends the deployment's progress. It is this connector's
	// designation, not GitHub's — GitHub designates none — which is why it is published rather than
	// assumed.
	Terminal bool
	Why      string
}

var dispositions = map[string]StatusDisposition{
	StateSuccess: {
		State: StateSuccess, Outcome: OutcomeRollout, Terminal: true,
		Why: "something is running in production that was not running before",
	},
	StateFailure: {
		State: StateFailure, Outcome: OutcomeFailedAttempt, Terminal: true,
		Why: "a deploy was attempted and did not land; nothing in production moved",
	},
	StateError: {
		State: StateError, Outcome: OutcomeFailedAttempt, Terminal: true,
		Why: "as `failure`; GitHub distinguishes the two by cause rather than by outcome",
	},
	StateInactive: {
		State: StateInactive, Outcome: OutcomeProperty, Terminal: false,
		Why: "a newer deployment superseded this one, which is a fact about the rollout that already " +
			"happened rather than a rollout of its own (FR-022)",
	},
	StateQueued: {
		State: StateQueued, Outcome: OutcomeNothing, Terminal: false,
		Why: "nothing in production has moved",
	},
	StateInProgress: {
		State: StateInProgress, Outcome: OutcomeNothing, Terminal: false,
		Why: "nothing in production has moved yet",
	},
	StatePending: {
		State: StatePending, Outcome: OutcomeNothing, Terminal: false,
		Why: "nothing in production has moved",
	},
}

// DispositionOf reads one state. An unrecognised state comes back named, so it can be counted under
// its own spelling rather than under a heading somebody invented.
func DispositionOf(state string) StatusDisposition {
	trimmed := strings.TrimSpace(state)
	if d, ok := dispositions[trimmed]; ok {
		return d
	}
	return StatusDisposition{
		State: trimmed, Outcome: OutcomeUnrecognised, Terminal: false,
		Why: "a state this connector's published table has not ruled on; counted and not acted on, " +
			"because a state typed as a production change on the strength of its spelling is a guess",
	}
}

// Dispositions returns the published table, sorted by state, so the connector page and the code are
// generated from one source.
func Dispositions() []StatusDisposition {
	states := make([]string, 0, len(dispositions))
	for state := range dispositions {
		states = append(states, state)
	}
	slices.Sort(states)
	out := make([]StatusDisposition, 0, len(states))
	for _, state := range states {
		out = append(out, dispositions[state])
	}
	return out
}

// Rollout is one deployment's whole status history, read.
type Rollout struct {
	// RolledOut is true where the platform stated a success.
	RolledOut bool
	// CompletedAt is the platform's **completion instant**: the moment the success status was
	// recorded. FR-012 and SC-003 both turn on it not being anything else — not the run's start, not
	// the build instant, not the instant this connector polled.
	CompletedAt time.Time
	// CompletionStated is false where the platform reported a success with no instant. The valid start
	// is then unknown and must be recorded as unknown rather than guessed (T053): filling it with the
	// poll instant would date a rollout to when we noticed it.
	CompletionStated bool

	// Deactivated records a later `inactive`, as a property of this same rollout (FR-022).
	Deactivated   bool
	DeactivatedAt time.Time

	// Attempted records a `failure` or an `error`. It is a fact worth having — a deploy was tried and
	// did not land — and it is never a ROLLOUT.
	Attempted   bool
	AttemptedAt time.Time
	// AttemptState is which of the two the platform said, kept rather than flattened.
	AttemptState string

	// Successes is how many success statuses the history carried. More than one is an ambiguity this
	// records rather than resolves: FR-028 keys a re-run as a NEW change, so two successes against one
	// deployment id is something to surface, not to average.
	Successes int

	// Unrecognised is the states the table has not ruled on, by spelling and count. It is a map so a
	// checkpoint can report "4 × mannequin" rather than "4 excluded", which is the distinction FR-019
	// exists for.
	Unrecognised map[string]int
}

// ReadStatuses reads a deployment's statuses as GitHub returned them.
//
// The order is **not** normalised. GitHub returns statuses newest first, Edge case 11 turns on the
// platform's own stated ordering, and re-sorting by timestamp would silently break the tie between two
// transitions recorded in the same second — replacing the platform's answer with this code's.
//
// So the instants come from comparison rather than from position: the rollout's completion is the
// EARLIEST success, because that is when it first went live, and a deactivation is the LATEST
// `inactive`, because that is the state it ended in.
func ReadStatuses(statuses []DeploymentStatus) Rollout {
	out := Rollout{}
	for _, status := range statuses {
		d := DispositionOf(status.State)
		switch d.Outcome {
		case OutcomeRollout:
			out.Successes++
			out.RolledOut = true
			if status.CreatedAt.IsZero() {
				continue
			}
			if !out.CompletionStated || status.CreatedAt.Before(out.CompletedAt) {
				out.CompletedAt = status.CreatedAt
				out.CompletionStated = true
			}
		case OutcomeProperty:
			out.Deactivated = true
			if status.CreatedAt.After(out.DeactivatedAt) {
				out.DeactivatedAt = status.CreatedAt
			}
		case OutcomeFailedAttempt:
			out.Attempted = true
			// The latest attempt, and its own state: `failure` and `error` are GitHub's distinction and
			// flattening them would lose which.
			if status.CreatedAt.After(out.AttemptedAt) || out.AttemptState == "" {
				out.AttemptedAt = status.CreatedAt
				out.AttemptState = d.State
			}
		case OutcomeUnrecognised:
			if out.Unrecognised == nil {
				out.Unrecognised = map[string]int{}
			}
			name := d.State
			if name == "" {
				// A status with no state at all. Counted under a name rather than dropped, because an
				// exclusion nobody can attribute is the exact case the counter exists to make visible.
				name = "unnamed"
			}
			out.Unrecognised[name]++
		case OutcomeNothing:
			// Progress, not an outcome. Nothing in production has moved.
		}
	}
	return out
}

// DeactivatedWithoutRollingOut reports a deployment marked `inactive` that never succeeded.
//
// It is called out because it is the case where `inactive` has nothing to be a property OF, and
// attaching it to a rollout that was never stated would invent the rollout. Such a deployment yields
// no change, and the caller counts it rather than passing over it.
func (r Rollout) DeactivatedWithoutRollingOut() bool {
	return r.Deactivated && !r.RolledOut
}

// ValidStartUnknown reports a rollout whose start instant the platform did not state (T053).
func (r Rollout) ValidStartUnknown() bool {
	return r.RolledOut && !r.CompletionStated
}

// Ambiguous reports a history carrying more than one success against one deployment id.
func (r Rollout) Ambiguous() bool { return r.Successes > 1 }
