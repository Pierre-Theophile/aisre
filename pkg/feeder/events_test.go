// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// testDescription is the k8s feeder of the baseline fixture, so the events built here have the
// shape the fixtures record.
func testDescription() feeder.Description {
	return feeder.Description{
		SourceID:         "k8s:demo",
		Kind:             "k8s",
		Ordering:         feeder.OrderingPerSourceSequence,
		ReorderingWindow: 60_000_000_000, // 60s
		Namespaces:       []string{feeder.NSK8sDeployment, feeder.NSK8sCluster, feeder.NSK8sChange, feeder.NSOTelService, feeder.NSAppName},
	}
}

func TestEventConstructorsValidateClean(t *testing.T) {
	t.Parallel()
	d := testDescription()
	validAt := mustTime(t, "2026-09-01T13:00:00Z")

	props, err := feeder.NewProps().
		Str(feeder.AttrK8sNamespaceName, "shop").
		Int(feeder.PropK8sReplicas, 3).
		Build()
	if err != nil {
		t.Fatalf("props: %v", err)
	}

	events := map[string]*graphv1.EventEnvelope{
		"upsert_node": feeder.UpsertNode(d, feeder.NewID(d.SourceID, "deploy", "shop/checkout@rv1001"), feeder.NodeFact{
			Meta:        feeder.Meta{Seq: 1001, SourceObservedAt: validAt},
			Ref:         feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
			Type:        graphv1.NodeType_WORKLOAD,
			DisplayName: "checkout",
			Props:       props,
			Pointers: []*graphv1.Pointer{feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource,
				"apps/v1/namespaces/shop/deployments/checkout",
				map[string]string{feeder.AttrK8sNamespaceName: "shop"})},
			ValidAt: validAt,
		}),
		"upsert_node with an unknown start": feeder.UpsertNode(d, "k8s:demo:deploy:shop/legacy@rv1", feeder.NodeFact{
			Ref:              feeder.Ref(feeder.NSK8sDeployment, "shop/legacy"),
			Type:             graphv1.NodeType_WORKLOAD,
			ValidFromUnknown: true,
		}),
		"upsert_edge": feeder.UpsertEdge(d, "k8s:demo:edge:runs-on:shop/checkout@rv1001", feeder.EdgeFact{
			Src:         feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
			Dst:         feeder.Ref(feeder.NSK8sCluster, "shop-prod"),
			Type:        graphv1.EdgeType_RUNS_ON,
			WeightClass: feeder.WeightClassPtr(feeder.WeightClass(12)),
			ValidAt:     validAt,
		}),
		"retract_node": feeder.RetractNode(d, "k8s:demo:retract:shop/checkout@rv2000", feeder.NodeRetraction{
			Ref:      feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
			ValidEnd: validAt,
		}),
		"retract_edge": feeder.RetractEdge(d, "k8s:demo:retract-edge:shop/checkout@rv2000", feeder.EdgeRetraction{
			Src:      feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
			Dst:      feeder.Ref(feeder.NSK8sCluster, "shop-prod"),
			Type:     graphv1.EdgeType_RUNS_ON,
			ValidEnd: validAt,
		}),
		"observe_change": feeder.ObserveChange(d, "k8s:demo:change:shop/checkout@rev8", feeder.ChangeFact{
			Ref:       feeder.Ref(feeder.NSK8sChange, "shop/checkout@rev8"),
			Kind:      graphv1.ChangeKind_ROLLOUT,
			Summary:   "rollout checkout to revision 8",
			Actor:     "deploy-bot",
			OriginRef: "https://github.com/shop/checkout/actions/runs/1841",
			Targets:   []*graphv1.Ref{feeder.Ref(feeder.NSK8sDeployment, "shop/checkout")},
			ValidAt:   validAt,
		}),
		"identity_claim": feeder.IdentityClaim(d, "k8s:demo:claim:shop/checkout@app.kubernetes.io-name", feeder.IdentityFact{
			Subject:    feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
			Claim:      feeder.Ref(feeder.NSAppName, "checkout"),
			Attributes: props,
		}),
		"source_checkpoint": feeder.SourceCheckpoint(d, feeder.CheckpointID(d, validAt), feeder.CheckpointFact{
			ExtentFrom: validAt,
			ExtentTo:   validAt,
			Note:       "initial informer list complete",
		}),
	}

	for name, ev := range events {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if r := feeder.Validate(ev); r != nil {
				t.Fatalf("%s does not validate: %v", name, r)
			}
			if ev.GetSourceId() != d.SourceID {
				t.Errorf("source id %q, want %q", ev.GetSourceId(), d.SourceID)
			}
			if ev.GetSchemaVersion() != feeder.SchemaVersion {
				t.Errorf("schema version %q, want %q", ev.GetSchemaVersion(), feeder.SchemaVersion)
			}
			if ev.GetIdempotencyKey() != ev.GetEventId() {
				t.Errorf("idempotency key %q, want the event id %q", ev.GetIdempotencyKey(), ev.GetEventId())
			}
		})
	}
}

func TestEventMetaOverrides(t *testing.T) {
	t.Parallel()
	d := testDescription()
	ev := feeder.UpsertNode(d, "k8s:demo:deploy:shop/checkout@rv1001", feeder.NodeFact{
		Meta: feeder.Meta{
			Seq:              1001,
			SourceObservedAt: mustTime(t, "2026-09-01T13:00:02Z"),
			IdempotencyKey:   "k8s:demo:deploy:shop/checkout",
		},
		Ref:     feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
		Type:    graphv1.NodeType_WORKLOAD,
		ValidAt: mustTime(t, "2026-09-01T13:00:00Z"),
	})
	if ev.GetSourceSeq() != 1001 {
		t.Errorf("source seq %d, want 1001", ev.GetSourceSeq())
	}
	if got, want := ev.GetIdempotencyKey(), "k8s:demo:deploy:shop/checkout"; got != want {
		t.Errorf("idempotency key %q, want %q", got, want)
	}
	if ev.GetSourceObservedAt() == nil {
		t.Error("source observed at was dropped; it is kept as provenance (FR-019)")
	}
}

func TestEventMissingValidTimeIsRefusedLocally(t *testing.T) {
	t.Parallel()
	d := testDescription()
	ev := feeder.UpsertNode(d, "k8s:demo:deploy:shop/checkout@rv1001", feeder.NodeFact{
		Ref:  feeder.Ref(feeder.NSK8sDeployment, "shop/checkout"),
		Type: graphv1.NodeType_WORKLOAD,
	})
	r := feeder.Validate(ev)
	if r == nil {
		t.Fatal("an upsert with no valid time validated; FR-010 requires it to be refused")
	}
	if r.ReasonCode != feeder.ReasonMissingValidTime {
		t.Errorf("reason code %q, want %q", r.ReasonCode, feeder.ReasonMissingValidTime)
	}
}

func TestPointerConstructors(t *testing.T) {
	t.Parallel()
	attrs := map[string]string{
		feeder.AttrServiceName:           "storefront",
		feeder.AttrServiceNamespace:      "shop",
		feeder.AttrDeploymentEnvironment: "prod",
	}
	selector := feeder.OTelSelector(attrs, feeder.AttrServiceName, feeder.AttrServiceNamespace)
	if want := `service.name="storefront" AND service.namespace="shop"`; selector != want {
		t.Fatalf("OTelSelector = %q, want %q", selector, want)
	}

	trace := feeder.TracePointer("tempo", selector, attrs)
	if trace.GetKind() != graphv1.PointerKind_TRACE || trace.GetVocabulary() != feeder.VocabOTelSemconv {
		t.Errorf("TracePointer = %v", trace)
	}
	if len(trace.GetAttributes()) != len(attrs) {
		t.Errorf("TracePointer attributes = %v, want all %d", trace.GetAttributes(), len(attrs))
	}

	metricSelector := feeder.MetricSelector("http.server.request.duration", attrs,
		feeder.AttrServiceName, feeder.AttrServiceNamespace)
	want := `service.name="storefront" AND service.namespace="shop" AND metric.name="http.server.request.duration"`
	if metricSelector != want {
		t.Fatalf("MetricSelector = %q, want %q", metricSelector, want)
	}
	if feeder.MetricPointer("prometheus", metricSelector, attrs).GetKind() != graphv1.PointerKind_METRIC {
		t.Error("MetricPointer has the wrong kind")
	}
	if feeder.LogPointer("loki", selector, attrs).GetKind() != graphv1.PointerKind_LOG {
		t.Error("LogPointer has the wrong kind")
	}
	if p := feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource, "apps/v1/namespaces/shop/deployments/checkout", nil); p.GetKind() != graphv1.PointerKind_SOURCE_LINK {
		t.Error("SourceLinkPointer has the wrong kind")
	}

	// Sorted by key when no keys are named, so a selector is byte-stable across runs.
	if got, want := feeder.OTelSelector(attrs), `deployment.environment.name="prod" AND service.name="storefront" AND service.namespace="shop"`; got != want {
		t.Errorf("OTelSelector with no keys = %q, want %q", got, want)
	}
	// A named key that is absent is skipped rather than rendered empty.
	if got := feeder.OTelSelector(attrs, feeder.AttrServiceName, "absent.key"); got != `service.name="storefront"` {
		t.Errorf("OTelSelector with an absent key = %q", got)
	}
}

func TestPointerAttributesAreCopied(t *testing.T) {
	t.Parallel()
	attrs := map[string]string{feeder.AttrServiceName: "checkout"}
	p := feeder.TracePointer("tempo", "", attrs)
	attrs[feeder.AttrServiceName] = "payments"
	if got := p.GetAttributes()[feeder.AttrServiceName]; got != "checkout" {
		t.Errorf("mutating the caller's map changed an emitted pointer: %q", got)
	}
}

func TestDescriptionValidateAndRegisterRequest(t *testing.T) {
	t.Parallel()
	if err := (feeder.Description{Kind: "k8s"}).Validate(); err == nil {
		t.Error("a description with no source id validated")
	}
	if err := (feeder.Description{SourceID: "k8s:demo"}).Validate(); err == nil {
		t.Error("a description with no kind validated")
	}
	if err := (feeder.Description{SourceID: "k8s:demo", Kind: "k8s", Ordering: "whenever"}).Validate(); err == nil {
		t.Error("a description with an unknown ordering validated")
	}

	d := testDescription()
	req := d.RegisterRequest()
	if req.GetSourceId() != "k8s:demo" || req.GetKind() != "k8s" {
		t.Errorf("RegisterRequest = %v", req)
	}
	if req.GetOrdering() != string(feeder.OrderingPerSourceSequence) {
		t.Errorf("ordering %q", req.GetOrdering())
	}
	if req.GetReorderingWindowSeconds() != 60 {
		t.Errorf("reordering window %d seconds, want 60", req.GetReorderingWindowSeconds())
	}
	if req.GetSchemaVersion() != feeder.SchemaVersion {
		t.Errorf("schema version %q", req.GetSchemaVersion())
	}

	if !d.DeclaresNamespace(feeder.NSK8sDeployment) {
		t.Error("a declared namespace was reported undeclared")
	}
	if d.DeclaresNamespace(feeder.NSK8sSecret) {
		t.Error("an undeclared namespace was reported declared")
	}
	if !(feeder.Description{}).DeclaresNamespace("anything") {
		t.Error("a description naming no namespaces should declare them all")
	}
}
