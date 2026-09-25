// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Unit tests for the pointer lookup (T068). Pointers are versioned like any other property, so
// the rules worth pinning here are all about time and grouping: which selectors an instant
// returns, which kinds appear, and that either name of a merged entity reaches the same answer.

// pointered upserts a node carrying a set of pointers, valid from an instant. It is how a test
// makes a node's telemetry move without moving anything else about it.
func (b *builder) pointered(name, displayName string, validAt time.Time, pointers ...*graphv1.Pointer) {
	b.t.Helper()
	if validAt.IsZero() {
		validAt = baseValid
	}
	props, err := structpb.NewStruct(map[string]any{"service.name": name})
	if err != nil {
		b.t.Fatalf("props: %v", err)
	}
	b.apply(&graphv1.UpsertNode{
		Ref:         &graphv1.Ref{Namespace: testNS, Value: name},
		Type:        graphv1.NodeType_SERVICE,
		DisplayName: displayName,
		Props:       props,
		Pointers:    pointers,
		ValidAt:     timestamppb.New(validAt),
	}, time.Time{})
}

func pointer(kind graphv1.PointerKind, backend, selector string) *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind:        kind,
		BackendKind: backend,
		Vocabulary:  "otel-semconv/1.30",
		Selector:    selector,
	}
}

func askPointers(t *testing.T, b *builder, name string, validAt time.Time) *graphv1.PointersResponse {
	t.Helper()
	if validAt.IsZero() {
		validAt = queryAt
	}
	resp, err := b.engine().Pointers(context.Background(), &graphv1.PointersRequest{
		Focus: focus(name),
		AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(validAt)},
	})
	if err != nil {
		t.Fatalf("Pointers: %v", err)
	}
	return resp
}

func selectors(list *graphv1.PointerList) []string {
	out := make([]string, 0, len(list.GetPointers()))
	for _, p := range list.GetPointers() {
		out = append(out, p.GetSelector())
	}
	return out
}

// TestPointersGroupByKindAndOmitEmptyOnes is US5 scenario 1: a node with a metric, a log and a
// trace selector answers with three groups, each naming its backend and vocabulary, and no key
// at all for the kinds it has nothing for.
func TestPointersGroupByKindAndOmitEmptyOnes(t *testing.T) {
	b := newBuilder(t)
	b.pointered("payments", "payments", time.Time{},
		pointer(graphv1.PointerKind_TRACE, "tempo", `service.name="payments"`),
		pointer(graphv1.PointerKind_METRIC, "prometheus", `service.name="payments" AND metric.name="http.server.request.duration"`),
		pointer(graphv1.PointerKind_LOG, "loki", `service.name="payments"`),
	)

	resp := askPointers(t, b, "payments", time.Time{})

	kinds := make([]string, 0, len(resp.GetByKind()))
	for kind := range resp.GetByKind() {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, []string{"LOG", "METRIC", "TRACE"}) {
		t.Errorf("by_kind keys = %v, want [LOG METRIC TRACE]: an empty kind is absent, not empty", kinds)
	}
	if resp.GetNode().GetDisplayName() != "payments" {
		t.Errorf("node = %q, want the payments version", resp.GetNode().GetDisplayName())
	}
	for kind, list := range resp.GetByKind() {
		for _, p := range list.GetPointers() {
			if p.GetBackendKind() == "" || p.GetVocabulary() == "" {
				t.Errorf("%s pointer %+v names no backend or vocabulary (FR-008)", kind, p)
			}
		}
	}
}

// TestPointersAreOrderedWithinAKind: two selectors of one kind come back in a documented order
// — backend, then selector — so a golden can be compared byte for byte.
func TestPointersAreOrderedWithinAKind(t *testing.T) {
	b := newBuilder(t)
	b.pointered("payments", "payments", time.Time{},
		pointer(graphv1.PointerKind_METRIC, "prometheus", `metric.name="http.server.request.duration"`),
		pointer(graphv1.PointerKind_METRIC, "datadog", `avg:payments.latency{*}`),
		pointer(graphv1.PointerKind_METRIC, "prometheus", `metric.name="http.server.active_requests"`),
	)

	resp := askPointers(t, b, "payments", time.Time{})
	got := selectors(resp.GetByKind()["METRIC"])
	want := []string{
		`avg:payments.latency{*}`,                    // datadog
		`metric.name="http.server.active_requests"`,  // prometheus, then selector order
		`metric.name="http.server.request.duration"`, //
	}
	if !slices.Equal(got, want) {
		t.Errorf("METRIC selectors = %v, want %v (backend, then selector)", got, want)
	}
}

// TestPointersUseTheNameValidAtTheInstant is US5 scenario 2: payments is renamed at 14:00 and
// its selectors follow. Asked before the rename, the query returns the old ones.
func TestPointersUseTheNameValidAtTheInstant(t *testing.T) {
	renamedAt := time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)

	b := newBuilder(t)
	b.pointered("payments", "payments", baseValid,
		pointer(graphv1.PointerKind_TRACE, "tempo", `service.name="payments"`))
	b.pointered("payments", "payments-v2", renamedAt,
		pointer(graphv1.PointerKind_TRACE, "tempo", `service.name="payments-v2"`))

	before := askPointers(t, b, "payments", time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC))
	if got := selectors(before.GetByKind()["TRACE"]); !slices.Equal(got, []string{`service.name="payments"`}) {
		t.Errorf("at 13:00 selectors = %v, want the pre-rename name", got)
	}
	if got := before.GetNode().GetDisplayName(); got != "payments" {
		t.Errorf("at 13:00 display name = %q, want payments", got)
	}

	after := askPointers(t, b, "payments", queryAt)
	if got := selectors(after.GetByKind()["TRACE"]); !slices.Equal(got, []string{`service.name="payments-v2"`}) {
		t.Errorf("at 14:32 selectors = %v, want the post-rename name", got)
	}
	if got := after.GetNode().GetDisplayName(); got != "payments-v2" {
		t.Errorf("at 14:32 display name = %q, want payments-v2", got)
	}
}

// TestPointersAnswerUnderEitherAliasOfAMergedEntity: a Kubernetes deployment and the telemetry
// service running on it are one entity under two names, and a pointer lookup by either name
// returns the survivor's pointers (FR-039).
func TestPointersAnswerUnderEitherAliasOfAMergedEntity(t *testing.T) {
	b := newBuilder(t)
	b.pointered("checkout", "checkout", time.Time{},
		pointer(graphv1.PointerKind_TRACE, "tempo", `service.name="checkout"`))

	// The same identifier claimed by two sources with matching attributes is what a certain
	// rule merges on (research §10); the deployment side is claimed by the Kubernetes feeder.
	k8s := b.source("k8s:test", "k8s")
	k8s.apply(&graphv1.UpsertNode{
		Ref:         &graphv1.Ref{Namespace: "k8s.deployment", Value: "shop/checkout"},
		Type:        graphv1.NodeType_WORKLOAD,
		DisplayName: "checkout",
		ValidAt:     timestamppb.New(baseValid),
	}, time.Time{})
	k8s.claim(&graphv1.Ref{Namespace: "k8s.deployment", Value: "shop/checkout"},
		&graphv1.Ref{Namespace: testNS, Value: "checkout"})
	b.claim(&graphv1.Ref{Namespace: testNS, Value: "checkout"},
		&graphv1.Ref{Namespace: testNS, Value: "checkout"})

	byDeployment, err := b.engine().Pointers(context.Background(), &graphv1.PointersRequest{
		Focus: &graphv1.Ref{Namespace: "k8s.deployment", Value: "shop/checkout"},
		AsOf:  &graphv1.AsOf{ValidAt: timestamppb.New(queryAt)},
	})
	if err != nil {
		t.Fatalf("Pointers by deployment name: %v", err)
	}
	byService := askPointers(t, b, "checkout", time.Time{})

	if byDeployment.GetNode().GetEntityId() != byService.GetNode().GetEntityId() {
		t.Fatalf("the two names resolved to %s and %s; a merge makes them one entity",
			byDeployment.GetNode().GetEntityId(), byService.GetNode().GetEntityId())
	}
	if !slices.Equal(selectors(byDeployment.GetByKind()["TRACE"]), selectors(byService.GetByKind()["TRACE"])) {
		t.Errorf("the two names returned different pointers: %v vs %v",
			selectors(byDeployment.GetByKind()["TRACE"]), selectors(byService.GetByKind()["TRACE"]))
	}
}

// TestPointersMissingValidInstantIsInvalidArgument: pointers are versioned, so the instant is
// required and never defaulted.
func TestPointersMissingValidInstantIsInvalidArgument(t *testing.T) {
	b := newBuilder(t)
	b.node("a")

	_, err := b.engine().Pointers(context.Background(), &graphv1.PointersRequest{Focus: focus("a")})
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("pointers without an as-of: code %v (%v), want InvalidArgument", got, err)
	}
}

// TestPointersOfANodeWithNone: a node that carries no pointers is an empty answer, not an
// error, and not an empty map either — the key simply is not there.
func TestPointersOfANodeWithNone(t *testing.T) {
	b := newBuilder(t)
	b.node("quiet")

	resp := askPointers(t, b, "quiet", time.Time{})
	if len(resp.GetByKind()) != 0 {
		t.Errorf("by_kind = %v, want empty", resp.GetByKind())
	}
	if resp.GetNode() == nil {
		t.Error("the node itself must still be returned; it exists, it just has no pointers")
	}
}
