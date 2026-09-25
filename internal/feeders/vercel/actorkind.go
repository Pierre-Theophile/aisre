// SPDX-License-Identifier: Apache-2.0

package vercel

import (
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Actor kind, from the payload rather than from the name (004 T088; FR-013, FR-037, research §3.2).
//
// ---------------------------------------------------------------------------------------------
// Why the login is never consulted
//
// FR-037 says the account type MUST come from the payload and never from the login string, and this
// connector honours it by construction rather than by discipline: `creator.username` and
// `creator.githubLogin` are not decoded at all (see transport.go). They are not available here to be
// misused, which is a stronger guarantee than a comment asking the next reader not to.
//
// The reason is the same one GitHub's ladder documents: a login is a display choice. An organisation's
// machine user may be called `deploy-bot` and be typed as an ordinary user account, and a person's
// handle may read like a service. Typing from the string gets both wrong, confidently.
//
// # The ladder, stopping at the first hit
//
//  1. Vercel types the account as a non-person (`app`, `bot`, `system`) → AUTOMATION
//  2. the trigger is another system (`api-trigger-git-deploy`, `git-deploy-hook`) → AUTOMATION
//  3. the trigger is a redeploy of an existing deployment → AUTOMATION
//  4. Vercel types the account `user` → PERSON
//  5. something acted and none of the above placed it → ACTOR_KIND_UNKNOWN
//  6. the payload said nothing about who or what acted → ACTOR_KIND_UNSPECIFIED
//
// # Why the account type outranks the trigger here, unlike GitHub
//
// GitHub puts the trigger first because a scheduled run is attributed to whoever last edited the
// workflow file — the account is actively misleading there. Vercel has no such attribution quirk: a
// deployment's `creator` is the account that caused it, and `source` says how. So when Vercel states
// the creator is an app, that IS the answer, and `source=git` behind it means a machine pushed, not
// that a person did.
//
// That difference is worth stating rather than quietly copying the other ladder's order. The two
// connectors disagree about precedence because the platforms disagree about what `creator` means.

// Automation trigger sources: another system asked for this deployment.
var automationSources = map[string]bool{
	"api-trigger-git-deploy": true,
	"git-deploy-hook":        true,
	"redeploy":               true,
}

// Non-person account types, as Vercel states them.
var automationAccountTypes = map[string]bool{
	"app":    true,
	"bot":    true,
	"system": true,
}

// ActorKind types what caused a deployment, from `creator.type` and `source` only.
func ActorKind(d Deployment) graphv1.ActorKind {
	accountType := strings.ToLower(strings.TrimSpace(d.Creator.Type))
	source := strings.ToLower(strings.TrimSpace(d.Source))
	hasActor := accountType != "" || source != "" || strings.TrimSpace(d.Creator.UID) != ""

	switch {
	case automationAccountTypes[accountType]:
		return graphv1.ActorKind_AUTOMATION
	case automationSources[source]:
		return graphv1.ActorKind_AUTOMATION
	case accountType == "user":
		return graphv1.ActorKind_PERSON
	case hasActor:
		// Something acted and this ladder cannot place it. UNKNOWN is a statement — "a deployment
		// happened and its cause is untyped" — and it is deliberately not PERSON. Guessing PERSON
		// would put a human in an audit trail on no evidence, which is the worse of the two errors:
		// an investigation that blames a person who did nothing is harder to undo than one that says
		// it does not know.
		return graphv1.ActorKind_ACTOR_KIND_UNKNOWN
	default:
		return graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED
	}
}

// ActorEvidence is what the ladder read, recorded on the change so a reader can check the verdict.
//
// The login is absent, and that is deliberate rather than incidental: an evidence line naming the login
// invites the next reader to draw a conclusion from it, which is the conclusion this file exists to
// prevent. Sorted and fixed-order so a golden does not depend on map iteration.
func ActorEvidence(d Deployment) []string {
	var out []string
	if t := strings.ToLower(strings.TrimSpace(d.Creator.Type)); t != "" {
		out = append(out, "creator.type="+t)
	}
	if s := strings.ToLower(strings.TrimSpace(d.Source)); s != "" {
		out = append(out, "source="+s)
	}
	if strings.TrimSpace(d.Creator.UID) != "" {
		out = append(out, "creator.uid=present")
	}
	return out
}
