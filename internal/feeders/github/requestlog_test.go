// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"errors"
	"strings"
	"testing"

	githubfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
)

// The published read-only operation surface (004 FR-004, SC-007; T005, T007).
//
// FR-004 asks for write-incapability verifiable from **the set of operations the feeder can issue**,
// not from one run's behaviour. These are that verification for GitHub: the surface holds no write,
// changes no state, refuses what it does not publish, and is the same list as the page an operator
// approves.

// Every published operation is a read, and the state-change count is zero.
//
// Zero rather than 003's "exactly one" is the difference this connector's contract turns on: nothing
// is acknowledged here, so there is no exception to name (contracts/read-only-operations.md §2).
func TestEveryPublishedOperationIsAReadAndNothingChangesState(t *testing.T) {
	t.Parallel()

	ops := githubfeeder.Surface.Operations()
	if len(ops) == 0 {
		t.Fatal("the published surface is empty, so every assertion below would hold over nothing")
	}
	for _, op := range ops {
		method, _, ok := strings.Cut(string(op), " ")
		if !ok {
			t.Errorf("%q is not `METHOD path`", op)
			continue
		}
		if method != "GET" && method != "HEAD" {
			t.Errorf("%q has method %s; docs/connectors/github.md §2 claims every operation is a read, "+
				"and constitution VII permits no write to any production system", op, method)
		}
	}
	if got := githubfeeder.Surface.StateChanges(); got != 0 {
		t.Errorf("StateChanges() = %d; this connector acknowledges nothing and dispatches nothing, so "+
			"the count is zero with no declared exception", got)
	}
}

// And a planted write is refused, so the assertion above is a gate rather than an observation about
// today's table. This is the probe the test above would otherwise pass vacuously.
func TestAPlantedWriteCannotJoinTheSurface(t *testing.T) {
	t.Parallel()

	for _, write := range []feeder.ReadOperation{
		"POST /repos/{owner}/{repo}/deployments",
		"DELETE /repos/{owner}/{repo}/deployments/{deployment_id}",
		"POST /repos/{owner}/{repo}/actions/runs/{run_id}/rerun",
		"PATCH /repos/{owner}/{repo}/releases/{release_id}",
	} {
		ops := map[feeder.ReadOperation]feeder.ReadOperationSpec{}
		for _, op := range githubfeeder.Surface.Operations() {
			spec, _ := githubfeeder.Surface.SpecOf(op)
			ops[op] = spec
		}
		ops[write] = feeder.ReadOperationSpec{Area: "deployments", Why: "planted by a test"}
		if _, err := feeder.NewReadOnlySurface(githubfeeder.Platform, ops); err == nil {
			t.Errorf("a surface was built carrying %q; the write gate is not holding", write)
		}
	}
}

// An unpublished operation is refused at the call, before any quota is spent — and the refusal names
// the platform, because a shared gate that did not would leave an operator guessing which connector
// stopped.
func TestAnUnpublishedOperationIsRefusedBeforeAnyQuotaIsSpent(t *testing.T) {
	t.Parallel()

	for _, op := range []feeder.ReadOperation{
		"GET /repos/{owner}/{repo}/actions/secrets",            // never, in any form
		"GET /repos/{owner}/{repo}/actions/runs/{run_id}/logs", // the application's own output
		"GET /repos/{owner}/{repo}/contents/{path}",            // the code, not the rollout
		"GET /repos/{owner}/{repo}/issues",                     // a different subject
		"GET /orgs/{org}/members",                              // a different subject
	} {
		_, err := githubfeeder.Issuable(op)
		var unpublished *feeder.UnpublishedOperationError
		if !errors.As(err, &unpublished) {
			t.Errorf("Issuable(%q) returned %v, want UnpublishedOperationError; docs/connectors/github.md "+
				"§3 says this is never read", op, err)
			continue
		}
		if !strings.Contains(err.Error(), githubfeeder.Platform) {
			t.Errorf("the refusal of %q does not name the platform: %q", op, err)
		}
	}

	// And a published one is admitted, so the refusals above are not simply "everything is refused".
	if _, err := githubfeeder.Issuable(githubfeeder.OpDeployments); err != nil {
		t.Errorf("a published read was refused: %v", err)
	}
}

// The page an operator approves and the surface the process enforces are one list.
func TestThePublishedPageAndTheEnforcedSurfaceAreTheSameList(t *testing.T) {
	t.Parallel()
	testkit.AssertSurfaceMatchesPage(t, githubfeeder.Surface, "../../../docs/connectors/github.md")
}
