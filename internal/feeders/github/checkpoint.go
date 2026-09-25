// SPDX-License-Identifier: Apache-2.0

package github

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The checkpoint's note: what was covered, under which grant, and under which filters (004 T043;
// FR-057, FR-008, FR-012, FR-019).
//
// ---------------------------------------------------------------------------------------------
// What this file is for, and the defect that produced it
//
// FR-057 asks a checkpoint to state three things: the extent actually covered, **the filters and
// scope in force**, and whether the feeder was watching immediately before that extent. The first
// and third were carried. The second was not — and not because nobody had thought about it. This
// package's doc.go already said "an operator reading a checkpoint has to know what window was
// covered, **under which grant**", and the `scope` field on the Feeder carried the comment "the
// regime the grant puts the feeder in, **for the checkpoint** (FR-008)". Both described a checkpoint
// that did not exist.
//
// What stopped it was one signature. `feeder.Emitter.Checkpoint` took `(ctx, from, to, gapBefore)`,
// which has nowhere to put a scope — so the field was gathered, documented, and then not passed.
// The GCP feeder hit the same wall from the other side and lost more: it builds a checkpoint stating
// the audit scope, the region scope, the held shifts, the suppressions, the environment mapping and
// a measured lag distribution, validates it, asserts its `Note()` in four test files, and then handed
// three fields to the same method. The Kubernetes feeder hit it first and worked around it by calling
// `feeder.SourceCheckpoint` directly, which fixed its own checkpoint and left the hole for the next
// three connectors.
//
// So the fix is at the seam — `Emitter.Checkpoint` now takes the whole `CheckpointFact` — and this
// file is GitHub's side of it.
//
// # Why the note is a rendered string rather than structured fields
//
// Because `SourceCheckpoint.note` is a published string and this is what a human reads when asking
// "was that window quiet, or was nobody looking?". The rendering is deterministic — every map is
// sorted before it is joined — so two checkpoints over the same state produce the same bytes and a
// golden does not depend on Go's map iteration order.
//
// # What is deliberately NOT in it
//
// No repository names beyond the count, and no deployment identifiers. A checkpoint is operational
// metadata about a poll, it is written on every cycle whether or not anything happened, and a list
// that grew with the estate would put the whole estate's repository names into every checkpoint of
// every recording. The counts answer the question FR-057 asks — how much was in scope — without that.

// Checkpoint is one poll's account of itself.
type Checkpoint struct {
	// From and To bound the window, half-open.
	From, To time.Time
	// Partial says the poll did not cover its whole window, which makes the NEXT checkpoint follow
	// a gap (FR-056).
	Partial bool
	// PartialReason is why, where there is one. A partial poll with no reason is still recorded as
	// partial: the honest answer to "why?" is sometimes "the feeder does not know", and dropping the
	// partial flag because the reason is missing would turn ignorance into a claim of completeness.
	PartialReason string
	// GapBefore says the feeder was not watching immediately before From.
	GapBefore bool
	// Scope is the regime the credential puts the feeder in (FR-008).
	Scope feeder.CredentialScope
	// GateEvidence is how the regime was established, or empty on a replay — where there is no
	// credential to prove, and saying so is better than a silence a reader takes for a passed check.
	GateEvidence string
	// Repositories is how many repositories the grant covered this cycle.
	Repositories int
	// Environments are the deployment environments that count as production. EMPTY IS MEANINGFUL: it
	// stops the connector emitting anything, so a checkpoint over an unconfigured allowlist has to
	// say so or a reader sees a quiet window instead of a connector that was never going to speak.
	Environments []string
	// Workflows are the deploy workflows allowed where no deployment object records the rollout.
	Workflows []string
	// TargetRules is how many repositories have a target mapping. A rollout in a repository with
	// none is emitted unattached (Edge case 1), so the count is what tells a reader whether a cycle
	// of unattached changes was configuration or absence.
	TargetRules int
	// ReorderingWindow is the window this feeder declares it may deliver out of order within.
	ReorderingWindow time.Duration
	// PollInterval is the cadence the window was read at, which is the interval the history it
	// covers is SAMPLED at (FR-052). It is reported beside the reordering window rather than instead
	// of it, because they are different facts that happen to default to the same number — see
	// MapOptions.PollInterval for why conflating them would misreport a widened window as a coarser
	// sampling interval.
	PollInterval time.Duration
	// Excluded is what the cycle decided not to emit, by reason (FR-019).
	Excluded map[string]int
	// Deferred is how many deployments were held across the poll boundary — work not done yet, which
	// FR-073 distinguishes from work decided against.
	Deferred int
	// Released is how many deployments became changes this cycle.
	Released int
	// Skew is the observed clock skew, reported and never used to correct (FR-058).
	Skew feeder.SkewReport
}

// Note renders the checkpoint for a human. Deterministic: every map is sorted before it is joined.
func (c Checkpoint) Note() string {
	parts := []string{"poll=" + c.outcome()}
	if c.PartialReason != "" {
		parts = append(parts, "partial_reason="+c.PartialReason)
	}
	parts = append(parts, "grant="+c.grant())
	if c.GateEvidence != "" {
		parts = append(parts, "gate_evidence="+c.GateEvidence)
	} else {
		// A replay has no credential, and the checkpoint says that rather than leaving a silence a
		// reader would take for a check that passed (FR-003).
		parts = append(parts, "gate_evidence=none (no credential was proved: a replay has none)")
	}
	parts = append(parts, fmt.Sprintf("repositories=%d", c.Repositories))
	parts = append(parts, "environments="+renderList(c.Environments,
		"none configured, so no deployment can be production and nothing will be emitted"))
	parts = append(parts, "deploy_workflows="+renderList(c.Workflows, "none configured"))
	parts = append(parts, fmt.Sprintf("target_rules=%d", c.TargetRules))
	parts = append(parts, "reordering_window="+c.ReorderingWindow.String())
	// FR-052. The extent this checkpoint claims is a POLLED extent, and saying so here is what stops
	// a reader treating the window as continuously observed: a change inside it that this connector
	// never saw, because it began and ended between two polls, is not excluded by the extent.
	parts = append(parts, "sampled=true", "sampled_every="+c.PollInterval.String())
	parts = append(parts, fmt.Sprintf("released=%d", c.Released), fmt.Sprintf("deferred=%d", c.Deferred))
	if len(c.Excluded) > 0 {
		parts = append(parts, "excluded=["+renderCounts(c.Excluded)+"]")
	}
	if c.Skew.Samples > 0 {
		parts = append(parts, fmt.Sprintf("skew_samples=%d", c.Skew.Samples),
			"skew_mean="+c.Skew.Mean.String(), "skew_max="+c.Skew.Max.String())
		if c.Skew.Beyond() {
			parts = append(parts, fmt.Sprintf("skew_beyond_threshold=%d/%d samples over %s",
				c.Skew.Exceeded, c.Skew.Samples, c.Skew.Threshold))
		}
	}
	if c.GapBefore {
		parts = append(parts, "gap_before=true (the silence before this extent was ignorance, not absence)")
	}
	return strings.Join(parts, " ")
}

// outcome is the poll's own word for itself, from the same closed pair the GCP feeder uses so that
// two connectors' checkpoints read the same to one operator.
func (c Checkpoint) outcome() string {
	if c.Partial {
		return "partial"
	}
	return "complete"
}

// grant states the regime, and states whose boundary it is.
//
// The distinction is the whole of FR-008 and it is not cosmetic: GitHub's `repository_selection` is
// the PLATFORM's boundary — the installation cannot read a repository outside it whatever this
// process does — whereas an operator's allowlist is a narrowing of what the credential could reach.
// Flattening the two into one word would let a configuration mistake read as an enforced limit.
func (c Checkpoint) grant() string {
	selection := strings.TrimSpace(c.Scope.Selection)
	if selection == "" {
		selection = "unstated"
	}
	if c.Scope.PlatformEnforced {
		return selection + " (platform-enforced)"
	}
	return selection + " (operator configuration, not a boundary the platform holds)"
}

// renderList sorts and joins, or says what an empty list means. An empty list is never rendered as
// `[]`: the reason it is empty is the thing a reader needs.
func renderList(values []string, whenEmpty string) string {
	if len(values) == 0 {
		return "[] (" + whenEmpty + ")"
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return "[" + strings.Join(sorted, ",") + "]"
}

// renderCounts renders a reason tally, sorted by reason.
func renderCounts(counts map[string]int) string {
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	pairs := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		pairs = append(pairs, fmt.Sprintf("%s=%d", reason, counts[reason]))
	}
	return strings.Join(pairs, ",")
}
