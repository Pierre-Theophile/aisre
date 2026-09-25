// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/fixture/fixturetest"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// US2, read off the recorded goldens of `rollout-regression-01` (T043, SC-005).
//
// The fixture is the scenario the product thesis is stated in: payments is rolled out at 14:20
// among four decoys, and the diff of checkout's 2-hop neighbourhood between 13:00 and 14:32 has
// to put the culprit first and say why. These assertions are what stop the goldens from being
// merely stable — a ranking that quietly inverted would still round-trip its own recording.
//
// Every number here is the published formula applied to this fixture by hand; the worked
// example in docs/schema/ranking.md carries the same arithmetic.

// goldenDiff is the part of a recorded DiffResponse these assertions read.
type goldenDiff struct {
	NodesAdded   []goldenNode `json:"nodesAdded"`
	NodesRemoved []goldenNode `json:"nodesRemoved"`
	NodesChanged []struct {
		EntityID string     `json:"entityId"`
		After    goldenNode `json:"after"`
		Deltas   []struct {
			Key string `json:"key"`
			Old any    `json:"old"`
			New any    `json:"new"`
		} `json:"deltas"`
	} `json:"nodesChanged"`
	EdgesAdded   []any `json:"edgesAdded"`
	EdgesRemoved []any `json:"edgesRemoved"`
	EdgesChanged []struct {
		Key    string `json:"key"`
		Before struct {
			WeightClass *uint32 `json:"weightClass"`
		} `json:"before"`
		After struct {
			WeightClass *uint32 `json:"weightClass"`
		} `json:"after"`
		Deltas []struct {
			Key string  `json:"key"`
			Old float64 `json:"old"`
			New float64 `json:"new"`
		} `json:"deltas"`
	} `json:"edgesChanged"`
	Changes []struct {
		Change              goldenNode `json:"change"`
		Score               float64    `json:"score"`
		Temporal            float64    `json:"temporal"`
		Topological         float64    `json:"topological"`
		Traffic             float64    `json:"traffic"`
		HopDistance         uint32     `json:"hopDistance"`
		TimeDistanceSeconds string     `json:"timeDistanceSeconds"`
		Unattached          bool       `json:"unattached"`
		TargetEntityIDs     []string   `json:"targetEntityIds"`
		TieBreak            string     `json:"tieBreak"`
	} `json:"changes"`
	RankingFormula string `json:"rankingFormula"`
}

// refs lists the ranked changes by the identifier the fixture names them with, in rank order.
func (g goldenDiff) refs() []string {
	out := make([]string, 0, len(g.Changes))
	for _, item := range g.Changes {
		name := item.Change.EntityID
		for _, alias := range item.Change.Aliases {
			if alias.Namespace == "k8s.change" {
				name = alias.Namespace + "=" + alias.Value
			}
		}
		out = append(out, name)
	}
	return out
}

func readDiffGolden(t *testing.T, fixtureID, name string) goldenDiff {
	t.Helper()
	path := filepath.Join(fixturesDir, fixtureID, "golden", "diff."+name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var out goldenDiff
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return out
}

// TestRolloutRegressionRanksTheCulpritFirst is US2 scenario 1: the payments rollout ranks first
// and its rationale states both its hop distance and its time distance.
//
//	temporal    = exp(-720/1800)      = 0.670320
//	topological = 1/(1+1)             = 0.5
//	traffic     = (1+3)/6             = 0.666667   (checkout -> payments is weight class 3)
//	score       = 0.5*0.670320046 + 0.35*0.5 + 0.15*0.666666667 = 0.610160
func TestRolloutRegressionRanksTheCulpritFirst(t *testing.T) {
	got := readDiffGolden(t, "rollout-regression-01", "checkout-diff")

	if len(got.Changes) != 4 {
		t.Fatalf("ranked %d changes (%v), want 4", len(got.Changes), got.refs())
	}
	culprit := got.Changes[0]
	if name := got.refs()[0]; name != "k8s.change=shop/payments@rev7" {
		t.Fatalf("rank 1 = %s, want the payments rollout", name)
	}
	if culprit.HopDistance != 1 {
		t.Errorf("hop_distance = %d, want 1", culprit.HopDistance)
	}
	if culprit.TimeDistanceSeconds != "720" {
		t.Errorf("time_distance_seconds = %s, want 720 (12 minutes before 14:32)",
			culprit.TimeDistanceSeconds)
	}
	closeTo(t, "culprit temporal", culprit.Temporal, 0.670320)
	closeTo(t, "culprit topological", culprit.Topological, 0.5)
	closeTo(t, "culprit traffic", culprit.Traffic, 0.666667)
	closeTo(t, "culprit score", culprit.Score, 0.610160)
	if culprit.Change.Type != "CHANGE" {
		t.Errorf("the ranked item's node type = %q, want CHANGE (FR-003)", culprit.Change.Type)
	}
	if len(culprit.TargetEntityIDs) != 1 {
		t.Errorf("target_entity_ids = %v, want the payments entity", culprit.TargetEntityIDs)
	}
	if !strings.Contains(got.RankingFormula, "0.50*temporal") {
		t.Errorf("the response must publish the formula it ranked with, got %q", got.RankingFormula)
	}
}

// TestRolloutRegressionOrdersTheDecoys is the rest of US2 scenario 1 and the whole of scenario
// 3: the recent scaling second, the change nobody can place third and flagged, the stale
// rollout last, and the rollout of an unrelated service not in the list at all.
//
//	storefront scaling  dt = 1020 s -> temporal 0.567414, h 1 -> 0.5, w 4 -> 0.833333
//	                    score = 0.283707 + 0.175 + 0.125          = 0.583707
//	checkout-worker     dt =  600 s -> temporal 0.716531, unattached h = 3+1 -> 0.2, w 0 -> 0.166667
//	                    score = 0.358266 + 0.07  + 0.025          = 0.453266
//	inventory rollout   dt = 4920 s -> temporal 0.065002, h 1 -> 0.5, w 3 -> 0.666667
//	                    score = 0.032501 + 0.175 + 0.1            = 0.307501
func TestRolloutRegressionOrdersTheDecoys(t *testing.T) {
	got := readDiffGolden(t, "rollout-regression-01", "checkout-diff")

	want := []string{
		"k8s.change=shop/payments@rev7",
		"k8s.change=shop/storefront@scale-1415",
		"k8s.change=shop/checkout-worker@rev12",
		"k8s.change=shop/inventory@rev8",
	}
	if !slices.Equal(got.refs(), want) {
		t.Fatalf("ranked %v,\nwant %v", got.refs(), want)
	}

	scaling := got.Changes[1]
	if scaling.HopDistance != 1 || scaling.TimeDistanceSeconds != "1020" {
		t.Errorf("scaling rationale = hop %d, %ss; want hop 1, 1020s",
			scaling.HopDistance, scaling.TimeDistanceSeconds)
	}
	closeTo(t, "scaling score", scaling.Score, 0.583707)
	closeTo(t, "scaling traffic", scaling.Traffic, 0.833333)

	// US2 scenario 3: the change whose target no source has ever described is in the list,
	// flagged, with no targets and below the attached changes it is fresher than.
	unattached := got.Changes[2]
	if !unattached.Unattached {
		t.Error("the checkout-worker rollout must be flagged unattached")
	}
	if len(unattached.TargetEntityIDs) != 0 {
		t.Errorf("target_entity_ids = %v, want none", unattached.TargetEntityIDs)
	}
	if unattached.HopDistance != 4 {
		t.Errorf("unattached hop_distance = %d, want hop_cap+1 = 4 (2 hops + 1 margin + 1)",
			unattached.HopDistance)
	}
	closeTo(t, "unattached score", unattached.Score, 0.453266)
	if unattached.Score >= got.Changes[0].Score {
		t.Errorf("the unattached change (%v) outranks the culprit (%v)",
			unattached.Score, got.Changes[0].Score)
	}
	// It is fresher than both attached changes above it and still below them, which is the
	// reduced rank FR-028 asks for rather than a coincidence of timing.
	if unattached.Temporal <= got.Changes[0].Temporal {
		t.Errorf("the unattached change should be the freshest of the three: %v vs %v",
			unattached.Temporal, got.Changes[0].Temporal)
	}

	closeTo(t, "inventory score", got.Changes[3].Score, 0.307501)

	// The reporting rollout is four hops from checkout, past the 2-hop neighbourhood and its
	// one-hop change margin, so it is excluded outright rather than ranked last.
	for _, name := range got.refs() {
		if strings.Contains(name, "reporting") {
			t.Errorf("the reporting rollout is beyond the change margin and must not be ranked: %v",
				got.refs())
		}
	}

	for i, item := range got.Changes {
		if item.TieBreak != "change_id" {
			t.Errorf("item %d tie_break = %q, want change_id (US2 scenario 4)", i, item.TieBreak)
		}
	}
}

// TestRolloutRegressionReportsTheVersionChangeAndNothingSpurious is US2 scenario 2: the
// payments version property is listed as changed with old and new values, and nothing that was
// stable across the window is listed as added or removed.
func TestRolloutRegressionReportsTheVersionChangeAndNothingSpurious(t *testing.T) {
	got := readDiffGolden(t, "rollout-regression-01", "checkout-diff")

	if len(got.NodesAdded) != 0 || len(got.NodesRemoved) != 0 {
		t.Errorf("nodes added = %d, removed = %d; the topology did not move in this window",
			len(got.NodesAdded), len(got.NodesRemoved))
	}
	if len(got.EdgesAdded) != 0 || len(got.EdgesRemoved) != 0 {
		t.Errorf("edges added = %d, removed = %d; no relationship moved in this window",
			len(got.EdgesAdded), len(got.EdgesRemoved))
	}
	assertPaymentsDBReweight(t, got)

	var changed []string
	for _, delta := range got.NodesChanged {
		changed = append(changed, delta.After.DisplayName)
	}
	slices.Sort(changed)
	if !slices.Equal(changed, []string{"inventory", "payments-v2", "storefront"}) {
		t.Fatalf("nodes changed = %v, want exactly the three the window touched", changed)
	}

	// The window holds two different kinds of movement on payments, and the diff must report
	// both: the rollout moved `service.version`, and the 14:00 rename moved the display name
	// and `service.name` (T069, US5 scenario 2). Neither is a change node, so the ranking above
	// is untouched by the rename — which is the point of asserting them here rather than there.
	want := map[string][2]any{
		"display_name":    {"payments", "payments-v2"},
		"service.name":    {"payments", "payments-v2"},
		"service.version": {"1.4.0", "1.4.1"},
	}
	for _, delta := range got.NodesChanged {
		if delta.After.DisplayName != paymentsAfterRename {
			continue
		}
		seen := map[string]bool{}
		for _, prop := range delta.Deltas {
			pair, interesting := want[prop.Key]
			if !interesting {
				continue
			}
			seen[prop.Key] = true
			if prop.Old != pair[0] || prop.New != pair[1] {
				t.Errorf("%s delta = %v -> %v, want %v -> %v", prop.Key, prop.Old, prop.New, pair[0], pair[1])
			}
		}
		for key := range want {
			if !seen[key] {
				t.Errorf("the payments delta does not report %s: %+v", key, delta.Deltas)
			}
		}
	}
}

// paymentsAfterRename is what payments is called at the end of this fixture's window. The node
// is renamed at 14:00 so that US5 scenario 2 has a fixture (T069); every assertion that finds
// payments by the name it ends up with goes through this constant, so the next rename is one
// line rather than a hunt.
const paymentsAfterRename = "payments-v2"

// assertPaymentsDBReweight is the edge-absorb regression, read off a recorded diff (research §4,
// "Implementation notes recorded after Phase 2").
//
// `payments → payments-db` is asserted at weight class 3 before the claim that merges the
// Kubernetes workload with the OpenTelemetry service, and at class 4 after it. The projector used
// to store the second assertion under the survivor and leave the first under the id that was
// merged away; the exclusion constraint keys on the raw endpoints, so it allowed two rows that
// were both valid at 14:32, and the diff then picked one of them by a tie-break instead of
// reporting a change. Exactly one entry, with the class moving 3 → 4 on the relationship out of
// the node the window's culprit rolled out, is that fix seen from the outside.
func assertPaymentsDBReweight(t *testing.T, got goldenDiff) {
	t.Helper()
	if len(got.EdgesChanged) != 1 {
		t.Fatalf("edges changed = %d, want exactly the payments -> payments-db re-weight",
			len(got.EdgesChanged))
	}
	changed := got.EdgesChanged[0]

	var payments string
	for _, delta := range got.NodesChanged {
		if delta.After.DisplayName == paymentsAfterRename {
			payments = delta.EntityID
		}
	}
	if payments == "" {
		t.Fatal("the diff does not report payments as changed, so the edge cannot be tied to it")
	}
	if want := payments + "|"; !strings.HasPrefix(changed.Key, want) || !strings.HasSuffix(changed.Key, "|calls") {
		t.Errorf("edge delta key = %q, want a calls relationship out of payments (%s)", changed.Key, payments)
	}
	if changed.Before.WeightClass == nil || *changed.Before.WeightClass != 3 ||
		changed.After.WeightClass == nil || *changed.After.WeightClass != 4 {
		t.Errorf("weight class %v -> %v, want 3 -> 4",
			changed.Before.WeightClass, changed.After.WeightClass)
	}
	if len(changed.Deltas) != 1 || changed.Deltas[0].Key != query.DeltaKeyWeightClass {
		t.Fatalf("deltas = %+v, want exactly the %s pseudo-key", changed.Deltas, query.DeltaKeyWeightClass)
	}
	if changed.Deltas[0].Old != 3 || changed.Deltas[0].New != 4 {
		t.Errorf("%s delta = %v -> %v, want 3 -> 4",
			query.DeltaKeyWeightClass, changed.Deltas[0].Old, changed.Deltas[0].New)
	}
}

// TestRolloutRegressionWindowExcludesOlderChanges: the one-hour diff starts at 13:32, so the
// 13:10 inventory rollout and the property change it caused are both outside it. The 14:20
// re-weight is inside it, so it is reported in both windows.
func TestRolloutRegressionWindowExcludesOlderChanges(t *testing.T) {
	got := readDiffGolden(t, "rollout-regression-01", "checkout-diff-1h")
	assertPaymentsDBReweight(t, got)

	for _, name := range got.refs() {
		if strings.Contains(name, "inventory") {
			t.Errorf("the 13:10 inventory rollout is before 13:32 and must not be ranked: %v", got.refs())
		}
	}
	if len(got.Changes) != 3 {
		t.Errorf("ranked %d changes (%v), want 3", len(got.Changes), got.refs())
	}
	for _, delta := range got.NodesChanged {
		if delta.After.DisplayName == "inventory" {
			t.Error("inventory did not change between 13:32 and 14:32")
		}
	}
}

// TestRolloutRegressionReportMeasuresTheCulprit is SC-005 through the harness rather than the
// goldens: `fixture verify --report` must say the culprit ranked first out of four.
// TestRecordedRolloutRanksTheCulpritFirst is SC-005 on data nobody designed.
//
// rollout-regression-01 is a scenario built to be ranked well: four decoys at chosen hops and
// chosen ages. rollout-regression-02 is a kind cluster where `scenarios/rollout.sh` rolled
// shop/payments and nothing else was touched, so the ranked list is whatever two real feeders
// happened to mint. That makes it a weaker test of the *ordering* — there is only one real
// culprit and one echo of it — and a much stronger test of everything upstream: that a rollout
// on a real cluster becomes a change candidate at all, attached to the right target, at the
// right hop distance, with a temporal component computed from times a real graph assigned.
//
// The second entry is the same rollout seen by the other feeder: the OpenTelemetry topology
// feeder reports the `service.version` transition it observed in spans. It ranks below the
// Kubernetes one because its valid time is the start of the aggregation window that saw the
// new version, which is older than the instant the API server recorded the new revision.
func TestRecordedRolloutRanksTheCulpritFirst(t *testing.T) {
	got := readDiffGolden(t, "rollout-regression-02", "checkout-diff")

	if len(got.Changes) == 0 {
		t.Fatal("the recorded rollout produced no ranked change candidates")
	}
	first := got.Changes[0]
	if !slices.Contains(refsOf(first.Change), "k8s.change=shop/payments@rev2") {
		t.Errorf("rank 1 is %v, want the payments rollout the scenario performed", refsOf(first.Change))
	}
	if first.HopDistance != 1 {
		t.Errorf("the culprit is %d hops from checkout, want 1 (checkout -> payments)", first.HopDistance)
	}
	if first.Unattached {
		t.Error("the culprit targets shop/payments, which the graph holds; it is not unattached")
	}
	for i, item := range got.Changes[1:] {
		if item.Score > first.Score {
			t.Errorf("change %d scores %g, above the culprit's %g; the list is not in rank order",
				i+2, item.Score, first.Score)
		}
	}

	// The rollout is visible as a changed node too, not only as a ranked candidate: both the
	// Kubernetes revision and the service.version the emitters report moved.
	var deltas []string
	for _, changed := range got.NodesChanged {
		if changed.After.DisplayName != "payments" {
			continue
		}
		for _, d := range changed.Deltas {
			deltas = append(deltas, d.Key)
		}
	}
	for _, want := range []string{"sre.k8s.revision", "service.version"} {
		if !slices.Contains(deltas, want) {
			t.Errorf("payments changed in %v, want %s among them", deltas, want)
		}
	}
}

// refsOf names a change the way a manifest's ground truth does.
func refsOf(n goldenNode) []string {
	out := []string{n.EntityID}
	for _, alias := range n.Aliases {
		out = append(out, alias.Namespace+"="+alias.Value)
	}
	return out
}

func TestRolloutRegressionReportMeasuresTheCulprit(t *testing.T) {
	ctx := context.Background()
	report, err := fixture.Verify(ctx, fixturetest.PgtestFactory(t),
		filepath.Join(fixturesDir, "rollout-regression-01"),
		fixture.VerifyOptions{Shuffles: -1, Report: true, NewRunner: newRunner})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Passed {
		t.Fatalf("the fixture did not verify: %s", report.Markdown())
	}

	ranking, ok := report.Metrics["ranking"].(map[string]any)
	if !ok {
		t.Fatalf("metrics.ranking = %v, want the per-query ranking measurements", report.Metrics["ranking"])
	}
	metrics, ok := ranking["checkout-diff"].(map[string]any)
	if !ok {
		t.Fatalf("no ranking metrics for checkout-diff, got %v", ranking)
	}
	if rank, _ := metrics["rank"].(int); rank != 1 {
		t.Errorf("culprit rank = %v, want 1 (SC-005)", metrics["rank"])
	}
	if hit, _ := metrics["top_k_hit"].(bool); !hit {
		t.Errorf("top_k_hit = %v, want true", metrics["top_k_hit"])
	}
	if candidates, _ := metrics["candidates"].(int); candidates != 4 {
		t.Errorf("candidates = %v, want 4", metrics["candidates"])
	}
	if !strings.Contains(report.Markdown(), "### Ranking") {
		t.Errorf("the report should carry a Ranking section:\n%s", report.Markdown())
	}
}

// closeTo compares a published score component, which is rounded to six decimals.
func closeTo(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 5e-7 {
		t.Errorf("%s = %.9f, want %.6f", what, got, want)
	}
}
