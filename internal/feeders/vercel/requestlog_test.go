// SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"errors"
	"strings"
	"testing"

	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
)

// The published read-only operation surface (004 FR-004, SC-007; T006, T008).

// Every published operation is a read, and the state-change count is zero.
func TestEveryPublishedOperationIsAReadAndNothingChangesState(t *testing.T) {
	t.Parallel()

	ops := vercelfeeder.Surface.Operations()
	if len(ops) == 0 {
		t.Fatal("the published surface is empty, so every assertion below would hold over nothing")
	}
	for _, op := range ops {
		method, path, ok := strings.Cut(string(op), " ")
		if !ok {
			t.Errorf("%q is not `METHOD path`", op)
			continue
		}
		if method != "GET" && method != "HEAD" {
			t.Errorf("%q has method %s; docs/connectors/vercel.md §2 claims every operation is a read, "+
				"and constitution VII permits no write to any production system", op, method)
		}
		// Vercel versions its API in the path, and the page states that the version is part of an
		// operation's identity. An unversioned path would make the surface silently follow whatever
		// the platform defaults to.
		if !strings.HasPrefix(path, "/v") {
			t.Errorf("%q has no API version in its path; the version is part of the published "+
				"operation, so an unversioned call would drift with the platform", op)
		}
	}
	if got := vercelfeeder.Surface.StateChanges(); got != 0 {
		t.Errorf("StateChanges() = %d; nothing here is promoted, rolled back, redeployed or deleted, so "+
			"the count is zero with no declared exception", got)
	}
}

// And a planted write is refused, so the assertion above is a gate rather than an observation about
// today's table.
func TestAPlantedWriteCannotJoinTheSurface(t *testing.T) {
	t.Parallel()

	for _, write := range []feeder.ReadOperation{
		"POST /v13/deployments",
		"PATCH /v9/projects/{idOrName}",
		"DELETE /v13/deployments/{idOrUrl}",
		"POST /v10/projects/{idOrName}/promote/{deploymentId}",
		"POST /v10/projects/{idOrName}/rollback/{deploymentId}",
	} {
		ops := map[feeder.ReadOperation]feeder.ReadOperationSpec{}
		for _, op := range vercelfeeder.Surface.Operations() {
			spec, _ := vercelfeeder.Surface.SpecOf(op)
			ops[op] = spec
		}
		ops[write] = feeder.ReadOperationSpec{Area: "deployments", Why: "planted by a test"}
		if _, err := feeder.NewReadOnlySurface(vercelfeeder.Platform, ops); err == nil {
			t.Errorf("a surface was built carrying %q; the write gate is not holding", write)
		}
	}
}

// An unpublished operation is refused at the call, before any quota is spent.
func TestAnUnpublishedOperationIsRefusedBeforeAnyQuotaIsSpent(t *testing.T) {
	t.Parallel()

	for _, op := range []feeder.ReadOperation{
		// The decrypted value of an environment variable: FR-038's never, and the closest thing on
		// this surface to something a reader might assume is included.
		"GET /v1/projects/{idOrName}/env/{id}",
		"GET /v2/deployments/{idOrUrl}/events", // build logs
		"GET /v6/deployments/{id}/files",       // what was built, not that it was
		"GET /v2/teams/{teamId}/members",       // a different subject
		"GET /v1/access-groups",                // member access, not what a token may read
	} {
		_, err := vercelfeeder.Issuable(op)
		var unpublished *feeder.UnpublishedOperationError
		if !errors.As(err, &unpublished) {
			t.Errorf("Issuable(%q) returned %v, want UnpublishedOperationError; docs/connectors/vercel.md "+
				"§3 says this is never read", op, err)
			continue
		}
		if !strings.Contains(err.Error(), vercelfeeder.Platform) {
			t.Errorf("the refusal of %q does not name the platform: %q", op, err)
		}
	}

	if _, err := vercelfeeder.Issuable(vercelfeeder.OpDeployments); err != nil {
		t.Errorf("a published read was refused: %v", err)
	}
}

// The page an operator approves and the surface the process enforces are one list.
func TestThePublishedPageAndTheEnforcedSurfaceAreTheSameList(t *testing.T) {
	t.Parallel()
	testkit.AssertSurfaceMatchesPage(t, vercelfeeder.Surface, "../../../docs/connectors/vercel.md")
}
