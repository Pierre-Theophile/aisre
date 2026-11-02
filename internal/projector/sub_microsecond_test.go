// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Two assertions about one entity, less than a microsecond apart, are applied.
//
// PostgreSQL keeps microseconds; a feeder stamps events with Go's clock, which has nanoseconds.
// The live k8s feeder's first list stamps each object as it reads it, so one Deployment asserted
// twice in a tight loop carries two instants a few hundred nanoseconds apart. The planner cut the
// timeline between them in nanoseconds, the database rounded both bounds to the same microsecond,
// and the insert failed `entity_versions_valid_nonempty`: the whole batch, and every retry of it,
// was refused (staging, 2026-11-02).
func TestAssertionsLessThanAMicrosecondApartApply(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	at := time.Date(2026, 11, 2, 15, 17, 56, 595_123_100, time.UTC)
	node := func(id string, sourceAt time.Time, name string) *graphv1.EventEnvelope {
		return &graphv1.EventEnvelope{
			EventId: id, IdempotencyKey: id, SourceId: "github:acme", SchemaVersion: "1.0.0",
			SourceObservedAt: timestamppb.New(sourceAt),
			Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
				Ref:  &graphv1.Ref{Namespace: "k8s.deployment", Value: "verda/sre-agent/feeder"},
				Type: graphv1.NodeType_WORKLOAD, DisplayName: name, ValidFromUnknown: true,
			}},
		}
	}
	apply(t, p, node("github:acme:deploy@1", at, "feeder"), "2026-11-02T15:18:00Z")
	apply(t, p, node("github:acme:deploy@2", at.Add(400*time.Nanosecond), "feeder v2"), "2026-11-02T15:18:01Z")

	var versions int
	if err := store.Pool().QueryRow(t.Context(), `
		SELECT count(*) FROM graph.entity_versions v
		JOIN graph.identity_claims c ON c.entity_id = v.entity_id
		WHERE c.namespace = 'k8s.deployment' AND upper_inf(v.observed)`).Scan(&versions); err != nil {
		t.Fatalf("read the versions: %v", err)
	}
	if versions == 0 {
		t.Fatal("no current version of the deployment")
	}
}
