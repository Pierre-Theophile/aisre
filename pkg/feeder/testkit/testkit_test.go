// SPDX-License-Identifier: Apache-2.0

package testkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// A feeder small enough to read in one sitting, written the way the documentation tells a
// connector author to write one: deterministic ids from source-native identifiers, valid time
// from the payload, one checkpoint at the end, Flush before returning.
//
// It exists to prove the testkit itself works end to end — record a fixture from it, then hold
// it to the conformance suite — so it is deliberately ordinary.

const (
	fakeSourceID = "fake:demo"
	fakeCluster  = "shop-prod"
)

// workloadPayload is what the fake source system produces: one JSON document per workload.
type workloadPayload struct {
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	AppName         string    `json:"appName"`
	Replicas        int64     `json:"replicas"`
	ResourceVersion string    `json:"resourceVersion"`
	ValidAt         time.Time `json:"validAt"`
}

type fakeFeeder struct{}

func (fakeFeeder) Describe() feeder.Description {
	return feeder.Description{
		SourceID:         fakeSourceID,
		Kind:             "fake",
		Ordering:         feeder.OrderingPerSourceSequence,
		ReorderingWindow: 60 * time.Second,
		RequiredScopes:   []string{"get,list,watch on apps/v1 deployments"},
		Namespaces:       []string{feeder.NSK8sCluster, feeder.NSK8sDeployment, feeder.NSAppName},
	}
}

func (f fakeFeeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	d := f.Describe()
	var first, last time.Time

	for {
		payload, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		var wl workloadPayload
		if err := json.Unmarshal(payload.Bytes, &wl); err != nil {
			return fmt.Errorf("fake: decode payload: %w", err)
		}
		if err := f.emitWorkload(ctx, em, d, wl); err != nil {
			return err
		}
		// The extent is computed from the whole set, never from "the last one seen", so that
		// delivery order cannot change it (FR-021).
		if first.IsZero() || wl.ValidAt.Before(first) {
			first = wl.ValidAt
		}
		if wl.ValidAt.After(last) {
			last = wl.ValidAt
		}
	}

	if !first.IsZero() {
		if err := em.Checkpoint(ctx, feeder.CheckpointFact{ExtentFrom: first, ExtentTo: last}); err != nil {
			return err
		}
	}
	return em.Flush(ctx)
}

func (f fakeFeeder) emitWorkload(ctx context.Context, em feeder.Emitter, d feeder.Description, wl workloadPayload) error {
	ref := wl.Namespace + "/" + wl.Name
	rv := "@rv" + wl.ResourceVersion

	clusterProps, err := feeder.NewProps().
		Str(feeder.AttrK8sClusterName, fakeCluster).
		Str(feeder.AttrDeploymentEnvironment, "prod").
		Build()
	if err != nil {
		return err
	}
	workloadProps, err := feeder.NewProps().
		Str(feeder.AttrK8sClusterName, fakeCluster).
		Str(feeder.AttrK8sNamespaceName, wl.Namespace).
		Str(feeder.AttrK8sDeploymentName, wl.Name).
		Str(feeder.AttrDeploymentEnvironment, "prod").
		Str(feeder.PropK8sResourceVersion, wl.ResourceVersion).
		Int(feeder.PropK8sReplicas, wl.Replicas).
		Build()
	if err != nil {
		return err
	}
	claimAttrs, err := feeder.NewProps().
		Str(feeder.AttrK8sNamespaceName, wl.Namespace).
		Str(feeder.AttrDeploymentEnvironment, "prod").
		Str(feeder.PropK8sClaimKey, feeder.NSAppName).
		Str(feeder.PropK8sClaimKind, "label").
		Build()
	if err != nil {
		return err
	}

	events := []*graphv1.EventEnvelope{
		// The cluster is asserted by every payload; its id is a pure function of the cluster,
		// so every assertion after the first is a DUPLICATE_NOOP rather than a new version.
		//
		// Its valid start is genuinely unknown: the cluster existed before this feeder looked,
		// and the arrival time of whichever workload happened to be seen first is not evidence
		// of anything. Asserting one would also make the event depend on delivery order, which
		// is precisely what testkit.Shuffle refuses (FR-011, FR-021).
		feeder.UpsertNode(d, feeder.NewID(d.SourceID, "cluster", fakeCluster), feeder.NodeFact{
			Ref:         feeder.Ref(feeder.NSK8sCluster, fakeCluster),
			Type:        graphv1.NodeType_INFRA_RESOURCE,
			DisplayName: fakeCluster,
			Props:       clusterProps,
			Pointers: []*graphv1.Pointer{feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource,
				"v1/clusters/"+fakeCluster, map[string]string{feeder.AttrK8sClusterName: fakeCluster})},
			ValidFromUnknown: true,
		}),
		feeder.UpsertNode(d, feeder.NewID(d.SourceID, "workload", ref+rv), feeder.NodeFact{
			Meta:        feeder.Meta{Seq: seqOf(wl.ResourceVersion)},
			Ref:         feeder.Ref(feeder.NSK8sDeployment, ref),
			Type:        graphv1.NodeType_WORKLOAD,
			DisplayName: wl.Name,
			Props:       workloadProps,
			Pointers: []*graphv1.Pointer{
				feeder.SourceLinkPointer("k8s", feeder.VocabK8sResource,
					"apps/v1/namespaces/"+wl.Namespace+"/deployments/"+wl.Name,
					map[string]string{
						feeder.AttrK8sNamespaceName:  wl.Namespace,
						feeder.AttrK8sDeploymentName: wl.Name,
					}),
				feeder.LogPointer("loki", feeder.OTelSelector(map[string]string{
					feeder.AttrK8sNamespaceName:  wl.Namespace,
					feeder.AttrK8sDeploymentName: wl.Name,
				}), map[string]string{
					feeder.AttrK8sNamespaceName:  wl.Namespace,
					feeder.AttrK8sDeploymentName: wl.Name,
				}),
			},
			ValidAt: wl.ValidAt,
		}),
		feeder.UpsertEdge(d, feeder.NewID(d.SourceID, "edge", "runs-on", ref+rv), feeder.EdgeFact{
			Meta:    feeder.Meta{Seq: seqOf(wl.ResourceVersion)},
			Src:     feeder.Ref(feeder.NSK8sDeployment, ref),
			Dst:     feeder.Ref(feeder.NSK8sCluster, fakeCluster),
			Type:    graphv1.EdgeType_RUNS_ON,
			ValidAt: wl.ValidAt,
		}),
		feeder.IdentityClaim(d, feeder.NewID(d.SourceID, "claim", ref+"@"+feeder.NSAppName), feeder.IdentityFact{
			Subject:    feeder.Ref(feeder.NSK8sDeployment, ref),
			Claim:      feeder.Ref(feeder.NSAppName, wl.AppName),
			Attributes: claimAttrs,
		}),
	}
	for _, ev := range events {
		if _, err := em.Emit(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func seqOf(resourceVersion string) int64 {
	var n int64
	for _, r := range resourceVersion {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// fakePayloads is the recorded corpus: five workloads, one second apart, all inside the
// feeder's declared sixty-second reordering window so the shuffle check permutes them freely.
func fakePayloads(t *testing.T) []feeder.Payload {
	t.Helper()
	start, err := time.Parse(time.RFC3339, "2026-09-01T13:00:00Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	names := []string{"checkout", "payments", "inventory", "storefront", "search"}

	payloads := make([]feeder.Payload, 0, len(names))
	for i, name := range names {
		wl := workloadPayload{
			Namespace:       "shop",
			Name:            name,
			AppName:         name,
			Replicas:        int64(i + 1),
			ResourceVersion: fmt.Sprintf("%d", 1001+i),
			ValidAt:         start.Add(time.Duration(i) * time.Second),
		}
		raw, err := json.Marshal(wl)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		payloads = append(payloads, feeder.Payload{
			Kind:  "deployments",
			At:    wl.ValidAt.Add(2 * time.Second),
			Seq:   seqOf(wl.ResourceVersion),
			Bytes: raw,
		})
	}
	return payloads
}

// recordFixture runs the feeder once with both recorders in place and returns the fixture
// directory. This is exactly the procedure docs/connectors/writing-a-feeder.md describes.
func recordFixture(t *testing.T, f feeder.Feeder) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fake-topology-01")

	observed, err := time.Parse(time.RFC3339, "2026-09-01T13:00:05Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	memory := emit.NewMemoryEmitter(f.Describe(),
		emit.WithStrict(false),
		emit.WithMemoryClock(func() time.Time { return observed }))

	src := record.Wrap(source.NewSliceSource(fakePayloads(t)), dir)
	em := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), src, em); err != nil {
		t.Fatalf("recording run: %v", err)
	}
	if err := src.Err(); err != nil {
		t.Fatalf("payload recorder: %v", err)
	}
	if err := em.Err(); err != nil {
		t.Fatalf("event recorder: %v", err)
	}
	if got := len(memory.Rejected()); got != 0 {
		t.Fatalf("the recording refused %d events", got)
	}

	manifest := record.Manifest{
		Family:         "fake-topology",
		Description:    "five workloads recorded by the SDK's own conformance test",
		Sources:        []record.ManifestSource{record.SourceOf(f.Describe())},
		ExpectRejected: em.Rejections(),
	}
	if err := record.WriteManifest(dir, manifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	return dir
}

// TestConformanceOfAFakeFeeder is the end-to-end proof that the testkit works: a feeder, a
// recording made with pkg/feeder/record, and the three conformance checks over it.
func TestConformanceOfAFakeFeeder(t *testing.T) {
	t.Parallel()
	f := fakeFeeder{}
	dir := recordFixture(t, f)
	testkit.Conformance(t, f, dir)
}

func TestRunReportsWhatItCompared(t *testing.T) {
	t.Parallel()
	f := fakeFeeder{}
	dir := recordFixture(t, f)

	result := testkit.Run(t, f, dir)
	if result.Payloads != 5 {
		t.Errorf("replayed %d payloads, want 5", result.Payloads)
	}
	// Four events per payload, minus the four duplicate cluster assertions, plus one
	// checkpoint.
	if want := 5*4 - 4 + 1; len(result.Events) != want {
		t.Errorf("emitted %d events, want %d", len(result.Events), want)
	}
	if result.Compared != len(result.Events) {
		t.Errorf("compared %d events against events.jsonl, want all %d", result.Compared, len(result.Events))
	}
	if result.Description.SourceID != fakeSourceID {
		t.Errorf("description = %+v", result.Description)
	}
}

// TestConformanceAgainstARealGraph repeats the suite with a real projector beside the
// in-memory emitter, so the events are proved to be acceptable to the graph as well as valid
// against the schema.
func TestConformanceAgainstARealGraph(t *testing.T) {
	f := fakeFeeder{}
	dir := recordFixture(t, f)

	withGraph := testkit.WithEmitter(func(t *testing.T, d feeder.Description) testkit.ResultEmitter {
		t.Helper()
		return emit.NewProjectorEmitter(projector.New(pgtest.Open(t)), d)
	})
	testkit.Conformance(t, f, dir, withGraph, testkit.WithShuffles(2))
}

func TestRunWithoutAnEventsFile(t *testing.T) {
	t.Parallel()
	f := fakeFeeder{}
	dir := filepath.Join(t.TempDir(), "payloads-only-01")

	src := record.Wrap(source.NewSliceSource(fakePayloads(t)), dir)
	for {
		if _, err := src.Next(t.Context()); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	// A fixture with payloads but no recorded stream still gets validity, namespace and flush
	// checks; only the comparison is skipped.
	result := testkit.Run(t, f, dir)
	if result.Compared != 0 {
		t.Errorf("compared %d events against a fixture with no events.jsonl", result.Compared)
	}
	if len(result.Events) == 0 {
		t.Error("nothing was emitted")
	}
}
