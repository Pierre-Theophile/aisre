// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	githubfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/github"
	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// The deploy cross-source fixture: two connectors observe one rollout (004 T019–T022, FR-045, SC-004).
//
// ---------------------------------------------------------------------------------------------
// Why this fixture exists
//
// C8 merges two observations of the same rollout — a deploy pipeline's and the platform's. It was
// written, registered and unit-tested in T014–T018, and until this fixture nothing in the corpus
// paired a GitHub deploy with a Cloud Run one. That is exactly the position C4, C5 and C7 were in
// before `gcp-cross-source-merge-01`: published, evaluated on every claim, never once fired, and every
// defect latent. Building that fixture found two real defects in C4. This one is the same instrument
// pointed at C8.
//
// # Why the operator's target map makes the two sides meet
//
// The GitHub connector cannot know which service a repository deploys to — that mapping is the
// operator's, and `targets.go` explains at length why it must never be read from the observed
// repository. Here the operator's rule names the target in the graph's own vocabulary, as
// `gcp.cloudrun.service`, with the value the GCP feeder mints for the same service. So the two
// connectors address ONE target entity directly, and C8's target arm is satisfied without a third
// source having to merge two targets first.
//
// That is the realistic shape. An operator who runs a deploy pipeline against Cloud Run knows the
// service it deploys to; that is the whole content of the mapping. A fixture that instead leaned on
// C4 to join the targets would be testing C4 and calling it C8.
//
// # What the fixture contains, and what each part refuses
//
// TWO deployments of one repository, and the operator's map names two Cloud Run services for its
// production environment — so FR-017 makes each deployment two changes, and the fixture holds four:
//
//   - deployment 4320, commit OLD → `storefront` and `orders`. The platform's revisions 41 and 17
//     carry the same commit, so C8 merges both pairs.
//   - deployment 4321, commit NEW → `storefront` and `orders`. Revisions 42 and 18 carry it, and C8
//     merges both pairs.
//
// Four merges rather than one, which matters: a rule that fired only on the first pair it saw, or only
// on the service whose name happens to match the repository's, would satisfy a ground truth naming one
// pair and fail this one.
//
// The two refusals are the point, and both are in `ground_truth.distinct_pairs`:
//
//   - the REDEPLOY: 4320's `storefront` change and 4321's target one service and carry different
//     commits. Merging them collapses a rollout with the one it replaced — the defect 003's C4
//     shipped with.
//   - the MONOREPO run: 4321's two changes carry one commit and target different services. One commit
//     reaching several services is the normal case, not evidence of identity. Here the two changes
//     come from the SAME deployment, which is the strongest form of the case: everything about them
//     agrees except the target.
//
// # The third source: Vercel (004 T093)
//
// The monorepo has a third service, `web`, which is served by Vercel rather than Cloud Run. The
// operator's map sends the pipeline's production deploy to the Vercel PROJECT as well — named in
// Vercel's own vocabulary, `vercel.project`, exactly as the Cloud Run services are named in GCP's — so
// each production deployment is three changes. The Vercel feeder describes the project and reports its
// own promotion of the NEW commit, and C8 merges that with the pipeline's `web` change: one commit, one
// target, one environment, three sources in the fixture.
//
// Until T093 this could not be built. The Vercel feeder never described the thing it serves, so its
// target was a reference to nothing; C8 reads targets from the graph and found none to share. The first
// attempt recorded every pair as a P7 suggestion and is written up in tasks.md.
//
// A rule generous enough to merge on the commit alone passes the four C8 pairs and fails both of
// these, so the gate cannot be satisfied by a rule that merges everything. And C8's third condition
// is load-bearing rather than decorative: all four changes and all four revisions are in `production`,
// so the environment agrees everywhere and cannot be what is doing the refusing.
//
// # Why both halves are recorded into one directory, GCP first
//
// The recorders append, so the fixture is one payload stream and one event stream in arrival order —
// which is what a fixture is. The GCP half is recorded FIRST here, and that is the opposite of
// `gcp-cross-source-merge-01`'s choice, deliberately: there the telemetry is continuous and the poll
// explains it afterwards, so the observation lands first. A deploy pipeline is the other way round.
// The platform reports a revision as soon as it exists, and the pipeline's own record of having
// deployed it arrives from a separate poll — often later, sometimes not.
//
// What matters for C8 is that the shuffle step permutes events inside one arrival window, so the
// completing correlation key arrives either side of the other across the seeds, and a rule that only
// fires when the platform's key is stored first builds a different graph under permutation. That is
// the order dependence T137's re-trigger exists to remove, and this fixture is where it is measured
// rather than argued.

const (
	// The two commits. Full 40-hex, because that is what both platforms state and C8 compares the
	// values byte for byte — an abbreviation on one side and a full id on the other is a
	// disagreement, which is the behaviour `pkg/feeder`'s normalisation is responsible for and this
	// fixture must not paper over.
	deployCommitOld = "af5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4"
	deployCommitNew = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"

	// The second Cloud Run service the monorepo run also reaches.
	deployOrdersService = "orders"
	deployOrdersRevOld  = "orders-00017-ccc"
	deployOrdersRevNew  = "orders-00018-ddd"

	// The repository the pipeline runs in, and the three deployments.
	deployRepoName = "storefront"
	deployRepoID   = 555
)

func TestGenerateDeployCrossSourceFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the deploy cross-source fixture", genFixturesEnv)
	}
	writeDeployCrossSourceFixture(t)
}

// writeDeployCrossSourceFixture runs the GCP feeder and the GitHub feeder into one fixture directory.
func writeDeployCrossSourceFixture(t *testing.T) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "fixtures/deploy-cross-source-merge-01")
	// Only what a generator writes is cleared. `os.RemoveAll(dir)` was the first cut in every fixture
	// generator, and it took `golden/` with it — regenerating deleted the frozen answers, and
	// `fixture verify` then had nothing left to disagree with — along with anything recorded beside
	// the fixture by other means, such as `gcp-cross-source-merge-01`'s `world/` (004 T079, T153).
	// A stale golden that survives a regeneration makes verify FAIL and name the query, which is the
	// failure worth having.
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatalf("clear %s: %v", filepath.Join(dir, generated), err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	// One shared arrival clock across both feeders: observed time is monotonic over the whole
	// fixture, not per source. Two independent clocks would let the second source's first event be
	// observed before the first source's last, which is a fixture that could not have happened.
	clock := &arrivalClock{base: fixtureStart}

	gcpDesc := writeDeployCrossSourceGCPHalf(t, dir, clock)
	githubDesc := writeDeployCrossSourceGitHubHalf(t, dir, clock)
	vercelDesc := writeDeployCrossSourceVercelHalf(t, dir, clock)

	if err := record.WriteManifest(dir, record.Manifest{
		Family: "deploy-cross-source",
		Description: "One repository's rollouts seen by three connectors: the GCP feeder polls Cloud " +
			"Run and reports revisions carrying the commit they were built from, and the GitHub " +
			"feeder polls the same pipeline's deployments, and the Vercel feeder reports the " +
			"promotion of the monorepo's third service, which Vercel serves. The operator's target " +
			"map names each rollout's target in the platform's own vocabulary — the Cloud Run " +
			"service, the Vercel project — so the connectors address one entity and C8 merges each " +
			"rollout's observations across three sources — while refusing the two " +
			"near misses that share one of C8's two conditions and not the other: a REDEPLOY " +
			"(one target, two commits) and a MONOREPO run (one commit, two targets). It is the " +
			"only fixture in the corpus in which a deploy pipeline's observation meets a " +
			"platform's, which is what makes SC-004 a measurement rather than 100% of nothing. " +
			"Synthetic structural twin: no identifier is derived from any organisation " +
			"(contracts/sanitisation.md §7), and constitution VIII says synthetic-only data is not " +
			"enough for a connector to be marked stable — a recording campaign replaces it by " +
			"swapping payloads/, not by changing code.",
		Sources: []record.ManifestSource{
			record.SourceOf(gcpDesc), record.SourceOf(githubDesc), record.SourceOf(vercelDesc),
		},
		Start: fixtureStart,
		// The declared window must CONTAIN the fixture's own arrivals, and until 004 T093 it did not.
		// The GitHub half arrives at `cycleAt(2)` — 15:00 — and the arrival clock hands out
		// 15:00:00.000002 and later, so with `End` at 15:00:00.000000 the pipeline's claims were
		// observed two microseconds AFTER the window this fixture declares. The pinned pass overrides
		// observed time with `clock.end`, so it could not see them, and
		// `audit.why-the-redeploy-is-not-the-rollout-it-replaced` recorded a pinned golden of `{}`.
		//
		// That golden is one of the empty pinned goldens 004 T151's gate found and T153 is about — and
		// it is NOT one of T153's questions. A fixture whose clock excludes its own events is simply
		// mis-declared: no policy about the pinned pass makes a window that ends before the events
		// arrive correct. Fixed here by declaring a window that covers them, which is a declaration
		// made truthful rather than a decision about how the pinned pass should behave.
		End: fixtureStart.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("%s: write manifest: %v", dir, err)
	}
	appendQueries(t, dir, deployCrossSourceQueries())
	t.Logf("%s: two sources written", dir)
}

// The platform's half: two Cloud Run services, each with two revisions, each revision declaring the
// commit it was built from through the `commit-sha` label T135 reads.
func writeDeployCrossSourceGCPHalf(t *testing.T, dir string, clock *arrivalClock) feeder.Description {
	t.Helper()
	labels := map[string]string{
		"team":        twinTeam,
		"environment": "production",
		"service":     twinService,
	}
	payloads := []feeder.Payload{
		servicesPayloadAt(t, cycleAt(1),
			twinServiceJSON("uid-service-1", 42, fixtureStart.Add(-720*time.Hour), fixtureCreated,
				twinRevOld, twinRevNew, map[string]int{twinRevNew: 100}, labels),
			deployOrdersServiceJSON(labels)),
		revisionsPayloadAt(t, cycleAt(1),
			// The revision under test, and the one it replaced. Both carry a commit, and they carry
			// DIFFERENT ones — a fixture where the old revision stated no commit would let a rule
			// that merges on the target alone pass, because there would be nothing to disagree with.
			deployRevisionJSON(twinService, twinRevNew, fixtureCreated, "42", deployCommitNew),
			deployRevisionJSON(twinService, twinRevOld, fixtureStart.Add(-24*time.Hour), "41", deployCommitOld),
			deployRevisionJSON(deployOrdersService, deployOrdersRevNew, fixtureCreated, "18", deployCommitNew),
			deployRevisionJSON(deployOrdersService, deployOrdersRevOld, fixtureStart.Add(-24*time.Hour), "17", deployCommitOld)),
		pollPayloadAt(cycleAt(1), "complete", ""),
	}

	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "twin",
		Scope:   gcpfeeder.Scope{Projects: []string{twinProject}, Regions: []string{twinRegion}},
		Labels:  twinLabelPolicy(),
		Actors:  twinActorPolicy(),
		Horizon: gcpfeeder.Horizon{Earliest: fixtureStart, Reason: gcpfeeder.HorizonConfigured},
		AuditFilters: []string{
			`logName="projects/` + twinProject + `/logs/cloudaudit.googleapis.com%2Factivity"`,
			`protoPayload.serviceName="run.googleapis.com"`,
		},
		OmittedSurfaces: []string{"cloud_dns", "load_balancers"},
		FingerprintKey:  []byte(twinFingerprintKey),
	})
	if err != nil {
		t.Fatalf("new gcp feeder: %v", err)
	}
	desc := f.Describe()
	src := record.Wrap(clock.wrap(source.NewSliceSource(payloads)), dir)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatalf("gcp half: run: %v", err)
	}
	assertRecorded(t, "gcp half", src, events, memory)
	return desc
}

// The pipeline's half: three deployments of one repository, two of them to one service and one to
// another, carrying the two commits the platform also reported.
func writeDeployCrossSourceGitHubHalf(t *testing.T, dir string, clock *arrivalClock) feeder.Description {
	t.Helper()
	at := cycleAt(2)
	payloads := []feeder.Payload{
		deployScopePayload(at),
		// The runs that produced the deployments, matched to them by head sha. They are what gives a
		// rollout a link that opens the run (004 T128, SC-016): the run's SOURCE_LINK pointer.
		deployRunsPayload(at, deployRun{id: 9820, sha: deployCommitOld}, deployRun{id: 9821, sha: deployCommitNew}),
		deployDeploymentsPayload(at,
			deployDeployment{id: 4320, sha: deployCommitOld, environment: "production"},
			deployDeployment{id: 4321, sha: deployCommitNew, environment: "production"},
			// The canary rollout of the same commit to the same service. Everything C8 compares
			// agrees except the environment.
			deployDeployment{id: 4322, sha: deployCommitNew, environment: "canary"},
		),
		// A rollout is only a rollout once a status says it succeeded. Without these the deployments
		// are held and the fixture would carry no change at all.
		deployStatusesPayload(at, 4320, "production", "2026-09-20T14:02:00Z"),
		deployStatusesPayload(at, 4321, "production", "2026-09-21T14:20:00Z"),
		deployStatusesPayload(at, 4322, "canary", "2026-09-21T14:24:00Z"),
		deployPollMarker(at),
	}

	f, err := githubfeeder.New(githubfeeder.Options{
		OrgSlug: "twin",
		Map:     deployCrossSourceMapOptions(),
	})
	if err != nil {
		t.Fatalf("new github feeder: %v", err)
	}
	desc := f.Describe()
	src := record.Wrap(clock.wrap(source.NewSliceSource(payloads)), dir)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatalf("github half: run: %v", err)
	}
	assertRecorded(t, "github half", src, events, memory)
	return desc
}

// The operator's mapping: this repository deploys to these two Cloud Run services, one per
// environment. Naming the target in the platform's own vocabulary is what lets the two connectors
// meet; see the file comment.
func deployCrossSourceMapOptions() githubfeeder.MapOptions {
	return githubfeeder.MapOptions{
		Allowlist: githubfeeder.Allowlist{Environments: []string{"production", "canary"}},
		// TWO rules for ONE environment, which is exactly the monorepo mapping: this repository's
		// production deploy reaches both services, so FR-017 makes each deployment N changes.
		//
		// Both rules name `production` rather than one of them naming an environment of its own, and
		// that is not cosmetic. C8 requires the environment to AGREE as well as the commit and the
		// target, and the platform reports both services with the `environment: production` label. A
		// first draft of this fixture gave the second service its own GitHub environment
		// (`orders-production`) and C8 correctly refused to merge that pair — the fixture was wrong,
		// not the rule, and it is worth leaving the reason here because the failure looked like a
		// missing merge rather than like a disagreement.
		Targets: githubfeeder.TargetMap{Repositories: map[string][]githubfeeder.TargetRule{
			"acme/" + deployRepoName: {
				{
					Environment: "production",
					Namespace:   gcpfeeder.NSService,
					Value:       cloudRunServiceValue(twinService),
				},
				{
					Environment: "production",
					Namespace:   gcpfeeder.NSService,
					Value:       cloudRunServiceValue(deployOrdersService),
				},
				// The third service, served by Vercel and named as the Vercel feeder names it (T093).
				{
					Environment: "production",
					Namespace:   feeder.NSVercelProject,
					Value:       deployVercelProject,
				},
				// And the canary environment, deploying to the SAME storefront service. That is what a
				// traffic-split canary is, and it is here to give C8's environment arm something to
				// refuse: a canary rollout of the same commit to the same service agrees with the
				// platform's revision on both of C8's other conditions.
				{
					Environment: "canary",
					Namespace:   gcpfeeder.NSService,
					Value:       cloudRunServiceValue(twinService),
				},
			},
		}},
		Actors: githubfeeder.ActorPolicy{DeploymentAutomation: []string{"release-runner"}},
	}
}

// The platform's third half: Vercel's own record of promoting `web`, and the project it serves.
//
// Arrives last, a cycle after the pipeline, which is Vercel's shape: the promotion is a state rather
// than a timestamp (research §5.2), so the change is dated from the read, with its start marked
// unknown, and C8 has to merge it with a pipeline change that states its instant.
func writeDeployCrossSourceVercelHalf(t *testing.T, dir string, clock *arrivalClock) feeder.Description {
	t.Helper()
	at := cycleAt(3)
	payloads := []feeder.Payload{
		{Kind: vercelfeeder.PayloadProjects, At: at, Bytes: []byte(fmt.Sprintf(
			`{"projects":[{"id":%q,"name":"web","createdAt":%d,`+
				`"link":{"type":"github","org":"acme","repo":%q,"repoId":%d}}]}`,
			deployVercelProject, fixtureStart.Add(-720*time.Hour).UnixMilli(), deployRepoName, deployRepoID))},
		{Kind: vercelfeeder.PayloadDeployments, At: at, Bytes: []byte(`{"deployments":[` +
			deployVercelDeploymentJSON(deployVercelDeployment, "production", "PROMOTED", deployCommitNew) + `,` +
			// A preview of the same commit. It shares the commit and the project with the promotion,
			// and is not a rollout at all: nothing here may merge it, because nothing here may emit it.
			deployVercelDeploymentJSON("dpl_web_preview", "", "PROMOTED", deployCommitNew) + `]}`)},
		{Kind: vercelfeeder.PayloadPollMarker, At: at, Bytes: []byte(`{"outcome":"complete"}`)},
	}
	f, err := vercelfeeder.New(vercelfeeder.Options{OrgSlug: "twin"})
	if err != nil {
		t.Fatalf("new vercel feeder: %v", err)
	}
	desc := f.Describe()
	src := record.Wrap(clock.wrap(source.NewSliceSource(payloads)), dir)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatalf("vercel half: run: %v", err)
	}
	assertRecorded(t, "vercel half", src, events, memory)
	return desc
}

func deployVercelDeploymentJSON(uid, target, substate, sha string) string {
	return fmt.Sprintf(`{"uid":%q,"name":"web","projectId":%q,"target":%q,
		"readyState":"READY","readySubstate":%q,"source":"git","isRollbackCandidate":false,
		"inspectorUrl":"https://vercel.com/twin/web/%s",
		"creator":{"uid":"usr_release","type":"user"},
		"attribution":{"commitMeta":{"githubCommitSha":%q}},
		"createdAt":%d,"buildingAt":%d,"ready":%d}`,
		uid, deployVercelProject, target, substate, uid, sha,
		fixtureStart.Add(15*time.Minute).UnixMilli(),
		fixtureStart.Add(16*time.Minute).UnixMilli(),
		fixtureStart.Add(19*time.Minute).UnixMilli())
}

func cloudRunServiceValue(service string) string {
	return twinProject + "/" + twinRegion + "/" + service
}

// deployRevisionJSON is twinRevisionJSON with the commit label T135 reads, and for any service rather
// than only `storefront`.
func deployRevisionJSON(service, name string, createTime time.Time, digestSuffix, commit string) string {
	return fmt.Sprintf(`{
  "name": "projects/%s/locations/%s/services/%s/revisions/%s",
  "uid": "uid-%s",
  "generation": "1",
  "createTime": %q,
  "labels": {"team": %q, "commit-sha": %q},
  "serviceAccount": "runtime@%s.iam.gserviceaccount.com",
  "executionEnvironment": "EXECUTION_ENVIRONMENT_GEN2",
  "containers": [{"image": "europe-docker.pkg.dev/%s/twin/%s@sha256:00000000000000000000000000000000000000000000000000000000000000%s"}],
  "conditions": [{"type": "Ready", "state": "CONDITION_SUCCEEDED"}]
}`, twinProject, twinRegion, service, name, name, rfc3339(createTime), twinTeam, commit,
		twinProject, twinProject, service, digestSuffix)
}

// deployOrdersServiceJSON is the second service, serving its newer revision.
func deployOrdersServiceJSON(labels map[string]string) string {
	return fmt.Sprintf(`{
  "name": "projects/%s/locations/%s/services/%s",
  "uid": "uid-service-orders",
  "generation": "18",
  "createTime": %q,
  "updateTime": %q,
  "labels": {"team": %q, "environment": %q, "service": %q},
  "latestReadyRevision": "projects/%s/locations/%s/services/%s/revisions/%s",
  "latestCreatedRevision": "projects/%s/locations/%s/services/%s/revisions/%s",
  "trafficStatuses": [{"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", "revision": %q, "percent": 100}],
  "conditions": [{"type": "Ready", "state": "CONDITION_SUCCEEDED"}]
}`, twinProject, twinRegion, deployOrdersService,
		rfc3339(fixtureStart.Add(-720*time.Hour)), rfc3339(fixtureCreated),
		labels["team"], labels["environment"], deployOrdersService,
		twinProject, twinRegion, deployOrdersService, deployOrdersRevNew,
		twinProject, twinRegion, deployOrdersService, deployOrdersRevNew,
		deployOrdersRevNew)
}

// The GitHub payload builders. They are written here rather than imported because the GitHub
// generator's own helpers are unexported test code in another package — the same reason
// `gcp-cross-source-merge-01` builds its OTLP payloads inline.

type deployDeployment struct {
	id          int64
	sha         string
	environment string
}

func (d deployDeployment) json() string {
	return fmt.Sprintf(`{"id":%d,"sha":%q,"ref":"main","task":"deploy","environment":%q,
		"production_environment":true,
		"created_at":"2026-09-21T14:00:00Z","updated_at":"2026-09-21T14:03:12Z",
		"creator":{"login":"release-runner","id":7,"type":"Bot"},
		"url":"https://api.github.com/repos/acme/%s/deployments/%d",
		"statuses_url":"https://api.github.com/repos/acme/%s/deployments/%d/statuses"}`,
		d.id, d.sha, d.environment, deployRepoName, d.id, deployRepoName, d.id)
}

type deployRun struct {
	id  int64
	sha string
}

func deployRunsPayload(at time.Time, runs ...deployRun) feeder.Payload {
	bodies := make([]string, 0, len(runs))
	for _, r := range runs {
		bodies = append(bodies, fmt.Sprintf(`{"id":%d,"name":"Deploy","run_number":%d,"run_attempt":1,
			"head_sha":%q,"event":"push","status":"completed","conclusion":"success",
			"run_started_at":"2026-09-21T14:00:00Z",
			"actor":{"login":"release-runner","id":7,"type":"Bot"},
			"triggering_actor":{"login":"release-runner","id":7,"type":"Bot"},
			"html_url":"https://github.com/acme/%s/actions/runs/%d",
			"logs_url":"https://api.github.com/repos/acme/%s/actions/runs/%d/logs"}`,
			r.id, r.id, r.sha, deployRepoName, r.id, deployRepoName, r.id))
	}
	return feeder.Payload{
		Kind: githubfeeder.PayloadWorkflowRuns, At: at,
		Bytes: []byte(fmt.Sprintf(`{"total_count":%d,"workflow_runs":[%s]}`, len(runs), strings.Join(bodies, ","))),
	}
}

func deployScopePayload(at time.Time) feeder.Payload {
	return feeder.Payload{
		Kind: githubfeeder.PayloadInstallationRepositories, At: at,
		Bytes: []byte(fmt.Sprintf(
			`{"total_count":1,"repository_selection":"selected","repositories":[`+
				`{"id":%d,"name":%q,"full_name":"acme/%s","private":true,"owner":{"login":"acme"}}]}`,
			deployRepoID, deployRepoName, deployRepoName)),
	}
}

func deployDeploymentsPayload(at time.Time, deployments ...deployDeployment) feeder.Payload {
	bodies := make([]string, 0, len(deployments))
	for _, d := range deployments {
		bodies = append(bodies, d.json())
	}
	return feeder.Payload{
		Kind: githubfeeder.PayloadDeployments, At: at,
		Bytes: []byte("[" + strings.Join(bodies, ",") + "]"),
	}
}

func deployStatusesPayload(at time.Time, deployment int64, environment, succeededAt string) feeder.Payload {
	return feeder.Payload{
		Kind: githubfeeder.PayloadDeploymentStatuses, At: at,
		Bytes: []byte(fmt.Sprintf(
			`{"repository":"acme/%s","deployment_id":%d,"statuses":[`+
				`{"id":%d,"state":"success","environment":%q,"created_at":%q,`+
				`"creator":{"login":"release-runner","id":7,"type":"Bot"}}]}`,
			deployRepoName, deployment, deployment*10, environment, succeededAt)),
	}
}

func deployPollMarker(at time.Time) feeder.Payload {
	return feeder.Payload{
		Kind: githubfeeder.PayloadPollMarker, At: at, Bytes: []byte(`{"outcome":"complete"}`),
	}
}

// The change refs the two connectors mint, which are what the audit queries and the ground truth name.
//
// They are spelled out rather than built by calling the feeders' own id functions on purpose: a
// fixture's ground truth is a statement about what the graph should contain, and deriving it from the
// code under test would make it agree with that code by construction.
const (
	// Deployment 4320 (commit OLD) and 4321 (commit NEW), each making one change per target.
	deployGitHubChangeOld = "repositories/555/deployments/4320/targets/" +
		"gcp.cloudrun.service/twin-production/europe-west1/storefront"
	deployGitHubChangeNew = "repositories/555/deployments/4321/targets/" +
		"gcp.cloudrun.service/twin-production/europe-west1/storefront"
	deployGitHubChangeOrders = "repositories/555/deployments/4321/targets/" +
		"gcp.cloudrun.service/twin-production/europe-west1/orders"

	// The canary rollout: deployment 4322, same commit and same target as 4321's storefront change.
	deployGitHubChangeCanary = "repositories/555/deployments/4322/targets/" +
		"gcp.cloudrun.service/twin-production/europe-west1/storefront"

	// The Vercel half: the project, the promotion, and the pipeline's changes that target the project.
	deployVercelProject      = "prj_web"
	deployVercelDeployment   = "dpl_web_42"
	deployGitHubChangeWeb    = "repositories/555/deployments/4321/targets/vercel.project/" + deployVercelProject
	deployGitHubChangeWebOld = "repositories/555/deployments/4320/targets/vercel.project/" +
		deployVercelProject

	deployGCPChangeNew = "rollout-created/twin-production/europe-west1/storefront/" +
		twinRevNew + "@2026-09-21T14:18:00Z"
	deployGCPChangeOrders = "rollout-created/twin-production/europe-west1/orders/" +
		deployOrdersRevNew + "@2026-09-21T14:18:00Z"
)

// deployCrossSourceQueries are the manifest queries this fixture is verified against.
//
// The audits are the load-bearing ones: they run the same code path as `resolve why`, so a C8 merge
// that stopped being explainable — or started being explained by a different rule — is a golden diff
// rather than a conversation. Two of them ask about pairs that must NOT have merged, which is the half
// a fixture usually forgets.
//
// Every query is observed-time pinned, for the reason in fixture_specs_test.go: an unpinned query
// answers "as of now" and its golden encodes the day it was recorded.
func deployCrossSourceQueries() string {
	return `queries:
  # The C8 merge itself: the pipeline's record of a rollout and the platform's, on one commit and one
  # target. Its golden carries the decision, its rule, and the CORRELATION keys it stood on.
  - name: why-the-two-rollouts-are-one
    kind: audit
    ref_a: github.change=` + deployGitHubChangeNew + `
    ref_b: gcp.change=` + deployGCPChangeNew + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # The REDEPLOY. One target, two commits: these are two rollouts of one service, and merging them
  # collapses a rollout with the one it replaced.
  - name: why-the-redeploy-is-not-the-rollout-it-replaced
    kind: audit
    ref_a: github.change=` + deployGitHubChangeOld + `
    ref_b: github.change=` + deployGitHubChangeNew + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # The MONOREPO run. One commit, two targets: FR-017 makes this N changes, and a commit reaching
  # several services is the normal case rather than evidence that the services are one.
  - name: why-one-commit-to-two-services-is-two-rollouts
    kind: audit
    ref_a: github.change=` + deployGitHubChangeNew + `
    ref_b: github.change=` + deployGitHubChangeOrders + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # The CANARY rollout against the platform's production revision: one commit, one target, two
  # environments. C8's third condition is the only thing refusing this one.
  - name: why-the-canary-is-not-the-production-rollout
    kind: audit
    ref_a: github.change=` + deployGitHubChangeCanary + `
    ref_b: gcp.change=` + deployGCPChangeNew + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # The third source. Vercel's own promotion of web and the pipeline's web change: one commit, one target
  # (the project the Vercel feeder describes), one environment — C8, across a platform that states the
  # instant and one that does not.
  - name: why-the-vercel-promotion-is-the-pipelines-rollout
    kind: audit
    ref_a: github.change=` + deployGitHubChangeWeb + `
    ref_b: vercel.change=` + deployVercelDeployment + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # The same question on the Vercel-served service: ONE rollout, carrying the pipeline's instant and
  # actor and Vercel's promotion, because C8 merged the two. The preview of the same commit is absent,
  # because it was never a rollout (FR-032).
  - name: what-changed-on-web
    kind: diff
    focus: vercel.project=` + deployVercelProject + `
    t1: ` + rfc3339(fixtureStart) + `
    t2: ` + rfc3339(fixtureStart.Add(2*time.Hour)) + `
    reference_at: ` + rfc3339(fixtureStart.Add(40*time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 1
    direction: both
  # The question an investigation actually asks, and the one this fixture exists to answer: what
  # changed on storefront in the window before the incident? (004 T152)
  #
  # A DIFF over the window, focused on the service — not a subgraph. A change's valid interval is one
  # microsecond, so a subgraph at any instant but the change's own shows no change, and an investigation
  # never knows that instant in advance; it knows the window. This replaced a subgraph whose comment said
  # it showed "one change, the service it targeted" and whose golden held no change at all.
  #
  # The assertion is in the COUNT: the production rollout GitHub and Cloud Run both observed appears
  # ONCE, carrying both sources' facts, because C8 merged it — and the canary appears separately,
  # because C8 refused it on environment. Two rollouts where there should be one would mean the merge
  # did not reach what an investigation reads. The same commit's orders rollout is there too, two hops
  # out and merged on its own: one commit to two services is two rollouts (FR-017).
  - name: what-changed-on-storefront
    kind: diff
    focus: gcp.cloudrun.service=` + cloudRunServiceValue(twinService) + `
    t1: ` + rfc3339(fixtureStart) + `
    t2: ` + rfc3339(fixtureStart.Add(2*time.Hour)) + `
    reference_at: ` + rfc3339(fixtureStart.Add(40*time.Minute)) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The suggestion queue. C8 is certain, so the monorepo near miss must appear here as P7's
  # SUGGESTION rather than as a merge — an empty queue would mean the case was dropped, not refused.
  - name: the-monorepo-run-is-suggested-not-merged
    kind: suggestions
    observed_at: ` + rfc3339(pinnedAt) + `

ground_truth:
  # The pairs SC-004 is measured over: one rollout observed by two connectors, which must merge
  # automatically under a CERTAIN rule.
  cross_source_pairs:
    - pair:
        - github.change=` + deployGitHubChangeNew + `
        - gcp.change=` + deployGCPChangeNew + `
      same: true
      rule: C8
    # The second service's pair, from the SAME deployment. It is here because C8 must fire per
    # target: a rule that merged the monorepo run into one change would still satisfy a ground truth
    # that only named one of the two.
    - pair:
        - github.change=` + deployGitHubChangeOrders + `
        - gcp.change=` + deployGCPChangeOrders + `
      same: true
      rule: C8
    # The third source: the pipeline's web change and Vercel's promotion (T093).
    - pair:
        - github.change=` + deployGitHubChangeWeb + `
        - vercel.change=` + deployVercelDeployment + `
      same: true
      rule: C8
  # And the two near misses. Each shares exactly ONE of C8's two conditions, so a rule generous
  # enough to merge on either alone fails here — which is what stops the pairs above from being
  # satisfiable by a rule that merges everything.
  distinct_pairs:
    # The redeploy: same target, different commits.
    - pair:
        - github.change=` + deployGitHubChangeOld + `
        - github.change=` + deployGitHubChangeNew + `
      same: false
    # The monorepo run: same commit, different targets.
    - pair:
        - github.change=` + deployGitHubChangeNew + `
        - github.change=` + deployGitHubChangeOrders + `
      same: false
    # The CANARY rollout: same commit, same target, different environment. This is the pair that
    # makes C8's third condition load-bearing — without it, deleting the environment check changes
    # nothing anywhere in this corpus, which is how a condition becomes decoration.
    - pair:
        - github.change=` + deployGitHubChangeCanary + `
        - gcp.change=` + deployGCPChangeNew + `
      same: false
    # The redeploy again, across sources: Vercel's promotion of NEW and the pipeline's OLD deploy of web.
    # Same target, different commits.
    - pair:
        - github.change=` + deployGitHubChangeWebOld + `
        - vercel.change=` + deployVercelDeployment + `
      same: false
    # The monorepo run across platforms: Vercel's web promotion and Cloud Run's storefront rollout carry
    # one commit and share no target.
    - pair:
        - vercel.change=` + deployVercelDeployment + `
        - gcp.change=` + deployGCPChangeNew + `
      same: false
    # And across the two services, for completeness: neither condition holds.
    - pair:
        - github.change=` + deployGitHubChangeOld + `
        - gcp.change=` + deployGCPChangeOrders + `
      same: false
`
}
