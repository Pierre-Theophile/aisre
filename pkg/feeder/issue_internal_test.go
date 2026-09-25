// SPDX-License-Identifier: Apache-2.0

package feeder

import "testing"

// StateChanges over a cycle's log, from inside the package (004 SC-007).
//
// This one test is internal, and the reason is the point: the surface constructor refuses to admit a
// write, so no caller outside this package can put an issued write into the log — which means
// StateChanges() returning a constant zero would satisfy every external test. Planting the row is the
// only way the count is falsifiable, and a count nobody can falsify is the kind of machinery this
// project keeps finding declared and never exercised.
func TestStateChangesCountsAnIssuedWriteAndNotABlockedOne(t *testing.T) {
	t.Parallel()
	issuer, err := NewIssuer(MustReadOnlySurface("testhub", map[ReadOperation]ReadOperationSpec{
		"GET /deployments": {Area: "deployments", Why: "a deployment is a rollout"},
	}), nil, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	// Reachable only by mutating the surface map after construction, which the constructor's copy is
	// meant to prevent — so this is the defence-in-depth case, asserted rather than assumed.
	issuer.log["DELETE /deployments/{id}"] = &RequestRecord{
		Operation: "DELETE /deployments/{id}", Issued: 1,
	}
	if got := issuer.StateChanges(); got != 1 {
		t.Fatalf("StateChanges() = %d with one issued write in the log, want 1; a count that cannot "+
			"reach anything but zero is not evidence of anything", got)
	}

	// An attempt the surface blocked is not a state change: nothing reached the platform. It belongs
	// in the log — that is what makes FR-004 falsifiable — but not in this count.
	issuer.log["POST /deployments"] = &RequestRecord{Operation: "POST /deployments", Blocked: 1}
	if got := issuer.StateChanges(); got != 1 {
		t.Errorf("StateChanges() = %d once a BLOCKED write is in the log, want 1; an attempt that "+
			"never went out changed no state", got)
	}

	// And an issued read still does not count, so the two assertions above are not "everything counts".
	issuer.log["GET /deployments"] = &RequestRecord{Operation: "GET /deployments", Issued: 3}
	if got := issuer.StateChanges(); got != 1 {
		t.Errorf("StateChanges() = %d with three issued reads added, want 1", got)
	}
}
