// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// One observed deployment becomes changes on the graph (004 US1).
//
// The mapper only composes decisions made elsewhere, so these tests are about the composition: the
// order the filters run in, what reaches the change and what does not, and that an exclusion is
// counted rather than passed over.

func description() feeder.Description {
	return feeder.Description{SourceID: "github:acme", Kind: "github"}
}

func options() github.MapOptions {
	return github.MapOptions{
		Allowlist: github.Allowlist{Environments: []string{"production"}},
		Targets:   targetMap(),
	}
}

// observation is a deployment that rolled out to production, with a run behind it.
func observation() github.Observation {
	return github.Observation{
		Repo:         monorepo,
		RepositoryID: monorepoID,
		Deployment: github.Deployment{
			ID: 4321, SHA: shippedSHA, Ref: "main", Environment: "production",
			CreatedAt: runStarted, Creator: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
		},
		Statuses: []github.DeploymentStatus{
			status(github.StateSuccess, completedAt),
			status(github.StateInProgress, builtAt),
		},
		Run: &github.WorkflowRun{
			ID: 99, Name: "Deploy", RunAttempt: 1, HeadSHA: shippedSHA, Event: github.EventPush,
			Status: "completed", Conclusion: "success", RunStartedAt: runStarted,
			Actor:           github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
			TriggeringActor: github.Account{Login: "ada", ID: 1, Type: github.AccountUser},
			HTMLURL:         "https://github.com/acme/monorepo/actions/runs/99",
			LogsURL:         "https://api.github.com/repos/acme/monorepo/actions/runs/99/logs",
		},
	}
}

func changesOf(events []*graphv1.EventEnvelope) []*graphv1.ObserveChange {
	var out []*graphv1.ObserveChange
	for _, event := range events {
		if change := event.GetObserveChange(); change != nil {
			out = append(out, change)
		}
	}
	return out
}

func correlationsOf(events []*graphv1.EventEnvelope) []*graphv1.CorrelateEntity {
	var out []*graphv1.CorrelateEntity
	for _, event := range events {
		if key := event.GetCorrelateEntity(); key != nil {
			out = append(out, key)
		}
	}
	return out
}

// claimsOf is the other kind, and the assertion that matters about it here is that there are none: a
// deploy identifier is a correlation and the event log refuses it as a claim (004 T148).
func claimsOf(events []*graphv1.EventEnvelope) []*graphv1.IdentityClaim {
	var out []*graphv1.IdentityClaim
	for _, event := range events {
		if claim := event.GetIdentityClaim(); claim != nil {
			out = append(out, claim)
		}
	}
	return out
}

// The whole path: two targets, two changes, each with its keys, its properties and its pointers.
func TestARolloutBecomesOneChangePerTargetWithItsEvidence(t *testing.T) {
	t.Parallel()
	events, excluded, err := github.MapDeployment(description(), observation(), options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	if excluded.Total() != 0 {
		t.Errorf("a production rollout was excluded: %v", excluded.ByReason())
	}

	changes := changesOf(events)
	if len(changes) != 2 {
		t.Fatalf("the rollout became %d change(s), want one per target", len(changes))
	}
	for _, change := range changes {
		switch {
		case change.GetChange().GetKind() != graphv1.ChangeKind_ROLLOUT:
			t.Errorf("kind = %v, want ROLLOUT", change.GetChange().GetKind())
		case !change.GetValidAt().AsTime().Equal(completedAt):
			t.Errorf("ValidAt = %v, want the platform's completion instant %v",
				change.GetValidAt().AsTime(), completedAt)
		case change.GetChange().GetActorKind() != graphv1.ActorKind_PERSON:
			t.Errorf("actor kind = %v, want PERSON for a push by a user account",
				change.GetChange().GetActorKind())
		case len(change.GetTargets()) != 1:
			t.Errorf("a change carries %d targets, want exactly its own", len(change.GetTargets()))
		}
		// The properties the graph could not carry until 004's schema change. Each is a fact about the
		// CHANGE rather than about the service, which is what earns it a place here.
		props := change.GetProps().GetFields()
		if props[github.PropDeploymentEnvironment].GetStringValue() != "production" {
			t.Errorf("the change does not carry its environment: %v", props)
		}
		if props[github.PropActorRung].GetStringValue() != github.RungUserAccount {
			t.Errorf("the change does not carry which rung typed its actor: %v", props)
		}
		// And the pointers, which the projector has always had a column for and a change observation
		// never filled.
		var kinds []string
		for _, pointer := range change.GetPointers() {
			kinds = append(kinds, pointer.GetKind().String())
			if strings.Contains(pointer.GetSelector(), "?") {
				t.Errorf("a pointer selector carries a query: %q", pointer.GetSelector())
			}
		}
		if len(kinds) != 3 {
			t.Errorf("the change carries %v, want the deployment's and the run's SOURCE_LINKs and the "+
				"run's LOG pointer", kinds)
		}
	}

	// Each change gets the one key C8 keys on: the commit, as a CORRELATION and not a claim. Both
	// changes carry the same commit — one deployment, two targets — so as claims one of them would have
	// got it and the other nothing, according to which the projector reached first (004 T148).
	keys := correlationsOf(events)
	if len(keys) != 2 {
		t.Fatalf("the rollout emitted %d correlation keys, want one commit key per change", len(keys))
	}
	namespaces := map[string]int{}
	values := map[string]int{}
	for _, key := range keys {
		namespaces[key.GetKey().GetNamespace()]++
		values[key.GetKey().GetValue()]++
		if key.GetAttributes().GetFields()[github.PropDeploymentEnvironment].GetStringValue() != "production" {
			t.Errorf("a key does not carry the environment C8 compares: %v", key.GetAttributes())
		}
	}
	if namespaces[feeder.NSDeployCommitSHA] != 2 {
		t.Errorf("the key namespaces are %v, want one commit key per change", namespaces)
	}
	if len(values) != 1 {
		t.Errorf("the two changes carry %d distinct commits %v; one deployment ships one commit, and "+
			"that they SHARE it is what makes it a correlation rather than a name", len(values), values)
	}
	if namespaces[feeder.NSGitHubRepo] != 0 {
		t.Errorf("the repository is still minted in its own namespace: %v", namespaces)
	}
	// And no identity claim at all in the deploy vocabulary, which is what the event log refuses.
	for _, claim := range claimsOf(events) {
		if ns := claim.GetClaim().GetNamespace(); graph.IsCorrelationNamespace(ns) {
			t.Errorf("a deploy value was emitted as an identity claim in %q; the event log refuses it "+
				"(internal/log, ReasonCorrelationAsIdentity)", ns)
		}
	}
	for _, change := range changes {
		if got := change.GetProps().GetFields()[github.AttrRepository].GetStringValue(); got != "acme/monorepo" {
			t.Errorf("the change does not carry its repository as a property: %q", got)
		}
	}
}

// The allowlist runs before anything is read, so a preview deployment's status history is never even
// parsed — and the exclusion is counted under the filter that decided.
func TestAnEnvironmentNobodyListedIsExcludedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	obs := observation()
	obs.Deployment.Environment = "preview"
	events, excluded, err := github.MapDeployment(description(), obs, options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("a deployment to an unlisted environment emitted %d events", len(events))
	}
	if excluded.ByReason()[github.ReasonEnvironmentNotAllowed] != 1 {
		t.Errorf("the exclusion is %v, want it credited to the environment filter", excluded.ByReason())
	}
}

// A deploy that did not land emits nothing and says which of the two ways it failed to land, because
// "no status reports a rollout" and "a deploy failed" send an operator to different places.
func TestADeployThatDidNotLandEmitsNothingAndSaysWhy(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		statuses []github.DeploymentStatus
		want     string
	}{
		"a failure": {
			statuses: []github.DeploymentStatus{status(github.StateFailure, completedAt)},
			want:     "the deploy was attempted and did not land: failure",
		},
		"only progress": {
			statuses: []github.DeploymentStatus{status(github.StateQueued, runStarted)},
			want:     "no status reports that anything reached production",
		},
		"deactivated without ever succeeding": {
			statuses: []github.DeploymentStatus{status(github.StateInactive, completedAt)},
			want:     "the deployment was deactivated without ever succeeding",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			obs := observation()
			obs.Statuses = tc.statuses
			events, excluded, err := github.MapDeployment(description(), obs, options(), polledAt)
			if err != nil {
				t.Fatalf("MapDeployment: %v", err)
			}
			if len(events) != 0 {
				t.Errorf("%s emitted %d events", name, len(events))
			}
			if excluded.ByReason()[tc.want] != 1 {
				t.Errorf("%s was excluded as %v, want %q", name, excluded.ByReason(), tc.want)
			}
		})
	}
}

// A deploy nothing maps is emitted unattached and counted, which is a different number from a deploy
// declined: one is a change on the graph waiting for its target, the other is not a change at all.
func TestAnUnmappedDeployIsEmittedUnattachedAndCounted(t *testing.T) {
	t.Parallel()
	obs := observation()
	obs.Repo = github.Repo{Owner: "acme", Name: "unmapped"}
	events, excluded, err := github.MapDeployment(description(), obs, options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	changes := changesOf(events)
	if len(changes) != 1 {
		t.Fatalf("an unmapped deploy became %d changes, want one unattached", len(changes))
	}
	if len(changes[0].GetTargets()) != 0 {
		t.Errorf("the unattached change carries targets %v", changes[0].GetTargets())
	}
	if excluded.Total() != 1 {
		t.Errorf("the unattached emission is counted %d times, want once — a caller's checkpoint has to "+
			"report how many deploys it could not attach", excluded.Total())
	}
}

// A run's triggering actor is who asked for THIS attempt, which on a re-run is not the run's owner.
func TestTheActorIsWhoeverAskedForThisAttempt(t *testing.T) {
	t.Parallel()
	obs := observation()
	obs.Run.RunAttempt = 2
	obs.Run.Actor = github.Account{Login: "ada", ID: 1, Type: github.AccountUser}
	obs.Run.TriggeringActor = github.Account{Login: "release-bot", ID: 9, Type: github.AccountBot}

	events, _, err := github.MapDeployment(description(), obs, options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	change := changesOf(events)[0]
	if change.GetChange().GetActorKind() != graphv1.ActorKind_AUTOMATION {
		t.Errorf("actor kind = %v, want AUTOMATION: the bot asked for this attempt, and the run's owner "+
			"is who owns the run rather than who re-ran it (FR-028)", change.GetChange().GetActorKind())
	}
	if change.GetChange().GetActor() != "release-bot" {
		t.Errorf("actor = %q, want the triggering actor", change.GetChange().GetActor())
	}
	if got := change.GetProps().GetFields()[github.PropRunAttempt].GetNumberValue(); got != 2 {
		t.Errorf("the change records attempt %v, want 2 so a reader comparing two changes that look "+
			"alike can tell them apart", got)
	}
}

// A deployment created through the API has no run, so the actor is its creator and the origin link is
// left empty rather than pointed at a URL this code assembled.
func TestADeploymentWithNoRunUsesItsCreatorAndNoOriginLink(t *testing.T) {
	t.Parallel()
	obs := observation()
	obs.Run = nil
	events, _, err := github.MapDeployment(description(), obs, options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	change := changesOf(events)[0]
	if change.GetChange().GetActorKind() != graphv1.ActorKind_PERSON {
		t.Errorf("actor kind = %v, want PERSON from the deployment's creator",
			change.GetChange().GetActorKind())
	}
	if got := change.GetChange().GetOriginRef(); got != "" {
		t.Errorf("OriginRef = %q; a deployment created through the API has no page GitHub links to, and "+
			"a link this code assembled would send somebody to a URL nobody published", got)
	}
	// It still has the deployment's SOURCE_LINK, so the change is not unreachable.
	if len(change.GetPointers()) != 1 {
		t.Errorf("the change carries %d pointers, want the deployment's SOURCE_LINK", len(change.GetPointers()))
	}
}

// GitHub's own production flag is recorded as evidence beside the operator's list, and absent where
// GitHub stated nothing — absent and false are different facts.
func TestGitHubsProductionFlagIsEvidenceAndNotTheDecision(t *testing.T) {
	t.Parallel()
	stated := false
	obs := observation()
	obs.Deployment.ProductionEnvironment = &stated

	events, _, err := github.MapDeployment(description(), obs, options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	changes := changesOf(events)
	if len(changes) == 0 {
		t.Fatal("GitHub saying `production_environment: false` stopped the rollout; the operator's list " +
			"decides, and the flag is whoever created the deployment stating an opinion (FR-023)")
	}
	field, ok := changes[0].GetProps().GetFields()[github.PropProductionEnvironment]
	if !ok {
		t.Fatal("the flag was not recorded as evidence")
	}
	if field.GetBoolValue() {
		t.Error("the recorded flag is true, and GitHub said false")
	}

	// And where GitHub stated nothing, the property is absent rather than false.
	silent := observation()
	silent.Deployment.ProductionEnvironment = nil
	events, _, err = github.MapDeployment(description(), silent, options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	if _, ok := changesOf(events)[0].GetProps().GetFields()[github.PropProductionEnvironment]; ok {
		t.Error("a flag GitHub did not state was recorded anyway; absent and false are different facts")
	}
}

// A later `inactive` is recorded against the same rollout, as a property, and does not become a second
// change (FR-022).
func TestADeactivationIsAPropertyOfTheSameChange(t *testing.T) {
	t.Parallel()
	supersededAt := completedAt.Add(6 * time.Hour)
	obs := observation()
	obs.Statuses = append([]github.DeploymentStatus{status(github.StateInactive, supersededAt)}, obs.Statuses...)

	events, _, err := github.MapDeployment(description(), obs, options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	changes := changesOf(events)
	if len(changes) != 2 {
		t.Fatalf("a deactivated rollout became %d changes, want the same two (one per target)", len(changes))
	}
	got := changes[0].GetProps().GetFields()[github.PropDeactivatedAt].GetStringValue()
	if !strings.Contains(got, supersededAt.Format("2006-01-02T15:04:05")) {
		t.Errorf("the deactivation instant is %q, want %v recorded against the rollout it is a property "+
			"of (FR-022)", got, supersededAt)
	}
	if !changes[0].GetValidAt().AsTime().Equal(completedAt) {
		t.Errorf("ValidAt = %v, want the success instant; the deactivation is a fact about this rollout "+
			"and not its date", changes[0].GetValidAt().AsTime())
	}
}

// A success the platform did not date leaves the valid start unknown rather than filled in.
func TestASuccessWithNoInstantIsRecordedAsUnknown(t *testing.T) {
	t.Parallel()
	obs := observation()
	obs.Statuses = []github.DeploymentStatus{{State: github.StateSuccess, Environment: "production"}}

	events, _, err := github.MapDeployment(description(), obs, options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	change := changesOf(events)[0]
	if !change.GetValidFromUnknown() {
		t.Error("the valid start is not marked unknown; filling it with the poll instant would date the " +
			"rollout to when we noticed it (T053)")
	}
}

// The same observation maps to the same events twice: nothing reads a clock and no map iteration
// reaches a golden.
func TestTheMappingIsDeterministic(t *testing.T) {
	t.Parallel()
	first, _, err := github.MapDeployment(description(), observation(), options(), polledAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	for range 8 {
		again, _, err := github.MapDeployment(description(), observation(), options(), polledAt)
		if err != nil {
			t.Fatalf("MapDeployment: %v", err)
		}
		if len(again) != len(first) {
			t.Fatalf("two runs emitted %d and %d events", len(first), len(again))
		}
		for i := range first {
			if first[i].GetEventId() != again[i].GetEventId() {
				t.Fatalf("event %d is %q then %q; a golden cannot depend on Go's map iteration order",
					i, first[i].GetEventId(), again[i].GetEventId())
			}
		}
	}
}
