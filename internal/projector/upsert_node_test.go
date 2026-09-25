// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"fmt"
	"math/rand/v2"
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

// Order independence against the real database (FR-021, research §5).
//
// The exhaustive property test lives in segments_test.go, where the planner can be driven
// thousands of times in milliseconds. This one takes the same property through the whole
// write path — resolution, materialization, the exclusion constraint, close_observed — for a
// handful of generated scenarios, because a planner that is order-independent on paper is of no
// use if the rows it produces are not.
//
// Observed time is allowed to differ between orderings: it records when each delivery actually
// arrived. The comparison is therefore of valid-time state as known now, which is what
// `upper_inf(observed)` selects.

const propSources = 2

func propSourceID(i int) string { return fmt.Sprintf("src:%d", i) }

// upsertSpec is one generated assertion.
type upsertSpec struct {
	source  int
	entity  string
	validAt time.Time
	props   map[string]string
	name    string
}

func (u upsertSpec) event(id string) *graphv1.EventEnvelope {
	fields := map[string]any{}
	for k, v := range u.props {
		fields[k] = v
	}
	props, err := structpb.NewStruct(fields)
	if err != nil {
		panic(err)
	}
	return &graphv1.EventEnvelope{
		EventId:        id,
		IdempotencyKey: id,
		SourceId:       propSourceID(u.source),
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref:         &graphv1.Ref{Namespace: "otel.service.name", Value: u.entity},
			Type:        graphv1.NodeType_SERVICE,
			DisplayName: u.name,
			ValidAt:     timestamppb.New(u.validAt),
			Props:       props,
		}},
	}
}

func generateSpecs(rng *rand.Rand) []upsertSpec {
	base := mustTime("2026-09-01T13:00:00Z")
	entities := []string{"checkout", "payments"}
	versions := []string{"1.0", "2.0"}
	tiers := []string{"gold", "silver"}
	names := []string{"a", "b"}

	count := 3 + rng.IntN(4)
	seen := map[string]bool{}
	var specs []upsertSpec
	for range count {
		spec := upsertSpec{
			source:  rng.IntN(propSources),
			entity:  entities[rng.IntN(len(entities))],
			validAt: base.Add(time.Duration(rng.IntN(5)) * time.Hour),
			name:    names[rng.IntN(len(names))],
			props: map[string]string{
				"service.version": versions[rng.IntN(len(versions))],
				"sre.tier":        tiers[rng.IntN(len(tiers))],
			},
		}
		// Two assertions from one source at one valid instant are a correction, and the later
		// delivery is meant to win — that is observed-time behaviour, not valid-time, so it is
		// outside this property.
		key := fmt.Sprint(spec.source, spec.entity, spec.validAt)
		if seen[key] {
			continue
		}
		seen[key] = true
		specs = append(specs, spec)
	}
	return specs
}

func TestUpsertNodeOrderIndependence(t *testing.T) {
	for scenario := range uint64(4) {
		t.Run(fmt.Sprintf("scenario-%d", scenario), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(scenario, 0x0D1E))
			specs := generateSpecs(rng)
			if len(specs) < 2 {
				t.Skip("need at least two distinct assertions")
			}

			inOrder := sequence(len(specs))
			var want string
			t.Run("in-order", func(t *testing.T) {
				want = applySpecs(t, specs, inOrder)
			})

			for permutation := range 3 {
				order := slices.Clone(inOrder)
				rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
				t.Run(fmt.Sprintf("permutation-%d", permutation), func(t *testing.T) {
					if got := applySpecs(t, specs, order); got != want {
						t.Errorf("valid-time state depends on arrival order\nin order %v:\n%s\norder %v:\n%s",
							inOrder, want, order, got)
					}
				})
			}
		})
	}
}

func sequence(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// applySpecs applies the generated assertions in the given order, each with the next observed
// instant, and returns the resulting valid-time state.
func applySpecs(t *testing.T, specs []upsertSpec, order []int) string {
	t.Helper()
	store := openStore(t)
	p := projector.New(store)
	ctx := context.Background()

	observedAt := mustTime("2026-09-02T00:00:00Z")
	for step, index := range order {
		env := specs[index].event(fmt.Sprintf("src:%d:e%d", specs[index].source, index))
		result, err := p.Apply(ctx, env, observedAt.Add(time.Duration(step)*time.Second))
		if err != nil {
			t.Fatalf("apply %s: %v", env.GetEventId(), err)
		}
		if result.GetStatus() != graphv1.IngestResult_APPLIED {
			t.Fatalf("apply %s: status %s (%s: %s)", env.GetEventId(), result.GetStatus(),
				result.GetReasonCode(), result.GetReasonDetail())
		}
	}
	return validTimeState(t, store)
}

// openStore returns a migrated database with the generated sources registered.
func openStore(t *testing.T) *postgres.Store {
	t.Helper()
	store := openStoreT(t)
	p := projector.New(store)
	for i := range propSources {
		if err := p.RegisterSource(context.Background(), eventlog.Source{
			SourceID: propSourceID(i), Kind: "test", Ordering: "none", SchemaVersion: "1.0.0",
		}); err != nil {
			t.Fatalf("register source: %v", err)
		}
	}
	return store
}

// validTimeState renders every entity's current valid-time partition: for each entity, the list
// of (valid range, asserted property values). Provenance is left out on purpose — two orderings
// may credit the same value to different, equally true, events.
func validTimeState(t *testing.T, store *postgres.Store) string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `
		SELECT v.entity_id, v.valid, v.display_name, v.props, v.conflicts
		FROM graph.entity_versions v
		WHERE upper_inf(v.observed)
		ORDER BY v.entity_id, lower(v.valid)`)
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	defer rows.Close()

	var out strings.Builder
	for rows.Next() {
		var (
			entityID    string
			valid       postgres.TimeRange
			displayName string
			props       []byte
			conflicts   []string
		)
		if err := rows.Scan(&entityID, &valid, &displayName, &props, &conflicts); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		fmt.Fprintf(&out, "%s %s name=%q props=%s conflicts=%v\n",
			entityID, valid, displayName, propValues(t, props), conflicts)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read versions: %v", err)
	}
	return out.String()
}

// TestUpsertNodeConflictingSources pins the edge case "conflicting sources": two feeders
// asserting different values for one key at one valid time produce one version holding both
// records, with the key named in `conflicts`. The projector never picks a winner.
func TestUpsertNodeConflictingSources(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	validAt := mustTime("2026-09-01T13:00:00Z")
	observedAt := mustTime("2026-09-01T13:05:00Z")
	for i, owner := range []string{"team-a", "team-b"} {
		spec := upsertSpec{
			source: i, entity: "checkout", validAt: validAt, name: "checkout",
			props: map[string]string{"sre.owner.kind": owner},
		}
		env := spec.event(fmt.Sprintf("src:%d:owner", i))
		result, err := p.Apply(ctx, env, observedAt.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if result.GetStatus() != graphv1.IngestResult_APPLIED {
			t.Fatalf("status %s (%s)", result.GetStatus(), result.GetReasonDetail())
		}
	}

	var (
		props     []byte
		conflicts []string
	)
	if err := store.Pool().QueryRow(ctx, `
		SELECT props, conflicts FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`,
		graph.EntityID("otel.service.name", "checkout")).Scan(&props, &conflicts); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0] != "sre.owner.kind" {
		t.Errorf("conflicts = %v, want [sre.owner.kind]", conflicts)
	}
	text := string(props)
	for _, want := range []string{"team-a", "team-b", `"src:0"`, `"src:1"`} {
		if !strings.Contains(text, want) {
			t.Errorf("props %s do not keep %s; both claims must survive with provenance", text, want)
		}
	}
}

// TestUpsertNodeNoop checks that re-asserting the same facts at a later valid instant does not
// cut the timeline, and that remembering it costs a bounded number of rows.
//
// A telemetry feeder re-emits an unchanged node every aggregation window — 288 times a day, for
// ever. Two things must hold across those windows, and they pull in opposite directions:
//
//   - the valid timeline stays *one* version, [first assertion, ∞). Cutting it into one segment
//     per window would answer "what changed?" with the feeder's schedule (FR-021).
//   - the graph remembers that the fact was said again, and from when, because a retraction
//     arriving later has to split the segment at that instant rather than truncate the whole of
//     it (segments.go, rememberRestatement).
//
// The reconciliation is that only the *first* restatement is remembered. So four windows write
// two rows — the original and the one that records the first restatement — and the fifth,
// hundredth and ten-thousandth write nothing at all. The second half of this test is the part
// that matters: the count does not grow with the number of windows.
func TestUpsertNodeNoop(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	base := mustTime("2026-09-01T13:00:00Z")
	entityID := graph.EntityID("otel.service.name", "checkout")
	emit := func(window int) {
		t.Helper()
		spec := upsertSpec{
			source: 0, entity: "checkout", name: "checkout",
			validAt: base.Add(time.Duration(window) * 5 * time.Minute),
			props:   map[string]string{"service.version": "1.0"},
		}
		env := spec.event(fmt.Sprintf("src:0:w%d", window))
		if _, err := p.Apply(ctx, env, base.Add(time.Duration(window)*5*time.Minute+time.Second)); err != nil {
			t.Fatalf("apply window %d: %v", window, err)
		}
	}
	count := func() (current, total int) {
		t.Helper()
		if err := store.Pool().QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE upper_inf(observed)), count(*)
			FROM graph.entity_versions WHERE entity_id = $1`, entityID).Scan(&current, &total); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		return current, total
	}

	for window := range 4 {
		emit(window)
	}
	current, total := count()
	if current != 1 {
		t.Errorf("current versions = %d, want 1: nothing changed across the four windows", current)
	}
	if total != 2 {
		t.Errorf("total versions = %d, want 2: the original plus the one that records the first "+
			"restatement; every window after it writes nothing", total)
	}

	for window := 4; window < 20; window++ {
		emit(window)
	}
	current, grown := count()
	if current != 1 {
		t.Errorf("current versions = %d after twenty windows, want 1", current)
	}
	if grown != total {
		t.Errorf("total versions grew from %d to %d over sixteen more unchanged windows; the cost "+
			"of remembering a restatement must not scale with the feeder's schedule", total, grown)
	}
}

// TestUpsertNodeLateArrivingFactSplits is the out-of-order case in miniature: a fact learned
// later about an earlier instant splits the version it lands in rather than overwriting it
// (edge case "out-of-order arrival", FR-012).
func TestUpsertNodeLateArrivingFactSplits(t *testing.T) {
	ctx := context.Background()
	store := openStore(t)
	p := projector.New(store)

	thirteen := mustTime("2026-09-01T13:00:00Z")
	fourteen := mustTime("2026-09-01T14:00:00Z")

	first := upsertSpec{source: 0, entity: "checkout", validAt: thirteen, name: "checkout",
		props: map[string]string{"service.version": "1.0"}}
	if _, err := p.Apply(ctx, first.event("src:0:v1"), mustTime("2026-09-01T13:01:00Z")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	second := upsertSpec{source: 0, entity: "checkout", validAt: fourteen, name: "checkout",
		props: map[string]string{"service.version": "2.0"}}
	if _, err := p.Apply(ctx, second.event("src:0:v2"), mustTime("2026-09-01T15:00:00Z")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	entityID := graph.EntityID("otel.service.name", "checkout")
	rows, err := store.Pool().Query(ctx, `
		SELECT valid, props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed) ORDER BY lower(valid)`, entityID)
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var (
			valid postgres.TimeRange
			props []byte
		)
		if err := rows.Scan(&valid, &props); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, valid.String()+" "+propValues(t, props))
	}
	want := []string{
		"[2026-09-01T13:00:00Z,2026-09-01T14:00:00Z) {service.version=src:0:\"1.0\"}",
		"[2026-09-01T14:00:00Z,) {service.version=src:0:\"2.0\"}",
	}
	if !slices.Equal(got, want) {
		t.Errorf("valid-time partition =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	// The superseded version is kept, closed in observed time (FR-012, FR-014).
	if total := countRows(t, store,
		`SELECT count(*) FROM graph.entity_versions WHERE closed_by_event_id IS NOT NULL`); total != 1 {
		t.Errorf("closed versions = %d, want 1: the correction closes the old row, never deletes it", total)
	}
}
