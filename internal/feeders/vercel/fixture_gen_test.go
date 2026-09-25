// SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// The US2 and US3 fixtures (004 T091, T092, T102).
//
// ---------------------------------------------------------------------------------------------
// Why `vercel-promotion-01` records the STAGED deployment as well as the promoted one
//
// T091 asks for "staged, then promoted", and the ordering is the assertion rather than the setting. A
// fixture holding only the promoted deployment would pass with `readySubstate` ignored entirely: the
// mapper would see `READY`, emit a rollout, and the goldens would agree. It is the STAGED payload
// arriving first that makes the refusal observable — the fixture's event stream has to show one change
// where two deployments were reported, and the deployment that produced nothing is the one being
// tested.
//
// The two payloads are the SAME deployment at two instants, which is what a promotion actually looks
// like from a poll: a build finishes and is staged, a later poll finds it promoted. So the fixture also
// proves the second poll does not emit a second change for a deployment already seen — the event id is
// a function of the deployment's uid, so the promoted observation is one event and re-reading it is a
// duplicate rather than a new rollout.
//
// # Why `vercel-preview-excluded-01` is its own fixture
//
// A preview deployment excluded inside the promotion fixture would be one of several refusals in one
// stream, and a reader could not tell which payload the empty event list belonged to. Separating it
// makes the assertion unambiguous: this fixture reports deployments and emits NO CHANGE.
//
// Since 004 T093 it asserts that on the project rather than on an empty graph. The projects read
// describes the project, so there is a node to ask about, and a diff over the whole window focused on
// it answers "nothing changed here" — declared with `expect_empty` and a reason. That is a stronger
// statement than the extent-only golden it replaced: an empty graph is also what a broken fixture
// produces, whereas an existing project that no change touched is the refusal itself.

const (
	fixtureProject = "prj_storefront"
	fixtureCommit  = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	fixtureOrg     = "twin"
)

var (
	// The poll instants. Two cycles: the first finds the deployment staged, the second finds it promoted.
	fixtureStart      = time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	fixtureSecondPoll = fixtureStart.Add(18 * time.Minute)
	fixtureEnd        = fixtureStart.Add(time.Hour)
	fixturePinnedAt   = fixtureStart.Add(6 * time.Hour)
)

// genFixturesEnv arms the generator. Regenerating a corpus is a deliberate act: the corpus is the test.
const genFixturesEnv = "SRE_AGENT_GEN_FIXTURES"

func TestGenerateVercelFixtures(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the US2 Vercel fixtures", genFixturesEnv)
	}
	writeVercelFixture(t, promotionFixture())
	writeVercelFixture(t, previewExcludedFixture())
	writeVercelFixture(t, configChangeFixture())
	writeVercelFixture(t, rollbackFixture())
}

type vercelFixtureSpec struct {
	dir         string
	description string
	payloads    []feeder.Payload
	// queries are authored rather than derived: they state what the fixture proves.
	queries string
}

// promotionFixture is T091: staged, then promoted.
func promotionFixture() vercelFixtureSpec {
	return vercelFixtureSpec{
		dir: "fixtures/vercel-promotion-01",
		description: "One Vercel deployment, polled twice. The first poll finds it READY and STAGED " +
			"and it produces NO change — `READY` means built and available, not serving, and a " +
			"connector that read it as live would put a change node on the graph claiming production " +
			"moved when it had not. The second poll finds the same deployment PROMOTED and it becomes " +
			"a ROLLOUT carrying the commit C8 joins on. The STAGED payload is recorded rather than " +
			"omitted because it is what makes the refusal observable: a fixture holding only the " +
			"promoted deployment would pass with `readySubstate` ignored entirely. Valid time is " +
			"marked UNKNOWN rather than taken from the build instant, which Vercel does state — " +
			"substituting it would date the rollout earlier than it happened and move it up a causal " +
			"ranking (research §5.2, FR-041).",
		payloads: []feeder.Payload{
			vercelProjectsPayload(fixtureStart),
			vercelDeploymentsPayload(fixtureStart,
				vercelDeploymentJSON("dpl_storefront_42", "production", "READY", "STAGED", fixtureCommit)),
			vercelPollPayload(fixtureStart, "complete"),
			vercelDeploymentsPayload(fixtureSecondPoll,
				vercelDeploymentJSON("dpl_storefront_42", "production", "READY", "PROMOTED", fixtureCommit)),
			vercelPollPayload(fixtureSecondPoll, "complete"),
		},
		queries: vercelPromotionQueries(),
	}
}

// previewExcludedFixture is T092.
func previewExcludedFixture() vercelFixtureSpec {
	return vercelFixtureSpec{
		dir: "fixtures/vercel-preview-excluded-01",
		description: "Two preview deployments and a staging one, all PROMOTED, and the cycle emits " +
			"no change. Exclusion rests on the platform-stated `target` rather than on a heuristic over " +
			"the URL or the branch name (FR-032), so a preview is unreachable by this connector rather " +
			"than skipped by policy. The project the deployments belong to IS described, so the " +
			"refusal is asserted on it: a diff over the whole window, focused on the project, finds " +
			"no change — which an empty graph could not distinguish from a broken fixture.",
		payloads: []feeder.Payload{
			vercelProjectsPayload(fixtureStart),
			vercelDeploymentsPayload(fixtureStart,
				vercelDeploymentJSON("dpl_preview_1", "", "READY", "PROMOTED", fixtureCommit),
				vercelDeploymentJSON("dpl_preview_2", "", "READY", "PROMOTED", fixtureCommit),
				vercelDeploymentJSON("dpl_staging_1", "staging", "READY", "PROMOTED", fixtureCommit)),
			vercelPollPayload(fixtureStart, "complete"),
		},
		queries: vercelPreviewQueries(),
	}
}

// The platform instants the configuration variables in configChangeFixture carry. They are HOURS before
// the first poll, which is the whole point of FR-039: a variable edited at 03:10 is a change at 03:10,
// whatever time anybody next looked.
var (
	fixtureVariableCreated = time.Date(2026, 9, 21, 3, 10, 0, 0, time.UTC)
	fixtureVariableEdited  = time.Date(2026, 9, 21, 9, 5, 0, 0, time.UTC)
	fixtureVariableMade    = time.Date(2026, 9, 20, 2, 0, 0, 0, time.UTC)
)

// configChangeFixture is T102: environment variables as CONFIG changes, with the value nowhere.
func configChangeFixture() vercelFixtureSpec {
	return vercelFixtureSpec{
		dir: "fixtures/vercel-config-change-01",
		description: "Four environment variables and one rollout. Two variables become CONFIG changes " +
			"— one created at 03:10, one edited at 09:05 — and each is dated by the instant the " +
			"PLATFORM states it changed rather than by the poll that found it at 14:00 or by the " +
			"deployment that next applied it (FR-039). The other two are refused and counted: one " +
			"scoped to preview only, by the same platform-stated `target` filter the deployment mapper " +
			"applies (FR-032), and one the platform assigns itself, whose instants are the project's " +
			"rather than anybody's decision. The rollout is here so that `neither folded into the " +
			"other` is visible in the corpus rather than only in a unit test: it is its own change with " +
			"its own valid time, and it carries no `sre.vercel.config_*` property. " +
			"**No value appears anywhere, including in payloads/**, and the absence there is the " +
			"point rather than an omission: `GET /projects/{id}/env` lists `value` among its REQUIRED " +
			"response fields and ships `legacyValue` and `internalContentHint.encryptedValue` beside " +
			"it, so the platform sends the secret on every read — and sanitisation runs in the " +
			"connector before anything touches disk (FR-062), dropping all three, so a recording never " +
			"holds them. A twin that held them would be modelling a file that must not exist. What " +
			"this fixture therefore proves is the graph half: no event, property, claim or golden " +
			"holds the value in any form — not plaintext, not ciphertext, not truncated, not hashed " +
			"(FR-038, SC-006). That a POPULATED value field has nowhere to land is a unit test's job, " +
			"because a fixture only ever sees post-sanitisation bytes. No fingerprint either, which is " +
			"the deliberate " +
			"difference from the GCP config feeder: a keyed HMAC answers `did this change` without " +
			"storing what it is, and SC-006 counts hashes of values among what must be zero. " +
			"A REMOVAL is absent from this fixture because it is absent from the API: Vercel records " +
			"`project.env_variable.deleted` in its audit log, which has no REST endpoint and is " +
			"enterprise-plan, owner-role, CSV-export or push-drain only. Reading a variable's " +
			"disappearance between two polls as a deletion would assert that somebody removed it when " +
			"the only fact is that a listing no longer shows it.",
		payloads: []feeder.Payload{
			vercelProjectsPayload(fixtureStart),
			vercelEnvPayload(fixtureStart,
				// Created at 03:10 and not touched since: a `created` change at 03:10.
				vercelEnvJSON("icfg_payments_url", "PAYMENTS_API_URL", []string{"production"},
					fixtureVariableCreated, time.Time{}, false),
				// Made yesterday, edited at 09:05: an `updated` change at 09:05, with the creation
				// instant kept as a property so nothing is lost.
				vercelEnvJSON("icfg_feature_checkout", "FEATURE_NEW_CHECKOUT",
					[]string{"production", "preview"}, fixtureVariableMade, fixtureVariableEdited, false),
				// Preview only: excluded and counted (T101).
				vercelEnvJSON("icfg_preview_flag", "PREVIEW_FLAG", []string{"preview", "development"},
					fixtureVariableCreated, time.Time{}, false),
				// The platform's own: excluded and counted.
				vercelEnvJSON("icfg_vercel_url", "VERCEL_URL", []string{"production"},
					fixtureVariableMade, time.Time{}, true),
			),
			vercelDeploymentsPayload(fixtureStart,
				vercelDeploymentJSON("dpl_storefront_77", "production", "READY", "PROMOTED", fixtureCommit)),
			vercelPollPayload(fixtureStart, "complete"),
		},
		queries: vercelConfigQueries(),
	}
}

func vercelConfigQueries() string {
	created := "projects/" + fixtureProject + "/env/icfg_payments_url/created/" +
		strconv.FormatInt(fixtureVariableCreated.UnixMilli(), 10)
	updated := "projects/" + fixtureProject + "/env/icfg_feature_checkout/updated/" +
		strconv.FormatInt(fixtureVariableEdited.UnixMilli(), 10)
	return `queries:
  # The 03:10 creation, at its own instant. A change's valid interval is one microsecond, so this is the
  # only instant that can show it (004 T150) — and it being 03:10 rather than 14:00 is the whole of
  # FR-039's first clause.
  - name: config-created-at-0310
    kind: subgraph
    focus: ` + feeder.NSVercelChange + `=` + created + `
    valid_at: ` + fixtureVariableCreated.Format(time.RFC3339) + `
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
    hops: 2
    direction: both
  # The 09:05 edit, likewise. Two changes at two instants from ONE reading is what "independently of
  # when a deployment next applied it" looks like in a golden.
  - name: config-updated-at-0905
    kind: subgraph
    focus: ` + feeder.NSVercelChange + `=` + updated + `
    valid_at: ` + fixtureVariableEdited.Format(time.RFC3339) + `
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
    hops: 2
    direction: both
  # The rollout, as a history rather than a subgraph, because its valid start is UNKNOWN and so begins
  # at an observation instant the arrival clock assigns rather than one that can be written down
  # (research §5.2). A history takes no instant and shows the whole record, which is what is wanted
  # here: the rollout is its OWN change, not an attribute of either configuration edit.
  - name: the-rollout-is-its-own-change
    kind: history
    focus: ` + feeder.NSVercelChange + `=dpl_storefront_77
  # The extent: ` + "`not present`" + ` and ` + "`not read at that time`" + ` are different answers, and with two of the
  # four variables refused this fixture needs the difference stated.
  - name: extent
    kind: extent
    valid_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
`
}

// vercelEnvPayload is a recorded environment-metadata response. The project id travels IN the body
// because the real response does not carry one, so a recording of the response alone could not say whose
// configuration it was.
func vercelEnvPayload(at time.Time, envs ...string) feeder.Payload {
	return vercelPayload(vercelfeeder.PayloadProjectEnv, at,
		`{"projectId":"`+fixtureProject+`","envs":[`+strings.Join(envs, ",")+`]}`)
}

// vercelEnvJSON is one variable's metadata as a RECORDING holds it, which is not the same as what the
// platform sends.
//
// The platform sends the secret three times over — `value` is among the endpoint's required response
// fields, with `legacyValue` and `internalContentHint.encryptedValue` beside it — and none of the three
// appears here. That is not the twin being coy: it is what a recording IS. Sanitisation runs in the
// connector before anything touches disk (FR-062, FR-137), and the policy drops all three
// (internal/sanitise/policy.go), so a committed payload never holds them and a twin that held them would
// be modelling a file that must not exist. `check-no-secrets.sh` refuses the first draft of this
// function for exactly that reason, which is the gate working.
//
// The consequence is worth stating: the assertion that a POPULATED value field has nowhere to land
// cannot be made by a fixture, because a fixture only ever sees post-sanitisation bytes. It is made by
// TestThePayloadsValueFieldsHaveNowhereToLand in config_test.go, which decodes the real response shape
// in memory and never writes it anywhere.
//
// Everything else is synthetic (contracts/sanitisation.md §7): no identifier is derived from any
// organisation, and a recording campaign replaces these payloads without changing code.
func vercelEnvJSON(id, key string, targets []string, created, updated time.Time, system bool) string {
	quoted := make([]string, 0, len(targets))
	for _, target := range targets {
		quoted = append(quoted, strconv.Quote(target))
	}
	edited := ""
	if !updated.IsZero() {
		edited = fmt.Sprintf(`,"updatedAt":%d,"updatedBy":"usr_grace"`, updated.UnixMilli())
	}
	return fmt.Sprintf(`{"id":%q,"key":%q,"target":[%s],"type":"encrypted","system":%t,
		"createdBy":"usr_ada","createdAt":%d%s,
		"decrypted":false,"securityIssues":[]}`,
		id, key, strings.Join(quoted, ","), system, created.UnixMilli(), edited)
}

// writeVercelFixture replays the payloads through the feeder with both recorders in place, which is
// what `feed vercel --record --replay` does — so the fixture is what the feeder produced rather than
// what somebody thought it would produce.
func writeVercelFixture(t *testing.T, fx vercelFixtureSpec) {
	t.Helper()
	// `go test` runs with the working directory set to the package, so the path resolves against the
	// repository root. Writing it relative would silently create
	// `internal/feeders/vercel/fixtures/` and report success.
	dir := filepath.Join(repoRoot(t), fx.dir)
	// Only what this function writes is cleared. `os.RemoveAll(dir)` was the first cut here as well as
	// in the GitHub generator, and it took `golden/` with it — regenerating deleted the frozen answers
	// of every fixture it touched, and `fixture verify` then had nothing left to disagree with. That is
	// the worse of the two failures: a stale golden that survives makes verify FAIL and name the query,
	// where a golden that silently disappeared makes it pass (004 T079).
	for _, generated := range []string{"payloads", "events.jsonl", "manifest.yaml"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatalf("clear %s: %v", filepath.Join(dir, generated), err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	f, err := vercelfeeder.New(vercelfeeder.Options{
		OrgSlug: fixtureOrg,
		Targets: map[string]*graphv1.Ref{
			fixtureProject: feeder.Ref(feeder.NSK8sDeployment, "shop/storefront"),
		},
	})
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	desc := f.Describe()

	clock := &vercelClock{base: fixtureStart}
	src := record.Wrap(clock.wrap(source.NewSliceSource(fx.payloads)), dir)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)

	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatalf("%s: run: %v", dir, err)
	}
	if err := src.Err(); err != nil {
		t.Fatalf("%s: record payloads: %v", dir, err)
	}
	if err := events.Err(); err != nil {
		t.Fatalf("%s: record events: %v", dir, err)
	}
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("%s: %d events were refused; the first is %s (%s)",
			dir, len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	if err := record.WriteManifest(dir, record.Manifest{
		Family: "vercel",
		Description: fx.description +
			" Synthetic structural twin: no identifier is derived from any organisation " +
			"(contracts/sanitisation.md §7), and constitution VIII says synthetic-only data is not " +
			"enough for a connector to be marked stable — a recording campaign replaces it by " +
			"swapping payloads/, not by changing code.",
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		Start:          fixtureStart,
		End:            fixtureEnd,
		ExpectRejected: events.Rejections(),
	}); err != nil {
		t.Fatalf("%s: write manifest: %v", dir, err)
	}
	if fx.queries != "" {
		appendVercelQueries(t, dir, fx.queries)
	}
	t.Logf("%s: %d payloads, %d events", fx.dir, src.Count(), events.Accepted())
}

func vercelPromotionQueries() string {
	return `queries:
  # What an investigation asks first: what changed on the project, over the window, as known at the end
  # (004 T152, T093). The project is described by this feeder's projects read, so the question has a
  # node to be asked about, and the promotion is the one answer. The reference instant is the window's
  # end, the way an alert raised then would ask it.
  - name: what-changed-on-the-project
    kind: diff
    focus: vercel.project=` + fixtureProject + `
    t1: ` + fixtureStart.Format(time.RFC3339) + `
    t2: ` + fixtureEnd.Format(time.RFC3339) + `
    reference_at: ` + fixtureEnd.Format(time.RFC3339) + `
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
    hops: 1
    direction: both
  # The rollout itself, with what it attaches to. It targets the project, which this feeder describes,
  # and the operator's ` + "`shop/storefront`" + `, which no source here describes — so that one is recorded as an
  # unattached target and waits (Edge case 1, FR-034).
  #
  # One change is the assertion. A second would mean the STAGED observation had been read as live.
  #
  # The two instants are DIFFERENT and neither is a default. The valid instant is where this change is
  # valid — for a change whose valid start is unknown that is the observation it begins at, which is the
  # second poll — and the observed instant is the clock's end, by which the whole recording has arrived.
  # 004 T150 found eight deploy goldens empty because both fields held the clock's end while a change's
  # valid interval is one microsecond, and 004 T079 found that the fix had been applied to the
  # manifests by hand while this generator still wrote one instant into both.
  - name: promoted-rollout-2hop
    kind: subgraph
    focus: ` + feeder.NSVercelChange + `=dpl_storefront_42
    valid_at: ` + fixtureSecondPoll.Format(time.RFC3339) + `
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
    hops: 2
    direction: both
  # The suggestion queue, empty. One source cannot corroborate itself, so nothing here should be
  # proposing a merge — and a probable rule that fired on a single source's own events would show up.
  - name: no-merge-from-one-source
    kind: suggestions
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
    expect_empty: "one source cannot corroborate itself, so no rule should propose a merge; a probable rule that fired on a single source's own events would appear here"
`
}

func vercelPreviewQueries() string {
	return `queries:
  # The refusal, asserted where an investigation would look: the project exists (the projects read
  # describes it), and across the whole window nothing changed it. Three PROMOTED deployments arrived;
  # none targeted production, so none is a rollout (FR-032).
  - name: no-change-touched-the-project
    kind: diff
    focus: vercel.project=` + fixtureProject + `
    t1: ` + fixtureStart.Format(time.RFC3339) + `
    t2: ` + fixtureEnd.Format(time.RFC3339) + `
    reference_at: ` + fixtureEnd.Format(time.RFC3339) + `
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
    hops: 1
    direction: both
    expect_empty: "every deployment in the window was a preview or staging build, and a build that did not target production is not a rollout (FR-032); a change here would mean the exclusion leaked"
  # The extent: this source was read, which is what separates "nothing changed" from "nobody looked".
  - name: the-source-was-read
    kind: extent
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
`
}

// rollbackFixture is T123: a deploy, the incident it causes, the rollback that stops it, and the redeploy
// that is NOT a rollback (004 US5, FR-016, SC-009).
//
// The timeline, and what each step refuses:
//
//   - 14:00 `dpl_good` (commit A) is production.
//   - 14:20 `dpl_bad` (commit B) is promoted. The incident's onset is 14:30, the diff's reference.
//   - 14:41 the project read states a rollback to `dpl_good`, requested at 14:40, job IN PROGRESS: counted
//     as an exclusion and emitted as nothing, because production has not moved yet.
//   - 14:45 the same request, SUCCEEDED: a ROLLOUT flagged `rollback`, `rolled_back_to = dpl_good`, dated
//     14:40 from the platform's `requestedAt`, and AFTER the onset — so a diff whose reference is the
//     onset marks it post-reference and the engine treats it as a candidate effect, never a cause.
//   - 14:55 `dpl_redeploy` re-ships commit A from `source: redeploy` and is a rollback CANDIDATE. It is an
//     ordinary rollout: an older commit and a candidacy are both inferences, and the platform did not say
//     rollback (SC-009's negative case).
func rollbackFixture() vercelFixtureSpec {
	onset := fixtureStart.Add(30 * time.Minute)
	requested := fixtureStart.Add(40 * time.Minute)
	alias := func(status string) string {
		return fmt.Sprintf(`{"id":%q,"name":"storefront","createdAt":%d,`+
			`"link":{"type":"github","org":"acme","repo":"storefront","repoId":555},`+
			`"lastAliasRequest":{"type":"rollback","jobStatus":%q,"fromDeploymentId":"dpl_bad",`+
			`"toDeploymentId":"dpl_good","requestedAt":%d}}`,
			fixtureProject, fixtureProjectCreated.UnixMilli(), status, requested.UnixMilli())
	}
	return vercelFixtureSpec{
		dir: "fixtures/deploy-rollback-01",
		description: "A deploy, the incident it causes, and the rollback that stops it, with the rollback " +
			"flagged because the platform SAID so: Vercel's single-project read states " +
			"`lastAliasRequest.type = rollback`, the only place either deploy platform states one " +
			"(research §5.4). The rollback is reported in progress first and emits nothing until its job " +
			"succeeds. A later redeploy of the older commit, which is also a rollback candidate, is an " +
			"ordinary rollout: neither an older commit nor candidacy is the platform stating a rollback " +
			"(FR-016, SC-009).",
		payloads: []feeder.Payload{
			vercelProjectsPayload(fixtureStart),
			vercelDeploymentsPayload(fixtureStart,
				rollbackDeploymentJSON("dpl_good", "git", false, fixtureCommit, fixtureStart.Add(-10*time.Minute))),
			vercelPollPayload(fixtureStart, "complete"),
			vercelDeploymentsPayload(fixtureStart.Add(20*time.Minute),
				rollbackDeploymentJSON("dpl_bad", "git", false, rollbackBadCommit, fixtureStart.Add(15*time.Minute))),
			vercelPollPayload(fixtureStart.Add(20*time.Minute), "complete"),
			vercelPayload(vercelfeeder.PayloadProject, fixtureStart.Add(41*time.Minute), alias("in-progress")),
			vercelPollPayload(fixtureStart.Add(41*time.Minute), "complete"),
			vercelPayload(vercelfeeder.PayloadProject, fixtureStart.Add(45*time.Minute), alias("succeeded")),
			vercelPollPayload(fixtureStart.Add(45*time.Minute), "complete"),
			vercelDeploymentsPayload(fixtureStart.Add(55*time.Minute),
				rollbackDeploymentJSON("dpl_redeploy", "redeploy", true, fixtureCommit, fixtureStart.Add(50*time.Minute))),
			vercelPollPayload(fixtureStart.Add(55*time.Minute), "complete"),
		},
		queries: `queries:
  # What changed on the project around the incident, with the reference at its ONSET (14:30) rather than
  # the window's end, because that is the instant a cause has to precede. Four rollouts: the good deploy
  # and the bad one before it, and after it the rollback — flagged, naming dpl_good, post_reference — and
  # the unflagged redeploy. A rollback that ranked as a cause of the incident it stopped would be the
  # engine blaming the fix.
  - name: what-changed-around-the-incident
    kind: diff
    focus: vercel.project=` + fixtureProject + `
    t1: ` + fixtureStart.Format(time.RFC3339) + `
    t2: ` + fixtureEnd.Format(time.RFC3339) + `
    reference_at: ` + onset.Format(time.RFC3339) + `
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
    hops: 1
    direction: both
  # The rollback as its own change, at its own instant: what it restored and what it moved away from.
  - name: the-rollback-names-what-it-restored
    kind: history
    focus: vercel.change=rollback/` + fixtureProject + `/dpl_good@` + fmt.Sprint(requested.UnixMilli()) + `
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
  - name: extent
    kind: extent
    observed_at: ` + fixturePinnedAt.Format(time.RFC3339) + `
`,
	}
}

// rollbackBadCommit is the commit the bad deploy shipped.
const rollbackBadCommit = "0b1c2d3e4f5a69788796a5b4c3d2e1f0a9b8c7d6"

func rollbackDeploymentJSON(uid, source string, candidate bool, sha string, ready time.Time) string {
	return fmt.Sprintf(`{"uid":%q,"name":"storefront","projectId":%q,"target":"production",
		"readyState":"READY","readySubstate":"PROMOTED","source":%q,"isRollbackCandidate":%t,
		"inspectorUrl":"https://vercel.com/twin/storefront/%s",
		"creator":{"uid":"usr_ada","type":"user"},
		"attribution":{"commitMeta":{"githubCommitSha":%q}},
		"createdAt":%d,"buildingAt":%d,"ready":%d}`,
		uid, fixtureProject, source, candidate, uid, sha,
		ready.Add(-3*time.Minute).UnixMilli(), ready.Add(-2*time.Minute).UnixMilli(), ready.UnixMilli())
}

// --- payload builders -----------------------------------------------------------------------------

func vercelPayload(kind string, at time.Time, body string) feeder.Payload {
	return feeder.Payload{Kind: kind, At: at, Bytes: []byte(body)}
}

// vercelProjectsPayload states `createdAt`, as the platform's projects read always does. The project
// is described as a node dated from it (004 T093); a payload without it dates the node from the read,
// with its start marked unknown (FR-011). Before 004 T154 that fallback used the ingest instant, which
// a shuffle moves, and these fixtures were how that was found.
func vercelProjectsPayload(at time.Time) feeder.Payload {
	return vercelPayload(vercelfeeder.PayloadProjects, at, fmt.Sprintf(
		`{"projects":[{"id":%q,"name":"storefront","createdAt":%d,`+
			`"link":{"type":"github","org":"acme","repo":"storefront","repoId":555}}]}`,
		fixtureProject, fixtureProjectCreated.UnixMilli()))
}

// fixtureProjectCreated is when the twin project was created: well before any window here.
var fixtureProjectCreated = time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

func vercelDeploymentsPayload(at time.Time, deployments ...string) feeder.Payload {
	body := `{"deployments":[`
	for i, d := range deployments {
		if i > 0 {
			body += ","
		}
		body += d
	}
	return vercelPayload(vercelfeeder.PayloadDeployments, at, body+`]}`)
}

// vercelDeploymentJSON renders one deployment as the API states it.
//
// `readyState` and `readySubstate` are separate arguments on purpose: a builder that derived one from
// the other could not express the case this fixture exists for, which is READY without PROMOTED.
func vercelDeploymentJSON(uid, target, readyState, substate, sha string) string {
	return fmt.Sprintf(`{"uid":%q,"name":"storefront","projectId":%q,"target":%q,
		"readyState":%q,"readySubstate":%q,"source":"git","isRollbackCandidate":false,
		"inspectorUrl":"https://vercel.com/twin/storefront/%s",
		"creator":{"uid":"usr_ada","type":"user"},
		"attribution":{"commitMeta":{"githubCommitSha":%q}},
		"createdAt":%d,"buildingAt":%d,"ready":%d}`,
		uid, fixtureProject, target, readyState, substate, uid, sha,
		fixtureStart.Add(-6*time.Minute).UnixMilli(),
		fixtureStart.Add(-5*time.Minute).UnixMilli(),
		fixtureStart.Add(-1*time.Minute).UnixMilli())
}

func vercelPollPayload(at time.Time, outcome string) feeder.Payload {
	return vercelPayload(vercelfeeder.PayloadPollMarker, at, `{"outcome":"`+outcome+`"}`)
}

func appendVercelQueries(t *testing.T, dir, queries string) {
	t.Helper()
	path := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(existing, []byte("\n"+queries)...), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- the arrival clock ----------------------------------------------------------------------------

// vercelClock makes observed time track payload ARRIVAL, which is what a real recording produces.
type vercelClock struct {
	base time.Time
	step int64
	last time.Time
}

const vercelEventStep = time.Microsecond

func (c *vercelClock) Now() time.Time {
	at := c.base.Add(time.Duration(c.step) * vercelEventStep)
	c.step++
	if !c.last.IsZero() && !at.After(c.last) {
		at = c.last.Add(vercelEventStep)
	}
	c.last = at
	return at
}

func (c *vercelClock) wrap(src feeder.Source) feeder.Source {
	return vercelClockedSource{src: src, clock: c}
}

type vercelClockedSource struct {
	src   feeder.Source
	clock *vercelClock
}

func (s vercelClockedSource) Next(ctx context.Context) (feeder.Payload, error) {
	payload, err := s.src.Next(ctx)
	if err != nil {
		return payload, err
	}
	if !payload.At.IsZero() {
		s.clock.base, s.clock.step = payload.At, 0
	}
	return payload, nil
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
