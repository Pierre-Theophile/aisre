// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"sort"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Actor kind (T057, T058, FR-020, data-model.md §4.1, contracts/gcp-feeder.md §3.5).
//
// FR-020 forbids inferring actor kind from the principal *string*, which rules out the
// implementation everybody writes first: `strings.HasSuffix(email, ".gserviceaccount.com")` ⇒
// AUTOMATION. It is wrong in both directions. A service account can be a human's impersonation
// session — the audit entry says so one level down — and a human's address at a company using
// Workload Identity Federation is not an address at all. So the kind comes from a
// **classification**: a configured list, or a documented GCP signal, and nothing else.
//
// The ladder, in order, stopping at the first hit (data-model.md §4.1):
//
//  1. on the configured deployment-automation list → AUTOMATION
//  2. GCP states the caller is a platform service agent → CONTROLLER
//  3. a configured human-directory entry, or GCP states it is a user account → PERSON
//  4. the change came from the vendor-notice feeder or is a provider maintenance event → VENDOR
//  5. otherwise → ACTOR_KIND_UNKNOWN, with the evidence that was available recorded
//
// # Why AUTOMATION before CONTROLLER matters downstream
//
// A CI principal deploying is a candidate *cause*. An autoscaler reacting four minutes after
// onset is a candidate *effect*, and feature 002's causal ordering exonerates on exactly that
// difference (ADR-0005 D1). Collapsing them into one "robot" kind removes the distinction the
// investigation engine uses to stop blaming the autoscaler for the outage it reacted to.
//
// # UNSPECIFIED is not UNKNOWN
//
// UNSPECIFIED means the source said nothing and canonical serialisation omits the field. UNKNOWN
// means an actor *was* observed and could not be classified. A feeder writing UNKNOWN where it
// learned nothing is claiming to have looked — it turns "no audit entry exists for this change"
// into "somebody did this and we could not tell who", which is a different and more alarming
// statement. Classify returns UNSPECIFIED for an empty AuthenticationInfo and UNKNOWN only when
// there was a principal it could not place.

// AuthenticationInfo is the evidence an audit entry carries about who called, as
// `protoPayload.authenticationInfo` plus the one field beside it that is evidence and never
// decisive. It is this package's own type rather than the SDK's so that the recorded fixture
// shape and the live shape are the same struct.
type AuthenticationInfo struct {
	// PrincipalEmail is the caller. It is evidence for the *classification*, never for a
	// string-shape inference (FR-020), and it is dropped before anything reaches disk (FR-135) —
	// this struct exists in memory, inside the feeder, and its principal fields never leave it.
	PrincipalEmail string
	// PrincipalSubject is the same caller in subject form, which is what a federated identity
	// carries instead of an address.
	PrincipalSubject string
	// ServiceAccountKeyName is present when the call authenticated with a static key, which is a
	// CI signal — a human uses a short-lived token, a pipeline that predates Workload Identity
	// uses a key file. It is a signal and not a verdict: the automation list decides.
	ServiceAccountKeyName string
	// FirstPartyPrincipals are the `serviceAccountDelegationInfo[].firstPartyPrincipal` entries.
	// Research §2 establishes this as **the only documented signal that the caller is a Google
	// first-party principal**, and it is therefore the evidence for CONTROLLER. Nothing else in
	// an audit entry distinguishes a platform service agent from any other service account.
	FirstPartyPrincipals []string
	// ThirdPartyPrincipal is set when the delegation chain names an external principal.
	ThirdPartyPrincipal bool
	// CallerSuppliedUserAgent is useful for telling gcloud from Terraform from a custom CI
	// client, and GCP's own reference says it "is not authenticated and should be treated
	// accordingly". It is recorded as evidence and is **never** the deciding factor (T058).
	CallerSuppliedUserAgent string
}

// Empty reports whether there is no authentication evidence at all, which is the case that must
// yield UNSPECIFIED rather than UNKNOWN.
func (a AuthenticationInfo) Empty() bool {
	return strings.TrimSpace(a.PrincipalEmail) == "" &&
		strings.TrimSpace(a.PrincipalSubject) == "" &&
		strings.TrimSpace(a.ServiceAccountKeyName) == "" &&
		len(a.FirstPartyPrincipals) == 0 &&
		!a.ThirdPartyPrincipal
}

// ActorPolicy is the configured classification, from `config/gcp.yaml`. Both lists are
// configuration because CI identities and human directories are per-organisation, and a default
// list would be a guess about somebody else's estate.
type ActorPolicy struct {
	// DeploymentAutomation is the principals attributed AUTOMATION. Rung 1.
	DeploymentAutomation []string
	// HumanDirectory is the principals attributed PERSON. Rung 3. It is a list rather than a
	// domain pattern on purpose: a domain pattern is a string inference by another name, and it
	// types a service account at the company domain as a person.
	HumanDirectory []string
}

// Actor is the typed reading of one audit entry's caller.
type Actor struct {
	// Kind is the classification.
	Kind graphv1.ActorKind
	// Rung names which step of the ladder decided, so a reviewer can see *why* rather than
	// reverse-engineer it from the kind.
	Rung string
	// Evidence is what was available, as `field=what-it-showed` pairs, sorted. It carries **no
	// principal address**: a principal is dropped, never recorded (FR-135), so the evidence says
	// that a principal email was present and what the classification made of it.
	Evidence []string
}

// The published rungs.
const (
	// RungAutomationList is rung 1: the principal is on the configured automation list.
	RungAutomationList = "configured deployment-automation list"
	// RungFirstPartyPrincipal is rung 2: GCP states the caller is a first-party principal.
	RungFirstPartyPrincipal = "serviceAccountDelegationInfo[].firstPartyPrincipal"
	// RungHumanDirectory is rung 3: the principal is a configured human-directory entry.
	RungHumanDirectory = "configured human directory"
	// RungVendorEvent is rung 4: the change is a provider event rather than a caller's action.
	RungVendorEvent = "a provider maintenance or notice event"
	// RungUnclassified is rung 5: a principal was observed and none of the above placed it.
	RungUnclassified = "a principal was observed and no classification placed it"
	// RungNoEvidence is the case above the ladder: there was nothing to classify.
	RungNoEvidence = "the source said nothing about who acted"
)

// Classify runs the ladder. `vendorEvent` is true for a change that came from the vendor-notice
// feeder or is a provider maintenance event — rung 4, which is a fact about the *change* rather
// than about the caller, which is why it is a parameter rather than a field of AuthenticationInfo.
func (p ActorPolicy) Classify(auth AuthenticationInfo, vendorEvent bool) Actor {
	evidence := auth.evidence()

	// Above the ladder: nothing to classify. UNSPECIFIED, and the field is omitted downstream.
	// A vendor event with no caller is still rung 4, because the provider *is* the actor.
	if auth.Empty() && !vendorEvent {
		return Actor{Kind: graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED, Rung: RungNoEvidence, Evidence: evidence}
	}

	principal := strings.TrimSpace(auth.PrincipalEmail)
	if principal == "" {
		principal = strings.TrimSpace(auth.PrincipalSubject)
	}

	// 1. The configured automation list. First, so that a CI principal that also authenticated
	// with a static key and happens to appear in a delegation chain is still AUTOMATION — the
	// organisation's own statement about its own pipelines outranks every inferred signal.
	if listed(p.DeploymentAutomation, principal) {
		return Actor{Kind: graphv1.ActorKind_AUTOMATION, Rung: RungAutomationList, Evidence: evidence}
	}

	// 2. A Google first-party principal. The only documented CONTROLLER signal (research §2).
	if len(auth.FirstPartyPrincipals) > 0 {
		return Actor{Kind: graphv1.ActorKind_CONTROLLER, Rung: RungFirstPartyPrincipal, Evidence: evidence}
	}

	// 3. The configured human directory.
	if listed(p.HumanDirectory, principal) {
		return Actor{Kind: graphv1.ActorKind_PERSON, Rung: RungHumanDirectory, Evidence: evidence}
	}

	// 4. A provider event: the vendor is the actor.
	if vendorEvent {
		return Actor{Kind: graphv1.ActorKind_VENDOR, Rung: RungVendorEvent, Evidence: evidence}
	}

	// 5. An actor was observed and could not be classified. The evidence goes with it, because
	// "unknown" with no evidence is indistinguishable from a feeder that did not look.
	return Actor{Kind: graphv1.ActorKind_ACTOR_KIND_UNKNOWN, Rung: RungUnclassified, Evidence: evidence}
}

// evidence renders what was available, without the principal's address.
//
// Each entry is `field=<what it showed>`, and the values are deliberately categorical rather than
// literal: "present", "static key", a user-agent product token. The one exception is the
// user-agent, which is recorded as GCP returned it because its whole value is telling gcloud from
// Terraform — and because it is not a person, it is a client.
func (a AuthenticationInfo) evidence() []string {
	var out []string
	if strings.TrimSpace(a.PrincipalEmail) != "" {
		out = append(out, "authenticationInfo.principalEmail=present (dropped, never recorded: FR-135)")
	}
	if strings.TrimSpace(a.PrincipalSubject) != "" {
		out = append(out, "authenticationInfo.principalSubject=present (dropped, never recorded: FR-135)")
	}
	if strings.TrimSpace(a.ServiceAccountKeyName) != "" {
		out = append(out, "authenticationInfo.serviceAccountKeyName=present (static-key auth)")
	}
	if n := len(a.FirstPartyPrincipals); n > 0 {
		out = append(out, "authenticationInfo.serviceAccountDelegationInfo[].firstPartyPrincipal=present")
	}
	if a.ThirdPartyPrincipal {
		out = append(out, "authenticationInfo.serviceAccountDelegationInfo[].thirdPartyPrincipal=present")
	}
	if ua := strings.TrimSpace(a.CallerSuppliedUserAgent); ua != "" {
		// Recorded as evidence, never decisive: GCP's reference says it "is not authenticated and
		// should be treated accordingly". The note travels with the value so that a reader of a
		// golden does not draw a conclusion from it either.
		out = append(out, "requestMetadata.callerSuppliedUserAgent="+userAgentProduct(ua)+
			" (not authenticated; evidence only, never decisive)")
	}
	sort.Strings(out)
	return out
}

// userAgentProduct reduces a user-agent to its leading product token — `gcloud/456.0.0` from a
// long string of platform detail. The detail is a fingerprint of one operator's workstation, and
// the product is the whole of what the field is useful for.
func userAgentProduct(ua string) string {
	if idx := strings.IndexAny(ua, " ("); idx > 0 {
		return ua[:idx]
	}
	return ua
}

// listed reports whether principal appears in the configured list, comparing case-insensitively
// and ignoring surrounding space — the two ways a hand-maintained config file differs from an
// audit entry. It is an exact comparison otherwise: no prefix matching and no domain matching,
// because both are the string inference FR-020 forbids.
func listed(list []string, principal string) bool {
	if principal == "" {
		return false
	}
	target := strings.ToLower(principal)
	for _, entry := range list {
		if strings.ToLower(strings.TrimSpace(entry)) == target {
			return true
		}
	}
	return false
}
