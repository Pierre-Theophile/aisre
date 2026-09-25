// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Structural invariants of the projection (constitution II, FR-021, FR-038, FR-039).
//
// The version tables enforce a lot on their own: the exclusion constraints in 0002_graph.sql
// make two current versions of one entity, or of one stored `(src, dst, type)` triple, unable
// to overlap in valid time. What they cannot see is *identity*. They key on the raw id columns,
// and a merge changes which ids mean the same thing without rewriting a single row — so two
// rows that the database is sure are about different things can be two statements about one
// relationship, at one instant, in two different weight classes.
//
// That is exactly the shape the edge-absorb bug had: `payments → payments-db` asserted under
// the pre-merge OpenTelemetry id, re-asserted after the merge under the survivor, two rows,
// both open, both valid at 14:32, the constraint silent because the raw endpoints differ. The
// query layer canonicalises endpoints as it reads, so it saw the relationship twice; the diff
// hid it behind a tie-break and the impact walk would have taken whichever weight it met first.
//
// CheckInvariants is the assertion the constraints cannot make. It is cheap — the projection is
// small enough to read whole in a test — and it is the natural thing to run after replaying a
// fixture, which is where the bug would have been caught.

// Violation is one broken invariant.
type Violation struct {
	// Invariant names the rule, as one of the Invariant* constants.
	Invariant string
	// Detail says what broke it, naming the rows involved.
	Detail string
}

func (v Violation) String() string { return v.Invariant + ": " + v.Detail }

// The invariants CheckInvariants verifies, by name.
const (
	// InvariantEdgeIdentity is the one the exclusion constraint cannot make: at most one
	// current edge version per *canonical* (src, dst, type) per valid instant.
	InvariantEdgeIdentity = "one current edge version per canonical relationship"
	// InvariantNodeIdentity is its node counterpart: a merged-away entity's versions are
	// closed in observed time, so no two current versions resolve to one entity at one instant.
	InvariantNodeIdentity = "one current node version per canonical entity"
	// InvariantCanonicalEndpoints is what makes the other two reachable at all: a current edge
	// version never names an entity that has been merged away.
	InvariantCanonicalEndpoints = "current edge endpoints are canonical"
)

// CheckInvariants reads the whole projection and returns an error naming every invariant it
// breaks, or nil when it holds.
//
// It reads only what is current in observed time. History is meant to name merged-away ids and
// to hold rows that a later merge superseded: that is the point of an append-only projection,
// and a query rewinding observed time is supposed to see it. The invariants are statements
// about what the graph believes *now*.
func CheckInvariants(ctx context.Context, store *postgres.Store) error {
	violations, err := Invariants(ctx, store)
	if err != nil {
		return err
	}
	if len(violations) == 0 {
		return nil
	}
	lines := make([]string, 0, len(violations))
	for _, v := range violations {
		lines = append(lines, "  "+v.String())
	}
	return fmt.Errorf("projector: %d broken invariant(s):\n%s", len(violations), strings.Join(lines, "\n"))
}

// Invariants is CheckInvariants with the violations returned rather than rendered, for a caller
// that wants to report them its own way.
func Invariants(ctx context.Context, store *postgres.Store) ([]Violation, error) {
	canonical, err := canonicalIDs(ctx, store)
	if err != nil {
		return nil, err
	}

	var violations []Violation
	edges, err := checkEdgeInvariants(ctx, store, canonical)
	if err != nil {
		return nil, err
	}
	violations = append(violations, edges...)

	nodes, err := checkNodeInvariants(ctx, store, canonical)
	if err != nil {
		return nil, err
	}
	return append(violations, nodes...), nil
}

// canonicalIDs resolves every entity id through its merge chain, once, so the checks can group
// by identity rather than by stored id.
func canonicalIDs(ctx context.Context, store *postgres.Store) (func(string) string, error) {
	rows, err := store.Pool().Query(ctx,
		`SELECT entity_id, merged_into FROM graph.entities WHERE merged_into IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("projector: read merge redirects: %w", err)
	}
	defer rows.Close()

	target := map[string]string{}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, fmt.Errorf("projector: read merge redirects: %w", err)
		}
		target[from] = to
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read merge redirects: %w", err)
	}

	return func(id string) string {
		for hop := 0; hop < maxMergeChain; hop++ {
			next, ok := target[id]
			if !ok {
				return id
			}
			id = next
		}
		return id
	}, nil
}

// currentVersion is one open-in-observed-time row, reduced to what the invariants are about.
type currentVersion struct {
	versionID string
	key       string
	valid     postgres.TimeRange
}

func checkEdgeInvariants(ctx context.Context, store *postgres.Store, canonical func(string) string) ([]Violation, error) {
	rows, err := store.Pool().Query(ctx, `
		SELECT version_id, src_id, dst_id, type, valid
		FROM graph.edge_versions
		WHERE upper_inf(observed)
		ORDER BY src_id, dst_id, type, lower(valid), version_id`)
	if err != nil {
		return nil, fmt.Errorf("projector: read current edge versions: %w", err)
	}
	defer rows.Close()

	var (
		violations []Violation
		current    []currentVersion
	)
	for rows.Next() {
		var (
			versionID, src, dst, typ string
			valid                    postgres.TimeRange
		)
		if err := rows.Scan(&versionID, &src, &dst, &typ, &valid); err != nil {
			return nil, fmt.Errorf("projector: read current edge versions: %w", err)
		}
		canonicalSrc, canonicalDst := canonical(src), canonical(dst)
		if canonicalSrc != src || canonicalDst != dst {
			violations = append(violations, Violation{
				Invariant: InvariantCanonicalEndpoints,
				Detail: fmt.Sprintf("edge version %s stores %s-%s->%s, which resolves to %s-%s->%s: the merge did not absorb it",
					versionID, src, typ, dst, canonicalSrc, typ, canonicalDst),
			})
		}
		current = append(current, currentVersion{
			versionID: versionID,
			key:       canonicalSrc + "|" + canonicalDst + "|" + typ,
			valid:     valid,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read current edge versions: %w", err)
	}
	return append(violations, overlaps(current, InvariantEdgeIdentity, "relationship")...), nil
}

func checkNodeInvariants(ctx context.Context, store *postgres.Store, canonical func(string) string) ([]Violation, error) {
	rows, err := store.Pool().Query(ctx, `
		SELECT version_id, entity_id, valid
		FROM graph.entity_versions
		WHERE upper_inf(observed)
		ORDER BY entity_id, lower(valid), version_id`)
	if err != nil {
		return nil, fmt.Errorf("projector: read current entity versions: %w", err)
	}
	defer rows.Close()

	var current []currentVersion
	for rows.Next() {
		var (
			versionID, entityID string
			valid               postgres.TimeRange
		)
		if err := rows.Scan(&versionID, &entityID, &valid); err != nil {
			return nil, fmt.Errorf("projector: read current entity versions: %w", err)
		}
		current = append(current, currentVersion{
			versionID: versionID,
			key:       canonical(entityID),
			valid:     valid,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read current entity versions: %w", err)
	}
	return overlaps(current, InvariantNodeIdentity, "entity"), nil
}

// overlaps reports every pair of current versions that share a canonical key and a valid
// instant. Versions are grouped, sorted by lower bound and compared with their successors,
// which is O(n log n) over rows that in practice number in the tens per key.
func overlaps(versions []currentVersion, invariant, subject string) []Violation {
	byKey := map[string][]currentVersion{}
	var keys []string
	for _, v := range versions {
		if _, seen := byKey[v.key]; !seen {
			keys = append(keys, v.key)
		}
		byKey[v.key] = append(byKey[v.key], v)
	}
	slices.Sort(keys)

	var violations []Violation
	for _, key := range keys {
		group := byKey[key]
		slices.SortFunc(group, func(a, b currentVersion) int {
			if c := a.valid.Start.Compare(b.valid.Start); c != 0 {
				return c
			}
			return strings.Compare(a.versionID, b.versionID)
		})
		for i := 1; i < len(group); i++ {
			previous, next := group[i-1], group[i]
			if !intervalsOverlap(previous.valid, next.valid) {
				continue
			}
			violations = append(violations, Violation{
				Invariant: invariant,
				Detail: fmt.Sprintf("%s %s has two current versions valid at once: %s over %s and %s over %s",
					subject, key, previous.versionID, previous.valid, next.versionID, next.valid),
			})
		}
	}
	return violations
}

// intervalsOverlap is the half-open `&&` PostgreSQL applies to two tstzranges.
func intervalsOverlap(a, b postgres.TimeRange) bool {
	return startsBefore(a.Start, b) && startsBefore(b.Start, a)
}

// startsBefore reports whether t is inside r's lower half: at or after its start is implied by
// the caller, so this is "before r's exclusive upper bound".
func startsBefore(t time.Time, r postgres.TimeRange) bool {
	return r.EndUnbounded || t.Before(r.End)
}
