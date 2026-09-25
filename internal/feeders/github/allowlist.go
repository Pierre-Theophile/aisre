// SPDX-License-Identifier: Apache-2.0

package github

import (
	"strings"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The two allowlists, and why an exclusion is counted rather than applied quietly (004 T061, T062;
// FR-019, FR-023, FR-024).
//
// ---------------------------------------------------------------------------------------------
// Why an allowlist and not a heuristic
//
// GitHub carries a `production_environment` flag on a deployment, and it is tempting to read it as the
// answer. It is not: it is whoever created the deployment stating an opinion, it is absent as often as
// it is set, and an estate whose environments are called `prod-eu` and `prod-us` gets no help from it
// at all. So which environments are production is the **operator's** list, and the flag is evidence
// recorded beside it rather than a substitute for it.
//
// The same for workflows. A run that produces no deployment object can still be a rollout — plenty of
// pipelines deploy by calling a cloud API directly — and no signal in the payload says which. FR-024
// makes that a configured list of workflows, because the alternative is treating every green run in
// the repository as a production change.
//
// # Why exclusions are counted, by reason
//
// FR-019, and it is the distinction the whole telemetry design turns on. "340 excluded" cannot tell
// *"the environment filter excluded 340 preview deployments, as configured"* from *"the terminal-state
// filter excluded 340 deployments because we misread the status vocabulary"* — and the second is a
// connector that has gone silent while reporting a healthy number. So every exclusion is attributed to
// the filter that decided it, and the filter names itself.

// The exclusion reasons, published because a checkpoint reports them and an operator reads them.
const (
	// ReasonEnvironmentNotAllowed is a deployment to an environment the operator did not list.
	ReasonEnvironmentNotAllowed = "environment not on the production allowlist"
	// ReasonWorkflowNotAllowed is a run of a workflow the operator did not list as a deploy workflow.
	ReasonWorkflowNotAllowed = "workflow not on the deploy-workflow allowlist"
	// ReasonNoEnvironmentStated is a deployment naming no environment at all. It is its own reason
	// rather than being folded into the one above: a payload that stated nothing and a payload that
	// stated something unlisted are different failures, and only the first is a reason to look at the
	// platform rather than at the configuration.
	ReasonNoEnvironmentStated = "the deployment states no environment"
)

// Allowlist is the operator's statement about which environments and workflows are production.
type Allowlist struct {
	// Environments are the deployment environments that count as production. Empty means **none**,
	// which stops the connector emitting rather than letting it emit everything: a list nobody
	// configured is not a licence to treat every preview deployment as a production change.
	Environments []string
	// Workflows are the workflow names whose runs are rollouts even where no deployment object
	// records them (FR-024). Empty means none, for the same reason.
	Workflows []string
}

// AllowsEnvironment reports whether a deployment's environment is production, and why not when it is
// not. The reason is returned rather than logged so the caller counts it under the filter that decided.
func (a Allowlist) AllowsEnvironment(environment string) (bool, string) {
	if strings.TrimSpace(environment) == "" {
		return false, ReasonNoEnvironmentStated
	}
	if listedFold(a.Environments, environment) {
		return true, ""
	}
	return false, ReasonEnvironmentNotAllowed
}

// AllowsWorkflow reports whether a run's workflow is a deploy workflow.
func (a Allowlist) AllowsWorkflow(workflow string) (bool, string) {
	if listedFold(a.Workflows, workflow) {
		return true, ""
	}
	return false, ReasonWorkflowNotAllowed
}

// Exclusions accumulates what a cycle decided not to emit, by reason.
//
// It is separate from feeder.AreaStats rather than a wrapper of it because the two are recorded at
// different moments — the stats are the cycle's, and this is one repository's pass — and because a
// caller holding both has to be able to hand the counts over at the end rather than as it goes.
type Exclusions struct {
	byReason map[string]int
}

// Exclude records one.
//
// An empty reason is counted under a name rather than dropped, because an exclusion nobody can
// attribute is the exact case the counter exists to make visible.
func (e *Exclusions) Exclude(reason string) {
	if e.byReason == nil {
		e.byReason = map[string]int{}
	}
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" {
		trimmed = "unnamed"
	}
	e.byReason[trimmed]++
}

// ByReason returns the counts, as a copy so a caller cannot edit the accumulator through it.
func (e *Exclusions) ByReason() map[string]int {
	out := make(map[string]int, len(e.byReason))
	for reason, n := range e.byReason {
		out[reason] = n
	}
	return out
}

// Total is every exclusion. It exists for a headline number and is never the whole report: a bare
// count cannot tell a configured filter doing its job from a connector that has gone silent.
func (e *Exclusions) Total() int {
	var n int
	for _, count := range e.byReason {
		n += count
	}
	return n
}

// Publish hands the counts to the cycle's per-area accounting, attributed to the area that decided.
func (e *Exclusions) Publish(stats *feeder.AreaStats, area string) {
	if stats == nil {
		return
	}
	for reason, count := range e.byReason {
		for range count {
			stats.Excluded(area, reason)
		}
	}
}

// listedFold reports membership, case-insensitively and ignoring surrounding space — the two ways a
// hand-maintained configuration file differs from an API payload. Exact otherwise: a prefix match on
// `prod` would admit `prod-experiment`, which is precisely the deployment an operator listing `prod`
// did not mean.
func listedFold(list []string, value string) bool {
	target := strings.ToLower(strings.TrimSpace(value))
	if target == "" {
		return false
	}
	for _, entry := range list {
		if strings.ToLower(strings.TrimSpace(entry)) == target {
			return true
		}
	}
	return false
}
