// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Generating the US1 fixtures (T063–T066).
//
// These are **synthetic structural twins**: the same event shapes, the same sequences and the same
// instants as a real recording, with no identifier derived from the organisation. The sanitisation
// contract's §7 split is why they exist in this repository at all — a claim made here is a claim about
// the twins, and a property that can only be demonstrated on real payloads is labelled as such rather
// than implied.
//
// They are authored rather than recorded, and the manifests say so. Constitution VIII is explicit
// that synthetic-only data is **not enough** for a connector to be marked stable: the twins are
// written against the shapes we *expect*, so they cannot fail in the one way that matters, which is
// GCP returning something nobody predicted. US7's recording campaign is what covers that, and it
// replaces these by swapping the payload directory — not by changing any code, because the feeder
// cannot tell the difference (FR-044).
//
// Regenerate with:
//
//	SRE_AGENT_GEN_FIXTURES=1 go test ./internal/feeders/gcp -run TestGenerateFixtures
//
// The output is byte-for-byte reproducible: every instant and every identifier is a literal below,
// and nothing reads a clock.
//
// # gcp-service-recreated-01 was held out, and was not a property of the model
//
// It failed the shuffle, and for a while that was recorded as unavoidable: "a retraction and a
// re-assertion on one identifier do not commute". They do — the projector's segment planner makes an
// assertion made at or after a retraction resurrect the entity, in any order (projector/segments.go).
// What was actually wrong were two defects the fixture could see and nothing else did (003 T066):
//
//   - the service's event id was its ref alone, so the recreated service's assertion was a DUPLICATE
//     of the first one and never reached the graph. The service was retracted and simply gone. The
//     same id froze every service at its first poll: a traffic split that moved was never seen on the
//     node. serviceEventID (map.go) now names each state by the uid and `updateTime`.
//   - a `changed_by` edge is written from an `observe_change`, so no edge assertion produced it, and a
//     node retraction's cascade dropped it as though it had been asserted before the retraction —
//     when the retraction happened to be applied after the change (projector/retract_node.go,
//     laterUnassertedVersion).
//
// Both are fixed and the fixture is committed with the rest of the corpus.
const genFixturesEnv = "SRE_AGENT_GEN_FIXTURES"

// The fixture clock. One deploy: a revision created at 14:18, given all the traffic at 14:20, rolled
// back at 14:41. Those three instants are the whole of what US1 has to get right.
var (
	fixtureStart      = time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	fixtureCreated    = time.Date(2026, 9, 21, 14, 18, 0, 0, time.UTC)
	fixtureShifted    = time.Date(2026, 9, 21, 14, 20, 0, 0, time.UTC)
	fixtureRolledBack = time.Date(2026, 9, 21, 14, 41, 0, 0, time.UTC)
)

// The twin's identifiers. None is derived from the organisation (§7): a project called
// `twin-production`, services called `storefront` and `orders`, a team called `platform`.
const (
	twinProject = "twin-production"
	twinRegion  = "europe-west1"
	twinService = "storefront"
	twinOther   = "orders"
	twinRevOld  = "storefront-00041-aaa"
	twinRevNew  = "storefront-00042-bbb"
	twinTeam    = "platform"
	// twinInstance is the Cloud SQL instance the US5 twins turn on, and twinLedger the one nothing
	// names — the case that must produce no invented edge (T139).
	twinInstance = "orders-primary"
	twinLedger   = "billing-ledger"
	twinCluster  = "inference"
	// twinFingerprintKey keys the configuration fingerprints in the generated corpus. See
	// writeFixture: a committed key is reproducible and is not a production one.
	twinFingerprintKey = "twin-fixture-fingerprint-key"
)

// twinConnectionName renders an instance's connection name, which is what a Cloud Run service is
// configured with and what FR-120's certain rule resolves on.
func twinConnectionName(instance string) string {
	return twinProject + ":" + twinRegion + ":" + instance
}

func TestGenerateFixtures(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the US1 fixtures", genFixturesEnv)
	}
	for _, fx := range fixtureSet(t) {
		writeFixture(t, fx)
	}
}

// fixtureSpec is one fixture to generate.
type fixtureSpec struct {
	dir         string
	family      string
	description string
	payloads    []feeder.Payload
	// queries are appended to the manifest by hand after generation; WriteManifest does not write
	// them, and a fixture with no queries verifies nothing — which the description says out loud so
	// that committing one unfinished is a visible omission rather than a silent one.
	queries string
	// rejected are hand-authored events that must ALWAYS be refused, written to
	// `rejected.jsonl` beside the accepted stream. They cannot come from the feeder: the feeder
	// under test never emits a telemetry payload, which is precisely why the refusal needs a
	// fixture that carries one anyway. Each entry is one JSON object, exactly as the event log
	// receives it.
	rejected []string
	// start and end are the fixture's clock. Zero means the corpus default — fixtureStart to one
	// hour later — and a fixture states its own only when its story needs a different span: the
	// announced-maintenance twin is read in September for a window in October, exactly as
	// vendor-maintenance-future-01 is.
	start, end time.Time
	// omittedSurfaces overrides the surfaces this fixture's run declares it did not read (FR-057).
	// Empty uses the generator's default pair, which is what every other fixture states.
	omittedSurfaces []string
	// incidentsEnabled turns on the alert-incident capability for this fixture. It is per fixture
	// because one of them exists to assert the flag being OFF, and a generator that enabled it
	// everywhere could not produce that fixture at all.
	incidentsEnabled bool
	// expectRejected states the reason code each of those events must be refused with. A
	// fixture that asserted only "it was refused" would pass on a refusal for the wrong reason,
	// and a refusal for the wrong reason is a rule that is not being enforced.
	expectRejected []record.Rejection
}

// clockStart and clockEnd default the fixture's clock to the corpus one.
func (fx fixtureSpec) clockStart() time.Time {
	if fx.start.IsZero() {
		return fixtureStart
	}
	return fx.start
}

// clockEnd is the end of the window the fixture declares it covers.
//
// The default used to be a flat hour after the start, and six fixtures polled later than that: their
// last cycle arrived at `cycleAt(2)` or later while the window closed at `cycleAt(2)` exactly, so the
// arrival clock's microsecond offsets put their final events OUTSIDE the declared window — by a full
// hour in `gcp-rollback-01`. The pinned pass answers as known at clock.end, so those events vanished
// from every pinned answer without anything saying so (004 T153). The default now closes the window
// one cycle after the last poll, and never earlier than it used to, so a fixture that was already
// correct keeps its clock.
func (fx fixtureSpec) clockEnd() time.Time {
	if !fx.end.IsZero() {
		return fx.end
	}
	end := fx.clockStart().Add(time.Hour)
	for _, payload := range fx.payloads {
		if after := payload.At.Add(cycleGap); after.After(end) {
			end = after
		}
	}
	return end
}

// omitted is the surfaces this fixture's run declares it did not read. The default pair is the one
// every fixture before US9 stated: these surfaces are P3 and off by default (FR-057).
func (fx fixtureSpec) omitted() []string {
	if len(fx.omittedSurfaces) > 0 {
		return fx.omittedSurfaces
	}
	return []string{"cloud_dns", "load_balancers"}
}

func fixtureSet(t *testing.T) []fixtureSpec {
	t.Helper()
	return []fixtureSpec{
		baselineTopologyFixture(t),
		rolloutTrafficShiftFixture(t),
		revisionZeroTrafficFixture(t),
		telemetryRejectionFixture(t),
		alertTransitionFixture(t),
		groupedAlertFixture(t),
		forgedDoorbellFixture(t),
		alertTransitionsUnavailableFixture(t),
		rollbackFixture(t),
		partialPollFixture(t),
		cloudSQLFlagChangeFixture(t),
		proposedDependencyFixture(t),
		cloudSQLScheduledMaintenanceFixture(t),
		auditOtherKindFixture(t),
		auditControllerEffectFixture(t),
		quotaExhaustedFixture(t),
		lbDNSFixture(t),
		lbDNSOmittedFixture(t),
		// T066, committed since the two defects recorded in the file comment were fixed.
		serviceRecreatedFixture(t),
	}
}

// writeFixture replays the payloads through the feeder with both recorders in place, which is exactly
// what `feed gcp --record` does — so the fixture is what the feeder produced, not what somebody
// thought it would produce.
func writeFixture(t *testing.T, fx fixtureSpec) {
	t.Helper()
	// `go test` runs with the working directory set to the package, so a fixture path has to be
	// resolved against the repository root. Writing it relative would silently create
	// `internal/feeders/gcp/fixtures/` — which is what happened the first time this ran, and the run
	// reported success.
	dir := filepath.Join(repoRoot(t), fx.dir)
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
		OmittedSurfaces:     fx.omitted(),
		IncidentsAPIEnabled: fx.incidentsEnabled,
		// The twin's configuration fingerprint key. It is a literal here so the generated corpus is
		// byte-for-byte reproducible, and it is not a production key — the point of the key is that
		// a fingerprint is not a digest anybody can reverse by enumeration, and a key committed to a
		// repository would not satisfy that for a real deployment (config.go).
		FingerprintKey: []byte(twinFingerprintKey),
	})
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	desc := f.Describe()

	// Observed time tracks **payload arrival**, which is what a real recording produces and what
	// makes both conformance steps meaningful at once. See arrivalClock.
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
	// The window must contain every event the fixture holds (004 T153). Checked here as well as by
	// `fixture verify`, so an explicit `end` that is too early fails at generation with the numbers
	// in front of whoever set it, rather than later as a verification step.
	if last := clock.last; last.After(fx.clockEnd()) {
		t.Fatalf("%s: the last event is observed at %s, after the declared clock.end %s; widen `end`",
			dir, last.Format(time.RFC3339Nano), fx.clockEnd().Format(time.RFC3339Nano))
	}
	if err := record.WriteManifest(dir, record.Manifest{
		Family: fx.family,
		Description: fx.description +
			" Synthetic structural twin: no identifier is derived from the organisation " +
			"(contracts/sanitisation.md §7), and constitution VIII says synthetic-only data is not " +
			"enough for a connector to be marked stable — US7's recording replaces it by swapping " +
			"payloads/, not by changing code.",
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		Start:          fx.clockStart(),
		End:            fx.clockEnd(),
		ExpectRejected: append(events.Rejections(), fx.expectRejected...),
	}); err != nil {
		t.Fatalf("%s: write manifest: %v", dir, err)
	}
	if len(fx.rejected) > 0 {
		writeRejected(t, dir, fx.rejected)
	}
	if fx.queries != "" {
		appendQueries(t, dir, fx.queries)
	}
	t.Logf("%s: %d payloads, %d events", fx.dir, src.Count(), events.Accepted())
}

// writeRejected writes the hand-authored refusals beside the recorded stream.
//
// They are written AFTER WriteManifest rather than before, because WriteManifest names
// `rejected.jsonl` only when the file exists, and a manifest that named a file the recorder had
// not written yet would be a manifest describing a fixture that did not exist.
func writeRejected(t *testing.T, dir string, events []string) {
	t.Helper()
	path := filepath.Join(dir, "rejected.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(events, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	manifestPath := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", manifestPath, err)
	}
	if err := os.WriteFile(manifestPath,
		append(existing, []byte("rejected_events: rejected.jsonl\n")...), 0o644); err != nil {
		t.Fatalf("write %s: %v", manifestPath, err)
	}
}

// appendQueries adds the manifest's `queries:` block. WriteManifest deliberately does not know about
// queries — they are a statement about what the fixture proves, and that is authored rather than
// derived.
func appendQueries(t *testing.T, dir, queries string) {
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

func twinLabelPolicy() gcpfeeder.LabelPolicy {
	policy := gcpfeeder.DefaultLabelPolicy()
	// The twin's mapping is stated in the fixture rather than taken from a default, because no
	// checked-in default may assume a project name (FR-131) — and the checkpoint records which
	// mapping produced each node's environment.
	policy.EnvironmentFromProject = map[string]string{twinProject: "production"}
	return policy
}

func twinActorPolicy() gcpfeeder.ActorPolicy {
	return gcpfeeder.ActorPolicy{
		DeploymentAutomation: []string{"deploy@" + twinProject + ".iam.gserviceaccount.com"},
		HumanDirectory:       []string{"operator@example.com"},
	}
}

// --- payload builders -----------------------------------------------------------------------------

// twinService renders one Cloud Run service as the API returns it.
func twinServiceJSON(uid string, generation int64, createTime, updateTime time.Time, latestReady, latestCreated string, split map[string]int, labels map[string]string) string {
	statuses := make([]string, 0, len(split))
	for _, rev := range sortedKeys(split) {
		statuses = append(statuses, fmt.Sprintf(
			`{"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","revision":%q,"percent":%d}`, rev, split[rev]))
	}
	labelJSON, _ := json.Marshal(labels)
	return fmt.Sprintf(`{
  "name": "projects/%s/locations/%s/services/%s",
  "uid": %q,
  "generation": "%d",
  "observedGeneration": "%d",
  "createTime": %q,
  "updateTime": %q,
  "labels": %s,
  "ingress": "INGRESS_TRAFFIC_ALL",
  "latestReadyRevision": "projects/%s/locations/%s/services/%s/revisions/%s",
  "latestCreatedRevision": "projects/%s/locations/%s/services/%s/revisions/%s",
  "trafficStatuses": [%s],
  "template": {"containers": [{"image": "europe-docker.pkg.dev/%s/twin/%s@sha256:0000000000000000000000000000000000000000000000000000000000000041",
    "env": [{"name": "OTEL_SERVICE_NAME", "value": %q}]}]}
}`, twinProject, twinRegion, twinService, uid, generation, generation,
		rfc3339(createTime), rfc3339(updateTime),
		labelJSON,
		twinProject, twinRegion, twinService, latestReady,
		twinProject, twinRegion, twinService, latestCreated,
		joinComma(statuses), twinProject, twinService, twinService)
}

// twinRevisionJSON renders one Cloud Run revision.
func twinRevisionJSON(name string, createTime time.Time, digestSuffix string) string {
	return fmt.Sprintf(`{
  "name": "projects/%s/locations/%s/services/%s/revisions/%s",
  "uid": "uid-%s",
  "generation": "1",
  "createTime": %q,
  "labels": {"team": %q},
  "serviceAccount": "runtime@%s.iam.gserviceaccount.com",
  "executionEnvironment": "EXECUTION_ENVIRONMENT_GEN2",
  "containers": [{"image": "europe-docker.pkg.dev/%s/twin/%s@sha256:00000000000000000000000000000000000000000000000000000000000000%s"}],
  "conditions": [{"type": "Ready", "state": "CONDITION_SUCCEEDED"}]
}`, twinProject, twinRegion, twinService, name, name, rfc3339(createTime), twinTeam,
		twinProject, twinProject, twinService, digestSuffix)
}

// twinAuditPair renders the two entries of one UpdateService operation: the request entry and the
// completion entry whose timestamp is the instant the split took effect.
//
// # There is no principal in these entries, and that is the point
//
// A sanitised audit entry has **no** `authenticationInfo` and no `requestMetadata` at all — the
// principal is dropped, never renamed or hashed (FR-135), and `callerIp` is where a person was sitting
// rather than a machine that serves traffic. `scripts/check-no-secrets.sh` enforces exactly that, on
// the *field name*, because in a recording the presence of the name is the leak. A twin of a sanitised
// recording therefore does not carry them either.
//
// The consequence is worth stating rather than working around: **a sanitised corpus cannot test actor
// classification**, because the input to the classification is precisely what sanitisation drops. What
// survives is the classification's *output* — the actor kind, a closed enumeration recorded verbatim
// (contracts/sanitisation.md §2.2). So the ladder is asserted by actorkind_test.go, and the
// request-entry-versus-completion-entry principal rule end to end by
// TestASplitChangeWithACompletionEntryIsDatedFromThatEntry in map_test.go, where a principal can exist
// because nothing is being committed. The fixtures assert what they can: the instant, the correlation
// by `operation.id`, and the two changes at their two distinct instants.
func twinAuditPair(opID string, requestedAt, completedAt time.Time) string {
	return fmt.Sprintf(`{"entries":[
  {"insertId":"%s-req","logName":"projects/%s/logs/cloudaudit.googleapis.com%%2Factivity",
   "timestamp":%q,"receiveTimestamp":%q,
   "operation":{"id":%q,"producer":"run.googleapis.com","first":true},
   "protoPayload":{"serviceName":"run.googleapis.com",
     "methodName":"google.cloud.run.v2.Services.UpdateService",
     "resourceName":"projects/%s/locations/%s/services/%s"}},
  {"insertId":"%s-done","logName":"projects/%s/logs/cloudaudit.googleapis.com%%2Factivity",
   "timestamp":%q,"receiveTimestamp":%q,
   "operation":{"id":%q,"producer":"run.googleapis.com","last":true},
   "protoPayload":{"serviceName":"run.googleapis.com",
     "methodName":"google.cloud.run.v2.Services.UpdateService",
     "resourceName":"projects/%s/locations/%s/services/%s",
     "status":{"code":0}}}
]}`, opID, twinProject, rfc3339(requestedAt), rfc3339(requestedAt.Add(4*time.Second)), opID,
		twinProject, twinRegion, twinService,
		opID, twinProject, rfc3339(completedAt), rfc3339(completedAt.Add(5*time.Second)), opID,
		twinProject, twinRegion, twinService)
}

func servicesPayloadAt(t *testing.T, at time.Time, services ...string) feeder.Payload {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(services))
	for _, svc := range services {
		raw = append(raw, json.RawMessage(svc))
	}
	body, err := json.Marshal(map[string]any{"services": raw})
	if err != nil {
		t.Fatalf("marshal services: %v", err)
	}
	return feeder.Payload{Kind: gcpfeeder.PayloadServices, At: at, Bytes: body}
}

func revisionsPayloadAt(t *testing.T, at time.Time, revisions ...string) feeder.Payload {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(revisions))
	for _, rev := range revisions {
		raw = append(raw, json.RawMessage(rev))
	}
	body, err := json.Marshal(map[string]any{"revisions": raw})
	if err != nil {
		t.Fatalf("marshal revisions: %v", err)
	}
	return feeder.Payload{Kind: gcpfeeder.PayloadRevisions, At: at, Bytes: body}
}

func auditPayloadAt(at time.Time, body string) feeder.Payload {
	return feeder.Payload{Kind: gcpfeeder.PayloadAuditEntries, At: at, Bytes: []byte(body)}
}

func pollPayloadAt(at time.Time, outcome, reason string) feeder.Payload {
	body := fmt.Sprintf(`{"outcome":%q,"projects":[%q],"regions":[%q]`, outcome, twinProject, twinRegion)
	if reason != "" {
		body += fmt.Sprintf(`,"reason":%q`, reason)
	}
	body += "}"
	return feeder.Payload{Kind: gcpfeeder.PayloadPollMarker, At: at, Bytes: []byte(body)}
}

// deferredPollPayloadAt renders a poll the budget cut short: the areas it dropped, from the published
// order, and the typed stop reason that keeps "we ran out of quota" from reading as "we looked".
func deferredPollPayloadAt(at time.Time, reason string, deferred ...gcpx.Area) feeder.Payload {
	names := make([]string, 0, len(deferred))
	for _, area := range deferred {
		names = append(names, fmt.Sprintf("%q", area))
	}
	body := fmt.Sprintf(`{"outcome":"partial","projects":[%q],"regions":[%q],"reason":%q,`+
		`"stop_reason":%q,"deferred":[%s]}`,
		twinProject, twinRegion, reason, gcpx.StopQuota, strings.Join(names, ","))
	return feeder.Payload{Kind: gcpfeeder.PayloadPollMarker, At: at, Bytes: []byte(body)}
}

func alertPoliciesPayloadAt(t *testing.T, at time.Time, policies ...string) feeder.Payload {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(policies))
	for _, p := range policies {
		raw = append(raw, json.RawMessage(p))
	}
	body, err := json.Marshal(map[string]any{"alertPolicies": raw})
	if err != nil {
		t.Fatalf("marshal alert policies: %v", err)
	}
	return feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: body}
}

func alertsPayloadAt(t *testing.T, at time.Time, incidents ...string) feeder.Payload {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(incidents))
	for _, i := range incidents {
		raw = append(raw, json.RawMessage(i))
	}
	body, err := json.Marshal(map[string]any{"alerts": raw})
	if err != nil {
		t.Fatalf("marshal alerts: %v", err)
	}
	return feeder.Payload{Kind: gcpfeeder.PayloadAlerts, At: at, Bytes: body}
}

// twinPolicyJSON renders one alert policy as `projects.alertPolicies.list` returns it. groupBy is
// the condition's aggregation grouping: empty makes an ungrouped policy, and naming a field is what
// makes it open one incident per group.
func twinPolicyJSON(id, displayName, severity string, groupBy []string) string {
	filter := fmt.Sprintf(`metric.type="run.googleapis.com/request_count" AND `+
		`resource.type="cloud_run_revision" AND resource.labels.project_id="%s" AND `+
		`resource.labels.location="%s" AND resource.labels.service_name="%s"`,
		twinProject, twinRegion, twinService)
	condition := map[string]any{
		"displayName": "5xx above 1%",
		"conditionThreshold": map[string]any{
			"filter":         filter,
			"comparison":     "COMPARISON_GT",
			"thresholdValue": 0.01,
		},
	}
	if len(groupBy) > 0 {
		condition["conditionThreshold"].(map[string]any)["aggregations"] = []any{
			map[string]any{"alignmentPeriod": "60s", "groupByFields": groupBy},
		}
	}
	policy := map[string]any{
		"name":           "projects/" + twinProject + "/alertPolicies/" + id,
		"displayName":    displayName,
		"combiner":       "OR",
		"enabled":        true,
		"userLabels":     map[string]string{"team": twinTeam, "environment": "production"},
		"creationRecord": map[string]any{"mutateTime": rfc3339(fixtureStart.Add(-720 * time.Hour))},
		"conditions":     []any{condition},
	}
	if severity != "" {
		policy["severity"] = severity
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// twinIncidentJSON renders one alerting incident as `projects.alerts.list` returns it. revision is
// empty for an ungrouped policy's incident.
func twinIncidentJSON(id, policyID, state string, open, closed time.Time, revision string) string {
	labels := map[string]string{
		"project_id":   twinProject,
		"location":     twinRegion,
		"service_name": twinService,
	}
	if revision != "" {
		labels["revision_name"] = revision
	}
	body := map[string]any{
		"name":     "projects/" + twinProject + "/alerts/" + id,
		"state":    state,
		"openTime": rfc3339(open),
		"policy": map[string]any{
			"name":        "projects/" + twinProject + "/alertPolicies/" + policyID,
			"displayName": "storefront 5xx above 1%",
			"severity":    "ERROR",
		},
		"resource": map[string]any{"type": "cloud_run_revision", "labels": labels},
	}
	if !closed.IsZero() {
		body["closeTime"] = rfc3339(closed)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func joinComma(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += ","
		}
		out += part
	}
	return out
}

// repoRoot returns the repository root, found by walking up to the directory holding go.mod.
//
// It is here rather than assumed as "../../.." because a relative hop count is a fact about this
// file's depth, and moving the file would then write a fixture tree somewhere nobody looks.
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

// arrivalClock assigns observed times the way a real recording produces them: a base per payload,
// taken from when that payload arrived, and a microsecond step per event within it.
//
// Both halves are load-bearing, and each was arrived at by a conformance step failing.
//
// **Distinct per event.** An `UpsertEdge` whose destination the graph has not seen opens a placeholder
// entity version; the `UpsertNode` for that entity then supersedes it. If both carry the *same*
// observed time and the shuffle delivers the edge first, the node has to close an observed interval at
// the instant it opened, which the projector refuses (`close_observed: closed_at is not after the
// observed lower bound`). Every OWNER node in every one of these fixtures is that pair. A step of one
// microsecond is enough, and it is what feature 002's recorded fixtures carry for the same reason.
//
// **Grouped by payload arrival.** A fact asserted with `ValidFromUnknown` has no valid start from the
// source, so the graph bounds it by when it learned it — and the shuffle keeps the observed-time slots
// and permutes which event lands in which. If the owner node is asserted on every poll cycle and the
// cycles are inside one reordering window, permuting them moves that bound and the shuffle fails for a
// reason that says nothing about the feeder. Spacing the cycles beyond the window (cycleGap) keeps the
// permutation inside a cycle, which is the reordering a poll can genuinely produce.
type arrivalClock struct {
	// base is the arrival instant of the payload being processed.
	base time.Time
	// step counts events within that payload.
	step int64
	// last is the highest instant handed out, so observed time only ever moves forwards even if a
	// payload were to arrive out of order.
	last time.Time
}

// eventStep is the gap between two events of one payload. A microsecond, because it is bookkeeping
// rather than a claim about duration: the facts' *valid* times are the instants that matter, and those
// come from the payload content.
const eventStep = time.Microsecond

// Now returns the next observed time.
func (c *arrivalClock) Now() time.Time {
	at := c.base.Add(time.Duration(c.step) * eventStep)
	c.step++
	if !c.last.IsZero() && !at.After(c.last) {
		at = c.last.Add(eventStep)
	}
	c.last = at
	return at
}

// wrap returns src with the clock rebased on each payload's arrival time as it is read.
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

// --- US5 payload builders -------------------------------------------------------------------------

func sqlInstancesPayloadAt(t *testing.T, at time.Time, instances ...string) feeder.Payload {
	t.Helper()
	return itemsPayloadAt(t, gcpfeeder.PayloadSQLInstances, at, instances...)
}

func sqlOperationsPayloadAt(t *testing.T, at time.Time, operations ...string) feeder.Payload {
	t.Helper()
	return itemsPayloadAt(t, gcpfeeder.PayloadSQLOperations, at, operations...)
}

func sqlFlagsPayloadAt(t *testing.T, at time.Time, flags ...string) feeder.Payload {
	t.Helper()
	return itemsPayloadAt(t, gcpfeeder.PayloadSQLFlags, at, flags...)
}

// itemsPayloadAt wraps a list in the `items` envelope the Cloud SQL Admin REST API returns.
func itemsPayloadAt(t *testing.T, kind string, at time.Time, items ...string) feeder.Payload {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		raw = append(raw, json.RawMessage(item))
	}
	body, err := json.Marshal(map[string]any{"items": raw})
	if err != nil {
		t.Fatalf("marshal %s: %v", kind, err)
	}
	return feeder.Payload{Kind: kind, At: at, Bytes: body}
}

func gkeClustersPayloadAt(t *testing.T, at time.Time, clusters ...string) feeder.Payload {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(clusters))
	for _, cluster := range clusters {
		raw = append(raw, json.RawMessage(cluster))
	}
	body, err := json.Marshal(map[string]any{"clusters": raw})
	if err != nil {
		t.Fatalf("marshal clusters: %v", err)
	}
	return feeder.Payload{Kind: gcpfeeder.PayloadGKEClusters, At: at, Bytes: body}
}

// twinSQLInstanceJSON renders one Cloud SQL instance as `instances.list` returns it.
//
// `scheduled` is the announced maintenance block or the empty string. It is passed as JSON rather than
// as a time so that a fixture can express "GCP announced nothing", which is a different fact from "GCP
// announced a window with no start".
func twinSQLInstanceJSON(name string, flags map[string]string, tier, scheduled string) string {
	entries := make([]string, 0, len(flags))
	for _, flag := range sortedStringKeys(flags) {
		entries = append(entries, fmt.Sprintf(`{"name":%q,"value":%q}`, flag, flags[flag]))
	}
	maintenance := ""
	if scheduled != "" {
		maintenance = `,
  "scheduledMaintenance": ` + scheduled
	}
	return fmt.Sprintf(`{
  "kind": "sql#instance",
  "name": %q,
  "project": %q,
  "region": %q,
  "connectionName": %q,
  "databaseVersion": "POSTGRES_16",
  "state": "RUNNABLE",
  "createTime": %q,
  "ipAddresses": [{"ipAddress": "10.24.0.7", "type": "PRIVATE"}],
  "settings": {
    "tier": %q,
    "availabilityType": "REGIONAL",
    "dataDiskSizeGb": "100",
    "dataDiskType": "PD_SSD",
    "databaseFlags": [%s],
    "maintenanceWindow": {"day": 7, "hour": 3, "updateTrack": "stable"},
    "userLabels": {"team": %q, "environment": "production"}
  }%s
}`, name, twinProject, twinRegion, twinConnectionName(name),
		rfc3339(fixtureStart.Add(-720*time.Hour)), tier, strings.Join(entries, ","), twinTeam, maintenance)
}

// twinSQLOperationJSON renders one `operations.list` entry.
func twinSQLOperationJSON(name, operationType, instance string, start, end time.Time) string {
	return fmt.Sprintf(`{
  "kind": "sql#operation",
  "name": %q,
  "operationType": %q,
  "targetId": %q,
  "targetProject": %q,
  "status": "DONE",
  "insertTime": %q,
  "startTime": %q,
  "endTime": %q
}`, name, operationType, instance, twinProject,
		rfc3339(start.Add(-5*time.Minute)), rfc3339(start), rfc3339(end))
}

// twinSQLFlagJSON renders one entry of the flag catalogue, which is neither a node nor a change: it is
// what makes a flag change interpretable (contract §4).
func twinSQLFlagJSON(name string, requiresRestart bool) string {
	return fmt.Sprintf(`{"kind":"sql#flag","name":%q,"type":"INTEGER","appliesTo":["POSTGRES_16"],`+
		`"requiresRestart":%t,"minValue":"1","maxValue":"100000"}`, name, requiresRestart)
}

// twinGKEClusterJSON renders one cluster as `clusters.list` returns it — node pools included, because
// the API returns them and the point is that the feeder emits none (FR-030).
func twinGKEClusterJSON(name, location string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "location": %q,
  "selfLink": "https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s",
  "status": "RUNNING",
  "createTime": %q,
  "currentMasterVersion": "1.31.4-gke.1183000",
  "currentNodeVersion": "1.31.3-gke.1056000",
  "releaseChannel": {"channel": "REGULAR"},
  "resourceLabels": {"team": %q, "environment": "production"},
  "maintenancePolicy": {"window": {"dailyMaintenanceWindow": {"startTime": "03:00", "duration": "PT4H"}}},
  "nodePools": [{"name": "gpu-pool", "initialNodeCount": 3}]
}`, name, location, twinProject, location, name,
		rfc3339(fixtureStart.Add(-5000*time.Hour)), twinTeam)
}

// twinSQLAuditPageJSON renders one page of Cloud SQL admin-activity entries.
//
// The operation is long-running, so it writes two entries correlated by `operation.id`: a request
// entry with `operation.first` and a completion entry with `operation.last`, whose timestamp is the
// instant the edit took effect (§5.2).
//
// # There is no principal in these entries, and that is the point
//
// A sanitised audit entry has **no** `authenticationInfo` block at all — FR-135 drops a principal,
// it does not hash or rename one — so the twin has none either, and `scripts/check-no-secrets.sh`
// fails the build on the field *name* regardless of its value. What the twin can therefore prove is
// the instant, the operation correlation and the change that falls out of them; the actor ladder's
// classification of a principal is asserted from memory in actorkind_test.go and in
// TestAnAuditPrincipalReachesTheCloudSQLConfigChange, where nothing is written to disk. That split is
// contracts/sanitisation.md §7 applied: a property that can only be demonstrated on real payloads is
// labelled as such rather than faked in a twin.
func twinSQLAuditPageJSON(instance string, at time.Time) string {
	logName := "projects/" + twinProject + "/logs/cloudaudit.googleapis.com%2Factivity"
	resource := "projects/" + twinProject + "/instances/" + instance
	return fmt.Sprintf(`{"entries":[
 {"insertId":"sql-req-1","logName":%q,"timestamp":%q,"receiveTimestamp":%q,
  "operation":{"id":"op-flag-1","producer":"cloudsql.googleapis.com","first":true},
  "protoPayload":{"serviceName":"cloudsql.googleapis.com","methodName":"cloudsql.instances.update",
   "resourceName":%q}},
 {"insertId":"sql-done-1","logName":%q,"timestamp":%q,"receiveTimestamp":%q,
  "operation":{"id":"op-flag-1","producer":"cloudsql.googleapis.com","last":true},
  "protoPayload":{"serviceName":"cloudsql.googleapis.com","methodName":"cloudsql.instances.update",
   "resourceName":%q,"status":{}}}
]}`,
		logName, rfc3339(at.Add(-40*time.Second)), rfc3339(at.Add(-10*time.Second)), resource,
		logName, rfc3339(at), rfc3339(at.Add(30*time.Second)), resource)
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// twinServiceConfigJSON renders one Cloud Run service whose template carries the configuration the US5
// twins turn on: environment variables, a secret-sourced one, and a Cloud SQL volume.
//
// It is a separate builder rather than more parameters on twinServiceJSON because the US1 twins assert
// the *topology* and the US5 twins assert the *configuration*, and a single builder carrying both would
// have changed every US1 golden the first time a US5 fixture needed another variable.
func twinServiceConfigJSON(uid string, generation int64, split map[string]int,
	secretVersion string, attach []string, extraEnv map[string]string) string {
	statuses := make([]string, 0, len(split))
	for _, rev := range sortedKeys(split) {
		statuses = append(statuses, fmt.Sprintf(
			`{"type":"TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION","revision":%q,"percent":%d}`, rev, split[rev]))
	}
	env := []string{fmt.Sprintf(`{"name":"OTEL_SERVICE_NAME","value":%q}`, twinService)}
	for _, name := range sortedStringKeys(extraEnv) {
		env = append(env, fmt.Sprintf(`{"name":%q,"value":%q}`, name, extraEnv[name]))
	}
	if secretVersion != "" {
		env = append(env, fmt.Sprintf(
			`{"name":"API_TOKEN","valueSource":{"secretKeyRef":{"secret":"storefront-api-token","version":%q}}}`,
			secretVersion))
	}
	volumes := ""
	if len(attach) > 0 {
		quoted := make([]string, 0, len(attach))
		for _, instance := range attach {
			quoted = append(quoted, fmt.Sprintf("%q", instance))
		}
		volumes = fmt.Sprintf(`,
    "volumes": [{"name": "cloudsql", "cloudSqlInstance": {"instances": [%s]}}]`,
			strings.Join(quoted, ","))
	}
	labelJSON, _ := json.Marshal(map[string]string{
		"team": twinTeam, "environment": "production", "service": twinService,
	})
	return fmt.Sprintf(`{
  "name": "projects/%s/locations/%s/services/%s",
  "uid": %q,
  "generation": "%d",
  "observedGeneration": "%d",
  "createTime": %q,
  "updateTime": %q,
  "labels": %s,
  "ingress": "INGRESS_TRAFFIC_ALL",
  "latestReadyRevision": "projects/%s/locations/%s/services/%s/revisions/%s",
  "latestCreatedRevision": "projects/%s/locations/%s/services/%s/revisions/%s",
  "trafficStatuses": [%s],
  "template": {
    "serviceAccount": "storefront@%s.iam.gserviceaccount.com",
    "containers": [{"image": "europe-docker.pkg.dev/%s/twin/%s@sha256:0000000000000000000000000000000000000000000000000000000000000041",
      "env": [%s]}]%s
  }
}`, twinProject, twinRegion, twinService, uid, generation, generation,
		rfc3339(fixtureStart.Add(-720*time.Hour)), rfc3339(fixtureCreated),
		labelJSON,
		twinProject, twinRegion, twinService, twinRevOld,
		twinProject, twinRegion, twinService, twinRevOld,
		strings.Join(statuses, ","),
		twinProject, twinProject, twinService,
		strings.Join(env, ","), volumes)
}

// twinRevisionConfigJSON renders one Cloud Run revision carrying the configuration the US5 twins turn
// on: environment variables, a secret-sourced one, and a Cloud SQL volume.
//
// It is a separate builder from twinRevisionJSON for the same reason twinServiceConfigJSON is: the US1
// twins assert the topology and the US5 twins assert the configuration, and one builder carrying both
// would have changed every US1 golden the first time a US5 fixture needed another variable.
func twinRevisionConfigJSON(name string, createTime time.Time, digestSuffix, secretVersion string,
	attach []string, extraEnv map[string]string) string {
	env := []string{fmt.Sprintf(`{"name":"OTEL_SERVICE_NAME","value":%q}`, twinService)}
	for _, key := range sortedStringKeys(extraEnv) {
		env = append(env, fmt.Sprintf(`{"name":%q,"value":%q}`, key, extraEnv[key]))
	}
	if secretVersion != "" {
		env = append(env, fmt.Sprintf(
			`{"name":"API_TOKEN","valueSource":{"secretKeyRef":{"secret":"storefront-api-token","version":%q}}}`,
			secretVersion))
	}
	volumes := ""
	if len(attach) > 0 {
		quoted := make([]string, 0, len(attach))
		for _, instance := range attach {
			quoted = append(quoted, fmt.Sprintf("%q", instance))
		}
		volumes = fmt.Sprintf(`,
  "volumes": [{"name": "cloudsql", "cloudSqlInstance": {"instances": [%s]}}]`, strings.Join(quoted, ","))
	}
	return fmt.Sprintf(`{
  "name": "projects/%s/locations/%s/services/%s/revisions/%s",
  "uid": "uid-%s",
  "generation": "1",
  "createTime": %q,
  "labels": {"team": %q},
  "serviceAccount": "runtime@%s.iam.gserviceaccount.com",
  "executionEnvironment": "EXECUTION_ENVIRONMENT_GEN2",
  "containers": [{"image": "europe-docker.pkg.dev/%s/twin/%s@sha256:00000000000000000000000000000000000000000000000000000000000000%s",
    "env": [%s]}],
  "conditions": [{"type": "Ready", "state": "CONDITION_SUCCEEDED"}]%s
}`, twinProject, twinRegion, twinService, name, name, rfc3339(createTime), twinTeam,
		twinProject, twinProject, twinService, digestSuffix, strings.Join(env, ","), volumes)
}

// twinAuditEntryJSON renders one general admin-activity entry.
//
// It carries no `authenticationInfo`, for the reason twinSQLAuditPageJSON states: a sanitised entry
// has no such field at all (FR-135) and `scripts/check-no-secrets.sh` fails the build on the field
// name. So every actor these twins produce is unclassified, and the actor ladder — including the
// CONTROLLER classification T152 is about — is asserted from memory in auditlog_test.go instead.
func twinAuditEntryJSON(insertID, operationID, serviceName, methodName, resourceName string,
	at time.Time, statusCode int) string {
	operation := ""
	if operationID != "" {
		operation = fmt.Sprintf(`
  "operation": {"id": %q, "producer": %q, "first": true, "last": true},`, operationID, serviceName)
	}
	status := `"status": {}`
	if statusCode != 0 {
		status = fmt.Sprintf(`"status": {"code": %d}`, statusCode)
	}
	return fmt.Sprintf(`{
  "insertId": %q,
  "logName": "projects/%s/logs/cloudaudit.googleapis.com%%2Factivity",
  "timestamp": %q,
  "receiveTimestamp": %q,%s
  "protoPayload": {"serviceName": %q, "methodName": %q, "resourceName": %q, %s}
}`, insertID, twinProject, rfc3339(at), rfc3339(at.Add(20*time.Second)), operation,
		serviceName, methodName, resourceName, status)
}

// twinAuditGeneralPage wraps general entries into one `entries.list` page.
func twinAuditGeneralPage(entries ...string) string {
	return `{"entries":[` + strings.Join(entries, ",") + `]}`
}

// --- US9 payload builders -------------------------------------------------------------------------

func computeItemsPayloadAt(t *testing.T, kind string, at time.Time, items ...string) feeder.Payload {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		raw = append(raw, json.RawMessage(item))
	}
	body, err := json.Marshal(map[string]any{"items": raw})
	if err != nil {
		t.Fatalf("marshal %s: %v", kind, err)
	}
	return feeder.Payload{Kind: kind, At: at, Bytes: body}
}

// twinForwardingRuleJSON renders a global HTTPS forwarding rule fronting a URL map.
func twinForwardingRuleJSON(name, urlMap string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "selfLink": "https://www.googleapis.com/compute/v1/projects/%s/global/forwardingRules/%s",
  "IPAddress": "34.111.0.7",
  "IPProtocol": "TCP",
  "portRange": "443-443",
  "loadBalancingScheme": "EXTERNAL_MANAGED",
  "target": "https://www.googleapis.com/compute/v1/projects/%s/global/targetHttpsProxies/%s"
}`, name, twinProject, name, twinProject, urlMap)
}

// twinURLMapJSON renders a URL map routing one host to one backend service.
func twinURLMapJSON(name, host, backend string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "selfLink": "https://www.googleapis.com/compute/v1/projects/%s/global/urlMaps/%s",
  "defaultService": "https://www.googleapis.com/compute/v1/projects/%s/global/backendServices/%s",
  "hostRules": [{"hosts": [%q], "pathMatcher": "all"}],
  "pathMatchers": [{"name": "all", "defaultService": "https://www.googleapis.com/compute/v1/projects/%s/global/backendServices/%s"}]
}`, name, twinProject, name, twinProject, backend, host, twinProject, backend)
}

// twinBackendServiceJSON renders a backend service whose one backend is a serverless NEG.
func twinBackendServiceJSON(name, neg string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "selfLink": "https://www.googleapis.com/compute/v1/projects/%s/global/backendServices/%s",
  "loadBalancingScheme": "EXTERNAL_MANAGED",
  "protocol": "HTTPS",
  "backends": [{"group": "https://www.googleapis.com/compute/v1/projects/%s/regions/%s/networkEndpointGroups/%s"}]
}`, name, twinProject, name, twinProject, twinRegion, neg)
}

// twinNEGJSON renders the serverless NEG that names the Cloud Run service it fronts — the one field
// that makes the exposure chain a derivation rather than a name match.
func twinNEGJSON(name, service string) string {
	return fmt.Sprintf(`{
  "name": %q,
  "selfLink": "https://www.googleapis.com/compute/v1/projects/%s/regions/%s/networkEndpointGroups/%s",
  "region": "https://www.googleapis.com/compute/v1/projects/%s/regions/%s",
  "networkEndpointType": "SERVERLESS",
  "cloudRun": {"service": %q}
}`, name, twinProject, twinRegion, name, twinProject, twinRegion, service)
}

// twinDNSPayloadAt renders one `resourceRecordSets.list` response for a zone.
func twinDNSPayloadAt(t *testing.T, at time.Time, zone string, records ...string) feeder.Payload {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(records))
	for _, record := range records {
		raw = append(raw, json.RawMessage(record))
	}
	body, err := json.Marshal(map[string]any{"zone": zone, "rrsets": raw})
	if err != nil {
		t.Fatalf("marshal dns record sets: %v", err)
	}
	return feeder.Payload{Kind: gcpfeeder.PayloadDNSRecordSets, At: at, Bytes: body}
}

func twinDNSRecordJSON(name, recordType string, targets ...string) string {
	quoted := make([]string, 0, len(targets))
	for _, target := range targets {
		quoted = append(quoted, fmt.Sprintf("%q", target))
	}
	return fmt.Sprintf(`{"kind":"dns#resourceRecordSet","name":%q,"type":%q,"ttl":300,"rrdatas":[%s]}`,
		name, recordType, strings.Join(quoted, ","))
}

// twinDNSAuditPage renders the Cloud DNS change entry that dates a switch. No principal, for the reason
// twinSQLAuditPageJSON states.
func twinDNSAuditPage(at time.Time) string {
	return twinAuditGeneralPage(twinAuditEntryJSON("dns-change-1", "", "dns.googleapis.com",
		"dns.changes.create", "projects/"+twinProject+"/managedZones/twin-zone/changes/1", at, 0))
}
