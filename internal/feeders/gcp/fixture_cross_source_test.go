// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	otelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/otel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// The cross-source fixture: two connectors, one entity (T175, FR-117, SC-021).
//
// ---------------------------------------------------------------------------------------------
// Why this fixture exists, and what its absence hid.
//
// Every other fixture in this corpus is single-source. That is fine for what each of them proves,
// and it meant that **no fixture anywhere paired a GCP claim with another source's** — so C4, C5 and
// C7 were published, registered, evaluated on every claim, and never once fired in the corpus.
//
// SC-021 asks for two figures over that corpus: 100% of automated merges involving a GCP or vendor
// claim explainable by `resolve why`, and at least 95% automatic merging under a certain rule for
// entities present in both GCP and another source with an agreed environment. Over a corpus with
// zero cross-source merges, both were vacuously satisfied — 100% of nothing and 95% of nothing —
// which is the kind of green that is worse than red.
//
// Building it found the defect the vacuum had been hiding: C4 merged the observed service with the
// **revision** rather than with the revision's Cloud Run service, so across a rollout two revisions
// of one service became one entity, with no human in the loop (internal/resolution/gcp.go). A rule
// nothing exercises is a rule whose defects are all latent.
//
// # What the two sources see
//
// The GCP feeder polls the twin project and reports `storefront` with two revisions, exactly as
// gcp-baseline-topology-01 does. The OpenTelemetry feeder receives spans from a process running
// inside the newer revision, whose resource attributes the GCP detector set: `cloud.platform`,
// `faas.name`, `faas.version`, `cloud.region`, `cloud.account.id`. Neither connector knows the other
// exists. C4 is what joins them, and the merge is recorded with the revision as its evidence.
//
// # Why the OTel side is an OTLP payload rather than a hand-written claim
//
// A fixture whose second source were a hand-authored identity claim would assert that the resolution
// rule works on input somebody wrote to make it work. The payload here is an OTLP export with the
// attribute names the GCP OpenTelemetry detector actually sets, and the claim is whatever
// `internal/feeders/otel` makes of it — so a change in the aggregator's translation breaks this
// fixture, which is the point.

// The OTel service name the instrumentation reports, and it is DELIBERATELY NOT the Cloud Run
// service's name.
//
// This is the difference between a fixture that exercises C4 and one that only looks as though it
// does. With the same name on both sides — which is what this fixture had first, and is the common
// real configuration — the two sources share an identifier in one namespace, so **C1** merges them
// and C4 never gets there: the recorded decision came out `auto_merge/C1` and deleting C4's service
// arm changed nothing. C5 would take the case after that, on the declared name.
//
// C4's case is the one where the two sources agree on nothing an identifier rule can see, and the
// only thing joining them is the revision the instrumentation reports from inside itself. That is
// what FR-117 is about, and it is a real deployment too: a service Cloud Run calls `storefront`
// whose instrumentation was configured as `storefront-web`, by a different team, years apart.
const twinOTelService = "storefront-web"

// The second pair, and the rule it exercises.
//
// `twinOther` (`orders`) declares an OpenTelemetry service name in `OTEL_SERVICE_NAME`, telemetry
// arrives claiming that name in the same environment, and **C5** merges them: somebody configured it
// to be true, and the declaration is the evidence. It is here for the same reason the first pair is —
// C5 was published, registered and had never fired anywhere either — and it is a different rule
// rather than a second instance of C4, so the two exercise the two halves of FR-117 and FR-118.
//
// C1 would also fire on this pair, on the shared identifier. C5 wins because it is more specific
// (30 against C1's 10) and `CertainRules` evaluates most-specific-first, so the recorded reason is
// the one that explains the merge best rather than the first one that happened to match. Neither
// `cloud.platform` nor `faas.version` is set on this export, so C4 has nothing to say about it.
const twinOtherOTelService = "orders-api"

// The third pair, and the rule that had never fired for a reason of its own.
//
// `twinInstance` (`orders-primary`) is a Cloud SQL instance the GCP feeder lists. A process reaches
// it over the Cloud SQL connector's Unix socket, so the OpenTelemetry dependency claim's own value
// is `/cloudsql/twin-production:europe-west1:orders-primary` — the caller's address **contains the
// instance connection name verbatim**, because that is the string its configuration was given.
// **C7** merges them on it.
//
// C4 and C5 had never fired for want of a cross-source fixture. C7 had never fired for a different
// reason, and a fixture alone would not have fixed it: the rule searched `gcp.sql.instance` on both
// sides while refusing two claims from one source, and the GCP feeder is the only source that
// claims in that namespace — so the only pair it could match was one that cannot exist. Published,
// registered, evaluated on every claim, unfireable. This pair is what would have caught it.
//
// C1 cannot take this pair: the two identifiers are different strings in different namespaces, which
// is the case C7's file comment says it exists for.
const crossSourceSocketAddress = otelfeeder.CloudSQLSocketPrefix + twinProject + ":" + twinRegion + ":" + twinInstance

// crossSourceSpans is how many spans the one export carries. Enough to land the aggregator in a
// weight class rather than on its "too little traffic to say" path.
const crossSourceSpans = 300

func TestGenerateCrossSourceFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the cross-source fixture", genFixturesEnv)
	}
	writeCrossSourceFixture(t)
}

// writeCrossSourceFixture runs BOTH feeders into one fixture directory.
//
// The recorders append, so two runs over the same directory produce one payload stream and one event
// stream — which is what a fixture is: everything the graph was told, in arrival order, from every
// source it was told by. The manifest then names both sources, and that is the only place the
// two-connector shape is visible; the replay does not care which feeder wrote which line.
func writeCrossSourceFixture(t *testing.T) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "fixtures/gcp-cross-source-merge-01")
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

	// One shared arrival clock across both feeders. Observed time has to be monotonic over the whole
	// fixture, not per source: two sources with independent clocks would let the second source's
	// first event be observed before the first source's last, which is a fixture that could not have
	// happened.
	clock := &arrivalClock{base: fixtureStart}

	// The OTel half is recorded FIRST, and that ordering is the fixture's subject rather than an
	// accident. Telemetry is continuous and a poll is every half hour, so the observation almost
	// always reaches the graph before the poll that explains it — and with the observed claim already
	// there, which of the GCP claims completes C4 depends on the order the poll's own events are
	// applied in. The service claim first and the rule fires from the SERVICE arm; the revision claim
	// first and it fires from the revision arm. The shuffle step permutes events inside one arrival
	// window, so both orderings are exercised across its seeds, and a rule missing either arm builds
	// a different graph under permutation.
	//
	// Recorded the other way round — the poll first — the observed claim always lands last, the
	// observed arm always fires, and the other two are never needed. That is what this fixture looked
	// like when it was first written, and it passed with the service arm deleted.
	otelDesc := writeCrossSourceOTelHalf(t, dir, clock)
	gcpDesc := writeCrossSourceGCPHalf(t, dir, clock)

	if err := record.WriteManifest(dir, record.Manifest{
		Family: "gcp-cross-source",
		Description: "One Cloud Run service seen by two connectors: the GCP feeder polls it and " +
			"reports two revisions, and the OpenTelemetry feeder receives spans from a process " +
			"running inside the newer one, carrying the resource attributes the GCP detector sets. " +
			"Neither connector knows the other exists, and C4 merges the observed service with the " +
			"revision's Cloud Run service — citing the revision as its evidence, and never merging " +
			"the revisions with each other. It carries one labelled pair per certain GCP rule: C4 " +
			"on a service whose telemetry name differs from its Cloud Run name, C5 on one that " +
			"declares OTEL_SERVICE_NAME, and C7 on a Cloud SQL instance reached over the " +
			"connector's Unix socket, where the caller's own address carries the connection name. " +
			"It is the only fixture in the corpus in which a GCP claim meets another source's, " +
			"which is what makes SC-021's two cross-source figures measurements rather than 100% " +
			"and 95% of nothing. " +
			"Synthetic structural twin: no identifier is derived from the organisation " +
			"(contracts/sanitisation.md §7), and constitution VIII says synthetic-only data is not " +
			"enough for a connector to be marked stable — US7's recording replaces it by swapping " +
			"payloads/, not by changing code.",
		Sources: []record.ManifestSource{record.SourceOf(otelDesc), record.SourceOf(gcpDesc)},
		Start:   fixtureStart,
		End:     fixtureStart.Add(time.Hour),
	}); err != nil {
		t.Fatalf("%s: write manifest: %v", dir, err)
	}
	appendQueries(t, dir, crossSourceQueries())
	t.Logf("%s: two sources written", dir)
}

func writeCrossSourceGCPHalf(t *testing.T, dir string, clock *arrivalClock) feeder.Description {
	t.Helper()
	labels := map[string]string{
		"team":        twinTeam,
		"environment": "production",
		"service":     twinService,
	}
	payloads := []feeder.Payload{
		// The C7 pair's GCP side: the instance as GCP lists it. Its claim carries the connection
		// name as a supporting attribute, which is the string the observed side quotes.
		sqlInstancesPayloadAt(t, cycleAt(1),
			twinSQLInstanceJSON(twinInstance, nil, "db-custom-2-8192", "")),
		servicesPayloadAt(t, cycleAt(1), twinServiceJSON(
			"uid-service-1", 42, fixtureStart.Add(-720*time.Hour), fixtureCreated,
			twinRevOld, twinRevNew, map[string]int{twinRevNew: 100}, labels),
			// The second service, whose declared OpenTelemetry name is what C5 resolves on. It has
			// no revisions in this fixture: C5 is about a declaration and an observation, and giving
			// it revisions would add nodes that assert nothing here.
			twinDeclaringServiceJSON(twinOther, twinOtherOTelService, labels)),
		revisionsPayloadAt(t, cycleAt(1),
			twinRevisionJSON(twinRevNew, fixtureCreated, "42"),
			twinRevisionJSON(twinRevOld, fixtureStart.Add(-24*time.Hour), "41")),
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

func writeCrossSourceOTelHalf(t *testing.T, dir string, clock *arrivalClock) feeder.Description {
	t.Helper()
	// TWO exports, from processes in the two DIFFERENT revisions — which is what a rollout looks
	// like from the telemetry side: while traffic is shifting, both revisions serve, and the
	// instrumentation reports whichever one each process is running in, all on the one subject
	// `otel.service.name`.
	//
	// This is the pair that makes the fixture catch the defect that motivated it. With C4 merging the
	// observed service into the REVISION, these two claims produce two certain merges sharing a
	// side, and the two revisions become one entity. With it merging into the service, both produce
	// the same pair and the merge is idempotent. A fixture with one export could not tell the two
	// apart.
	//
	// They arrive BEFORE the poll. See writeCrossSourceFixture for why that ordering is the
	// fixture's subject: it is what leaves the completing claim on the GCP side, where the shuffle
	// can permute which one it is.
	exports := []struct {
		at       time.Time
		revision string
	}{
		{fixtureStart, twinRevOld},
		{fixtureStart.Add(5 * time.Minute), twinRevNew},
	}
	payloads := make([]feeder.Payload, 0, len(exports)+1)
	for _, e := range exports {
		raw, err := proto.Marshal(cloudRunExport(e.at, e.revision, crossSourceSpans))
		if err != nil {
			t.Fatalf("marshal export: %v", err)
		}
		payloads = append(payloads, feeder.Payload{
			Kind: otelfeeder.PayloadKindTraces, At: e.at, Bytes: raw,
		})
	}
	// The C5 pair's observation: the declared name, the agreed environment, and no Cloud Run
	// attributes at all — so this export says nothing C4 could use, and the merge rests entirely on
	// somebody having configured `OTEL_SERVICE_NAME`.
	plainAt := fixtureStart.Add(10 * time.Minute)
	plain, err := proto.Marshal(plainExport(plainAt, twinOtherOTelService, crossSourceSpans))
	if err != nil {
		t.Fatalf("marshal plain export: %v", err)
	}
	payloads = append(payloads, feeder.Payload{
		Kind: otelfeeder.PayloadKindTraces, At: plainAt, Bytes: plain,
	})

	// The C7 pair's observation: a process calling Cloud SQL over the connector's Unix socket. The
	// dependency claim's own value is the socket path, and the connection name rides on it as an
	// attribute — so the two sides address the instance completely differently and agree on the one
	// string both were configured with.
	sqlAt := fixtureStart.Add(15 * time.Minute)
	sqlBytes, err := proto.Marshal(cloudSQLExport(sqlAt, crossSourceSocketAddress, crossSourceSpans))
	if err != nil {
		t.Fatalf("marshal cloud sql export: %v", err)
	}
	payloads = append(payloads, feeder.Payload{
		Kind: otelfeeder.PayloadKindTraces, At: sqlAt, Bytes: sqlBytes,
	})

	f := &otelfeeder.Feeder{SourceID: "otel:twin"}
	desc := f.Describe()
	src := record.Wrap(clock.wrap(source.NewSliceSource(payloads)), dir)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatalf("otel half: run: %v", err)
	}
	assertRecorded(t, "otel half", src, events, memory)
	return desc
}

func assertRecorded(t *testing.T, half string, src *record.PayloadRecorder, events *record.EventRecorder, memory *emit.MemoryEmitter) {
	t.Helper()
	if err := src.Err(); err != nil {
		t.Fatalf("%s: record payloads: %v", half, err)
	}
	if err := events.Err(); err != nil {
		t.Fatalf("%s: record events: %v", half, err)
	}
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("%s: %d events were refused; the first is %s (%s)",
			half, len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	if events.Accepted() == 0 {
		t.Fatalf("%s: recorded no events", half)
	}
	t.Logf("%s: %d payloads, %d events", half, src.Count(), events.Accepted())
}

// cloudRunExport is one OTLP export from a process running inside twinRevNew.
//
// The resource attributes are the ones the GCP OpenTelemetry resource detector sets on Cloud Run,
// spelled exactly as it spells them — `faas.version` is the revision, `cloud.account.id` the
// project, `cloud.region` the region, all of them meaningful only because `cloud.platform` says
// which platform this is. `faas.version` is an alias on AWS Lambda, which is why the feeder gates on
// the platform and why a fixture that left it out would be asserting the wrong thing.
func cloudRunExport(at time.Time, revision string, spans int) *coltracepb.ExportTraceServiceRequest {
	resource := map[string]any{
		"service.name":    twinOTelService,
		"service.version": "42",
		// `deployment.environment.name`, not the deprecated `deployment.environment` — the feeder
		// reads the current semconv spelling, and the first version of this fixture used the old one
		// and silently produced a claim with no environment on it at all. SC-021's 95% figure is
		// about entities present in both sources "with an agreed environment", so a claim missing it
		// is a pair that cannot be counted.
		"deployment.environment.name": "production",
		"cloud.platform":              otelfeeder.CloudRunPlatform,
		"cloud.provider":              "gcp",
		"faas.name":                   twinService,
		"faas.version":                revision,
		"cloud.region":                twinRegion,
		"cloud.account.id":            twinProject,
	}
	// One callee, so the aggregator has a call path to emit and the fixture carries a dependency
	// edge as well as a claim. A fixture whose only assertion were the merge would not notice the
	// merge breaking the topology around it.
	attrs := map[string]any{"peer.service": twinOther}

	window := at.Truncate(time.Minute)
	step := time.Minute / time.Duration(max(spans, 1))
	out := make([]*tracepb.Span, 0, spans)
	for i := range spans {
		start := window.Add(time.Duration(i) * step)
		out = append(out, &tracepb.Span{
			// The revision is mixed into the ids so the two exports do not share span ids, which
			// a sampler would read as one span reported twice.
			TraceId:           fixtureIDBytes(16, i, 1, revision),
			SpanId:            fixtureIDBytes(8, i, 2, revision),
			Name:              "GET /",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(start.UnixNano()),       //nolint:gosec // a fixture clock well inside range
			EndTimeUnixNano:   uint64(start.UnixNano() + 2e7), //nolint:gosec // as above
			Attributes:        otlpKeyValues(attrs),
			Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_UNSET},
		})
	}
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: otlpKeyValues(resource)},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "telemetrygen", Version: "v0.161.0"},
				Spans: out,
			}},
		}},
	}
}

// fixtureIDBytes derives a deterministic trace or span id, so the corpus is byte-for-byte
// reproducible and a regeneration produces no diff unless something really changed.
func fixtureIDBytes(n, span, salt int, scope string) []byte {
	var seed [16]byte
	binary.BigEndian.PutUint64(seed[0:], uint64(span)) //nolint:gosec // small positive ints
	binary.BigEndian.PutUint64(seed[8:], uint64(salt)) //nolint:gosec // small positive ints
	sum := sha256.Sum256(append(seed[:], scope...))
	return sum[:n]
}

// otlpKeyValues renders an attribute map as OTLP key-values, sorted so the bytes are stable.
// cloudSQLExport is a process calling a database over the Cloud SQL connector's Unix socket.
//
// The resource carries no Cloud Run attributes: this export is about the *dependency*, and C7 rests
// on the address rather than on where the caller runs. `db.system` with a `server.address` is what
// makes the aggregator classify the callee as a database and claim it by its address.
func cloudSQLExport(at time.Time, address string, spans int) *coltracepb.ExportTraceServiceRequest {
	// The caller is the C5 pair's service, and that is deliberate on two counts. It is the true
	// shape — `orders-api` is what talks to `orders-primary` — and it keeps this export's resource
	// attributes identical to that pair's, so the emitter's fingerprint is unchanged and no second
	// service claim is minted.
	//
	// Using the C4 pair's service here instead **broke C4**, and the shuffle is what said so: this
	// export carries no Cloud Run attributes, so a second `storefront-web` claim arrived without the
	// revision, and which claim C4 saw then depended on the order events were applied in. The rule
	// went from firing to not firing, and the corpus-level effect was silent — `auto_merge/C4`
	// simply disappeared from the counts while every other number stayed plausible.
	resource := map[string]any{
		"service.name":                twinOtherOTelService,
		"service.version":             "7",
		"deployment.environment.name": "production",
	}
	attrs := map[string]any{
		"db.system":      "postgresql",
		"server.address": address,
	}

	window := at.Truncate(time.Minute)
	step := time.Minute / time.Duration(max(spans, 1))
	out := make([]*tracepb.Span, 0, spans)
	for i := range spans {
		start := window.Add(time.Duration(i) * step)
		out = append(out, &tracepb.Span{
			TraceId:           fixtureIDBytes(16, i, 5, address),
			SpanId:            fixtureIDBytes(8, i, 6, address),
			Name:              "SELECT orders",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(start.UnixNano()),       //nolint:gosec // a fixture clock well inside range
			EndTimeUnixNano:   uint64(start.UnixNano() + 2e7), //nolint:gosec // as above
			Attributes:        otlpKeyValues(attrs),
			Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_UNSET},
		})
	}
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: otlpKeyValues(resource)},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "telemetrygen", Version: "v0.161.0"},
				Spans: out,
			}},
		}},
	}
}

func otlpKeyValues(attrs map[string]any) []*commonpb.KeyValue {
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]*commonpb.KeyValue, 0, len(keys))
	for _, key := range keys {
		value := &commonpb.AnyValue{}
		switch v := attrs[key].(type) {
		case string:
			value.Value = &commonpb.AnyValue_StringValue{StringValue: v}
		case int64:
			value.Value = &commonpb.AnyValue_IntValue{IntValue: v}
		case float64:
			value.Value = &commonpb.AnyValue_DoubleValue{DoubleValue: v}
		case bool:
			value.Value = &commonpb.AnyValue_BoolValue{BoolValue: v}
		default:
			value.Value = &commonpb.AnyValue_StringValue{StringValue: ""}
		}
		out = append(out, &commonpb.KeyValue{Key: key, Value: value})
	}
	return out
}

// crossSourceQueries are the manifest queries this fixture is verified against.
//
// The `audit` query is the load-bearing one and it is the same code path `resolve why` runs: it is
// how SC-021's "explainable by the audit query" stops being a claim about intent. Its golden records
// the verdict, the claims from both sources, and the decision with its rule and rationale — so a
// merge that stopped being explainable, or started being explained differently, is a golden diff
// rather than a discussion.
//
// Every query is observed-time pinned, for the reason in fixture_specs_test.go: an unpinned query
// answers "as of now" and its golden encodes the day it was recorded.
func crossSourceQueries() string {
	return `queries:
  # The audit: are these two the same entity, and why? This is ` + "`resolve why`" + `'s own query.
  - name: why-observed-is-the-cloud-run-service
    kind: audit
    ref_a: otel.service.name=` + twinOTelService + `
    ref_b: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # And the pair C4 must NOT have merged. The revision is the evidence for the merge, not a party
  # to it; merging with it made two revisions of one service one entity across a rollout.
  - name: why-the-two-revisions-are-not-one
    kind: audit
    ref_a: gcp.cloudrun.revision=` + twinProject + `/` + twinRegion + `/` + twinService + `/` + twinRevOld + `
    ref_b: gcp.cloudrun.revision=` + twinProject + `/` + twinRegion + `/` + twinService + `/` + twinRevNew + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # The neighbourhood around the merged entity, so the merge is visible as a graph rather than only
  # as a decision: one service, its two revisions, its owner, and the dependency the spans showed.
  - name: merged-service-2hop
    kind: subgraph
    focus: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
    valid_at: ` + rfc3339(pinnedAt) + `
    observed_at: ` + rfc3339(pinnedAt) + `
    hops: 2
    direction: both
  # The second pair, and its rule. C5 rests on somebody having configured OTEL_SERVICE_NAME, so
  # the audit's golden is where "the declaration is the evidence" stops being a sentence.
  - name: why-the-declared-name-is-the-service
    kind: audit
    ref_a: otel.service.name=` + twinOtherOTelService + `
    ref_b: gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinOther + `
    observed_at: ` + rfc3339(pinnedAt) + `
  # The suggestion queue, empty. SC-021's first clause is that no probable rule merged on its own,
  # and a fixture in which two sources meet is where a probable rule would have the chance.
  - name: no-probable-merge
    kind: suggestions
    observed_at: ` + rfc3339(pinnedAt) + `
    expect_empty: "SC-021's first clause: no probable rule merged or proposed on its own in a fixture where two sources meet, so the suggestion queue must hold nothing"

ground_truth:
  # The cross-source pair SC-021's 95% figure is measured over: one entity present in both GCP and
  # another source, with an agreed environment, that must merge automatically under a CERTAIN rule.
  cross_source_pairs:
    - pair:
        - otel.service.name=` + twinOTelService + `
        - gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
      same: true
      rule: C4
    - pair:
        - otel.service.name=` + twinOtherOTelService + `
        - gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinOther + `
      same: true
      rule: C5
    # C7: the instance as GCP lists it, and the socket address a caller reached it at. The two
    # sides share no identifier and no namespace — the only thing joining them is the connection
    # name both were configured with, which is what makes this rule certain rather than a
    # resemblance.
    - pair:
        - ` + gcpfeeder.NSSQLInstance + `=` + twinConnectionName(twinInstance) + `
        - server.address=` + crossSourceSocketAddress + `
      same: true
      rule: C7
  # And the pair that must stay apart, for the same reason the second audit query exists.
  distinct_pairs:
    - pair:
        - gcp.cloudrun.revision=` + twinProject + `/` + twinRegion + `/` + twinService + `/` + twinRevOld + `
        - gcp.cloudrun.revision=` + twinProject + `/` + twinRegion + `/` + twinService + `/` + twinRevNew + `
      same: false
    # Two different services, both observed, both in the same environment, both with a declared
    # OpenTelemetry name. Nothing may merge them, and a rule loose enough to do it would satisfy
    # every figure above.
    - pair:
        - gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinService + `
        - gcp.cloudrun.service=` + twinProject + `/` + twinRegion + `/` + twinOther + `
      same: false
`
}

// twinDeclaringServiceJSON renders a Cloud Run service whose only job in this fixture is to declare
// an OpenTelemetry service name.
//
// It is separate from twinServiceJSON rather than a parameter on it because that helper is shared by
// nineteen fixtures whose recorded bytes must not move, and because this one deliberately has no
// revisions and no traffic: C5 is about a declaration meeting an observation, and revisions would add
// nodes that assert nothing here.
func twinDeclaringServiceJSON(service, otelName string, labels map[string]string) string {
	own := maps.Clone(labels)
	own["service"] = service
	labelJSON, err := json.Marshal(own)
	if err != nil {
		panic(err) // a literal map; a failure here is a programming error in this file
	}
	return fmt.Sprintf(`{
  "name": "projects/%s/locations/%s/services/%s",
  "uid": "uid-service-%s",
  "generation": "7",
  "observedGeneration": "7",
  "createTime": %q,
  "updateTime": %q,
  "labels": %s,
  "ingress": "INGRESS_TRAFFIC_ALL",
  "template": {"containers": [{"image": "europe-docker.pkg.dev/%s/twin/%s@sha256:0000000000000000000000000000000000000000000000000000000000000007",
    "env": [{"name": "OTEL_SERVICE_NAME", "value": %q}]}]}
}`, twinProject, twinRegion, service, service,
		rfc3339(fixtureStart.Add(-720*time.Hour)), rfc3339(fixtureCreated),
		labelJSON, twinProject, service, otelName)
}

// plainExport is one OTLP export from a process that is NOT on Cloud Run, or at least says nothing
// about it: the service name, the environment, and no `cloud.platform`.
//
// The absence is the assertion. With no Cloud Run attributes C4 cannot fire, so the merge this export
// takes part in rests entirely on the declaration C5 reads — and a later change that made C4 fire on
// a partial signal would show up here as a decision recorded under the wrong rule.
func plainExport(at time.Time, service string, spans int) *coltracepb.ExportTraceServiceRequest {
	resource := map[string]any{
		"service.name":                service,
		"service.version":             "7",
		"deployment.environment.name": "production",
	}
	attrs := map[string]any{"peer.service": twinService}

	window := at.Truncate(time.Minute)
	step := time.Minute / time.Duration(max(spans, 1))
	out := make([]*tracepb.Span, 0, spans)
	for i := range spans {
		start := window.Add(time.Duration(i) * step)
		out = append(out, &tracepb.Span{
			TraceId:           fixtureIDBytes(16, i, 1, service),
			SpanId:            fixtureIDBytes(8, i, 2, service),
			Name:              "GET /orders",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(start.UnixNano()),       //nolint:gosec // a fixture clock well inside range
			EndTimeUnixNano:   uint64(start.UnixNano() + 2e7), //nolint:gosec // as above
			Attributes:        otlpKeyValues(attrs),
			Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_UNSET},
		})
	}
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: otlpKeyValues(resource)},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "telemetrygen", Version: "v0.161.0"},
				Spans: out,
			}},
		}},
	}
}
