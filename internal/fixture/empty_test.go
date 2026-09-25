// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"errors"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Refusing a golden that asserts nothing (004 T151).
//
// These tests are in `package fixture` rather than `fixture_test` because `checkGolden` and
// `emptyAnswer` are unexported: the gate is not API, it is a rule the recorder applies, and exporting
// it so a test could reach it would make it something a caller could route around.

// A response with nothing but its metadata is refused, and the refusal says what a reader will see.
func TestAnEmptyAnswerIsRefused(t *testing.T) {
	t.Parallel()
	// The exact shape of the eight vacuous deploy goldens T150 found: an extent, a truncation, and
	// nothing else.
	got := &graphv1.SubgraphResponse{
		Extent:     &graphv1.Extent{},
		Truncation: &graphv1.Truncation{PerHopCap: 50, TotalCap: 500},
	}
	err := checkGolden(&Manifest{ID: "github-deployment-01"},
		Query{Kind: "subgraph", Name: "rollout-2hop"}, got)
	if err == nil {
		t.Fatal("a response holding only an extent and a truncation was accepted. That is the exact " +
			"shape of the goldens that made the acceptance evidence for two user stories assert " +
			"nothing (T150), and it is what this gate exists to refuse")
	}
	var empty *EmptyGoldenError
	if !errors.As(err, &empty) {
		t.Fatalf("the refusal is %T, want *EmptyGoldenError: the fix is never a retry, it is a human "+
			"deciding whether the query is wrong or the emptiness is the point", err)
	}
	if empty.What != "no nodes and no edges" {
		t.Errorf("the refusal describes the file as %q; a reader should recognise it without opening "+
			"it", empty.What)
	}
	if !strings.Contains(err.Error(), "github-deployment-01") ||
		!strings.Contains(err.Error(), "subgraph.rollout-2hop") {
		t.Errorf("the refusal does not name the fixture and the query: %v", err)
	}
}

// The extent and the truncation are NOT content, and that is the whole hinge of the gate.
//
// Both are present on every response whether or not anything was found, so counting either as content
// would make every answer non-empty and this file a no-op that looked like a guard. It is worth its
// own test because the mistake is invisible: the gate would still compile, still run, and never fire.
func TestTheExtentAndTruncationDoNotCountAsContent(t *testing.T) {
	t.Parallel()
	for name, got := range map[string]*graphv1.SubgraphResponse{
		"extent only":     {Extent: &graphv1.Extent{}},
		"truncation only": {Truncation: &graphv1.Truncation{TotalCap: 500}},
		"both":            {Extent: &graphv1.Extent{}, Truncation: &graphv1.Truncation{TotalCap: 500}},
		"neither":         {},
	} {
		if _, empty := emptyAnswer(got); !empty {
			t.Errorf("%s: counted as content. Every response carries these whether or not anything was "+
				"found, so counting them makes this gate a no-op that still looks like a guard", name)
		}
	}
}

// One node is content. A gate that refused a real answer would be worse than none, because the fix
// for a false refusal is to stop using the gate.
func TestOneNodeIsContent(t *testing.T) {
	t.Parallel()
	got := &graphv1.SubgraphResponse{
		Extent:     &graphv1.Extent{},
		Truncation: &graphv1.Truncation{TotalCap: 500},
		Nodes:      []*graphv1.NodeVersion{{EntityId: "e1"}},
	}
	if what, empty := emptyAnswer(got); empty {
		t.Errorf("a response holding one node was called empty (%q)", what)
	}
	if err := checkGolden(&Manifest{ID: "f"}, Query{Kind: "subgraph", Name: "q"}, got); err != nil {
		t.Errorf("a response holding one node was refused: %v", err)
	}
}

// A declared empty answer is allowed, and the declaration is a REASON rather than a boolean.
//
// `baseline-topology-01` asks what the graph held before it held anything, and nothing is the right
// answer. A bool would let an author silence this gate with one character; a sentence has to be
// written down and read by a reviewer in a diff.
func TestADeclaredEmptyAnswerIsAllowed(t *testing.T) {
	t.Parallel()
	got := &graphv1.SubgraphResponse{Extent: &graphv1.Extent{}}
	q := Query{
		Kind: "subgraph", Name: "before-history",
		ExpectEmpty: "asked before the first observation, so the graph was watching nothing yet",
	}
	if err := checkGolden(&Manifest{ID: "baseline-topology-01"}, q, got); err != nil {
		t.Errorf("a declared empty answer was refused: %v", err)
	}
	// Whitespace is not a declaration: a reason has to say something.
	q.ExpectEmpty = "   "
	if err := checkGolden(&Manifest{ID: "baseline-topology-01"}, q, got); err == nil {
		t.Error("a blank `expect_empty` silenced the gate; the value is the reason a reviewer checks, " +
			"and whitespace is not a reason")
	}
}

// The refusal names which PASS produced it, because the explanation differs between the two and
// finding out which costs a reader minutes otherwise.
func TestTheRefusalNamesWhichPassWasEmpty(t *testing.T) {
	t.Parallel()
	got := &graphv1.DiffResponse{Truncation: &graphv1.Truncation{}}
	normal := checkGolden(&Manifest{ID: "f"}, Query{Kind: "diff", Name: "q"}, got)
	pinned := checkGolden(&Manifest{ID: "f"}, Query{Kind: "diff", Name: "q", Pinned: true}, got)
	if normal == nil || pinned == nil {
		t.Fatal("an empty diff was accepted on one of the passes")
	}
	if !strings.Contains(normal.Error(), "the normal pass") {
		t.Errorf("the normal pass's refusal does not say so: %v", normal)
	}
	if !strings.Contains(pinned.Error(), "observed-time-pinned pass") {
		t.Errorf("the pinned pass's refusal does not say so: %v", pinned)
	}
	if !strings.Contains(pinned.Error(), "clock.end") {
		t.Errorf("the pinned refusal does not explain that observed time was overridden, which is the "+
			"usual cause: %v", pinned)
	}
}

// A kind this file has never heard of is still checked.
//
// The gate asks the message whether it holds anything rather than switching on the query kind, so a
// kind added next year is covered without anybody remembering to add it here. A gate that had to be
// told about each kind is a gate that silently stops covering the newest one.
func TestAnUnknownResponseKindIsStillChecked(t *testing.T) {
	t.Parallel()
	// ResolutionAuditResponse is not in describeEmpty's switch.
	empty := &graphv1.ResolutionAuditResponse{}
	what, isEmpty := emptyAnswer(empty)
	if !isEmpty {
		t.Fatal("a kind this file does not name was treated as content")
	}
	if what != "nothing but the extent and the truncation" {
		t.Errorf("the fallback description is %q", what)
	}
	populated := &graphv1.ResolutionAuditResponse{Decisions: []*graphv1.ResolutionDecision{{}}}
	if _, isEmpty := emptyAnswer(populated); isEmpty {
		t.Error("a kind this file does not name was called empty although it holds a decision")
	}
}
