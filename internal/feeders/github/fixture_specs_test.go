// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The US1 fixtures, one spec each (004 T073–T079).
//
// Each one exists to make a specific claim falsifiable against a recorded window rather than against a
// struct built in a test. Where a fixture's whole point is that something is NOT emitted, the manifest
// says so in its description — a fixture whose success looks like an empty graph needs a reader to be
// able to tell that from a fixture that failed to load.

func fixtureSet(t *testing.T) []fixtureSpec {
	t.Helper()
	return []fixtureSpec{
		baselineFixture(),
		statusStatesFixture(),
		actorKindsFixture(),
		unattachedFixture(),
		rerunFixture(),
		releaseFixture(),
		// T076/SC-010, committed since 004 T148 made the shuffle pass. See fixture_gen_test.go.
		monorepoFixture(),
		// T079, recorded once the doorbell existed (T039-T042).
		doorbellForgedFixture(),
		partialPollFixture(),
		// T110: the refusal path, carried by hand beside the baseline's recorded stream.
		telemetryRejectionFixture(t),
	}
}

// T073: the baseline window. One repository, one production rollout, resolved from its statuses.
func baselineFixture() fixtureSpec {
	return fixtureSpec{
		dir:    "fixtures/github-deployment-01",
		family: "github",
		description: "One production deployment of one repository, rolled out at the platform's " +
			"completion instant. The deployment's log_url carries a query string with a token in it, " +
			"which is exactly where one arrives in practice: no pointer this feeder mints may carry it " +
			"(FR-014). The run is matched to the deployment by head sha, because GitHub's deployment " +
			"object does not name the run that created it.",
		options: storefrontOptions(),
		payloads: []feeder.Payload{
			scopePayload(fixtureFirst, repository(twinStorefrontID, twinStorefront)),
			runsPayload(fixtureFirst, runSpec{id: 99, name: "Deploy", sha: twinSHA, event: "push"}),
			deploymentsPayload(fixtureFirst, deploymentSpec{
				id: 4321, repo: twinStorefront, sha: twinSHA, environment: "production", production: "true",
			}),
			statusesPayload(fixtureFirst, twinStorefront, 4321, "production",
				statusSpec{id: 2, state: "success", at: "2026-03-01T14:03:12Z",
					logURL: "https://deploy.example/logs?token=SHOULD-NEVER-BE-STORED"},
				statusSpec{id: 1, state: "in_progress", at: "2026-03-01T14:01:30Z"},
			),
			pollMarker(fixturePoll, "complete"),
		},
		queries: queriesYAML(
			subgraphQuery("rollout-2hop", "github.change=repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront",
				statusSucceededAt,
				"The rollout itself, at the instant it happened. Nothing in this fixture describes its\n"+
					"target, so the change stands alone with k8s.deployment=shop/storefront listed as\n"+
					"unattached (FR-028): kept, not dropped, and linked when the target shows up. \"What\n"+
					"changed on storefront?\" is a diff focused on the service, which\n"+
					"deploy-cross-source-merge-01 asks (T152)."),
			fixtureQuery{
				name: "rollout-pointers", kind: "pointers",
				focus:   "github.change=repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront",
				validAt: statusSucceededAt,
				comment: "The pointers the change carries: the deployment's and the run's SOURCE_LINKs\n" +
					"and the run's LOG pointer — none of them carrying the token the status payload\n" +
					"put in its log_url (FR-014).",
			},
			extentQuery(),
		),
	}
}

// T074: all seven documented statuses. GitHub designates no terminal state, so this is where the
// published table is exercised against a window rather than against a map literal.
func statusStatesFixture() fixtureSpec {
	const env = "production"
	return fixtureSpec{
		dir:    "fixtures/github-status-states-01",
		family: "github",
		description: "All seven documented deployment statuses across six deployments, plus one state " +
			"the published table has not ruled on. Only `success` becomes a ROLLOUT: `queued`, " +
			"`in_progress` and `pending` produce nothing because nothing in production moved, " +
			"`failure` and `error` produce a failed attempt and never a rollout, a later `inactive` is " +
			"a property of the rollout it followed, and the unrecognised state is counted under its " +
			"own spelling and acted on not at all. A mostly-empty graph is the CORRECT outcome here: " +
			"four of the five deployments are deliberately not changes. The success with no INSTANT " +
			"lives in github-unknown-start-01, which is generated on demand rather than committed — " +
			"see the note in fixture_gen_test.go.",
		options: storefrontOptions(),
		payloads: []feeder.Payload{
			scopePayload(fixtureFirst, repository(twinStorefrontID, twinStorefront)),
			deploymentsPayload(fixtureFirst,
				deploymentSpec{id: 1, repo: twinStorefront, sha: twinSHA, environment: env},
				deploymentSpec{id: 2, repo: twinStorefront, sha: twinSHA, environment: env},
				deploymentSpec{id: 3, repo: twinStorefront, sha: twinSHA, environment: env},
				deploymentSpec{id: 4, repo: twinStorefront, sha: twinSHA, environment: env},
				deploymentSpec{id: 5, repo: twinStorefront, sha: twinSHA, environment: env},
			),
			// 1: the rollout, later superseded. The `inactive` is a property of it, not a second change.
			statusesPayload(fixtureFirst, twinStorefront, 1, env,
				statusSpec{id: 12, state: "inactive", at: "2026-03-01T20:00:00Z"},
				statusSpec{id: 11, state: "success", at: "2026-03-01T14:03:12Z"},
			),
			// 2: progress only. Nothing in production has moved.
			statusesPayload(fixtureFirst, twinStorefront, 2, env,
				statusSpec{id: 22, state: "in_progress", at: "2026-03-01T14:02:00Z"},
				statusSpec{id: 21, state: "queued", at: "2026-03-01T14:01:00Z"},
			),
			// 3: pending, which is also nothing.
			statusesPayload(fixtureFirst, twinStorefront, 3, env,
				statusSpec{id: 31, state: "pending", at: "2026-03-01T14:01:00Z"}),
			// 4: a failure. A fact worth recording and never a ROLLOUT.
			statusesPayload(fixtureFirst, twinStorefront, 4, env,
				statusSpec{id: 41, state: "failure", at: "2026-03-01T14:04:00Z"}),
			// 5: an error, which GitHub distinguishes from a failure by cause.
			statusesPayload(fixtureFirst, twinStorefront, 5, env,
				statusSpec{id: 51, state: "error", at: "2026-03-01T14:05:00Z"}),
			pollMarker(fixturePoll, "complete"),
		},
		queries: queriesYAML(
			subgraphQuery("the-one-rollout", "github.change=repositories/555/deployments/1/targets/k8s.deployment/shop/storefront",
				statusSucceededAt,
				"Exactly one change: of the five deployments, four are deliberately not rollouts.\n"+
					"A nearly empty graph IS the assertion here."),
			extentQuery(),
		),
	}
}

// T075: the look-alike logins. The whole point is that the kind comes from the payload and never from
// the name.
func actorKindsFixture() fixtureSpec {
	const env = "production"
	return fixtureSpec{
		dir:    "fixtures/github-actor-kinds-01",
		family: "github",
		description: "Four rollouts whose actor kinds must come from the payload and never from the " +
			"login (SC-005). A machine user called `deploy-bot` that GitHub types as a USER account is " +
			"a PERSON; a person whose login reads like one, `marie-curie`, typed as a BOT is " +
			"AUTOMATION; a scheduled run is a CONTROLLER whatever its actor says, because GitHub " +
			"attributes a scheduled run to whoever last edited the workflow file and that person did " +
			"not deploy anything; and a login on the operator's automation list is AUTOMATION whatever " +
			"GitHub types it. Each change records which rung decided and on what evidence — and the " +
			"evidence never carries the login's value.",
		options: storefrontOptions(),
		payloads: []feeder.Payload{
			scopePayload(fixtureFirst, repository(twinStorefrontID, twinStorefront)),
			runsPayload(fixtureFirst,
				runSpec{id: 101, name: "Deploy", sha: "a" + twinSHA[1:], event: "push",
					actor: `{"login":"deploy-bot","id":11,"type":"User"}`},
				runSpec{id: 102, name: "Deploy", sha: "b" + twinSHA[1:], event: "push",
					actor: `{"login":"marie-curie","id":12,"type":"Bot"}`},
				runSpec{id: 103, name: "Deploy", sha: "c" + twinSHA[1:], event: "schedule",
					actor: `{"login":"ada","id":1,"type":"User"}`},
				runSpec{id: 104, name: "Deploy", sha: "d" + twinSHA[1:], event: "push",
					actor: `{"login":"release-runner","id":4,"type":"User"}`},
			),
			deploymentsPayload(fixtureFirst,
				deploymentSpec{id: 201, repo: twinStorefront, sha: "a" + twinSHA[1:], environment: env},
				deploymentSpec{id: 202, repo: twinStorefront, sha: "b" + twinSHA[1:], environment: env},
				deploymentSpec{id: 203, repo: twinStorefront, sha: "c" + twinSHA[1:], environment: env},
				deploymentSpec{id: 204, repo: twinStorefront, sha: "d" + twinSHA[1:], environment: env},
			),
			statusesPayload(fixtureFirst, twinStorefront, 201, env,
				statusSpec{id: 1, state: "success", at: "2026-03-01T14:03:00Z"}),
			statusesPayload(fixtureFirst, twinStorefront, 202, env,
				statusSpec{id: 2, state: "success", at: "2026-03-01T14:04:00Z"}),
			statusesPayload(fixtureFirst, twinStorefront, 203, env,
				statusSpec{id: 3, state: "success", at: "2026-03-01T14:05:00Z"}),
			statusesPayload(fixtureFirst, twinStorefront, 204, env,
				statusSpec{id: 4, state: "success", at: "2026-03-01T14:06:00Z"}),
			pollMarker(fixturePoll, "complete"),
		},
		queries: queriesYAML(
			subgraphQuery("machine-user-is-a-person", "github.change=repositories/555/deployments/201/targets/k8s.deployment/shop/storefront",
				"2026-03-01T14:03:00Z",
				"Four changes on one service, each with a different actor kind, so a golden that\n"+
					"mistyped any of them differs visibly."),
			extentQuery(),
		),
	}
}

// The unknown-start fixture, generated on demand and NOT committed. See fixture_gen_test.go's note.
func unknownStartFixture() fixtureSpec {
	const env = "production"
	return fixtureSpec{
		dir:    "fixtures/github-unknown-start-01",
		family: "github",
		description: "A deployment GitHub reports as SUCCEEDED without stating when, so its valid start " +
			"is recorded as unknown rather than filled in with the poll instant (T053, FR-012). Also " +
			"carries a status state the published table has not ruled on, counted under its own " +
			"spelling. Generated on demand rather than committed: it passes every verification step " +
			"but the shuffle, and for a reason that is a property of the model rather than a defect — " +
			"a change with an unknown valid start begins at the OBSERVATION, and the observation " +
			"instant is exactly what reordering changes.",
		options: storefrontOptions(),
		payloads: []feeder.Payload{
			scopePayload(fixtureFirst, repository(twinStorefrontID, twinStorefront)),
			deploymentsPayload(fixtureFirst, deploymentSpec{
				id: 6, repo: twinStorefront, sha: twinOtherSHA, environment: env,
			}),
			statusesPayload(fixtureFirst, twinStorefront, 6, env,
				statusSpec{id: 62, state: "rolled_back", at: "2026-03-01T14:07:00Z"},
				statusSpec{id: 61, state: "success", at: ""},
			),
			pollMarker(fixturePoll, "complete"),
		},
		queries: queriesYAML(
			// The one query here whose valid instant cannot be a literal: this change's start is
			// UNKNOWN, so it begins at the observation, and the observation instant is assigned by
			// the arrival clock rather than written down. The clock's end stands in. Nothing is
			// frozen on it either way — this fixture is generated on demand and has no recorded
			// goldens, so T151's empty-golden gate never runs over it.
			subgraphQuery("unknown-start",
				"github.change=repositories/555/deployments/6/targets/k8s.deployment/shop/storefront",
				"",
				"The change exists, its start is marked unknown, and the unrecognised state is counted\n"+
					"under its own spelling."),
			extentQuery(),
		),
	}
}

// T076: one run, three targets. FR-017 and SC-010 both turn on this being three changes.
func monorepoFixture() fixtureSpec {
	return fixtureSpec{
		dir:    "fixtures/github-monorepo-01",
		family: "github",
		description: "One deployment of a monorepo that the operator's mapping resolves to THREE " +
			"services, which is three changes rather than one change with three targets (FR-017, " +
			"SC-010). An investigation asking what changed on checkout must get an answer about " +
			"checkout; one node at hop 0 of three unrelated services would rank three times for three " +
			"different onsets. The three share one origin reference and one deploy.commit_sha, which " +
			"is what keeps them recognisable as one pipeline run and lets C8 merge each with the " +
			"platform feeder's own observation separately. The targets come from the operator's " +
			"mapping and never from a file in the observed repository, which this connector cannot " +
			"read at all: `contents` is not on its published surface.",
		options: monorepoOptions(),
		payloads: []feeder.Payload{
			scopePayload(fixtureFirst, repository(twinMonorepoID, twinMonorepo)),
			runsPayload(fixtureFirst, runSpec{
				id: 301, name: "Deploy search", sha: twinSHA, event: "workflow_dispatch",
			}),
			deploymentsPayload(fixtureFirst, deploymentSpec{
				id: 4400, repo: twinMonorepo, sha: twinSHA, environment: "production", production: "true",
			}),
			statusesPayload(fixtureFirst, twinMonorepo, 4400, "production",
				statusSpec{id: 1, state: "success", at: "2026-03-01T14:03:12Z"}),
			pollMarker(fixturePoll, "complete"),
		},
		queries: queriesYAML(
			subgraphQuery("checkout-2hop", "github.change=repositories/556/deployments/4400/targets/k8s.deployment/shop/checkout",
				statusSucceededAt,
				"Each of the three services separately: an investigation asking what changed on\n"+
					"checkout must get an answer about checkout, and one node at hop 0 of three\n"+
					"services would rank three times for three different onsets."),
			subgraphQuery("catalogue-2hop", "github.change=repositories/556/deployments/4400/targets/k8s.deployment/shop/catalogue",
				statusSucceededAt, ""),
			subgraphQuery("search-2hop", "github.change=repositories/556/deployments/4400/targets/k8s.deployment/shop/search",
				statusSucceededAt, ""),
			extentQuery(),
		),
	}
}

// T077: a deploy whose target appears later. Edge case 1, and the pending queue's reason to exist.
func unattachedFixture() fixtureSpec {
	return fixtureSpec{
		dir:    "fixtures/github-unattached-01",
		family: "github",
		description: "A production deployment of a repository the operator's mapping does not cover, " +
			"emitted as ONE UNATTACHED change rather than dropped (Edge case 1). A deploy that " +
			"happened is a fact whether or not this connector can say what it touched, and a target " +
			"that appears later attaches through the projector's pending queue rather than requiring " +
			"the change to be re-observed. The change carries its commit claim, so the attachment can " +
			"happen on the identifier rather than on a name.",
		options: func() github.MapOptions {
			opts := storefrontOptions()
			// The grant carries the repository; the target mapping deliberately does not.
			delete(opts.Targets.Repositories, twinOwner+"/"+twinStorefront)
			return opts
		}(),
		payloads: []feeder.Payload{
			scopePayload(fixtureFirst, repository(twinStorefrontID, twinStorefront)),
			deploymentsPayload(fixtureFirst, deploymentSpec{
				id: 4500, repo: twinStorefront, sha: twinSHA, environment: "production",
			}),
			statusesPayload(fixtureFirst, twinStorefront, 4500, "production",
				statusSpec{id: 1, state: "success", at: "2026-03-01T14:03:12Z"}),
			pollMarker(fixturePoll, "complete"),
		},
		queries: queriesYAML(
			fixtureQuery{
				name: "unattached-change", kind: "subgraph", hops: 1,
				focus:   "github.change=repositories/555/deployments/4500",
				validAt: statusSucceededAt,
				comment: "The change exists and attaches to nothing. Focused on the change itself,\n" +
					"because there is no target to look at it from — which is Edge case 1.",
			},
			extentQuery(),
		),
	}
}

// T078a: a re-run. FR-028 and Edge case 3: a new change, and the earlier one is not amended.
func rerunFixture() fixtureSpec {
	const env = "production"
	return fixtureSpec{
		dir:    "fixtures/github-rerun-01",
		family: "github",
		description: "A deployment that failed, and the re-run of the same workflow that succeeded. " +
			"The two are keyed on (run, attempt, target), so the second is a NEW change and the first " +
			"is neither amended nor retracted (FR-028, Edge case 3): somebody pressed the button " +
			"again, production moved again, and one key for both would retract a rollout that really " +
			"happened. The failed attempt produces NO rollout, and the re-run's actor is whoever asked " +
			"for THAT attempt rather than whoever owns the run. Two polls, so the second change " +
			"arrives in a later window than the first.",
		options: storefrontOptions(),
		// Half an hour past the second poll's marker, which arrives AT `fixtureEnd`: a window closing
		// on that instant left the marker's events two microseconds outside it (004 T153).
		end: fixtureEnd.Add(30 * time.Minute),
		payloads: []feeder.Payload{
			scopePayload(fixtureFirst, repository(twinStorefrontID, twinStorefront)),
			runsPayload(fixtureFirst, runSpec{
				id: 500, name: "Deploy", attempt: 1, sha: twinSHA, event: "workflow_dispatch",
			}),
			deploymentsPayload(fixtureFirst, deploymentSpec{
				id: 4600, repo: twinStorefront, sha: twinSHA, environment: env,
			}),
			statusesPayload(fixtureFirst, twinStorefront, 4600, env,
				statusSpec{id: 1, state: "failure", at: "2026-03-01T14:04:00Z"}),
			pollMarker(fixturePoll, "complete"),

			// The re-run: attempt 2, a new deployment object, a different person pressing the button.
			runsPayload(fixtureSecond, runSpec{
				id: 500, name: "Deploy", attempt: 2, sha: twinSHA, event: "workflow_dispatch",
				actor:   `{"login":"ada","id":1,"type":"User"}`,
				trigger: `{"login":"release-runner","id":4,"type":"User"}`,
			}),
			deploymentsPayload(fixtureSecond, deploymentSpec{
				id: 4601, repo: twinStorefront, sha: twinSHA, environment: env,
			}),
			statusesPayload(fixtureSecond, twinStorefront, 4601, env,
				statusSpec{id: 2, state: "success", at: "2026-03-01T15:03:00Z"}),
			pollMarker(fixtureEnd, "complete"),
		},
		queries: queriesYAML(
			subgraphQuery("the-rerun", "github.change=repositories/555/deployments/4601/targets/k8s.deployment/shop/storefront",
				"2026-03-01T15:03:00Z",
				"One change, from the re-run: the first attempt failed and produced none."),
			// The failed attempt has NO query, and that is a limit of the harness rather than a gap in
			// the fixture: its expected answer is "no such entity", and `fixture record` treats a
			// not_found as an error rather than recording absence. So the claim is carried by this
			// fixture's single-change golden — one change from two attempts — and by the unit test
			// that asserts a failed deploy produces none.
			extentQuery(),
		),
	}
}

// T078b: releases. FR-025 and Edge case 10 — a release that is not a rollout is never dropped.
func releaseFixture() fixtureSpec {
	return fixtureSpec{
		dir:    "fixtures/github-release-01",
		family: "github",
		description: "Three releases of a repository the operator has NOT declared as shipping by " +
			"release, plus one draft. The published ones become changes of the taxonomy's `other` kind " +
			"carrying GitHub's own object kind as a property — never dropped (Edge case 10), because " +
			"an operator asking what happened around 03:14 is asking about their estate rather than " +
			"about this connector's opinion of which events are interesting, and a release published " +
			"minutes before an incident is exactly what they want to see even when it shipped nothing. " +
			"The draft is not a change at all: nothing has been published, so nothing has happened. " +
			"One release's target_commitish is a BRANCH NAME, which the commit normaliser refuses — " +
			"omitted rather than recorded as a commit it is not.",
		options: storefrontOptions(),
		payloads: []feeder.Payload{
			scopePayload(fixtureFirst, repository(twinStorefrontID, twinStorefront)),
			releasesPayload(fixtureFirst, twinStorefront,
				release(7001, "v1.2.3", twinSHA, "2026-03-01T14:05:00Z", false, false),
				release(7002, "v1.3.0-rc1", twinOtherSHA, "2026-03-01T14:10:00Z", false, true),
				release(7003, "v1.3.0", "main", "2026-03-01T14:15:00Z", false, false),
				release(7004, "v1.4.0", twinSHA, "2026-03-01T14:20:00Z", true, false),
			),
			pollMarker(fixturePoll, "complete"),
		},
		queries: queriesYAML(
			subgraphQuery("first-release", "github.change=repositories/555/releases/7001",
				"2026-03-01T14:05:00Z",
				"Three changes of the `other` kind and no fourth: the draft is not a change."),
			extentQuery(),
		),
	}
}

// The specs above are data, and a typo in one is a fixture that proves something other than what it
// says. This asserts the set is coherent before anything is written to disk.
func TestTheFixtureSetIsCoherent(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, fx := range fixtureSet(t) {
		switch {
		case fx.dir == "":
			t.Errorf("a fixture spec has no directory: %+v", fx)
		case seen[fx.dir]:
			t.Errorf("%s appears twice; the second would overwrite the first", fx.dir)
		// The refusal fixture is a GitHub recording under the deploy family's name, because what it
		// asserts is the event log's rule rather than anything this feeder decides (T110).
		case fx.family != "github" && fx.dir != "fixtures/deploy-telemetry-rejection-01":
			t.Errorf("%s declares the family %q", fx.dir, fx.family)
		case len(fx.payloads) == 0:
			t.Errorf("%s has no payloads, so it would verify nothing", fx.dir)
		case fx.description == "":
			t.Errorf("%s has no description; several of these fixtures succeed by emitting very "+
				"little, and a reader cannot tell that from a fixture that failed to load", fx.dir)
		}
		seen[fx.dir] = true

		// Every fixture ends with a poll marker: the marker is what releases held deployments, and a
		// fixture without one records the reads and none of the changes.
		last := fx.payloads[len(fx.payloads)-1]
		if last.Kind != github.PayloadPollMarker {
			t.Errorf("%s ends with a %q payload, not a poll marker; held deployments would never be "+
				"released and the fixture would record no change at all", fx.dir, last.Kind)
		}
		for i, payload := range fx.payloads {
			if payload.At.IsZero() {
				t.Errorf("%s payload %d (%s) has no arrival instant; observed time tracks arrival, and "+
					"a zero one would put the recording in 1970", fx.dir, i, payload.Kind)
			}
			if len(payload.Bytes) == 0 {
				t.Errorf("%s payload %d (%s) is empty", fx.dir, i, payload.Kind)
			}
		}
	}
	// Ten: the seven of T073-T078, the monorepo one T148 let in, the two T079 asks for — the forged
	// doorbell and the partial poll — and T110's telemetry refusal. The unknown-start case is NOT among
	// them; it is generated on demand for the reason recorded in fixture_gen_test.go. The number is
	// asserted so that adding a fixture is a deliberate act rather than something a generator run does
	// quietly.
	if len(seen) != 10 {
		t.Errorf("the set has %d committed fixtures, want ten", len(seen))
	}
}

// Each fixture runs clean through the feeder before it is ever written: a generator that wrote a
// fixture the feeder refuses would commit a corpus nothing can replay.
func TestEveryFixtureRunsCleanThroughTheFeeder(t *testing.T) {
	t.Parallel()
	for _, fx := range fixtureSet(t) {
		t.Run(fx.dir, func(t *testing.T) {
			t.Parallel()
			f, err := github.New(github.Options{OrgSlug: twinOrg, Map: fx.options})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			em := &capturingEmitter{}
			if err := f.Run(t.Context(), &payloadSource{payloads: fx.payloads}, em); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if em.checkpoints == 0 {
				t.Error("the run wrote no checkpoint, so a consumer cannot tell the window was read")
			}
			// Every event's namespaces are declared, which is what the conformance harness checks.
			declared := map[string]bool{}
			for _, ns := range f.Describe().Namespaces {
				declared[ns] = true
			}
			for _, event := range em.events {
				for _, ref := range refsOf(event) {
					if ref.GetNamespace() != "" && !declared[ref.GetNamespace()] {
						t.Errorf("%s emits the undeclared namespace %q", event.GetEventId(), ref.GetNamespace())
					}
				}
			}
			t.Logf("%d events, %d checkpoints, deferred=%d, excluded=%v",
				len(em.events), em.checkpoints, f.Deferred(), f.Excluded())
		})
	}
}

// The window each fixture declares has to cover the instants inside it, or `fixture verify` compares a
// graph against an extent that excludes half of it.
func TestEveryFixtureDeclaresAWindowCoveringItsPayloads(t *testing.T) {
	t.Parallel()
	for _, fx := range fixtureSet(t) {
		for i, payload := range fx.payloads {
			if payload.At.Before(fixtureStart) {
				t.Errorf("%s payload %d arrives at %v, before the declared start %v",
					fx.dir, i, payload.At, fixtureStart)
			}
			if payload.At.After(fx.clockEnd()) {
				t.Errorf("%s payload %d arrives at %v, after the declared end %v",
					fx.dir, i, payload.At, fx.clockEnd())
			}
		}
	}
}

var _ = time.Time{}

// --- the manifest's queries, built rather than spelled out -----------------------------------------
//
// # Why every query focuses on the CHANGE and not on the service it changed
//
// A GitHub-only replay from an empty graph produces changes whose targets do not exist, and that is
// correct rather than a defect: the services belong to the platform connectors — a Cloud Run service
// is the GCP feeder's node, a Deployment is the Kubernetes feeder's — and this connector creates
// neither. FR-030 is the same rule for repositories. So from empty there is no `k8s.deployment` node
// to focus a query on, and asking for one is `not_found`.
//
// That makes `github-unattached-01` a subtler fixture than its name suggests, and the difference is
// worth stating: every fixture here is unattached in the sense that its target node is absent, but
// only that one has a change carrying **no target reference at all**. `targets: []` means "a deploy
// happened and we cannot say what it touched"; `targets: [k8s.deployment=shop/storefront]` with no such
// node means "we know what it touched and the graph has not met it yet". The projector's pending queue
// resolves the second when the platform feeder arrives; nothing can resolve the first but configuration.
//
// Demonstrating the ATTACHED case needs a cross-source window, which is SC-004's fixture rather than
// one of these.
//
// A manifest query is YAML, and a hand-written YAML block inside a Go string is two languages in one
// literal with no compiler checking either. These builders make the shape a value: a typo in a field
// name stops compiling, and every fixture's queries are pinned to the same instant without that
// instant being written out seven times.

// pinnedAt is the instant every query is asked at: the end of the twins' clock. Pinned, so a golden
// does not encode the day it was recorded.
const pinnedAt = "2026-03-01T15:30:00Z"

// statusSucceededAt is the instant most of these twins report a deployment succeeding, and therefore the
// valid instant of the change it produces. Named because it is a change's own microsecond rather than a
// round number anyone can re-derive: asking one microsecond either side answers nothing (004 T150).
const statusSucceededAt = "2026-03-01T14:03:12Z"

// A query in a generated manifest. The two instants are SEPARATE fields, and the reason is a trap this
// file walked into once.
//
// 004 T150 found eight deploy goldens answering with nothing, because they asked about the graph at the
// clock's end while a change's valid interval is one MICROSECOND (`changeInstant`) — no instant but the
// change's own contains it. The fix set `valid_at` to each change's own instant. But the fix was applied
// to the committed manifests BY HAND, and this generator still emitted one instant for both fields, so
// the next regeneration silently reverted all eight. That is the shape the corpus exists to catch, and
// it hid here, in the thing doing the catching.
//
// So the instants are not one value. `validAt` is when the thing happened — a change's own microsecond,
// which the caller must state, because a default is exactly what went wrong. `observedAt` is when we had
// learned it, which is the clock's end for every query here: everything a fixture records is known by
// then, and asking as-known-at the same microsecond the change happened answers nothing, since the
// events are observed at their arrival instants.
//
// Neither instant is written for a `history`, which is the whole of observed time and takes none.
type fixtureQuery struct {
	name  string
	kind  string
	focus string
	hops  int
	// validAt is the instant asked about. Empty means the clock's end, which is right for a query about
	// an entity that persists (a service, the extent) and wrong for one about a change.
	validAt string
	// observedAt is the instant it is asked AS KNOWN AT. Empty means the clock's end: by then the whole
	// recording has arrived, which is what a golden should be frozen against.
	observedAt string
	comment    string
}

func (q fixtureQuery) yaml() string {
	validAt := q.validAt
	if validAt == "" {
		validAt = pinnedAt
	}
	observedAt := q.observedAt
	if observedAt == "" {
		observedAt = pinnedAt
	}
	var b strings.Builder
	if q.comment != "" {
		for _, line := range strings.Split(q.comment, "\n") {
			fmt.Fprintf(&b, "  # %s\n", line)
		}
	}
	fmt.Fprintf(&b, "  - name: %s\n    kind: %s\n", q.name, q.kind)
	if q.focus != "" {
		fmt.Fprintf(&b, "    focus: %s\n", q.focus)
	}
	// A history takes NO instant: it is the whole of observed time, and `NodeHistoryRequestFromQuery`
	// refuses a `valid_at` outright (FR-032). Writing one here produced a manifest the recorder would
	// not run.
	if q.kind != "history" {
		fmt.Fprintf(&b, "    valid_at: %s\n    observed_at: %s\n", validAt, observedAt)
	}
	if q.hops > 0 {
		fmt.Fprintf(&b, "    hops: %d\n    direction: both\n", q.hops)
	}
	return b.String()
}

func queriesYAML(queries ...fixtureQuery) string {
	var b strings.Builder
	b.WriteString("queries:\n")
	for _, q := range queries {
		b.WriteString(q.yaml())
	}
	return b.String()
}

// subgraphQuery is the shape almost every fixture asks: what does the graph hold around this entity.
//
// `validAt` is a positional argument rather than an option, so that omitting it does not compile. Every
// one of these is focused on a CHANGE, whose valid interval is one microsecond, so the instant is the
// whole question — a helper that defaulted it wrote eight goldens asserting nothing (004 T150).
func subgraphQuery(name, focus, validAt, comment string) fixtureQuery {
	return fixtureQuery{name: name, kind: "subgraph", focus: focus, hops: 2, validAt: validAt, comment: comment}
}

// historyQuery asks how many VERSIONS an entity has and over which intervals, which is the only
// question here that can tell one change from two written at the same ref. A subgraph cannot: it answers
// at one instant, so a second version written by a second reading, or a change invented at a ref the
// query is not focused on, both leave it looking exactly the same. It takes no instant — a history is
// the whole record — so there is nothing to get wrong in the way T150 did.
func historyQuery(name, focus, comment string) fixtureQuery {
	return fixtureQuery{name: name, kind: "history", focus: focus, comment: comment}
}

// extentQuery states the window and the scope, because "not present" and "not read at that time" are
// different answers.
func extentQuery() fixtureQuery {
	return fixtureQuery{
		name: "extent", kind: "extent",
		comment: "The extent: `not present` and `not read at that time` are different answers.",
	}
}

// T079/T041: what a forged or replayed doorbell costs — one extra poll, and nothing else.
//
// The doorbell's body reaching nothing is enforced STRUCTURALLY rather than by this fixture:
// `Doorbell.Ring` takes an int, `verify` returns a bool, and the package has no type a webhook payload
// could be decoded into, so there is nowhere for a body to go. A fixture can only observe that
// indirectly, and a fixture that claimed to prove it would be claiming more than it can see.
//
// The other half — that an extra poll over the same window creates, alters and retracts nothing — was
// what this fixture was built for, and a probe showed a fixture cannot carry it either. Making the
// second poll report a DIFFERENT success instant at the same deployment, regenerating, and re-verifying
// left all four steps green: a replay applies a recorded event log, the log deduplicates on event id,
// and so a second event at the same ref is a DUPLICATE_NOOP before anything compares its contents. A
// change invented at a different ref would be missed for the opposite reason — it is in no golden's
// focus — and `fixture verify` never re-runs the mapper (T145), so no step can see either.
//
// So this fixture pins what it can, which is the rollout and its single version, and SC-012's "no node,
// no change and no event" is asserted by `TestTwoReadingsOfOneWindowProduceOneChange` in
// doorbell_test.go, over the emitted events rather than the replayed ones. The fixture and that test
// are a pair; neither is sufficient alone.
//
// It is the same shape as `gcp-forged-doorbell-01`, deliberately: one estate, two connectors, one
// property, so the two can be compared by eye.
func doorbellForgedFixture() fixtureSpec {
	const env = "production"
	return fixtureSpec{
		dir:    "fixtures/github-doorbell-forged-01",
		family: "github",
		description: "What a forged or replayed doorbell costs: one extra poll, and nothing else. The " +
			"same production rollout is read by two polls — the scheduled one, and the early one a " +
			"forged notification would trigger — and the recording holds ONE rollout with ONE version, " +
			"which the history query pins. Two things this fixture does NOT prove, stated here because " +
			"a fixture that overstated them would be worse than no fixture. It cannot prove that the " +
			"extra poll wrote nothing: a replay applies a recorded log that deduplicates on event id, " +
			"so a second reading is a DUPLICATE_NOOP before its contents are compared, and a probe " +
			"that made the second poll report something different at the same deployment left every " +
			"verification step green. That claim is carried by " +
			"`TestTwoReadingsOfOneWindowProduceOneChange`, over the emitted events, where the mapper " +
			"actually runs. Nor does it prove the doorbell's BODY reaches nothing: that is enforced by " +
			"`Doorbell.Ring` taking an int and the package having no type a payload could be decoded " +
			"into. Polling is the source of truth and this corpus runs with the doorbell absent, which " +
			"is why no payload here is a notification. Synthetic " +
			"structural twin: no identifier is derived from any organisation " +
			"(contracts/sanitisation.md §7), and constitution VIII says synthetic-only data is not " +
			"enough for a connector to be marked stable — a recording campaign replaces it by swapping " +
			"payloads/, not by changing code.",
		options: storefrontOptions(),
		payloads: []feeder.Payload{
			// The scheduled poll.
			scopePayload(fixtureFirst, repository(twinStorefrontID, twinStorefront)),
			deploymentsPayload(fixtureFirst,
				deploymentSpec{id: 4321, repo: twinStorefront, sha: twinSHA, environment: env,
					production: "true"}),
			statusesPayload(fixtureFirst, twinStorefront, 4321, env,
				statusSpec{id: 2, state: "success", at: "2026-03-01T14:03:12Z"},
				statusSpec{id: 1, state: "in_progress", at: "2026-03-01T14:01:30Z"},
			),
			runsPayload(fixtureFirst, runSpec{id: 99, name: "Deploy", sha: twinSHA, event: "push"}),
			pollMarker(fixturePoll, "complete"),
			// The early poll a ring would trigger: the same window, read again, byte for byte. Every
			// event it produces carries the same deterministic id, so the log answers DUPLICATE_NOOP.
			scopePayload(fixtureSecond, repository(twinStorefrontID, twinStorefront)),
			deploymentsPayload(fixtureSecond,
				deploymentSpec{id: 4321, repo: twinStorefront, sha: twinSHA, environment: env,
					production: "true"}),
			statusesPayload(fixtureSecond, twinStorefront, 4321, env,
				statusSpec{id: 2, state: "success", at: "2026-03-01T14:03:12Z"},
				statusSpec{id: 1, state: "in_progress", at: "2026-03-01T14:01:30Z"},
			),
			runsPayload(fixtureSecond, runSpec{id: 99, name: "Deploy", sha: twinSHA, event: "push"}),
			pollMarker(fixtureSecond, "complete"),
		},
		queries: queriesYAML(
			fixtureQuery{
				name: "one-rollout-after-two-polls", kind: "subgraph", hops: 2,
				focus:   "github.change=repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront",
				validAt: statusSucceededAt,
				comment: "The rollout, at the change's own instant — a change's valid interval is one\n" +
					"microsecond and no other instant contains it (T150). This says the rollout is\n" +
					"THERE; it cannot say there is only one of it, which is what the history below is\n" +
					"for.",
			},
			historyQuery("one-rollout-after-two-polls",
				"github.change=repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront",
				"The fixture's actual claim, and the only query here that can carry it: ONE version,\n"+
					"observed from the first poll onwards. The second poll reads the same window byte\n"+
					"for byte, so every event it produces carries the same deterministic id and the log\n"+
					"answers DUPLICATE_NOOP. Anything it wrote instead — a second version at this ref,\n"+
					"a re-observation, a retraction — appears here as a second row, where the subgraph\n"+
					"above would look identical. That is SC-012's \"no node, no change and no event\" in\n"+
					"the form a replay can assert it."),
			extentQuery(),
		),
	}
}

// T079: a poll that stopped part-way, which T037, T043 and T044 all name.
//
// The window is read incompletely and says so: the marker's outcome is `partial` and it carries the
// reason. What the fixture pins is the three things FR-056 asks for together — the rollout that WAS
// read is emitted, the deployment whose statuses were not read is deferred rather than retracted, and
// the NEXT checkpoint declares the gap.
func partialPollFixture() fixtureSpec {
	const env = "production"
	return fixtureSpec{
		dir:    "fixtures/github-partial-poll-01",
		family: "github",
		description: "A poll that failed part-way. Two deployments are announced; the statuses of the " +
			"first arrive and the statuses of the second do not, and the poll marker says `partial` " +
			"with its reason. One ROLLOUT is emitted, the unread deployment is DEFERRED rather than " +
			"retracted — work not done yet is not work decided against (FR-073) — and no retraction " +
			"appears anywhere, because retracting for an unread tail would assert that something " +
			"stopped existing when the only fact is that nobody looked. The second cycle completes, " +
			"and its checkpoint follows a declared gap (FR-056). Synthetic structural twin: no " +
			"identifier is derived from any organisation (contracts/sanitisation.md §7), and " +
			"constitution VIII says synthetic-only data is not enough for a connector to be marked " +
			"stable — a recording campaign replaces it by swapping payloads/, not by changing code.",
		options: storefrontOptions(),
		payloads: []feeder.Payload{
			scopePayload(fixtureFirst, repository(twinStorefrontID, twinStorefront)),
			deploymentsPayload(fixtureFirst,
				deploymentSpec{id: 4321, repo: twinStorefront, sha: twinSHA, environment: env,
					production: "true"},
				// Announced and never resolved in this cycle: its statuses are the part the poll did
				// not reach.
				deploymentSpec{id: 4322, repo: twinStorefront, sha: twinOtherSHA, environment: env,
					production: "true"},
			),
			statusesPayload(fixtureFirst, twinStorefront, 4321, env,
				statusSpec{id: 2, state: "success", at: "2026-03-01T14:03:12Z"},
				statusSpec{id: 1, state: "in_progress", at: "2026-03-01T14:01:30Z"},
			),
			runsPayload(fixtureFirst, runSpec{id: 99, name: "Deploy", sha: twinSHA, event: "push"}),
			// The poll stopped here. `partial` is a declaration rather than something inferred from a
			// gap in the events, and the reason travels with it.
			partialPollMarker(fixturePoll, "the rate limit was reached before the second deployment's statuses"),
			// The next cycle completes, and its checkpoint is the one that follows the gap.
			pollMarker(fixtureSecond, "complete"),
		},
		queries: queriesYAML(
			fixtureQuery{
				name: "the-rollout-that-was-read", kind: "subgraph", hops: 2,
				focus:   "github.change=repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront",
				validAt: statusSucceededAt,
				comment: "The rollout the poll DID read, at its own instant. A partial poll still\n" +
					"delivers what it reached: the tail it did not reach is not a reason to withhold\n" +
					"the head.",
			},
			// The other half of FR-073 — that deployment 4322, whose statuses the poll never
			// reached, has NO change at all — is not asserted here, and cannot be. A query focused
			// on a ref no entity matches answers `not_found`, which is an error rather than an
			// empty answer, so T151's `expect_empty` cannot carry it either: the corpus has no way
			// to ask "is this absent?". It is asserted instead by
			// `TestADeploymentWithNoStatusesIsDeferredRatherThanRetracted` and
			// `TestAPartialPollEmitsNoRetraction` in partial_poll_test.go, over the emitted events
			// rather than the graph, with `TestARetractionIsExpressibleSoTheGuardIsNotVacuous`
			// standing behind them to show the guard can fail.
			historyQuery("the-read-rollout-is-not-retracted",
				"github.change=repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront",
				"ONE version. Its valid interval is closed after a microsecond because that is what\n"+
					"a change IS (`changeInstant`), not because anything retracted it — a retraction\n"+
					"issued because the poll stopped part-way would appear here as a SECOND version\n"+
					"whose observed start closes this one's, and that is the row that must not exist.\n"+
					"No subgraph at a single instant can show it."),
			extentQuery(),
		),
	}
}
