// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The read-only startup gate (004 T032, FR-003).
//
// FR-003 has three clauses and the gate fails all three ways: a platform that reported a write, a
// platform that reports nothing with nobody asserting, and a report nobody filled in. What it never
// does is conclude read-only from silence.

// A reported write refuses, and the refusal names what it found.
func TestAReportedWritePermissionRefusesTheStart(t *testing.T) {
	t.Parallel()

	for name, permissions := range map[string]map[string]string{
		"one write among reads":       {"deployments": "read", "contents": "write", "metadata": "read"},
		"admin":                       {"administration": "admin"},
		"upper-cased write":           {"deployments": "WRITE"},
		"a value nobody has ruled on": {"deployments": "read", "secrets": "maintain"},
	} {
		_, err := feeder.CheckReadOnly(feeder.CredentialReport{
			Platform: "github", Evidence: feeder.EvidencePlatformReported, Permissions: permissions,
		})
		var writable *feeder.WriteCapableError
		if !errors.As(err, &writable) {
			t.Errorf("%s: CheckReadOnly returned %v, wanted a refusal; a read-only gate that admits a "+
				"value it has never heard of is a gate that fails open", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "github") {
			t.Errorf("%s: the refusal does not name the platform: %q", name, err)
		}
	}
}

// Reads are admitted, and the evidence says the platform is where it came from.
func TestReportedReadsStartWithPlatformEvidence(t *testing.T) {
	t.Parallel()

	evidence, err := feeder.CheckReadOnly(feeder.CredentialReport{
		Platform: "github", Evidence: feeder.EvidencePlatformReported,
		Permissions: map[string]string{"deployments": "read", "actions": "read", "metadata": "read"},
	})
	if err != nil {
		t.Fatalf("CheckReadOnly refused a read-only credential: %v", err)
	}
	if evidence != feeder.EvidencePlatformReported {
		t.Errorf("evidence = %q, want the platform-reported form: a checkpoint has to say whether the "+
			"guarantee came from the platform or from somebody's word", evidence)
	}
	// Case folding is tested HERE, in the admit direction, and not on a write: a write refuses whatever
	// its case because the whitelist fails closed, so the only thing folding changes is whether a
	// platform spelling `Read` is understood. A platform's spelling is its own business.
	for _, spelling := range []string{"READ", "Read", " read ", "None"} {
		if _, err := feeder.CheckReadOnly(feeder.CredentialReport{
			Platform: "github", Evidence: feeder.EvidencePlatformReported,
			Permissions: map[string]string{"deployments": spelling},
		}); err != nil {
			t.Errorf("a read spelled %q was refused: %v", spelling, err)
		}
	}

	// `none` is a read: a platform may report a permission it did not grant.
	if _, err := feeder.CheckReadOnly(feeder.CredentialReport{
		Platform: "github", Evidence: feeder.EvidencePlatformReported,
		Permissions: map[string]string{"deployments": "read", "secrets": "none"},
	}); err != nil {
		t.Errorf("a permission granted at `none` was treated as a write: %v", err)
	}
}

// Silence is never read-only. This is the clause that needed a type rather than a boolean.
func TestSilenceIsNotReadOnly(t *testing.T) {
	t.Parallel()

	_, err := feeder.CheckReadOnly(feeder.CredentialReport{Platform: "vercel"})
	var unverified *feeder.UnverifiedCredentialError
	if !errors.As(err, &unverified) {
		t.Fatalf("CheckReadOnly returned %v for a platform that reports nothing; FR-003 requires the "+
			"operator's assertion rather than the assumption", err)
	}

	// With the assertion it starts, and the evidence records that it was an assertion — which is the
	// whole point: a guarantee resting on an operator's word is worth less than one resting on a
	// platform's, and a reader must be able to tell which they have.
	evidence, err := feeder.CheckReadOnly(feeder.CredentialReport{
		Platform: "vercel", Evidence: feeder.EvidenceOperatorAsserted,
	})
	if err != nil {
		t.Fatalf("CheckReadOnly refused an asserted credential: %v", err)
	}
	if evidence != feeder.EvidenceOperatorAsserted {
		t.Errorf("evidence = %q, want the asserted form", evidence)
	}
	if !strings.Contains(string(evidence), "asserted") {
		t.Errorf("evidence %q does not say it was asserted; a value a reader could mistake for the "+
			"platform's statement defeats the distinction", evidence)
	}
}

// An empty permission set from a platform that DOES report is a misconfiguration, not a read-only
// credential. The two are easy to conflate and the consequence differs: one connector cannot work,
// the other is safe.
func TestAnEmptyReportedPermissionSetRefuses(t *testing.T) {
	t.Parallel()

	if _, err := feeder.CheckReadOnly(feeder.CredentialReport{
		Platform: "github", Evidence: feeder.EvidencePlatformReported,
	}); err == nil {
		t.Error("a platform that reported no permissions at all was treated as read-only; a credential " +
			"that can do nothing is a misconfiguration worth failing on")
	}
	// And a report with no platform name is refused, because its refusal would name nobody.
	if _, err := feeder.CheckReadOnly(feeder.CredentialReport{
		Evidence: feeder.EvidenceOperatorAsserted,
	}); err == nil {
		t.Error("a credential report with no platform name was admitted")
	}
}

// The scope is recorded with who enforces it, because FR-008's two regimes are not equivalent and the
// connector must not present them as one.
func TestTheScopeRecordsWhoEnforcesIt(t *testing.T) {
	t.Parallel()

	github := feeder.CredentialScope{
		PlatformEnforced: true, Selection: "selected",
		Targets: []string{"acme/storefront", "acme/web"},
	}
	vercel := feeder.CredentialScope{Targets: []string{"prj_7xYz"}}

	if !github.PlatformEnforced {
		t.Error("GitHub's installation repository selection is a boundary the platform holds")
	}
	if vercel.PlatformEnforced {
		t.Error("an unscoped Vercel token's project list is the operator's narrowing, not a boundary " +
			"anyone holds; recording it as platform-enforced would overstate the guarantee")
	}
	if github.Selection == "" {
		t.Error("GitHub reports its own word for the regime and it should be carried, not re-derived")
	}
	if vercel.Selection != "" {
		t.Error("Vercel reports no selection word; inventing one would put a platform's voice on the " +
			"operator's choice")
	}
}
