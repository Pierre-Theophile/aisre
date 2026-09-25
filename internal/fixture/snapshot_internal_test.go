// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The shuffle step compares snapshots, so a snapshot that rendered nothing would make it pass
// vacuously. This test is the guard: it asserts that the structural snapshot of a loaded fixture
// actually describes the graph, and that it names entities by alias set rather than by canonical
// id (research §4) — which is the property that makes shuffled deliveries comparable at all.
func TestSnapshotDescribesTheGraph(t *testing.T) {
	ctx := context.Background()
	store := pgtest.Open(t)

	if _, err := Load(ctx, projector.New(store), filepath.Join("../../fixtures", "baseline-topology-01"), LoadOptions{}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	rendered, err := snapshot(ctx, store)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if strings.Count(rendered, "\nNODE ") < 10 {
		t.Fatalf("snapshot has %d nodes, want the whole baseline topology:\n%s",
			strings.Count(rendered, "\nNODE "), rendered)
	}
	if strings.Count(rendered, "\nEDGE ") < 10 {
		t.Errorf("snapshot has %d edges, want the whole baseline topology", strings.Count(rendered, "\nEDGE "))
	}

	// checkout is claimed under both namespaces and is merged by certain rule C2, so its
	// alias-set name carries both — and no canonical id.
	if !strings.Contains(rendered, "k8s.deployment=shop/checkout") || !strings.Contains(rendered, "otel.service.name=checkout") {
		t.Errorf("snapshot does not name checkout by its alias set:\n%s", rendered)
	}
	if strings.Contains(rendered, "anonymous:") {
		t.Errorf("snapshot fell back to canonical ids, which a shuffle is free to change:\n%s", rendered)
	}
}
