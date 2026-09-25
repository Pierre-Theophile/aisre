// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// A change with an unknown start is dated from the SOURCE's instant, not the ingest instant (004).
//
// ---------------------------------------------------------------------------------------------
// Why this is a correctness test and not a preference
//
// 003 FR-069 says a change whose start nobody stated begins "at the observation". There are two
// candidates for that, and only one of them is replay-stable:
//
//   - the INGEST instant, assigned per event as events are applied. Two events delivered in one arrival
//     window get different ones depending on the order they arrived in, so a change dated from it has a
//     VALID time — a statement about the world — that depends on the order the graph was told things.
//     FR-021 forbids exactly that.
//   - the SOURCE's instant, carried on the envelope as `source_observed_at`. It is a property of the
//     payload, so it does not move under permutation.
//
// It went uncaught until `vercel-promotion-01`, because every other unknown-start change in the corpus
// sits alone in its arrival window and permuting it moves nothing. The Vercel connector emits a change
// and its correlation key together on every promotion, so the case is ordinary rather than contrived.
func TestAnUnknownStartIsDatedFromTheSourceInstant(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	sourceAt := time.Date(2026, 9, 21, 14, 18, 0, 0, time.UTC)
	env := &graphv1.EventEnvelope{
		EventId: "github:acme:change:dpl_1", IdempotencyKey: "github:acme:change:dpl_1",
		SourceId: "github:acme", SchemaVersion: "1.0.0",
		SourceObservedAt: timestamppb.New(sourceAt),
		Body: &graphv1.EventEnvelope_ObserveChange{ObserveChange: &graphv1.ObserveChange{
			Ref:              &graphv1.Ref{Namespace: "vercel.change", Value: "dpl_1"},
			ValidFromUnknown: true,
			Change: &graphv1.Change{
				Kind: graphv1.ChangeKind_ROLLOUT, Summary: "promoted storefront to production",
			},
		}},
	}

	// Ingested a long while after the source saw it, which is what makes the two candidates
	// distinguishable at all.
	if got := apply(t, p, env, "2026-09-21T16:45:00Z"); got == graphv1.IngestResult_REJECTED {
		t.Fatalf("the event was refused: %v", got)
	}

	var lower time.Time
	if err := store.Pool().QueryRow(t.Context(), `
		SELECT lower(v.valid)
		FROM graph.entity_versions v
		JOIN graph.identity_claims c ON c.entity_id = v.entity_id
		WHERE c.namespace = 'vercel.change' AND c.value = 'dpl_1'
		ORDER BY lower(v.observed) DESC LIMIT 1`).Scan(&lower); err != nil {
		t.Fatalf("read the change's valid start: %v", err)
	}
	if !lower.UTC().Equal(sourceAt) {
		t.Errorf("the change is valid from %s, want the source's instant %s. Dating it from the ingest "+
			"instant makes a statement about the world depend on when the graph got round to reading "+
			"it, and on the order events arrived in (FR-021)", lower.UTC(), sourceAt)
	}
}

// And with no source instant stated, the ingest instant is still the fallback — an unknown start is
// never the zero timestamp.
//
// 1970 is not merely wrong but PLAUSIBLY wrong: an ancient change ranks as maximally distant and is
// never excluded as a future announcement, so it reads as a fact.
func TestAnUnknownStartWithNoSourceInstantFallsBackRatherThanToTheEpoch(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	env := &graphv1.EventEnvelope{
		EventId: "github:acme:change:dpl_2", IdempotencyKey: "github:acme:change:dpl_2",
		SourceId: "github:acme", SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_ObserveChange{ObserveChange: &graphv1.ObserveChange{
			Ref:              &graphv1.Ref{Namespace: "vercel.change", Value: "dpl_2"},
			ValidFromUnknown: true,
			Change:           &graphv1.Change{Kind: graphv1.ChangeKind_ROLLOUT, Summary: "promoted"},
		}},
	}
	ingest := "2026-09-21T16:45:00Z"
	if got := apply(t, p, env, ingest); got == graphv1.IngestResult_REJECTED {
		t.Fatalf("the event was refused: %v", got)
	}

	var lower time.Time
	if err := store.Pool().QueryRow(t.Context(), `
		SELECT lower(v.valid)
		FROM graph.entity_versions v
		JOIN graph.identity_claims c ON c.entity_id = v.entity_id
		WHERE c.namespace = 'vercel.change' AND c.value = 'dpl_2'
		ORDER BY lower(v.observed) DESC LIMIT 1`).Scan(&lower); err != nil {
		t.Fatalf("read the change's valid start: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, ingest)
	if !lower.UTC().Equal(want) {
		t.Errorf("the change is valid from %s, want the ingest instant %s", lower.UTC(), want)
	}
	if lower.Year() < 2000 {
		t.Errorf("the change is dated %s — an unknown start read as the epoch ranks as maximally "+
			"distant and is never excluded as a future announcement, so it looks like a fact", lower)
	}
}

// The same rule for nodes and edges (004 T154).
//
// It was carried only to changes at first. T093 found the rest: a Vercel project listed without a
// `createdAt` was dated from the ingest instant, and all three Vercel fixtures failed the shuffle step.
// The corpus had passed until then only because every other unknown-start node sat alone in its window.
//
// The apply path is what this pins; a second source re-folding the node is included so the stored start
// is checked after a fold too. The log read-back (`nodeAssertions`, `edgeAssertions`) is changed to agree
// with it, but this scenario does not distinguish the two read-backs: a segment's start comes from the
// stored row, and the read-back instant only orders assertions. Kept consistent so no later ordering
// decision is made on the ingest instant.
func TestAnUnknownStartNodeOrEdgeIsDatedFromTheSourceInstant(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	sourceAt := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	node := func(id, source string, validAt *timestamppb.Timestamp) *graphv1.EventEnvelope {
		return &graphv1.EventEnvelope{
			EventId: id, IdempotencyKey: id, SourceId: source, SchemaVersion: "1.0.0",
			SourceObservedAt: timestamppb.New(sourceAt),
			Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
				Ref:  &graphv1.Ref{Namespace: "vercel.project", Value: "prj_web"},
				Type: graphv1.NodeType_SERVICE, DisplayName: "web",
				ValidAt: validAt, ValidFromUnknown: validAt == nil,
			}},
		}
	}
	edge := &graphv1.EventEnvelope{
		EventId: "gcp:acme:edge", IdempotencyKey: "gcp:acme:edge", SourceId: "gcp:acme", SchemaVersion: "1.0.0",
		SourceObservedAt: timestamppb.New(sourceAt),
		Body: &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: &graphv1.UpsertEdge{
			Src:              &graphv1.Ref{Namespace: "vercel.project", Value: "prj_web"},
			Dst:              &graphv1.Ref{Namespace: "otel.service.name", Value: "api"},
			Type:             graphv1.EdgeType_CALLS,
			ValidFromUnknown: true,
		}},
	}

	// Applied well after the source saw them, which is what makes the two candidates distinguishable.
	apply(t, p, node("github:acme:project", "github:acme", nil), "2026-09-21T16:45:00Z")
	apply(t, p, edge, "2026-09-21T16:46:00Z")
	// A second source restating the node from a later stated instant, so the entity is re-folded.
	apply(t, p, node("gcp:acme:project", "gcp:acme", timestamppb.New(sourceAt.Add(time.Hour))),
		"2026-09-21T16:47:00Z")

	var nodeStart, edgeStart time.Time
	if err := store.Pool().QueryRow(t.Context(), `
		SELECT min(lower(v.valid))
		FROM graph.entity_versions v
		JOIN graph.identity_claims c ON c.entity_id = v.entity_id
		WHERE c.namespace = 'vercel.project' AND c.value = 'prj_web' AND upper_inf(v.observed)`).
		Scan(&nodeStart); err != nil {
		t.Fatalf("read the node's valid start: %v", err)
	}
	if !nodeStart.UTC().Equal(sourceAt) {
		t.Errorf("the node is valid from %s, want the source's instant %s, still after a second source "+
			"re-folded it", nodeStart.UTC(), sourceAt)
	}
	if err := store.Pool().QueryRow(t.Context(), `
		SELECT min(lower(valid)) FROM graph.edge_versions
		WHERE type = 'calls' AND upper_inf(observed)`).Scan(&edgeStart); err != nil {
		t.Fatalf("read the edge's valid start: %v", err)
	}
	if !edgeStart.UTC().Equal(sourceAt) {
		t.Errorf("the edge is valid from %s, want the source's instant %s", edgeStart.UTC(), sourceAt)
	}
}
