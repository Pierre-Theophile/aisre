// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// Actor kind (T057, T058, FR-020, data-model.md §4.1).

func actorPolicy() gcpfeeder.ActorPolicy {
	return gcpfeeder.ActorPolicy{
		DeploymentAutomation: []string{"deploy@nova-production.iam.gserviceaccount.com"},
		HumanDirectory:       []string{"operator@example.com"},
	}
}

// The ladder, in order, stopping at the first hit.
func TestTheActorLadderStopsAtItsFirstHit(t *testing.T) {
	policy := actorPolicy()
	cases := []struct {
		name        string
		auth        gcpfeeder.AuthenticationInfo
		vendorEvent bool
		want        graphv1.ActorKind
		wantRung    string
	}{
		{
			name:     "rung 1: the configured automation list",
			auth:     gcpfeeder.AuthenticationInfo{PrincipalEmail: "deploy@nova-production.iam.gserviceaccount.com"},
			want:     graphv1.ActorKind_AUTOMATION,
			wantRung: gcpfeeder.RungAutomationList,
		},
		{
			name: "rung 1 outranks every inferred signal",
			// The organisation's own statement about its own pipelines beats a delegation chain
			// and a static key: a CI principal that appears in both is still AUTOMATION.
			auth: gcpfeeder.AuthenticationInfo{
				PrincipalEmail:        "deploy@nova-production.iam.gserviceaccount.com",
				ServiceAccountKeyName: "projects/p/serviceAccounts/deploy/keys/abc",
				FirstPartyPrincipals:  []string{"service-agent"},
			},
			want:     graphv1.ActorKind_AUTOMATION,
			wantRung: gcpfeeder.RungAutomationList,
		},
		{
			name:     "rung 2: a Google first-party principal is a controller",
			auth:     gcpfeeder.AuthenticationInfo{PrincipalEmail: "service-123@gcp-sa-run.iam.gserviceaccount.com", FirstPartyPrincipals: []string{"run-agent"}},
			want:     graphv1.ActorKind_CONTROLLER,
			wantRung: gcpfeeder.RungFirstPartyPrincipal,
		},
		{
			name:     "rung 3: the configured human directory",
			auth:     gcpfeeder.AuthenticationInfo{PrincipalEmail: "operator@example.com"},
			want:     graphv1.ActorKind_PERSON,
			wantRung: gcpfeeder.RungHumanDirectory,
		},
		{
			name:        "rung 4: a provider event",
			auth:        gcpfeeder.AuthenticationInfo{PrincipalEmail: "someone@elsewhere.test"},
			vendorEvent: true,
			want:        graphv1.ActorKind_VENDOR,
			wantRung:    gcpfeeder.RungVendorEvent,
		},
		{
			name:     "rung 5: observed and unclassified",
			auth:     gcpfeeder.AuthenticationInfo{PrincipalEmail: "someone@elsewhere.test"},
			want:     graphv1.ActorKind_ACTOR_KIND_UNKNOWN,
			wantRung: gcpfeeder.RungUnclassified,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actor := policy.Classify(tc.auth, tc.vendorEvent)
			if actor.Kind != tc.want {
				t.Fatalf("kind = %s, want %s (rung %q)", actor.Kind, tc.want, actor.Rung)
			}
			if actor.Rung != tc.wantRung {
				t.Errorf("rung = %q, want %q", actor.Rung, tc.wantRung)
			}
		})
	}
}

// AUTOMATION is not CONTROLLER. A CI principal deploying is a candidate *cause*; an autoscaler
// reacting four minutes after onset is a candidate *effect*, and feature 002's causal ordering
// exonerates on exactly that difference (ADR-0005 D1).
func TestAutomationAndControllerAreNotOneRobotKind(t *testing.T) {
	policy := actorPolicy()
	ci := policy.Classify(gcpfeeder.AuthenticationInfo{
		PrincipalEmail: "deploy@nova-production.iam.gserviceaccount.com"}, false)
	agent := policy.Classify(gcpfeeder.AuthenticationInfo{
		PrincipalEmail: "service-123@gcp-sa-run.iam.gserviceaccount.com", FirstPartyPrincipals: []string{"run-agent"}}, false)
	if ci.Kind == agent.Kind {
		t.Fatalf("a CI principal and a platform service agent both classified %s; collapsing them "+
			"removes the distinction feature 002 uses to stop blaming the autoscaler for the outage "+
			"it reacted to (ADR-0005 D1)", ci.Kind)
	}
}

// UNSPECIFIED is not UNKNOWN. A feeder writing UNKNOWN where it learned nothing is claiming to have
// looked: it turns "no audit entry exists" into "somebody did this and we could not tell who".
func TestUnspecifiedIsNotUnknown(t *testing.T) {
	policy := actorPolicy()
	nothing := policy.Classify(gcpfeeder.AuthenticationInfo{}, false)
	if nothing.Kind != graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED {
		t.Fatalf("no evidence gave %s, want ACTOR_KIND_UNSPECIFIED; UNKNOWN would claim a principal "+
			"was observed", nothing.Kind)
	}
	if nothing.Rung != gcpfeeder.RungNoEvidence {
		t.Errorf("rung = %q, want %q", nothing.Rung, gcpfeeder.RungNoEvidence)
	}

	observed := policy.Classify(gcpfeeder.AuthenticationInfo{PrincipalEmail: "someone@elsewhere.test"}, false)
	if observed.Kind != graphv1.ActorKind_ACTOR_KIND_UNKNOWN {
		t.Fatalf("an observed and unplaceable principal gave %s, want ACTOR_KIND_UNKNOWN", observed.Kind)
	}
	// UNKNOWN carries the evidence that was available, or it is indistinguishable from a feeder
	// that did not look.
	if len(observed.Evidence) == 0 {
		t.Error("ACTOR_KIND_UNKNOWN carries no evidence (T058)")
	}
}

// The user agent is recorded as evidence and is never decisive: GCP's own reference says it "is not
// authenticated and should be treated accordingly".
func TestTheUserAgentIsEvidenceAndNeverDecisive(t *testing.T) {
	policy := actorPolicy()
	base := gcpfeeder.AuthenticationInfo{PrincipalEmail: "someone@elsewhere.test"}

	withoutUA := policy.Classify(base, false)
	for _, ua := range []string{
		"gcloud/456.0.0 (Linux)", "terraform-provider-google/5.0",
		"deploy-bot/1.0", "Mozilla/5.0",
	} {
		withUA := base
		withUA.CallerSuppliedUserAgent = ua
		got := policy.Classify(withUA, false)
		if got.Kind != withoutUA.Kind {
			t.Errorf("the user agent %q changed the classification to %s; GCP's own reference says "+
				"it is not authenticated and it must never be the deciding factor (T058)", ua, got.Kind)
		}
		if !strings.Contains(strings.Join(got.Evidence, "\n"), "callerSuppliedUserAgent") {
			t.Errorf("the user agent %q was not recorded as evidence", ua)
		}
	}
}

// The evidence carries no principal address: a principal is dropped, never recorded (FR-135).
func TestTheEvidenceCarriesNoPrincipalAddress(t *testing.T) {
	actor := actorPolicy().Classify(gcpfeeder.AuthenticationInfo{
		PrincipalEmail:          "jane.doe@acme-corp.io",
		PrincipalSubject:        "user:jane.doe@acme-corp.io",
		ServiceAccountKeyName:   "projects/p/serviceAccounts/jane.doe@acme-corp.io/keys/abc",
		CallerSuppliedUserAgent: "gcloud/456.0.0 (Linux; jane-laptop)",
	}, false)
	joined := strings.Join(actor.Evidence, "\n")
	for _, leaked := range []string{"jane.doe", "acme-corp", "jane-laptop"} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("the evidence carries %q: %s", leaked, joined)
		}
	}
	// It still says a principal was there, or the classification would be unreviewable.
	if !strings.Contains(joined, "principalEmail") {
		t.Errorf("the evidence does not record that a principal was present: %s", joined)
	}
}

// FR-020 forbids inferring actor kind from the principal string. A service-account-shaped address
// that is not on the configured list is UNKNOWN, not AUTOMATION — the implementation everybody
// writes first is wrong in both directions.
func TestActorKindIsNotInferredFromTheShapeOfThePrincipal(t *testing.T) {
	policy := actorPolicy()
	unlisted := policy.Classify(gcpfeeder.AuthenticationInfo{
		PrincipalEmail: "some-other-sa@nova-production.iam.gserviceaccount.com"}, false)
	if unlisted.Kind == graphv1.ActorKind_AUTOMATION {
		t.Fatal("a .gserviceaccount.com address was typed AUTOMATION from its shape; FR-020 forbids " +
			"inferring the kind from the principal string, and a service account can be a human's " +
			"impersonation session")
	}
	// And a company-domain address that is not in the directory is not a person either.
	notListed := policy.Classify(gcpfeeder.AuthenticationInfo{PrincipalEmail: "someone@example.com"}, false)
	if notListed.Kind == graphv1.ActorKind_PERSON {
		t.Fatal("a company-domain address was typed PERSON from its domain")
	}
}

// The comparison against the configured lists is exact after folding case and space: a prefix or
// domain match would be the string inference FR-020 forbids, wearing a list as a disguise.
func TestTheConfiguredListsAreMatchedExactly(t *testing.T) {
	policy := gcpfeeder.ActorPolicy{DeploymentAutomation: []string{"  Deploy@Example.COM  "}}
	if got := policy.Classify(gcpfeeder.AuthenticationInfo{PrincipalEmail: "deploy@example.com"}, false); got.Kind != graphv1.ActorKind_AUTOMATION {
		t.Errorf("case and surrounding space defeated the list: %s", got.Kind)
	}
	for _, near := range []string{"deploy@example.com.evil.test", "xdeploy@example.com", "deploy@example.co"} {
		if got := policy.Classify(gcpfeeder.AuthenticationInfo{PrincipalEmail: near}, false); got.Kind == graphv1.ActorKind_AUTOMATION {
			t.Errorf("%q matched the automation list; the comparison must be exact", near)
		}
	}
}
