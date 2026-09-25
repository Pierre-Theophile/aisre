// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Correlation keys through a real graph (004 T148).
//
// The rule-level behaviour is in internal/resolution; what these tests cover is the three things only a
// database can show: that a merge moves the keys onto the survivor, that a redelivery after a merge is
// still a no-op, and that the resolution audit reports the evidence a C8 merge actually stood on.
//
// The third is not decoration. C8's `supporting_claim_ids` are CORRELATION ids, so an audit that read
// only `graph.identity_claims` would answer a merge it had just made with an empty evidence list — a
// published conclusion nobody can check, which is what FR-031 exists to prevent.

// correlationRows returns every correlation key on an entity, as `namespace=value` strings.
func correlationRows(t *testing.T, store *postgres.Store, entityID string) []string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `
		SELECT namespace || '=' || value FROM graph.correlation_keys
		WHERE entity_id = $1 ORDER BY 1`, entityID)
	if err != nil {
		t.Fatalf("read correlations of %s: %v", entityID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// correlationCount is how many rows the table holds in total, which is what says a re-point moved rows
// rather than copying them.
func correlationCount(t *testing.T, store *postgres.Store) int {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM graph.correlation_keys`).Scan(&n); err != nil {
		t.Fatalf("count correlations: %v", err)
	}
	return n
}

// survivorOf returns the entity a C8 merge kept, and the one it absorbed.
func survivorOf(t *testing.T, store *postgres.Store) (survivor, merged string) {
	t.Helper()
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT surviving_id, merged_id FROM graph.resolution_decisions
		WHERE rule_id = 'C8' AND superseded_by IS NULL
		ORDER BY decided_at DESC LIMIT 1`).Scan(&survivor, &merged); err != nil {
		t.Fatalf("no C8 decision to read: %v", err)
	}
	return survivor, merged
}

// A merge moves the absorbed entity's correlation keys onto the survivor.
//
// Without this the keys stay on an entity that no longer exists as far as every query is concerned, and
// `CorrelatedWith` goes on returning it as a candidate — so C8 would keep proposing a merge it has
// already made, on every later event that touched the value.
func TestAMergeRePointsTheCorrelationKeys(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	applyInOrder(t, p, c8Events(), []int{0, 1, 2, 3, 4, 5})
	survivor, merged := survivorOf(t, store)

	if got := correlationRows(t, store, merged); len(got) != 0 {
		t.Errorf("the absorbed entity %s still carries %v; a query would keep offering it as a "+
			"correlation candidate for a merge that has already happened", merged, got)
	}
	// Both sources' keys, on the survivor. Two rows and not one: the entity is one, but which SOURCE
	// said what is evidence, and the audit prints it.
	got := correlationRows(t, store, survivor)
	if len(got) != 2 {
		t.Errorf("the survivor %s carries %v, want one commit key from each source; a re-point that "+
			"collapsed them would lose which source corroborated", survivor, got)
	}
	for _, row := range got {
		if row != "deploy.commit_sha="+retriggerCommit {
			t.Errorf("the survivor carries %q, want the commit both sources stated", row)
		}
	}
	if n := correlationCount(t, store); n != 2 {
		t.Errorf("the table holds %d rows, want the same 2 moved rather than copied", n)
	}
}

// And a redelivery after that merge is still a no-op.
//
// What this asserts is the property, not the mechanism: re-delivering the two deploy keys after their
// changes have been merged adds no row, loses no row, and reports DUPLICATE_NOOP. It holds today
// because the log short-circuits on the idempotency key before the body is applied at all, which is
// why it passes with either conflict target on the insert — see storeCorrelation's note on why the
// natural key is still the one used.
func TestARedeliveryAfterAMergeIsStillANoop(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	events := c8Events()
	applyInOrder(t, p, events, []int{0, 1, 2, 3, 4, 5})
	survivor, _ := survivorOf(t, store)
	before := correlationCount(t, store)

	// The two deploy keys again, exactly as first delivered.
	for _, i := range []int{4, 5} {
		if got := apply(t, p, events[i], "2026-09-21T15:00:00Z"); got != graphv1.IngestResult_DUPLICATE_NOOP {
			t.Errorf("re-delivering %s returned %v, want DUPLICATE_NOOP", events[i].GetEventId(), got)
		}
	}
	if n := correlationCount(t, store); n != before {
		t.Errorf("the table grew from %d to %d rows on re-delivery", before, n)
	}
	if got := correlationRows(t, store, survivor); len(got) != 2 {
		t.Errorf("the survivor carries %v after re-delivery, want the same two keys", got)
	}
}

// The resolution audit reports the correlation keys a C8 merge stood on (FR-031, 004 T148).
//
// This is the one that would have gone unnoticed. C8's `supporting_claim_ids` are CORRELATION ids, and
// the audit read only `graph.identity_claims` — so the query whose entire job is to show why two things
// were merged answered a C8 merge with an evidence list that did not contain its evidence. Nothing
// errored; the field was simply empty, which is how a published conclusion becomes unfalsifiable.
func TestTheAuditReportsTheCorrelationKeysAC8MergeStoodOn(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	applyInOrder(t, p, c8Events(), []int{0, 1, 2, 3, 4, 5})

	engine := query.NewEngine(store)
	resp, err := engine.ResolutionAudit(context.Background(), &graphv1.ResolutionAuditRequest{
		A: &graphv1.Ref{Namespace: "github.change", Value: "gh/deploy/1"},
		B: &graphv1.Ref{Namespace: "gcp.change", Value: "rollout/storefront-00042"},
	})
	if err != nil {
		t.Fatalf("ResolutionAudit: %v", err)
	}
	if !resp.GetSameEntity() {
		t.Fatalf("the audit says the two rollouts are not one entity, but C8 merged them; the rest of "+
			"this test would be asserting against the wrong graph. decisions: %d", len(resp.GetDecisions()))
	}

	// The decision, and the ids it cites.
	var cited []string
	for _, d := range resp.GetDecisions() {
		if d.GetRuleId() == "C8" {
			cited = d.GetSupportingClaimIds()
		}
	}
	if len(cited) == 0 {
		t.Fatal("no C8 decision in the audit, or it cites no evidence at all")
	}

	// Every id it cites is present in the response, under one kind or the other. An id a decision
	// names and the audit cannot produce is the unfalsifiable case.
	have := map[string]string{}
	for _, c := range resp.GetClaims() {
		have[c.GetClaimId()] = "claim"
	}
	for _, c := range resp.GetCorrelations() {
		have[c.GetCorrelationId()] = "correlation"
	}
	for _, id := range cited {
		kind, ok := have[id]
		if !ok {
			t.Errorf("C8 cites %s as its evidence and the audit reports no record with that id; a "+
				"reviewer asked to check the merge has nothing to check", id)
			continue
		}
		if kind != "correlation" {
			t.Errorf("C8's evidence %s came back as a %s; a deploy identifier is a correlation key, and "+
				"reporting it as a claim would say the graph stores it as a name", id, kind)
		}
	}

	// And the reported keys are the commit, from both sources — the fact the rule actually used.
	if len(resp.GetCorrelations()) < 2 {
		t.Errorf("the audit reports %d correlation keys, want one from each source",
			len(resp.GetCorrelations()))
	}
	for _, c := range resp.GetCorrelations() {
		if c.GetKey().GetNamespace() != deployNamespace || c.GetKey().GetValue() != retriggerCommit {
			t.Errorf("the audit reports %s=%s, want the commit C8 keyed on",
				c.GetKey().GetNamespace(), c.GetKey().GetValue())
		}
		if c.GetSourceId() == "" || c.GetEventId() == "" {
			t.Errorf("a reported key names no source or no event: %+v", c)
		}
	}
}
