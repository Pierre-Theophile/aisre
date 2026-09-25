// SPDX-License-Identifier: Apache-2.0

package otel_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	otelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/otel"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// What one window says (T051, T052, T054), tested a decision at a time. The whole corpora are
// in feeder_test.go; these build the spans that make one thing true.

// replay runs the feeder over hand-built exports and returns what it emitted.
func replay(t *testing.T, f *otelfeeder.Feeder, specs []exportSpec) *emit.MemoryEmitter {
	t.Helper()
	payloads := make([]feeder.Payload, 0, len(specs))
	for i, spec := range specs {
		raw, err := proto.Marshal(buildExport(spec, i))
		if err != nil {
			t.Fatalf("marshal export %d: %v", i, err)
		}
		payloads = append(payloads, feeder.Payload{
			Kind:  otelfeeder.PayloadKindTraces,
			At:    spec.at,
			Bytes: raw,
		})
	}
	em := emit.NewMemoryEmitter(f.Describe(), emit.WithStrict(false))
	if err := f.Run(t.Context(), source.NewSliceSource(payloads), em); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rejected := em.Rejected(); len(rejected) > 0 {
		t.Fatalf("the feeder emitted %d events the graph refused; the first is %s: %s",
			len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonCode())
	}
	return em
}

// export builds one export of a caller talking to a callee, inside one window.
func export(window time.Time, caller map[string]any, callee map[string]any, spans int) exportSpec {
	return exportSpec{
		at:           window.Add(time.Minute),
		window:       window,
		resource:     caller,
		attrs:        callee,
		spans:        spans,
		windowLength: otelfeeder.DefaultWindow,
	}
}

func shopCaller(service, version string) map[string]any {
	return map[string]any{
		"service.name":                service,
		"service.namespace":           "shop",
		"service.version":             version,
		"deployment.environment.name": "prod",
	}
}

// edges returns the weight class of every calls edge emitted, in order.
func edges(em *emit.MemoryEmitter) []uint32 {
	var out []uint32
	for _, ev := range em.Events() {
		if body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertEdge); ok {
			out = append(out, body.UpsertEdge.GetWeightClass())
		}
	}
	return out
}

// TestWeightClassFollowsTheSpanCount pins the published boundaries (research §8) at the one
// place a feeder can get them wrong: the count it derives from a window.
func TestWeightClassFollowsTheSpanCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		spans    int
		sampling float64
		want     uint32
	}{
		{name: "one span in five minutes is class 1", spans: 1, want: 1},
		{name: "0.1 rps is class 2", spans: 30, want: 2},
		{name: "1 rps is class 3", spans: 300, want: 3},
		{name: "10 rps is class 4", spans: 3000, want: 4},
		{name: "a tenth sampled, 1 rps is still class 3", spans: 30, sampling: 0.1, want: 3},
		{name: "a hundredth sampled, 10 rps is still class 4", spans: 30, sampling: 0.01, want: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			callee := map[string]any{"peer.service": "checkout"}
			if tc.sampling > 0 {
				callee["sampling.probability"] = tc.sampling
			}
			em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"},
				[]exportSpec{export(fixtureStart, shopCaller("storefront", "1.0.0"), callee, tc.spans)})
			got := edges(em)
			if len(got) != 1 {
				t.Fatalf("emitted %d calls edges, want 1", len(got))
			}
			if got[0] != tc.want {
				t.Errorf("weight class = %d, want %d", got[0], tc.want)
			}
		})
	}
}

// TestWeightClassChangeNeedsASecondWindow is the hysteresis of research §8: a class that moves
// for one window and moves back was noise, and the graph is never told about it.
func TestWeightClassChangeNeedsASecondWindow(t *testing.T) {
	t.Parallel()
	caller := shopCaller("storefront", "1.0.0")
	callee := map[string]any{"peer.service": "checkout"}
	window := func(i int) time.Time {
		return fixtureStart.Add(time.Duration(i) * otelfeeder.DefaultWindow)
	}

	t.Run("a one-window excursion is not reported", func(t *testing.T) {
		t.Parallel()
		em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
			export(window(0), caller, callee, 300),
			export(window(1), caller, callee, 30),
			export(window(2), caller, callee, 300),
		})
		if got := edges(em); len(got) != 1 || got[0] != 3 {
			t.Fatalf("weight classes = %v, want one class 3: an excursion of one window is noise", got)
		}
	})

	t.Run("a change that holds is reported, stamped where it became true", func(t *testing.T) {
		t.Parallel()
		em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
			export(window(0), caller, callee, 300),
			export(window(1), caller, callee, 30),
			export(window(2), caller, callee, 30),
		})
		got := edges(em)
		if len(got) != 2 || got[0] != 3 || got[1] != 2 {
			t.Fatalf("weight classes = %v, want 3 then 2", got)
		}
		for _, ev := range em.Events() {
			body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertEdge)
			if !ok || body.UpsertEdge.GetWeightClass() != 2 {
				continue
			}
			if at := body.UpsertEdge.GetValidAt().AsTime(); !at.Equal(window(1)) {
				t.Errorf("the new class is valid from %s, want %s: hysteresis delays belief, not validity",
					at, window(1))
			}
			if !strings.HasSuffix(ev.GetEventId(), "@w20260901T1305Z") {
				t.Errorf("event id %s does not name the window the class became true in", ev.GetEventId())
			}
		}
	})
}

// TestVersionTransitionIsARollout: the first version seen is the state of the world, not a
// change; the second is a rollout.
func TestVersionTransitionIsARollout(t *testing.T) {
	t.Parallel()
	callee := map[string]any{"peer.service": "checkout"}
	em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
		export(fixtureStart, shopCaller("storefront", "3.1.0"), callee, 300),
		export(fixtureStart.Add(otelfeeder.DefaultWindow), shopCaller("storefront", "3.2.0"), callee, 300),
	})

	var changes []*graphv1.ObserveChange
	for _, ev := range em.Events() {
		if body, ok := ev.GetBody().(*graphv1.EventEnvelope_ObserveChange); ok {
			changes = append(changes, body.ObserveChange)
		}
	}
	if len(changes) != 1 {
		t.Fatalf("emitted %d changes, want exactly one rollout", len(changes))
	}
	change := changes[0]
	if change.GetChange().GetKind() != graphv1.ChangeKind_ROLLOUT {
		t.Errorf("change kind = %s, want ROLLOUT", change.GetChange().GetKind())
	}
	if got, want := feeder.RefString(change.GetRef()), "otel.change=storefront@3.2.0"; got != want {
		t.Errorf("change ref = %s, want %s", got, want)
	}
	if got, want := change.GetChange().GetSummary(), "service.version 3.1.0 → 3.2.0 observed in traces"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if len(change.GetTargets()) != 1 ||
		feeder.RefString(change.GetTargets()[0]) != "otel.service.name=storefront" {
		t.Errorf("targets = %v, want the service the version belongs to", change.GetTargets())
	}
	if at := change.GetValidAt().AsTime(); !at.Equal(fixtureStart.Add(otelfeeder.DefaultWindow)) {
		t.Errorf("valid at %s, want the window the new version was first seen in", at)
	}
	// ADR-0005 D1: something deployed this, and OTLP carries no field that could say what. An
	// actor WAS observed — a service that was running one version is running another — so the
	// honest answer is ACTOR_KIND_UNKNOWN and not the zero value, which would claim the source
	// said nothing at all.
	if got := change.GetChange().GetActorKind(); got != graphv1.ActorKind_ACTOR_KIND_UNKNOWN {
		t.Errorf("actor_kind = %v, want ACTOR_KIND_UNKNOWN: an actor was observed and cannot be typed", got)
	}
}

// TestServicePointersCarryJoinKeys is ADR-0005 D6 on this feeder: every service pointer says
// which attribute of `otel-semconv/1.30` carries the version and the workload, which is what
// makes `errors_by_version(pointer, window)` expressible at all (002 FR-014b).
func TestServicePointersCarryJoinKeys(t *testing.T) {
	t.Parallel()
	em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
		export(fixtureStart, shopCaller("storefront", "3.1.0"), map[string]any{"peer.service": "checkout"}, 300),
	})

	var pointers []*graphv1.Pointer
	for _, ev := range em.Events() {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertNode)
		if !ok || body.UpsertNode.GetType() != graphv1.NodeType_SERVICE {
			continue
		}
		pointers = append(pointers, body.UpsertNode.GetPointers()...)
	}
	if len(pointers) == 0 {
		t.Fatal("no service pointers emitted")
	}
	for _, p := range pointers {
		if got := p.GetJoinKeys()[feeder.JoinRoleVersion]; got != feeder.AttrServiceVersion {
			t.Errorf("%s join_keys[version] = %q, want %q", p.GetKind(), got, feeder.AttrServiceVersion)
		}
		if got := p.GetJoinKeys()[feeder.JoinRoleWorkload]; got != feeder.AttrK8sDeploymentName {
			t.Errorf("%s join_keys[workload] = %q, want %q", p.GetKind(), got, feeder.AttrK8sDeploymentName)
		}
	}
}

// TestPointerCompatOmitsJoinKeys: a corpus recorded before ADR-0005 D6 replays byte-identically,
// because an empty join_keys map is omitted by canonical serialisation exactly as every other
// empty map is (002 tasks.md T026).
func TestPointerCompatOmitsJoinKeys(t *testing.T) {
	t.Parallel()
	em := replay(t, &otelfeeder.Feeder{
		SourceID:      "otel:unit",
		PointerCompat: feeder.PointerCompatNoJoinKeys,
	}, []exportSpec{
		export(fixtureStart, shopCaller("storefront", "3.1.0"), map[string]any{"peer.service": "checkout"}, 300),
	})

	for _, ev := range em.Events() {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertNode)
		if !ok {
			continue
		}
		for _, p := range body.UpsertNode.GetPointers() {
			if len(p.GetJoinKeys()) != 0 {
				t.Errorf("pointer %v carries join keys under PointerCompatNoJoinKeys", p.GetKind())
			}
		}
	}
}

// TestSpanAttributesNeverReachAProperty is constitution IV at the boundary this connector owns:
// a span may carry anything, and only the whitelist crosses into an event.
func TestSpanAttributesNeverReachAProperty(t *testing.T) {
	t.Parallel()
	caller := shopCaller("checkout", "2.3.1")
	caller["k8s.deployment.name"] = "checkout"
	caller["k8s.namespace.name"] = "shop"
	// Everything a real span carries and the graph must never learn.
	caller["host.name"] = "checkout-7d9f"
	caller["process.command_line"] = "/app/checkout --secret=hunter2"
	callee := map[string]any{
		"peer.service":         "payments",
		"db.statement":         "SELECT * FROM cards WHERE id = 42",
		"http.url":             "https://shop.example/pay?token=secret",
		"http.response.body":   `{"status":"ok"}`,
		"exception.stacktrace": "goroutine 1 [running]:",
		"trace_id":             "4bf92f3577b34da6a3ce929d0e0e4736",
		"metric_value":         12.5,
	}

	em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"},
		[]exportSpec{export(fixtureStart, caller, callee, 30)})

	allowed := map[string]bool{
		feeder.AttrServiceName: true, feeder.AttrServiceNamespace: true,
		feeder.AttrServiceVersion: true, feeder.AttrDeploymentEnvironment: true,
		feeder.AttrK8sDeploymentName: true, feeder.AttrK8sNamespaceName: true,
		feeder.AttrDBSystem: true, feeder.AttrServerAddress: true,
		feeder.AttrServerPort: true, feeder.AttrURLScheme: true,
		feeder.PropWindowSeconds: true,
	}
	for _, ev := range em.Events() {
		for _, props := range propsOf(ev) {
			for key, value := range props.GetFields() {
				if !allowed[key] {
					t.Errorf("%s carries the property %q, which is not on the whitelist", ev.GetEventId(), key)
				}
				if _, isString := value.GetKind().(*structpb.Value_StringValue); !isString {
					if _, isNumber := value.GetKind().(*structpb.Value_NumberValue); !isNumber {
						t.Errorf("%s property %q is neither a string nor a number", ev.GetEventId(), key)
					}
				}
			}
		}
	}
	if len(em.Events()) == 0 {
		t.Fatal("the feeder emitted nothing; the test proves nothing")
	}
}

// TestEmittingSpanDataAsPropertiesIsRefused is T054 and US3 scenario 4: the path a feeder would
// take if it "just attached the numbers" is refused before it leaves the process, with the
// published reason code naming the field (FR-009, SC-009).
//
// The feeder in this package cannot take that path — its properties come from a whitelist of
// scalars — so the test builds the event the naive way and hands it to the same emitter, which
// is the only honest way to show what the guard rail catches.
func TestEmittingSpanDataAsPropertiesIsRefused(t *testing.T) {
	t.Parallel()
	desc := (&otelfeeder.Feeder{SourceID: "otel:demo"}).Describe()

	cases := []struct {
		name  string
		props map[string]any
	}{
		{
			name: "a series of latency samples",
			props: map[string]any{
				"service.name":    "checkout",
				"latency_samples": []any{12.1, 18.4, 22.9, 31.0, 44.7},
			},
		},
		{
			name: "a span attribute map copied wholesale",
			props: map[string]any{
				"service.name": "checkout",
				"span.attributes": map[string]any{
					"trace_id":     "4bf92f3577b34da6a3ce929d0e0e4736",
					"http.url":     "https://shop.example/pay",
					"db.statement": "SELECT 1",
				},
			},
		},
		{
			name: "a single measurement under an innocent name",
			props: map[string]any{
				"service.name": "checkout",
				"p99":          map[string]any{"value": 431.2},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			props, err := structpb.NewStruct(tc.props)
			if err != nil {
				t.Fatalf("build props: %v", err)
			}
			ev := feeder.UpsertNode(desc, "otel:demo:bad-span-props-1", feeder.NodeFact{
				Ref:         feeder.Ref(feeder.NSOTelService, "checkout"),
				Type:        graphv1.NodeType_SERVICE,
				DisplayName: "checkout",
				Props:       props,
				ValidAt:     fixtureStart,
			})

			em := emit.NewMemoryEmitter(desc, emit.WithStrict(false))
			result, err := em.Emit(t.Context(), ev)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			if result.GetStatus() != graphv1.IngestResult_REJECTED {
				t.Fatalf("status = %s, want REJECTED", result.GetStatus())
			}
			if result.GetReasonCode() != feeder.ReasonTelemetryPayload {
				t.Errorf("reason code = %q, want %q", result.GetReasonCode(), feeder.ReasonTelemetryPayload)
			}
			if !strings.Contains(result.GetReasonDetail(), "props") {
				t.Errorf("reason detail %q does not name the offending property", result.GetReasonDetail())
			}
			if len(em.Events()) != 0 {
				t.Errorf("a refused event was kept as if it had been applied")
			}
			// The same refusal is available before the emitter, which is where a feeder
			// author wants it.
			if rejection := feeder.Validate(ev); rejection == nil ||
				rejection.ReasonCode != feeder.ReasonTelemetryPayload {
				t.Errorf("feeder.Validate returned %v, want a telemetry_payload rejection", rejection)
			}
		})
	}
}

// propsOf collects every property struct an event carries.
func propsOf(ev *graphv1.EventEnvelope) []*structpb.Struct {
	switch body := ev.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return []*structpb.Struct{body.UpsertNode.GetProps()}
	case *graphv1.EventEnvelope_UpsertEdge:
		return []*structpb.Struct{body.UpsertEdge.GetProps()}
	case *graphv1.EventEnvelope_IdentityClaim:
		return []*structpb.Struct{body.IdentityClaim.GetAttributes()}
	default:
		return nil
	}
}

// --- the observed side of C4 (003 T182, FR-117) ----------------------------------------------------

// cloudRunCaller is a process the GCP resource detector has annotated: the four attributes it sets on
// Cloud Run, on top of the ordinary service ones.
func cloudRunCaller(service, version, revision, project, region string) map[string]any {
	attrs := shopCaller(service, version)
	attrs["cloud.platform"] = otelfeeder.CloudRunPlatform
	attrs["faas.version"] = revision
	attrs["cloud.account.id"] = project
	attrs["cloud.region"] = region
	return attrs
}

func claimAttrs(t *testing.T, em *emit.MemoryEmitter, service string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ev := range em.Events() {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_IdentityClaim)
		if !ok || body.IdentityClaim.GetSubject().GetValue() != service {
			continue
		}
		for key, value := range body.IdentityClaim.GetAttributes().GetFields() {
			out[key] = value.GetStringValue()
		}
	}
	return out
}

// The claim carries the Cloud Run revision the process is running in, under the spellings the published
// C4 rule reads — which is what makes that rule fire at all.
//
// Until this landed, C4 was published, registered, evaluated on every claim and **never fired**: the
// GCP feeder carried the three revision-locating attributes on its own claims and nothing carried the
// matching half. The symptom was an absence, which is what makes it worth a test rather than a
// changelog line.
func TestAnObservedServiceOnCloudRunClaimsTheRevisionItRunsIn(t *testing.T) {
	t.Parallel()
	em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
		export(fixtureStart,
			cloudRunCaller("storefront", "3.1.0", "storefront-00042-abc", "nova-production", "europe-west1"),
			map[string]any{"peer.service": "checkout"}, 300),
	})

	attrs := claimAttrs(t, em, "storefront")
	for key, want := range map[string]string{
		resolution.AttrGCPRevisionName: "storefront-00042-abc",
		resolution.AttrGCPProject:      "nova-production",
		resolution.AttrGCPRegion:       "europe-west1",
	} {
		if attrs[key] != want {
			t.Errorf("claim attribute %s = %q, want %q. C4 reads these three to pair an observed "+
				"service with the revision it runs in, and a rule whose supporting attribute nobody "+
				"emits never fires — silently (003 FR-117)", key, attrs[key], want)
		}
	}
}

// And the gate: `faas.version` is a revision on Cloud Run and an alias on AWS Lambda, so a process that
// does not state `cloud.platform=gcp_cloud_run` claims none of it.
//
// Without the gate a *certain* rule would pair a Lambda alias with a Cloud Run revision of the same
// name and merge two unrelated entities with no human in the loop, which is the expensive direction.
func TestAProcessThatIsNotOnCloudRunClaimsNoRevision(t *testing.T) {
	t.Parallel()
	for _, platform := range []string{"aws_lambda", "azure_functions", ""} {
		attrs := shopCaller("storefront", "3.1.0")
		attrs["faas.version"] = "storefront-00042-abc"
		attrs["cloud.account.id"] = "nova-production"
		attrs["cloud.region"] = "europe-west1"
		if platform != "" {
			attrs["cloud.platform"] = platform
		}
		em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
			export(fixtureStart, attrs, map[string]any{"peer.service": "checkout"}, 300),
		})
		if got := claimAttrs(t, em, "storefront")[resolution.AttrGCPRevisionName]; got != "" {
			t.Errorf("cloud.platform=%q claimed revision %q; faas.version means a Cloud Run revision "+
				"only on Cloud Run, and a certain rule pairing on it elsewhere would merge two "+
				"unrelated entities", platform, got)
		}
	}
}

// All three or none. A claim carrying the revision without the project would let C4 merge across
// projects — `storefront-00042-abc` in staging and in production are two revisions — and that is the
// failure FR-010 exists to prevent, made by a certain rule with no human in the loop.
func TestAPartialSetOfCloudRunCoordinatesClaimsNothing(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"faas.version", "cloud.account.id", "cloud.region"} {
		attrs := cloudRunCaller("storefront", "3.1.0", "storefront-00042-abc", "nova-production", "europe-west1")
		delete(attrs, missing)
		em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
			export(fixtureStart, attrs, map[string]any{"peer.service": "checkout"}, 300),
		})
		claimed := claimAttrs(t, em, "storefront")
		for _, key := range []string{
			resolution.AttrGCPRevisionName, resolution.AttrGCPProject, resolution.AttrGCPRegion,
		} {
			if claimed[key] != "" {
				t.Errorf("with %s absent, the claim still carries %s=%q; a revision name is unique "+
					"within a service and not globally, so a partial set would merge across projects",
					missing, key, claimed[key])
			}
		}
	}
}

// And the whole point, end to end: with both halves present, C4 fires.
//
// This is the assertion the two connectors exist to satisfy between them, and it is here rather than in
// internal/resolution because that package may not import a feeder — a rule has to be testable without
// a connector present. The pairing itself is asserted from this side, where the import is legal.
func TestC4FiresOnceBothHalvesArePresent(t *testing.T) {
	t.Parallel()
	em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
		export(fixtureStart,
			cloudRunCaller("storefront", "3.1.0", "storefront-00042-abc", "nova-production", "europe-west1"),
			map[string]any{"peer.service": "checkout"}, 300),
	})
	observed := resolution.Claim{
		ClaimID: "claim-otel", EntityID: "entity-observed", EntityType: graph.NodeTypeService,
		SourceID: "otel:unit", Namespace: resolution.NamespaceOTelService, Value: "storefront",
		Attributes: claimAttrs(t, em, "storefront"),
	}
	// The GCP feeder's half, as it emits it.
	revision := resolution.Claim{
		ClaimID: "claim-gcp", EntityID: "entity-revision", EntityType: graph.NodeTypeWorkload,
		SourceID: "gcp:nova", Namespace: resolution.NamespaceGCPCloudRunRevision,
		Value: "nova-production/europe-west1/storefront/storefront-00042-abc",
		Attributes: map[string]string{
			resolution.AttrGCPRevisionName: "storefront-00042-abc",
			resolution.AttrGCPProject:      "nova-production",
			resolution.AttrGCPRegion:       "europe-west1",
		},
	}
	// The Cloud Run SERVICE the revision belongs to, which is the entity C4 merges with. The
	// revision is the evidence rather than a party to the merge: merging with a revision made two
	// revisions of one service into one entity across a rollout (internal/resolution, 2026-09-22).
	service := resolution.Claim{
		ClaimID: "claim-gcp-svc", EntityID: "entity-service", EntityType: graph.NodeTypeService,
		SourceID: "gcp:nova", Namespace: resolution.NamespaceGCPCloudRunService,
		Value: "nova-production/europe-west1/storefront",
	}

	matches, err := resolution.Evaluate(context.Background(),
		claimPair{claims: []resolution.Claim{observed, revision, service}}, observed)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	var fired bool
	for _, match := range matches {
		if match.RuleID == "C4" {
			fired = true
			if !match.Certain {
				t.Error("C4 fired without being certain")
			}
			if match.EntityA == match.EntityB {
				t.Error("C4 paired an entity with itself")
			}
			if match.EntityB != "entity-service" {
				t.Errorf("C4 merged the observed service with %q, want the Cloud Run service; a "+
					"revision is a version of the service, not the service", match.EntityB)
			}
		}
	}
	if !fired {
		t.Fatalf("C4 did not fire with both halves present: %+v. It is published, registered and "+
			"evaluated on every claim, so a rule that finds nothing to pair with fails silently — "+
			"which is what this test exists to stop happening again (003 FR-117, T182)", matches)
	}
}

// claimPair is the smallest ClaimStore that can answer C4: a fixed pair of claims.
type claimPair struct{ claims []resolution.Claim }

func (s claimPair) ClaimsFor(_ context.Context, namespace, value string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s.claims {
		if claim.Namespace == namespace && claim.Value == value {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s claimPair) ClaimsMatchingAttributes(_ context.Context, namespace string, attrs map[string]string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s.claims {
		if claim.Namespace != namespace {
			continue
		}
		match := true
		for key, want := range attrs {
			if claim.Attr(key) != want {
				match = false
				break
			}
		}
		if match {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s claimPair) ClaimsInNamespaces(_ context.Context, namespaces []string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s.claims {
		if slices.Contains(namespaces, claim.Namespace) {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s claimPair) SharedOwner(context.Context, string, string) (bool, error) { return false, nil }

func (s claimPair) ChangeTargets(context.Context, string) ([]string, error) { return nil, nil }

// CorrelatedWith completes resolution.ClaimStore. No correlation key reaches C4 or C5: both compare identity claims, and a store that returned
// one here would be answering a question no rule under test asks.
func (claimPair) CorrelatedWith(context.Context, string, string) ([]resolution.Correlation, error) {
	return nil, nil
}

// ---- C7's observed half: the Cloud SQL connection name (003 FR-120) ----------------------------

// dbCallee is a span calling a database at an address.
func dbCallee(system, address string) map[string]any {
	return map[string]any{"db.system": system, "server.address": address}
}

// The dependency claim carries the instance connection name when the caller reached the database
// over the Cloud SQL connector's Unix socket.
//
// The address IS the evidence: `/cloudsql/<project>:<region>:<instance>` is the exact string the
// client's configuration was given, so a caller using it is quoting the identifier rather than
// resembling it. That is what C7 rests on, and until this landed the rule had only the GCP feeder's
// own side.
func TestADatabaseReachedOverTheCloudSQLSocketClaimsTheConnectionName(t *testing.T) {
	t.Parallel()
	const connection = "nova-production:europe-west1:orders-primary"
	for name, address := range map[string]string{
		"the instance directory":   otelfeeder.CloudSQLSocketPrefix + connection,
		"the driver's socket file": otelfeeder.CloudSQLSocketPrefix + connection + "/.s.PGSQL.5432",
		"a MySQL socket file":      otelfeeder.CloudSQLSocketPrefix + connection + "/mysql.sock",
	} {
		t.Run(name, func(t *testing.T) {
			em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
				export(fixtureStart, shopCaller("storefront", "3.1.0"),
					dbCallee("postgresql", address), 300),
			})
			attrs := claimAttrs(t, em, address)
			if got := attrs[resolution.AttrGCPSQLConnectionName]; got != connection {
				t.Errorf("claim attribute %s = %q, want %q. C7 reads it to pair a caller's address "+
					"with the instance GCP lists, and a rule whose supporting attribute nobody "+
					"emits never fires — silently (003 FR-120)",
					resolution.AttrGCPSQLConnectionName, got, connection)
			}
		})
	}
}

// An ordinary database address claims no connection name. There is nothing to quote: a host is a
// resemblance, and C7 is a certain rule.
func TestAnOrdinaryDatabaseAddressClaimsNoConnectionName(t *testing.T) {
	t.Parallel()
	for _, address := range []string{
		"orders-primary.shop.svc.cluster.local",
		"10.0.0.7",
		"127.0.0.1",
		"nova-production:europe-west1:orders-primary", // the connection name with no socket path
	} {
		em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
			export(fixtureStart, shopCaller("storefront", "3.1.0"),
				dbCallee("postgresql", address), 300),
		})
		if got := claimAttrs(t, em, address)[resolution.AttrGCPSQLConnectionName]; got != "" {
			t.Errorf("address %q claimed connection name %q; only the connector's socket path "+
				"carries one, and a certain rule pairing on anything looser would merge unrelated "+
				"instances with no human in the loop", address, got)
		}
	}
}

// A socket path whose instance part is not shaped like a connection name claims nothing.
//
// This is the guard that matters most, and it is the same one the rule itself carries: a malformed
// string in this attribute would merge every instance that made the same mistake into one entity,
// automatically and with a score of 1.0. Two guards are better than one here because either side
// alone is a single point of failure for a merge nobody reviews.
func TestAMalformedCloudSQLSocketPathClaimsNoConnectionName(t *testing.T) {
	t.Parallel()
	for _, rest := range []string{
		"", ":", "::", "a:b:c:d",
		"nova-production:europe-west1",
		"nova-production::orders-primary",
		"nova-production: :orders-primary",
		"nova-production:europe west1:orders-primary",
	} {
		address := otelfeeder.CloudSQLSocketPrefix + rest
		em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
			export(fixtureStart, shopCaller("storefront", "3.1.0"),
				dbCallee("postgresql", address), 300),
		})
		if got := claimAttrs(t, em, address)[resolution.AttrGCPSQLConnectionName]; got != "" {
			t.Errorf("malformed socket path %q claimed connection name %q; three non-empty "+
				"whitespace-free colon-separated parts or nothing", address, got)
		}
	}
}

// And the whole point, end to end: with both halves present, C7 fires — across two namespaces.
//
// It is the namespace crossing that was missing. The rule searched `gcp.sql.instance` on both sides,
// and the GCP feeder is the only source that claims there while the rule refuses two claims from one
// source — so the only pair it could match was one that cannot exist. It was published, registered
// and evaluated on every claim, and could not fire. This asserts the pairing from the side where
// importing a feeder is legal, as C4's does.
func TestC7FiresAcrossNamespacesOnceBothHalvesArePresent(t *testing.T) {
	t.Parallel()
	const connection = "nova-production:europe-west1:orders-primary"
	address := otelfeeder.CloudSQLSocketPrefix + connection + "/.s.PGSQL.5432"
	em := replay(t, &otelfeeder.Feeder{SourceID: "otel:unit"}, []exportSpec{
		export(fixtureStart, shopCaller("storefront", "3.1.0"),
			dbCallee("postgresql", address), 300),
	})
	observed := resolution.Claim{
		ClaimID: "claim-otel", EntityID: "entity-observed", EntityType: graph.NodeTypeInfraResource,
		SourceID: "otel:unit", Namespace: resolution.NamespaceServerAddress, Value: address,
		Attributes: claimAttrs(t, em, address),
	}
	// The GCP feeder's half, as it emits it: the instance addressed the way GCP lists it.
	instance := resolution.Claim{
		ClaimID: "claim-gcp", EntityID: "entity-instance", EntityType: graph.NodeTypeInfraResource,
		SourceID: "gcp:nova", Namespace: resolution.NamespaceGCPSQLInstance,
		Value:      "nova-production:europe-west1:orders-primary",
		Attributes: map[string]string{resolution.AttrGCPSQLConnectionName: connection},
	}

	matches, err := resolution.Evaluate(context.Background(),
		claimPair{claims: []resolution.Claim{observed, instance}}, observed)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	var fired bool
	for _, match := range matches {
		if match.RuleID != "C7" {
			continue
		}
		fired = true
		if !match.Certain || match.Score != 1.0 {
			t.Errorf("C7 fired certain=%v score=%v, want true and 1.0", match.Certain, match.Score)
		}
		if match.EntityA == match.EntityB {
			t.Error("C7 paired an entity with itself")
		}
		if !strings.Contains(match.Rationale, connection) {
			t.Errorf("rationale does not quote the connection name it merged on: %q", match.Rationale)
		}
	}
	if !fired {
		t.Fatalf("C7 did not fire with both halves present: %+v. It searched one namespace on both "+
			"sides, which made it unfireable by construction rather than for want of data — the "+
			"failure this test exists to stop happening again (003 FR-120)", matches)
	}
}
