// SPDX-License-Identifier: Apache-2.0

package github

import (
	"slices"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Actor kind, from the payload rather than from the name (004 T058–T060, FR-013, FR-027, SC-005).
//
// ---------------------------------------------------------------------------------------------
// Why the login is never consulted
//
// The implementation everybody writes first is `strings.HasSuffix(login, "[bot]")` ⇒ AUTOMATION, and
// SC-005 has a fixture built to break it in both directions: an organisation's machine user called
// `deploy-bot` that GitHub types as a **user account**, and a person whose login happens to read like a
// service. A login is a display choice; the kind is a fact about what acted, and GitHub states it in
// two fields that are not the login — the run's `event` and the account's `type`.
//
// So the login appears nowhere below, not even in the recorded evidence. It stays off the evidence
// list deliberately: an evidence line naming the login is an invitation for the next reader to draw a
// conclusion from it, which is the conclusion this file exists to prevent. The login still travels on
// the change as the actor's identity, which is a different job from typing it.
//
// # The ladder, stopping at the first hit
//
//  1. the login is on the configured automation list → AUTOMATION
//  2. the trigger is the clock (`schedule`) → CONTROLLER
//  3. the trigger is another system (`repository_dispatch`, `workflow_run`, `workflow_call`) → AUTOMATION
//  4. GitHub types the account `Bot` → AUTOMATION
//  5. GitHub types the account `User` → PERSON
//  6. something acted and none of the above placed it → ACTOR_KIND_UNKNOWN, with the evidence
//  7. the payload said nothing about who or what acted → ACTOR_KIND_UNSPECIFIED
//
// # Why the trigger outranks the account
//
// On a scheduled run GitHub attributes the actor to whoever last touched the workflow file. That
// person did not deploy anything; a cron did, possibly months later. Reading the account first would
// type every nightly rollout as a PERSON and name somebody who was asleep.
//
// # Why AUTOMATION and CONTROLLER are not one kind
//
// The same reason the GCP feeder keeps them apart (internal/feeders/gcp/actorkind.go): a CI principal
// deploying is a candidate **cause**, and something reacting to the state of the world is a candidate
// **effect**. Feature 002's causal ordering exonerates on exactly that difference, and folding them
// into one "robot" kind removes the distinction that stops the engine blaming a reaction for the
// outage it reacted to. `schedule` is CONTROLLER because nothing chose this moment: the clock did.
//
// # UNSPECIFIED is not UNKNOWN
//
// UNSPECIFIED means the payload said nothing and the field is omitted downstream. UNKNOWN means
// something **was** observed and could not be placed. Writing UNKNOWN where nothing was learned turns
// "this payload records no actor" into "somebody did this and we cannot tell who", which is a
// different and more alarming statement (FR-013).

// GitHub's own account types, as the API spells them. `Organization` is in the recognised set so that
// it lands on rung 6 with its evidence rather than falling through as if nothing were stated: an
// organisation is not something that acts, and saying so is more useful than saying nothing.
const (
	AccountUser         = "User"
	AccountBot          = "Bot"
	AccountOrganization = "Organization"
)

// GitHub's trigger events, for the two rungs that turn on them.
const (
	EventSchedule           = "schedule"
	EventRepositoryDispatch = "repository_dispatch"
	EventWorkflowRun        = "workflow_run"
	EventWorkflowCall       = "workflow_call"
	EventPush               = "push"
	EventWorkflowDispatch   = "workflow_dispatch"
)

// systemTriggers are the events where another system asked, rather than a person or the clock.
//
// `workflow_call` is included because a reusable workflow invoked by another workflow is literally a
// workflow triggered by a workflow, which is the case FR-027 names. GitHub usually reports such a run
// under the CALLER's event rather than as its own run, so this rung fires rarely — and a rung that
// fires rarely is still better than one that types the case as a person.
var systemTriggers = []string{EventRepositoryDispatch, EventWorkflowRun, EventWorkflowCall}

// ActorEvidence is what one payload says about who or what acted.
//
// Event is empty for a payload that states no trigger — a deployment and a release each carry only
// the account that created them — and an empty Event skips the two rungs that turn on it rather than
// being treated as an unrecognised one.
type ActorEvidence struct {
	Event   string
	Account Account
}

// Empty reports whether the payload said nothing at all, which is the case that must yield
// UNSPECIFIED rather than UNKNOWN.
//
// The login counts here, and only here: a payload carrying a login and nothing else did record that
// something acted, even though the login cannot say what. That is rung 6, not rung 7.
func (e ActorEvidence) Empty() bool {
	return strings.TrimSpace(e.Event) == "" &&
		strings.TrimSpace(e.Account.Type) == "" &&
		strings.TrimSpace(e.Account.Login) == "" &&
		e.Account.ID == 0
}

// ActorPolicy is the operator's own statement about its own automation.
type ActorPolicy struct {
	// DeploymentAutomation is the logins attributed AUTOMATION whatever GitHub types them.
	//
	// A configured list is not the string inference SC-005 forbids, and the difference is the whole
	// point: a list is the organisation stating which of its accounts are pipelines, and a pattern is
	// this code guessing from a name. The comparison below is exact for that reason — no prefix, no
	// suffix, no substring — because a `*-bot` pattern would be the forbidden inference wearing a
	// configuration file as a disguise.
	DeploymentAutomation []string
}

// Actor is the typed reading of one payload's actor.
type Actor struct {
	Kind graphv1.ActorKind
	// Rung names which step decided, so a reviewer sees why rather than reverse-engineering it.
	Rung string
	// Evidence is the fields consulted and what they showed, sorted. The login is deliberately absent:
	// see the file comment.
	Evidence []string
	// Login is the actor's identity for the change, carried separately from the classification so that
	// the two cannot be confused for one another.
	Login string
}

// The published rungs.
const (
	RungAutomationList  = "configured deployment-automation list"
	RungScheduleTrigger = "workflow_run.event=schedule, so the clock acted and nobody chose the moment"
	RungSystemTrigger   = "workflow_run.event names another system as the trigger"
	RungBotAccount      = "GitHub types the account as a bot, which is what an App acts as"
	RungUserAccount     = "GitHub types the account as a user"
	RungUnclassified    = "something acted and no signal GitHub states placed it"
	RungNoEvidence      = "the payload records no actor"
)

// Classify runs the ladder.
func (p ActorPolicy) Classify(e ActorEvidence) Actor {
	login := strings.TrimSpace(e.Account.Login)
	actor := Actor{Evidence: e.evidence(), Login: login}

	if e.Empty() {
		actor.Kind = graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED
		actor.Rung = RungNoEvidence
		return actor
	}

	event := strings.TrimSpace(e.Event)
	accountType := strings.TrimSpace(e.Account.Type)

	switch {
	// 1. The organisation's own statement, first, so that a machine user GitHub types as a person is
	// still AUTOMATION — the operator knows which of its accounts are pipelines and this code does not.
	case listedLogin(p.DeploymentAutomation, login):
		actor.Kind, actor.Rung = graphv1.ActorKind_AUTOMATION, RungAutomationList

	// 2. The clock. Before the account, because a scheduled run names whoever last touched the
	// workflow file and that person did not deploy anything.
	case event == EventSchedule:
		actor.Kind, actor.Rung = graphv1.ActorKind_CONTROLLER, RungScheduleTrigger

	// 3. Another system asked.
	case slices.Contains(systemTriggers, event):
		actor.Kind, actor.Rung = graphv1.ActorKind_AUTOMATION, RungSystemTrigger

	// 4. A bot account, which is what a GitHub App acts as. Before the user rung, because a bot that
	// pushed is still a bot and the push is not what makes it one.
	case accountType == AccountBot:
		actor.Kind, actor.Rung = graphv1.ActorKind_AUTOMATION, RungBotAccount

	// 5. A user account, with the clock, the other systems and the bots ruled out.
	case accountType == AccountUser:
		actor.Kind, actor.Rung = graphv1.ActorKind_PERSON, RungUserAccount

	// 6. Something acted and nothing GitHub states placed it — an account type nobody has ruled on, an
	// organisation (which does not act), or a trigger with no account at all. The evidence travels
	// with it, because UNKNOWN without evidence is indistinguishable from a feeder that did not look.
	default:
		actor.Kind, actor.Rung = graphv1.ActorKind_ACTOR_KIND_UNKNOWN, RungUnclassified
	}
	return actor
}

// evidence renders the fields consulted, sorted, and never the login.
func (e ActorEvidence) evidence() []string {
	var out []string
	if event := strings.TrimSpace(e.Event); event != "" {
		out = append(out, "event="+event)
	}
	if accountType := strings.TrimSpace(e.Account.Type); accountType != "" {
		out = append(out, "actor.type="+accountType)
	}
	if e.Account.ID != 0 {
		// The numeric id, not the login: it is the account's stable identity across renames, and it
		// says an account was named without saying what it was called.
		out = append(out, "actor.id=present")
	}
	if strings.TrimSpace(e.Account.Login) != "" && strings.TrimSpace(e.Account.Type) == "" {
		// Worth recording only in the case it changes the answer: a login with no type is what makes
		// rung 6 fire rather than rung 7. Still not the login's value.
		out = append(out, "actor.login=present, actor.type=absent")
	}
	slices.Sort(out)
	return out
}

// listedLogin reports whether login appears in the configured list, comparing case-insensitively and
// ignoring surrounding space — the two ways a hand-maintained file differs from an API payload.
// Exact otherwise: see ActorPolicy.DeploymentAutomation.
func listedLogin(list []string, login string) bool {
	if login == "" {
		return false
	}
	target := strings.ToLower(login)
	for _, entry := range list {
		if strings.ToLower(strings.TrimSpace(entry)) == target {
			return true
		}
	}
	return false
}
