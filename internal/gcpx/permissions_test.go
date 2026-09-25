// SPDX-License-Identifier: Apache-2.0

package gcpx_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/gcpx"
)

// The read-only gate (FR-004, FR-112, SC-020; research §8).
//
// The property these protect is the one the whole integration rests on: a principal that can write
// production does not get to start. Everything else in this feature is a promise about behaviour;
// this is a property of the credential, which is the only kind of promise a connector can actually
// keep.

// fakeTester answers testIamPermissions from a fixed set, and records what it was asked.
type fakeTester struct {
	held      map[string]bool
	asked     [][]string
	failOver  int // return INVALID_ARGUMENT when a chunk is larger than this; 0 disables
	failWith  error
	callCount int
}

func (f *fakeTester) TestPermissions(_ context.Context, _ string, permissions []string) ([]string, error) {
	f.callCount++
	if f.failWith != nil {
		return nil, f.failWith
	}
	if f.failOver > 0 && len(permissions) > f.failOver {
		return nil, errors.New("googleapi: Error 400: Permissions field exceeds limit, INVALID_ARGUMENT")
	}
	f.asked = append(f.asked, append([]string(nil), permissions...))
	var out []string
	for _, p := range permissions {
		if f.held[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

func TestAPrincipalHoldingEditorIsRefusedAndTheOffendingPermissionsAreNamed(t *testing.T) {
	t.Parallel()
	// The common catastrophe this gate exists for: somebody bound roles/editor to the connector's
	// service account at project level.
	tester := &fakeTester{held: map[string]bool{
		"run.services.update":       true,
		"cloudsql.instances.delete": true,
		"logging.logMetrics.create": true,
	}}

	held, _, err := gcpx.TestWritePermissions(context.Background(), tester, "proj", gcpx.WritePermissions(), 0)
	if err != nil {
		t.Fatalf("test permissions: %v", err)
	}
	if len(held) != 3 {
		t.Fatalf("held = %v, want the three planted write permissions", held)
	}
	// Naming them is the requirement, not just refusing (FR-004).
	for _, want := range []string{"run.services.update", "cloudsql.instances.delete", "logging.logMetrics.create"} {
		if !contains(held, want) {
			t.Errorf("held = %v, missing %q; the refusal must name what was found", held, want)
		}
	}
}

func TestAReadOnlyPrincipalPasses(t *testing.T) {
	t.Parallel()
	tester := &fakeTester{held: map[string]bool{}}
	held, _, err := gcpx.TestWritePermissions(context.Background(), tester, "proj", gcpx.WritePermissions(), 0)
	if err != nil {
		t.Fatalf("test permissions: %v", err)
	}
	if len(held) != 0 {
		t.Errorf("a read-only principal was reported as holding %v", held)
	}
}

func TestImpersonationCountsAsWrite(t *testing.T) {
	t.Parallel()
	// A principal that can impersonate is not read-only however clean the rest looks: it can mint
	// a token for something that writes. This is the one people forget (research §8).
	for _, perm := range []string{
		"iam.serviceAccounts.getAccessToken",
		"iam.serviceAccounts.signJwt",
		"iam.serviceAccounts.signBlob",
		"iam.serviceAccounts.implicitDelegation",
	} {
		if !contains(gcpx.WritePermissions(), perm) {
			t.Errorf("%s is not in the tested set; a principal that can impersonate would pass the gate", perm)
		}
		tester := &fakeTester{held: map[string]bool{perm: true}}
		held, _, err := gcpx.TestWritePermissions(context.Background(), tester, "proj", gcpx.WritePermissions(), 0)
		if err != nil {
			t.Fatalf("test permissions: %v", err)
		}
		if len(held) != 1 {
			t.Errorf("%s: held = %v, want it reported", perm, held)
		}
	}
}

func TestChunkingHalvesOnInvalidArgumentRatherThanAssertingALimit(t *testing.T) {
	t.Parallel()
	// The per-call permission limit is NOT documented (research §8). So the implementation starts
	// at a tunable size and halves on INVALID_ARGUMENT, converging on whatever the real limit is
	// without ever claiming to know it.
	tester := &fakeTester{held: map[string]bool{}, failOver: 25}

	_, settled, err := gcpx.TestWritePermissions(context.Background(), tester, "proj", gcpx.WritePermissions(), 100)
	if err != nil {
		t.Fatalf("chunking did not recover from INVALID_ARGUMENT: %v", err)
	}
	if settled > 25 {
		t.Errorf("settled on chunk size %d, but the server refused anything over 25", settled)
	}
	// And it asked about everything, not just the first chunk that happened to fit.
	var askedCount int
	for _, chunk := range tester.asked {
		askedCount += len(chunk)
	}
	if askedCount != len(gcpx.WritePermissions()) {
		t.Errorf("asked about %d permissions, want all %d; halving must not drop the remainder",
			askedCount, len(gcpx.WritePermissions()))
	}
}

func TestChunkingGivesUpRatherThanHalvingForever(t *testing.T) {
	t.Parallel()
	// Below the floor the API is refusing for some reason other than size, and continuing would
	// turn one bad response into twenty.
	tester := &fakeTester{held: map[string]bool{}, failOver: 1}
	_, _, err := gcpx.TestWritePermissions(context.Background(), tester, "proj", gcpx.WritePermissions(), 100)
	if err == nil {
		t.Error("halved past the floor instead of reporting that the call fails for another reason")
	}
}

// TestTheClassifierDoesNotFalsePositiveOnTheSixNamedPermissions is research §8.2's list.
//
// A substring rule over create|update|delete|write|set|insert misfires on every one of these, which
// is why the classifier is an explicit allowlist over the extracted VERB.
func TestTheClassifierDoesNotFalsePositiveOnTheSixNamedPermissions(t *testing.T) {
	t.Parallel()
	reads := []string{
		"compute.urlMaps.validate",
		"serviceusage.values.test",
		"pubsub.schemas.validate",
		"cloudsql.instances.preCheckMajorVersionUpgrade",
		"compute.instances.listEffectiveTags",
		"compute.instances.troubleshoot",
	}
	for _, perm := range reads {
		if gcpx.Classify(perm) != gcpx.ClassRead {
			t.Errorf("%s classified %v, want read — a substring rule is what gets this wrong",
				perm, gcpx.Classify(perm))
		}
		if gcpx.IsWrite(perm) {
			t.Errorf("%s treated as a write; the gate would refuse a clean credential", perm)
		}
	}
}

func TestTheClassifierFailsClosedOnAnUnknownVerb(t *testing.T) {
	t.Parallel()
	// The two failure modes are not symmetric. Calling a write a read lets a credential that can
	// write production through; calling a read a write refuses to start and is fixed in a minute.
	for _, perm := range []string{
		"someservice.things.frobnicate",
		"newapi.resources.materialise",
		"weird.thing.ZZZ",
	} {
		if gcpx.Classify(perm) != gcpx.ClassUnknown {
			t.Errorf("%s classified %v, want unknown", perm, gcpx.Classify(perm))
		}
		if !gcpx.IsWrite(perm) {
			t.Errorf("%s: an unrecognised verb was treated as a read. The gate must fail CLOSED", perm)
		}
	}
}

func TestExportAndReportAreWritesForThisIntegration(t *testing.T) {
	t.Parallel()
	// Both are deliberate narrowings, and both are why a predefined role disqualifies itself.
	// cloudsql.instances.export writes a dump to a bucket — data egress with a side effect — and
	// is in roles/cloudsql.viewer. trafficdirector.networks.reportMetrics writes metrics, and is
	// in roles/compute.networkViewer but not roles/compute.viewer (research §8.2).
	for _, perm := range []string{
		"cloudsql.instances.export",
		"cloudsql.backupRuns.export",
		"trafficdirector.networks.reportMetrics",
	} {
		// ClassWrite specifically, not merely IsWrite. An earlier version of this test asserted
		// only IsWrite and passed when `export` was removed from the denylist entirely: the verb
		// then falls through to unrecognised, and fail-closed reports it as a write anyway. That
		// is the right SAFETY outcome and the wrong thing to have a test assert, because the
		// denylist entry is where the reason lives — these are deliberate narrowings of a verb
		// that reads, not verbs nobody has gotten round to classifying.
		if got := gcpx.Classify(perm); got != gcpx.ClassWrite {
			t.Errorf("%s classified %v, want ClassWrite. It is exactly what disqualifies a "+
				"predefined viewer role, so it must be denylisted deliberately rather than left "+
				"to fail-closed", perm, got)
		}
	}
}

func TestTheAllowlistAndDenylistDoNotOverlap(t *testing.T) {
	t.Parallel()
	// Two lists that disagree would make Classify's order of checks the real policy, which is not
	// something anybody should have to read the implementation to learn.
	write := map[string]bool{}
	for _, v := range gcpx.WriteVerbs() {
		write[v] = true
	}
	for _, v := range gcpx.ReadVerbs() {
		if write[v] {
			t.Errorf("verb %q is on both the read allowlist and the write denylist", v)
		}
	}
}

// TestAnUntestableAreaProducesAnAssertionRequirementRatherThanAnAssumption is FR-004's clause.
func TestAnUntestableAreaProducesAnAssertionRequirementRatherThanAnAssumption(t *testing.T) {
	t.Parallel()
	areas := gcpx.UnverifiableAreas()
	if len(areas) == 0 {
		t.Fatal("no unverifiable areas are declared; research §8.1 lists seven, and a gate that " +
			"claims to verify everything is claiming more than testIamPermissions can deliver")
	}

	// No assertion at all: refused, and the refusal names the layer and lists what it could not check.
	err := (&gcpx.Assertion{}).Validate(areas)
	if err == nil {
		t.Fatal("a missing operator assertion was accepted")
	}
	var refusal *gcpx.GateRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("error %T is not a *GateRefusal; the refusal must name its layer", err)
	}
	if refusal.Layer != gcpx.LayerAssertion {
		t.Errorf("layer = %q, want %q", refusal.Layer, gcpx.LayerAssertion)
	}
	if !strings.Contains(err.Error(), "child-resource bindings") {
		t.Errorf("the refusal does not name what could not be verified:\n%s", err)
	}

	// A PARTIAL acknowledgement is refused too: it looks like coverage, which is worse than none.
	partial := &gcpx.Assertion{By: "dana@example.com", Acknowledged: []string{areas[0][:strings.Index(areas[0], ":")]}}
	if err := partial.Validate(areas); err == nil {
		t.Error("a partial acknowledgement was accepted; the areas nobody re-read are exactly the " +
			"ones a stale config drops")
	}

	// A full one passes.
	full := &gcpx.Assertion{By: "dana@example.com"}
	for _, area := range areas {
		full.Acknowledged = append(full.Acknowledged, area[:strings.Index(area, ":")])
	}
	if err := full.Validate(areas); err != nil {
		t.Errorf("a complete assertion was refused: %v", err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
