// SPDX-License-Identifier: Apache-2.0

package otel_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
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

	otelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/otel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Generating the recorded corpus (T061).
//
// Docker is not available on this machine, so there is no collector and no cluster to record
// from: the payloads under testdata/ are synthesized here instead, from the same attribute
// shapes deploy/kind/otel-demo/minimal/traffic.yaml makes telemetrygen send. That makes them
// synthetic, which constitution VIII says is not enough for a connector to be called stable —
// the manifests say so, and replacing them with a real recording is a file swap, not a code
// change, because the feeder never sees the difference (FR-044).
//
// Regenerate with:
//
//	SRE_AGENT_GEN_TESTDATA=1 go test ./internal/feeders/otel -run TestGenerateTestdata
//
// The output is byte-for-byte reproducible: every trace and span id is a hash of the payload's
// position, and every timestamp is derived from the fixture clock.

// genEnv is the environment variable that arms the generator. Regenerating a corpus is a
// deliberate act: the corpus is the test.
const genEnv = "SRE_AGENT_GEN_TESTDATA"

// The fixture clock. Both corpora start at the same instant as fixtures/baseline-topology-01,
// which is what lets the baseline one reproduce its 23 recorded events.
var fixtureStart = time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)

// exportSpec is one OTLP export request: one resource (the caller), one set of span attributes
// (the callee), and how many spans carry them.
type exportSpec struct {
	// at is when the exporter sent the request.
	at time.Time
	// window is the aggregation window the spans fall in; their start times are spread
	// across it.
	window time.Time
	// resource and attrs are the resource and span attributes, exactly as telemetrygen's
	// --otlp-attributes and --telemetry-attributes would set them.
	resource map[string]any
	attrs    map[string]any
	// spans is how many spans the request carries.
	spans int
	// windowLength bounds the spread of span start times.
	windowLength time.Duration
}

// baselineShop is the synthetic recording of fixtures/baseline-topology-01's shop, in the
// order the fixture's 23 events are recorded in: the callers appear storefront, checkout,
// payments, inventory and the dependencies payments-db, stripe, redis.
//
// Every span carries sampling.probability=0.1, so each stands for ten. That is what keeps the
// corpus at tens of kilobytes rather than half a megabyte while still landing on the fixture's
// weight classes — 360 recorded spans are 3600 spans over 300 s, 12 rps, class 4 — and it
// exercises the sampling path of research §11 in the acceptance fixture rather than only in a
// unit test.
func baselineShop() []exportSpec {
	caller := func(service, version string) map[string]any {
		return map[string]any{
			"service.name":                service,
			"service.namespace":           "shop",
			"service.version":             version,
			"deployment.environment.name": "prod",
			"k8s.cluster.name":            "sre-agent-demo",
			"k8s.namespace.name":          "shop",
			"k8s.deployment.name":         service,
			"telemetry.sdk.language":      "go",
			"telemetry.distro.version":    "0.161.0",
			"process.runtime.description": "go1.27.0",
			"host.name":                   "traffic-" + service + "-0",
		}
	}
	sampled := func(extra map[string]any) map[string]any {
		attrs := map[string]any{"sampling.probability": 0.1}
		for k, v := range extra {
			attrs[k] = v
		}
		return attrs
	}
	redis := map[string]any{
		"peer.service":   "redis",
		"db.system":      "redis",
		"server.address": "redis.shop.svc.cluster.local",
		"server.port":    int64(6379),
	}

	storefront, checkout := caller("storefront", "3.1.0"), caller("checkout", "2.3.1")
	payments, inventory := caller("payments", "1.4.0"), caller("inventory", "0.9.7")

	specs := []exportSpec{
		{resource: storefront, attrs: sampled(map[string]any{"peer.service": "checkout"}), spans: 360},
		{resource: checkout, attrs: sampled(map[string]any{"peer.service": "payments"}), spans: 60},
		{resource: checkout, attrs: sampled(map[string]any{"peer.service": "inventory"}), spans: 60},
		{resource: payments, attrs: sampled(map[string]any{
			"db.system":      "postgresql",
			"db.namespace":   "payments",
			"server.address": "payments-db.shop.svc.cluster.local",
			"server.port":    int64(5432),
		}), spans: 60},
		{resource: payments, attrs: sampled(map[string]any{
			"server.address":      "api.stripe.com",
			"server.port":         int64(443),
			"url.scheme":          "https",
			"http.request.method": "POST",
		}), spans: 6},
		{resource: checkout, attrs: sampled(redis), spans: 60},
		{resource: payments, attrs: sampled(redis), spans: 6},
		{resource: inventory, attrs: sampled(redis), spans: 6},
	}
	for i := range specs {
		specs[i].at = fixtureStart.Add(time.Minute + time.Duration(i)*10*time.Second)
		specs[i].window = fixtureStart
		specs[i].windowLength = otelfeeder.DefaultWindow
	}
	return specs
}

// rolloutAndRetract is the second corpus: six windows in which inventory is rolled out, the
// storefront→checkout call path changes weight class, and payments stops calling stripe.
//
// Window 0 (13:00) is the whole shop. From window 1 (13:05) payments never calls stripe again,
// so the edge is retracted six windows later. Window 2 (13:10) is the first at inventory
// 0.9.8, so that is where the rollout is observed. storefront→checkout drops from class 4 to
// class 3 in window 3 and stays there, so the new class is believed in window 4 and stamped
// with window 3, where it became true.
func rolloutAndRetract() []exportSpec {
	named := func(service, version string) map[string]any {
		return map[string]any{
			"service.name":                service,
			"service.namespace":           "shop",
			"service.version":             version,
			"deployment.environment.name": "prod",
			"k8s.namespace.name":          "shop",
			"k8s.deployment.name":         service,
		}
	}

	var specs []exportSpec
	for w := range 7 {
		window := fixtureStart.Add(time.Duration(w) * otelfeeder.DefaultWindow)
		inventoryVersion := "0.9.7"
		if w >= 2 {
			inventoryVersion = "0.9.8"
		}
		storefrontSpans := 360
		if w >= 3 {
			storefrontSpans = 60
		}
		round := []exportSpec{
			{resource: named("storefront", "3.1.0"),
				attrs: map[string]any{"sampling.probability": 0.1, "peer.service": "checkout"},
				spans: storefrontSpans},
			{resource: named("checkout", "2.3.1"),
				attrs: map[string]any{"sampling.probability": 0.1, "peer.service": "inventory"},
				spans: 60},
			{resource: named("inventory", inventoryVersion),
				attrs: map[string]any{
					"sampling.probability": 0.1,
					"db.system":            "postgresql",
					"server.address":       "inventory-db.shop.svc.cluster.local",
					"server.port":          int64(5432),
				},
				spans: 60},
		}
		if w == 0 {
			round = append(round, exportSpec{
				resource: named("payments", "1.4.0"),
				attrs: map[string]any{
					"sampling.probability": 0.1,
					"server.address":       "api.stripe.com",
					"server.port":          int64(443),
					"url.scheme":           "https",
				},
				spans: 6,
			})
		}
		for i := range round {
			round[i].at = window.Add(time.Minute + time.Duration(i)*10*time.Second)
			round[i].window = window
			round[i].windowLength = otelfeeder.DefaultWindow
		}
		specs = append(specs, round...)
	}
	return specs
}

// TestGenerateTestdata writes both recorded corpora. It is skipped unless SRE_AGENT_GEN_TESTDATA=1.
func TestGenerateTestdata(t *testing.T) {
	if os.Getenv(genEnv) != "1" {
		t.Skipf("set %s=1 to regenerate the recorded payloads", genEnv)
	}
	writeCorpus(t, filepath.Join("testdata", "baseline-shop"), baselineShop())

	// baseline-shop's events.jsonl is not recorded: it is the twenty-three otel:demo lines of
	// fixtures/baseline-topology-01, which is what the corpus has to reproduce. Only the
	// second corpus records what this feeder emitted, after the author has read it.
	dir := filepath.Join("testdata", "rollout-and-retract")
	writeCorpus(t, dir, rolloutAndRetract())
	recordEvents(t, dir, &otelfeeder.Feeder{SourceID: "otel:demo"})
}

// recordEvents replays a corpus through the feeder and writes what it emitted as the corpus's
// events.jsonl (the SDK guide §9). The observed times are the replay clock's, which is what
// testkit ignores when it compares.
func recordEvents(t *testing.T, dir string, f *otelfeeder.Feeder) {
	t.Helper()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear %s: %v", path, err)
	}
	src, err := source.NewFileSource(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	clock := fixtureStart
	em := record.Emitter(emit.NewMemoryEmitter(f.Describe(), emit.WithMemoryClock(func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	})), dir)
	if err := f.Run(t.Context(), src, em); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := em.Err(); err != nil {
		t.Fatalf("record: %v", err)
	}
	if rejections := em.Rejections(); len(rejections) > 0 {
		t.Fatalf("the feeder emitted %d events the graph would refuse, the first is %v", len(rejections), rejections[0])
	}
	t.Logf("recorded %d events to %s", em.Accepted(), path)
}

// writeCorpus writes payloads/traces/*.pb and payloads/index.jsonl for one corpus.
func writeCorpus(t *testing.T, dir string, specs []exportSpec) {
	t.Helper()
	tracesDir := filepath.Join(dir, source.PayloadsDir, otelfeeder.PayloadKindTraces)
	if err := os.RemoveAll(tracesDir); err != nil {
		t.Fatalf("clear %s: %v", tracesDir, err)
	}
	if err := os.MkdirAll(tracesDir, 0o750); err != nil {
		t.Fatalf("create %s: %v", tracesDir, err)
	}

	var index []byte
	for i, spec := range specs {
		name := fmt.Sprintf("%06d.pb", i+1)
		raw, err := proto.Marshal(buildExport(spec, i))
		if err != nil {
			t.Fatalf("marshal export %d: %v", i, err)
		}
		if err := os.WriteFile(filepath.Join(tracesDir, name), raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		line, err := json.Marshal(source.IndexEntry{
			Kind: otelfeeder.PayloadKindTraces,
			At:   spec.at.UTC(),
			File: otelfeeder.PayloadKindTraces + "/" + name,
		})
		if err != nil {
			t.Fatalf("marshal index entry %d: %v", i, err)
		}
		index = append(append(index, line...), '\n')
	}
	path := filepath.Join(dir, source.PayloadsDir, source.IndexFile)
	if err := os.WriteFile(path, index, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("wrote %d payloads to %s", len(specs), tracesDir)
}

// buildExport renders one export request. Ids are hashes of (corpus position, span index), so
// two runs of the generator produce identical bytes.
func buildExport(spec exportSpec, index int) *coltracepb.ExportTraceServiceRequest {
	spans := make([]*tracepb.Span, 0, spec.spans)
	step := spec.windowLength / time.Duration(max(spec.spans, 1))
	for i := range spec.spans {
		start := spec.window.Add(time.Duration(i) * step)
		spans = append(spans, &tracepb.Span{
			TraceId:           idBytes(16, index, i, 1),
			SpanId:            idBytes(8, index, i, 2),
			Name:              "lets-go",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(start.UnixNano()),       //nolint:gosec // a fixture clock well inside range
			EndTimeUnixNano:   uint64(start.UnixNano() + 2e7), //nolint:gosec // as above
			Attributes:        keyValues(spec.attrs),
			Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_UNSET},
		})
	}
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: keyValues(spec.resource)},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Scope: &commonpb.InstrumentationScope{Name: "telemetrygen", Version: "v0.161.0"},
				Spans: spans,
			}},
		}},
	}
}

// idBytes derives a deterministic trace or span id.
func idBytes(n, index, span, salt int) []byte {
	var seed [24]byte
	binary.BigEndian.PutUint64(seed[0:], uint64(index)) //nolint:gosec // small positive ints
	binary.BigEndian.PutUint64(seed[8:], uint64(span))  //nolint:gosec // small positive ints
	binary.BigEndian.PutUint64(seed[16:], uint64(salt)) //nolint:gosec // small positive ints
	sum := sha256.Sum256(seed[:])
	return sum[:n]
}

// keyValues renders an attribute map as OTLP key-values, sorted so the bytes are stable.
func keyValues(attrs map[string]any) []*commonpb.KeyValue {
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]*commonpb.KeyValue, 0, len(keys))
	for _, key := range keys {
		out = append(out, &commonpb.KeyValue{Key: key, Value: anyValue(attrs[key])})
	}
	return out
}

func anyValue(v any) *commonpb.AnyValue {
	switch value := v.(type) {
	case string:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}
	case int64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}
	case float64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: value}}
	case bool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: value}}
	default:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprint(value)}}
	}
}
