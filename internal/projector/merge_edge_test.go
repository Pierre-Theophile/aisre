// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// A merge has to absorb edges, not only node versions (FR-038, FR-039, research §4).
//
// The scenario is the one the shipped topology fixture is built from, reduced to its bones: a
// Kubernetes workload and the OpenTelemetry service running on it are two entities until a
// claim merges them, and the telemetry feeder asserts `payments → payments-db` on both sides of
// that merge. Before the fix the first assertion stayed on the id that was merged away and the
// second landed on the survivor, giving two rows that were valid at the same instant and carried
// different weight classes. The exclusion constraint could not see it — it keys on the raw
// endpoint columns — so only a reader that resolves merges could, which is what
// projector.CheckInvariants is.
//
// Every ordering below has to produce the same valid-time edge set, because that is FR-021
// applied to a merge: whether the merge arrives before or after the assertions it re-points
// cannot change what the graph says was true.

const (
	mergeSourceK8s  = "k8s:merge-test"
	mergeSourceOtel = "otel:merge-test"
)

func newMergeProjector(t *testing.T) (*projector.Projector, *postgres.Store) {
	t.Helper()
	store := openStoreT(t)
	p := projector.New(store)
	for _, id := range []string{mergeSourceK8s, mergeSourceOtel} {
		kind := "k8s"
		if strings.HasPrefix(id, "otel") {
			kind = "otel"
		}
		if err := p.RegisterSource(context.Background(), eventlog.Source{
			SourceID:         id,
			Kind:             kind,
			Ordering:         "none",
			ReorderingWindow: 5 * time.Minute,
			SchemaVersion:    "1.0.0",
		}); err != nil {
			t.Fatalf("register source %s: %v", id, err)
		}
	}
	return p, store
}

func nodeEvent(id, source string, ref graph.Ref, typ graphv1.NodeType, validAt time.Time) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId:        id,
		IdempotencyKey: id,
		SourceId:       source,
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref:         &graphv1.Ref{Namespace: ref.Namespace, Value: ref.Value},
			Type:        typ,
			DisplayName: ref.Value,
			ValidAt:     timestamppb.New(validAt),
		}},
	}
}

func callsEvent(id, source string, src, dst graph.Ref, weight uint32, validAt time.Time) *graphv1.EventEnvelope {
	props, err := structpb.NewStruct(map[string]any{"sre.window.seconds": 300})
	if err != nil {
		panic(err)
	}
	return &graphv1.EventEnvelope{
		EventId:        id,
		IdempotencyKey: id,
		SourceId:       source,
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: &graphv1.UpsertEdge{
			Src:         &graphv1.Ref{Namespace: src.Namespace, Value: src.Value},
			Dst:         &graphv1.Ref{Namespace: dst.Namespace, Value: dst.Value},
			Type:        graphv1.EdgeType_CALLS,
			WeightClass: &weight,
			Props:       props,
			ValidAt:     timestamppb.New(validAt),
		}},
	}
}

func claimEvent(id, source string, subject, claim graph.Ref) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId:        id,
		IdempotencyKey: id,
		SourceId:       source,
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_IdentityClaim{IdentityClaim: &graphv1.IdentityClaim{
			Subject: &graphv1.Ref{Namespace: subject.Namespace, Value: subject.Value},
			Claim:   &graphv1.Ref{Namespace: claim.Namespace, Value: claim.Value},
		}},
	}
}

var (
	k8sPayments  = graph.Ref{Namespace: "k8s.deployment", Value: "shop/payments"}
	otelPayments = graph.Ref{Namespace: "otel.service.name", Value: "payments"}
	paymentsDB   = graph.Ref{Namespace: "server.address", Value: "payments-db.shop.svc.cluster.local"}
)

// mergeScenario is the seven events every ordering is built from.
//
//	k8s-node    the workload, which will survive: it is in the log first
//	otel-node   the service the telemetry feeder sees
//	db-node     the database it calls
//	edge-1300   payments -> payments-db at weight class 3, from 13:00
//	k8s-claim   the workload declares the service name, which merges the two
//	otel-claim  the service re-states its own name, which merges nothing new
//	edge-1420   the same relationship at weight class 4, from 14:20
func mergeScenario(reweight uint32) map[string]*graphv1.EventEnvelope {
	from := mustTime("2026-09-01T13:00:00Z")
	later := mustTime("2026-09-01T14:20:00Z")
	return map[string]*graphv1.EventEnvelope{
		"k8s-node":   nodeEvent("k8s-node", mergeSourceK8s, k8sPayments, graphv1.NodeType_WORKLOAD, from),
		"otel-node":  nodeEvent("otel-node", mergeSourceOtel, otelPayments, graphv1.NodeType_SERVICE, from),
		"db-node":    nodeEvent("db-node", mergeSourceOtel, paymentsDB, graphv1.NodeType_THIRD_PARTY, from),
		"edge-1300":  callsEvent("edge-1300", mergeSourceOtel, otelPayments, paymentsDB, 3, from),
		"k8s-claim":  claimEvent("k8s-claim", mergeSourceK8s, k8sPayments, otelPayments),
		"otel-claim": claimEvent("otel-claim", mergeSourceOtel, otelPayments, otelPayments),
		"edge-1420":  callsEvent("edge-1420", mergeSourceOtel, otelPayments, paymentsDB, reweight, later),
	}
}

// applyOrder delivers the named events in order, one observed second apart, and returns the
// store it built.
func applyOrder(t *testing.T, events map[string]*graphv1.EventEnvelope, order []string) *postgres.Store {
	t.Helper()
	ctx := context.Background()
	p, store := newMergeProjector(t)
	at := mustTime("2026-09-01T15:00:00Z")
	for i, name := range order {
		env, ok := events[name]
		if !ok {
			t.Fatalf("unknown event %q", name)
		}
		result, err := p.Apply(ctx, env, at.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if result.GetStatus() != graphv1.IngestResult_APPLIED {
			t.Fatalf("apply %s: status %s (%s: %s)", name, result.GetStatus(),
				result.GetReasonCode(), result.GetReasonDetail())
		}
	}
	return store
}

// currentEdges renders every edge version that is current in observed time, keyed by its stored
// endpoints so a row left behind under a merged-away id is visible rather than resolved away.
func currentEdges(t *testing.T, store *postgres.Store) []string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `
		SELECT src_id, dst_id, type, valid, weight_class
		FROM graph.edge_versions
		WHERE upper_inf(observed)
		ORDER BY src_id, dst_id, type, lower(valid)`)
	if err != nil {
		t.Fatalf("read current edges: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var (
			src, dst, typ string
			valid         postgres.TimeRange
			weight        *int16
		)
		if err := rows.Scan(&src, &dst, &typ, &valid, &weight); err != nil {
			t.Fatalf("scan current edge: %v", err)
		}
		weightText := "none"
		if weight != nil {
			weightText = fmt.Sprint(*weight)
		}
		out = append(out, fmt.Sprintf("%s -%s-> %s valid=%s weight=%s", src, typ, dst, valid, weightText))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read current edges: %v", err)
	}
	return out
}

// mergeOrderings are the deliveries FR-021 says must agree. The nodes come first in all of them
// so that the survivor is fixed by the log and the comparison is about the edges, which is what
// is under test; the shuffle fixture test covers node ordering.
var mergeOrderings = map[string][]string{
	// (a) the edge is asserted under the pre-merge id, then re-asserted after the merge.
	"edge-then-merge": {"k8s-node", "otel-node", "db-node", "edge-1300", "k8s-claim", "otel-claim", "edge-1420"},
	// (b) the merge happens first and both assertions land on the survivor.
	"merge-then-edge": {"k8s-node", "otel-node", "db-node", "k8s-claim", "otel-claim", "edge-1300", "edge-1420"},
	// The merge falls between the two assertions of the relationship.
	"merge-between": {"k8s-node", "otel-node", "db-node", "edge-1300", "k8s-claim", "edge-1420", "otel-claim"},
	// The later fact arrives first: the older assertion has to fill the gap in front of it, and
	// the merge then re-points both.
	"newest-first": {"k8s-node", "otel-node", "db-node", "edge-1420", "edge-1300", "k8s-claim", "otel-claim"},
}

// TestMergeAbsorbsEdgesOnReweight is case (c): the relationship is re-asserted after the merge
// with a *different* weight class. It must become one version at a time with a valid-time split,
// never two rows that are both true at 14:32.
func TestMergeAbsorbsEdgesOnReweight(t *testing.T) {
	survivor := graph.EntityID(k8sPayments.Namespace, k8sPayments.Value)
	merged := graph.EntityID(otelPayments.Namespace, otelPayments.Value)
	db := graph.EntityID(paymentsDB.Namespace, paymentsDB.Value)
	want := []string{
		fmt.Sprintf("%s -calls-> %s valid=[2026-09-01T13:00:00Z,2026-09-01T14:20:00Z) weight=3", survivor, db),
		fmt.Sprintf("%s -calls-> %s valid=[2026-09-01T14:20:00Z,) weight=4", survivor, db),
	}

	for _, name := range slices.Sorted(maps.Keys(mergeOrderings)) {
		t.Run(name, func(t *testing.T) {
			store := applyOrder(t, mergeScenario(4), mergeOrderings[name])

			got := currentEdges(t, store)
			if !slices.Equal(got, want) {
				t.Errorf("current edges =\n  %s\nwant\n  %s",
					strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			for _, line := range got {
				if strings.Contains(line, merged) {
					t.Errorf("an edge is still stored under the merged-away id %s: %s", merged, line)
				}
			}
			assertInvariants(t, store, "after "+name)
		})
	}
}

// TestMergeAbsorbsEdgesOnRestatement is case (a) with nothing to split: the same relationship,
// same weight, asserted on both sides of the merge. Two statements of one fact are one fact, so
// the timeline must not be cut at 14:20 either.
func TestMergeAbsorbsEdgesOnRestatement(t *testing.T) {
	survivor := graph.EntityID(k8sPayments.Namespace, k8sPayments.Value)
	db := graph.EntityID(paymentsDB.Namespace, paymentsDB.Value)
	want := []string{
		fmt.Sprintf("%s -calls-> %s valid=[2026-09-01T13:00:00Z,) weight=3", survivor, db),
	}

	for _, name := range slices.Sorted(maps.Keys(mergeOrderings)) {
		t.Run(name, func(t *testing.T) {
			store := applyOrder(t, mergeScenario(3), mergeOrderings[name])
			if got := currentEdges(t, store); !slices.Equal(got, want) {
				t.Errorf("current edges =\n  %s\nwant\n  %s",
					strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			assertInvariants(t, store, "after "+name)
		})
	}
}

// TestMergeAbsorbsChangeEdges covers the rows no assertion stands behind: a `changed_by` edge is
// written directly by attach.go, so absorbing it means carrying it over rather than re-deriving
// it. Losing it would mean losing the link between a rollout and what it rolled out.
func TestMergeAbsorbsChangeEdges(t *testing.T) {
	ctx := context.Background()
	rollout := &graphv1.EventEnvelope{
		EventId:        "change-1420",
		IdempotencyKey: "change-1420",
		SourceId:       mergeSourceOtel,
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_ObserveChange{ObserveChange: &graphv1.ObserveChange{
			Ref:     &graphv1.Ref{Namespace: "otel.change", Value: "payments@rev7"},
			Targets: []*graphv1.Ref{{Namespace: otelPayments.Namespace, Value: otelPayments.Value}},
			Change: &graphv1.Change{
				Kind:    graphv1.ChangeKind_ROLLOUT,
				Actor:   "deploy-bot",
				Summary: "rollout payments to revision 7",
			},
			ValidAt: timestamppb.New(mustTime("2026-09-01T14:20:00Z")),
		}},
	}
	events := mergeScenario(4)
	events["change-1420"] = rollout

	store := applyOrder(t, events, []string{
		"k8s-node", "otel-node", "db-node", "edge-1300", "change-1420", "k8s-claim", "otel-claim",
	})

	survivor := graph.EntityID(k8sPayments.Namespace, k8sPayments.Value)
	change := graph.EntityID("otel.change", "payments@rev7")

	var (
		stored int
		src    string
	)
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*), coalesce(min(src_id), '')
		FROM graph.edge_versions
		WHERE dst_id = $1 AND type = 'changed_by' AND upper_inf(observed)`, change).Scan(&stored, &src); err != nil {
		t.Fatalf("read changed_by edge: %v", err)
	}
	if stored != 1 {
		t.Fatalf("changed_by edges current = %d, want exactly 1", stored)
	}
	if src != survivor {
		t.Errorf("changed_by edge is stored under %s, want the survivor %s", src, survivor)
	}
	assertInvariants(t, store, "after absorbing a changed_by edge")
}
