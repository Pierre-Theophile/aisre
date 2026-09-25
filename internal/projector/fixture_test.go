// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// Replaying the shipped fixtures (FR-047, FR-048, SC-003, SC-004).
//
// These are the tests the constitution asks for by name: replay every fixture from empty,
// double-deliver every event and verify nothing changes, and shuffle each source's events
// within its declared reordering window and verify the valid-time state is the same. Golden
// query outputs are not compared here — the query layer does not exist yet — so the assertions
// are made directly against the projection.

const fixturesDir = "../../fixtures"

type manifest struct {
	ID             string `yaml:"id"`
	Events         string `yaml:"events"`
	RejectedEvents string `yaml:"rejected_events"`
	SchemaVersion  string `yaml:"schema_version"`
	Sources        []struct {
		SourceID         string `yaml:"source_id"`
		Kind             string `yaml:"kind"`
		Ordering         string `yaml:"ordering"`
		ReorderingWindow string `yaml:"reordering_window"`
	} `yaml:"sources"`
}

// fixtureEvent is one events.jsonl line: the envelope plus the two fields the log added.
type fixtureEvent struct {
	env         *graphv1.EventEnvelope
	observedAt  time.Time
	appendedSeq int64
}

func loadManifest(t *testing.T, dir string) manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	return m
}

// loadEvents reads an events.jsonl file.
//
// `observedAt` and `appendedSeq` are added by the log and are not fields of EventEnvelope, so
// they are lifted off before unmarshalling. Unmarshalling is deliberately strict
// (DiscardUnknown: false): a fixture with a field the schema does not have is a broken fixture,
// and silently ignoring it would let the event stream drift from the published contract
// (contracts/fixture-format.md).
func loadEvents(t *testing.T, path string) []fixtureEvent {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var events []fixtureEvent
	for i, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("%s line %d: %v", path, i+1, err)
		}
		event := fixtureEvent{appendedSeq: int64(i + 1)}
		if raw, ok := fields["observedAt"]; ok {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				t.Fatalf("%s line %d: observedAt: %v", path, i+1, err)
			}
			parsed, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				t.Fatalf("%s line %d: observedAt: %v", path, i+1, err)
			}
			event.observedAt = parsed.UTC()
		}
		delete(fields, "observedAt")
		delete(fields, "appendedSeq")

		body, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("%s line %d: %v", path, i+1, err)
		}
		env := &graphv1.EventEnvelope{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(body, env); err != nil {
			t.Fatalf("%s line %d does not match the published event schema: %v", path, i+1, err)
		}
		event.env = env
		events = append(events, event)
	}
	return events
}

// openStoreT is the one place tests get a database: a fresh, migrated one per test, dropped at
// cleanup. Nothing is ever truncated (constitution III).
func openStoreT(t *testing.T) *postgres.Store {
	t.Helper()
	return pgtest.Open(t)
}

func newProjector(t *testing.T, m manifest) (*projector.Projector, *postgres.Store) {
	t.Helper()
	store := openStoreT(t)
	p := projector.New(store)
	for _, src := range m.Sources {
		window, err := time.ParseDuration(src.ReorderingWindow)
		if err != nil && src.ReorderingWindow != "" {
			t.Fatalf("source %s: reordering_window %q: %v", src.SourceID, src.ReorderingWindow, err)
		}
		if err := p.RegisterSource(context.Background(), eventlog.Source{
			SourceID:         src.SourceID,
			Kind:             src.Kind,
			Ordering:         src.Ordering,
			ReorderingWindow: window,
			SchemaVersion:    m.SchemaVersion,
		}); err != nil {
			t.Fatalf("register source %s: %v", src.SourceID, err)
		}
	}
	return p, store
}

func applyAll(t *testing.T, p *projector.Projector, events []fixtureEvent) []*graphv1.IngestResult {
	t.Helper()
	ctx := context.Background()
	results := make([]*graphv1.IngestResult, 0, len(events))
	for _, event := range events {
		result, err := p.Apply(ctx, event.env, event.observedAt)
		if err != nil {
			t.Fatalf("apply %s: %v", event.env.GetEventId(), err)
		}
		results = append(results, result)
	}
	return results
}

// shippedFixtures are the fixtures replayed here. Every one of them is checked against
// projector.CheckInvariants after its replay: the exclusion constraints cannot see identity, so
// "two current versions of one canonical relationship" is an assertion only a reader that
// resolves merges can make, and a fixture replay is where it is cheapest to make it.
var shippedFixtures = []string{
	"baseline-topology-01",
	"late-arriving-fact-01",
	"retraction-with-edges-01",
	"rollout-regression-01",
}

// assertInvariants is the general structural check run after every fixture replay.
func assertInvariants(t *testing.T, store *postgres.Store, when string) {
	t.Helper()
	if err := projector.CheckInvariants(context.Background(), store); err != nil {
		t.Errorf("%s: %v", when, err)
	}
}

func TestFixturesReplay(t *testing.T) {
	for _, id := range shippedFixtures {
		t.Run(id, func(t *testing.T) {
			dir := filepath.Join(fixturesDir, id)
			m := loadManifest(t, dir)
			events := loadEvents(t, filepath.Join(dir, m.Events))
			p, store := newProjector(t, m)

			for i, result := range applyAll(t, p, events) {
				if result.GetStatus() != graphv1.IngestResult_APPLIED {
					t.Fatalf("event %s: status %s (%s: %s), want APPLIED",
						events[i].env.GetEventId(), result.GetStatus(),
						result.GetReasonCode(), result.GetReasonDetail())
				}
			}

			assertInvariants(t, store, "after replay")

			// Double delivery changes nothing (FR-020, SC-004).
			before := snapshot(t, store)
			versionsBefore := countRows(t, store, `SELECT count(*) FROM graph.entity_versions`)
			edgesBefore := countRows(t, store, `SELECT count(*) FROM graph.edge_versions`)

			for i, result := range applyAll(t, p, events) {
				if result.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
					t.Fatalf("re-delivered %s: status %s, want DUPLICATE_NOOP",
						events[i].env.GetEventId(), result.GetStatus())
				}
			}
			if got := countRows(t, store, `SELECT count(*) FROM graph.entity_versions`); got != versionsBefore {
				t.Errorf("entity_versions grew from %d to %d on re-delivery", versionsBefore, got)
			}
			if got := countRows(t, store, `SELECT count(*) FROM graph.edge_versions`); got != edgesBefore {
				t.Errorf("edge_versions grew from %d to %d on re-delivery", edgesBefore, got)
			}
			if after := snapshot(t, store); after != before {
				t.Errorf("valid-time state changed on re-delivery:\nbefore %s\nafter  %s", before, after)
			}
			assertInvariants(t, store, "after re-delivery")

			switch id {
			case "baseline-topology-01":
				assertBaseline(t, store, dir, m, p)
			case "late-arriving-fact-01":
				assertLateArriving(t, store)
			case "retraction-with-edges-01":
				assertRetraction(t, store)
			}
		})
	}
}

// assertBaseline checks the certain-rule merges the fixture is built to exercise
// (fixtures/README.md, research §10 C2, ADR-0001 D7) and the telemetry rejection path (SC-009).
func assertBaseline(t *testing.T, store *postgres.Store, dir string, m manifest, p *projector.Projector) {
	t.Helper()
	ctx := context.Background()

	rows, err := store.Pool().Query(ctx, `
		SELECT surviving_id, merged_id, rule_id, score
		FROM graph.resolution_decisions WHERE kind = 'auto_merge' ORDER BY surviving_id`)
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	type decision struct {
		survivor, merged, rule string
		score                  float64
	}
	var decisions []decision
	for rows.Next() {
		var d decision
		if err := rows.Scan(&d.survivor, &d.merged, &d.rule, &d.score); err != nil {
			t.Fatalf("scan decision: %v", err)
		}
		decisions = append(decisions, d)
	}
	rows.Close()

	if len(decisions) != 4 {
		t.Fatalf("auto merges = %d, want 4 (one per workload declaring its service name)", len(decisions))
	}
	for _, name := range []string{"checkout", "inventory", "payments", "storefront"} {
		k8sID := graph.EntityID("k8s.deployment", "shop/"+name)
		otelID := graph.EntityID("otel.service.name", name)
		idx := slices.IndexFunc(decisions, func(d decision) bool { return d.survivor == k8sID })
		if idx < 0 {
			t.Errorf("%s: no merge with the Kubernetes entity as survivor; decisions=%v", name, decisions)
			continue
		}
		d := decisions[idx]
		if d.merged != otelID {
			t.Errorf("%s: merged id = %s, want the OpenTelemetry entity %s", name, d.merged, otelID)
		}
		if d.rule != "C2" {
			t.Errorf("%s: rule_id = %s, want C2 (the workload declares its service name)", name, d.rule)
		}
		if d.score != 1 {
			t.Errorf("%s: score = %v, want 1 for a certain rule", name, d.score)
		}

		var (
			typ    string
			facets []string
		)
		if err := store.Pool().QueryRow(ctx,
			`SELECT type, facets FROM graph.entities WHERE entity_id = $1`, k8sID).Scan(&typ, &facets); err != nil {
			t.Fatalf("%s: read survivor: %v", name, err)
		}
		if typ != "service" {
			t.Errorf("%s: survivor type = %q, want service (precedence SERVICE > WORKLOAD)", name, typ)
		}
		if !slices.Equal(facets, []string{"service", "workload"}) {
			t.Errorf("%s: facets = %v, want [service workload]", name, facets)
		}

		// The merged-away id still resolves to the survivor (FR-039).
		var mergedInto *string
		if err := store.Pool().QueryRow(ctx,
			`SELECT merged_into FROM graph.entities WHERE entity_id = $1`, otelID).Scan(&mergedInto); err != nil {
			t.Fatalf("%s: read merged entity: %v", name, err)
		}
		if mergedInto == nil || *mergedInto != k8sID {
			t.Errorf("%s: merged_into = %v, want %s", name, mergedInto, k8sID)
		}
	}

	// The fixture's one deliberately invalid event must be refused, naming the field (SC-009).
	if m.RejectedEvents == "" {
		t.Fatal("manifest has no rejected_events file")
	}
	stateBefore := snapshot(t, store)
	for _, event := range loadEvents(t, filepath.Join(dir, m.RejectedEvents)) {
		result, err := p.Apply(ctx, event.env, mustTime("2026-09-01T13:04:30Z"))
		if err != nil {
			t.Fatalf("apply rejected event: %v", err)
		}
		if result.GetStatus() != graphv1.IngestResult_REJECTED {
			t.Fatalf("rejected fixture event %s: status %s, want REJECTED", event.env.GetEventId(), result.GetStatus())
		}
		if result.GetReasonCode() != eventlog.ReasonTelemetryPayload {
			t.Errorf("reason_code = %q, want %q", result.GetReasonCode(), eventlog.ReasonTelemetryPayload)
		}
		if !strings.Contains(result.GetReasonDetail(), "latency_samples") {
			t.Errorf("reason_detail = %q, want it to name latency_samples", result.GetReasonDetail())
		}
	}
	if after := snapshot(t, store); after != stateBefore {
		t.Error("a rejected event changed the graph")
	}
}

// assertLateArriving checks US1 scenario 4: a fact true from 14:20 but only learned at 15:00
// has an observed lower bound of 15:00, so pinning observed time to 14:32 hides it.
func assertLateArriving(t *testing.T, store *postgres.Store) {
	t.Helper()
	src := graph.EntityID("otel.service.name", "checkout")
	dst := graph.EntityID("otel.service.name", "inventory-search")

	var (
		valid    postgres.TimeRange
		observed postgres.TimeRange
	)
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT valid, observed FROM graph.edge_versions
		WHERE src_id = $1 AND dst_id = $2 AND type = 'calls' AND upper_inf(observed)`,
		src, dst).Scan(&valid, &observed); err != nil {
		t.Fatalf("read checkout->inventory-search edge: %v", err)
	}
	if want := mustTime("2026-09-01T14:20:00Z"); !valid.Start.Equal(want) {
		t.Errorf("valid start = %s, want %s (when the call became true)", valid.Start, want)
	}
	if floor := mustTime("2026-09-01T15:00:00Z"); observed.Start.Before(floor) {
		t.Errorf("observed start = %s, want at or after %s (when the graph learned it)", observed.Start, floor)
	}
	if !observed.EndUnbounded {
		t.Error("observed interval is closed, want open: this is current knowledge")
	}
}

// assertRetraction checks FR-013 and the edge case "retracting a node with live edges": one
// retract_node closes the node and both its live edges at the same valid instant, and the
// cascaded edge versions say which node version caused it.
func assertRetraction(t *testing.T, store *postgres.Store) {
	t.Helper()
	ctx := context.Background()
	validEnd := mustTime("2026-09-01T14:10:00Z")
	cart := graph.EntityID("otel.service.name", "legacy-cart")

	// Two node versions: the original, now closed in observed time, and the bounded one.
	var versions int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM graph.entity_versions WHERE entity_id = $1`, cart).Scan(&versions); err != nil {
		t.Fatalf("count legacy-cart versions: %v", err)
	}
	if versions != 2 {
		t.Errorf("legacy-cart has %d versions, want 2 (the original kept, the retracted one added)", versions)
	}

	var current postgres.TimeRange
	if err := store.Pool().QueryRow(ctx, `
		SELECT valid FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, cart).Scan(&current); err != nil {
		t.Fatalf("read current legacy-cart version: %v", err)
	}
	if current.EndUnbounded || !current.End.Equal(validEnd) {
		t.Errorf("current valid interval = %s, want it to end at %s", current, validEnd)
	}

	rows, err := store.Pool().Query(ctx, `
		SELECT src_id, dst_id, valid, coalesce(closed_as_consequence_of, '')
		FROM graph.edge_versions
		WHERE (src_id = $1 OR dst_id = $1) AND upper_inf(observed)
		ORDER BY src_id, dst_id`, cart)
	if err != nil {
		t.Fatalf("read legacy-cart edges: %v", err)
	}
	defer rows.Close()

	edges := 0
	for rows.Next() {
		var (
			src, dst, consequence string
			valid                 postgres.TimeRange
		)
		if err := rows.Scan(&src, &dst, &valid, &consequence); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		edges++
		if valid.EndUnbounded || !valid.End.Equal(validEnd) {
			t.Errorf("edge %s->%s valid = %s, want it to end at %s", src, dst, valid, validEnd)
		}
		if consequence == "" {
			t.Errorf("edge %s->%s has no closed_as_consequence_of; the cascade must say why it ended", src, dst)
		}
	}
	if edges != 2 {
		t.Errorf("legacy-cart has %d current edges, want 2", edges)
	}
}

// TestFixturesShuffled is FR-048's third check: within a source's declared reordering window the
// order of delivery must not change the valid-time state. Entity ids may differ, because a
// canonical id derives from the claim the log saw first (research §4), so entities are matched
// by alias set and compared structurally.
func TestFixturesShuffled(t *testing.T) {
	for _, id := range shippedFixtures {
		t.Run(id, func(t *testing.T) {
			dir := filepath.Join(fixturesDir, id)
			m := loadManifest(t, dir)
			events := loadEvents(t, filepath.Join(dir, m.Events))

			ordered, store := newProjector(t, m)
			applyAll(t, ordered, events)
			want := snapshot(t, store)

			windows := map[string]time.Duration{}
			for _, src := range m.Sources {
				window, _ := time.ParseDuration(src.ReorderingWindow)
				windows[src.SourceID] = window
			}

			for seed := range uint64(6) {
				t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
					shuffled := shuffleWithinWindow(events, windows, rand.New(rand.NewPCG(seed, 0x5E3A6E17)))
					p, shuffledStore := newProjector(t, m)
					for i, result := range applyAll(t, p, shuffled) {
						if result.GetStatus() != graphv1.IngestResult_APPLIED {
							t.Fatalf("event %s: status %s (%s: %s), want APPLIED",
								shuffled[i].env.GetEventId(), result.GetStatus(),
								result.GetReasonCode(), result.GetReasonDetail())
						}
					}
					if got := snapshot(t, shuffledStore); got != want {
						t.Errorf("shuffled delivery produced a different valid-time state\nin order: %s\nshuffled: %s", want, got)
					}
					assertInvariants(t, shuffledStore, "after shuffled delivery")
				})
			}
		})
	}
}

// shuffleWithinWindow permutes each source's events among those whose observed times lie within
// its declared reordering window, and keeps the observed-time slots, so observed time still only
// moves forwards. That is exactly the freedom a feeder declares: it may deliver out of order
// inside the window, it may not deliver from the future.
func shuffleWithinWindow(events []fixtureEvent, windows map[string]time.Duration, rng *rand.Rand) []fixtureEvent {
	bySource := map[string][]fixtureEvent{}
	var order []string
	for _, event := range events {
		id := event.env.GetSourceId()
		if _, ok := bySource[id]; !ok {
			order = append(order, id)
		}
		bySource[id] = append(bySource[id], event)
	}

	out := make([]fixtureEvent, 0, len(events))
	for _, sourceID := range order {
		queue := slices.Clone(bySource[sourceID])
		slots := make([]time.Time, len(queue))
		for i, event := range queue {
			slots[i] = event.observedAt
		}
		window := windows[sourceID]
		permuted := make([]fixtureEvent, 0, len(queue))
		for len(queue) > 0 {
			limit := 1
			for limit < len(queue) && queue[limit].observedAt.Sub(queue[0].observedAt) <= window {
				limit++
			}
			pick := rng.IntN(limit)
			permuted = append(permuted, queue[pick])
			queue = append(queue[:pick], queue[pick+1:]...)
		}
		for i := range permuted {
			permuted[i].observedAt = slots[i]
		}
		out = append(out, permuted...)
	}
	slices.SortStableFunc(out, func(a, b fixtureEvent) int { return a.observedAt.Compare(b.observedAt) })
	return out
}

// ---------- structural snapshot ----------

// snapshot renders the valid-time state of the whole graph as canonical text, keyed by alias
// set rather than by entity id, so two runs that minted different ids for the same things
// compare equal (research §4, fixture-format.md step 3).
func snapshot(t *testing.T, store *postgres.Store) string {
	t.Helper()
	ctx := context.Background()

	redirect := map[string]string{}
	rows, err := store.Pool().Query(ctx, `SELECT entity_id, coalesce(merged_into, '') FROM graph.entities`)
	if err != nil {
		t.Fatalf("read entities: %v", err)
	}
	for rows.Next() {
		var id, into string
		if err := rows.Scan(&id, &into); err != nil {
			t.Fatalf("scan entity: %v", err)
		}
		redirect[id] = into
	}
	rows.Close()

	resolve := func(id string) string {
		for hop := 0; hop < 64; hop++ {
			next, ok := redirect[id]
			if !ok || next == "" {
				return id
			}
			id = next
		}
		return id
	}

	aliases := map[string][]string{}
	rows, err = store.Pool().Query(ctx, `SELECT entity_id, namespace, value FROM graph.identity_claims`)
	if err != nil {
		t.Fatalf("read claims: %v", err)
	}
	for rows.Next() {
		var id, namespace, value string
		if err := rows.Scan(&id, &namespace, &value); err != nil {
			t.Fatalf("scan claim: %v", err)
		}
		key := resolve(id)
		aliases[key] = append(aliases[key], namespace+"="+value)
	}
	rows.Close()

	name := func(id string) string {
		resolved := resolve(id)
		list := slices.Clone(aliases[resolved])
		slices.Sort(list)
		list = slices.Compact(list)
		if len(list) == 0 {
			return "anonymous:" + resolved
		}
		return strings.Join(list, ",")
	}

	nodes := map[string][]string{}
	rows, err = store.Pool().Query(ctx, `
		SELECT entity_id, valid, valid_from_unknown, display_name, props, conflicts, facets, coalesce(change::text, '')
		FROM graph.entity_versions WHERE upper_inf(observed)`)
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	for rows.Next() {
		var (
			id          string
			valid       postgres.TimeRange
			fromUnknown bool
			displayName string
			props       []byte
			conflicts   []string
			facets      []string
			change      string
		)
		if err := rows.Scan(&id, &valid, &fromUnknown, &displayName, &props, &conflicts, &facets, &change); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		key := name(id)
		nodes[key] = append(nodes[key], fmt.Sprintf(
			"valid=%s from_unknown=%v name=%q props=%s conflicts=%v facets=%v change=%s",
			valid, fromUnknown, displayName, propValues(t, props), conflicts, facets, change))
	}
	rows.Close()

	edges := map[string][]string{}
	rows, err = store.Pool().Query(ctx, `
		SELECT src_id, dst_id, type, valid, weight_class, props, coalesce(closed_as_consequence_of, '') <> ''
		FROM graph.edge_versions WHERE upper_inf(observed)`)
	if err != nil {
		t.Fatalf("read edge versions: %v", err)
	}
	for rows.Next() {
		var (
			src, dst, typ string
			valid         postgres.TimeRange
			weight        *int16
			props         []byte
			cascaded      bool
		)
		if err := rows.Scan(&src, &dst, &typ, &valid, &weight, &props, &cascaded); err != nil {
			t.Fatalf("scan edge version: %v", err)
		}
		key := name(src) + " -" + typ + "-> " + name(dst)
		weightText := "none"
		if weight != nil {
			weightText = fmt.Sprint(*weight)
		}
		edges[key] = append(edges[key], fmt.Sprintf("valid=%s weight=%s props=%s cascaded=%v",
			valid, weightText, propValues(t, props), cascaded))
	}
	rows.Close()

	var out strings.Builder
	for _, key := range sortedMapKeys(nodes) {
		lines := slices.Clone(nodes[key])
		slices.Sort(lines)
		out.WriteString("\nNODE " + key + "\n  " + strings.Join(lines, "\n  "))
	}
	for _, key := range sortedMapKeys(edges) {
		lines := slices.Clone(edges[key])
		slices.Sort(lines)
		out.WriteString("\nEDGE " + key + "\n  " + strings.Join(lines, "\n  "))
	}
	return out.String()
}

// propValues reduces a stored props column to "who asserted what", dropping the event ids: two
// orderings may credit the same value to different (equally true) events, and the fixture
// contract compares valid-time state, not provenance.
func propValues(t *testing.T, raw []byte) string {
	t.Helper()
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode props: %v", err)
	}
	type record struct {
		Value    json.RawMessage `json:"value"`
		SourceID string          `json:"source_id"`
	}
	parts := make([]string, 0, len(decoded))
	for _, key := range sortedMapKeys(decoded) {
		var records []record
		if err := json.Unmarshal(decoded[key], &records); err != nil {
			var single record
			if err := json.Unmarshal(decoded[key], &single); err != nil {
				t.Fatalf("decode prop %s: %v", key, err)
			}
			records = []record{single}
		}
		rendered := make([]string, 0, len(records))
		for _, r := range records {
			rendered = append(rendered, r.SourceID+":"+string(r.Value))
		}
		slices.Sort(rendered)
		parts = append(parts, key+"="+strings.Join(rendered, "|"))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func countRows(t *testing.T, store *postgres.Store, sql string) int {
	t.Helper()
	var count int
	if err := store.Pool().QueryRow(context.Background(), sql).Scan(&count); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return count
}

func mustTime(s string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return parsed.UTC()
}
