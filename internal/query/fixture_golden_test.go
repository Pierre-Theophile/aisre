// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/fixture/fixturetest"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The shipped fixtures, their goldens, and what the goldens are supposed to say (US1,
// constitution VIII, SC-003).
//
// Three tests, in the order they matter:
//
//  1. TestRecordShippedFixtures regenerates the goldens. It is opt-in (SRE_AGENT_RECORD=1)
//     because it writes into the repository; it is a test rather than a script so that
//     re-recording needs neither Docker nor a PostgreSQL of one's own — pgtest supplies one.
//  2. TestShippedGoldensAssertTheAcceptanceScenarios reads the recorded files and checks they
//     say what US1 says they must. A golden that is merely *stable* proves nothing; these
//     assertions are what make a regression in the goldens mean something.
//  3. TestVerifyShippedFixtures runs the whole `fixture verify` contract — replay, goldens,
//     pinned goldens, double delivery, shuffle, expect-rejected — through the query runner.

func TestMain(m *testing.M) { pgtest.TestMain(m) }

const fixturesDir = "../../fixtures"

// shippedFixtures are the fixtures this phase records and verifies.
var shippedFixtures = []string{
	"baseline-topology-01",
	"late-arriving-fact-01",
	"retraction-with-edges-01",
	"rollout-regression-01",
	"rollout-regression-02",
	"config-change-01",
	"feeder-gap-01",
	"announced-fact-01",
}

// shippedIncidentFixtures are feature 002's incident fixtures, which live one directory deeper
// (`fixtures/incidents/`) and are a 001 fixture plus an `incident:` block, a `world/` and,
// later, a `trajectories/` (002 contracts/incident-format.md, FR-063).
//
// They are recorded and verified here, by the same two tests and against the same contract,
// because that is the whole claim of FR-063: an incident fixture is not a second input format
// and must not need a second harness. What is 002-specific about them — the ground truth, the
// recorded world, the trajectory layer — is asserted in `internal/fixture` and by the
// verifier's own `incident` step, not here.
var shippedIncidentFixtures = []string{
	"incidents/rollout-regression-01-incident",
	"incidents/declared-incident-01",
	"incidents/human-fact-reopen-01",
	"incidents/unknown-feeder-gap-01",
	"incidents/two-simultaneous-01",
	"incidents/merged-alias-01",
	"incidents/telemetry-rejection-01",
	"incidents/injection-01",
	"incidents/knowledge-scope-01",
	"incidents/unobserved-latent-bug-01",
	"incidents/unobserved-client-config-01",
	"incidents/unobserved-business-data-01",
	"incidents/unobserved-credential-leak-01",
}

// recordedAndVerifiedFixtures is every fixture these two tests record and verify — 001's, plus
// feature 002's incident fixtures one directory deeper. It is a list rather than a directory
// walk (fixture_export_test.go has the walk) because these two tests are the ones whose
// failures a reviewer reads as "this fixture was meant to be here", and a walk cannot tell a
// fixture that was deleted from a fixture that was never added.
func recordedAndVerifiedFixtures() []string {
	return append(slices.Clone(shippedFixtures), shippedIncidentFixtures...)
}

// shuffleExceptions names fixtures whose shuffle step is known to fail, and why. A negative
// count skips the step (fixture.VerifyOptions).
var shuffleExceptions = map[string]string{
	// Empty, and it should stay that way.
	//
	// Two entries have lived here and both are gone, each removed by a fix rather than by a
	// re-recording:
	//
	//  1. The Kubernetes feeder took the initial list's valid time from whichever payload
	//     arrived first, so a permutation inside the declared 60 s window moved every
	//     initial-list fact by a few microseconds and materialized placeholder versions of
	//     nodes an edge reached before its endpoint. Fixed in internal/feeders/k8s/feeder.go
	//     (validFor, ListSettle, closeListAt) and internal/projector/upsert_edge.go
	//     (markPlaceholder inherits valid_from_unknown).
	//
	//  2. The OpenTelemetry feeder declared its *aggregation* window — 300 s — as its
	//     reordering window, while emitting its windows strictly in order and never
	//     reordering them. `feeder-gap-01` has a retraction of `inventory->redis` and a
	//     re-assertion of the same edge 296 s apart, so the verifier was entitled to swap
	//     them; delivered that way round the re-assertion coalesced into the segment the
	//     retraction then cut, and the [11:55, ∞) interval was lost. That was fixed twice
	//     over: first by the feeder declaring a window it can honour
	//     (internal/feeders/otel.DeclaredReorderingWindow, 30 s, independent of --window),
	//     and then properly, in the projector — a coalesced segment remembers the first
	//     instant each source restated it and a retraction splits rather than truncates
	//     (internal/projector/segments.go, rememberRestatement and expandRestatements,
	//     pinned by TestRetractionAndReassertionAreOrderIndependent). The swap is survived
	//     now, which is what let k8s.DefaultReorderingWindow go back to 60 s.
	//
	// An entry here is not a way to make a fixture pass. It is the opposite: a recording the
	// graph cannot replay order-independently is evidence of a bug, and the fixture is shipped
	// *because* it is the reproduction. Adding one needs the diagnosis written out, as those
	// two were; removing one is what fixing the bug looks like.
}

func newRunner(store *postgres.Store) fixture.QueryRunner {
	return query.NewRunner(query.NewEngine(store))
}

// TestRecordShippedFixtures rewrites every shipped fixture's goldens.
//
// Run it with:
//
//	SRE_AGENT_RECORD=1 go test ./internal/query -run TestRecordShippedFixtures
//
// and review the diff. This is the documented way to re-record: `aisre fixture record`
// does the same thing but needs a PostgreSQL DSN, while the test brings its own server.
func TestRecordShippedFixtures(t *testing.T) {
	if os.Getenv("SRE_AGENT_RECORD") != "1" {
		t.Skip("set SRE_AGENT_RECORD=1 to rewrite the shipped fixtures' goldens")
	}
	ctx := context.Background()
	factory := fixturetest.PgtestFactory(t)

	for _, name := range recordedAndVerifiedFixtures() {
		t.Run(name, func(t *testing.T) {
			report, err := fixture.Record(ctx, factory, filepath.Join(fixturesDir, name), newRunner)
			if err != nil {
				t.Fatalf("Record: %v", err)
			}
			if len(report.Written) == 0 {
				t.Fatalf("no goldens written for %s", name)
			}
			t.Logf("%s: wrote %d golden(s) %v; skipped %v",
				name, len(report.Written), report.Written, report.Skipped)
		})
	}
}

// TestVerifyShippedFixtures is the constitution's replay gate with the query layer attached:
// every fixture replays from empty, answers its manifest queries byte for byte against the
// recorded goldens, answers them again with observed time pinned to the manifest's clock end,
// survives double delivery and survives shuffling within each source's reordering window.
func TestVerifyShippedFixtures(t *testing.T) {
	ctx := context.Background()
	factory := fixturetest.PgtestFactory(t)

	for _, name := range recordedAndVerifiedFixtures() {
		t.Run(name, func(t *testing.T) {
			shuffles := 2
			if why, skipped := shuffleExceptions[name]; skipped {
				t.Logf("shuffle step skipped: %s", why)
				shuffles = -1
			}
			report, err := fixture.Verify(ctx, factory, filepath.Join(fixturesDir, name), fixture.VerifyOptions{
				Shuffles:  shuffles,
				NewRunner: newRunner,
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			for _, step := range report.Steps {
				if !step.Passed {
					t.Errorf("step %s failed: %s", step.Name, step.Detail)
				}
			}
			if !report.Passed {
				t.Errorf("%s did not verify", name)
			}
			if compared := report.Steps[0].Detail; !strings.Contains(compared, "goldens match") {
				t.Errorf("replay step did not compare goldens: %s", compared)
			}
		})
	}
}

// ---------- the acceptance scenarios, read off the recorded goldens ----------

// goldenSubgraph is the part of a recorded SubgraphResponse these assertions read. The goldens
// are canonical protobuf JSON, so the field names are the lowerCamel ones.
type goldenSubgraph struct {
	Focus *goldenNode `json:"focus"`
	Nodes []struct {
		goldenNode
	} `json:"nodes"`
	Edges []struct {
		SrcID       string  `json:"srcId"`
		DstID       string  `json:"dstId"`
		Type        string  `json:"type"`
		WeightClass *uint32 `json:"weightClass"`
	} `json:"edges"`
	Truncation struct {
		Truncated            bool     `json:"truncated"`
		Reason               string   `json:"reason"`
		PerHopCap            uint32   `json:"perHopCap"`
		TotalCap             uint32   `json:"totalCap"`
		TruncatedAtEntityIDs []string `json:"truncatedAtEntityIds"`
	} `json:"truncation"`
	Extent struct {
		EarliestObserved string `json:"earliestObserved"`
		LatestObserved   string `json:"latestObserved"`
	} `json:"extent"`
}

type goldenNode struct {
	EntityID    string   `json:"entityId"`
	Type        string   `json:"type"`
	DisplayName string   `json:"displayName"`
	Facets      []string `json:"facets"`
	Aliases     []struct {
		Namespace string `json:"namespace"`
		Value     string `json:"value"`
	} `json:"aliases"`
}

// names returns the display names in the response, sorted, so an assertion can talk about the
// graph the way a person does rather than in base32 ids.
func (g goldenSubgraph) names() []string {
	out := make([]string, 0, len(g.Nodes))
	for _, node := range g.Nodes {
		out = append(out, node.DisplayName)
	}
	slices.Sort(out)
	return out
}

func (g goldenSubgraph) node(t *testing.T, displayName string) goldenNode {
	t.Helper()
	for _, node := range g.Nodes {
		if node.DisplayName == displayName {
			return node.goldenNode
		}
	}
	t.Fatalf("no node named %q in the response; it has %v", displayName, g.names())
	return goldenNode{}
}

func readGolden(t *testing.T, fixtureID, name string) goldenSubgraph {
	t.Helper()
	path := filepath.Join(fixturesDir, fixtureID, "golden", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var out goldenSubgraph
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}

func requireNames(t *testing.T, got goldenSubgraph, want ...string) {
	t.Helper()
	have := got.names()
	for _, name := range want {
		if !slices.Contains(have, name) {
			t.Errorf("%q is missing from the neighbourhood; it holds %v", name, have)
		}
	}
}

func refuseNames(t *testing.T, got goldenSubgraph, unwanted ...string) {
	t.Helper()
	have := got.names()
	for _, name := range unwanted {
		if slices.Contains(have, name) {
			t.Errorf("%q should not be in the neighbourhood; it holds %v", name, have)
		}
	}
}

// TestBaselineTwoHopNeighbourhood is US1 scenario 1: the 2-hop neighbourhood of checkout at
// 14:32 holds exactly what is one and two hops away, with the merged workload/service entity
// resolved to SERVICE and WORKLOAD kept as a facet (research §10).
func TestBaselineTwoHopNeighbourhood(t *testing.T) {
	got := readGolden(t, "baseline-topology-01", "subgraph.checkout-2hop")

	if got.Focus == nil || got.Focus.DisplayName != "checkout" {
		t.Fatalf("focus = %+v, want the checkout node", got.Focus)
	}
	requireNames(t, got,
		"checkout",                                     // the focus itself
		"storefront", "payments", "inventory", "redis", // hop 1 over calls
		"payments-db",                    // hop 2: payments -> payments-db
		"stripe",                         // hop 2: payments -> stripe
		"team-checkout", "team-payments", // owners
		"checkout-config", // hop 1 depends_on
		"payments-secret", // hop 2 depends_on
		"general",         // hop 1 runs_on node pool
		"sre-agent-demo",  // hop 2 runs_on cluster — the kind cluster this was recorded from
		"svc/checkout",    // hop 1 exposed_via
		"svc/storefront",  // hop 2 exposed_via
		"ing/storefront",  // hop 2 exposed_via
	)

	// The Kubernetes deployment and the OpenTelemetry service are one entity under two names:
	// resolved type SERVICE, WORKLOAD kept in facets, and both names in the aliases.
	focus := got.node(t, "checkout")
	if focus.Type != "SERVICE" {
		t.Errorf("checkout type = %q, want SERVICE (merge precedence, research §10)", focus.Type)
	}
	if !slices.Contains(focus.Facets, "WORKLOAD") || !slices.Contains(focus.Facets, "SERVICE") {
		t.Errorf("checkout facets = %v, want both SERVICE and WORKLOAD", focus.Facets)
	}
	var namespaces []string
	for _, alias := range focus.Aliases {
		namespaces = append(namespaces, alias.Namespace+"="+alias.Value)
	}
	if !slices.Contains(namespaces, "otel.service.name=checkout") ||
		!slices.Contains(namespaces, "k8s.deployment=shop/checkout") {
		t.Errorf("checkout aliases = %v, want both the telemetry and the Kubernetes name", namespaces)
	}

	if got.Truncation.Truncated {
		t.Errorf("the 2-hop neighbourhood should fit under the caps, got %+v", got.Truncation)
	}
	if got.Extent.EarliestObserved == "" {
		t.Error("the response must carry the graph's extent (FR-052)")
	}

	// Every edge's endpoints are nodes of the response: an answer that points outside itself
	// cannot be consumed as structure (constitution V).
	ids := map[string]bool{}
	for _, node := range got.Nodes {
		ids[node.EntityID] = true
	}
	for _, edge := range got.Edges {
		if !ids[edge.SrcID] || !ids[edge.DstID] {
			t.Errorf("edge %s -> %s (%s) points outside the returned node set", edge.SrcID, edge.DstID, edge.Type)
		}
	}
}

// TestBaselineHubTruncation is US1 scenario 3: a 3-hop walk that crosses the shared cache with
// a per-hop cap of 5 is cut, and says so.
func TestBaselineHubTruncation(t *testing.T) {
	got := readGolden(t, "baseline-topology-01", "subgraph.redis-3hop-cap5")

	if !got.Truncation.Truncated {
		t.Fatalf("expected truncation with per_hop_cap 5, got %+v", got.Truncation)
	}
	if got.Truncation.Reason != query.ReasonPerHopCap {
		t.Errorf("reason = %q, want %q", got.Truncation.Reason, query.ReasonPerHopCap)
	}
	if got.Truncation.PerHopCap != 5 {
		t.Errorf("per_hop_cap = %d, want 5", got.Truncation.PerHopCap)
	}
	if len(got.Truncation.TruncatedAtEntityIDs) == 0 {
		t.Error("a truncation must name the nodes whose expansion it cut (FR-026)")
	}
}

// TestBaselineBeforeHistory is the "query before history" edge case: an instant earlier than
// anything observed is an empty, flagged answer — not an error and not a truncation.
func TestBaselineBeforeHistory(t *testing.T) {
	got := readGolden(t, "baseline-topology-01", "subgraph.before-history")

	if len(got.Nodes) != 0 || len(got.Edges) != 0 {
		t.Errorf("before recorded history must be empty, got %d nodes and %d edges",
			len(got.Nodes), len(got.Edges))
	}
	if got.Focus != nil {
		t.Errorf("before recorded history must carry no focus version, got %+v", got.Focus)
	}
	if got.Truncation.Reason != query.ReasonBeforeHistory {
		t.Errorf("reason = %q, want %q", got.Truncation.Reason, query.ReasonBeforeHistory)
	}
	if got.Truncation.Truncated {
		t.Error("before recorded history is a flag, not a truncation: nothing was cut")
	}
}

// TestLateArrivingObservedPinning is US1 scenario 4: a fact learned at 15:00 about 14:20 is in
// the answer as known now, and out of it as known at 14:32.
func TestLateArrivingObservedPinning(t *testing.T) {
	now := readGolden(t, "late-arriving-fact-01", "subgraph.checkout-1432-now")
	requireNames(t, now, "checkout", "inventory-search")

	pinned := readGolden(t, "late-arriving-fact-01", "subgraph.checkout-1432-pinned")
	requireNames(t, pinned, "checkout", "payments", "inventory")
	refuseNames(t, pinned, "inventory-search")
}

// TestRetractionRemovesNodeAndItsEdges is US1 scenario 2 and the retraction cascade: at 14:00
// legacy-cart and both its edges are there; at 14:32 neither is, while everything the retracted
// node used to lead to is still reachable the other way.
func TestRetractionRemovesNodeAndItsEdges(t *testing.T) {
	before := readGolden(t, "retraction-with-edges-01", "subgraph.checkout-1400")
	requireNames(t, before, "checkout", "legacy-cart", "redis")

	after := readGolden(t, "retraction-with-edges-01", "subgraph.checkout-1432")
	refuseNames(t, after, "legacy-cart")
	requireNames(t, after, "checkout", "redis")

	for _, edge := range after.Edges {
		if edge.SrcID == "" || edge.DstID == "" {
			t.Errorf("edge with an empty endpoint: %+v", edge)
		}
	}
}

// ---------- US4 and US5, read off the recorded goldens (T065, T068, T070) ----------

// goldenImpact is the part of a recorded ImpactResponse these assertions read.
type goldenImpact struct {
	Downstream []goldenImpactItem `json:"downstream"`
	Upstream   []goldenImpactItem `json:"upstream"`
	Truncation struct {
		Truncated bool   `json:"truncated"`
		Reason    string `json:"reason"`
	} `json:"truncation"`
}

type goldenImpactItem struct {
	Node             goldenNode `json:"node"`
	HopDistance      uint32     `json:"hopDistance"`
	HeaviestPath     []string   `json:"heaviestPath"`
	AlternativePaths uint32     `json:"alternativePaths"`
	WeightClass      uint32     `json:"weightClass"`
}

// goldenPointers is the part of a recorded PointersResponse these assertions read.
type goldenPointers struct {
	Node   goldenNode `json:"node"`
	ByKind map[string]struct {
		Pointers []struct {
			BackendKind string `json:"backendKind"`
			Vocabulary  string `json:"vocabulary"`
			Selector    string `json:"selector"`
		} `json:"pointers"`
	} `json:"byKind"`
}

// goldenHistory is the part of a recorded NodeHistoryResponse these assertions read.
type goldenHistory struct {
	Versions []struct {
		goldenNode
		VersionID string `json:"versionId"`
		Valid     struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"valid"`
		Observed struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"observed"`
		Props map[string]any `json:"props"`
	} `json:"versions"`
	Decisions []struct {
		Kind      string `json:"kind"`
		RuleID    string `json:"ruleId"`
		Principal string `json:"principal"`
		Rationale string `json:"rationale"`
	} `json:"decisions"`
}

// goldenBytes reads a recorded golden.
//
// A missing file is a failure, never a skip. Every query kind in the contract is answerable, so
// a manifest query with no golden means somebody added the query and did not record it — which
// `fixture verify` also reports as a failure. Skipping here would turn that into silence.
func goldenBytes(t *testing.T, fixtureID, name string) []byte {
	t.Helper()
	path := filepath.Join(fixturesDir, fixtureID, "golden", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (re-record with SRE_AGENT_RECORD=1 go test ./internal/query "+
			"-run TestRecordShippedFixtures): %v", err)
	}
	return raw
}

func readImpactGolden(t *testing.T, fixtureID, name string) goldenImpact {
	t.Helper()
	var out goldenImpact
	if err := json.Unmarshal(goldenBytes(t, fixtureID, "impact."+name), &out); err != nil {
		t.Fatalf("decode impact.%s: %v", name, err)
	}
	return out
}

func readPointersGolden(t *testing.T, fixtureID, name string) goldenPointers {
	t.Helper()
	var out goldenPointers
	if err := json.Unmarshal(goldenBytes(t, fixtureID, "pointers."+name), &out); err != nil {
		t.Fatalf("decode pointers.%s: %v", name, err)
	}
	return out
}

func readHistoryGolden(t *testing.T, fixtureID, name string) goldenHistory {
	t.Helper()
	var out goldenHistory
	if err := json.Unmarshal(goldenBytes(t, fixtureID, "history."+name), &out); err != nil {
		t.Fatalf("decode history.%s: %v", name, err)
	}
	return out
}

// impactItem finds one side's entry by display name.
func impactItem(t *testing.T, items []goldenImpactItem, displayName string) goldenImpactItem {
	t.Helper()
	for _, item := range items {
		if item.Node.DisplayName == displayName {
			return item
		}
	}
	have := make([]string, 0, len(items))
	for _, item := range items {
		have = append(have, item.Node.DisplayName)
	}
	t.Fatalf("no impact item for %q; the list holds %v", displayName, have)
	return goldenImpactItem{}
}

// assertImpactOrder checks the published ordering — weight descending, then hop ascending, then
// entity id — holds over a recorded list. An answer that is merely stable is not enough: the
// order is what an operator reads top-down when deciding who to page (FR-029).
func assertImpactOrder(t *testing.T, side string, items []goldenImpactItem) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		prev, cur := items[i-1], items[i]
		switch {
		case prev.WeightClass != cur.WeightClass:
			if prev.WeightClass < cur.WeightClass {
				t.Errorf("%s[%d] weight %d is below %s[%d]'s %d; the list is heaviest first",
					side, i-1, prev.WeightClass, side, i, cur.WeightClass)
			}
		case prev.HopDistance != cur.HopDistance:
			if prev.HopDistance > cur.HopDistance {
				t.Errorf("%s[%d] is %d hops out, past %s[%d]'s %d, at equal weight",
					side, i-1, prev.HopDistance, side, i, cur.HopDistance)
			}
		default:
			if prev.Node.EntityID > cur.Node.EntityID {
				t.Errorf("%s[%d] and [%d] tie on weight and hop but are not in entity-id order",
					side, i-1, i)
			}
		}
	}
}

// TestImpactSeparatesDependentsFromDependenciesOnAFixture is US4 scenario 1 read off
// `rollout-regression-01`: checkout is a downstream dependent of payments at hop 1 with its
// traffic weight, and payments-db is an upstream dependency at hop 1.
//
// This is the assertion that would catch the two lists being swapped — the one mistake in this
// query that no amount of internal consistency would reveal.
func TestImpactSeparatesDependentsFromDependenciesOnAFixture(t *testing.T) {
	got := readImpactGolden(t, "rollout-regression-01", "payments-impact")

	checkout := impactItem(t, got.Downstream, "checkout")
	if checkout.HopDistance != 1 {
		t.Errorf("checkout hop = %d, want 1: it calls payments directly", checkout.HopDistance)
	}
	if checkout.WeightClass != 3 {
		t.Errorf("checkout weight = %d, want 3: the class of checkout -> payments", checkout.WeightClass)
	}

	db := impactItem(t, got.Upstream, "payments-db")
	if db.HopDistance != 1 || db.WeightClass != 4 {
		t.Errorf("payments-db: hop %d weight %d, want hop 1 weight 4", db.HopDistance, db.WeightClass)
	}

	// The two lists never share a member here: nothing both depends on payments and is depended
	// on by it in this fixture, so a leak between them would show up immediately.
	for _, up := range got.Upstream {
		for _, down := range got.Downstream {
			if up.Node.EntityID == down.Node.EntityID {
				t.Errorf("%s is in both lists", up.Node.DisplayName)
			}
		}
	}
	assertImpactOrder(t, "downstream", got.Downstream)
	assertImpactOrder(t, "upstream", got.Upstream)
	if got.Truncation.Truncated {
		t.Errorf("this neighbourhood fits under the caps, got %+v", got.Truncation)
	}
}

// TestImpactReportsTheHeaviestOfTwoRoutes is US4 scenario 2 read off the same golden: storefront
// reaches payments directly at weight class 1 and through checkout at classes 4 and 3, so it
// appears once, at hop distance 1, with the two-hop route shown — the one whose weakest link is
// loudest — and one alternative counted.
func TestImpactReportsTheHeaviestOfTwoRoutes(t *testing.T) {
	got := readImpactGolden(t, "rollout-regression-01", "payments-impact")

	seen := 0
	for _, item := range got.Downstream {
		if item.Node.DisplayName == "storefront" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("storefront appears %d times downstream, want exactly once", seen)
	}

	store := impactItem(t, got.Downstream, "storefront")
	if store.HopDistance != 1 {
		t.Errorf("storefront hop = %d, want 1: the direct call is the shortest way in", store.HopDistance)
	}
	if store.WeightClass != 3 {
		t.Errorf("storefront weight = %d, want 3: the heaviest route bottlenecks on checkout -> payments",
			store.WeightClass)
	}
	if store.AlternativePaths != 1 {
		t.Errorf("storefront alternative_paths = %d, want 1: the direct edge is the other way in",
			store.AlternativePaths)
	}
	if len(store.HeaviestPath) != 3 {
		t.Fatalf("storefront heaviest path = %v, want three entity ids (payments, checkout, storefront)",
			store.HeaviestPath)
	}
	checkout := impactItem(t, got.Downstream, "checkout")
	if store.HeaviestPath[1] != checkout.Node.EntityID {
		t.Errorf("storefront's heaviest path does not run through checkout: %v", store.HeaviestPath)
	}
	if store.HeaviestPath[2] != store.Node.EntityID {
		t.Errorf("a heaviest path must end at the item it belongs to: %v", store.HeaviestPath)
	}
	// Every path starts at the focus, so all of them share a first element.
	for _, item := range append(slices.Clone(got.Downstream), got.Upstream...) {
		if len(item.HeaviestPath) == 0 {
			t.Errorf("%s has no heaviest path", item.Node.DisplayName)
			continue
		}
		if item.HeaviestPath[0] != store.HeaviestPath[0] {
			t.Errorf("%s's path starts at %s, not at the focus", item.Node.DisplayName, item.HeaviestPath[0])
		}
	}
}

// TestPointersUseTheSelectorsValidAtTheInstant is US5 scenario 1 and 2 on `rollout-regression-01`:
// every pointer names the backend it targets and the vocabulary it is written in, and the two
// instants either side of the 14:00 rename return different selectors.
func TestPointersUseTheSelectorsValidAtTheInstant(t *testing.T) {
	before := readPointersGolden(t, "rollout-regression-01", "payments-pointers-1300")
	after := readPointersGolden(t, "rollout-regression-01", "payments-pointers-1432")

	for _, got := range []goldenPointers{before, after} {
		if len(got.ByKind) == 0 {
			t.Fatalf("%s has no pointers at all", got.Node.DisplayName)
		}
		for kind, list := range got.ByKind {
			if len(list.Pointers) == 0 {
				t.Errorf("kind %s is present and empty; an empty kind is omitted", kind)
			}
			for _, p := range list.Pointers {
				if p.BackendKind == "" || p.Vocabulary == "" || p.Selector == "" {
					t.Errorf("%s pointer %+v is missing a backend, a vocabulary or a selector (FR-008)", kind, p)
				}
			}
		}
	}

	if before.Node.DisplayName != "payments" {
		t.Errorf("at 13:00 the node is called %q, want payments (before the rename)", before.Node.DisplayName)
	}
	if after.Node.DisplayName != "payments-v2" {
		t.Errorf("at 14:32 the node is called %q, want payments-v2 (after the rename)", after.Node.DisplayName)
	}

	// The point of the pair, and the point of unioning pointers across sources: the selectors
	// that the rename touched moved, and the ones it did not touch did not.
	//
	// Only the OpenTelemetry side of this entity was renamed — the service now reports itself as
	// `payments-v2` — while the Kubernetes deployment is still `shop/payments`. So the TRACE and
	// METRIC selectors move and the LOG and SOURCE_LINK ones must not. A pointer set taken from
	// one primary assertion could not show that at all; the union is what makes each source's
	// truth move on its own schedule.
	moved := map[string]bool{"TRACE": true, "METRIC": true, "LOG": false, "SOURCE_LINK": false}
	for kind, shouldMove := range moved {
		was, hadBefore := before.ByKind[kind]
		is, hasAfter := after.ByKind[kind]
		if !hadBefore || !hasAfter {
			t.Errorf("kind %s is missing on one side of the rename (before=%v after=%v); the union "+
				"must carry every source's pointers at both instants", kind, hadBefore, hasAfter)
			continue
		}
		if len(was.Pointers) != len(is.Pointers) {
			t.Errorf("kind %s has %d pointer(s) before the rename and %d after",
				kind, len(was.Pointers), len(is.Pointers))
			continue
		}
		for i, p := range was.Pointers {
			peer := is.Pointers[i]
			switch {
			case shouldMove && p.Selector == peer.Selector:
				t.Errorf("the %s selector %q is unchanged across the rename; US5 scenario 2 needs "+
					"the renamed source's selectors to move", kind, p.Selector)
			case shouldMove && !strings.Contains(peer.Selector, "payments-v2"):
				t.Errorf("the 14:32 %s selector should use the new name: %q", kind, peer.Selector)
			case shouldMove && strings.Contains(p.Selector, "payments-v2"):
				t.Errorf("the 13:00 %s selector should use the old name: %q", kind, p.Selector)
			case !shouldMove && p.Selector != peer.Selector:
				t.Errorf("the %s selector moved (%q -> %q) although the Kubernetes deployment was "+
					"not renamed", kind, p.Selector, peer.Selector)
			}
		}
	}
}

// TestBaselinePointersAreStableAcrossAStableWindow is the other half of US5 scenario 1, on the
// recorded baseline: that graph does not change during its window, so the two instants exist
// precisely to catch a pointer that drifts in a graph that did not
// (docs/benchmarks/live-run-2026-09-16.md, "For Phase 6 and Phase 7").
func TestBaselinePointersAreStableAcrossAStableWindow(t *testing.T) {
	end := goldenBytes(t, "baseline-topology-01", "pointers.payments-pointers-end")
	mid := goldenBytes(t, "baseline-topology-01", "pointers.payments-pointers-mid")

	if !bytes.Equal(bytes.TrimRight(end, "\n"), bytes.TrimRight(mid, "\n")) {
		t.Errorf("payments-pointers-end and -mid differ, but nothing in the baseline moved between " +
			"09:45 and 09:51:22; a pointer drifted in a graph that did not")
	}

	var got goldenPointers
	if err := json.Unmarshal(end, &got); err != nil {
		t.Fatalf("decode pointers.payments-pointers-end: %v", err)
	}
	// US5 scenario 1: at least a metric, a log and a trace selector, each naming its backend and
	// the vocabulary it is expressed in.
	for _, kind := range []string{"METRIC", "LOG", "TRACE"} {
		list, ok := got.ByKind[kind]
		if !ok || len(list.Pointers) == 0 {
			t.Errorf("no %s pointer on payments; US5 scenario 1 asks for one of each", kind)
			continue
		}
		for _, p := range list.Pointers {
			if p.BackendKind == "" || p.Vocabulary == "" {
				t.Errorf("%s pointer %+v names no backend or vocabulary (FR-008)", kind, p)
			}
		}
	}
}

// TestConfigChangeBlastRadius is the query `config-change-01` was recorded for: the ConfigMap
// edit's blast radius. checkout depends on the config, and storefront calls checkout, so a
// change to the one reaches the other — which is what "blast radius of a ConfigMap edit" means.
func TestConfigChangeBlastRadius(t *testing.T) {
	got := readImpactGolden(t, "config-change-01", "checkout-impact")

	down := make([]string, 0, len(got.Downstream))
	for _, item := range got.Downstream {
		down = append(down, item.Node.DisplayName)
	}
	if !slices.Contains(down, "storefront") {
		t.Errorf("storefront is not in checkout's blast radius; it holds %v", down)
	}

	up := make([]string, 0, len(got.Upstream))
	for _, item := range got.Upstream {
		up = append(up, item.Node.DisplayName)
	}
	if !slices.Contains(up, "checkout-config") {
		t.Errorf("checkout-config is not among checkout's dependencies; it holds %v", up)
	}
	assertImpactOrder(t, "downstream", got.Downstream)
	assertImpactOrder(t, "upstream", got.Upstream)
}

// TestRetractionHistoryShowsBothBeliefs is US7 on `retraction-with-edges-01`: legacy-cart has
// two versions, the superseded one open-ended in valid time and the current one bounded at the
// retraction instant. That pair *is* the bitemporal claim — production changed at 14:10 and the
// graph learned it at 14:11 — and no other query shows it.
func TestRetractionHistoryShowsBothBeliefs(t *testing.T) {
	got := readHistoryGolden(t, "retraction-with-edges-01", "legacy-cart-history")

	if len(got.Versions) != 2 {
		t.Fatalf("%d version(s), want 2", len(got.Versions))
	}
	first, second := got.Versions[0], got.Versions[1]
	if first.Valid.End != "" {
		t.Errorf("the superseded version was believed open-ended, got valid end %q", first.Valid.End)
	}
	if first.Observed.End == "" {
		t.Error("the superseded version's observed interval must be closed")
	}
	if second.Valid.End != "2026-09-01T14:10:00Z" {
		t.Errorf("current version valid end = %q, want 2026-09-01T14:10:00Z", second.Valid.End)
	}
	if second.Observed.End != "" {
		t.Errorf("the current version's observed interval must still be open, got %q", second.Observed.End)
	}
	if first.Observed.End != second.Observed.Start {
		t.Errorf("the observed intervals must abut: %q then %q", first.Observed.End, second.Observed.Start)
	}
}

// TestRolloutHistoryShowsTheVersionBump is US7 on `rollout-regression-01`: the 1.4.0 → 1.4.1
// rollout is two versions with observed boundaries, not one version that changed underneath.
func TestRolloutHistoryShowsTheVersionBump(t *testing.T) {
	got := readHistoryGolden(t, "rollout-regression-01", "payments-history")

	var versions []string
	current := 0
	for _, v := range got.Versions {
		if value, ok := v.Props["service.version"].(string); ok {
			versions = append(versions, value)
		}
		if v.Observed.End == "" {
			current++
		}
	}
	if !slices.Contains(versions, "1.4.0") || !slices.Contains(versions, "1.4.1") {
		t.Errorf("the history reports service.version %v, want both 1.4.0 and 1.4.1", versions)
	}
	if current == 0 {
		t.Error("no version has an open observed interval; something must be current")
	}
	// The merge absorbed the OpenTelemetry entity into the Kubernetes one, and its versions stay
	// under the id they were stored against — which is how a reader tells them apart (research §4).
	ids := map[string]bool{}
	for _, v := range got.Versions {
		ids[v.EntityID] = true
	}
	if len(ids) < 2 {
		t.Errorf("the history holds versions under %d entity id(s); the merged-away entity's "+
			"versions must be in it too", len(ids))
	}
}

// TestAmbiguousIdentityHistoryShowsTheMergeAndTheSplit is US6 seen from US7: checkout is merged
// with a Kubernetes deployment at 14:35 and split again at 15:00, so its facets gain WORKLOAD and
// lose it, and the decisions that did both are in the same answer (constitution VI, FR-032).
func TestAmbiguousIdentityHistoryShowsTheMergeAndTheSplit(t *testing.T) {
	got := readHistoryGolden(t, "ambiguous-identity-01", "checkout")

	var facetSets [][]string
	for _, v := range got.Versions {
		facetSets = append(facetSets, v.Facets)
	}
	if len(facetSets) < 3 {
		t.Fatalf("%d version(s), want at least three: before the merge, merged, and after the split",
			len(facetSets))
	}
	merged := false
	for _, facets := range facetSets {
		if slices.Contains(facets, "WORKLOAD") {
			merged = true
		}
	}
	if !merged {
		t.Errorf("no version carries the WORKLOAD facet; the merge is invisible: %v", facetSets)
	}
	if slices.Equal(facetSets[0], facetSets[len(facetSets)-1]) && merged {
		// The first and last are both SERVICE-only, which is right; what must differ is the
		// middle one. Checked explicitly so a history that never changed shape still fails.
		if slices.Equal(facetSets[0], facetSets[1]) {
			t.Errorf("the facets never change across the merge and split: %v", facetSets)
		}
	}

	var kinds []string
	for _, d := range got.Decisions {
		kinds = append(kinds, d.Kind)
		if d.Rationale == "" {
			t.Errorf("decision %s carries no rationale (constitution VI)", d.Kind)
		}
	}
	for _, want := range []string{"confirm", "split"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("the decisions are %v, want %s among them", kinds, want)
		}
	}
}
