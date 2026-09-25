// SPDX-License-Identifier: Apache-2.0

package query_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Unit tests for the node history (T070). The rules worth pinning are the ones every other
// query deliberately hides: superseded versions are in the answer, the versions of an entity
// that was merged away are too, and the decisions that did the merging come with them.

func askHistory(t *testing.T, b *builder, ref *graphv1.Ref) *graphv1.NodeHistoryResponse {
	t.Helper()
	resp, err := b.engine().NodeHistory(context.Background(), &graphv1.NodeHistoryRequest{Focus: ref})
	if err != nil {
		t.Fatalf("NodeHistory: %v", err)
	}
	return resp
}

// TestHistoryShowsSupersededVersions: a correction closes the observed interval of the old
// version and opens a new one; both are in the history, oldest knowledge first (FR-032,
// constitution II).
func TestHistoryShowsSupersededVersions(t *testing.T) {
	b := newBuilder(t)
	b.props("payments", baseValid, map[string]any{"service.version": "1.4.0"})
	b.props("payments", baseValid, map[string]any{"service.version": "1.4.1"})

	resp := askHistory(t, b, focus("payments"))
	if len(resp.GetVersions()) != 2 {
		t.Fatalf("%d version(s), want 2: a correction never overwrites", len(resp.GetVersions()))
	}

	first, second := resp.GetVersions()[0], resp.GetVersions()[1]
	if first.GetObserved().GetEnd() == nil {
		t.Error("the first version's observed interval must be closed: something superseded it")
	}
	if second.GetObserved().GetEnd() != nil {
		t.Error("the last version's observed interval must still be open: it is what the graph believes")
	}
	if !first.GetObserved().GetEnd().AsTime().Equal(second.GetObserved().GetStart().AsTime()) {
		t.Errorf("the two observed intervals must abut, got %v and %v",
			first.GetObserved().GetEnd().AsTime(), second.GetObserved().GetStart().AsTime())
	}
	if got := first.GetProps().GetFields()["service.version"].GetStringValue(); got != "1.4.0" {
		t.Errorf("first version service.version = %q, want 1.4.0", got)
	}
	if got := second.GetProps().GetFields()["service.version"].GetStringValue(); got != "1.4.1" {
		t.Errorf("second version service.version = %q, want 1.4.1", got)
	}
	for _, version := range resp.GetVersions() {
		if len(version.GetProvenance().GetProducedByEventIds()) == 0 {
			t.Errorf("version %s carries no provenance (FR-034)", version.GetVersionId())
		}
	}
}

// TestHistoryIsNotPinnedByObservedTime: the retraction case. legacy-cart stops being true at
// 14:10; the history holds the unbounded version the graph believed until then and the bounded
// one it believes now, and nothing about the query hides either.
func TestHistoryShowsARetraction(t *testing.T) {
	retractedAt := time.Date(2026, 9, 1, 14, 10, 0, 0, time.UTC)

	b := newBuilder(t)
	b.node("legacy-cart")
	b.apply(&graphv1.RetractNode{
		Ref:      &graphv1.Ref{Namespace: testNS, Value: "legacy-cart"},
		ValidEnd: timestamppb.New(retractedAt),
	}, time.Time{})

	resp := askHistory(t, b, focus("legacy-cart"))
	if len(resp.GetVersions()) != 2 {
		t.Fatalf("%d version(s), want 2: the retraction closes the valid interval of a new version",
			len(resp.GetVersions()))
	}
	if end := resp.GetVersions()[0].GetValid().GetEnd(); end != nil {
		t.Errorf("the superseded version was believed to be open-ended, got valid end %v", end.AsTime())
	}
	end := resp.GetVersions()[1].GetValid().GetEnd()
	if end == nil || !end.AsTime().Equal(retractedAt) {
		t.Errorf("the current version must end at the retraction instant, got %v", end)
	}
}

// TestHistoryKeepsTheVersionsOfAMergedAwayEntity: a merge does not move the rows it absorbed, so
// the history of the survivor includes them, under the entity id they are stored against — which
// is how a reader tells them apart — and the decisions that merged them.
func TestHistoryKeepsTheVersionsOfAMergedAwayEntity(t *testing.T) {
	b := newBuilder(t)
	b.node("checkout")

	k8s := b.source("k8s:test", "k8s")
	k8s.apply(&graphv1.UpsertNode{
		Ref:         &graphv1.Ref{Namespace: "k8s.deployment", Value: "shop/checkout"},
		Type:        graphv1.NodeType_WORKLOAD,
		DisplayName: "checkout",
		ValidAt:     timestamppb.New(baseValid),
	}, time.Time{})
	k8s.claim(&graphv1.Ref{Namespace: "k8s.deployment", Value: "shop/checkout"},
		&graphv1.Ref{Namespace: testNS, Value: "checkout"})
	b.claim(&graphv1.Ref{Namespace: testNS, Value: "checkout"},
		&graphv1.Ref{Namespace: testNS, Value: "checkout"})

	resp := askHistory(t, b, focus("checkout"))

	ids := map[string]bool{}
	for _, version := range resp.GetVersions() {
		ids[version.GetEntityId()] = true
	}
	if len(ids) < 2 {
		t.Fatalf("the history holds versions under %d entity id(s); a merge leaves the absorbed "+
			"entity's rows under their own id (research §4)", len(ids))
	}
	if len(resp.GetDecisions()) == 0 {
		t.Error("a merged node's history must carry the decisions that merged it (constitution VI)")
	}
	for _, d := range resp.GetDecisions() {
		if d.GetRationale() == "" {
			t.Errorf("decision %s has no rationale", d.GetDecisionId())
		}
	}

	// Both names reach the same history: the reference is resolved through `merged_into`.
	byDeployment := askHistory(t, b, &graphv1.Ref{Namespace: "k8s.deployment", Value: "shop/checkout"})
	if len(byDeployment.GetVersions()) != len(resp.GetVersions()) {
		t.Errorf("history by deployment name has %d versions, by service name %d; either alias "+
			"must reach the survivor", len(byDeployment.GetVersions()), len(resp.GetVersions()))
	}
}

// TestHistoryOrdersByObservedThenValid: the list is read down the page as the graph learned
// things. The order is part of the contract, because a golden is compared byte for byte.
func TestHistoryOrdersByObservedThenValid(t *testing.T) {
	b := newBuilder(t)
	b.props("payments", baseValid, map[string]any{"service.version": "1.4.0"})
	b.props("payments", queryAt, map[string]any{"service.version": "1.4.1"})
	b.props("payments", baseValid, map[string]any{"service.version": "1.4.0-hotfix"})

	resp := askHistory(t, b, focus("payments"))
	observed := make([]time.Time, 0, len(resp.GetVersions()))
	for _, version := range resp.GetVersions() {
		observed = append(observed, version.GetObserved().GetStart().AsTime())
	}
	if !slices.IsSortedFunc(observed, func(a, b time.Time) int { return a.Compare(b) }) {
		t.Errorf("observed starts are not in order: %v", observed)
	}
}

// TestHistoryOfAnUnknownNodeIsNotFound: a name the graph has never heard of is NOT_FOUND, not an
// empty history (FR-039).
func TestHistoryOfAnUnknownNodeIsNotFound(t *testing.T) {
	b := newBuilder(t)
	b.node("a")

	_, err := b.engine().NodeHistory(context.Background(), &graphv1.NodeHistoryRequest{
		Focus: focus("nothing-here"),
	})
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("history of an unknown node: code %v (%v), want NotFound", got, err)
	}
}

// The aliases come back in byte order, whatever collation the database was created with.
//
// This is the defect CI caught and a C.UTF-8 development database could not: the order was the
// database's `ORDER BY`, and that sorts under the *cluster's* locale. Under C, `TWIN-2026-1002`
// precedes `inference-api@…` because T is 0x54 and i is 0x69; under en_US or any ICU locale, case
// and punctuation fold and the order reverses. Every golden recording an alias list therefore
// encoded the locale of the machine that recorded it, and six fixtures recorded on one developer's
// database failed on CI's — with a byte-level diff and no hint of why.
//
// The identifiers here are chosen so the two orders disagree: on a C database this test passes
// either way, and on an en_US or ICU one it fails the moment the sort goes back to the database.
func TestAliasesComeBackInByteOrderWhateverTheDatabasesCollation(t *testing.T) {
	b := newBuilder(t)
	b.node("payments")
	// Deliberately mixed case with punctuation, which is where collations disagree.
	for _, value := range []string{
		"acme/inference-api@2026-10-02",
		"acme/TWIN-2026-1002",
		"acme/Training",
		"acme/billing",
	} {
		b.claim(focus("payments"), &graphv1.Ref{Namespace: "vendor.notice", Value: value})
	}

	resp := askHistory(t, b, focus("payments"))
	if len(resp.GetVersions()) == 0 {
		t.Fatal("no versions, so this test asserts nothing")
	}
	var got []string
	for _, alias := range resp.GetVersions()[0].GetAliases() {
		if alias.GetNamespace() == "vendor.notice" {
			got = append(got, alias.GetValue())
		}
	}
	want := []string{
		// Byte order: uppercase before lowercase.
		"acme/TWIN-2026-1002",
		"acme/Training",
		"acme/billing",
		"acme/inference-api@2026-10-02",
	}
	if !slices.Equal(got, want) {
		t.Errorf("aliases came back as\n  %v\nwant byte order\n  %v\n\nThe order is the database's "+
			"collation rather than this code's, so every golden holding an alias list now records "+
			"the locale of the machine that wrote it", got, want)
	}
}
