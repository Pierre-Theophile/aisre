// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Generating the US1 fixtures (004 T073–T079).
//
// These are **synthetic structural twins**: the same event shapes, the same sequences and the same
// instants as a real recording, with no identifier derived from any organisation
// (contracts/sanitisation.md §7). Constitution VIII is explicit that synthetic-only data is not enough
// for a connector to be marked stable — the twins are written against the shapes we *expect*, so they
// cannot fail in the one way that matters, which is GitHub returning something nobody predicted. A
// recording campaign replaces them by swapping `payloads/`, not by changing any code, because the
// feeder cannot tell the difference (FR-044).
//
// Regenerate with:
//
//	SRE_AGENT_GEN_FIXTURES=1 go test ./internal/feeders/github -run TestGenerateFixtures
//
// The output is byte-for-byte reproducible: every instant and every identifier is a literal below, and
// nothing reads a clock.
//
// # github-unknown-start-01 is generated on demand rather than committed
//
// Seven fixtures are committed and all seven pass `fixture verify` in full — replay from empty, double
// delivery, the shuffle, and the golden comparison. One case cannot, and the reason is a property of
// the model rather than a defect anything here can fix.
//
// A change whose valid start is genuinely unknown begins **at the observation**, marked unknown: the
// earliest instant anybody can show. The observation instant is precisely what reordering changes, so
// such a change's valid interval is order dependent by construction, and the shuffle exists to find
// exactly that. Neither way out is worth taking: filling the start in with the poll instant is the
// guess T053 forbids, and withholding the change would drop a rollout the platform stated had
// succeeded. So the case is exercised by `unknownStartFixture` on demand and by the mapper's unit
// test, and it is not part of the committed corpus.
//
// This is the same shape as feature 003's `gcp-service-recreated-01`, which passes every step but the
// shuffle because a retraction and a re-assertion on one identifier do not commute.
//
// # github-monorepo-01 was held out for a different reason, and T148 removed it
//
// It failed the shuffle too, and that failure was a REAL finding rather than something to write off.
// `graph.identity_claims` is constrained `UNIQUE (namespace, value, source_id)`, so one identifier value
// from one source belongs to exactly one entity. A monorepo deployment is three changes sharing one
// commit, so the `deploy.commit_sha` claim could land on only one of them — whichever arrived first —
// and the graph's state depended on delivery order.
//
// The `github.repo` claim had the same defect and the fix there was clear: nothing reads it, so it
// became a property. `deploy.commit_sha` could not take that fix, because **C8 keys on it**: it is how a
// GitHub rollout is recognised as the same rollout the platform feeder observed.
//
// T148 is the answer, and it is neither of the two bad ones. The value stays where a rule can read it
// and stops being a NAME: it is a correlation key (`graph.correlation_keys`), unique per
// (entity, namespace, value, source), so all three changes carry it and C8 reads it from there. The
// fixture now passes every step and is committed as part of the corpus.

const genFixturesEnv = "SRE_AGENT_GEN_FIXTURES"

// The twin's estate. No name here belongs to anybody: `acme` owns `storefront` and `monorepo`, and the
// numeric ids are the ones every change identity is built from.
const (
	twinOrg          = "twin"
	twinOwner        = "acme"
	twinStorefront   = "storefront"
	twinMonorepo     = "monorepo"
	twinStorefrontID = 555
	twinMonorepoID   = 556
)

// The twin's clock. One window, so two fixtures of the same shape are comparable by eye.
var (
	fixtureStart  = time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	fixtureFirst  = time.Date(2026, 3, 1, 14, 5, 0, 0, time.UTC)
	fixturePoll   = time.Date(2026, 3, 1, 14, 30, 0, 0, time.UTC)
	fixtureSecond = time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC)
	fixtureEnd    = time.Date(2026, 3, 1, 15, 30, 0, 0, time.UTC)
)

const (
	twinSHA      = "8f5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4"
	twinOtherSHA = "1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d"
)

func TestGenerateFixtures(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the US1 fixtures", genFixturesEnv)
	}
	for _, fx := range fixtureSet(t) {
		writeFixture(t, fx)
	}
}

// TestGenerateUnknownStartFixture writes the one fixture that is not committed. Separate so that
// regenerating the corpus does not silently add a fixture `fixture verify fixtures/*/` would then fail
// on — which is how an uncommitted fixture turns into a red build for the next person.
func TestGenerateUnknownStartFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to generate the unknown-start fixture (not committed; see the file comment)",
			genFixturesEnv)
	}
	writeFixture(t, unknownStartFixture())
}

// TestGenerateMonorepoFixture writes the fixture held out pending T148's decision. Separate for the
// same reason as the one above.
func TestGenerateMonorepoFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to generate the monorepo fixture (not committed; see the file comment and T148)",
			genFixturesEnv)
	}
	writeFixture(t, monorepoFixture())
}

type fixtureSpec struct {
	dir         string
	family      string
	description string
	payloads    []feeder.Payload
	options     github.MapOptions
	queries     string
	end         time.Time
}

func (f fixtureSpec) clockEnd() time.Time {
	if f.end.IsZero() {
		return fixtureEnd
	}
	return f.end
}

func writeFixture(t *testing.T, fx fixtureSpec) {
	t.Helper()
	// `go test` runs with the working directory set to the package, so a fixture path is resolved
	// against the repository root. Writing it relative would silently create
	// `internal/feeders/github/fixtures/`.
	dir := filepath.Join(repoRoot(t), fx.dir)
	// Only what this function writes is cleared. `os.RemoveAll(dir)` was the first cut, and it took
	// `golden/` with it: regenerating the corpus deleted the frozen answers of every fixture it
	// touched, and `fixture verify` then had nothing left to disagree with. That is the worse of the
	// two failures. A stale golden that survives a regeneration makes `fixture verify` FAIL and name
	// the query, which is how a mapper change is supposed to be noticed (004 T145); a golden that
	// silently disappeared makes it pass. Recording is a separate, deliberate step for the same
	// reason, so the generator must never do it by implication.
	for _, generated := range []string{"payloads", "events.jsonl", "manifest.yaml"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatalf("clear %s: %v", filepath.Join(dir, generated), err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	f, err := github.New(github.Options{OrgSlug: twinOrg, Map: fx.options})
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	desc := f.Describe()

	// Observed time tracks payload ARRIVAL, which is what a real recording produces.
	clock := &arrivalClock{base: fixtureStart}
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
	// The window must contain every event the fixture holds (004 T153): checked at generation so an
	// `end` that is too early fails with the numbers in front of whoever set it.
	if last := clock.last; last.After(fx.clockEnd()) {
		t.Fatalf("%s: the last event is observed at %s, after the declared clock.end %s; widen `end`",
			dir, last.Format(time.RFC3339Nano), fx.clockEnd().Format(time.RFC3339Nano))
	}
	if err := record.WriteManifest(dir, record.Manifest{
		Family: fx.family,
		Description: fx.description +
			" Synthetic structural twin: no identifier is derived from any organisation " +
			"(contracts/sanitisation.md §7), and constitution VIII says synthetic-only data is not " +
			"enough for a connector to be marked stable — a recording campaign replaces it by " +
			"swapping payloads/, not by changing code.",
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		Start:          fixtureStart,
		End:            fx.clockEnd(),
		ExpectRejected: events.Rejections(),
	}); err != nil {
		t.Fatalf("%s: write manifest: %v", dir, err)
	}
	if fx.queries != "" {
		appendQueries(t, dir, fx.queries)
	}
	t.Logf("%s: %d payloads, %d events", fx.dir, src.Count(), events.Accepted())
}

func appendQueries(t *testing.T, dir, queries string) {
	t.Helper()
	path := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(existing, []byte(queries)...), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

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
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// arrivalClock hands out observed times that track payload arrival, so both conformance steps —
// replay from empty and double delivery — are meaningful at once.
type arrivalClock struct {
	base time.Time
	step int64
	last time.Time
}

const eventStep = time.Microsecond

func (c *arrivalClock) Now() time.Time {
	at := c.base.Add(time.Duration(c.step) * eventStep)
	c.step++
	if !c.last.IsZero() && !at.After(c.last) {
		at = c.last.Add(eventStep)
	}
	c.last = at
	return at
}

func (c *arrivalClock) wrap(src feeder.Source) feeder.Source {
	return clockedSource{src: src, clock: c}
}

type clockedSource struct {
	src   feeder.Source
	clock *arrivalClock
}

func (s clockedSource) Next(ctx context.Context) (feeder.Payload, error) {
	payload, err := s.src.Next(ctx)
	if err != nil {
		return payload, err
	}
	if !payload.At.IsZero() {
		s.clock.base, s.clock.step = payload.At, 0
	}
	return payload, nil
}

// --- payload builders ----------------------------------------------------------------------------

func payloadAt(kind string, at time.Time, body string) feeder.Payload {
	return feeder.Payload{Kind: kind, At: at, Bytes: []byte(body)}
}

// scopePayload is the installation's grant. Every change identity is built from these numeric ids.
func scopePayload(at time.Time, repos ...string) feeder.Payload {
	return payloadAt(github.PayloadInstallationRepositories, at, fmt.Sprintf(
		`{"total_count":%d,"repository_selection":"selected","repositories":[%s]}`,
		len(repos), strings.Join(repos, ",")))
}

func repository(id int64, name string) string {
	return fmt.Sprintf(`{"id":%d,"name":%q,"full_name":"%s/%s","private":true,"owner":{"login":%q}}`,
		id, name, twinOwner, name, twinOwner)
}

type deploymentSpec struct {
	id          int64
	repo        string
	sha         string
	environment string
	production  string // "true", "false" or "" for a payload that states nothing
	creator     string
}

func (d deploymentSpec) json() string {
	production := ""
	if d.production != "" {
		production = `"production_environment":` + d.production + `,`
	}
	creator := d.creator
	if creator == "" {
		creator = `{"login":"ada","id":1,"type":"User"}`
	}
	return fmt.Sprintf(`{"id":%d,"sha":%q,"ref":"main","task":"deploy","environment":%q,%s
		"created_at":"2026-03-01T14:00:00Z","updated_at":"2026-03-01T14:03:12Z","creator":%s,
		"url":"https://api.github.com/repos/%s/%s/deployments/%d",
		"statuses_url":"https://api.github.com/repos/%s/%s/deployments/%d/statuses"}`,
		d.id, d.sha, d.environment, production, creator,
		twinOwner, d.repo, d.id, twinOwner, d.repo, d.id)
}

func deploymentsPayload(at time.Time, specs ...deploymentSpec) feeder.Payload {
	bodies := make([]string, 0, len(specs))
	for _, spec := range specs {
		bodies = append(bodies, spec.json())
	}
	return payloadAt(github.PayloadDeployments, at, "["+strings.Join(bodies, ",")+"]")
}

type statusSpec struct {
	id    int64
	state string
	at    string
	// logURL is the deployer-supplied log link. One fixture carries a token in it on purpose: it is
	// exactly where one arrives in practice, and no pointer may ever carry it (FR-014).
	logURL string
}

func (s statusSpec) json(environment string) string {
	logURL := ""
	if s.logURL != "" {
		logURL = fmt.Sprintf(`,"log_url":%q`, s.logURL)
	}
	// An instant GitHub did not state is an ABSENT field, not an empty string: the API would never
	// send `"created_at": ""`, and a fixture that did would be testing this code against a payload
	// shape the platform cannot produce.
	createdAt := ""
	if s.at != "" {
		createdAt = fmt.Sprintf(`"created_at":%q,`, s.at)
	}
	return fmt.Sprintf(`{"id":%d,"state":%q,"environment":%q,%s
		"creator":{"login":"ada","id":1,"type":"User"}%s}`, s.id, s.state, environment, createdAt, logURL)
}

func statusesPayload(at time.Time, repo string, deployment int64, environment string, specs ...statusSpec) feeder.Payload {
	bodies := make([]string, 0, len(specs))
	for _, spec := range specs {
		bodies = append(bodies, spec.json(environment))
	}
	return payloadAt(github.PayloadDeploymentStatuses, at, fmt.Sprintf(
		`{"repository":"%s/%s","deployment_id":%d,"statuses":[%s]}`,
		twinOwner, repo, deployment, strings.Join(bodies, ",")))
}

type runSpec struct {
	id      int64
	name    string
	attempt int
	sha     string
	event   string
	actor   string
	trigger string
}

func (r runSpec) json() string {
	actor, trigger := r.actor, r.trigger
	if actor == "" {
		actor = `{"login":"ada","id":1,"type":"User"}`
	}
	if trigger == "" {
		trigger = actor
	}
	attempt := r.attempt
	if attempt == 0 {
		attempt = 1
	}
	return fmt.Sprintf(`{"id":%d,"name":%q,"run_number":12,"run_attempt":%d,"head_sha":%q,"event":%q,
		"status":"completed","conclusion":"success","run_started_at":"2026-03-01T14:00:00Z",
		"actor":%s,"triggering_actor":%s,
		"html_url":"https://github.com/%s/%s/actions/runs/%d",
		"logs_url":"https://api.github.com/repos/%s/%s/actions/runs/%d/logs"}`,
		r.id, r.name, attempt, r.sha, r.event, actor, trigger,
		twinOwner, twinMonorepo, r.id, twinOwner, twinMonorepo, r.id)
}

func runsPayload(at time.Time, specs ...runSpec) feeder.Payload {
	bodies := make([]string, 0, len(specs))
	for _, spec := range specs {
		bodies = append(bodies, spec.json())
	}
	return payloadAt(github.PayloadWorkflowRuns, at, fmt.Sprintf(
		`{"total_count":%d,"workflow_runs":[%s]}`, len(specs), strings.Join(bodies, ",")))
}

func releasesPayload(at time.Time, repo string, bodies ...string) feeder.Payload {
	return payloadAt(github.PayloadReleases, at, fmt.Sprintf(
		`{"repository":"%s/%s","releases":[%s]}`, twinOwner, repo, strings.Join(bodies, ",")))
}

func release(id int64, tag, commitish, published string, draft, prerelease bool) string {
	return fmt.Sprintf(`{"id":%d,"tag_name":%q,"name":%q,"target_commitish":%q,"draft":%t,
		"prerelease":%t,"created_at":"2026-03-01T14:00:00Z","published_at":%q,
		"author":{"login":"ada","id":1,"type":"User"},
		"html_url":"https://github.com/%s/%s/releases/tag/%s"}`,
		id, tag, tag, commitish, draft, prerelease, published, twinOwner, twinStorefront, tag)
}

func pollMarker(at time.Time, outcome string) feeder.Payload {
	return payloadAt(github.PayloadPollMarker, at, `{"outcome":"`+outcome+`"}`)
}

// partialPollMarker is a poll that stopped part-way, with the reason the poller knew.
//
// The reason is optional in the payload and supplied here, because a partial poll that cannot say why
// is the weaker of the two honest states and a fixture should exercise the stronger one.
func partialPollMarker(at time.Time, reason string) feeder.Payload {
	return payloadAt(github.PayloadPollMarker, at,
		`{"outcome":"partial","reason":`+strconv.Quote(reason)+`}`)
}

// --- the operator's configuration ----------------------------------------------------------------

func storefrontOptions() github.MapOptions {
	return github.MapOptions{
		Allowlist: github.Allowlist{
			Environments: []string{"production"},
			Workflows:    []string{"Deploy"},
		},
		Targets: github.TargetMap{Repositories: map[string][]github.TargetRule{
			twinOwner + "/" + twinStorefront: {
				{Namespace: feeder.NSK8sDeployment, Value: "shop/storefront"},
			},
		}},
		Actors: github.ActorPolicy{DeploymentAutomation: []string{"release-runner"}},
	}
}

func monorepoOptions() github.MapOptions {
	opts := storefrontOptions()
	opts.Targets.Repositories[twinOwner+"/"+twinMonorepo] = []github.TargetRule{
		{Environment: "production", Namespace: feeder.NSK8sDeployment, Value: "shop/checkout"},
		{Environment: "production", Namespace: feeder.NSK8sDeployment, Value: "shop/catalogue"},
		{Environment: "production", Workflow: "Deploy search",
			Namespace: feeder.NSK8sDeployment, Value: "shop/search"},
	}
	return opts
}
