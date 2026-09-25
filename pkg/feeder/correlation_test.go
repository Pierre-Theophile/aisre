// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Identity and correlation are two kinds, and the SDK's list of which is which must match the one the
// event log enforces (004 T148).
//
// Both directions, because either drift is a feeder whose events are refused at run time rather than
// at review: a namespace this SDK calls a correlation that the log treats as a name would have its
// CorrelateEntity accepted and its rules never fire, and one the log calls a correlation that this SDK
// does not would have its IdentityClaim refused in production.
func TestTheSDKAndTheEventLogAgreeOnWhichNamespacesAreCorrelations(t *testing.T) {
	t.Parallel()
	log := map[string]bool{}
	for _, ns := range graph.CorrelationNamespaces {
		log[ns] = true
	}
	sdk := map[string]bool{}
	for _, ns := range feeder.CorrelationNamespaces {
		sdk[ns] = true
	}
	for ns := range sdk {
		if !log[ns] {
			t.Errorf("the SDK publishes %q as a correlation key and the event log does not; an "+
				"IdentityClaim in it would be accepted and C1 would merge on a value several entities "+
				"share", ns)
		}
	}
	for ns := range log {
		if !sdk[ns] {
			t.Errorf("the event log treats %q as a correlation key and the SDK does not; a feeder "+
				"following the SDK would emit an IdentityClaim the log refuses", ns)
		}
	}
	if len(sdk) == 0 {
		t.Fatal("the SDK publishes no correlation namespaces, so this comparison compares nothing")
	}
}

// The deploy vocabulary is correlation, not identity — all three of it.
func TestTheDeployVocabularyIsCorrelation(t *testing.T) {
	t.Parallel()
	for _, ns := range []string{
		feeder.NSDeployCommitSHA, feeder.NSDeployImage, feeder.NSDeployRelease,
	} {
		if !graph.IsCorrelationNamespace(ns) {
			t.Errorf("%q is not published as a correlation key. A monorepo run ships one commit to "+
				"several services and a redeploy ships it again, so several entities carry each of "+
				"these values — and stored as a name the value lands on whichever entity was "+
				"processed first", ns)
		}
	}
}

// `github.repo` is the near miss, and it is deliberately NOT a correlation namespace.
//
// It was in the first draft of the registry, for a reason that looks right: claimed on a change it is
// shared by every change the repository ships, and claimed on a service by every service in a monorepo.
// But the refusal this registry drives sees a NAMESPACE and not a SUBJECT, and `acme/storefront` does
// name exactly one repository — the deploy feeders address a rollout's target by it, which mints an
// entity, and `createEntity` gives every entity a primary identity claim in the namespace that
// addressed it. A registry holding `github.repo` would promise a refusal the projector itself breaks.
//
// What guards the misuse is C1's `sharedPropertyNamespaces`, and the GitHub connector emitting the
// repository as a property of the change rather than as either kind.
func TestTheRepositoryIsNotACorrelationNamespace(t *testing.T) {
	t.Parallel()
	if graph.IsCorrelationNamespace(feeder.NSGitHubRepo) {
		t.Errorf("%q is published as a correlation key, so an identity claim in it is refused — but a "+
			"change's TARGET is addressed by it, and minting that entity writes exactly such a claim",
			feeder.NSGitHubRepo)
	}
	// And the guard that does cover it is in place, or nothing covers it at all.
	if resolution.IdentifyingNamespace(feeder.NSGitHubRepo) {
		t.Errorf("%q is neither a correlation namespace nor narrowed away from C1; claimed on two "+
			"services in a monorepo it would merge them, certainly and with no human in the loop",
			feeder.NSGitHubRepo)
	}
}

// And the identity namespaces are NOT correlations, so the two lists are not simply everything.
func TestAnIdentifierIsNotACorrelation(t *testing.T) {
	t.Parallel()
	for _, ns := range []string{
		feeder.NSOTelService, feeder.NSK8sDeployment, feeder.NSGitHubChange, feeder.NSVercelChange,
	} {
		if graph.IsCorrelationNamespace(ns) {
			t.Errorf("%q is published as a correlation key, but it NAMES one thing: two sources "+
				"saying it are naming the same entity, which is what C1 merges on", ns)
		}
	}
}

// A correlation key renders a ref in its own namespace, like a claim does.
func TestACorrelationKeyRendersItsRef(t *testing.T) {
	t.Parallel()
	key := feeder.CorrelationKey{
		Namespace: feeder.NSDeployCommitSHA,
		Value:     "8f5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4",
		Why:       "deployment.sha",
		Attrs:     map[string]string{"deployment.environment.name": "production"},
	}
	ref := key.Ref()
	if ref.GetNamespace() != feeder.NSDeployCommitSHA || ref.GetValue() != key.Value {
		t.Errorf("Ref() = %s=%s, want %s=%s",
			ref.GetNamespace(), ref.GetValue(), feeder.NSDeployCommitSHA, key.Value)
	}
	// The distinction is enforced by the compiler, which is the reason for a separate type: this line
	// would not compile if CorrelationKey were an alias of Claim.
	if strings.Contains(strings.ToLower(key.Why), "identity") {
		t.Error("the evidence reads as an identity assertion")
	}
}
