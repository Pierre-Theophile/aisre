// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
)

// Actor kind (004 T058–T060, FR-013, FR-027, SC-005).

// The ladder, one case per rung, each naming the rung that must decide it.
func TestTheLadderTypesEachCaseFromWhatGitHubStates(t *testing.T) {
	t.Parallel()
	policy := github.ActorPolicy{DeploymentAutomation: []string{"release-runner"}}

	for name, tc := range map[string]struct {
		evidence github.ActorEvidence
		want     graphv1.ActorKind
		wantRung string
	}{
		"a push by a user is a person": {
			evidence: github.ActorEvidence{
				Event: github.EventPush, Account: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
			},
			want: graphv1.ActorKind_PERSON, wantRung: github.RungUserAccount,
		},
		"a manual dispatch by a user is a person": {
			evidence: github.ActorEvidence{
				Event: github.EventWorkflowDispatch, Account: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
			},
			want: graphv1.ActorKind_PERSON, wantRung: github.RungUserAccount,
		},
		"a scheduled run is the clock, not the person who last edited the workflow": {
			evidence: github.ActorEvidence{
				Event: github.EventSchedule, Account: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
			},
			want: graphv1.ActorKind_CONTROLLER, wantRung: github.RungScheduleTrigger,
		},
		"a repository dispatch is another system": {
			evidence: github.ActorEvidence{
				Event: github.EventRepositoryDispatch, Account: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
			},
			want: graphv1.ActorKind_AUTOMATION, wantRung: github.RungSystemTrigger,
		},
		"a workflow triggered by a workflow is another system": {
			evidence: github.ActorEvidence{
				Event: github.EventWorkflowRun, Account: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
			},
			want: graphv1.ActorKind_AUTOMATION, wantRung: github.RungSystemTrigger,
		},
		"a reusable workflow invoked by another is another system": {
			evidence: github.ActorEvidence{
				Event: github.EventWorkflowCall, Account: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
			},
			want: graphv1.ActorKind_AUTOMATION, wantRung: github.RungSystemTrigger,
		},
		"an app acts as a bot account": {
			evidence: github.ActorEvidence{
				Event: github.EventPush, Account: github.Account{Login: "dependabot[bot]", ID: 9, Type: github.AccountBot},
			},
			want: graphv1.ActorKind_AUTOMATION, wantRung: github.RungBotAccount,
		},
		"the configured list outranks GitHub's typing": {
			evidence: github.ActorEvidence{
				Event: github.EventPush, Account: github.Account{Login: "release-runner", ID: 4, Type: github.AccountUser},
			},
			want: graphv1.ActorKind_AUTOMATION, wantRung: github.RungAutomationList,
		},
		"a deployment states no trigger, so the account decides": {
			evidence: github.ActorEvidence{
				Account: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
			},
			want: graphv1.ActorKind_PERSON, wantRung: github.RungUserAccount,
		},
		"an organisation does not act": {
			evidence: github.ActorEvidence{
				Event: github.EventPush, Account: github.Account{Login: "acme", ID: 3, Type: github.AccountOrganization},
			},
			want: graphv1.ActorKind_ACTOR_KIND_UNKNOWN, wantRung: github.RungUnclassified,
		},
		"an account type nobody has ruled on is unknown rather than guessed": {
			evidence: github.ActorEvidence{
				Event: github.EventPush, Account: github.Account{Login: "mystery", ID: 5, Type: "Mannequin"},
			},
			want: graphv1.ActorKind_ACTOR_KIND_UNKNOWN, wantRung: github.RungUnclassified,
		},
		"a trigger with no account at all is unknown": {
			evidence: github.ActorEvidence{Event: "deployment"},
			want:     graphv1.ActorKind_ACTOR_KIND_UNKNOWN, wantRung: github.RungUnclassified,
		},
		"a payload that records nothing is unspecified": {
			evidence: github.ActorEvidence{},
			want:     graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED, wantRung: github.RungNoEvidence,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := policy.Classify(tc.evidence)
			if got.Kind != tc.want {
				t.Errorf("Classify(%+v).Kind = %v, want %v (rung %q)", tc.evidence, got.Kind, tc.want, got.Rung)
			}
			if got.Rung != tc.wantRung {
				t.Errorf("Classify(%+v) decided at %q, want %q; the rung is what a reviewer reads instead "+
					"of reverse-engineering the kind", tc.evidence, got.Rung, tc.wantRung)
			}
		})
	}
}

// SC-005: the kind is never derived from the login. The fixture is the one built to break the
// implementation everybody writes first — a machine user called `deploy-bot` that GitHub types as a
// user, and a person whose login reads like a service.
func TestTheKindIsNeverDerivedFromTheLogin(t *testing.T) {
	t.Parallel()
	var policy github.ActorPolicy

	// A login that reads like a bot, typed by GitHub as a user account. Without the operator saying
	// otherwise this is a PERSON, because GitHub's typing is the fact and the name is a display choice.
	lookalikeBot := policy.Classify(github.ActorEvidence{
		Event:   github.EventPush,
		Account: github.Account{Login: "deploy-bot", ID: 11, Type: github.AccountUser},
	})
	if lookalikeBot.Kind != graphv1.ActorKind_PERSON {
		t.Errorf("an account GitHub types `User` whose login reads `deploy-bot` was classified %v; the "+
			"login is a display choice and the type is the fact (SC-005)", lookalikeBot.Kind)
	}

	// And the other direction: a login that reads like a person, typed by GitHub as a bot.
	lookalikePerson := policy.Classify(github.ActorEvidence{
		Event:   github.EventPush,
		Account: github.Account{Login: "marie-curie", ID: 12, Type: github.AccountBot},
	})
	if lookalikePerson.Kind != graphv1.ActorKind_AUTOMATION {
		t.Errorf("an account GitHub types `Bot` whose login reads like a person was classified %v",
			lookalikePerson.Kind)
	}

	// The operator's escape hatch for the first case, which is a statement rather than an inference.
	named := github.ActorPolicy{DeploymentAutomation: []string{"deploy-bot"}}.Classify(github.ActorEvidence{
		Event:   github.EventPush,
		Account: github.Account{Login: "deploy-bot", ID: 11, Type: github.AccountUser},
	})
	if named.Kind != graphv1.ActorKind_AUTOMATION || named.Rung != github.RungAutomationList {
		t.Errorf("a login the operator listed as automation was classified %v at %q",
			named.Kind, named.Rung)
	}
}

// The configured list is an exact comparison. A pattern would be the forbidden inference wearing a
// configuration file as a disguise, and it would catch every account whose name happens to contain a
// listed one.
func TestTheConfiguredListMatchesExactlyRatherThanByShape(t *testing.T) {
	t.Parallel()
	policy := github.ActorPolicy{DeploymentAutomation: []string{" Release-Runner "}}

	// Case and surrounding space are the two ways a hand-maintained file differs from a payload.
	same := policy.Classify(github.ActorEvidence{
		Account: github.Account{Login: "release-runner", ID: 1, Type: github.AccountUser},
	})
	if same.Rung != github.RungAutomationList {
		t.Errorf("a listed login differing only in case and space decided at %q, want the list", same.Rung)
	}

	for _, login := range []string{"release-runner-2", "not-release-runner", "release", "runner"} {
		got := policy.Classify(github.ActorEvidence{
			Account: github.Account{Login: login, ID: 2, Type: github.AccountUser},
		})
		if got.Rung == github.RungAutomationList {
			t.Errorf("%q matched the list entry `release-runner`; a prefix or substring match is the "+
				"login inference SC-005 forbids, wearing a configuration file as a disguise", login)
		}
	}
}

// UNSPECIFIED and UNKNOWN are different statements, and a login with no type is the boundary: the
// payload did record that something acted, even though nothing in it says what.
func TestALoginWithNoTypeIsUnknownRatherThanUnspecified(t *testing.T) {
	t.Parallel()
	var policy github.ActorPolicy
	got := policy.Classify(github.ActorEvidence{Account: github.Account{Login: "somebody"}})
	if got.Kind != graphv1.ActorKind_ACTOR_KIND_UNKNOWN {
		t.Errorf("a payload naming an account with no type was classified %v; something acted, so "+
			"UNSPECIFIED would claim the payload recorded no actor at all (FR-013)", got.Kind)
	}
	if len(got.Evidence) == 0 {
		t.Error("an UNKNOWN came back with no evidence; UNKNOWN without evidence is indistinguishable " +
			"from a feeder that did not look")
	}
}

// The evidence names the fields consulted and never the login's value — an evidence line carrying it
// is an invitation for the next reader to draw the conclusion this file exists to prevent.
func TestTheEvidenceNeverCarriesTheLoginsValue(t *testing.T) {
	t.Parallel()
	var policy github.ActorPolicy
	const login = "deploy-bot"
	got := policy.Classify(github.ActorEvidence{
		Event:   github.EventSchedule,
		Account: github.Account{Login: login, ID: 77, Type: github.AccountUser},
	})
	for _, line := range got.Evidence {
		if strings.Contains(line, login) {
			t.Errorf("the evidence carries the login's value: %q", line)
		}
	}
	// It does carry what decided, so the assertion above is not "the evidence is empty".
	joined := strings.Join(got.Evidence, "; ")
	for _, want := range []string{"event=schedule", "actor.type=User", "actor.id=present"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the evidence %q does not record %q", joined, want)
		}
	}
	// And the login is still available as the actor's identity, which is a different job from typing it.
	if got.Login != login {
		t.Errorf("Actor.Login = %q, want %q; the change needs the identity even though the "+
			"classification must not read it", got.Login, login)
	}
}

// The evidence is sorted, because a golden that reorders is a golden that churns.
func TestTheEvidenceIsSorted(t *testing.T) {
	t.Parallel()
	var policy github.ActorPolicy
	got := policy.Classify(github.ActorEvidence{
		Event:   github.EventPush,
		Account: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
	})
	for i := 1; i < len(got.Evidence); i++ {
		if got.Evidence[i-1] > got.Evidence[i] {
			t.Errorf("the evidence is not sorted: %q before %q", got.Evidence[i-1], got.Evidence[i])
		}
	}
}
