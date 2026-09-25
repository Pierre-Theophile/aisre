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
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// Unit tests for the diff and its change candidates (T037, T038, T042), over graphs built here
// rather than loaded from a fixture. The fixture proves the whole story end to end; each of
// these proves one rule on the smallest graph that can show it, so a failure names the rule.
//
// The edge cases T042 calls for are the last four: a tie broken deterministically, an unattached
// change kept but ranked below an attached one, a change outside the window dropped, and a
// change whose target lies past the margin dropped.

// The window every test here diffs over: the graph is true from 13:00 and the question is asked
// at 14:32, the way US2 asks it.
var (
	diffFrom = baseValid
	diffTo   = queryAt
)

// changeNS is the namespace a change is named in, as the Kubernetes feeder names them.
const changeNS = "k8s.change"

// ---------- builders ----------

// props upserts a node with an explicit property set, valid from an instant. It is how a test
// makes a node's facts move without moving anything else.
func (b *builder) props(name string, validAt time.Time, fields map[string]any) {
	b.t.Helper()
	value, err := structpb.NewStruct(fields)
	if err != nil {
		b.t.Fatalf("props: %v", err)
	}
	b.apply(&graphv1.UpsertNode{
		Ref:         &graphv1.Ref{Namespace: testNS, Value: name},
		Type:        graphv1.NodeType_SERVICE,
		DisplayName: name,
		Props:       value,
		ValidAt:     timestamppb.New(validAt),
	}, time.Time{})
}

// change records a change against a node of the graph.
func (b *builder) change(id, target string, kind graphv1.ChangeKind, validAt time.Time) {
	b.t.Helper()
	b.observe(id, kind, validAt, &graphv1.Ref{Namespace: testNS, Value: target})
}

// danglingChange records a change against a name no source has ever described, which is what
// makes it unattached (edge case "dangling change").
func (b *builder) danglingChange(id, target string, validAt time.Time) {
	b.t.Helper()
	b.observe(id, graphv1.ChangeKind_ROLLOUT, validAt,
		&graphv1.Ref{Namespace: "k8s.deployment", Value: target})
}

func (b *builder) observe(id string, kind graphv1.ChangeKind, validAt time.Time, targets ...*graphv1.Ref) {
	b.t.Helper()
	b.apply(&graphv1.ObserveChange{
		Ref: &graphv1.Ref{Namespace: changeNS, Value: id},
		Change: &graphv1.Change{
			Kind:      kind,
			Summary:   id,
			Actor:     "deploy-bot",
			OriginRef: "https://ci.example/runs/" + id,
		},
		Targets: targets,
		ValidAt: timestamppb.New(validAt),
	}, time.Time{})
}

func (b *builder) retract(name string, validEnd time.Time) {
	b.t.Helper()
	b.apply(&graphv1.RetractNode{
		Ref:      &graphv1.Ref{Namespace: testNS, Value: name},
		ValidEnd: timestamppb.New(validEnd),
	}, time.Time{})
}

// ---------- helpers ----------

// diffAsk runs a diff of the focus's neighbourhood over the standard window, letting a test
// adjust the request before it is sent.
func diffAsk(t *testing.T, e *query.Engine, focusName string, hops uint32, adjust func(*graphv1.DiffRequest)) *graphv1.DiffResponse {
	t.Helper()
	req := &graphv1.DiffRequest{
		Subgraph: &graphv1.SubgraphRequest{Focus: focus(focusName), Hops: hops},
		T1:       timestamppb.New(diffFrom),
		T2:       timestamppb.New(diffTo),
	}
	if adjust != nil {
		adjust(req)
	}
	resp, err := e.Diff(context.Background(), req)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	return resp
}

// rankedIDs is the ranked changes by the name they were recorded under, in rank order.
func rankedIDs(resp *graphv1.DiffResponse) []string {
	out := make([]string, 0, len(resp.GetChanges()))
	for _, item := range resp.GetChanges() {
		out = append(out, changeRef(item.GetChange()))
	}
	return out
}

// changeRef is the `k8s.change=…` identifier a test names a change by.
func changeRef(node *graphv1.NodeVersion) string {
	for _, alias := range node.GetAliases() {
		if alias.GetNamespace() == changeNS {
			return alias.GetValue()
		}
	}
	return node.GetEntityId()
}

func nodeNames(nodes []*graphv1.NodeVersion) []string {
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node.GetDisplayName())
	}
	slices.Sort(out)
	return out
}

func changedNames(deltas []*graphv1.NodeDelta) []string {
	out := make([]string, 0, len(deltas))
	for _, delta := range deltas {
		out = append(out, delta.GetAfter().GetDisplayName())
	}
	slices.Sort(out)
	return out
}

func deltaOf(t *testing.T, deltas []*graphv1.PropertyDelta, key string) *graphv1.PropertyDelta {
	t.Helper()
	for _, delta := range deltas {
		if delta.GetKey() == key {
			return delta
		}
	}
	keys := make([]string, 0, len(deltas))
	for _, delta := range deltas {
		keys = append(keys, delta.GetKey())
	}
	t.Fatalf("no delta for %q; the deltas are %v", key, keys)
	return nil
}

// ---------- the diff itself ----------

// TestDiffReportsWhatMovedAndOnlyWhatMoved is US2 scenario 2: a property that changed is
// reported with its old and new value, a node that appeared and a node that was retracted are
// each on the right side, and the nodes that were stable throughout are on neither.
func TestDiffReportsWhatMovedAndOnlyWhatMoved(t *testing.T) {
	b := newBuilder(t)
	for _, name := range []string{"a", "b", "c", "d"} {
		b.node(name)
	}
	b.calls("a", "b", 4, time.Time{}, time.Time{})
	b.calls("a", "c", 3, time.Time{}, time.Time{})
	b.calls("b", "d", 2, time.Time{}, time.Time{})

	at := func(h, m int) time.Time { return time.Date(2026, 9, 1, h, m, 0, 0, time.UTC) }
	b.retract("c", at(14, 10))
	b.props("b", at(14, 20), map[string]any{"service.name": "b", "service.version": "2.0.0"})
	b.node("e")
	b.calls("b", "e", 1, at(14, 15), time.Time{})

	resp := diffAsk(t, b.engine(), "a", 2, nil)

	if got := nodeNames(resp.GetNodesAdded()); !slices.Equal(got, []string{"e"}) {
		t.Errorf("nodes added = %v, want [e]", got)
	}
	if got := nodeNames(resp.GetNodesRemoved()); !slices.Equal(got, []string{"c"}) {
		t.Errorf("nodes removed = %v, want [c]", got)
	}
	if got := changedNames(resp.GetNodesChanged()); !slices.Equal(got, []string{"b"}) {
		t.Errorf("nodes changed = %v, want [b]; a and d were stable", got)
	}

	delta := deltaOf(t, resp.GetNodesChanged()[0].GetDeltas(), "service.version")
	if delta.GetOld() != nil {
		t.Errorf("service.version old = %v, want unset: the property did not exist at 13:00", delta.GetOld())
	}
	if delta.GetNew().GetStringValue() != "2.0.0" {
		t.Errorf("service.version new = %v, want 2.0.0", delta.GetNew())
	}
}

// TestDiffReportsEdgesByTheirEndpointsAndType: an edge is the same relationship across the
// window even when its version changed, so a re-weighted call is *changed*, not removed and
// added (FR-027).
func TestDiffReportsEdgesByTheirEndpointsAndType(t *testing.T) {
	b := newBuilder(t)
	for _, name := range []string{"a", "b", "c"} {
		b.node(name)
	}
	b.calls("a", "b", 4, time.Time{}, time.Time{})
	b.calls("a", "c", 3, time.Time{}, time.Time{})

	at1420 := time.Date(2026, 9, 1, 14, 20, 0, 0, time.UTC)
	b.calls("a", "b", 2, at1420, time.Time{}) // traffic collapsed
	b.apply(&graphv1.RetractEdge{
		Src:      &graphv1.Ref{Namespace: testNS, Value: "a"},
		Dst:      &graphv1.Ref{Namespace: testNS, Value: "c"},
		Type:     graphv1.EdgeType_CALLS,
		ValidEnd: timestamppb.New(at1420),
	}, time.Time{})

	resp := diffAsk(t, b.engine(), "a", 1, nil)

	if len(resp.GetEdgesAdded()) != 0 {
		t.Errorf("edges added = %d, want 0", len(resp.GetEdgesAdded()))
	}
	if len(resp.GetEdgesRemoved()) != 1 {
		t.Fatalf("edges removed = %d, want 1 (a -> c)", len(resp.GetEdgesRemoved()))
	}
	if len(resp.GetEdgesChanged()) != 1 {
		t.Fatalf("edges changed = %d, want 1 (a -> b)", len(resp.GetEdgesChanged()))
	}
	changed := resp.GetEdgesChanged()[0]
	delta := deltaOf(t, changed.GetDeltas(), query.DeltaKeyWeightClass)
	if delta.GetOld().GetNumberValue() != 4 || delta.GetNew().GetNumberValue() != 2 {
		t.Errorf("weight_class delta = %v -> %v, want 4 -> 2", delta.GetOld(), delta.GetNew())
	}
	if changed.GetKey() == "" {
		t.Error("an edge delta must carry the key that identifies the relationship")
	}
}

// TestDiffRefusesAWindowThatIsNotOne: t1 must precede t2, and the refusal must be an argument
// error rather than an empty answer.
func TestDiffRefusesAWindowThatIsNotOne(t *testing.T) {
	b := newBuilder(t)
	b.chain()

	_, err := b.engine().Diff(context.Background(), &graphv1.DiffRequest{
		Subgraph: &graphv1.SubgraphRequest{Focus: focus("a")},
		T1:       timestamppb.New(diffTo),
		T2:       timestamppb.New(diffFrom),
	})
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("reversed window: code %v (%v), want InvalidArgument", got, err)
	}
}

// ---------- change candidates ----------

// TestDiffRanksTheClosestRecentChangeFirst is US2 scenario 1 in miniature: two rollouts, the
// nearer and fresher one first, with the rationale on the item.
func TestDiffRanksTheClosestRecentChangeFirst(t *testing.T) {
	b := newBuilder(t)
	b.chain() // a -> b -> c -> d, weights 4, 3, 1
	at := func(h, m int) time.Time { return time.Date(2026, 9, 1, h, m, 0, 0, time.UTC) }
	b.change("b-rollout", "b", graphv1.ChangeKind_ROLLOUT, at(14, 20))
	b.change("c-rollout", "c", graphv1.ChangeKind_ROLLOUT, at(13, 30))

	resp := diffAsk(t, b.engine(), "a", 2, nil)

	if got := rankedIDs(resp); !slices.Equal(got, []string{"b-rollout", "c-rollout"}) {
		t.Fatalf("ranked %v, want the closer and fresher change first", got)
	}
	first := resp.GetChanges()[0]
	if first.GetHopDistance() != 1 || first.GetTimeDistanceSeconds() != 720 {
		t.Errorf("rationale = hop %d, %ds; want hop 1, 720s",
			first.GetHopDistance(), first.GetTimeDistanceSeconds())
	}
	if len(first.GetTargetEntityIds()) != 1 {
		t.Errorf("target_entity_ids = %v, want the one node the rollout changed",
			first.GetTargetEntityIds())
	}
	if resp.GetRankingFormula() == "" {
		t.Error("a ranked answer must publish the formula it was ranked with (FR-028)")
	}
}

// TestDiffExcludesChangesOutsideTheWindow: a change before the window or after it is not part
// of the answer. The boundaries are inclusive in practice — a point change is stored over
// [t, t+1µs), so one stamped exactly at t1 still intersects `(t1, t2]` — which is the forgiving
// side to err on for a window an operator typed by hand (changes.go).
func TestDiffExcludesChangesOutsideTheWindow(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	at := func(h, m int) time.Time { return time.Date(2026, 9, 1, h, m, 0, 0, time.UTC) }
	b.change("before-window", "b", graphv1.ChangeKind_ROLLOUT, at(12, 30))
	b.change("at-t1", "b", graphv1.ChangeKind_ROLLOUT, diffFrom)
	b.change("inside", "b", graphv1.ChangeKind_ROLLOUT, at(14, 20))
	b.change("at-t2", "b", graphv1.ChangeKind_ROLLOUT, diffTo)
	b.change("after-window", "b", graphv1.ChangeKind_ROLLOUT, at(15, 10))

	got := rankedIDs(diffAsk(t, b.engine(), "a", 2, nil))
	slices.Sort(got)
	if !slices.Equal(got, []string{"at-t1", "at-t2", "inside"}) {
		t.Errorf("ranked %v, want the changes from t1 to t2 and neither of the two outside", got)
	}
}

// TestDiffExcludesChangesBeyondTheMargin: the change margin is one hop past the neighbourhood by
// default, and a change whose only target is further away belongs to someone else's incident
// (FR-027).
func TestDiffExcludesChangesBeyondTheMargin(t *testing.T) {
	b := newBuilder(t)
	b.chain() // a -> b -> c -> d
	at1420 := time.Date(2026, 9, 1, 14, 20, 0, 0, time.UTC)
	b.change("on-b", "b", graphv1.ChangeKind_ROLLOUT, at1420)
	b.change("on-c", "c", graphv1.ChangeKind_ROLLOUT, at1420)
	b.change("on-d", "d", graphv1.ChangeKind_ROLLOUT, at1420)
	e := b.engine()

	// One hop of neighbourhood plus the default one-hop margin reaches c and stops.
	got := rankedIDs(diffAsk(t, e, "a", 1, nil))
	slices.Sort(got)
	if !slices.Equal(got, []string{"on-b", "on-c"}) {
		t.Errorf("ranked %v with the default margin, want the changes within one extra hop", got)
	}

	// Widening the margin brings the far change in rather than changing its rank rule.
	wider := rankedIDs(diffAsk(t, e, "a", 1, func(req *graphv1.DiffRequest) {
		margin := uint32(2)
		req.ChangeHopMargin = &margin
	}))
	slices.Sort(wider)
	if !slices.Equal(wider, []string{"on-b", "on-c", "on-d"}) {
		t.Errorf("ranked %v with margin 2, want every change on the chain", wider)
	}
}

// TestDiffKeepsUnattachedChangesAtReducedRank is US2 scenario 3 and the second edge case T042
// names: a change the graph cannot place is listed, flagged, and below an attached change that
// happened at the same instant.
func TestDiffKeepsUnattachedChangesAtReducedRank(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	at1420 := time.Date(2026, 9, 1, 14, 20, 0, 0, time.UTC)
	b.change("attached", "b", graphv1.ChangeKind_ROLLOUT, at1420)
	b.danglingChange("dangling", "shop/nobody-knows", at1420)

	resp := diffAsk(t, b.engine(), "a", 2, nil)

	if got := rankedIDs(resp); !slices.Equal(got, []string{"attached", "dangling"}) {
		t.Fatalf("ranked %v, want the attached change first and the dangling one kept", got)
	}
	dangling := resp.GetChanges()[1]
	if !dangling.GetUnattached() {
		t.Error("a change whose target nothing describes must be flagged unattached")
	}
	if len(dangling.GetTargetEntityIds()) != 0 {
		t.Errorf("target_entity_ids = %v, want none: the target resolved to nothing",
			dangling.GetTargetEntityIds())
	}
	if dangling.GetScore() >= resp.GetChanges()[0].GetScore() {
		t.Errorf("unattached score %v is not below the attached %v",
			dangling.GetScore(), resp.GetChanges()[0].GetScore())
	}
}

// TestDiffTieBreakIsStableAcrossRuns is US2 scenario 4 against the engine rather than the
// scorer: two changes at the same instant on the same node cannot be separated by the score,
// and two runs of the same query must still agree.
func TestDiffTieBreakIsStableAcrossRuns(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	at1420 := time.Date(2026, 9, 1, 14, 20, 0, 0, time.UTC)
	b.change("twin-one", "b", graphv1.ChangeKind_ROLLOUT, at1420)
	b.change("twin-two", "b", graphv1.ChangeKind_ROLLOUT, at1420)
	e := b.engine()

	first := diffAsk(t, e, "a", 2, nil)
	second := diffAsk(t, e, "a", 2, nil)

	if len(first.GetChanges()) != 2 {
		t.Fatalf("ranked %d changes, want 2", len(first.GetChanges()))
	}
	if first.GetChanges()[0].GetScore() != first.GetChanges()[1].GetScore() {
		t.Fatalf("the twins should score identically, got %v and %v",
			first.GetChanges()[0].GetScore(), first.GetChanges()[1].GetScore())
	}
	if !slices.Equal(rankedIDs(first), rankedIDs(second)) {
		t.Errorf("two runs ordered the tie differently: %v then %v",
			rankedIDs(first), rankedIDs(second))
	}
	ids := []string{
		first.GetChanges()[0].GetChange().GetEntityId(),
		first.GetChanges()[1].GetChange().GetEntityId(),
	}
	if ids[0] > ids[1] {
		t.Errorf("tie order = %v, want the smaller change id first", ids)
	}
	for i, item := range first.GetChanges() {
		if item.GetTieBreak() != query.TieBreakChangeID {
			t.Errorf("item %d tie_break = %q, want %q", i, item.GetTieBreak(), query.TieBreakChangeID)
		}
	}
}

// TestDiffReferenceInstantMovesTheRanking: proximity is measured to the reference instant, not
// to the end of the window, which is how "rank these against when the alert fired" is asked.
// A change *after* the reference is as recent as it is possible to be — distance 0, not a
// negative one (research §9).
func TestDiffReferenceInstantMovesTheRanking(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	at := func(h, m int) time.Time { return time.Date(2026, 9, 1, h, m, 0, 0, time.UTC) }
	b.change("early", "b", graphv1.ChangeKind_ROLLOUT, at(13, 30))
	b.change("late", "b", graphv1.ChangeKind_ROLLOUT, at(14, 20))
	e := b.engine()

	distances := func(resp *graphv1.DiffResponse) map[string]int64 {
		out := map[string]int64{}
		for _, item := range resp.GetChanges() {
			out[changeRef(item.GetChange())] = item.GetTimeDistanceSeconds()
		}
		return out
	}

	// Against the end of the window: 14:32 - 13:30 = 3720 s and 14:32 - 14:20 = 720 s.
	against1432 := distances(diffAsk(t, e, "a", 2, nil))
	if against1432["early"] != 3720 || against1432["late"] != 720 {
		t.Errorf("distances against t2 = %v, want early 3720 and late 720", against1432)
	}
	if got := rankedIDs(diffAsk(t, e, "a", 2, nil)); !slices.Equal(got, []string{"late", "early"}) {
		t.Errorf("ranked %v against t2, want the later change first", got)
	}

	// Against 13:35: 13:35 - 13:30 = 300 s, and the 14:20 change has not happened yet.
	against1335 := distances(diffAsk(t, e, "a", 2, func(req *graphv1.DiffRequest) {
		req.ReferenceAt = timestamppb.New(at(13, 35))
	}))
	if against1335["early"] != 300 || against1335["late"] != 0 {
		t.Errorf("distances against 13:35 = %v, want early 300 and late 0", against1335)
	}
}

// ---------- the targets described (004 T156) ----------

// targetNamed is the change target the answer describes under an entity id, or nil.
func targetNamed(resp *graphv1.DiffResponse, entityID string) *graphv1.NodeVersion {
	for _, node := range resp.GetChangeTargets() {
		if node.GetEntityId() == entityID {
			return node
		}
	}
	return nil
}

// A change's target is usually the one thing in its window that did not change, so no delta
// describes it. The answer does, once, however many changes landed on it.
func TestDiffDescribesTheTargetsTheChangesLandedOn(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	at := func(h, m int) time.Time { return time.Date(2026, 9, 1, h, m, 0, 0, time.UTC) }
	b.change("c-rollout", "c", graphv1.ChangeKind_ROLLOUT, at(14, 20))
	b.change("c-config", "c", graphv1.ChangeKind_CONFIG_CHANGE, at(14, 0))

	resp := diffAsk(t, b.engine(), "a", 2, nil)

	if got := rankedIDs(resp); len(got) != 2 {
		t.Fatalf("ranked %v, want both changes to c", got)
	}
	if moved := len(resp.GetNodesAdded()) + len(resp.GetNodesRemoved()) + len(resp.GetNodesChanged()); moved != 0 {
		t.Fatalf("%d node delta(s): the test needs a target that did not change", moved)
	}
	targetID := resp.GetChanges()[0].GetTargetEntityIds()[0]
	if len(resp.GetChangeTargets()) != 1 {
		t.Fatalf("change_targets holds %d node(s), want c once: two changes landed on it",
			len(resp.GetChangeTargets()))
	}
	c := targetNamed(resp, targetID)
	if c == nil || c.GetDisplayName() != "c" {
		t.Fatalf("change_targets = %v, want c described under %s", resp.GetChangeTargets(), targetID)
	}
	if !slices.ContainsFunc(c.GetAliases(), func(r *graphv1.Ref) bool {
		return r.GetNamespace() == testNS && r.GetValue() == "c"
	}) {
		t.Errorf("c is described without the reference a read takes: aliases %v", c.GetAliases())
	}
}

// A target that no longer existed at t2 — retracted inside the window, after the change — is
// described as it was at t1 rather than dropped.
func TestDiffDescribesARetiredTargetAsItWasAtTheStart(t *testing.T) {
	b := newBuilder(t)
	b.chain()
	at := func(h, m int) time.Time { return time.Date(2026, 9, 1, h, m, 0, 0, time.UTC) }
	b.change("c-rollout", "c", graphv1.ChangeKind_ROLLOUT, at(14, 0))
	b.retract("c", at(14, 10))

	resp := diffAsk(t, b.engine(), "a", 2, nil)

	if len(resp.GetChanges()) != 1 {
		t.Fatalf("ranked %v, want the rollout of c", rankedIDs(resp))
	}
	targetID := resp.GetChanges()[0].GetTargetEntityIds()[0]
	if c := targetNamed(resp, targetID); c == nil || c.GetDisplayName() != "c" {
		t.Errorf("change_targets = %v, want the retired c described as it was at t1",
			resp.GetChangeTargets())
	}
}
