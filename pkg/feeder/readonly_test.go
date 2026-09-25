// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The published read-only operation surface (004 FR-004, SC-007).
//
// FR-004 asks for something stronger than "this run did not write": write-incapability must be
// verifiable from the **set of operations the connector can issue**. These tests are that
// verification, and the property they lean on is that a REST operation carries its own answer — the
// method is the write test, so there is no vendor naming convention to guess at.

func surface(t *testing.T, ops map[feeder.ReadOperation]feeder.ReadOperationSpec) *feeder.ReadOnlySurface {
	t.Helper()
	s, err := feeder.NewReadOnlySurface("testhub", ops)
	if err != nil {
		t.Fatalf("NewReadOnlySurface: %v", err)
	}
	return s
}

func reads() map[feeder.ReadOperation]feeder.ReadOperationSpec {
	return map[feeder.ReadOperation]feeder.ReadOperationSpec{
		"GET /repos/{owner}/{repo}/deployments": {Area: "deployments", Why: "a deployment is a rollout"},
		"HEAD /rate_limit":                      {Area: "budget", Why: "the remaining quota"},
	}
}

// A surface cannot be built around an operation that can write. This is the assertion the whole
// design exists for, and it fires at construction rather than on the call.
func TestASurfaceRefusesEveryMethodThatCanChangeState(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "post", "Delete"} {
		ops := reads()
		ops[feeder.ReadOperation(method+" /repos/{owner}/{repo}/deployments")] =
			feeder.ReadOperationSpec{Area: "deployments", Why: "create one"}
		if _, err := feeder.NewReadOnlySurface("testhub", ops); err == nil {
			t.Errorf("a surface was built containing %s; constitution VII permits no write to any "+
				"production system, and FR-004 requires that be verifiable from the operation set", method)
		}
	}
}

// And it panics for a package-level var, so a binary whose published surface contains a write cannot
// start — rather than failing in a test somebody can forget to run.
func TestMustPanicsRatherThanShippingAWritableSurface(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("MustReadOnlySurface returned for a surface containing a write; a binary whose " +
				"published surface can write should not be buildable")
		}
	}()
	ops := reads()
	ops["DELETE /repos/{owner}/{repo}/deployments/{id}"] = feeder.ReadOperationSpec{Area: "x", Why: "y"}
	_ = feeder.MustReadOnlySurface("testhub", ops)
}

// An operation that is not spelled `METHOD path` is refused rather than assumed to be a read, and
// the refusal says which defect it has.
//
// The message matters here, not only that an error came back: every malformed spelling below would
// ALSO be caught by the method test downstream — splitOperation returns an empty method on failure,
// and an empty method is not a read. So an assertion that only checked `err != nil` would pass with
// the spelling check deleted, and would be reporting a missing method for an operation that has one.
// An operator reading the refusal needs the defect it actually has.
func TestAnOperationThatIsNotMethodAndPathIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op   feeder.ReadOperation
		want string
	}{
		{"/repos/{owner}/{repo}/deployments", "is not `METHOD path`"}, // path only
		{"GET", "is not `METHOD path`"},                               // method only
		{"GET  /two /spaces", "is not `METHOD path`"},                 // ambiguous
		{"", "is not `METHOD path`"},                                  // nothing
		// Well-formed as `METHOD path`, so its defect is the path and the refusal must say so.
		{"GET repos/{owner}/{repo}", "does not begin with `/`"},
	} {
		ops := reads()
		ops[tc.op] = feeder.ReadOperationSpec{Area: "x", Why: "y"}
		_, err := feeder.NewReadOnlySurface("testhub", ops)
		if err == nil {
			t.Errorf("a surface admitted the malformed operation %q", tc.op)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("refusing %q said %q, which does not report %q", tc.op, err, tc.want)
		}
	}
}

// An unpublished operation is refused at the call, before any quota is spent.
func TestAnUnpublishedOperationIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	t.Parallel()
	s := surface(t, reads())
	_, err := s.Issuable("GET /repos/{owner}/{repo}/contents/{path}")
	var unpublished *feeder.UnpublishedOperationError
	if !errors.As(err, &unpublished) {
		t.Fatalf("Issuable on an unpublished read returned %v, want UnpublishedOperationError", err)
	}
	if !strings.Contains(err.Error(), "testhub") {
		t.Errorf("the refusal does not name the platform whose operation was refused: %q", err)
	}
	// And a published one is admitted, so the refusal above is not simply "everything is refused".
	if _, err := s.Issuable("GET /repos/{owner}/{repo}/deployments"); err != nil {
		t.Errorf("a published read was refused: %v", err)
	}
}

// Every published operation must justify itself. An operation nobody can explain is one to remove,
// and the published page is where an operator decides whether to grant it.
func TestAnOperationWithNoAreaOrNoReasonIsRefused(t *testing.T) {
	t.Parallel()
	for name, spec := range map[string]feeder.ReadOperationSpec{
		"no area":   {Why: "because"},
		"no reason": {Area: "deployments"},
		"neither":   {},
		"blank":     {Area: "   ", Why: "\t"},
	} {
		ops := reads()
		ops["GET /repos/{owner}/{repo}/releases"] = spec
		if _, err := feeder.NewReadOnlySurface("testhub", ops); err == nil {
			t.Errorf("a surface admitted an operation with %s", name)
		}
	}
}

// A surface over nothing is refused: a gate that holds over an empty set would refuse every call
// while reporting itself as satisfied.
func TestAnEmptySurfaceIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := feeder.NewReadOnlySurface("testhub", nil); err == nil {
		t.Error("an empty surface was admitted; it would refuse every call and hold over nothing")
	}
	if _, err := feeder.NewReadOnlySurface("", reads()); err == nil {
		t.Error("a surface with no platform name was admitted; its refusals would not say whose")
	}
}

// The state-change count is the cheapest form SC-007 takes, and it lives here so no connector can
// define it away.
func TestAReadOnlySurfaceChangesNoState(t *testing.T) {
	t.Parallel()
	if got := surface(t, reads()).StateChanges(); got != 0 {
		t.Errorf("StateChanges() = %d on a surface of reads, want 0", got)
	}
}

// Mutating the caller's map after construction does not widen the surface.
func TestTheSurfaceDoesNotAliasTheCallersMap(t *testing.T) {
	t.Parallel()
	ops := reads()
	s := surface(t, ops)
	ops["DELETE /everything"] = feeder.ReadOperationSpec{Area: "x", Why: "y"}
	if _, err := s.Issuable("DELETE /everything"); err == nil {
		t.Error("an operation added to the caller's map after construction became issuable")
	}
	if got := s.StateChanges(); got != 0 {
		t.Errorf("StateChanges() = %d after the caller's map was mutated, want 0", got)
	}
}

// Operations() is sorted, because the published page, the usage report and the gate are generated
// from it and a golden that reorders is a golden that churns.
func TestOperationsIsSortedSoThePageAndTheGateAgree(t *testing.T) {
	t.Parallel()
	got := surface(t, reads()).Operations()
	if len(got) != 2 {
		t.Fatalf("Operations() returned %d, want 2", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("Operations() is not sorted: %q before %q", got[i-1], got[i])
		}
	}
}
