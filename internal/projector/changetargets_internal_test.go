// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// ChangeTargets against a real graph (004 T138).
//
// It is the graph question C8 rests on, and it has to follow merge redirects: two changes whose targets
// resolution has already merged must report the SAME target, or the intersection C8 keys on is empty and
// the rule is unfireable. That is not a property a fake store can have, which is why this test exists
// beside the rule's unit tests rather than instead of them.
//
// It is an internal test because claimStore is unexported — the point of it being unexported is that a
// rule reaches the graph through one narrow door, and widening that door to test it would remove the
// thing worth testing.
func TestChangeTargetsFollowsMergeRedirects(t *testing.T) {
	t.Parallel()
	store := pgtest.Open(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)

	// Two targets and two changes, each change applied to one target; then the two targets are merged,
	// which is what C4, C5 or C1 would do to them in a real run.
	err := store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range []string{"target-a", "target-b", "change-a", "change-b"} {
			typ := graph.NodeTypeService
			if id == "change-a" || id == "change-b" {
				typ = graph.NodeTypeChange
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO graph.entities (entity_id, type, facets, created_by_event_id)
				VALUES ($1, $2, '{}', 'seed')`,
				id, string(typ)); err != nil {
				return err
			}
		}
		// The edge reads "target changed_by change" (observe_change.go's linkChange).
		for _, pair := range [][2]string{{"target-a", "change-a"}, {"target-b", "change-b"}} {
			if _, err := tx.Exec(ctx, `
				INSERT INTO graph.edge_versions
					(version_id, src_id, dst_id, type, valid, observed, produced_by_event_ids)
				VALUES ($1, $2, $3, $4, tstzrange($5, NULL), tstzrange($5, NULL), '{}')`,
				pair[0]+"-"+pair[1], pair[0], pair[1], string(graph.EdgeTypeChangedBy), at); err != nil {
				return err
			}
		}
		// target-b is absorbed into target-a.
		_, err := tx.Exec(ctx,
			`UPDATE graph.entities SET merged_into = 'target-a' WHERE entity_id = 'target-b'`)
		return err
	})
	if err != nil {
		t.Fatalf("seed the graph: %v", err)
	}

	err = store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s := claimStore{tx: tx}

		a, err := s.ChangeTargets(ctx, "change-a")
		if err != nil {
			return err
		}
		b, err := s.ChangeTargets(ctx, "change-b")
		if err != nil {
			return err
		}
		if len(a) != 1 || a[0] != "target-a" {
			t.Errorf("ChangeTargets(change-a) = %v, want [target-a]", a)
		}
		// The redirect is the whole point: change-b's edge names target-b, which no longer exists as a
		// distinct entity, and a rule comparing the raw ids would find no shared target.
		if len(b) != 1 || b[0] != "target-a" {
			t.Errorf("ChangeTargets(change-b) = %v, want [target-a] through the merge redirect; without "+
				"following it, C8 finds an empty intersection for two changes on one service", b)
		}

		// A change nobody targeted reports nothing rather than erroring, and so does the empty id.
		for _, id := range []string{"change-never-seen", ""} {
			got, err := s.ChangeTargets(ctx, id)
			if err != nil {
				return err
			}
			if len(got) != 0 {
				t.Errorf("ChangeTargets(%q) = %v, want nothing", id, got)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the targets: %v", err)
	}
}
